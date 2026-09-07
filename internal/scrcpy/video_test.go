package scrcpy

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// stream turns a hex string written out in a test into a reader. Every stream
// below is a literal: the point of a decoder test is that the bytes were chosen
// by hand and match a hex dump of the real thing, not that they were produced
// by an encoder in the same package.
func stream(t *testing.T, s string) io.Reader {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad literal in this test: %v", err)
	}
	return bytes.NewReader(b)
}

// A hand-built stream: h264, one session header for a 1080x1920 frame, then
// three packets.
//
//	68323634                              codec id, "h264"
//	80 00 00 00 00000438 00000780         session: bit 63 set, 1080x1920
//	4000000000000000 00000004 deadbeef    packet: CONFIG, 4 bytes
//	20000000000f4240 00000003 010203      packet: KEY, pts 1000000, 3 bytes
//	00000000001e8480 00000002 aabb        packet: pts 2000000, 2 bytes
const threePacketStream = "68323634" +
	"800000000000043800000780" +
	"400000000000000000000004deadbeef" +
	"20000000000f424000000003010203" +
	"00000000001e848000000002aabb"

// TestReaderDecodesAHandBuiltStream.
//
// A session header and three packets, decoded to three payloads with the flags
// and timestamps the header bits say. The layout was read out of scrcpy's
// app/src/demuxer.c: the top bit of the twelve-byte header distinguishes a
// session header from a packet header, then CONFIG at bit 62, KEY_FRAME at bit
// 61 and sixty-one bits of microseconds under them.
func TestReaderDecodesAHandBuiltStream(t *testing.T) {
	r, err := NewReader(stream(t, threePacketStream))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.Codec() != CodecH264 {
		t.Errorf("Codec() = %s, want h264", r.Codec())
	}

	first, err := r.Next()
	if err != nil {
		t.Fatalf("Next() for the session header: %v", err)
	}
	if first.Kind != KindSession {
		t.Fatalf("first unit is Kind %d, want KindSession; the top bit of its header is set", first.Kind)
	}
	if first.Session.Width != 1080 || first.Session.Height != 1920 {
		t.Errorf("session = %dx%d, want 1080x1920", first.Session.Width, first.Session.Height)
	}
	if first.Session.ClientResized {
		t.Error("session reports ClientResized; byte 3 of the header is zero")
	}

	want := []struct {
		payload  string
		config   bool
		keyFrame bool
		pts      uint64
	}{
		{"deadbeef", true, false, 0},
		{"010203", false, true, 1_000_000},
		{"aabb", false, false, 2_000_000},
	}
	for i, w := range want {
		u, err := r.Next()
		if err != nil {
			t.Fatalf("Next() for packet %d: %v", i, err)
		}
		if u.Kind != KindPacket {
			t.Fatalf("unit %d is Kind %d, want KindPacket", i, u.Kind)
		}
		if got := hex.EncodeToString(u.Packet.Payload); got != w.payload {
			t.Errorf("packet %d payload = %s, want %s", i, got, w.payload)
		}
		if u.Packet.Config != w.config {
			t.Errorf("packet %d Config = %v, want %v", i, u.Packet.Config, w.config)
		}
		if u.Packet.KeyFrame != w.keyFrame {
			t.Errorf("packet %d KeyFrame = %v, want %v", i, u.Packet.KeyFrame, w.keyFrame)
		}
		if u.Packet.PTS != w.pts {
			t.Errorf("packet %d PTS = %d, want %d", i, u.Packet.PTS, w.pts)
		}
	}

	// The stream ends between units, which is a server that exited rather than
	// a server that was cut off. Those must not be the same error.
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next() at a clean end = %v, want io.EOF", err)
	}
}

// TestSessionHeaderArrivesAgainOnRotation.
//
// The design document describes the session header as a one-time preamble.
// scrcpy's demuxer reads a header in a loop and dispatches on its top bit every
// time, and sends a fresh session header whenever the video size changes — which
// on a handset is every rotation.
//
// A reader that assumed packets after the first session header would read this
// rotation's width as a PTS and its height as a payload length. 1920 bytes is a
// plausible-looking frame, so it would not fail. It would emit garbage.
func TestSessionHeaderArrivesAgainOnRotation(t *testing.T) {
	const rotating = "68323634" +
		"800000000000043800000780" + // 1080x1920 portrait
		"20000000000f424000000002aabb" + // one key frame
		"800000010000078000000438" + // 1920x1080 landscape, client_resized
		"20000000001e848000000002ccdd" // another key frame

	r, err := NewReader(stream(t, rotating))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	kinds := []Kind{KindSession, KindPacket, KindSession, KindPacket}
	var sessions []Session
	for i, want := range kinds {
		u, err := r.Next()
		if err != nil {
			t.Fatalf("Next() at unit %d: %v", i, err)
		}
		if u.Kind != want {
			t.Fatalf("unit %d is Kind %d, want %d", i, u.Kind, want)
		}
		if u.Kind == KindSession {
			sessions = append(sessions, u.Session)
		}
	}
	if len(sessions) != 2 {
		t.Fatalf("saw %d session headers, want 2", len(sessions))
	}
	if sessions[1].Width != 1920 || sessions[1].Height != 1080 {
		t.Errorf("the second session is %dx%d, want 1920x1080",
			sessions[1].Width, sessions[1].Height)
	}
	if !sessions[1].ClientResized {
		t.Error("the second session does not report ClientResized; bit 0 of byte 3 is set")
	}

	// And the point of decoding it: the coordinate space follows the rotation.
	screen, err := ScreenFromSession(sessions[1])
	if err != nil {
		t.Fatalf("ScreenFromSession: %v", err)
	}
	if _, err := screen.At(1900, 100); err != nil {
		t.Errorf("At(1900,100) on the landscape frame: %v; the screen did not follow the rotation", err)
	}
	if _, err := screen.At(100, 1900); err == nil {
		t.Error("At(100,1900) succeeded on a 1920x1080 frame; the screen is still portrait")
	}
}

// TestTruncationIsNeverAShortFrame.
//
// Every one of these must be io.ErrUnexpectedEOF and never a Unit. A payload
// that stopped after zero bytes is as truncated as one that stopped after
// three: the header promised a count, and handing a decoder a fragment of a
// frame and calling it a frame is the failure this rule exists to prevent.
func TestTruncationIsNeverAShortFrame(t *testing.T) {
	for _, c := range []struct {
		name string
		hex  string
	}{
		{"one byte into the codec id", "68"},
		{"one byte into a header", "68323634" + "80"},
		{"eleven bytes into a header", "68323634" + "8000000000000438000007"},
		{"a header with no payload at all", "68323634" + "800000000000043800000780" +
			"200000000000000000000004"},
		{"a payload that stopped short", "68323634" + "800000000000043800000780" +
			"200000000000000000000004" + "dead"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := NewReader(stream(t, c.hex))
			if err != nil {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("NewReader = %v, want io.ErrUnexpectedEOF", err)
				}
				return
			}
			for {
				u, err := r.Next()
				if err == nil {
					if u.Kind == KindPacket && len(u.Packet.Payload) == 0 {
						t.Fatal("Next() returned a packet with an empty payload")
					}
					continue
				}
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("Next() = %v, want io.ErrUnexpectedEOF", err)
				}
				return
			}
		})
	}
}

// TestUnknownCodecIsATypedErrorNamingIt.
//
// The two failures this distinguishes look different in the two renderings the
// error carries. "opus" is a recognisable word: somebody wired the control
// server's audio socket to the video reader. Noise in hex is a stream that
// began mid-frame.
func TestUnknownCodecIsATypedErrorNamingIt(t *testing.T) {
	for _, c := range []struct {
		name string
		hex  string
		id   CodecID
	}{
		{"an audio codec", "6f707573", 0x6f707573},
		{"noise", "deadbeef", 0xdeadbeef},
		{"zero", "00000000", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewReader(stream(t, c.hex))
			var unknown *UnknownCodecError
			if !errors.As(err, &unknown) {
				t.Fatalf("NewReader = %v, want *UnknownCodecError", err)
			}
			if unknown.ID != c.id {
				t.Errorf("UnknownCodecError names 0x%08x, want 0x%08x", uint32(unknown.ID), uint32(c.id))
			}
		})
	}

	// And every codec this package claims to know is accepted, so that "not
	// known" means what it says rather than "not in the switch I remembered".
	for _, id := range []CodecID{CodecH264, CodecH265, CodecAV1, CodecVP8, CodecVP9} {
		b := []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
		r, err := NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("NewReader for %s: %v", id, err)
		}
		if r.Codec() != id {
			t.Errorf("Codec() = %s, want %s", r.Codec(), id)
		}
	}
}

// endlessReader would serve a gigabyte, or a terabyte, without complaining. It
// counts what it was actually asked for.
type endlessReader struct{ served int }

func (e *endlessReader) Read(p []byte) (int, error) {
	e.served += len(p)
	return len(p), nil
}

// TestAnOversizedLengthIsRefusedBeforeItAllocates.
//
// This is the assertion the whole package is arranged around. A u32 off the
// wire must not reach make(), because this code will run inside the process
// that answers POST /api/v1/leases/{id}/renew and an out-of-memory kill there
// costs every lease in the farm.
//
// The proof is not that an error came back. It is that the endless reader was
// never asked for a byte: the header came from the prefix, the cap refused the
// length, and nothing was read or allocated afterwards. A reader that allocated
// first and refused second would still return this error, and would still be
// the bug.
func TestAnOversizedLengthIsRefusedBeforeItAllocates(t *testing.T) {
	for _, c := range []struct {
		name   string
		length uint32
	}{
		{"one byte over the cap", MaxPacket + 1},
		{"a gigabyte", 1 << 30},
		{"the largest u32 there is", 0xffffffff},
	} {
		t.Run(c.name, func(t *testing.T) {
			hdr := []byte{
				0, 0, 0, 0, 0, 0, 0, 0,
				byte(c.length >> 24), byte(c.length >> 16), byte(c.length >> 8), byte(c.length),
			}
			prefix := append([]byte("h264"), hdr...)

			endless := &endlessReader{}
			r, err := NewReader(io.MultiReader(bytes.NewReader(prefix), endless))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}

			_, err = r.Next()
			var tooLarge *PacketTooLargeError
			if !errors.As(err, &tooLarge) {
				t.Fatalf("Next() = %v, want *PacketTooLargeError", err)
			}
			if tooLarge.Length != c.length {
				t.Errorf("PacketTooLargeError names %d bytes, want %d", tooLarge.Length, c.length)
			}
			if tooLarge.Max != MaxPacket {
				t.Errorf("PacketTooLargeError reports a cap of %d, want %d", tooLarge.Max, MaxPacket)
			}
			if endless.served != 0 {
				t.Errorf("the reader was asked for %d bytes of payload after a length of %d was "+
					"refused; the cap must be checked before anything is read or allocated",
					endless.served, c.length)
			}
		})
	}
}

// TestTheLargestAdmissiblePacketIsStillAdmitted.
//
// A cap that refuses the value at the cap is an off-by-one that would drop
// large key frames on a good stream, and it would present as intermittent
// video corruption rather than as a refusal. This is cheap to check and the
// only way to tell the two boundaries apart.
func TestTheLargestAdmissiblePacketIsStillAdmitted(t *testing.T) {
	n := uint32(MaxPacket)
	body := make([]byte, n)
	body[0] = 0x5a
	body[n-1] = 0xa5

	var buf bytes.Buffer
	buf.WriteString("h264")
	buf.Write([]byte{
		0x20, 0, 0, 0, 0, 0, 0, 0,
		byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
	})
	buf.Write(body)

	r, err := NewReader(&buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	u, err := r.Next()
	if err != nil {
		t.Fatalf("Next() for a packet of exactly MaxPacket bytes: %v", err)
	}
	if uint32(len(u.Packet.Payload)) != n {
		t.Fatalf("payload is %d bytes, want %d", len(u.Packet.Payload), n)
	}
	if u.Packet.Payload[0] != 0x5a || u.Packet.Payload[n-1] != 0xa5 {
		t.Error("the payload's first and last bytes did not survive the read")
	}
}

// TestZeroLengthPacketIsRefused.
//
// There is no such thing as an empty access unit. scrcpy's own demuxer says the
// same thing ("Invalid packet length: 0"), and it means the parse has come
// adrift rather than that a frame was small — so returning an empty packet
// would hand a decoder nothing and let the loop spin on a desynchronised
// stream forever.
func TestZeroLengthPacketIsRefused(t *testing.T) {
	r, err := NewReader(stream(t, "68323634"+"200000000000000000000000"+"deadbeef"))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrEmptyPacket) {
		t.Errorf("Next() = %v, want ErrEmptyPacket", err)
	}
}

// TestFlagBitsAreReadWhereScrcpyPutsThem.
//
// Sixty-one bits of PTS under three flag bits, and the boundary between them is
// the thing a hand-written mask gets wrong. The all-ones PTS below is the case
// that catches a mask one bit too wide: it would read the KEY_FRAME bit as part
// of the timestamp.
func TestFlagBitsAreReadWhereScrcpyPutsThem(t *testing.T) {
	for _, c := range []struct {
		name     string
		head     string
		config   bool
		keyFrame bool
		pts      uint64
	}{
		{"nothing set", "0000000000000000", false, false, 0},
		{"config only", "4000000000000000", true, false, 0},
		{"key frame only", "2000000000000000", false, true, 0},
		{"both flags", "6000000000000000", true, true, 0},
		{"the largest PTS", "1fffffffffffffff", false, false, 1<<61 - 1},
		{"the largest PTS with both flags", "7fffffffffffffff", true, true, 1<<61 - 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := NewReader(stream(t, "68323634"+c.head+"00000001"+"7f"))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			u, err := r.Next()
			if err != nil {
				t.Fatalf("Next(): %v", err)
			}
			if u.Kind != KindPacket {
				t.Fatalf("Kind = %d, want KindPacket; the top bit of %s is clear", u.Kind, c.head)
			}
			if u.Packet.Config != c.config || u.Packet.KeyFrame != c.keyFrame {
				t.Errorf("Config=%v KeyFrame=%v, want %v and %v",
					u.Packet.Config, u.Packet.KeyFrame, c.config, c.keyFrame)
			}
			if u.Packet.PTS != c.pts {
				t.Errorf("PTS = %d (0x%x), want %d (0x%x)", u.Packet.PTS, u.Packet.PTS, c.pts, c.pts)
			}
		})
	}
}

// TestPayloadsDoNotShareABuffer.
//
// Each packet's payload is the caller's to keep — it is handed to a browser
// through a queue that may outlive the next read. A reader that reused one
// scratch buffer would corrupt frames already in flight, and it would do so
// only under load.
func TestPayloadsDoNotShareABuffer(t *testing.T) {
	r, err := NewReader(stream(t, threePacketStream))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var held [][]byte
	for {
		u, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next(): %v", err)
		}
		if u.Kind == KindPacket {
			held = append(held, u.Packet.Payload)
		}
	}
	if len(held) != 3 {
		t.Fatalf("held %d payloads, want 3", len(held))
	}
	want := []string{"deadbeef", "010203", "aabb"}
	for i, p := range held {
		if got := hex.EncodeToString(p); got != want[i] {
			t.Errorf("payload %d, read back after the whole stream, is %s; want %s", i, got, want[i])
		}
	}
}
