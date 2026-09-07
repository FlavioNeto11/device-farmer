package scrcpy

import (
	"encoding/hex"
	"errors"
	"math"
	"testing"
)

// screenOf builds a Screen the only way one can be built, so that every test
// below goes through the same door a caller does.
func screenOf(t *testing.T, w, h uint32) Screen {
	t.Helper()
	s, err := ScreenFromSession(Session{Width: w, Height: h})
	if err != nil {
		t.Fatalf("ScreenFromSession(%dx%d): %v", w, h, err)
	}
	return s
}

func positionOf(t *testing.T, s Screen, x, y int32) Position {
	t.Helper()
	p, err := s.At(x, y)
	if err != nil {
		t.Fatalf("Screen.At(%d,%d): %v", x, y, err)
	}
	return p
}

// TestEncodersMatchScrcpysOwnGoldenBytes.
//
// These three cases are scrcpy's app/tests/test_control_msg_serialize.c,
// transcribed: the same inputs, the same expected bytes. They are the only
// assertions in this package that can catch a field written in the wrong order
// or the wrong width, because the control channel has no length prefix and no
// acknowledgement — the server will happily inject events read out of a
// shifted buffer, and a phone somebody is holding is where that shows up.
//
// The expected strings are written out rather than computed. A test that
// derives its expectation from the code under test asserts that the code agrees
// with itself.
func TestEncodersMatchScrcpysOwnGoldenBytes(t *testing.T) {
	screen := screenOf(t, 1080, 1920)

	t.Run("inject_keycode", func(t *testing.T) {
		// action UP, AKEYCODE_ENTER (0x42), repeat 5,
		// AMETA_SHIFT_ON|AMETA_SHIFT_LEFT_ON (0x41).
		got := KeyEvent{Action: KeyUp, Keycode: 0x42, Repeat: 5, MetaState: 0x41}.Encode()
		const want = "00010000004200000005" + "00000041"
		if h := hex.EncodeToString(got); h != want {
			t.Errorf("KeyEvent.Encode() = %s, want %s", h, want)
		}
		if len(got) != KeycodeMsgLen {
			t.Errorf("KeyEvent.Encode() is %d bytes, want %d", len(got), KeycodeMsgLen)
		}
	})

	t.Run("inject_touch_event", func(t *testing.T) {
		got, err := TouchEvent{
			Action:       TouchDown,
			PointerID:    0x1234567887654321,
			Position:     positionOf(t, screen, 100, 200),
			Pressure:     PressureFull,
			ActionButton: 1,
			Buttons:      1,
		}.Encode()
		if err != nil {
			t.Fatalf("TouchEvent.Encode(): %v", err)
		}
		const want = "0200" + "1234567887654321" +
			"00000064" + "000000c8" + "0438" + "0780" +
			"ffff" + "00000001" + "00000001"
		if h := hex.EncodeToString(got); h != want {
			t.Errorf("TouchEvent.Encode() = %s, want %s", h, want)
		}
		if len(got) != TouchMsgLen {
			t.Errorf("TouchEvent.Encode() is %d bytes, want %d", len(got), TouchMsgLen)
		}
	})

	t.Run("inject_scroll_event", func(t *testing.T) {
		got, err := ScrollEvent{
			Position:   positionOf(t, screen, 260, 1026),
			Horizontal: ScrollForward,
			Vertical:   ScrollBack,
			Buttons:    1,
		}.Encode()
		if err != nil {
			t.Fatalf("ScrollEvent.Encode(): %v", err)
		}
		const want = "03" + "00000104" + "00000402" + "0438" + "0780" +
			"7fff" + "8000" + "00000001"
		if h := hex.EncodeToString(got); h != want {
			t.Errorf("ScrollEvent.Encode() = %s, want %s", h, want)
		}
		if len(got) != ScrollMsgLen {
			t.Errorf("ScrollEvent.Encode() is %d bytes, want %d", len(got), ScrollMsgLen)
		}
	})
}

// TestMessageSizesAreTheOnesScrcpyAsserts pins the constants themselves.
//
// The encoders above are checked against golden bytes, so this looks redundant
// — until somebody changes a message and its constant together, at which point
// this is the assertion that says the wire format is not ours to change.
func TestMessageSizesAreTheOnesScrcpyAsserts(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"KeycodeMsgLen", KeycodeMsgLen, 14},
		{"TouchMsgLen", TouchMsgLen, 32},
		{"ScrollMsgLen", ScrollMsgLen, 21},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (app/tests/test_control_msg_serialize.c)", c.name, c.got, c.want)
		}
	}
}

// TestTouchTravelsInVideoPixelsNotDevicePixels.
//
// This is the assertion the adversarial review in
// docs/design/interactive-control.md §3 forced, and it is the one that would
// have caught the wrong design.
//
// The device is a 1080x1920 handset. The stream was started with --max-size 540,
// so the session header says 540x960 and every frame the human sees is 540x960.
// The human taps the middle of that frame, at (270, 480).
//
// What must go on the wire is 270, 480, 540, 960 — the numbers from the frame
// they were looking at. NOT 540, 960, 1080, 1920, which is what a mapper that
// "helpfully" rescales into device pixels would send, and not 270, 480, 1080,
// 1920, which is what a mapper that kept video coordinates but reported the
// device size would send. The second of those is the nastier bug: the
// coordinates are right, so it lands correctly on an undownscaled stream and
// wrong on every other one, and the size field exists precisely so the server
// can notice — which it would, by discarding the event as stale.
func TestTouchTravelsInVideoPixelsNotDevicePixels(t *testing.T) {
	const (
		deviceWidth, deviceHeight = 1080, 1920
		videoWidth, videoHeight   = 540, 960
	)

	video := screenOf(t, videoWidth, videoHeight)
	got, err := TouchEvent{
		Action:    TouchDown,
		PointerID: PointerGenericFinger,
		Position:  positionOf(t, video, 270, 480),
		Pressure:  PressureFull,
	}.Encode()
	if err != nil {
		t.Fatalf("TouchEvent.Encode(): %v", err)
	}

	// 270 = 0x10e, 480 = 0x1e0, 540 = 0x21c, 960 = 0x3c0.
	const want = "0200" + "fffffffffffffffe" +
		"0000010e" + "000001e0" + "021c" + "03c0" +
		"ffff" + "00000000" + "00000000"
	if h := hex.EncodeToString(got); h != want {
		t.Fatalf("tap at the centre of a %dx%d frame from a %dx%d device encoded as\n  %s\nwant\n  %s",
			videoWidth, videoHeight, deviceWidth, deviceHeight, h, want)
	}

	// Said again as numbers, so a failure reads as an accusation rather than as
	// a hex diff: neither the point nor the size may have picked up the
	// device's resolution.
	if s := video.Width(); s != videoWidth {
		t.Errorf("Screen.Width() = %d, want the video width %d", s, videoWidth)
	}
	if s := video.Height(); s != videoHeight {
		t.Errorf("Screen.Height() = %d, want the video height %d", s, videoHeight)
	}
}

// TestScreenComesOnlyFromASession.
//
// A Screen built from a rotation's session header describes the frame after the
// rotation, and one built from the previous header describes a frame nobody is
// looking at. Both are legal values; what must not be legal is a Screen that
// came from neither — which is enforced by there being no other constructor,
// and checked here by the zero value being refused everywhere it could be used.
func TestScreenComesOnlyFromASession(t *testing.T) {
	var zero Screen
	if zero.Valid() {
		t.Error("the zero Screen reports itself valid; it came from no session header")
	}
	if _, err := zero.At(0, 0); err == nil {
		t.Error("Screen{}.At(0,0) succeeded; a point on a 0x0 frame is not a point")
	}

	var noPosition TouchEvent
	if _, err := noPosition.Encode(); !errors.Is(err, ErrNoPosition) {
		t.Errorf("TouchEvent{}.Encode() error = %v, want ErrNoPosition", err)
	}
	if _, err := (ScrollEvent{}).Encode(); !errors.Is(err, ErrNoPosition) {
		t.Errorf("ScrollEvent{}.Encode() error = %v, want ErrNoPosition", err)
	}

	// AppendTo must leave the caller's buffer alone when it refuses. A caller
	// batching events into one write cannot unwind a partial message, and a
	// partial control message desynchronises every message after it.
	dst := []byte{0xaa, 0xbb}
	out, err := noPosition.AppendTo(dst)
	if err == nil {
		t.Fatal("TouchEvent{}.AppendTo succeeded")
	}
	if len(out) != len(dst) {
		t.Errorf("AppendTo returned %d bytes on error, want the %d it was given", len(out), len(dst))
	}
}

// TestScreenFromSessionRefusesSizesAControlMessageCannotCarry.
//
// The video framing carries the size in 32 bits and a control message carries
// it in 16. Truncating would produce a staleness guard that matches the wrong
// frame, which is worse than refusing to point at the stream at all.
func TestScreenFromSessionRefusesSizesAControlMessageCannotCarry(t *testing.T) {
	for _, c := range []struct {
		name string
		s    Session
		ok   bool
	}{
		{"zero", Session{}, false},
		{"zero width", Session{Width: 0, Height: 1920}, false},
		{"zero height", Session{Width: 1080, Height: 0}, false},
		{"4K portrait", Session{Width: 2160, Height: 3840}, true},
		{"the largest representable", Session{Width: math.MaxUint16, Height: math.MaxUint16}, true},
		{"one past it", Session{Width: math.MaxUint16 + 1, Height: 1080}, false},
		{"a wedged u32", Session{Width: 0xffffffff, Height: 0xffffffff}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ScreenFromSession(c.s)
			if c.ok {
				if err != nil {
					t.Fatalf("ScreenFromSession(%+v) = %v, want a screen", c.s, err)
				}
				if uint32(got.Width()) != c.s.Width || uint32(got.Height()) != c.s.Height {
					t.Errorf("ScreenFromSession(%+v) = %dx%d; the size must survive unchanged",
						c.s, got.Width(), got.Height())
				}
				return
			}
			var sizeErr *SessionSizeError
			if !errors.As(err, &sizeErr) {
				t.Fatalf("ScreenFromSession(%+v) error = %v, want *SessionSizeError", c.s, err)
			}
			if sizeErr.Width != c.s.Width || sizeErr.Height != c.s.Height {
				t.Errorf("SessionSizeError names %dx%d, want the %dx%d that arrived",
					sizeErr.Width, sizeErr.Height, c.s.Width, c.s.Height)
			}
		})
	}
}

// TestPointOutsideTheFrameIsRefusedNotClamped.
//
// Clamping would turn a client that is scaling wrongly into taps along the edge
// of the screen, which looks like a user doing something odd. Refusing makes it
// look like the bug it is.
func TestPointOutsideTheFrameIsRefusedNotClamped(t *testing.T) {
	s := screenOf(t, 540, 960)

	for _, c := range []struct {
		name string
		x, y int32
		ok   bool
	}{
		{"origin", 0, 0, true},
		{"last pixel", 539, 959, true},
		{"one past the right edge", 540, 480, false},
		{"one past the bottom edge", 270, 960, false},
		{"negative x", -1, 480, false},
		{"negative y", 270, -1, false},
		{"device pixels on a downscaled stream", 1079, 1919, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := s.At(c.x, c.y)
			if c.ok {
				if err != nil {
					t.Fatalf("At(%d,%d) = %v, want a position", c.x, c.y, err)
				}
				if p.X() != c.x || p.Y() != c.y {
					t.Errorf("At(%d,%d) holds (%d,%d); the point must survive unchanged",
						c.x, c.y, p.X(), p.Y())
				}
				return
			}
			var frameErr *OutOfFrameError
			if !errors.As(err, &frameErr) {
				t.Fatalf("At(%d,%d) error = %v, want *OutOfFrameError", c.x, c.y, err)
			}
			if frameErr.X != c.x || frameErr.Y != c.y {
				t.Errorf("OutOfFrameError names (%d,%d), want (%d,%d)", frameErr.X, frameErr.Y, c.x, c.y)
			}
		})
	}
}

// TestFixedPointConversionsMatchScrcpysArithmetic.
//
// sc_float_to_u16fp multiplies by 2^16 and clamps at 0xffff; sc_float_to_i16fp
// multiplies by 2^15 and clamps at both ends. scrcpy asserts its inputs are in
// range because they come from C one function away. Ours come from a browser,
// so out-of-range and NaN are clamped rather than asserted — a pointer event
// that reported something strange must not become a hard press.
func TestFixedPointConversionsMatchScrcpysArithmetic(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want Pressure
	}{
		{0, 0},
		{0.5, 0x8000},
		{1, 0xffff},
		{1.5, 0xffff},
		{-1, 0},
		{math.NaN(), 0},
	} {
		if got := PressureFromFloat(c.in); got != c.want {
			t.Errorf("PressureFromFloat(%v) = 0x%04x, want 0x%04x", c.in, uint16(got), uint16(c.want))
		}
	}

	for _, c := range []struct {
		in   float64
		want Scroll
	}{
		{0, 0},
		{0.5, 0x4000},
		{1, 0x7fff},
		{2, 0x7fff},
		{-0.5, -0x4000},
		{-1, -0x8000},
		{-2, -0x8000},
		{math.NaN(), 0},
	} {
		if got := ScrollFromFloat(c.in); got != c.want {
			t.Errorf("ScrollFromFloat(%v) = %d, want %d", c.in, int16(got), int16(c.want))
		}
	}
}

// TestAppendToProducesTheSameBytesAsEncode.
//
// The Append form exists so a caller writing a drag does not allocate per
// sample. Two encoders that can disagree are two wire formats.
func TestAppendToProducesTheSameBytesAsEncode(t *testing.T) {
	s := screenOf(t, 720, 1280)
	p := positionOf(t, s, 12, 34)

	key := KeyEvent{Action: KeyDown, Keycode: 4, Repeat: 1, MetaState: 2}
	if a, b := hex.EncodeToString(key.Encode()), hex.EncodeToString(key.AppendTo(nil)); a != b {
		t.Errorf("KeyEvent Encode %s != AppendTo %s", a, b)
	}

	touch := TouchEvent{Action: TouchMove, PointerID: PointerMouse, Position: p, Pressure: PressureFull}
	touchEnc, err := touch.Encode()
	if err != nil {
		t.Fatal(err)
	}
	touchApp, err := touch.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(touchEnc) != hex.EncodeToString(touchApp) {
		t.Errorf("TouchEvent Encode %x != AppendTo %x", touchEnc, touchApp)
	}

	scroll := ScrollEvent{Position: p, Vertical: ScrollFromFloat(-0.25)}
	scrollEnc, err := scroll.Encode()
	if err != nil {
		t.Fatal(err)
	}
	scrollApp, err := scroll.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(scrollEnc) != hex.EncodeToString(scrollApp) {
		t.Errorf("ScrollEvent Encode %x != AppendTo %x", scrollEnc, scrollApp)
	}

	// And appending to a non-empty buffer must neither disturb what was there
	// nor write different bytes than it would to an empty one.
	out, err := touch.AppendTo(key.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != KeycodeMsgLen+TouchMsgLen {
		t.Fatalf("a keycode followed by a touch is %d bytes, want %d",
			len(out), KeycodeMsgLen+TouchMsgLen)
	}
	if got, want := hex.EncodeToString(out[:KeycodeMsgLen]), hex.EncodeToString(key.Encode()); got != want {
		t.Errorf("AppendTo overwrote the message already in the buffer: %s, want %s", got, want)
	}
	if got, want := hex.EncodeToString(out[KeycodeMsgLen:]), hex.EncodeToString(touchEnc); got != want {
		t.Errorf("AppendTo to a non-empty buffer wrote %s, want %s", got, want)
	}
}
