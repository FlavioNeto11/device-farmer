package screen

// A session is three sockets and an encoder on a phone. These tests are about
// the two things that can go wrong with that which nothing else would catch.
//
// The first is the wire layout. This package reads sixteen bytes of a stream a
// handset writes, and those sixteen bytes decide where a touch lands. The bytes
// below are built by hand from app/src/demuxer.c in Genymobile/scrcpy rather
// than borrowed from test/fakeadb, so that the two are independent statements
// of the same protocol. They disagreed once — the fake had config on the session
// bit — and the only reason anybody found out is that something eventually read
// one with the other.
//
// The second is that nothing here ends a lease. That is not testable by
// inspection in the usual sense — there is no lease in this package to end —
// so what the tests below pin is the absence of the mechanism: no path from a
// dead socket to anything but a closed session, and a shutdown that closes
// sessions and says so.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
)

// ---------------------------------------------------------------------------
// The wire, built from demuxer.c
// ---------------------------------------------------------------------------

// codecH264 is the four ASCII bytes, big-endian, that open a video stream.
const codecH264 uint32 = 0x68323634

// sessionHeader is the twelve bytes scrcpy's demuxer recognises by its top bit:
// sc_demuxer_is_session tests header[0] & 0x80. Width at [4:8], height at
// [8:12], client_resized in header[3] & 1.
func sessionHeader(w, h uint32, resized bool) []byte {
	b := make([]byte, 12)
	b[0] = 0x80
	if resized {
		b[3] = 1
	}
	binary.BigEndian.PutUint32(b[4:8], w)
	binary.BigEndian.PutUint32(b[8:12], h)
	return b
}

// packetHeader is flags+PTS in bytes 0..7 and the payload length in 8..11.
// CONFIG is bit 62 and KEY_FRAME is bit 61 — one lower than test/fakeadb has
// them, which is the discrepancy noted at the top of this file.
func packetHeader(pts uint64, config, key bool, n uint32) []byte {
	b := make([]byte, 12)
	meta := pts & ((uint64(1) << 61) - 1)
	if config {
		meta |= uint64(1) << 62
	}
	if key {
		meta |= uint64(1) << 61
	}
	binary.BigEndian.PutUint64(b[0:8], meta)
	binary.BigEndian.PutUint32(b[8:12], n)
	return b
}

// videoStream builds a stream of the shape a phone produces.
func videoStream(w, h uint32, payloads ...[]byte) []byte {
	var out []byte
	var id [4]byte
	binary.BigEndian.PutUint32(id[:], codecH264)
	out = append(out, id[:]...)
	out = append(out, sessionHeader(w, h, false)...)
	for i, p := range payloads {
		out = append(out, packetHeader(uint64(i)*83_333, i == 0, i <= 1, uint32(len(p)))...)
		out = append(out, p...)
	}
	return out
}

// shellPacket frames one shell v2 packet the way a device does: the id byte,
// then a little-endian u32 length, then the payload.
func shellPacket(id byte, s string) []byte {
	b := make([]byte, 5+len(s))
	b[0] = id
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(s)))
	copy(b[5:], s)
	return b
}

// ---------------------------------------------------------------------------
// A device that is not test/fakeadb
// ---------------------------------------------------------------------------

// fakeDevice answers the three services a session opens.
//
// It is local to this package rather than reusing test/fakeadb because of the
// layout disagreement described at the top of this file, and because the cases
// below need to control the ORDER and the TIMING of the socket appearing, which
// is the one thing about starting a session that the protocol does not settle.
type fakeDevice struct {
	mu sync.Mutex

	// video is the bytes the first socket connection yields.
	video []byte

	// serverLog is what the shell service writes.
	serverLog string

	// socketAfter is how many connect attempts are refused before the abstract
	// socket "appears". Zero means it is there immediately.
	socketAfter int
	attempts    int

	// spawnErr, if set, is what starting the server fails with.
	spawnErr error

	opened []string

	// recordCtx keeps the context each service was opened with, so a test can
	// assert on its lifetime. Off by default: most cases here are about bytes,
	// and a test that held every context would hide which one it meant.
	recordCtx bool
	ctxs      []context.Context
	pushed    map[string][]byte
	control   *recorder
	closed    int
}

func newFakeDevice(video []byte) *fakeDevice {
	return &fakeDevice{
		video:     video,
		serverLog: "[server] INFO: Device: fake Pixel (Android 14)\n",
		pushed:    map[string][]byte{},
		control:   &recorder{},
	}
}

func (d *fakeDevice) OpenService(ctx context.Context, service string) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened = append(d.opened, service)
	if d.recordCtx {
		d.ctxs = append(d.ctxs, ctx)
	}

	switch {
	case strings.HasPrefix(service, "shell,v2,raw:"):
		if d.spawnErr != nil {
			return nil, d.spawnErr
		}
		return &rwc{r: strings.NewReader(string(shellPacket(1, d.serverLog))), dev: d}, nil

	case strings.HasPrefix(service, "localabstract:"):
		d.attempts++
		if d.attempts <= d.socketAfter {
			return nil, errors.New("closed: cannot connect to localabstract socket")
		}
		// First connection is video, second is control — the order the
		// protocol fixes and Open must therefore honour.
		if d.attempts == d.socketAfter+1 {
			return &rwc{r: newChunkReader(d.video), dev: d}, nil
		}
		return &rwc{r: blockForever{}, w: d.control, dev: d}, nil
	}
	return nil, errors.New("fakeDevice: unexpected service " + service)
}

func (d *fakeDevice) Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pushed[remote] = b
	return nil
}

func (d *fakeDevice) services() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.opened...)
}

func (d *fakeDevice) contexts() []context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]context.Context(nil), d.ctxs...)
}

func (d *fakeDevice) closes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// rwc is one socket.
type rwc struct {
	r    io.Reader
	w    io.Writer
	dev  *fakeDevice
	once sync.Once
}

func (s *rwc) Read(p []byte) (int, error) {
	if s.r == nil {
		return 0, io.EOF
	}
	return s.r.Read(p)
}

func (s *rwc) Write(p []byte) (int, error) {
	if s.w == nil {
		return len(p), nil
	}
	return s.w.Write(p)
}

func (s *rwc) Close() error {
	s.once.Do(func() {
		s.dev.mu.Lock()
		s.dev.closed++
		s.dev.mu.Unlock()
		if c, ok := s.r.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return nil
}

// recorder collects what was written to the control socket.
type recorder struct {
	mu sync.Mutex
	b  []byte
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	return len(p), nil
}

func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.b...)
}

// chunkReader hands out the stream a few bytes at a time.
//
// Deliberately NOT a bytes.Reader. A reader that satisfies every ReadFull in one
// call would let a parser that assumed header and payload arrive together pass,
// and a parser that works against such a reader does not work against a socket.
type chunkReader struct {
	b []byte
	i int
}

func newChunkReader(b []byte) *chunkReader { return &chunkReader{b: b} }

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.b) {
		return 0, io.EOF
	}
	n := 3
	if n > len(p) {
		n = len(p)
	}
	if c.i+n > len(c.b) {
		n = len(c.b) - c.i
	}
	copy(p[:n], c.b[c.i:c.i+n])
	c.i += n
	return n, nil
}

// blockForever is the read half of the control socket: a real one carries
// device-to-client messages this feature does not use, and a reader that
// returned EOF would make a closed session indistinguishable from an open one.
type blockForever struct{}

func (blockForever) Read([]byte) (int, error) {
	ch := make(chan struct{})
	<-ch // released by the test ending; Close does not need to unblock it
	return 0, io.EOF
}

// ---------------------------------------------------------------------------
// Starting a session
// ---------------------------------------------------------------------------

func openOne(t *testing.T, d *fakeDevice, m *Manager) *Session {
	t.Helper()
	s, err := m.Open(context.Background(), d, Options{
		DeviceID: "dev-1",
		JarID:    "0f1e2d3c4b5a",
		Version:  "3.1",
		MaxSize:  1024,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestOpenReadsTheFrameSizeAndNothingElse is the assertion the whole allocation
// argument rests on.
//
// The process that imports this package answers POST /leases/{id}/renew. A
// length read off a wedged handset and handed to make() takes that path down by
// OOM. So exactly sixteen fixed-width bytes are consumed — the codec id and the
// session header — and everything after them is handed back unread.
//
// Falsify: call vr.Next() a second time in Open. The preamble grows past 16 and
// the spliced stream is missing a frame.
func TestOpenReadsTheFrameSizeAndNothingElse(t *testing.T) {
	frames := [][]byte{[]byte("SPSPPS"), []byte("IDR-frame-bytes"), []byte("P-frame")}
	d := newFakeDevice(videoStream(460, 1024, frames...))
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	if s.Codec != scrcpy.CodecH264 {
		t.Errorf("codec = %v, want h264", s.Codec)
	}
	if w, h := s.Frame(); w != 460 || h != 1024 {
		t.Errorf("frame = %dx%d, want 460x1024; a touch is placed in this space, so a wrong "+
			"size here lands every tap somewhere else", w, h)
	}
	if n := len(s.Preamble()); n != 16 {
		t.Fatalf("preamble is %d bytes, want exactly 16 (a 4-byte codec id and a 12-byte "+
			"session header); anything more means a packet length was read, and a packet "+
			"length is a number a handset chose", n)
	}

	// And the rest of the stream must still be there, byte for byte. The
	// browser decodes these; a missing or re-encoded byte is a decoder error
	// the operator sees as a blank panel.
	rest, err := io.ReadAll(s.Video())
	if err != nil {
		t.Fatalf("reading the spliced stream: %v", err)
	}
	whole := videoStream(460, 1024, frames...)
	if got, want := string(s.Preamble())+string(rest), string(whole); got != want {
		t.Errorf("preamble + spliced body is %d bytes and the device wrote %d; the stream a "+
			"decoder receives is not the stream the phone produced", len(got), len(want))
	}
}

// TestOpenStartsTheServerThenVideoThenControl. The protocol carries no field
// distinguishing the two socket connections: the server hands the first to the
// encoder. So the order is the whole of the mechanism, and a concurrent or
// reordered Open gets control frames fed to a decoder.
//
// Falsify: swap the two connectSocket calls in Open.
func TestOpenStartsTheServerThenVideoThenControl(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	got := d.services()
	if len(got) != 3 {
		t.Fatalf("opened %d services, want 3 (shell, video, control): %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "shell,v2,raw:") {
		t.Errorf("the first service is %q; the server has to be started before its socket "+
			"can exist", got[0])
	}
	if got[1] != s.Socket || got[2] != s.Socket {
		t.Errorf("the two socket connects are %q and %q, want both %q", got[1], got[2], s.Socket)
	}

	// The command line must be the one the fence proxy's control class admits.
	// That is asserted properly in internal/scrcpy/admission_test.go; what is
	// checked here is that this package passes the arguments it documents,
	// because a caller cannot see them any other way.
	for _, want := range []string{
		"CLASSPATH=/data/local/tmp/scrcpy-server-0f1e2d3c4b5a.jar",
		"com.genymobile.scrcpy.Server 3.1",
		"video_codec=h264", "max_size=1024", "control=true",
		"tunnel_forward=true", "send_device_meta=false", "send_dummy_byte=false",
		"cleanup=true",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the spawn command is missing %q:\n  %s", want, got[0])
		}
	}
	if strings.Contains(got[0], "audio=true") {
		t.Error("audio is on; nothing in this path carries audio and it costs a second encoder")
	}
}

// TestOpenWaitsForTheSocketToAppear is the race the protocol does not resolve:
// the server is started, and its abstract socket exists a moment later.
//
// Falsify: remove the retry loop from connectSocket.
func TestOpenWaitsForTheSocketToAppear(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	d.socketAfter = 3 // three refusals, then it is there

	m := NewManager(4, nil)
	s := openOne(t, d, m)
	if w, _ := s.Frame(); w != 460 {
		t.Errorf("the session did not come up after the socket appeared")
	}
}

// TestASocketThatNeverAppearsNamesTheServersOwnWords. "The server never
// started" and "the server started and never published a socket" send an
// operator to different places, and the only evidence distinguishing them is
// what the handset wrote on the shell stream. A refusal that dropped it would
// leave an operator with a timeout and nothing else.
//
// Falsify: return the bare connect error from Open instead of a *SocketError.
func TestASocketThatNeverAppearsNamesTheServersOwnWords(t *testing.T) {
	d := newFakeDevice(nil)
	d.socketAfter = 1 << 30 // never
	d.serverLog = "[server] ERROR: The server version (3.1) does not match the client\n"

	m := NewManager(4, nil)
	_, err := m.Open(context.Background(), d, Options{
		DeviceID: "dev-1", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024,
		SpawnTimeout: 300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Open succeeded with a socket that never appeared")
	}
	var se *SocketError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T (%v), want a *SocketError so the caller can tell this from a "+
			"refusal", err, err)
	}
	if !strings.Contains(se.ServerLog, "does not match") {
		t.Errorf("the refusal does not carry the server's own words, which are the only "+
			"evidence about which failure this was. ServerLog = %q", se.ServerLog)
	}

	// And nothing is left registered, so a retry is not refused as a duplicate.
	if n := m.Len(); n != 0 {
		t.Errorf("%d sessions remain after a failed Open; the device is now permanently "+
			"unavailable to a retry", n)
	}
}

// TestAFailedOpenClosesEverythingItOpened. A half-started session holds an
// encoder on a phone with nothing in this process able to name it, which means
// nothing can ever release it.
//
// Falsify: drop the `if !ok { s.Close() }` defer in Open.
func TestAFailedOpenClosesEverythingItOpened(t *testing.T) {
	// A video socket that yields a stream that is not scrcpy's: the codec id
	// fails to parse, after the shell and the video socket are both open.
	d := newFakeDevice([]byte{0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0})
	m := NewManager(4, nil)
	_, err := m.Open(context.Background(), d, Options{
		DeviceID: "dev-1", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024,
	})
	if err == nil {
		t.Fatal("Open accepted a stream that is not a scrcpy video stream")
	}
	if d.closes() < 2 {
		t.Errorf("only %d sockets were closed after a failed Open; the shell stream and the "+
			"video socket were both open, and a socket this process cannot name is an "+
			"encoder on a phone that nothing will ever free", d.closes())
	}
}

// ---------------------------------------------------------------------------
// One session per device, and the cap
// ---------------------------------------------------------------------------

// TestASecondSessionOnOneDeviceIsRefused. scrcpy is one encoder per session.
// Two would be two people driving one phone with neither able to tell.
//
// Falsify: delete the byDevice check in reserve.
func TestASecondSessionOnOneDeviceIsRefused(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	openOne(t, d, m)

	_, err := m.Open(context.Background(), newFakeDevice(videoStream(460, 1024, []byte("SPS"))),
		Options{DeviceID: "dev-1", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024})
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("a second session on dev-1 returned %v, want ErrSessionExists", err)
	}
}

// TestClosingASessionFreesItsDevice. The refusal above must be temporary. A
// session that closed without releasing its device key would make the device
// permanently unscreenable by anyone, for the rest of the process's life.
//
// Falsify: delete the forget() call in Session.Close.
func TestClosingASessionFreesItsDevice(t *testing.T) {
	m := NewManager(4, nil)
	first := openOne(t, newFakeDevice(videoStream(460, 1024, []byte("SPS"))), m)
	first.Close()

	if n := m.Len(); n != 0 {
		t.Fatalf("%d sessions after Close", n)
	}
	second, err := m.Open(context.Background(), newFakeDevice(videoStream(460, 1024, []byte("SPS"))),
		Options{DeviceID: "dev-1", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024})
	if err != nil {
		t.Fatalf("the device was not freed by Close: %v", err)
	}
	second.Close()
}

// TestTheSessionCapRefusesRatherThanDegrades. Each session is three transports
// and a response this process cannot buffer. A wall of open tabs must refuse the
// next one with a reason, not starve the request that renews every lease.
//
// Falsify: delete the len(m.byDevice) >= m.max check in reserve.
func TestTheSessionCapRefusesRatherThanDegrades(t *testing.T) {
	m := NewManager(2, nil)
	for _, id := range []string{"dev-1", "dev-2"} {
		if _, err := m.Open(context.Background(), newFakeDevice(videoStream(460, 1024, []byte("S"))),
			Options{DeviceID: id, JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024}); err != nil {
			t.Fatalf("opening %s: %v", id, err)
		}
	}
	t.Cleanup(m.Close)

	_, err := m.Open(context.Background(), newFakeDevice(videoStream(460, 1024, []byte("S"))),
		Options{DeviceID: "dev-3", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024})
	if !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("the third session returned %v, want ErrTooManySessions", err)
	}
}

// TestShutdownClosesEverySessionAndRefusesNewOnes.
//
// This is not tidiness. A long-lived response that nothing closes makes
// http.Server.Shutdown wait out the entire drain grace period on every single
// deploy, which turns a rolling update into a thirty-second outage per replica.
//
// It also ends no lease, and cannot: there is no path from here to farm.leases.
//
// Falsify: make Manager.Close set closed and return without closing sessions.
func TestShutdownClosesEverySessionAndRefusesNewOnes(t *testing.T) {
	m := NewManager(4, nil)
	s := openOne(t, newFakeDevice(videoStream(460, 1024, []byte("SPS"))), m)

	m.Close()

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a session was still open after Manager.Close; every deploy now pays the full " +
			"shutdown grace period waiting for a response nothing will end")
	}
	if _, err := m.Open(context.Background(), newFakeDevice(nil),
		Options{DeviceID: "dev-9", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024}); !errors.Is(err, ErrClosed) {
		t.Errorf("Open during shutdown returned %v, want ErrClosed", err)
	}
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// TestInputGoesOutAsOneWrite. The control socket is a byte stream carrying
// length-delimited messages with no sequence number. A batch that left as
// several writes could interleave with a concurrent batch at the kernel, and a
// half-written touch is a phone that thinks a finger is still down.
//
// Falsify: write each event separately in Session.Send.
func TestInputGoesOutAsOneWrite(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	n, err := s.Send(
		Touch{Action: scrcpy.TouchDown, X: 100, Y: 200, Pressure: 1, PointerID: 0},
		Touch{Action: scrcpy.TouchUp, X: 100, Y: 200, Pressure: 0, PointerID: 0},
	)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n != 2 {
		t.Errorf("Send reported %d events, want 2", n)
	}
	if got := s.Inputs(); got != 2 {
		t.Errorf("Inputs() = %d, want 2", got)
	}
	if len(d.control.bytes()) == 0 {
		t.Fatal("nothing reached the control socket")
	}
}

// TestACoordinateOutsideTheFrameSendsNothingAtAll is the one that matters most
// about batching.
//
// A batch is down-then-up. If the down is sent and the up is refused, the phone
// is left with a pointer down that nothing will ever lift: it behaves as though
// somebody is holding the screen, and the only cure is another session. So a
// refusal must discard the WHOLE batch, including the events before the bad one
// that were already encoded.
//
// Falsify: in Session.Send, write buf before returning the error.
func TestACoordinateOutsideTheFrameSendsNothingAtAll(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	_, err := s.Send(
		Touch{Action: scrcpy.TouchDown, X: 10, Y: 10, Pressure: 1},
		Touch{Action: scrcpy.TouchUp, X: 10_000, Y: 10, Pressure: 0}, // past 460
	)
	if err == nil {
		t.Fatal("a touch outside the frame was accepted")
	}
	if !strings.Contains(err.Error(), "none of the batch was sent") {
		t.Errorf("the error does not say the batch was discarded, so a caller cannot know "+
			"whether a finger is still down: %v", err)
	}
	if b := d.control.bytes(); len(b) != 0 {
		t.Fatalf("%d bytes reached the device from a refused batch; the first touch went down "+
			"and nothing will lift it", len(b))
	}
	if got := s.Inputs(); got != 0 {
		t.Errorf("Inputs() = %d after a refused batch, want 0", got)
	}
}

// TestCoordinatesAreInVideoSpaceNotDevicePixels.
//
// A 1080x2400 panel streamed at max_size 1024 is 460x1024. A caller that sends
// device pixels misses by more than half the screen. The mechanism that catches
// it is that the frame refuses anything outside itself — so a device-pixel
// coordinate on a scaled stream is not silently mapped, it is refused.
//
// Falsify: construct the Position from X/Y directly instead of through
// Screen.At.
func TestCoordinatesAreInVideoSpaceNotDevicePixels(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	// 900 is a perfectly ordinary x on a 1080-wide panel and is off the right
	// edge of the 460-wide frame that was actually encoded.
	if _, err := s.Send(Touch{Action: scrcpy.TouchDown, X: 900, Y: 1200, Pressure: 1}); err == nil {
		t.Error("a device-pixel coordinate was accepted on a scaled stream; every tap from a " +
			"caller that did not rescale now lands in the wrong place, silently")
	}

	// The far corner of the frame itself must be reachable, or the check is
	// just an off-by-one that refuses legitimate taps.
	if _, err := s.Send(Touch{Action: scrcpy.TouchDown, X: 459, Y: 1023, Pressure: 1}); err != nil {
		t.Errorf("the bottom-right pixel of the frame was refused: %v", err)
	}
}

// TestSendOnAClosedSessionIsRefusedNotWritten.
//
// Falsify: delete the select on s.done at the top of Send.
func TestSendOnAClosedSessionIsRefusedNotWritten(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	s := openOne(t, d, m)
	s.Close()

	if _, err := s.Send(Key{Action: scrcpy.KeyDown, Keycode: 4}); !errors.Is(err, ErrNoSession) {
		t.Errorf("Send on a closed session returned %v, want ErrNoSession", err)
	}
}

// TestAKeyNeedsNoFrame is the asymmetry in the Event interface, stated as a
// test: a key event carries no position, so there is nothing for a frame to
// refuse, and the encoder for it has no error return. If this ever starts
// failing, someone has given Key a coordinate.
func TestAKeyNeedsNoFrame(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	// BACK, on a frame whose every coordinate would be out of range.
	if _, err := s.Send(Key{Action: scrcpy.KeyDown, Keycode: 4}, Key{Action: scrcpy.KeyUp, Keycode: 4}); err != nil {
		t.Fatalf("a key event was refused: %v", err)
	}
	if got := s.Inputs(); got != 2 {
		t.Errorf("Inputs() = %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// The server's own log
// ---------------------------------------------------------------------------

// TestTheServerLogIsDrainedForTheWholeSession.
//
// THIS IS NOT A DIAGNOSTIC LUXURY. The server writes to the shell stream. A
// reader that stops reading fills the socket buffer, the server's next write
// blocks, and a blocked server stops producing frames — so the picture freezes
// and nothing anywhere says why. This test exists so that "why did the video
// stop" is never answered with "because nobody was reading the log".
//
// Falsify: delete the `go s.drainServerLog()` line in Open.
func TestTheServerLogIsDrainedForTheWholeSession(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	d.serverLog = "[server] INFO: Device: fake Pixel (Android 14)\n"
	m := NewManager(4, nil)
	s := openOne(t, d, m)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.ServerLog(), "Android 14") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server's output never reached ServerLog(); nothing is reading the shell "+
		"stream, so the server will block on its next write and the video will freeze with "+
		"no explanation. ServerLog() = %q", s.ServerLog())
}

// TestTheServerLogIsBounded. It runs for the life of a session on a handset that
// may be chatty, and a diagnostic that grows without limit is a leak.
//
// Falsify: delete the trim in Session.note.
func TestTheServerLogIsBounded(t *testing.T) {
	s := &Session{}
	for range 100 {
		s.note([]byte(strings.Repeat("x", 1024)))
	}
	if got := len(s.ServerLog()); got > serverLogCap {
		t.Errorf("the server log is %d bytes, above the %d cap", got, serverLogCap)
	}
	// And it must keep the TAIL, because the interesting line is the last one
	// the server managed to write before it stopped.
	s.note([]byte("LAST LINE"))
	if !strings.HasSuffix(s.ServerLog(), "LAST LINE") {
		t.Error("the most recent output was trimmed away; the last thing a dying server says " +
			"is the one thing worth keeping")
	}
}

// ---------------------------------------------------------------------------
// The jar path
// ---------------------------------------------------------------------------

// TestTheJarIsPushedToThePathTheCommandLineNames. A CLASSPATH pointing at a file
// that is not there is a server that exits silently, and "silently" is the whole
// problem: there is no error to read, just a socket that never appears.
//
// Falsify: have Open pass a different path to EnsureJar than to the spawn.
func TestTheJarIsPushedToThePathTheCommandLineNames(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)

	var ensured string
	s, err := m.Open(context.Background(), d, Options{
		DeviceID: "dev-1", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024,
		EnsureJar: func(ctx context.Context, remote string) error {
			ensured = remote
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if ensured == "" {
		t.Fatal("EnsureJar was never called, so nothing put the server on the device")
	}
	if !strings.Contains(d.services()[0], "CLASSPATH="+ensured+" ") {
		t.Errorf("the jar was ensured at %q and the command line says otherwise:\n  %s",
			ensured, d.services()[0])
	}
}

// TestAJarThatCannotBePushedStopsBeforeTheServerIsStarted. Starting a server
// whose class path is empty costs a transport and produces a socket that never
// appears, which is the least diagnosable failure in this whole path.
//
// Falsify: move the EnsureJar call after the OpenService for the shell service.
func TestAJarThatCannotBePushedStopsBeforeTheServerIsStarted(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS")))
	m := NewManager(4, nil)

	_, err := m.Open(context.Background(), d, Options{
		DeviceID: "dev-1", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024,
		EnsureJar: func(context.Context, string) error { return errors.New("blob not in the store") },
	})
	if err == nil {
		t.Fatal("Open started a server with no jar on the device")
	}
	if !strings.Contains(err.Error(), "did not reach the device") {
		t.Errorf("the error does not say the jar is the problem: %v", err)
	}
	if n := len(d.services()); n != 0 {
		t.Errorf("%d services were opened after the push failed: %v; the cheapest diagnosis "+
			"was available before any of them", n, d.services())
	}
}

// TestJarIDFromSHAMatchesTheArtifactDigest. The digest an operator pins is 64
// hex characters; the name on the device is the first twelve. Doing that
// truncation by hand anywhere else is how the two come to disagree.
func TestJarIDFromSHAMatchesTheArtifactDigest(t *testing.T) {
	const sha = "3b1f4c0d9e2a6b8c7d5e4f0a1b2c3d4e5f60718293a4b5c6d7e8f9012345678a"
	id, err := JarIDFromSHA(sha)
	if err != nil {
		t.Fatalf("JarIDFromSHA: %v", err)
	}
	if id != sha[:scrcpy.JarIDLen] {
		t.Errorf("JarIDFromSHA = %q, want the first %d characters of the digest (%q)",
			id, scrcpy.JarIDLen, sha[:scrcpy.JarIDLen])
	}
	for _, bad := range []string{"", "zz", sha[:63], sha + "a"} {
		if _, err := JarIDFromSHA(bad); err == nil {
			t.Errorf("JarIDFromSHA(%q) was accepted; it names no artifact", bad)
		}
	}
}

// TestEverySessionGetsItsOwnSocketName. The abstract socket namespace is shared
// by everything on the handset. A predictable name is one another process can
// publish first, and the symptom is this process connecting to somebody else's
// socket and calling it a screen.
//
// Falsify: make newSCID return a constant.
func TestEverySessionGetsItsOwnSocketName(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		id, err := newSCID()
		if err != nil {
			t.Fatalf("newSCID: %v", err)
		}
		if !id.Valid() {
			t.Fatalf("newSCID produced %v, which scrcpy.SCID.Valid rejects", id)
		}
		seen[id.String()] = true
	}
	if len(seen) < 60 {
		t.Errorf("64 draws produced %d distinct socket names; they are meant to be "+
			"unguessable, and this many collisions means they are not random", len(seen))
	}
}

// TestTheVideoSocketOutlivesTheCallThatOpenedIt is the regression for the defect
// this package's own tests could not see.
//
// An adbwire stream's lifetime IS the context it was opened with — that is how a
// caller unblocks a read on a silent device — so opening the video socket with
// the SPAWN context, which Open cancels on its way out, kills the stream the
// instant Open returns.
//
// The symptom was precise and baffling, and every unit test here passed through
// it: the sixteen preamble bytes arrive, the response carries a correct session
// header and a correct frame size, the caller reports a healthy session, and
// then no frame ever comes. It was found by opening the stream over HTTP against
// the demo and counting the bytes, which is a thing no test in this package did,
// because the fake Device ignored the context it was handed.
//
// So this one does not ignore it.
//
// Falsify: pass spawnCtx as connectSocket's first argument instead of sessCtx.
func TestTheVideoSocketOutlivesTheCallThatOpenedIt(t *testing.T) {
	d := newFakeDevice(videoStream(460, 1024, []byte("SPS"), []byte("IDR")))
	d.recordCtx = true

	m := NewManager(4, nil)
	s := openOne(t, d, m)

	// Open has returned. Every context a socket was opened with must still be
	// alive, because each of them is a socket this session is about to read.
	for i, ctx := range d.contexts() {
		if err := ctx.Err(); err != nil {
			t.Errorf("the context socket %d was opened with is already cancelled (%v). "+
				"An adbwire stream dies with the context it was opened with, so this session "+
				"has a video socket that will never produce a frame — and it will report "+
				"itself healthy while doing it, because the session header was already read.",
				i+1, err)
		}
	}

	// And the stream really does still deliver.
	got := make([]byte, 8)
	if _, err := io.ReadFull(s.Video(), got); err != nil {
		t.Fatalf("reading the spliced stream after Open returned: %v", err)
	}
}

// TestAReservationIsNotASession pins the two places a half-started session was
// mistaken for a real one.
//
// Open puts a nil under both map keys BEFORE it dials, so that two concurrent
// Opens on one device cannot both push a jar and start a server. That
// placeholder is a *Session that is nil, and both readers of the map treated it
// as found:
//
//   - Lookup returned (nil, nil). A caller with no error has every reason to
//     dereference what it was handed, and the api's input route did.
//   - Close called (*Session).Close on it. That one panicked in the SHUTDOWN
//     path — which runs on every deploy, while requests are still draining, and
//     takes the process down hard instead of letting them finish.
//
// The window is small and entirely reachable: it is an input request or a
// SIGTERM arriving while another request is starting a session, which is what a
// client retrying a stream produces on a busy farm.
//
// Falsify: drop the `|| s == nil` in Lookup, or the `if s != nil` in Close. The
// first returns a nil session with no error; the second panics.
func TestAReservationIsNotASession(t *testing.T) {
	m := NewManager(4, nil)

	// Reach the state directly. Driving Open to its window would need a device
	// that blocks mid-dial and a second goroutine racing it — a test about
	// timing rather than about the property, and one that would pass by luck.
	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	if err := m.reserve("dev-1", id); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	got, err := m.Lookup(id)
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("Lookup of a reservation returned (%v, %v); want ErrNoSession. A caller with "+
			"no error dereferences what it is handed.", got, err)
	}
	if got != nil {
		t.Errorf("Lookup of a reservation returned a session: %v", got)
	}

	// The one that panicked. No assertion is needed beyond returning: a panic
	// here fails the test, and a panic in Shutdown fails a deploy.
	m.Close()

	// And the manager is properly shut afterwards, so the fix did not turn the
	// panic into a manager that skipped its own close.
	if _, err := m.Open(context.Background(), newFakeDevice(nil),
		Options{DeviceID: "dev-2", JarID: "0f1e2d3c4b5a", Version: "3.1", MaxSize: 1024}); !errors.Is(err, ErrClosed) {
		t.Errorf("Open after Close returned %v, want ErrClosed", err)
	}
}
