package scrcpy

// The control channel: a type byte, then big-endian fields, then nothing. No
// length prefix, no framing, no acknowledgement — the message's type determines
// its length and both ends agree on the table.
//
// That is a hostile shape to get wrong. There is no resynchronisation point: a
// single message written one byte short or one byte long shifts every message
// after it, and the server keeps injecting events from the shifted bytes rather
// than failing. A phone somebody is holding starts receiving taps at addresses
// nobody chose. So the sizes below are constants, they are asserted against the
// bytes scrcpy's own tests assert (app/tests/test_control_msg_serialize.c), and
// every encoder writes exactly its constant.
//
// # The coordinate space, which the design document got wrong once
//
// A touch carries an (x, y) AND a (width, height). The obvious reading — that
// the pair is there so the server can rescale — is wrong, and acting on it
// sends taps to the wrong place on any stream that is not full device
// resolution. scrcpy's coordinates are in the VIDEO coordinate space: after
// --max-size has downscaled it, after --crop has cut it, after the rotation
// filter has turned it. The width and height that travel alongside are a
// STALENESS GUARD, so the server can notice that the client was looking at a
// differently-sized frame when the human pressed, and discard the event rather
// than land it somewhere plausible.
//
// docs/design/interactive-control.md §3 records this as a correction an
// adversarial review made to it. This file is the correction made structural:
// [Position] holds the point and the frame size together and can only be built
// from a [Screen], and a [Screen] can only be built from a [Session] that came
// off the video stream. There is no path from a device's physical resolution
// into these bytes, because there is no constructor that accepts one.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Message types, from scrcpy's enum sc_control_msg_type.
//
// The values are positional in that enum, which is why there is a gap: 1 is
// INJECT_TEXT, and the twenty or so above 3 are clipboard, UHID, app launching,
// display power and camera control. They are absent because each of them is a
// capability, and the set granted to whoever can reach this socket should be
// the set a screen and a hand need. Adding one is a decision, not a
// completeness exercise.
const (
	msgInjectKeycode = 0
	msgInjectTouch   = 2
	msgInjectScroll  = 3
)

// Encoded message sizes. These are asserted against scrcpy's own serialization
// tests; see control_test.go, which reproduces those tests' golden bytes.
const (
	// KeycodeMsgLen is type, action, keycode, repeat, metaState.
	KeycodeMsgLen = 14

	// TouchMsgLen is type, action, pointer id, position, pressure,
	// action button, buttons.
	TouchMsgLen = 32

	// ScrollMsgLen is type, position, horizontal, vertical, buttons. It has no
	// action byte, which is why it is 21 and not 22.
	ScrollMsgLen = 21
)

// KeyAction is Android's AKEY_EVENT_ACTION_*.
type KeyAction uint8

const (
	KeyDown KeyAction = 0
	KeyUp   KeyAction = 1
)

// TouchAction is Android's AMOTION_EVENT_ACTION_*. It is a different
// enumeration from [KeyAction] despite both being a byte in the same position
// of a message, and conflating them is how a tap becomes a key press.
type TouchAction uint8

const (
	TouchDown TouchAction = 0
	TouchUp   TouchAction = 1
	TouchMove TouchAction = 2
)

// Pointer identities scrcpy reserves, from control_msg.h. They are the
// two's-complement encodings of -1 and -2 in a u64, which is what the server
// compares against.
//
// A real multi-touch gesture uses small non-negative ids of the client's own
// choosing; these name the singular pointer a mouse-shaped client has.
const (
	PointerMouse         uint64 = ^uint64(0)     // -1
	PointerGenericFinger uint64 = ^uint64(0) - 1 // -2
)

// Pressure is scrcpy's unsigned 16-bit fixed-point pressure: 0 is no contact
// and 0xffff is full.
type Pressure uint16

const (
	// PressureNone is what an UP event carries: the finger has left the glass.
	PressureNone Pressure = 0

	// PressureFull is 1.0. Note that it is 0xffff and not 0x10000 — the
	// fixed-point scale is 2^16, so 1.0 does not fit and scrcpy clamps. That
	// off-by-one is in the format, not in this code.
	PressureFull Pressure = 0xffff
)

// PressureFromFloat converts a 0..1 pressure the way scrcpy's
// sc_float_to_u16fp does: multiply by 2^16 and clamp at 0xffff.
//
// scrcpy asserts its input is in range and this clamps instead, because the
// input here arrives from a browser rather than from C code one function away.
// A NaN or a negative from a pointer event that reported something strange must
// become no pressure, not a wild u16 that reads as a hard press.
func PressureFromFloat(f float64) Pressure {
	if math.IsNaN(f) || f <= 0 {
		return PressureNone
	}
	u := f * (1 << 16)
	if u >= 0xffff {
		return PressureFull
	}
	return Pressure(uint16(u))
}

// Scroll is scrcpy's signed 16-bit fixed-point scroll distance: -1.0 is
// 0x8000, +1.0 clamps to 0x7fff.
type Scroll int16

const (
	// ScrollForward is +1.0, and ScrollBack is -1.0. The asymmetry in the
	// underlying numbers — 0x7fff against -0x8000 — is the same clamp as
	// [PressureFull] and is likewise the format's, not this package's.
	ScrollForward Scroll = 0x7fff
	ScrollBack    Scroll = -0x8000
)

// ScrollFromFloat converts a -1..1 scroll the way scrcpy's sc_float_to_i16fp
// does: multiply by 2^15 and clamp at both ends, with the same reasoning about
// untrusted input as [PressureFromFloat].
func ScrollFromFloat(f float64) Scroll {
	if math.IsNaN(f) {
		return 0
	}
	i := f * (1 << 15)
	if i >= 0x7fff {
		return ScrollForward
	}
	if i <= -0x8000 {
		return ScrollBack
	}
	return Scroll(int16(i))
}

// ---------------------------------------------------------------------------
// The coordinate space
// ---------------------------------------------------------------------------

// Screen is the size of the video frame a human is looking at, and the only
// space a [Position] can be expressed in.
//
// There is deliberately no constructor taking two integers. The one way to
// obtain a Screen is [ScreenFromSession], which takes a [Session] — a value
// that exists only because [Reader.Next] decoded one off the video stream. That
// is the correction from docs/design/interactive-control.md §3 enforced by the
// type system rather than by a comment nobody reads: a caller cannot pass the
// device's physical resolution here, because a caller cannot manufacture the
// value this needs without parsing the frames the size describes.
type Screen struct {
	width  uint16
	height uint16
}

// Width and Height report the frame size, for a caller that needs to echo it
// to a browser.
func (s Screen) Width() uint16  { return s.width }
func (s Screen) Height() uint16 { return s.height }

// Valid reports whether this screen came from a session header. The zero value
// does not, and every encoder that carries a position refuses it.
func (s Screen) Valid() bool { return s.width > 0 && s.height > 0 }

// SessionSizeError is a session header whose dimensions cannot be expressed in
// a control message.
//
// The video framing carries the size as u32 and the control messages carry it
// as u16, so the two formats disagree about what is representable. Every real
// panel fits — 65535 is sixteen times a 4K edge — so this is not a limit anyone
// runs into. It exists because the alternative to refusing is truncating, and a
// truncated frame size is a staleness guard that silently matches the wrong
// frame.
type SessionSizeError struct {
	Width  uint32
	Height uint32
}

func (e *SessionSizeError) Error() string {
	return fmt.Sprintf("scrcpy: session header reports %dx%d; a control message carries the frame "+
		"size in 16 bits, so this stream cannot be pointed at (max %dx%d)",
		e.Width, e.Height, math.MaxUint16, math.MaxUint16)
}

// ScreenFromSession builds the coordinate space from a session header.
//
// Call it again on every [KindSession] unit. A handset that rotates sends a new
// session header mid-stream, and a Screen built from the previous one describes
// a frame nobody is looking at any more — which is precisely the condition the
// width and height in a touch message exist to let the server detect.
func ScreenFromSession(s Session) (Screen, error) {
	if s.Width == 0 || s.Height == 0 {
		return Screen{}, &SessionSizeError{Width: s.Width, Height: s.Height}
	}
	if s.Width > math.MaxUint16 || s.Height > math.MaxUint16 {
		return Screen{}, &SessionSizeError{Width: s.Width, Height: s.Height}
	}
	return Screen{width: uint16(s.Width), height: uint16(s.Height)}, nil
}

// Position is a point on a particular frame, carrying the frame's size with it.
//
// The fields are unexported and there is one constructor, so a point from one
// frame cannot be paired with the size of another. That pairing is the failure
// the staleness guard exists to catch on the server; making it unrepresentable
// on this side means the guard only ever fires for the reason it was designed
// for — the human clicked before a resize reached them — rather than for a bug
// in this process.
type Position struct {
	x      int32
	y      int32
	screen Screen
}

// X, Y and Screen report what this position holds.
func (p Position) X() int32       { return p.x }
func (p Position) Y() int32       { return p.y }
func (p Position) Screen() Screen { return p.screen }

// OutOfFrameError is a point outside the frame it claims to be on.
type OutOfFrameError struct {
	X, Y          int32
	Width, Height uint16
}

func (e *OutOfFrameError) Error() string {
	return fmt.Sprintf("scrcpy: point (%d,%d) is outside the %dx%d video frame; coordinates are in "+
		"video space after max-size, crop and rotation, not device pixels",
		e.X, e.Y, e.Width, e.Height)
}

// At places a point on this screen.
//
// A point outside the frame is refused rather than clamped. Clamping would turn
// a client that is scaling wrongly — the exact bug the video-space correction
// is about — into taps along the edge of the screen, which look like a user
// doing something odd rather than like a defect.
func (s Screen) At(x, y int32) (Position, error) {
	if !s.Valid() {
		return Position{}, &OutOfFrameError{X: x, Y: y, Width: s.width, Height: s.height}
	}
	if x < 0 || y < 0 || x >= int32(s.width) || y >= int32(s.height) {
		return Position{}, &OutOfFrameError{X: x, Y: y, Width: s.width, Height: s.height}
	}
	return Position{x: x, y: y, screen: s}, nil
}

// ErrNoPosition is an event whose Position was never built from a [Screen] —
// the zero value. Encoding it would send a tap at (0,0) on a 0x0 frame, which
// the server would reject as stale on a good day and misplace on a bad one.
var ErrNoPosition = errors.New("scrcpy: event carries no position; build one with Screen.At on a Screen from ScreenFromSession")

// appendPosition writes scrcpy's twelve-byte struct sc_position: x, y, then the
// frame size. It is one function because both messages that carry a position
// carry the identical layout, and two copies of it would be two places to flip
// an endianness.
func appendPosition(dst []byte, p Position) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(p.x))
	dst = binary.BigEndian.AppendUint32(dst, uint32(p.y))
	dst = binary.BigEndian.AppendUint16(dst, p.screen.width)
	return binary.BigEndian.AppendUint16(dst, p.screen.height)
}

// ---------------------------------------------------------------------------
// The three messages
// ---------------------------------------------------------------------------

// KeyEvent is INJECT_KEYCODE.
//
// It is the one message here that carries no position and therefore cannot
// fail to encode, which is why its encoders return no error. A key press is not
// aimed at a pixel.
type KeyEvent struct {
	// Action is AKEY_EVENT_ACTION_DOWN or _UP.
	Action KeyAction

	// Keycode is an Android AKEYCODE_* value. It is signed because the field on
	// the wire is, not because a negative one means anything.
	Keycode int32

	// Repeat is the auto-repeat count, 0 for the first press.
	Repeat int32

	// MetaState is the AMETA_* bitset in effect.
	MetaState int32
}

// Encode returns the [KeycodeMsgLen] bytes of this message.
func (e KeyEvent) Encode() []byte {
	return e.AppendTo(make([]byte, 0, KeycodeMsgLen))
}

// AppendTo appends this message to dst, for a caller writing many events
// without allocating per event.
func (e KeyEvent) AppendTo(dst []byte) []byte {
	dst = append(dst, msgInjectKeycode, byte(e.Action))
	dst = binary.BigEndian.AppendUint32(dst, uint32(e.Keycode))
	dst = binary.BigEndian.AppendUint32(dst, uint32(e.Repeat))
	return binary.BigEndian.AppendUint32(dst, uint32(e.MetaState))
}

// TouchEvent is INJECT_TOUCH_EVENT.
type TouchEvent struct {
	// Action is AMOTION_EVENT_ACTION_DOWN, _UP or _MOVE.
	Action TouchAction

	// PointerID identifies one finger across a gesture. Use [PointerMouse] or
	// [PointerGenericFinger] for a single-pointer client; a multi-touch client
	// picks its own small non-negative ids and must keep them stable from DOWN
	// to UP or the server sees two gestures.
	PointerID uint64

	// Position is where, in the video frame the human was looking at.
	Position Position

	// Pressure is how hard. An UP event carries [PressureNone]: a release with
	// pressure still on it is a contradiction the server resolves in ways that
	// differ by Android version.
	Pressure Pressure

	// ActionButton and Buttons are AMOTION_EVENT_BUTTON_* — which button caused
	// this event, and which are held. Zero for a touchscreen.
	ActionButton int32
	Buttons      int32
}

// Encode returns the [TouchMsgLen] bytes of this message, or an error if its
// position did not come from a [Screen].
func (e TouchEvent) Encode() ([]byte, error) {
	return e.AppendTo(make([]byte, 0, TouchMsgLen))
}

// AppendTo appends this message to dst. On error dst is returned unchanged, so
// a caller batching events into one buffer does not have to unwind a partial
// write — a half-written control message would desynchronise the channel for
// every message after it.
func (e TouchEvent) AppendTo(dst []byte) ([]byte, error) {
	if !e.Position.screen.Valid() {
		return dst, ErrNoPosition
	}
	out := append(dst, msgInjectTouch, byte(e.Action))
	out = binary.BigEndian.AppendUint64(out, e.PointerID)
	out = appendPosition(out, e.Position)
	out = binary.BigEndian.AppendUint16(out, uint16(e.Pressure))
	out = binary.BigEndian.AppendUint32(out, uint32(e.ActionButton))
	return binary.BigEndian.AppendUint32(out, uint32(e.Buttons)), nil
}

// ScrollEvent is INJECT_SCROLL_EVENT.
type ScrollEvent struct {
	// Position is where the pointer was when the wheel turned.
	Position Position

	// Horizontal and Vertical are fixed-point distances in -1..1. Build them
	// with [ScrollFromFloat] rather than by scaling by hand.
	Horizontal Scroll
	Vertical   Scroll

	// Buttons is the AMOTION_EVENT_BUTTON_* bitset held during the scroll.
	Buttons int32
}

// Encode returns the [ScrollMsgLen] bytes of this message, or an error if its
// position did not come from a [Screen].
func (e ScrollEvent) Encode() ([]byte, error) {
	return e.AppendTo(make([]byte, 0, ScrollMsgLen))
}

// AppendTo appends this message to dst, leaving dst unchanged on error for the
// reason given on [TouchEvent.AppendTo].
func (e ScrollEvent) AppendTo(dst []byte) ([]byte, error) {
	if !e.Position.screen.Valid() {
		return dst, ErrNoPosition
	}
	out := append(dst, msgInjectScroll)
	out = appendPosition(out, e.Position)
	out = binary.BigEndian.AppendUint16(out, uint16(e.Horizontal))
	out = binary.BigEndian.AppendUint16(out, uint16(e.Vertical))
	return binary.BigEndian.AppendUint32(out, uint32(e.Buttons)), nil
}
