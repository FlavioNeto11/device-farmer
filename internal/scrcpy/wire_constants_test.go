package scrcpy

// Every constant that crosses the wire, pinned to a LITERAL.
//
// An adversarial review of this package found seven constants asserted only
// against themselves. TestUnknownCodecIsATypedErrorNamingIt built its input
// bytes from the constant it was checking; the encoder round-trips compared two
// encoders sharing one constant; the kind tables were written as
// []Kind{KindSession, …}. Every one of those passes with the constant set to
// anything at all, which is the failure control.go's own header names: a test
// that derives its expectation from the code under test asserts that the code
// agrees with itself.
//
// The values below are transcribed from the protocol, not from the package.
// They are what a phone on the other end of a socket believes, and this file is
// the only thing in the tree that says so. Changing a value here without
// changing it upstream is how a drag stops lifting the finger, or how a real
// h265 stream is refused as unknown.
//
// Read this file as a table of the protocol. If a constant is not here, it does
// not cross the wire — and if it does cross the wire and is not here, that is
// the bug this file exists to make visible.

import (
	"math"
	"testing"
)

func TestWireConstantsAreTheProtocolsValues(t *testing.T) {
	t.Parallel()

	// Android's AKEY_EVENT_ACTION_*.
	for _, c := range []struct {
		name string
		got  uint8
		want uint8
	}{
		{"KeyDown", uint8(KeyDown), 0},
		{"KeyUp", uint8(KeyUp), 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// Android's AMOTION_EVENT_ACTION_*. A different enumeration from the above
	// in the same byte position, which is why both are pinned rather than one.
	for _, c := range []struct {
		name string
		got  uint8
		want uint8
	}{
		{"TouchDown", uint8(TouchDown), 0},
		{"TouchUp", uint8(TouchUp), 1},
		{"TouchMove", uint8(TouchMove), 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d — a wrong TouchUp is a finger that never lifts, and a "+
				"wrong TouchMove is every drag dropped", c.name, c.got, c.want)
		}
	}

	// The two pointer identities scrcpy reserves: -1 and -2 as u64.
	if PointerMouse != 0xffffffffffffffff {
		t.Errorf("PointerMouse = %#x, want %#x (-1 in two's complement)", PointerMouse, uint64(0xffffffffffffffff))
	}
	if PointerGenericFinger != 0xfffffffffffffffe {
		t.Errorf("PointerGenericFinger = %#x, want %#x (-2)", PointerGenericFinger, uint64(0xfffffffffffffffe))
	}

	// The codec ids are the four-character codes, big-endian, as the server
	// writes them. Spelled here as the ASCII they are, so a transposition is
	// visible rather than hidden in hex.
	for _, c := range []struct {
		name string
		got  CodecID
		want CodecID
	}{
		{"CodecH264", CodecH264, CodecID('h')<<24 | CodecID('2')<<16 | CodecID('6')<<8 | CodecID('4')},
		{"CodecH265", CodecH265, CodecID('h')<<24 | CodecID('2')<<16 | CodecID('6')<<8 | CodecID('5')},
		{"CodecAV1", CodecAV1, CodecID('a')<<16 | CodecID('v')<<8 | CodecID('1')},
		{"CodecVP8", CodecVP8, CodecID('v')<<16 | CodecID('p')<<8 | CodecID('8')},
		{"CodecVP9", CodecVP9, CodecID('v')<<16 | CodecID('p')<<8 | CodecID('9')},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, uint32(c.got), uint32(c.want))
		}
	}
}

// TestMaxPacketIsFourMegabytes pins the cap to a number rather than to a range.
//
// Every other assertion about it refers to MaxPacket symbolically, and the
// largest literal anywhere in the suite is a gigabyte — so together they pinned
// the cap only to the open interval (0, 1 GiB). Raising it to 1 GiB minus one
// byte left the whole package green, which makes the constant's own four
// paragraphs about "an order of magnitude above the largest frame a handset's
// hardware encoder produces" an argument nothing defends.
//
// A gigabyte-per-packet cap in the process that answers
// POST /api/v1/leases/{id}/renew is the same out-of-memory with one fewer zero.
func TestMaxPacketIsFourMegabytes(t *testing.T) {
	t.Parallel()

	const want = 4 << 20
	if MaxPacket != want {
		t.Errorf("MaxPacket = %d, want %d (4 MiB).\n\n"+
			"If this is a deliberate change, say in the constant's doc comment what frame it "+
			"is now sized for and why that size is safe in the API server. The number is not "+
			"arbitrary: it is what stands between a u32 a wedged phone chose and a make() in "+
			"the process that renews every lease in the farm.", MaxPacket, want)
	}
}

// TestEveryNegativePressureIsNoPressure defends the guard the pressure table
// could not see.
//
// That table carried exactly one negative, -1, and -1 is among the handful of
// inputs for which deleting `|| f <= 0` is invisible: -1 × 2^16 is -65536, and
// converting an out-of-range float to uint16 truncates it to 0, which is the
// same answer the guard gives. The branch's stated reason — "a NaN or a
// negative from a pointer event that reported something strange must become no
// pressure, not a wild u16 that reads as a hard press" — was therefore
// undefended by the one case that looked like it was defending it.
//
// The values below are chosen so the unguarded conversion produces something
// OTHER than zero. -0.5 × 2^16 is -32768, which converts to 32768: a pointer
// event reporting a negative pressure would land as half a press on a phone.
func TestEveryNegativePressureIsNoPressure(t *testing.T) {
	t.Parallel()

	for _, f := range []float64{
		-0.5,   // unguarded: 32768, a half press
		-0.25,  // unguarded: 49152
		-0.999, // unguarded: something near full
		-1,     // the case the old table had, and the one that hides the bug
		-2,
		-1e9,
		math.Inf(-1),
		math.NaN(),
		0,
		math.Copysign(0, -1), // negative zero
	} {
		if got := PressureFromFloat(f); got != PressureNone {
			t.Errorf("PressureFromFloat(%v) = %d, want %d (no pressure). An input that is not a "+
				"press must not become one: this value is written into a touch event and sent "+
				"to a phone somebody may be holding.", f, got, PressureNone)
		}
	}
}
