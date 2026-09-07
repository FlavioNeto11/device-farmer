package scrcpy

// The video stream: a codec id, then a sequence of twelve-byte headers, each of
// which is either a session header or a packet header.
//
// This is the file the length rule is about. Everything else in this package
// turns Go values into bytes; this one turns a phone's bytes into a Go value,
// and one of those bytes groups is a u32 that says how much memory to allocate
// next. docs/design/interactive-control.md calls the transport's bandwidth
// behaviour "a first-class safety property here rather than a performance
// detail"; the allocation behaviour is the same property one layer down. A
// wedged encoder that emits 0xffffffff as a length is not a hypothetical — it
// is the same class of event as the saturated tunnel in DeviceFarmer/STF #663,
// and the process it would land in is the one that answers renewals.
//
// # The layout, verified against the scrcpy source
//
// The design document describes "a 4-byte codec id, a 12-byte session header,
// then per packet a 12-byte header (flags + 61-bit PTS) and a u32 length". That
// is right about the sizes and understates the structure in one way worth
// recording, because it changes what a correct reader looks like.
//
// The session header is not a one-time preamble. app/src/demuxer.c reads a
// twelve-byte header in a loop and dispatches on its top bit
// (sc_demuxer_is_session tests header[0] & 0x80): set means a session header,
// clear means a packet header. A session header arrives again whenever the
// video size changes, which on a handset means every rotation. A reader that
// consumed one session header and then assumed packets forever would parse a
// rotation's width as a PTS and its height as a payload length — and the
// height of a phone is a plausible-looking number of bytes, so it would not
// even fail loudly. It would silently emit garbage frames.
//
// So [Reader.Next] returns a discriminated union and the caller handles both.
// That is also what makes the coordinate mapping honest: the frame size a touch
// is expressed in comes from the most recent session header, so a tap after a
// rotation is measured against the frame the human is actually looking at.
//
// Header bits, from app/src/demuxer.c:
//
//	bit 63  session header when set        (header[0] & 0x80)
//	bit 62  SC_PACKET_FLAG_CONFIG
//	bit 61  SC_PACKET_FLAG_KEY_FRAME
//	bits 60..0  PTS in microseconds        (SC_PACKET_PTS_MASK)
//
// and a session header carries client_resized in header[3] & 1, width at
// header[4:8] and height at header[8:12], all big-endian.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxPacket caps a single video payload.
//
// FOUR MEBIBYTES IS NOT A GUESS ABOUT MEMORY. It is a statement about what a
// video access unit can be: one encoded frame, plus the codec configuration on
// the first packet. A 1080p H.264 keyframe at a generous 20 Mb/s is under a
// hundred kilobytes; a 4K keyframe at 50 Mb/s is a few hundred. Four mebibytes
// is more than an order of magnitude above the largest frame a handset's
// hardware encoder produces, which means a stream that reaches it is not a
// large frame. It is a desynchronised parse or a wedged encoder, and in both
// cases the only correct thing to do with the connection is drop it.
//
// It is deliberately the same order as internal/artifacts' maxManifestBytes,
// and for the same reason stated there: it "is what stops a zip bomb from
// turning an upload into an out-of-memory kill of the control plane". Here the
// bomb needs no craft at all — a phone with a confused encoder writes it by
// accident.
const MaxPacket = 4 << 20

// headerLen is the twelve bytes every header in this stream occupies, session
// and packet alike. It is one constant rather than two because the reader
// cannot know which kind it has until it has read all twelve.
const headerLen = 12

// The top three bits of the twelve-byte header.
const (
	flagSession  uint64 = 1 << 63
	flagConfig   uint64 = 1 << 62
	flagKeyFrame uint64 = 1 << 61

	// ptsMask is the low sixty-one bits. Sixty-one bits of microseconds is
	// seventy-three thousand years, so this never wraps in a way anyone has to
	// think about; it is masked rather than trusted because the flag bits above
	// it would otherwise read as an absurd timestamp.
	ptsMask uint64 = flagKeyFrame - 1
)

// CodecID is the four-byte identifier that opens a video stream. The values
// are ASCII packed big-endian, which is why they read as words in a hex dump —
// that is the format's own idea, not a decoration added here.
type CodecID uint32

// The video codecs a scrcpy server can be asked for. The audio codecs (opus,
// aac, flac, raw) are deliberately absent: this package decodes no audio, and a
// stream that announces one is a wiring mistake that should be named rather
// than half-handled.
const (
	CodecH264 CodecID = 0x68323634 // "h264"
	CodecH265 CodecID = 0x68323635 // "h265"
	CodecAV1  CodecID = 0x00617631 // "av1"
	CodecVP8  CodecID = 0x00767038 // "vp8"
	CodecVP9  CodecID = 0x00767039 // "vp9"
)

// String renders the codec the way the wire spells it, with the leading NUL
// of the three-letter names trimmed. An unknown id renders as hex, because the
// point of printing it at all is to let somebody grep the scrcpy source for it.
func (c CodecID) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	case CodecAV1:
		return "av1"
	case CodecVP8:
		return "vp8"
	case CodecVP9:
		return "vp9"
	default:
		return fmt.Sprintf("0x%08x", uint32(c))
	}
}

// Known reports whether this package can name the codec. It does not report
// whether anything downstream can decode it: that is the browser's question,
// and the answer differs per browser.
func (c CodecID) Known() bool {
	switch c {
	case CodecH264, CodecH265, CodecAV1, CodecVP8, CodecVP9:
		return true
	default:
		return false
	}
}

// UnknownCodecError is what a stream whose first four bytes name no video codec
// gets.
//
// It names the id in both renderings because the two failures it distinguishes
// look different in each: an audio codec id is a recognisable word in ASCII,
// while a stream that began mid-frame is noise in hex.
type UnknownCodecError struct {
	ID CodecID
}

func (e *UnknownCodecError) Error() string {
	return fmt.Sprintf("scrcpy: stream announced codec %s (0x%08x), which is not a video codec this "+
		"package decodes; check the server's video_codec argument and that this socket is the "+
		"video connection rather than the audio one", e.ID, uint32(e.ID))
}

// PacketTooLargeError is what a packet header above [MaxPacket] gets.
//
// It carries the length so that an operator reading a log line can tell the two
// stories apart: a number a little above the cap is an encoder configured for
// something absurd, and a number near 2^32 is a desynchronised parse. Neither
// is recoverable on the same connection — this reader cannot skip a count it
// does not believe — so a caller that sees this must close the stream rather
// than call Next again.
type PacketTooLargeError struct {
	// Length is the u32 the header declared.
	Length uint32

	// Max is the cap that refused it, so the message stands alone in a log.
	Max int
}

func (e *PacketTooLargeError) Error() string {
	return fmt.Sprintf("scrcpy: packet header declared %d bytes, above the %d-byte cap; nothing was "+
		"allocated and this stream is not recoverable — close it", e.Length, e.Max)
}

// ErrEmptyPacket is a packet header declaring a zero-length payload. scrcpy's
// own demuxer treats it the same way ("Invalid packet length: 0"): there is no
// such thing as an empty access unit, so a zero here means the parse has come
// adrift rather than that a frame was small.
var ErrEmptyPacket = errors.New("scrcpy: packet header declared a zero-length payload; the stream is desynchronised")

// Session is a session header: the video size everything after it is expressed
// in, until the next one.
//
// Width and Height are u32 on the wire and kept as u32 here rather than
// narrowed on arrival, because narrowing at the parse would decide what an
// absurd size means in the wrong place. [ScreenFromSession] is where a size has
// to fit into a control message's u16 and where refusing one is the caller's
// problem to handle.
type Session struct {
	Width  uint32
	Height uint32

	// ClientResized is the server telling us this size came from a request we
	// made rather than from the device rotating. Nothing in this package
	// branches on it; it is decoded because dropping a bit that is on the wire
	// makes the next reader wonder whether it was missed or refused.
	ClientResized bool
}

// Packet is one encoded video access unit.
type Packet struct {
	// Config marks the codec configuration packet — SPS and PPS for H.264.
	// scrcpy's own client discards the timestamp on such a packet and hands
	// the decoder AV_NOPTS_VALUE. This reader does not: it reports the bits
	// that arrived and leaves that policy to the caller, because a parser that
	// deletes a field is a parser whose output cannot be compared against a hex
	// dump. Read [Packet.PTS] as meaningless when this is set.
	Config bool

	// KeyFrame marks a packet a decoder can start from.
	KeyFrame bool

	// PTS is the presentation timestamp in microseconds, from the device's
	// clock. It is not comparable with anything on this side of the wire.
	PTS uint64

	// Payload is the encoded bytes: Annex-B for H.264 and H.265, which is what
	// a browser's VideoDecoder takes without transcoding. The slice is freshly
	// allocated per packet and is the caller's to keep.
	Payload []byte
}

// Kind distinguishes the two things [Reader.Next] can return.
type Kind uint8

const (
	// KindSession is a session header: the video size changed, or this is the
	// first one and the stream has just told us what size it is.
	KindSession Kind = iota + 1

	// KindPacket is an encoded frame.
	KindPacket
)

// Unit is one header and whatever followed it.
//
// A tagged struct rather than two methods or an interface, because the caller
// is a loop that must handle both and a shape that lets it forget one is the
// bug described at the top of this file.
type Unit struct {
	Kind Kind

	// Session is meaningful when Kind is [KindSession].
	Session Session

	// Packet is meaningful when Kind is [KindPacket].
	Packet Packet
}

// Reader decodes one scrcpy video stream.
//
// It is not safe for concurrent use, holds no goroutine, and closes nothing: it
// reads from whatever io.Reader it was handed and the lifetime of that reader
// is entirely the caller's business. That is the whole of its relationship with
// the outside world.
type Reader struct {
	r     io.Reader
	codec CodecID
	hdr   [headerLen]byte
}

// NewReader consumes the four-byte codec id and returns a reader positioned at
// the first header.
//
// The codec is read here rather than lazily so that a stream which is not a
// scrcpy video stream at all fails at the constructor, before any caller has
// written a loop around it. A truncated or absent codec id comes back as
// io.EOF or io.ErrUnexpectedEOF unwrapped, so errors.Is on either works: those
// two distinguish "the server never started" from "the server died mid-word",
// and an operator wants to know which.
func NewReader(r io.Reader) (*Reader, error) {
	var id [4]byte
	if _, err := io.ReadFull(r, id[:]); err != nil {
		return nil, err
	}
	codec := CodecID(binary.BigEndian.Uint32(id[:]))
	if !codec.Known() {
		return nil, &UnknownCodecError{ID: codec}
	}
	return &Reader{r: r, codec: codec}, nil
}

// Codec reports what the stream announced.
func (r *Reader) Codec() CodecID { return r.codec }

// Next reads the next session header or packet.
//
// A clean end of stream — the server exited between units — is io.EOF. Every
// other truncation is io.ErrUnexpectedEOF, including a payload that stopped
// after zero bytes: a header that promised n bytes and delivered fewer is a
// truncation whatever the count, and reporting it as a short packet would hand
// a decoder a fragment of a frame and call it a frame.
//
// The order of the checks below is the point of this function. The declared
// length is compared against [MaxPacket] BEFORE it reaches make, so a hostile
// or wedged length costs one comparison rather than one allocation.
func (r *Reader) Next() (Unit, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return Unit{}, err
	}
	head := binary.BigEndian.Uint64(r.hdr[0:8])

	if head&flagSession != 0 {
		return Unit{
			Kind: KindSession,
			Session: Session{
				Width:         binary.BigEndian.Uint32(r.hdr[4:8]),
				Height:        binary.BigEndian.Uint32(r.hdr[8:12]),
				ClientResized: r.hdr[3]&1 != 0,
			},
		}, nil
	}

	n := binary.BigEndian.Uint32(r.hdr[8:12])
	if n == 0 {
		return Unit{}, ErrEmptyPacket
	}
	if n > MaxPacket {
		return Unit{}, &PacketTooLargeError{Length: n, Max: MaxPacket}
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Unit{}, err
	}

	return Unit{
		Kind: KindPacket,
		Packet: Packet{
			Config:   head&flagConfig != 0,
			KeyFrame: head&flagKeyFrame != 0,
			PTS:      head & ptsMask,
			Payload:  payload,
		},
	}, nil
}
