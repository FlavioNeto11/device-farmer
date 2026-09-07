// Package scrcpy speaks the scrcpy wire protocol and does nothing else.
//
// It builds the command line that starts the server on a handset, it decodes
// the video stream that server writes back, and it encodes the input messages
// that travel the other way. It opens no socket, it reads no file, it knows
// nothing about a lease, a device row or an HTTP request. Every exported
// function here is a pure transformation between Go values and bytes, which is
// why every assertion in this package's tests is a golden-bytes comparison and
// why none of them need DATABASE_URL or a phone.
//
// # Why the protocol lives on its own
//
// docs/design/interactive-control.md argues that a live screen is a byte pipe
// and that the Go side of it should be a parser rather than a media stack:
// "that parses with encoding/binary and nothing else, and the payload is
// Annex-B H.264 that a browser's VideoDecoder accepts as-is". A parser with no
// I/O in it can be reviewed by reading it, and the review that matters here is
// not about goroutines. It is about arithmetic on numbers a wedged phone chose.
//
// # The rule this package exists to keep
//
// EVERY LENGTH READ OFF THE WIRE IS CHECKED BEFORE IT SIZES ANYTHING. The
// video framing carries a u32 payload length, and this package will eventually
// run inside the api server — the process that answers
// POST /api/v1/leases/{id}/renew, whose failure is the only failure in that API
// that costs a device (internal/api's package doc). A phone that emits
// 0xffffffff as a length must not become a four-gigabyte make() inside that
// process. [MaxPacket] is the cap, [PacketTooLargeError] is what a stream
// above it gets, and the refusal happens before the allocation rather than
// after it. internal/artifacts/apk.go bounds a decompressed manifest for the
// same reason and at a similar size; this is that discipline applied to a
// stream that never ends.
//
// # By construction, not by convention
//
// Two things here are shaped so that the wrong answer is unrepresentable
// rather than merely discouraged.
//
// The spawn command ([Spawn.Service]) is validated against the same alphabet
// and the same bounds that internal/fenceproxy's control class admits, so a
// command this package can build is a command the proxy will forward. That is
// not a claim to trust: admission_test.go runs every shape this builder can
// emit through fenceproxy.Admit and fails if one of them is refused. A builder
// that can produce a string the proxy rejects is a bug that would otherwise be
// discovered on a handset at 3am.
//
// The touch coordinate ([Screen], [Position]) can only be built from a
// [Session] that came off the video stream. scrcpy's coordinates are in the
// VIDEO coordinate space — after --max-size, --crop and the rotation filter —
// and the screen size that travels with every touch is a staleness guard so
// the server can tell the client was looking at a different frame, not a
// rescaling convenience. There is deliberately no constructor that takes two
// raw integers, because every wrong answer this type can give starts with
// somebody typing the device's physical resolution into it.
//
// # What is deliberately absent
//
// No audio, no clipboard, no UHID, no device-message decoding. scrcpy's
// control protocol has more than twenty message types; this package encodes
// three, because three is what a screen and a hand need and every one beyond
// them is a capability granted to whoever can reach the control socket.
package scrcpy
