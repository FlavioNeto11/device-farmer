package fakeadb

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------
// A scripted scrcpy device
//
// docs/design/interactive-control.md picks scrcpy as the transport for a live
// screen and a human's hands, and §3 sets out the framing: a codec id and the
// video geometry, then packets carrying flags, a presentation timestamp and a
// length. This fixture produces exactly that, over the three services
// internal/fenceproxy's control class whitelists — the app_process spawn and
// the two abstract sockets it publishes.
//
// THE TWO SIDES MUST AGREE, AND NOTHING MAKES THEM. internal/scrcpy parses
// this framing; this file produces it; they share no code on purpose, because
// a fixture and a parser built from one encoder prove only that the encoder is
// self-consistent. The wire format is the whole contract between them. Where
// §3's prose is loose this file follows scrcpy's own layout and says so
// against the field it affects, so a disagreement shows up here as a comment
// somebody can check rather than as a stream nobody can decode.
//
// It lives in test/fakeadb rather than in a package of its own because both
// internal/scrcpy and internal/api need it, and a fixture that lives with one
// of its callers is a fixture the other one imports for its side effects.
// ---------------------------------------------------------------------

// Codec ids, as scrcpy writes them: the four ASCII bytes of the codec name,
// big-endian, with AV1's short name left-padded with a NUL.
const (
	ScrcpyCodecH264 uint32 = 0x68323634 // "h264"
	ScrcpyCodecH265 uint32 = 0x68323635 // "h265"
	ScrcpyCodecAV1  uint32 = 0x00617631 // "av1"
)

// ScrcpySpawnPrefix is the device service the server jar is started on. It is
// the prefix of the whitelist pattern in internal/fenceproxy's control class:
// the classpath names the jar by the first twelve hex of its sha256, so no
// literal command line can be pinned here, and the fixture matches on the one
// part of it that is fixed.
const ScrcpySpawnPrefix = "shell,v2,raw:CLASSPATH="

// ScrcpySocketPrefix is the abstract socket family scrcpy publishes. The
// eight hex digits that follow are the session id the CLIENT chose and passed
// to the server on its command line — which is why bounding this prefix is
// what stops the fence proxy's rule from reading "any abstract socket on the
// phone, including another app's".
const ScrcpySocketPrefix = "localabstract:scrcpy_"

// Video packet header flags. scrcpy packs them into the top of a big-endian
// uint64 whose remaining bits are the presentation timestamp in microseconds.
//
// §3 of the design calls this "a 12-byte header (flags + 61-bit PTS) and a
// u32 length"; the 12 bytes and the u32 are right, and the timestamp field is
// 62 bits wide because scrcpy spends two bits on flags, not three. The
// difference matters only for a stream running past 146,000 years, and it is
// written down because a reader comparing this file against that sentence
// deserves to know which one moved.
const (
	scrcpyFlagConfig   uint64 = 1 << 63
	scrcpyFlagKeyFrame uint64 = 1 << 62
	scrcpyPTSMask      uint64 = scrcpyFlagKeyFrame - 1
)

// The same three fields in the layout a server that sends SESSION HEADERS
// uses, which [ScrcpyConfig.VideoSessionHeader] selects.
//
// The two layouts differ by one bit position and the difference is not
// cosmetic. A server that can re-announce its video geometry mid-stream needs a
// way to say "this twelve-byte header is a geometry, not a frame", and the only
// unclaimed space is the top bit — so the top bit became the discriminator and
// the flags below it each moved down one. That is the layout internal/scrcpy
// parses, documented against app/src/demuxer.c at the top of its video.go:
//
//	bit 63  this header is a session header
//	bit 62  the packet is codec configuration
//	bit 61  the packet is a key frame
//	bits 60..0  the presentation timestamp
//
// WRITING ONE LAYOUT'S FLAGS UNDER THE OTHER'S FRAMING IS NOT A NEAR MISS. A
// config packet in the older layout sets bit 63, which a reader expecting the
// newer one reads as a session header — so it takes the packet's payload length
// for a video height, reports a geometry nobody announced, and resynchronises
// somewhere inside a frame. Which is why the two sets are one choice and not
// two, and why VideoSessionHeader moves the flags as well as adding the header.
const (
	scrcpyFlagSession         uint64 = 1 << 63
	scrcpySessionFlagConfig   uint64 = 1 << 62
	scrcpySessionFlagKeyFrame uint64 = 1 << 61
	scrcpySessionPTSMask      uint64 = scrcpySessionFlagKeyFrame - 1
)

// scrcpyVideoHeaderLen is the preamble a server without session headers
// writes: codec id, width, height, each a big-endian uint32, all twelve bytes
// of it before the first packet and never repeated.
const scrcpyVideoHeaderLen = 12

// scrcpyCodecIDLen is the codec id on its own, which is what precedes a
// SESSION header rather than being packed into it.
const scrcpyCodecIDLen = 4

// scrcpyPacketHeaderLen is flags+PTS (uint64) then the payload length
// (uint32).
const scrcpyPacketHeaderLen = 12

// ScrcpyPacket is one video packet the fixture will write.
type ScrcpyPacket struct {
	// PTS is the presentation timestamp in microseconds. Only the low 62
	// bits reach the wire; the top two carry the flags below.
	PTS uint64

	// Config marks the codec configuration packet — the SPS/PPS a decoder
	// needs before any frame. scrcpy sends it first and ignores its PTS.
	Config bool

	// KeyFrame marks a frame a decoder may start from.
	KeyFrame bool

	// Data is the payload: Annex-B H.264 in the real thing, and either that or
	// arbitrary bytes here.
	//
	// Recognisable ASCII is still the right choice for a test about the
	// FRAMING, because an assertion against "SPS-PPS" reads as an assertion and
	// one against a hex dump reads as noise. It is the wrong choice for
	// everything downstream of the framing, which is why
	// [ScrcpyPacketsFromAnnexB] exists: a browser's VideoDecoder does not care
	// what a Go test asserted, and a payload no decoder accepts leaves the
	// whole decode path with no coverage and an operator with a black
	// rectangle. Use real bytes when the test is about video and readable ones
	// when it is about bytes.
	Data []byte
}

// ScrcpyConfig describes one scripted scrcpy device.
type ScrcpyConfig struct {
	// Devpath is the position this device occupies. It is added to the
	// server's table if it is not there already, so a scrcpy test can be a
	// one-liner.
	Devpath string

	// SCID is the eight-hex session id the abstract sockets are named after,
	// used when a client connects without having spawned the server, or
	// spawns it without naming one. A spawn that names a session id wins:
	// the id is the client's to choose.
	//
	// Defaulted from Devpath so the fixture is usable without a spawn at
	// all, which is what a test of the video framing alone wants.
	SCID string

	// Codec, Width and Height fill the session header. Zero values become
	// h264 at a Pixel 6a's panel size.
	Codec         uint32
	Width, Height uint32

	// VideoPrefix is written ahead of the session header, untouched.
	//
	// It exists because a real server writes things §3 does not describe: a
	// one-byte handshake on the first socket when the connection was made
	// through a forwarded tunnel, and a 64-byte device name when it was
	// asked for one. Both are the client's choice to request, so the fixture
	// has no opinion and takes bytes.
	VideoPrefix []byte

	// Packets are written in order on the video socket.
	Packets []ScrcpyPacket

	// PacketGap is slept before each packet. A live screen arrives over
	// time, and a client that only works when every frame is already in the
	// socket buffer is a client that will not work on a phone.
	PacketGap time.Duration

	// VideoEOF closes the video socket after the last packet instead of
	// holding it open. The default models a live screen, which does not end;
	// set this when the test wants a stream that finishes so it can read to
	// EOF.
	//
	// It contradicts VideoLoop, and the contradiction is a panic at install
	// time rather than a precedence rule: see the note on VideoLoop.
	VideoEOF bool

	// VideoLoop replays Packets forever instead of parking after the last one,
	// with PTS continuing to advance across loops.
	//
	// A fixture clip is seconds long and a screen session is minutes long. A
	// four-second loop of a test pattern is a live screen for every purpose a
	// test or a demo has; the same four seconds played once is a screen that
	// froze four seconds after somebody opened it, which is exactly the
	// symptom a stalled transport produces and therefore exactly the symptom
	// nobody can use the fixture to rule out.
	//
	// THE TIMESTAMPS ADVANCE, THEY DO NOT RESTART, and that is the whole
	// subtlety of looping a clip. A decoder uses the presentation timestamp to
	// decide when a frame is due. Hand it a stream whose time jumps back four
	// seconds at the splice and every frame of the next pass is already late:
	// a forgiving decoder drops the lot, a strict one treats the sequence as
	// corrupt and stops, and either way the failure appears four seconds in,
	// which is long enough after the connection that it reads as a transport
	// fault. So each pass adds the duration of one whole pass to every
	// timestamp in it, and the sequence the client sees never goes backwards.
	//
	// Each pass replays from packet zero, config packet included. That is not
	// laziness about the loop boundary — it is what a real server does for a
	// decoder that joined late, and the fixture's clip repeats its parameter
	// sets on every keyframe for the same reason.
	//
	// VideoLoop and VideoEOF are contradictory: one says the stream never ends
	// and the other says it ends now. [ScrcpyFixture] PANICS when both are set
	// rather than picking a winner, because both orders of precedence are
	// defensible and neither is guessable from the call site — a test that
	// asked for both has a bug in its own setup, and the stack trace of a panic
	// points at the line that wrote it. A silent precedence would instead give
	// that test a stream it did not ask for and let it pass or hang for reasons
	// nowhere near the mistake.
	VideoLoop bool

	// VideoSessionHeader writes the video geometry as an in-stream session
	// header instead of as a preamble packed in beside the codec id.
	//
	// WHICH ONE IS RIGHT DEPENDS ON WHICH SERVER YOU ARE PRETENDING TO BE, AND
	// THE TWO ARE NOT INTERCHANGEABLE. Left off, the socket opens with twelve
	// bytes — codec id, width, height — and then packets whose flags sit at
	// bits 63 and 62. That is the shape internal/adbwire's own duplex test
	// reads off the wire by offset, and it is the default so that test keeps
	// describing what the fixture does. Turned on, the socket opens with the
	// four-byte codec id and then a twelve-byte session header marked by the
	// top bit, and the packet flags move down to bits 62 and 61 — which is the
	// shape internal/scrcpy's Reader parses, and therefore the only shape whose
	// frames reach a decoder at all.
	//
	// A fixture that could only write the first shape could not be read by this
	// repository's own parser: internal/scrcpy would take the width for the top
	// half of a header, find the flags where it expects a length, and fail with
	// PacketTooLargeError before the first frame. A fake whose output the
	// production reader rejects is not a fake of anything. Hence the knob, and
	// hence TestTheFixturesVideoIsReadableByInternalScrcpy, which is the only
	// test in this package that makes the two halves meet.
	VideoSessionHeader bool

	// ServerLog is written to the spawn's stdout as one shell v2 packet, the
	// way the server jar announces itself. Nothing parses it.
	ServerLog string

	// ControlFramer splits the recorded control bytes into messages for
	// ControlWrites. Defaults to ScrcpyControlMessageLen.
	ControlFramer ScrcpyControlFramer
}

// ScrcpyFixture installs a scripted scrcpy device.
//
// It answers three services on one devpath:
//
//   - the app_process spawn, which records the command line, publishes the
//     session id it names, announces itself on stdout and then stays running
//     for as long as the client keeps the socket — because the server jar is
//     a process, and a fixture whose process exits immediately would let a
//     client that never noticed pass;
//   - the first connection to localabstract:scrcpy_<scid>, which is the video
//     socket: the session header and the configured packets;
//   - the second, which is the control socket: it writes nothing and records
//     every byte the client sends, for ControlWrites.
//
// WHY THE SOCKETS ARE REGISTERED HERE AND NOT BY THE SPAWN. The abstract
// socket does not exist until the server binds it, so answering it out of the
// spawn handler is the faithful order — and it is a race. A client that
// connects before the fixture has finished registering would fall through to
// the default scripted response and read Echo's text as though it were a
// video header, which is a fixture defect wearing a decoder bug's clothes.
// Registering the FAMILY up front and letting the spawn publish the NAME
// keeps the ordering honest without the race: a connection to a session id
// nobody spawned is refused with a reset, which is roughly what a client sees
// from a socket that is not there yet, and is unmistakably not a video
// header.
func ScrcpyFixture(cfg ScrcpyConfig) Fixture {
	return func(s *Server) {
		if cfg.Devpath == "" {
			panic("fakeadb: ScrcpyFixture needs a Devpath — the physical position is the key")
		}
		if cfg.VideoLoop && cfg.VideoEOF {
			panic("fakeadb: ScrcpyFixture was given both VideoLoop and VideoEOF — " +
				"one says the screen never ends and the other says it ends after the last packet; " +
				"a fixture that picked a winner would hand this test a stream it did not ask for")
		}
		if cfg.VideoLoop && len(cfg.Packets) == 0 {
			// Refused rather than served, because there is no reading of "replay
			// nothing forever" that a caller could have meant, and the shape it
			// produces is the worst one available: a loop whose body does not
			// block, spinning a core until the viewer disconnects. A fixture that
			// pegs a CPU is found by whoever notices the fan, not by whoever
			// wrote the line.
			panic("fakeadb: ScrcpyFixture was given VideoLoop with no Packets — " +
				"replaying an empty list forever is a busy loop, not a screen")
		}
		if cfg.SCID == "" {
			cfg.SCID = scrcpyDefaultSCID(cfg.Devpath)
		}
		if cfg.Codec == 0 {
			cfg.Codec = ScrcpyCodecH264
		}
		if cfg.Width == 0 {
			cfg.Width = 1080
		}
		if cfg.Height == 0 {
			cfg.Height = 2400
		}
		if cfg.ServerLog == "" {
			cfg.ServerLog = "[server] INFO: Device: fakeadb Pixel_6a (Android 14)\n"
		}
		if cfg.ControlFramer == nil {
			cfg.ControlFramer = ScrcpyControlMessageLen
		}

		if _, ok := s.Device(cfg.Devpath); !ok {
			s.Add(Device{
				Serial:   scrcpySerial(cfg.Devpath),
				Devpath:  cfg.Devpath,
				Model:    "Pixel 6a",
				Product:  "bluejay",
				Codename: "bluejay",
				State:    StateDevice,
			})
		}

		dev := &scrcpyDevice{cfg: cfg, scid: cfg.SCID}
		s.mu.Lock()
		if s.scrcpy == nil {
			s.scrcpy = make(map[string]*scrcpyDevice)
		}
		s.scrcpy[cfg.Devpath] = dev
		s.mu.Unlock()

		s.RespondStream(cfg.Devpath, ScrcpySpawnPrefix, dev.serveSpawn)
		s.RespondStream(cfg.Devpath, ScrcpySocketPrefix, dev.serveSocket)
	}
}

// scrcpyDevice is one device's scrcpy state. Everything a test reads back
// lives here rather than on Server, so two scripted handsets in one farm
// cannot see each other's control traffic.
type scrcpyDevice struct {
	mu     sync.Mutex
	cfg    ScrcpyConfig
	scid   string
	spawns []string

	// The three fields below belong to the CURRENT session and not to the
	// device's whole lifetime, which is the whole of the fix described on
	// serveSpawn.
	//
	// session is that session's generation, bumped by every spawn. It exists so
	// that a handler still running from a superseded session cannot write into
	// the new one's state: see videoWentLive, where a stale handler doing
	// exactly that would silently re-open the gate the new session is relying
	// on.
	session int

	// sockets is how many roles this session has handed out: zero means the
	// next connection is the video socket, one means it is the control socket,
	// two means the session's listener is spent.
	sockets int

	// videoLive says the video socket of this session has got as far as writing
	// its session header, which is what lets serveSocket tell a client that
	// connected its two sockets in order from one that connected them at the
	// same time. See the comment there; this field is the whole mechanism.
	videoLive bool

	controlBytes []byte
}

func (d *scrcpyDevice) config() ScrcpyConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// serveSpawn answers the app_process command line.
//
// A SPAWN STARTS A NEW SESSION, AND A NEW SESSION GETS A NEW PAIR OF SOCKETS.
// It did not, and the bug that came of it had a misleading error message
// attached. The socket counter was the device's rather than the session's, so
// it only ever went up: the first session took counts zero and one, and the
// second session — which is what a viewer reconnecting, a fence bump or a lease
// handover produces, none of them rare — found the counter at two and had every
// one of its connections refused by the arm that exists to catch a client
// reconnecting to a spent listener. The refusal named the client. The cause was
// the fixture.
//
// Resetting here is also the faithful order. A real spawn is a new server
// process that binds a new listener; the old process's listener does not
// survive it, and neither does the count of what it had accepted.
func (d *scrcpyDevice) serveSpawn(sess *StreamSession) error {
	cmd := strings.TrimPrefix(sess.Service, "shell,v2,raw:")

	d.mu.Lock()
	d.spawns = append(d.spawns, cmd)
	if scid, ok := scrcpyArg(cmd, "scid"); ok {
		d.scid = scid
	}
	d.session++
	d.sockets = 0
	d.videoLive = false
	log := d.cfg.ServerLog
	d.mu.Unlock()

	if _, err := sess.Write(shellPacket(shellPacketStdout, log)); err != nil {
		// The client hung up before reading the banner. That is a client's
		// prerogative, not a fixture failure, and reporting it as one would
		// sever a socket that is already gone and file an error against a
		// request nobody is waiting on.
		return nil
	}

	// The jar runs until it is killed. Parking here is what makes a test that
	// forgets to close the spawn stream visible as a hung Close rather than
	// as a server that quietly stopped serving.
	<-sess.Done
	return nil
}

// serveSocket answers a connection to an abstract socket: the first goes to the
// video stream and the second to the control stream.
//
// ORDER IS THE ONLY SIGNAL THERE IS, AND THAT IS THE PROTOCOL'S DOING. A real
// scrcpy server publishes ONE localabstract socket and the client connects it
// twice, video first. The second connection carries no field, no handshake and
// no length distinguishing it from the first; the server tells them apart by
// which one it accepted first and nothing else. So a fake cannot invent a
// better signal, and the only question is what it does when the signal is
// ambiguous.
//
// It used to swap the roles, about half the time. Each connection arrives on its
// own goroutine, so two connections opened at the same instant reached the
// counter below in whichever order the scheduler chose, and the client that
// meant to open video got the control handler — which writes nothing, so the
// symptom was a video socket that produced no bytes, and a doc comment here
// asserted the client's ordering as though this code enforced it.
//
// What makes the role deterministic now is that a SECOND role is not handed out
// until the FIRST one is live, where live means its video handler has written
// the session header. THAT IS A REQUIREMENT ON THE CLIENT AND IT IS WRITTEN DOWN
// HERE BECAUSE IT IS THE ONLY PLACE IT CAN BE: open the video socket, read its
// session header, and only then open the control socket. A client that opens
// both without waiting is refused on the second, by name, because two sockets
// racing is a client bug that a real handset resolves by coin flip and a fake
// that resolved it the same way would be the least useful possible version of
// this fixture. One of the two is served video, the other is told what it did.
// Which one is refused is still the scheduler's choice; that a role is never
// silently swapped is not.
//
// The gate is armed by the CLAIM and released by the HEADER, and the span
// between those two is short but not empty — it is the time it takes the video
// handler to get from here into serveVideo and out through one write. A client
// that has read the header cannot be inside that span, which is why the reading
// is the requirement rather than a suggestion.
//
// A video socket that dies before its header goes out GIVES THE ROLE BACK, in
// serveVideo. Without that the gate is a trap: the claim is spent, nothing will
// ever set videoLive, and every later connection — including the client's honest
// retry of the video socket — is refused with a message about racing that has
// nothing to do with what happened. Only a respawn would recover, and a fake
// that needs a respawn to recover from a dropped connection is not modelling
// anything a handset does.
//
// The session id is read under the same lock as the role, so a respawn that
// renames the sockets cannot be half applied. A socket named after the previous
// session's id is then refused, which is correct and not a race: the previous
// session's listener is gone with the process that bound it. The message says
// how many spawns have happened so that a log reads as "you asked for the old
// one" rather than as a mystery.
func (d *scrcpyDevice) serveSocket(sess *StreamSession) error {
	name := strings.TrimPrefix(sess.Service, "localabstract:")

	d.mu.Lock()
	want := "scrcpy_" + d.scid
	spawns := len(d.spawns)
	gen := d.session
	n := d.sockets
	racing := n == 1 && !d.videoLive
	if name == want && !racing {
		d.sockets++
	}
	d.mu.Unlock()

	if name != want {
		return fmt.Errorf("fakeadb: %s has no abstract socket %q; after %d spawn(s) the live session published %q",
			sess.Devpath, name, spawns, want)
	}
	if racing {
		// The role was NOT consumed, so the video socket this raced is still
		// the video socket and the next orderly connection is still the control
		// socket. Refusing without consuming is what keeps one client's mistake
		// from costing the session a socket nobody ever used.
		return fmt.Errorf("fakeadb: %s: a second connection to %q arrived before the first had "+
			"written its session header; scrcpy's two sockets are told apart by connection order "+
			"alone, so they must be opened one after the other — open video, read its session "+
			"header, then open control", sess.Devpath, name)
	}

	switch n {
	case 0:
		return d.serveVideo(sess, gen)
	case 1:
		return d.serveControl(sess)
	default:
		// scrcpy publishes one socket per stream and closes the listener. A
		// third connection means the client reconnected without respawning,
		// and answering it with a second video stream would hide that.
		return fmt.Errorf("fakeadb: %s: connection %d to %q; this session published two sockets",
			sess.Devpath, n+1, name)
	}
}

// serveVideo writes the session header and the scripted packets.
//
// A failed write ends the service without an error: the far side closing a
// screen stream is the ordinary way a screen stream ends, and calling it a
// fixture failure would sever a socket the client already closed and put a
// scary line in the request log for the least interesting event there is.
func (d *scrcpyDevice) serveVideo(sess *StreamSession, gen int) error {
	cfg := d.config()

	if len(cfg.VideoPrefix) > 0 {
		if _, err := sess.Write(cfg.VideoPrefix); err != nil {
			d.videoGaveUp(gen)
			return nil
		}
	}

	if err := d.writeVideoHeader(sess, cfg); err != nil {
		// The client went away before it had a session header. It never had a
		// video socket, so the session must not go on believing it handed one
		// out: see videoGaveUp.
		d.videoGaveUp(gen)
		return nil
	}

	// The session is live from here, which is what releases the control socket.
	// It is marked AFTER the header is on the wire rather than when this handler
	// was entered, because the thing the client is required to wait for before
	// opening its second socket is the header — so marking it here is what makes
	// serveSocket's gate invisible to a client that follows the rule and visible
	// to one that does not.
	d.videoWentLive(gen)

	period := scrcpyLoopPeriod(cfg.Packets)
	for pass := uint64(0); ; pass++ {
		// Checked at the top of every pass as well as in the gap below, so a
		// gapless loop against a client that has gone away ends here rather
		// than on whichever write the kernel happens to refuse first.
		select {
		case <-sess.Done:
			return nil
		default:
		}

		for _, p := range cfg.Packets {
			if cfg.PacketGap > 0 {
				t := time.NewTimer(cfg.PacketGap)
				select {
				case <-t.C:
				case <-sess.Done:
					t.Stop()
					return nil
				}
			}
			var ph [scrcpyPacketHeaderLen]byte
			binary.BigEndian.PutUint64(ph[0:8], scrcpyMeta(p, p.PTS+pass*period, cfg.VideoSessionHeader))
			binary.BigEndian.PutUint32(ph[8:12], uint32(len(p.Data)))
			// Header and payload go out as two writes on purpose. A client that
			// only works when both arrive in one read is a client that works
			// against this fixture and not against a phone.
			if _, err := sess.Write(ph[:]); err != nil {
				// The viewer closed the tab. Ending the loop on a failed write
				// is the same rule the rest of this handler follows and it is
				// the only thing stopping a loop from spinning on a dead socket
				// for the life of the server.
				return nil
			}
			if _, err := sess.Write(p.Data); err != nil {
				return nil
			}
		}

		if !cfg.VideoLoop {
			break
		}
	}

	if cfg.VideoEOF {
		return nil
	}
	<-sess.Done
	return nil
}

// videoWentLive records that the video socket of session gen has its header out,
// which is what lets the next connection be the control socket.
//
// THE GENERATION CHECK IS NOT DEFENSIVE PROGRAMMING. A spawn can land while a
// previous session's video handler is still parked in the write of a long
// VideoPrefix — the old handler does not know it has been superseded, because
// nothing tells it. Without the check that handler's eventual success would mark
// the NEW session's video socket live, which nobody has connected yet, and the
// new session's gate would be open from the moment it began. Two of its sockets
// opened at once would then be assigned by scheduler order again: the exact bug
// the gate exists to prevent, reintroduced by the fix to a different one.
func (d *scrcpyDevice) videoWentLive(gen int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == gen {
		d.videoLive = true
	}
}

// videoGaveUp hands the video role back after a socket died before it ever
// carried a session header.
//
// The claim is spent at the moment serveSocket hands out role zero, and nothing
// after that point can set videoLive if the write never lands. A session left in
// that state is WEDGED, not merely odd: every subsequent connection sees one role
// handed out and no live video, which is the racing condition, so it is refused —
// including the client's own honest retry of the video socket, and including the
// retry after that. The refusal blames the client for opening two sockets at once
// when it has none open at all, and only a respawn recovers.
//
// A client whose video socket dropped is the ordinary case this has to survive: a
// closed tab, a lease that moved, a proxy that hiccuped. So the role goes back,
// under the same conditions that prove it is still ours to return — this
// session's, still the only one handed out, still not live.
func (d *scrcpyDevice) videoGaveUp(gen int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == gen && d.sockets == 1 && !d.videoLive {
		d.sockets = 0
	}
}

// writeVideoHeader opens the video socket in whichever of the two shapes
// [ScrcpyConfig.VideoSessionHeader] asked for. The bytes are described there
// and the layouts are described against the flag constants; what is here is
// only the encoding.
func (d *scrcpyDevice) writeVideoHeader(sess *StreamSession, cfg ScrcpyConfig) error {
	if !cfg.VideoSessionHeader {
		var hdr [scrcpyVideoHeaderLen]byte
		binary.BigEndian.PutUint32(hdr[0:4], cfg.Codec)
		binary.BigEndian.PutUint32(hdr[4:8], cfg.Width)
		binary.BigEndian.PutUint32(hdr[8:12], cfg.Height)
		_, err := sess.Write(hdr[:])
		return err
	}

	var id [scrcpyCodecIDLen]byte
	binary.BigEndian.PutUint32(id[:], cfg.Codec)
	if _, err := sess.Write(id[:]); err != nil {
		return err
	}

	// The flag is written as the whole top uint64 and then partly overwritten,
	// rather than as a literal 0x80 in byte zero, so that the one constant
	// naming this bit is the one the header is built from. client_resized is
	// the low bit of byte three and stays clear: this fixture's geometry is the
	// device's, never a response to a resize the client asked for.
	var sh [scrcpyPacketHeaderLen]byte
	binary.BigEndian.PutUint64(sh[0:8], scrcpyFlagSession)
	binary.BigEndian.PutUint32(sh[4:8], cfg.Width)
	binary.BigEndian.PutUint32(sh[8:12], cfg.Height)
	_, err := sess.Write(sh[:])
	return err
}

// scrcpyMeta packs one packet's flags and timestamp into the top eight bytes of
// its header, in whichever revision's bit layout the socket is speaking.
func scrcpyMeta(p ScrcpyPacket, pts uint64, sessionHeaders bool) uint64 {
	if sessionHeaders {
		meta := pts & scrcpySessionPTSMask
		if p.Config {
			meta |= scrcpySessionFlagConfig
		}
		if p.KeyFrame {
			meta |= scrcpySessionFlagKeyFrame
		}
		return meta
	}
	meta := pts & scrcpyPTSMask
	if p.Config {
		meta |= scrcpyFlagConfig
	}
	if p.KeyFrame {
		meta |= scrcpyFlagKeyFrame
	}
	return meta
}

// scrcpyLoopPeriod is how much time one whole pass of the packet list takes, in
// the microseconds the packets' own timestamps are in. It is what each loop adds
// to every timestamp in the next pass.
//
// The last packet's timestamp plus one frame interval, because the last frame is
// DISPLAYED for an interval and the pass is not over until it has been: summing
// to the last timestamp alone would make the last frame of one pass and the
// first of the next due at the same instant, and a decoder handed two frames
// with one timestamp has been told the stream stuttered.
//
// The interval is inferred from the gap between the last two distinct
// timestamps, because a packet list carries no frame rate and the alternative —
// another field on ScrcpyConfig that every caller has to keep consistent with
// the timestamps it already wrote — is a field that will disagree with them. A
// list with fewer than two distinct timestamps has no interval to infer and gets
// one microsecond, which is enough to keep the sequence from repeating itself
// and is the honest answer for a clip that is one frame long.
func scrcpyLoopPeriod(packets []ScrcpyPacket) uint64 {
	var high, last, prev uint64
	seen := 0
	for _, p := range packets {
		if p.PTS > high {
			high = p.PTS
		}
		if seen > 0 && p.PTS == last {
			// The config packet shares its timestamp with the frame it
			// describes, so the list's first two entries are not an interval.
			continue
		}
		prev, last = last, p.PTS
		seen++
	}
	interval := uint64(1)
	if seen >= 2 && last > prev {
		interval = last - prev
	}
	// Measured from the HIGHEST timestamp rather than the last one, so that a
	// packet list somebody wrote out of order still produces a period larger
	// than everything in it. The promise this makes the decoder is that the
	// sequence never goes backwards, and that promise has to survive a list the
	// fixture did not generate.
	return high + interval
}

// serveControl records what the client sends and sends nothing back.
//
// The real control socket does carry device-to-client messages — a clipboard
// answer, an acknowledgement — and this fixture deliberately has none. They
// are a reply to something, so scripting them means scripting a conversation,
// and the seam for that already exists: RespondStream this service yourself
// and the recording is yours to keep. What is here is the half an input test
// needs, which is the exact bytes a tap produced.
func (d *scrcpyDevice) serveControl(sess *StreamSession) error {
	buf := make([]byte, 4096)
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			d.mu.Lock()
			d.controlBytes = append(d.controlBytes, buf[:n]...)
			d.mu.Unlock()
		}
		if err != nil {
			return nil
		}
	}
}

// ---------------------------------------------------------------------
// Reading a scripted scrcpy device back
// ---------------------------------------------------------------------

func (s *Server) scrcpyFor(devpath string) *scrcpyDevice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrcpy[devpath]
}

// ControlWrites returns the control messages the client sent to the scrcpy
// device at devpath, one element per message, bytes verbatim.
//
// It splits rather than returning what each read happened to hand over,
// because TCP does not preserve write boundaries: two taps written
// back to back may arrive as one read or as three, so a test asserting on
// read chunks would pass or fail on timing. Splitting by the protocol's own
// lengths is the only version of "exact frames" that means anything.
//
// A trailing partial message is held back until the rest of it arrives; use
// ControlBytes for the recording with no framing opinion applied to it.
func (s *Server) ControlWrites(devpath string) [][]byte {
	dev := s.scrcpyFor(devpath)
	if dev == nil {
		return nil
	}
	dev.mu.Lock()
	raw := append([]byte(nil), dev.controlBytes...)
	framer := dev.cfg.ControlFramer
	dev.mu.Unlock()

	if framer == nil {
		framer = ScrcpyControlMessageLen
	}
	var out [][]byte
	for len(raw) > 0 {
		n, ok := framer(raw)
		if !ok {
			break
		}
		if n <= 0 || n > len(raw) {
			n = len(raw)
		}
		out = append(out, raw[:n:n])
		raw = raw[n:]
	}
	return out
}

// ControlBytes returns every byte the client wrote to the control socket,
// concatenated and unframed. It is the ground truth ControlWrites is a
// reading of, and the thing to print when a framing assertion fails.
func (s *Server) ControlBytes(devpath string) []byte {
	dev := s.scrcpyFor(devpath)
	if dev == nil {
		return nil
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	return append([]byte(nil), dev.controlBytes...)
}

// ScrcpySCID returns the session id the abstract sockets of the device at
// devpath are currently named after: the one the last spawn asked for, or the
// configured default if nothing has spawned yet.
func (s *Server) ScrcpySCID(devpath string) (string, bool) {
	dev := s.scrcpyFor(devpath)
	if dev == nil {
		return "", false
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	return dev.scid, true
}

// ScrcpySpawns returns the app_process command lines the device at devpath
// was started with, in order, with the service prefix stripped.
func (s *Server) ScrcpySpawns(devpath string) []string {
	dev := s.scrcpyFor(devpath)
	if dev == nil {
		return nil
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	return append([]string(nil), dev.spawns...)
}

// ---------------------------------------------------------------------
// The control protocol's lengths
// ---------------------------------------------------------------------

// ScrcpyControlFramer reports the length of the message at the front of buf.
// ok is false when buf does not yet hold a whole one.
type ScrcpyControlFramer func(buf []byte) (n int, ok bool)

// scrcpyControlVarMax bounds a length field read off the wire. Past it the
// recording is desynchronised rather than long, and sizing anything from the
// number is the mistake the sync server refuses to make in the other
// direction.
const scrcpyControlVarMax = 1 << 20

// Control message types, as scrcpy numbers them.
const (
	scrcpyMsgInjectKeycode       = 0
	scrcpyMsgInjectText          = 1
	scrcpyMsgInjectTouch         = 2
	scrcpyMsgInjectScroll        = 3
	scrcpyMsgBackOrScreenOn      = 4
	scrcpyMsgExpandNotifications = 5
	scrcpyMsgExpandSettings      = 6
	scrcpyMsgCollapsePanels      = 7
	scrcpyMsgGetClipboard        = 8
	scrcpyMsgSetClipboard        = 9
	scrcpyMsgSetDisplayPower     = 10
	scrcpyMsgRotateDevice        = 11
)

// ScrcpyControlMessageLen is the default ScrcpyControlFramer: scrcpy's own
// control message layout, written out field by field so a reader can check it
// against the encoder rather than trust it.
//
// The table covers types 0 through 11 — everything a screen and a pair of
// hands needs. The higher numbers (the UHID family, START_APP, RESET_VIDEO)
// are deliberately absent: their layouts moved between scrcpy releases, and a
// fixture that guessed one would frame a recording confidently and wrongly.
// An unrecognised type is therefore not framed at all; the rest of the
// recording comes back as one piece, so a test fails with the bytes in front
// of it instead of with a plausible-looking split.
func ScrcpyControlMessageLen(buf []byte) (int, bool) {
	if len(buf) == 0 {
		return 0, false
	}
	switch buf[0] {
	case scrcpyMsgInjectKeycode:
		// action u8, keycode u32, repeat u32, metastate u32
		return scrcpyFixedLen(buf, 1+1+4+4+4)
	case scrcpyMsgInjectText:
		// u32 length, then that many bytes of UTF-8
		return scrcpyVarLen(buf, 1)
	case scrcpyMsgInjectTouch:
		// action u8, pointer id u64, then the position — x i32, y i32,
		// screen width u16, screen height u16 — then pressure u16,
		// action button u32, buttons u32.
		//
		// The position carries the screen size because scrcpy's coordinates
		// are in the VIDEO space, after --max-size, --crop and rotation; the
		// size travelling with every touch is a staleness guard, and a proxy
		// that reads these as device pixels sends taps to the wrong place.
		return scrcpyFixedLen(buf, 1+1+8+(4+4+2+2)+2+4+4)
	case scrcpyMsgInjectScroll:
		// position (12), hscroll i16, vscroll i16, buttons u32
		return scrcpyFixedLen(buf, 1+(4+4+2+2)+2+2+4)
	case scrcpyMsgBackOrScreenOn:
		// action u8
		return scrcpyFixedLen(buf, 1+1)
	case scrcpyMsgExpandNotifications, scrcpyMsgExpandSettings,
		scrcpyMsgCollapsePanels, scrcpyMsgRotateDevice:
		// the type byte and nothing else
		return scrcpyFixedLen(buf, 1)
	case scrcpyMsgGetClipboard:
		// copy key u8
		return scrcpyFixedLen(buf, 1+1)
	case scrcpyMsgSetClipboard:
		// sequence u64, paste u8, u32 length, then the text
		return scrcpyVarLen(buf, 1+8+1)
	case scrcpyMsgSetDisplayPower:
		// on u8
		return scrcpyFixedLen(buf, 1+1)
	default:
		return len(buf), true
	}
}

func scrcpyFixedLen(buf []byte, n int) (int, bool) {
	if len(buf) < n {
		return 0, false
	}
	return n, true
}

// scrcpyVarLen frames a message whose payload length is a big-endian uint32
// at off.
func scrcpyVarLen(buf []byte, off int) (int, bool) {
	if len(buf) < off+4 {
		return 0, false
	}
	size := binary.BigEndian.Uint32(buf[off : off+4])
	if size > scrcpyControlVarMax {
		// Not a length: a desynchronised recording. Hand the rest back whole
		// rather than waiting for a gigabyte that is never coming.
		return len(buf), true
	}
	total := off + 4 + int(size)
	if len(buf) < total {
		return 0, false
	}
	return total, true
}

// ---------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------

// scrcpyArg pulls "key=value" out of the server's command line. The command
// line is space separated and the values contain no spaces — the fence
// proxy's whitelist pattern will not admit one that does — so splitting on
// spaces is exact rather than approximate here.
func scrcpyArg(cmd, key string) (string, bool) {
	for _, f := range strings.Fields(cmd) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// scrcpyDefaultSCID derives the eight hex digits from the devpath, so a farm
// of scripted handsets has a distinct session id per position without a test
// having to name them, and the same one on every run.
func scrcpyDefaultSCID(devpath string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(devpath))
	return fmt.Sprintf("%08x", h.Sum32())
}

// scrcpySerial invents a serial for a device the fixture had to add itself.
// Non-alphanumerics become '_' because a serial carrying a colon would be
// torn in half by the target parser and address a device that does not exist.
func scrcpySerial(devpath string) string {
	return "SCRCPY_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, devpath)
}
