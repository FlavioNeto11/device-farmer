package fakeadb

import (
	_ "embed"
	"errors"
)

// ---------------------------------------------------------------------
// Real video for the scripted device
//
// ScrcpyPacket.Data was arbitrary bytes when this fixture was written, and the
// reasoning was sound as far as it went: a test that asserts on the ASCII
// "SPS-PPS" is easier to read than one that asserts on a hex dump, and the
// framing is what the framing tests are about. What it could not reach is
// everything downstream of the framing. internal/scrcpy hands its payloads to
// a browser's VideoDecoder untranscoded, and a VideoDecoder does not care what
// a Go test asserted — it either decodes the bytes or reports an error that
// names no file in this repository. A fake that can only serve undecodable
// payloads leaves that whole half of the path with no coverage at all, so the
// first person to see a real frame is an operator looking at a black
// rectangle.
//
// So the fixture also ships a real clip, and this file is the part that turns a
// file into packets. The split is deliberate: scripts/make-screen-fixture.sh
// owns the encoder's flags, testdata/screen.h264 owns the bytes, and
// ScrcpyPacketsFromAnnexB owns the one question neither of those can answer —
// where does one access unit end and the next begin, given that the scrcpy
// protocol's unit of transfer is an access unit and Annex-B's unit is a NAL.
//
// NOTHING HERE DECODES ANYTHING, and that is the point of doing it this way.
// Framing an elementary stream needs a start-code scanner and the five-bit NAL
// type; it does not need a parser for the slice header, a picture-order-count
// reconstruction, or any of the rest of H.264. A fixture that carried half a
// decoder would be a fixture with its own bugs, and those bugs would be
// reported as transport bugs.
// ---------------------------------------------------------------------

// The NAL unit types this file distinguishes, from ITU-T H.264 table 7-1.
//
// Only the two VCL types matter structurally: an access unit is whatever
// non-VCL NALs accumulated since the last one, plus the coded slice that
// closes it. The rest are named because a reader checking this against the
// standard should not have to count, and because nalSPS is the one the error
// below is about.
const (
	nalSliceNonIDR = 1 // a coded slice of a non-IDR picture: a P frame here
	nalSliceIDR    = 5 // a coded slice of an IDR picture: a key frame
	nalSEI         = 6 // supplemental enhancement information
	nalSPS         = 7 // sequence parameter set
	nalPPS         = 8 // picture parameter set
)

// The four ways an Annex-B stream can fail to be one, each its own value so a
// caller can tell them apart with errors.Is rather than by matching text. They
// are distinct errors rather than one wrapped message because they call for
// four different fixes, and a fixture that says "bad stream" makes somebody
// discover which one by experiment.
var (
	// ErrAnnexBFrameRate is a frame rate that cannot produce timestamps.
	ErrAnnexBFrameRate = errors.New("fakeadb: ScrcpyPacketsFromAnnexB needs a positive frame rate — " +
		"every packet's presentation timestamp is derived from it, so zero divides by zero and a " +
		"negative one hands a decoder a stream whose time runs backwards")

	// ErrAnnexBNoStartCode is a stream with no NAL unit delimiters at all.
	ErrAnnexBNoStartCode = errors.New("fakeadb: this is not an Annex-B elementary stream — " +
		"it contains no 00 00 01 or 00 00 00 01 start code anywhere, so there are no NAL units to " +
		"group into access units; a container (MP4, MKV) stores length-prefixed NALs instead and " +
		"has to be remuxed first, which is what `ffmpeg -f h264` does")

	// ErrAnnexBNoParameterSets is a stream whose first frame a decoder could
	// not configure itself for.
	ErrAnnexBNoParameterSets = errors.New("fakeadb: the stream carries no SPS (NAL type 7) ahead of " +
		"its first slice — the synthetic config packet would be empty, and a decoder handed frames " +
		"with no sequence parameter set has not been told the resolution or the profile it is " +
		"decoding; encode with repeat-headers=1, as scripts/make-screen-fixture.sh does, rather " +
		"than relying on parameter sets a container would have carried out of band")

	// ErrAnnexBNoSlices is a stream with no picture in it.
	ErrAnnexBNoSlices = errors.New("fakeadb: the stream carries no coded slice (NAL type 1 or 5) at " +
		"all — it is parameter sets and nothing else, so the video socket built from it would " +
		"announce a geometry and then never send a frame, which is indistinguishable from a screen " +
		"that hung")
)

//go:embed testdata/screen.h264
var screenFixture string

// ScreenFixture returns the committed Annex-B fixture: four seconds of a
// moving test pattern, 288x640 at 12 fps, with an IDR every twelve frames.
//
// scripts/make-screen-fixture.sh regenerates it and documents why every
// encoder flag is the way it is. The short version is that it is Constrained
// Baseline with no B-frames, so decode order is presentation order, and every
// IDR repeats its own SPS and PPS, so the clip can be joined or looped at any
// keyframe.
//
// A FRESH COPY PER CALL, which is not free and is not negotiable. go:embed
// hands out a writable []byte if you ask it for one, and every caller would
// share the one array: a test that trimmed it, re-framed it in place or
// corrupted a byte to prove a decoder complains would have silently rewritten
// the fixture for every test that ran afterwards in the same binary, including
// the ones that had already passed. The embedded copy is a string precisely so
// that this conversion is the only way to get at it.
func ScreenFixture() []byte { return []byte(screenFixture) }

// ScrcpyPacketsFromAnnexB splits an Annex-B H.264 elementary stream into the
// packets scrcpy would have sent for it.
//
// The grouping rule is the protocol's, not H.264's. scrcpy's video socket
// carries one packet per ACCESS UNIT — one picture, with whatever parameter
// sets and SEI the encoder chose to put in front of it — because that is the
// granularity a decoder consumes and the granularity a timestamp belongs to.
// Annex-B's granularity is the NAL unit, of which an access unit holds one or
// several. So every non-VCL NAL accumulates as a prefix and the access unit
// closes on the coded slice that follows it.
//
// The returned packets are a REPARTITIONING OF THE INPUT AND NOT A COPY OF IT.
// Every Data is a sub-slice of stream, with the original start codes left in
// place, so concatenating the payload of every non-config packet reproduces
// stream byte for byte — which is the property that makes this function
// checkable against a hex dump instead of against itself. It is also why this
// allocates nothing that scales with the input: one slice header per access
// unit and no payload bytes at all. Callers must therefore treat stream as
// read-only for as long as they hold the packets, which is why [ScreenFixture]
// hands out a copy.
//
// Packet zero is synthetic: a config packet carrying the parameter sets that
// precede the first slice. A real scrcpy server sends exactly that ahead of
// the first frame, and the duplication — those same bytes also open the first
// access unit, because the fixture is encoded with repeat-headers — is what a
// real server produces too. A decoder that joins late needs them and a decoder
// that was there from the start ignores the repeat.
func ScrcpyPacketsFromAnnexB(stream []byte, fps int) ([]ScrcpyPacket, error) {
	if fps <= 0 {
		return nil, ErrAnnexBFrameRate
	}
	nals := annexBNALs(stream)
	if len(nals) == 0 {
		return nil, ErrAnnexBNoStartCode
	}

	// The first access unit begins at offset zero rather than at the first
	// start code, so that a stream with leading_zero_8bits in front of it — or
	// anything else a muxer put there — is still reproduced exactly by
	// concatenating the payloads. Dropping those bytes would be harmless to a
	// decoder and would quietly falsify the one invariant this function offers.
	au := 0
	start := 0
	lastAU := -1 // where the most recently emitted access unit begins
	sawSlice := false
	out := make([]ScrcpyPacket, 0, len(nals)+1)

	for _, n := range nals {
		if start < 0 {
			start = n.start
		}
		if n.typ != nalSliceNonIDR && n.typ != nalSliceIDR {
			continue
		}
		if !sawSlice {
			sawSlice = true
			// The config packet is everything ahead of the first slice. Its
			// emptiness is the SPS check: a stream whose first NAL is already a
			// slice has no parameter sets to send, and the SPS specifically is
			// the one whose absence means a decoder was never told the
			// resolution.
			if !annexBHasType(nals, n.start, nalSPS) {
				return nil, ErrAnnexBNoParameterSets
			}
			out = append(out, ScrcpyPacket{Config: true, Data: stream[start:n.start]})
		}
		out = append(out, ScrcpyPacket{
			// Integer microseconds from the frame index rather than an
			// accumulated delta, so rounding error does not compound: at 12 fps
			// the interval is 83333.33µs and forty-eight accumulated
			// truncations would drift the last frame sixteen milliseconds
			// early. Computed from the index, every timestamp is within one
			// microsecond of the truth forever.
			PTS:      uint64(au) * 1_000_000 / uint64(fps),
			KeyFrame: n.typ == nalSliceIDR,
			Data:     stream[start:n.end],
		})
		au++
		lastAU = start
		start = -1
	}

	if !sawSlice {
		return nil, ErrAnnexBNoSlices
	}

	// Non-VCL NALs trailing the last slice — an end-of-sequence or
	// end-of-stream marker, a trailing SEI — belong to no access unit, because
	// nothing closes them. They go onto the last packet instead of being
	// dropped, so that "the packets are the stream, repartitioned" stays true
	// of every input rather than only of the ones that happen to end on a
	// slice. A fixture that silently shed its last few bytes would make a
	// truncation look exactly like a clean split.
	if start >= 0 && start < len(stream) {
		out[len(out)-1].Data = stream[lastAU:]
	}
	return out, nil
}

// annexBNAL is one NAL unit located in a stream: where its start code begins,
// where the next one begins, and what the five-bit type in its header says it
// is. Offsets rather than bytes, because every payload this file emits is a
// sub-slice of the caller's stream.
type annexBNAL struct {
	start int // the first byte of this NAL's start code
	end   int // one past its last byte: the next start code, or end of stream
	typ   int // nal_unit_type, the low five bits of the header byte
}

// annexBNALs locates every NAL unit in an Annex-B stream.
//
// BOTH START CODE LENGTHS, and the two arms are not symmetric — which is worth
// stating precisely, because the obvious summary of this function is wrong in
// one direction.
//
// The fixture ships both lengths in one file: x264 writes a four-byte
// 00 00 00 01 ahead of a parameter set or an access unit's first NAL and a
// three-byte 00 00 01 ahead of the slices inside one, which is what the byte
// stream format permits and what every encoder does. A scanner with only the
// FOUR-byte arm finds fifty-two of this file's fifty-seven NAL units and misses
// exactly the five that carry three-byte codes — every IDR and the SEI. The
// result is a stream that frames without complaint and contains no key frame at
// all, so a decoder can never start and nothing says why. That is the arm whose
// absence is a disaster.
//
// A scanner with only the THREE-byte arm is not a disaster, because 00 00 01 is
// a suffix of 00 00 00 01: it finds every NAL, one byte later, and leaves each
// four-byte code's leading zero attributed to the NAL before it. The access
// units come out identical. The four-byte arm is here anyway so that a start
// code belongs WHOLE to the NAL it introduces — which is what the standard
// says it is, and what makes annexBNAL.start an offset somebody can check
// against a hex dump rather than one they have to reason about.
//
// Scanning for start codes at all is exact rather than approximate, which is
// worth saying because it looks like a heuristic. H.264's emulation prevention
// guarantees the three-byte sequence cannot occur inside a NAL's payload — an
// encoder that would have emitted 00 00 01 emits 00 00 03 01 instead — so every
// match is a real delimiter and no search for one can land inside a frame.
func annexBNALs(stream []byte) []annexBNAL {
	var out []annexBNAL
	for i := 0; i+3 <= len(stream); {
		if stream[i] != 0 || stream[i+1] != 0 {
			i++
			continue
		}
		hdr := 0
		switch {
		case stream[i+2] == 1:
			hdr = i + 3
		case stream[i+2] == 0 && i+4 <= len(stream) && stream[i+3] == 1:
			hdr = i + 4
		default:
			// A run of zeroes that is not a start code, or one cut off by the
			// end of the stream. Advancing by one rather than by three is what
			// makes 00 00 00 00 01 resolve to a four-byte start code with a
			// leading_zero_8bits in front of it, which is the reading the
			// standard gives it.
			i++
			continue
		}
		if hdr >= len(stream) {
			break // a start code with no header byte after it: not a NAL.
		}
		if n := len(out); n > 0 {
			out[n-1].end = i
		}
		out = append(out, annexBNAL{start: i, end: len(stream), typ: int(stream[hdr] & 0x1f)})
		i = hdr + 1
	}
	return out
}

// annexBHasType reports whether any NAL starting before limit has type typ. It
// is how the SPS check asks "was there a sequence parameter set before the
// first slice" without a second pass over the stream.
func annexBHasType(nals []annexBNAL, limit, typ int) bool {
	for _, n := range nals {
		if n.start >= limit {
			return false
		}
		if n.typ == typ {
			return true
		}
	}
	return false
}
