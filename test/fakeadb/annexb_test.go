package fakeadb

import (
	"bytes"
	"context"
	"errors"

	"io"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
)

// The committed fixture's shape, as numbers rather than as whatever the
// splitter happens to produce.
//
// These are the values scripts/make-screen-fixture.sh prints when it runs, and
// they are written out here so that a regeneration which quietly changed the
// clip fails in this package with the two numbers side by side. A test that
// asked the splitter how many access units the splitter had found would pass
// against any fixture at all, including an empty one.
const (
	screenFixtureBytes       = 88789 // what the committed file weighs
	screenFixtureFPS         = 12
	screenFixtureAccessUnits = 48 // four seconds at twelve frames a second
	screenFixtureKeyFrames   = 4  // an IDR every twelfth frame
	screenFixtureWidth       = 288
	screenFixtureHeight      = 640
)

// TestTheCommittedScreenFixtureIsTheClipTheScriptMakes guards the file itself
// before anything tries to read it, because every number below this point is
// stated about THIS clip and a silently regenerated one would make the rest of
// the file's failures unreadable.
//
// Falsify: regenerate the fixture with a different -t, -g or size.
func TestTheCommittedScreenFixtureIsTheClipTheScriptMakes(t *testing.T) {
	t.Parallel()

	got := ScreenFixture()
	if len(got) != screenFixtureBytes {
		t.Fatalf("testdata/screen.h264 is %d bytes, want %d — the committed clip is not the one "+
			"scripts/make-screen-fixture.sh produces, so every count this file asserts is about a "+
			"different video", len(got), screenFixtureBytes)
	}
	if !bytes.HasPrefix(got, []byte{0x00, 0x00, 0x00, 0x01, 0x67}) {
		t.Fatalf("the clip opens with % x, want a four-byte start code and an SPS (0x67) — a "+
			"decoder joining this stream at byte zero has nothing to configure itself from",
			got[:min(8, len(got))])
	}
}

// TestScreenFixtureHandsOutACopyNobodyElseShares is the one property of the
// accessor that is not about video at all, and the one whose absence would be
// found by a test that had nothing to do with it.
//
// go:embed will hand out a writable []byte if a package asks for one, and every
// caller would then share a single array. A test that re-framed the clip in
// place, or corrupted a byte to prove a decoder complains, would have rewritten
// the fixture for every test that ran after it in the same binary — including
// the ones that had already passed, which is the version of this bug that takes
// a day to find.
//
// Falsify: embed the fixture as []byte and return it directly.
func TestScreenFixtureHandsOutACopyNobodyElseShares(t *testing.T) {
	t.Parallel()

	first := ScreenFixture()
	first[0] = 0xff
	if second := ScreenFixture(); second[0] != 0x00 {
		t.Fatalf("scribbling on one caller's fixture changed the next caller's to % x — the "+
			"embedded clip is shared, so any test that writes to it corrupts every later test in "+
			"this binary", second[:4])
	}
}

// TestTheScreenFixtureSplitsIntoItsAccessUnits is the splitter against the very
// file it ships with: the counts, the flags, and the synthetic config packet.
//
// Falsify: drop the three-byte arm of annexBNALs. The fixture writes four-byte
// codes ahead of parameter sets and three-byte codes ahead of slices, so a
// scanner without that arm misses every IDR and the SEI: the count below drops to
// forty-five and, worse, the key frame count drops to zero, which is a stream a
// decoder can never start on.
func TestTheScreenFixtureSplitsIntoItsAccessUnits(t *testing.T) {
	t.Parallel()

	stream := ScreenFixture()
	packets, err := ScrcpyPacketsFromAnnexB(stream, screenFixtureFPS)
	if err != nil {
		t.Fatalf("splitting the committed fixture: %v", err)
	}

	if want := screenFixtureAccessUnits + 1; len(packets) != want {
		t.Fatalf("the fixture framed as %d packets, want %d — one synthetic config packet and "+
			"%d access units", len(packets), want, screenFixtureAccessUnits)
	}

	if !packets[0].Config {
		t.Fatalf("packet zero is not the config packet; a decoder handed this stream gets frames " +
			"before it has been told what it is decoding")
	}
	if packets[0].PTS != 0 {
		t.Fatalf("the config packet's timestamp is %d, want 0", packets[0].PTS)
	}
	for _, want := range []int{nalSPS, nalPPS} {
		if !bytes.Contains(packets[0].Data, startCode(4, want)) && !bytes.Contains(packets[0].Data, startCode(3, want)) {
			t.Fatalf("the config packet carries no NAL of type %d (% x) — SPS and PPS are the two "+
				"things it exists to carry, and a decoder without both cannot start",
				want, packets[0].Data[:min(16, len(packets[0].Data))])
		}
	}

	keys := 0
	for i, p := range packets[1:] {
		if p.Config {
			t.Fatalf("packet %d claims to be codec configuration; only the synthetic first one is", i+1)
		}
		if p.KeyFrame {
			keys++
		}
		if want := uint64(i) * 1_000_000 / screenFixtureFPS; p.PTS != want {
			t.Fatalf("access unit %d is timestamped %dµs, want %dµs — at %d fps the nth frame is "+
				"due n/%d of a second in", i, p.PTS, want, screenFixtureFPS, screenFixtureFPS)
		}
	}
	if keys != screenFixtureKeyFrames {
		t.Fatalf("%d of the %d access units are key frames, want %d — the clip is encoded with an "+
			"IDR every twelfth frame so that a loop point and a late join are both decodable, and "+
			"a different count means the encoder's keyint did not take",
			keys, len(packets)-1, screenFixtureKeyFrames)
	}
	if !packets[1].KeyFrame {
		t.Fatalf("the first access unit is not a key frame; a client that joined at the start of " +
			"the stream could not decode it")
	}
}

// TestEveryAccessUnitTogetherIsTheStreamItself is the invariant that makes the
// splitter checkable against a hex dump rather than against itself: the packets
// are a repartitioning of the input and nothing else. If this holds, no byte was
// dropped, none was duplicated, no start code was rewritten and no payload was
// copied into a different order.
//
// The config packet is excluded because it alone is synthetic — those bytes also
// open the first access unit, which is what a server encoding with
// repeat-headers produces and what a decoder joining late needs.
//
// Falsify: emit the slice without its accumulated prefix NALs, or strip the
// start codes from an emitted payload.
func TestEveryAccessUnitTogetherIsTheStreamItself(t *testing.T) {
	t.Parallel()

	stream := ScreenFixture()
	packets, err := ScrcpyPacketsFromAnnexB(stream, screenFixtureFPS)
	if err != nil {
		t.Fatalf("splitting the committed fixture: %v", err)
	}

	var joined []byte
	for _, p := range packets {
		if p.Config {
			continue
		}
		joined = append(joined, p.Data...)
	}
	if !bytes.Equal(joined, stream) {
		t.Fatalf("the access units concatenated are %d bytes and do not match the stream's %d — "+
			"the packets are supposed to BE the stream, repartitioned, so a difference here means "+
			"a frame somewhere is missing bytes a decoder needs, or is carrying bytes that belong "+
			"to its neighbour", len(joined), len(stream))
	}
}

// TestBothStartCodeLengthsCloseAnAccessUnit states the scanner's rule on a
// stream small enough to read, with the two lengths deliberately mixed the way
// x264 mixes them and with junk in front of the first start code.
//
// Falsify: drop the three-byte arm of annexBNALs, or start the first access unit
// at the first start code instead of at offset zero. (Dropping the FOUR-byte arm
// does not fail this one, and the note on annexBNALs says why: 00 00 01 is a
// suffix of 00 00 00 01, so the three-byte arm alone still finds every NAL. It
// fails TestTheScreenFixtureSplitsIntoItsAccessUnits, where the asymmetry
// actually bites.)
func TestBothStartCodeLengthsCloseAnAccessUnit(t *testing.T) {
	t.Parallel()

	// Two leading zero bytes, then: SPS behind a four-byte code, PPS behind a
	// three-byte code, an IDR slice, then a non-IDR slice. Two access units.
	stream := []byte{0x00, 0x00}
	stream = append(stream, nal(4, nalSPS, "sps")...)
	stream = append(stream, nal(3, nalPPS, "pps")...)
	stream = append(stream, nal(3, nalSliceIDR, "idr")...)
	stream = append(stream, nal(4, nalSliceNonIDR, "p-frame")...)

	packets, err := ScrcpyPacketsFromAnnexB(stream, 10)
	if err != nil {
		t.Fatalf("splitting a hand-built stream: %v", err)
	}
	if len(packets) != 3 {
		t.Fatalf("a stream of SPS, PPS, IDR, P framed as %d packets, want a config packet and two "+
			"access units; the start code lengths are mixed on purpose and a scanner that knows "+
			"one of them sees a different stream", len(packets))
	}
	if !packets[1].KeyFrame || packets[2].KeyFrame {
		t.Fatalf("key frame flags came back as %t,%t, want true,false — the IDR is the one a "+
			"decoder may start from and the P frame is not",
			packets[1].KeyFrame, packets[2].KeyFrame)
	}
	// The leading zeroes belong to the first access unit, because the promise is
	// that concatenating the payloads reproduces the input and not merely the
	// part of it that looked like NAL units.
	if got := append(append([]byte(nil), packets[1].Data...), packets[2].Data...); !bytes.Equal(got, stream) {
		t.Fatalf("the access units concatenated are % x, want the whole input % x", got, stream)
	}
	if got, want := packets[2].PTS, uint64(100_000); got != want {
		t.Fatalf("the second access unit is timestamped %dµs, want %dµs at 10 fps", got, want)
	}
}

// TestBytesTrailingTheLastSliceAreNotDropped covers the one input shape where
// "the packets are the stream, repartitioned" is not automatic.
//
// A non-VCL NAL after the last slice — an end-of-sequence marker, an end-of-
// stream marker, a trailing SEI — belongs to no access unit, because an access
// unit is closed by the slice that follows its prefix and nothing follows this
// one. The obvious implementation drops it. What makes that worse than untidy is
// what it does to the invariant: a splitter that silently sheds its last few
// bytes makes a TRUNCATED stream and a cleanly split one produce identical
// output, so the test that would have caught a truncation cannot.
//
// Falsify: delete the trailing-tail extension at the end of
// ScrcpyPacketsFromAnnexB.
func TestBytesTrailingTheLastSliceAreNotDropped(t *testing.T) {
	t.Parallel()

	const nalEndOfSequence = 10

	stream := nal(4, nalSPS, "sps")
	stream = append(stream, nal(4, nalPPS, "pps")...)
	stream = append(stream, nal(3, nalSliceIDR, "idr")...)
	stream = append(stream, nal(3, nalSliceNonIDR, "p-frame")...)
	stream = append(stream, nal(4, nalEndOfSequence, "")...)

	packets, err := ScrcpyPacketsFromAnnexB(stream, 12)
	if err != nil {
		t.Fatalf("splitting a stream that ends on an end-of-sequence NAL: %v", err)
	}
	if len(packets) != 3 {
		t.Fatalf("framed as %d packets, want a config packet and two access units — the trailing "+
			"marker is not an access unit of its own", len(packets))
	}

	var joined []byte
	for _, p := range packets[1:] {
		joined = append(joined, p.Data...)
	}
	if !bytes.Equal(joined, stream) {
		t.Fatalf("the access units concatenated are %d bytes against the stream's %d: the %d bytes "+
			"trailing the last slice were dropped. A splitter that sheds its tail makes a truncated "+
			"stream indistinguishable from a whole one",
			len(joined), len(stream), len(stream)-len(joined))
	}
}

// TestAStreamThatIsNotUsableVideoIsNamedRatherThanGuessedAt walks the four
// refusals. Each of them calls for a different fix, which is why they are four
// values and not one message: a fixture that answered "bad stream" would make
// somebody find out which by experiment.
//
// Falsify: replace any of the four returns with a nil error and a best effort.
func TestAStreamThatIsNotUsableVideoIsNamedRatherThanGuessedAt(t *testing.T) {
	t.Parallel()

	sps := nal(4, nalSPS, "sps")
	pps := nal(4, nalPPS, "pps")

	cases := []struct {
		name   string
		stream []byte
		fps    int
		want   error
	}{
		{
			name:   "a frame rate that cannot make a timestamp",
			stream: ScreenFixture(),
			fps:    0,
			want:   ErrAnnexBFrameRate,
		},
		{
			name:   "a container, or anything else with no start codes",
			stream: []byte("ftypisom, not an elementary stream"),
			fps:    12,
			want:   ErrAnnexBNoStartCode,
		},
		{
			name:   "slices with no parameter sets in front of them",
			stream: nal(4, nalSliceIDR, "idr with nothing to decode it by"),
			fps:    12,
			want:   ErrAnnexBNoParameterSets,
		},
		{
			name:   "parameter sets and no picture at all",
			stream: append(append([]byte(nil), sps...), pps...),
			fps:    12,
			want:   ErrAnnexBNoSlices,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ScrcpyPacketsFromAnnexB(tc.stream, tc.fps)
			if !errors.Is(err, tc.want) {
				t.Fatalf("splitting %s returned (%d packets, %v), want %v — a caller cannot tell "+
					"which of the four things went wrong, so it cannot tell whether to fix its "+
					"encoder flags, its remux or its call", tc.name, len(got), err, tc.want)
			}
		})
	}
}

// TestTheFixturesVideoIsReadableByInternalScrcpy is the assertion that makes the
// two halves of this protocol meet, and it is the only one in the package that
// does.
//
// scrcpy.go's own header says it: internal/scrcpy parses this framing, the
// fixture produces it, they share no code on purpose, and NOTHING MAKES THEM
// AGREE. Every other test in this file reads the fixture's bytes back with
// offsets written out by hand, which proves the fixture is self-consistent and
// says nothing whatever about whether the parser that ships in the binary can
// read it. This one builds the fixture, reaches it the way the control plane
// does — one ADB transport per service, through internal/adbwire — and hands the
// socket to the reader internal/api will use.
//
// It asserts on the PAYLOADS because the payloads are what leaves this
// repository. Everything the browser's VideoDecoder receives is a Packet.Payload
// from this reader, so a splitter and a parser that agree on every access unit's
// bytes agree about the only thing a decoder is given.
//
// Falsify: unset VideoSessionHeader on the fixture below, which is the default
// and is the framing internal/scrcpy cannot read — it takes the width for the
// top half of a header, finds a flag where a length belongs, and fails with
// PacketTooLargeError before the first frame.
func TestTheFixturesVideoIsReadableByInternalScrcpy(t *testing.T) {
	t.Parallel()

	const devpath = "usb:8-1.1"
	packets, err := ScrcpyPacketsFromAnnexB(ScreenFixture(), screenFixtureFPS)
	if err != nil {
		t.Fatalf("splitting the committed fixture: %v", err)
	}

	srv := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:            devpath,
		Width:              screenFixtureWidth,
		Height:             screenFixtureHeight,
		Packets:            packets,
		VideoSessionHeader: true,
		VideoEOF:           true,
	}))
	scid, ok := srv.ScrcpySCID(devpath)
	if !ok {
		t.Fatalf("the fixture published no session id for %s", devpath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := adbwire.New(srv.Addr())
	video, err := cli.OpenService(ctx, devpath, "localabstract:scrcpy_"+scid)
	if err != nil {
		t.Fatalf("opening the video socket: %v", err)
	}
	defer video.Close()

	r, err := scrcpy.NewReader(video)
	if err != nil {
		t.Fatalf("internal/scrcpy refused the stream at its codec id: %v", err)
	}
	if got := r.Codec(); got != scrcpy.CodecH264 {
		t.Fatalf("the stream announced codec %v, want h264", got)
	}

	// The session header comes first and carries the geometry. A reader that
	// took it for a packet would be in the state this whole test exists to
	// detect, so it is asserted rather than skipped.
	first, err := r.Next()
	if err != nil {
		t.Fatalf("reading the session header: %v", err)
	}
	if first.Kind != scrcpy.KindSession {
		t.Fatalf("the first unit came back as kind %d, want a session header — the fixture's "+
			"geometry is not where internal/scrcpy looks for it", first.Kind)
	}
	if first.Session.Width != screenFixtureWidth || first.Session.Height != screenFixtureHeight {
		t.Fatalf("the session header announced %dx%d, want %dx%d",
			first.Session.Width, first.Session.Height, screenFixtureWidth, screenFixtureHeight)
	}

	for i, want := range packets {
		u, err := r.Next()
		if err != nil {
			t.Fatalf("reading packet %d of %d back through internal/scrcpy: %v", i, len(packets), err)
		}
		if u.Kind != scrcpy.KindPacket {
			t.Fatalf("packet %d came back as kind %d, want a packet", i, u.Kind)
		}
		if u.Packet.Config != want.Config || u.Packet.KeyFrame != want.KeyFrame {
			t.Fatalf("packet %d came back config=%t keyframe=%t, want config=%t keyframe=%t — the "+
				"two sides disagree about where the flag bits are, which a decoder sees as frames "+
				"it may not start from and configuration it never receives",
				i, u.Packet.Config, u.Packet.KeyFrame, want.Config, want.KeyFrame)
		}
		if u.Packet.PTS != want.PTS {
			t.Fatalf("packet %d came back timestamped %dµs, want %dµs", i, u.Packet.PTS, want.PTS)
		}
		if !bytes.Equal(u.Packet.Payload, want.Data) {
			t.Fatalf("packet %d's payload did not survive: %d bytes back against %d written. "+
				"Every byte a browser's VideoDecoder ever sees is one of these, so a difference "+
				"here is a frame it cannot decode", i, len(u.Packet.Payload), len(want.Data))
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last access unit the stream returned %v, want io.EOF — the fixture was "+
			"asked to end and something is still on the wire", err)
	}
}

// ---------------------------------------------------------------------
// Hand-built NAL units
// ---------------------------------------------------------------------

// nal builds one NAL unit: a start code of scLen bytes, a header byte carrying
// typ in its low five bits, and payload. The reference idc bits above the type
// are set for everything except SEI, which is what an encoder does and which
// also keeps the header byte from being a value a scanner could mistake for a
// start code byte.
func nal(scLen, typ int, payload string) []byte {
	out := startCode(scLen, typ)
	return append(out, payload...)
}

// startCode is a start code of scLen bytes followed by the header byte for typ —
// the prefix that identifies a NAL of that type in a stream, which is what the
// config packet assertions search for.
func startCode(scLen, typ int) []byte {
	out := make([]byte, 0, scLen+1)
	for i := 0; i < scLen-1; i++ {
		out = append(out, 0x00)
	}
	out = append(out, 0x01)
	hdr := byte(typ)
	if typ != nalSEI {
		hdr |= 0x60 // nal_ref_idc, nonzero for anything a decoder must keep
	}
	return append(out, hdr)
}
