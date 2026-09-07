package adbwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// The other side of test/fakeadb's stream_test.go.
//
// That file speaks the protocol by hand so that a bug in this package and a
// matching bug in the fake cannot cancel out. This one closes the loop the
// other way: a progressive, duplex device service is only test equipment if a
// REAL client reads it as one long stream, gets what it wrote delivered, and
// files a mid-stream reset under the taxonomy that keeps a dead socket from
// looking like a refusal. Every one of those is a claim about the pair, and
// neither file can make it alone.

const duplexDevpath = "usb:4-1.1"

func duplexServer(tb testing.TB) *fakeadb.Server {
	tb.Helper()
	return fakeadb.Start(tb, fakeadb.WithDevices(
		fakeadb.Device{Serial: "SERDUPLEX", Devpath: duplexDevpath},
	))
}

// TestAProgressiveStreamIsReadWhole is the volume claim: a megabyte written
// four kilobytes at a time arrives intact through Stream.Read, which is what
// a screen is and what a scripted payload could never be.
func TestAProgressiveStreamIsReadWhole(t *testing.T) {
	t.Parallel()

	const (
		chunk = 4096
		total = 1 << 20
	)
	want := make([]byte, total)
	for i := range want {
		want[i] = byte(i % 251)
	}

	srv := duplexServer(t)
	srv.RespondStream(duplexDevpath, "screen:", func(s *fakeadb.StreamSession) error {
		for off := 0; off < total; off += chunk {
			if _, err := s.Write(want[off : off+chunk]); err != nil {
				return err
			}
		}
		return nil
	})

	st, err := dialFake(t, srv).OpenService(testContext(t), duplexDevpath, "screen:live")
	if err != nil {
		t.Fatalf("OpenService: %v", err)
	}
	defer st.Close()

	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if len(got) != total || !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes of %d, intact=%t", len(got), total, bytes.Equal(got, want))
	}
	if st.Service() != "screen:live" || st.Devpath() != duplexDevpath {
		t.Fatalf("the stream reports service=%q devpath=%q", st.Service(), st.Devpath())
	}
}

// TestTheClientDirectionReachesTheHandler is the half deviceStream never had.
// An input path is bytes going towards the phone, and a fake that could not
// receive them left the whole direction untestable.
func TestTheClientDirectionReachesTheHandler(t *testing.T) {
	t.Parallel()

	srv := duplexServer(t)
	srv.RespondStream(duplexDevpath, "control:", func(s *fakeadb.StreamSession) error {
		// Read to the end rather than to a length: the client half-closes
		// when it is finished, and a handler that cannot tell that from a
		// failure would report a completed conversation as a broken one.
		in, err := io.ReadAll(s)
		if err != nil {
			return err
		}
		_, err = s.Write(append([]byte("SAW:"), in...))
		return err
	})

	st, err := dialFake(t, srv).OpenService(testContext(t), duplexDevpath, "control:v1")
	if err != nil {
		t.Fatalf("OpenService: %v", err)
	}
	defer st.Close()

	if _, err := st.Write([]byte("TAP 540 1200")); err != nil {
		t.Fatalf("writing to the device: %v", err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("reading the receipt: %v", err)
	}
	if string(got) != "SAW:TAP 540 1200" {
		t.Fatalf("the handler received %q", got)
	}
}

// TestASeveredStreamIsATransportFailureNotARefusal is the classification this
// whole package exists for, applied to the shape a live screen dies in.
//
// The distinction is not academic. A *ProtocolError is the server saying no,
// which is a fact about the request; a *TransportError with KindPeerClosed is
// a socket that went away, which is a fact about the wire and about nothing
// else. STF #663 is what happens when the second is read as evidence about
// the work on the far end, so a mid-stream reset that arrived here as a
// refusal — or as a clean end of stream — would be the exact confusion this
// project was built to refuse.
func TestASeveredStreamIsATransportFailureNotARefusal(t *testing.T) {
	t.Parallel()

	srv := duplexServer(t)
	read := make(chan struct{})
	srv.RespondStream(duplexDevpath, "screen:", func(s *fakeadb.StreamSession) error {
		if _, err := s.Write([]byte("HEADER")); err != nil {
			return err
		}
		// Severing only once the client holds the header keeps the test
		// about the cut rather than about which bytes a reset discards.
		<-read
		s.Sever()
		return nil
	})

	st, err := dialFake(t, srv).OpenService(testContext(t), duplexDevpath, "screen:live")
	if err != nil {
		t.Fatalf("OpenService: %v", err)
	}
	defer st.Close()

	var hdr [6]byte
	if _, err := io.ReadFull(st, hdr[:]); err != nil {
		t.Fatalf("reading the header: %v", err)
	}
	if string(hdr[:]) != "HEADER" {
		t.Fatalf("header = %q", hdr)
	}
	close(read)

	var tail [64]byte
	_, err = io.ReadFull(st, tail[:])
	if err == nil {
		t.Fatalf("the stream kept delivering after the sever")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("a severed stream read as %v, which is how a COMPLETED stream reads; "+
			"a caller cannot tell a dead transport from a finished service", err)
	}
	te, ok := AsTransport(err)
	if !ok {
		t.Fatalf("a severed stream produced %v (%T), want *TransportError", err, err)
	}
	if te.Kind != KindPeerClosed || !te.PeerClosed() {
		t.Fatalf("kind = %v (peer_closed=%t), want peer_closed", te.Kind, te.PeerClosed())
	}
	if IsProtocol(err) {
		t.Fatalf("a severed socket was classified as a server refusal: %v", err)
	}
	if IsCanceled(err) {
		t.Fatalf("a severed socket was classified as the caller's own cancellation: %v", err)
	}
	if te.Op != "stream_read" || te.Endpoint != srv.Addr() {
		t.Fatalf("the blip was recorded as op=%q endpoint=%q", te.Op, te.Endpoint)
	}
}

// TestAScriptedScrcpyDeviceIsReachableThroughOpenService walks the fixture the
// way internal/scrcpy will: the spawn, then the video socket, then the
// control socket, each its own transport with its own service string —
// because one ADB transport carries one service, which is why a screen and a
// pair of hands are two connections and two admissions.
func TestAScriptedScrcpyDeviceIsReachableThroughOpenService(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:4-2.1"
		scid    = "1a2b3c4d"
		spawn   = "shell,v2,raw:CLASSPATH=/data/local/tmp/scrcpy-server-0123456789ab.jar " +
			"app_process / com.genymobile.scrcpy.Server 3.1 scid=" + scid
		socket = "localabstract:scrcpy_" + scid
	)
	payload := bytes.Repeat([]byte("F"), 128)

	srv := fakeadb.Start(t, fakeadb.ScrcpyFixture(fakeadb.ScrcpyConfig{
		Devpath:  devpath,
		Width:    1080,
		Height:   2400,
		Packets:  []fakeadb.ScrcpyPacket{{PTS: 42, KeyFrame: true, Data: payload}},
		VideoEOF: true,
	}))
	cli := dialFake(t, srv)
	ctx := testContext(t)

	// The spawn is a shell v2 stream, so it decodes with this package's own
	// packet reader — which is the point of asserting on it here rather than
	// in the fake's tests.
	proc, err := cli.OpenService(ctx, devpath, spawn)
	if err != nil {
		t.Fatalf("spawning the server: %v", err)
	}
	defer proc.Close()
	id, banner, err := NewShellPacketReader(proc).Next()
	if err != nil {
		t.Fatalf("reading the server's first line: %v", err)
	}
	if id != ShellStdout || len(banner) == 0 {
		t.Fatalf("the server announced itself as id=%d payload=%q", id, banner)
	}
	if got, ok := srv.ScrcpySCID(devpath); !ok || got != scid {
		t.Fatalf("ScrcpySCID = %q (found=%t), want %q", got, ok, scid)
	}

	video, err := cli.OpenService(ctx, devpath, socket)
	if err != nil {
		t.Fatalf("opening the video socket: %v", err)
	}
	defer video.Close()
	frames, err := io.ReadAll(video)
	if err != nil {
		t.Fatalf("reading the video socket: %v", err)
	}
	if len(frames) != 12+12+len(payload) {
		t.Fatalf("the video socket carried %d bytes, want a 12-byte session header, "+
			"a 12-byte packet header and %d of payload", len(frames), len(payload))
	}
	if got := binary.BigEndian.Uint32(frames[0:4]); got != fakeadb.ScrcpyCodecH264 {
		t.Fatalf("codec id = %#08x, want h264", got)
	}
	if !bytes.Equal(frames[24:], payload) {
		t.Fatalf("the packet payload did not survive the transport")
	}

	control, err := cli.OpenService(ctx, devpath, socket)
	if err != nil {
		t.Fatalf("opening the control socket: %v", err)
	}
	tap := make([]byte, 32) // an inject-touch message, whose length the fake knows
	tap[0] = 2
	if _, err := control.Write(tap); err != nil {
		t.Fatalf("writing a control message: %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatalf("closing the control socket: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for len(srv.ControlWrites(devpath)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the control socket recorded nothing: %x", srv.ControlBytes(devpath))
		}
		time.Sleep(time.Millisecond)
	}
	got := srv.ControlWrites(devpath)
	if len(got) != 1 || !bytes.Equal(got[0], tap) {
		t.Fatalf("ControlWrites = %x, want the one 32-byte touch message", got)
	}
}
