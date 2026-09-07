package screen

import (
	"fmt"

	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
)

// Event is one input message.
//
// The set is CLOSED: appendTo is unexported, so the only implementations are the
// three in this file, which are the three messages internal/scrcpy encodes. That
// is deliberate. The control socket also carries clipboard text, UHID device
// definitions and a "set display power" message, and each of those is a
// capability this feature has not argued for. A caller outside this package
// cannot add one by defining a type; it has to come back here and say why.
type Event interface {
	// appendTo encodes the event onto dst, using sc to place coordinates.
	//
	// It takes the frame rather than reading it from a field so that no event
	// can be constructed already holding a position — a Position is only
	// obtainable from a live session's dimensions, which is what keeps a
	// coordinate from a stale or differently-scaled stream out of the wire.
	appendTo(dst []byte, sc scrcpy.Screen) ([]byte, error)
}

// Touch is a finger going down, moving or coming up.
//
// X and Y are in VIDEO space: the width and height the session reports, after
// max_size scaling and after the rotation the device was in when the stream
// started. They are not device pixels, and the difference is not small — a
// 1080x2400 panel streamed at max_size 1024 is 460x1024, so a caller sending
// device pixels misses by more than half the screen.
//
// The frame's dimensions travel with the stream for exactly this reason. A
// caller that cached them from an earlier session and reused them is sending
// coordinates for a screen that may since have rotated, and the refusal it gets
// from an out-of-frame value is the mechanism that catches it.
type Touch struct {
	Action    scrcpy.TouchAction
	X, Y      int32
	Pressure  float64
	PointerID uint64
}

func (t Touch) appendTo(dst []byte, sc scrcpy.Screen) ([]byte, error) {
	pos, err := sc.At(t.X, t.Y)
	if err != nil {
		return dst, fmt.Errorf("touch at (%d,%d): %w", t.X, t.Y, err)
	}
	ev := scrcpy.TouchEvent{
		Action:    t.Action,
		PointerID: t.PointerID,
		Position:  pos,
		Pressure:  scrcpy.PressureFromFloat(t.Pressure),
	}
	return ev.AppendTo(dst)
}

// Key is a key going down or coming up.
//
// Keycode is an Android KeyEvent constant — 4 is BACK, 3 is HOME, 187 is
// APP_SWITCH. This package does not enumerate them and deliberately does not
// validate them: the set is Android's, it grows with each release, and a
// whitelist maintained here would refuse a key that the handset in the rack
// understands perfectly well. What bounds this is that reaching it at all
// requires the operator role and a live session.
type Key struct {
	Action    scrcpy.KeyAction
	Keycode   int32
	MetaState int32
	Repeat    int32
}

func (k Key) appendTo(dst []byte, _ scrcpy.Screen) ([]byte, error) {
	ev := scrcpy.KeyEvent{
		Action:    k.Action,
		Keycode:   k.Keycode,
		Repeat:    k.Repeat,
		MetaState: k.MetaState,
	}
	// No error: a key event carries no position, so there is nothing for a
	// frame to refuse. internal/scrcpy's Encode has no error return here for
	// the same reason, and that asymmetry between Key and Touch is the type
	// system saying which one can be out of bounds.
	return ev.AppendTo(dst), nil
}

// Scroll is a wheel, at a position.
//
// Horizontal and Vertical are in wheel units where 1.0 is one notch; negative
// Vertical scrolls content down, which is what a browser's wheel event reports
// as a positive deltaY. A caller that passes a browser delta through unchanged
// scrolls the wrong way, so the sign is converted at the edge that knows about
// browsers and not here.
type Scroll struct {
	X, Y                 int32
	Horizontal, Vertical float64
}

func (s Scroll) appendTo(dst []byte, sc scrcpy.Screen) ([]byte, error) {
	pos, err := sc.At(s.X, s.Y)
	if err != nil {
		return dst, fmt.Errorf("scroll at (%d,%d): %w", s.X, s.Y, err)
	}
	ev := scrcpy.ScrollEvent{
		Position:   pos,
		Horizontal: scrcpy.ScrollFromFloat(s.Horizontal),
		Vertical:   scrcpy.ScrollFromFloat(s.Vertical),
	}
	return ev.AppendTo(dst)
}
