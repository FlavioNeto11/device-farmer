// Package screen owns one interactive-control session: the live video coming
// off a handset and a human's input going back to it.
//
// # What a session is, and what it is not
//
// A session is three ADB transports and a hardware encoder on a phone. It is
// NOT a claim on the device. The claim is a lease, it lives in farm.leases, and
// this package cannot create one, extend one or end one — it has no method that
// could and no database handle to do it with.
//
// That separation is the founding invariant of this system restated for a new
// kind of connection. A lease ends when the job says so, when a written-down
// deadline elapses, or when a human takes it back. Never because of
// connectivity. Everything in this file that can stop — a severed socket, a
// write deadline, a process shutting down, an encoder dying on the handset —
// stops BYTES. Afterwards the device is exactly as leased as it was before,
// because farm.leases.release_reason has seven allowed values and none of them
// is about a socket.
//
// The reason that needs saying out loud here, rather than being left to the
// schema, is that a screen is the first thing in this system a person WATCHES.
// A watched thing invites a liveness check, and a liveness check on a watched
// thing is one refactor away from "the viewer went away, so release the
// device". That is STF #663 with a video stream in front of it. If you find
// yourself adding a field to Session that a reaper would read, stop.
//
// # The api must not size anything from bytes a phone chose
//
// The process that imports this package also serves POST /leases/{id}/renew —
// the one request in the API whose failure costs a device. The scrcpy video
// stream is length-prefixed, and those lengths come from a handset that may be
// wedged. A u32 read from a wedged phone and handed to make() would take the
// renewal path down by OOM.
//
// So this package reads exactly sixteen bytes of the video stream: the
// four-byte codec id and the twelve-byte session header, both fixed-width. It
// reads them because the input path needs the frame's dimensions to place a
// touch, and there is nowhere else to learn them. Everything after those
// sixteen bytes is opaque: [Session.Video] hands back the reader untouched and
// the caller splices it with a buffer of its own choosing. No length a device
// wrote ever reaches an allocator in this process.
//
// The decoder is the browser's. WebCodecs takes Annex-B H.264 directly, which
// is what the handset already produces, so there is no transcode anywhere in
// the path and no frame is ever assembled in Go.
//
// # The three transports, and why the order matters
//
// Starting a session is four steps:
//
//  1. The jar is put on the device. This package does not do it — it calls the
//     EnsureJar callback it was handed, for the same reason
//     artifacts.PushFunc exists: a package that owned both the protocol and
//     the transfer would be a package that could not be tested without one.
//  2. The server is started with a shell service built by internal/scrcpy.
//     That stream stays open for the whole session and is DRAINED the whole
//     time. The server logs to it; a reader that stops reading fills the
//     buffer, blocks the server, and stalls the video with nothing anywhere
//     saying why.
//  3. The video socket is connected to the abstract socket the server
//     published.
//  4. The control socket is connected to the same name.
//
// Three and four are separate ADB transports, so each is separately admitted
// at the fence proxy, and they must happen in that order: the protocol carries
// no field distinguishing them and the server hands the first connection to
// the encoder. A client that opens them concurrently gets them swapped.
//
// Between two and three there is a race the protocol does not resolve: the
// server needs a moment to publish its socket, and a connect that arrives
// first is refused. [Open] retries with a bounded backoff and gives up with a
// message that says which of the two failures it was, because "the server
// never started" and "the server started and the socket never appeared" send
// an operator to different places.
//
// # Teardown runs no command
//
// Closing the three sockets is what ends the session. There is deliberately no
// kill step: a pkill for the server's class name would end every session on
// that handset, including one somebody else is driving. The server exits when
// its sockets go away, which is the behaviour its cleanup flag exists for.
//
// # One session per device
//
// scrcpy is one encoder per session. A second Open on a live device is refused
// rather than queued, because the alternative is two people driving one phone
// with neither able to tell.
//
// # What has never touched hardware
//
// Nothing in this package has run against an Android device. The command line
// it builds, the argument names it passes and the framing it expects are taken
// from the protocol and exercised against test/fakeadb. That is a real test of
// this code and it is not evidence about a phone. The first run against
// hardware should be read as a first run.
package screen
