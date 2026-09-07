package fakeadb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The scrcpy fixture's own tests read the framing back by hand, byte offset
// by byte offset, rather than through a decoder. A fixture and a decoder
// built from one description agree with each other whatever either of them
// says about scrcpy, and the point of this file is that neither of them gets
// to be the authority: the offsets below are written out so a reader can
// check them against docs/design/interactive-control.md §3 and against
// scrcpy's own source.

const (
	testSCID    = "1a2b3c4d"
	testJarHash = "0123456789ab"
)

// The framing, restated in literals rather than through the fixture's own
// names. Reading a header back with the constants that wrote it is a test
// that cannot fail while both move together, and both moving together is
// precisely how this fixture would come to disagree with internal/scrcpy
// without anything going red.
const (
	wireVideoHeaderLen  = 12                    // codec id u32, width u32, height u32
	wirePacketHeaderLen = 12                    // flags+pts u64, payload length u32
	wireCodecH264       = uint32(0x68323634)    // "h264"
	wireFlagConfig      = uint64(1) << 63       // top bit
	wireFlagKeyFrame    = uint64(1) << 62       // the one below it
	wirePTSMask         = (uint64(1) << 62) - 1 // everything under the flags
)

// Control message type bytes, likewise as numbers.
const (
	wireMsgKeycode  = 0
	wireMsgText     = 1
	wireMsgTouch    = 2
	wireMsgScroll   = 3
	wireMsgBack     = 4
	wireMsgNotifs   = 5
	wireMsgSettings = 6
	wireMsgCollapse = 7
	wireMsgGetClip  = 8
	wireMsgSetClip  = 9
	wireMsgPower    = 10
	wireMsgRotate   = 11
)

func spawnService(scid string) string {
	return fmt.Sprintf(
		"shell,v2,raw:CLASSPATH=/data/local/tmp/scrcpy-server-%s.jar "+
			"app_process / com.genymobile.scrcpy.Server 3.1 scid=%s log_level=info video_bit_rate=8000000",
		testJarHash, scid)
}

// readShellPacket reads one shell v2 frame: an id byte, a little-endian
// length, and the payload.
func readShellPacket(tb testing.TB, w *wire) (byte, string) {
	tb.Helper()
	var hdr [5]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
		tb.Fatalf("reading a shell packet header: %v", err)
	}
	n := binary.LittleEndian.Uint32(hdr[1:5])
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.br, buf); err != nil {
		tb.Fatalf("reading a %d-byte shell payload: %v", n, err)
	}
	return hdr[0], string(buf)
}

// TestScrcpyFixtureServesSpawnVideoAndControl walks one whole session the way
// a client will: start the server, take the video socket, take the control
// socket, and send something down it.
func TestScrcpyFixtureServesSpawnVideoAndControl(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.1"
	packets := []ScrcpyPacket{
		{Config: true, Data: []byte("SPS-PPS")},
		{PTS: 1_000_000, KeyFrame: true, Data: bytes.Repeat([]byte("K"), 300)},
		{PTS: 1_033_000, Data: bytes.Repeat([]byte("P"), 64)},
	}
	s := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:   devpath,
		Codec:     wireCodecH264,
		Width:     1080,
		Height:    2400,
		Packets:   packets,
		PacketGap: time.Millisecond,
		VideoEOF:  true,
	}))

	// The fixture installs the position itself, so a scrcpy test is one line.
	if _, ok := s.Device(devpath); !ok {
		t.Fatalf("the fixture did not install %s", devpath)
	}

	// 1. The spawn. It stays open for as long as the client keeps it, which
	// is what a process does.
	spawn := streamWire(t, s, devpath, spawnService(testSCID))
	id, banner := readShellPacket(t, spawn)
	if id != shellPacketStdout {
		t.Fatalf("the server announced itself on packet id %d, want stdout (%d)", id, shellPacketStdout)
	}
	if banner == "" {
		t.Fatalf("the server announced itself with nothing at all")
	}
	if got, ok := s.ScrcpySCID(devpath); !ok || got != testSCID {
		t.Fatalf("ScrcpySCID = %q (found=%t), want %q — the spawn's session id was not published",
			got, ok, testSCID)
	}
	if got := s.ScrcpySpawns(devpath); len(got) != 1 || !strings.Contains(got[0], "com.genymobile.scrcpy.Server") {
		t.Fatalf("ScrcpySpawns = %q", got)
	}

	// 2. The video socket: the session header, then the packets.
	video := streamWire(t, s, devpath, "localabstract:scrcpy_"+testSCID)
	hdr := mustRead(t, video, wireVideoHeaderLen, "the session header")
	if got := binary.BigEndian.Uint32(hdr[0:4]); got != wireCodecH264 {
		t.Fatalf("codec id = %#08x, want %#08x (\"h264\")", got, wireCodecH264)
	}
	if w, h := binary.BigEndian.Uint32(hdr[4:8]), binary.BigEndian.Uint32(hdr[8:12]); w != 1080 || h != 2400 {
		t.Fatalf("geometry = %dx%d, want 1080x2400", w, h)
	}

	for i, want := range packets {
		ph := mustRead(t, video, wirePacketHeaderLen, fmt.Sprintf("packet %d's header", i))
		meta := binary.BigEndian.Uint64(ph[0:8])
		size := binary.BigEndian.Uint32(ph[8:12])
		if got := meta&wireFlagConfig != 0; got != want.Config {
			t.Fatalf("packet %d: config=%t, want %t", i, got, want.Config)
		}
		if got := meta&wireFlagKeyFrame != 0; got != want.KeyFrame {
			t.Fatalf("packet %d: keyframe=%t, want %t", i, got, want.KeyFrame)
		}
		if got := meta & wirePTSMask; got != want.PTS {
			t.Fatalf("packet %d: pts=%d, want %d", i, got, want.PTS)
		}
		if int(size) != len(want.Data) {
			t.Fatalf("packet %d: length=%d, want %d", i, size, len(want.Data))
		}
		if got := mustRead(t, video, int(size), "the payload"); !bytes.Equal(got, want.Data) {
			t.Fatalf("packet %d payload = %q", i, got)
		}
	}
	if rest, err := io.ReadAll(video.br); err != nil || len(rest) != 0 {
		t.Fatalf("after the last packet the video socket carried %q (err=%v), want a clean end", rest, err)
	}

	// 3. The control socket, which writes nothing and remembers everything.
	control := streamWire(t, s, devpath, "localabstract:scrcpy_"+testSCID)
	tap := touchMessage(0, 540, 1200, 1080, 2400)
	key := keycodeMessage(0, 4)
	// Both in ONE write, so what comes back is split by the protocol's
	// lengths and not by whatever the kernel handed the reader.
	if _, err := control.c.Write(append(append([]byte(nil), tap...), key...)); err != nil {
		t.Fatalf("writing control messages: %v", err)
	}
	if err := control.c.Close(); err != nil {
		t.Fatalf("closing the control socket: %v", err)
	}

	waitFor(t, func() bool { return len(s.ControlWrites(devpath)) == 2 },
		func() string { return fmt.Sprintf("ControlWrites = %x", s.ControlWrites(devpath)) })

	want := [][]byte{tap, key}
	if got := s.ControlWrites(devpath); !reflect.DeepEqual(got, want) {
		t.Fatalf("ControlWrites = %x, want %x (raw recording %x)", got, want, s.ControlBytes(devpath))
	}
	if got, want := s.ControlBytes(devpath), append(append([]byte(nil), tap...), key...); !bytes.Equal(got, want) {
		t.Fatalf("ControlBytes = %x, want %x", got, want)
	}
}

// TestScrcpySocketNobodyPublishedIsRefused keeps the fixture from answering a
// session id it never handed out. The alternative is worse than a refusal: a
// client with a stale id would read the default scripted payload and take
// Echo's text for a video header, which is a fixture defect wearing a
// decoder bug's clothes.
func TestScrcpySocketNobodyPublishedIsRefused(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.2"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: devpath, VideoEOF: true}))

	spawn := streamWire(t, s, devpath, spawnService(testSCID))
	readShellPacket(t, spawn)

	const wrong = "localabstract:scrcpy_deadbeef"
	w := streamWire(t, s, devpath, wrong)
	// Assert on the BYTES, not on how the socket died.
	//
	// This read "err == nil" until the fake stopped resetting a connection it
	// had written nothing on. A reset discards what the peer has not yet read,
	// and the four bytes at the front of that queue are the protocol's own
	// OKAY — so a refusal that reset immediately raced its own acceptance and
	// the suite failed about one run in twenty on whichever the kernel chose.
	//
	// What the test actually cares about is that a client asking for a socket
	// nobody published gets nothing usable and can find out why. Whether the
	// nothing arrives as a reset or as a clean EOF is a property of TCP timing,
	// and asserting on it was asserting on the weather.
	if got, _ := io.ReadAll(w.br); len(got) != 0 {
		t.Fatalf("a connection to a session id nobody published was served %d bytes", len(got))
	}
	reply := lastReply(t, s, wrong)
	if !strings.HasPrefix(reply, "ERROR: ") || !strings.Contains(reply, "scrcpy_"+testSCID) {
		t.Fatalf("the request log recorded %q; it should name the id this session did publish", reply)
	}
}

// TestScrcpyThirdSocketIsRefused pins the video-then-control ordering. scrcpy
// publishes one socket per stream and closes the listener behind it, so a
// third connection is a client that reconnected without respawning — and
// answering it with a second video stream would hide that.
func TestScrcpyThirdSocketIsRefused(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.3"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: devpath, VideoEOF: true}))
	scid, ok := s.ScrcpySCID(devpath)
	if !ok {
		t.Fatalf("the fixture published no default session id for %s", devpath)
	}
	// No spawn at all: the configured session id is live from the start, so a
	// test of the framing alone does not have to pretend to start a server.
	service := "localabstract:scrcpy_" + scid

	video := streamWire(t, s, devpath, service)
	if _, err := io.ReadAll(video.br); err != nil {
		t.Fatalf("the video socket: %v", err)
	}
	control := streamWire(t, s, devpath, service)
	if err := control.c.Close(); err != nil {
		t.Fatalf("closing the control socket: %v", err)
	}

	third := streamWire(t, s, devpath, service)
	// The bytes, not the manner of death — see the note in
	// TestScrcpySocketNobodyPublishedIsRefused.
	if got, _ := io.ReadAll(third.br); len(got) != 0 {
		t.Fatalf("a third socket was served %d bytes; scrcpy publishes two and closes the "+
			"listener behind them, so a third connection is a client that reconnected without "+
			"respawning and answering it would hide that", len(got))
	}
	// No polling: the refusal is filed before the socket is cut, so a client
	// that has seen the end has necessarily seen the record land.
	if reply := lastReply(t, s, service); !strings.HasPrefix(reply, "ERROR: ") {
		t.Fatalf("the third connection was recorded as %q, want a named refusal", reply)
	}
}

// TestScrcpyControlMessageLenFramesWholeMessages exercises the length table
// directly, including the two variable-length forms and the case it refuses
// to guess at.
func TestScrcpyControlMessageLenFramesWholeMessages(t *testing.T) {
	t.Parallel()

	text := textMessage("hi there")
	clip := setClipboardMessage(7, true, "paste me")
	msgs := [][]byte{
		keycodeMessage(1, 66),
		text,
		touchMessage(2, 10, 20, 1080, 2400),
		scrollMessage(10, 20, 1080, 2400),
		{wireMsgBack, 1},
		{wireMsgNotifs},
		{wireMsgSettings},
		{wireMsgCollapse},
		{wireMsgGetClip, 0},
		clip,
		{wireMsgPower, 1},
		{wireMsgRotate},
	}

	var all []byte
	for _, m := range msgs {
		all = append(all, m...)
	}

	// Whole messages, one at a time, out of one undivided recording.
	rest := all
	for i, want := range msgs {
		n, ok := ScrcpyControlMessageLen(rest)
		if !ok {
			t.Fatalf("message %d (type %d): the framer wanted more bytes than the %d it had", i, want[0], len(rest))
		}
		if n != len(want) {
			t.Fatalf("message %d (type %d): framed %d bytes, want %d", i, want[0], n, len(want))
		}
		if !bytes.Equal(rest[:n], want) {
			t.Fatalf("message %d framed %x, want %x", i, rest[:n], want)
		}
		rest = rest[n:]
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes left over after framing every message", len(rest))
	}

	// A message that has not fully arrived is not framed, at any truncation.
	for cut := 1; cut < len(text); cut++ {
		if n, ok := ScrcpyControlMessageLen(text[:cut]); ok {
			t.Fatalf("a %d-byte prefix of a %d-byte text message framed as %d bytes",
				cut, len(text), n)
		}
	}

	// An unknown type is not guessed at: the rest comes back whole, so a test
	// fails holding the bytes rather than a plausible-looking split.
	unknown := append([]byte{200}, []byte("whatever follows")...)
	if n, ok := ScrcpyControlMessageLen(unknown); !ok || n != len(unknown) {
		t.Fatalf("an unknown type framed as (%d, %t), want the whole remainder", n, ok)
	}

	// And a length field that cannot be one is treated the same way rather
	// than parked waiting for a gigabyte that is not coming.
	desync := make([]byte, 16)
	desync[0] = wireMsgText
	binary.BigEndian.PutUint32(desync[1:5], 0xffffffff)
	if n, ok := ScrcpyControlMessageLen(desync); !ok || n != len(desync) {
		t.Fatalf("a desynchronised length framed as (%d, %t), want the whole remainder", n, ok)
	}
}

// TestControlWritesHoldsBackAPartialMessage keeps a half-arrived frame out of
// the answer. Handing one back short would let an assertion pass against a
// message the client had not finished sending.
func TestControlWritesHoldsBackAPartialMessage(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.4"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: devpath, VideoEOF: true}))
	scid, _ := s.ScrcpySCID(devpath)
	service := "localabstract:scrcpy_" + scid

	video := streamWire(t, s, devpath, service)
	if _, err := io.ReadAll(video.br); err != nil {
		t.Fatalf("the video socket: %v", err)
	}

	whole := keycodeMessage(0, 4)
	half := touchMessage(0, 1, 2, 1080, 2400)[:10]
	control := streamWire(t, s, devpath, service)
	if _, err := control.c.Write(append(append([]byte(nil), whole...), half...)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := control.c.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	waitFor(t, func() bool { return len(s.ControlBytes(devpath)) == len(whole)+len(half) },
		func() string { return fmt.Sprintf("recorded %x", s.ControlBytes(devpath)) })

	got := s.ControlWrites(devpath)
	if len(got) != 1 || !bytes.Equal(got[0], whole) {
		t.Fatalf("ControlWrites = %x, want just the complete message %x", got, whole)
	}
	// The bytes are not lost, they are merely not a message yet.
	if n := len(s.ControlBytes(devpath)); n != len(whole)+len(half) {
		t.Fatalf("ControlBytes holds %d bytes, want %d", n, len(whole)+len(half))
	}
}

// TestControlWritesIsEmptyForAnUnscriptedDevice keeps the accessor honest for
// a devpath that carries no scrcpy fixture, so a test that names the wrong
// position sees nothing rather than somebody else's traffic.
func TestControlWritesIsEmptyForAnUnscriptedDevice(t *testing.T) {
	t.Parallel()

	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: "usb:5-1.5"}))
	if got := s.ControlWrites("usb:9-9.9"); got != nil {
		t.Fatalf("ControlWrites for an unknown position = %x", got)
	}
	if _, ok := s.ScrcpySCID("usb:9-9.9"); ok {
		t.Fatalf("ScrcpySCID reported a session id for a position with no fixture")
	}
}

// ---------------------------------------------------------------------
// Control message encoders, written here rather than shared with the fixture
// ---------------------------------------------------------------------

func keycodeMessage(action byte, keycode uint32) []byte {
	b := make([]byte, 14)
	b[0] = wireMsgKeycode
	b[1] = action
	binary.BigEndian.PutUint32(b[2:6], keycode)
	binary.BigEndian.PutUint32(b[6:10], 0)  // repeat
	binary.BigEndian.PutUint32(b[10:14], 0) // metastate
	return b
}

func textMessage(text string) []byte {
	b := make([]byte, 5, 5+len(text))
	b[0] = wireMsgText
	binary.BigEndian.PutUint32(b[1:5], uint32(len(text)))
	return append(b, text...)
}

func touchMessage(action byte, x, y int32, w, h uint16) []byte {
	b := make([]byte, 32)
	b[0] = wireMsgTouch
	b[1] = action
	binary.BigEndian.PutUint64(b[2:10], 0xffffffffffffffff) // pointer id
	binary.BigEndian.PutUint32(b[10:14], uint32(x))
	binary.BigEndian.PutUint32(b[14:18], uint32(y))
	binary.BigEndian.PutUint16(b[18:20], w)
	binary.BigEndian.PutUint16(b[20:22], h)
	binary.BigEndian.PutUint16(b[22:24], 0xffff) // pressure
	binary.BigEndian.PutUint32(b[24:28], 0)      // action button
	binary.BigEndian.PutUint32(b[28:32], 0)      // buttons
	return b
}

func scrollMessage(x, y int32, w, h uint16) []byte {
	b := make([]byte, 21)
	b[0] = wireMsgScroll
	binary.BigEndian.PutUint32(b[1:5], uint32(x))
	binary.BigEndian.PutUint32(b[5:9], uint32(y))
	binary.BigEndian.PutUint16(b[9:11], w)
	binary.BigEndian.PutUint16(b[11:13], h)
	binary.BigEndian.PutUint16(b[13:15], 0) // hscroll
	binary.BigEndian.PutUint16(b[15:17], 1) // vscroll
	binary.BigEndian.PutUint32(b[17:21], 0) // buttons
	return b
}

func setClipboardMessage(sequence uint64, paste bool, text string) []byte {
	b := make([]byte, 14, 14+len(text))
	b[0] = wireMsgSetClip
	binary.BigEndian.PutUint64(b[1:9], sequence)
	if paste {
		b[9] = 1
	}
	binary.BigEndian.PutUint32(b[10:14], uint32(len(text)))
	return append(b, text...)
}

// ---------------------------------------------------------------------
// Waiting
// ---------------------------------------------------------------------

// waitFor polls until cond holds. It is a failure guard, not a
// synchronisation device: what it waits for is a handler on the other side of
// a closed socket finishing its last append, which is bounded by nothing this
// side can observe directly.
func waitFor(tb testing.TB, cond func() bool, detail func() string) {
	tb.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("condition never held: %s", detail())
		}
		time.Sleep(time.Millisecond)
	}
}
