package fakeadb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is the scrcpy fixture's structural tests: the loop, which socket is
// which, and what a respawn is. It reads the wire the same way scrcpy_test.go
// does and for the same reason — by offset, with the framing restated in
// literals — so that a fixture which drifted from internal/scrcpy fails here
// rather than agreeing with itself.

// ---------------------------------------------------------------------
// A screen that does not stop after four seconds
// ---------------------------------------------------------------------

// TestALoopedScreenNeverSendsTimeBackwards is the whole of VideoLoop's claim,
// and the half that matters is the timestamps rather than the repetition.
//
// A clip that restarted its clock at the splice would hand a decoder a stream
// whose time jumps backwards every pass. Every frame of the next pass is then
// already overdue: a forgiving decoder drops them, a strict one calls the
// sequence corrupt and stops, and either way the symptom arrives seconds after
// the connection was made and so reads as a transport fault. The assertion is
// therefore not "the payloads repeat" — it is that the timestamps the client
// sees are ONE rising sequence, with the splice invisible in it.
//
// Falsify: drop `pass*period` from the packet header in serveVideo, so that each
// pass replays its own timestamps.
func TestALoopedScreenNeverSendsTimeBackwards(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:6-1.1"
		passes  = 3
	)
	packets := []ScrcpyPacket{
		{Config: true, Data: []byte("CFG")},
		{PTS: 0, KeyFrame: true, Data: []byte("IDR")},
		{PTS: 1000, Data: []byte("P-1")},
		{PTS: 2000, Data: []byte("P-2")},
	}
	s := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:   devpath,
		Packets:   packets,
		VideoLoop: true,
	}))
	scid, _ := s.ScrcpySCID(devpath)

	video := streamWire(t, s, devpath, "localabstract:scrcpy_"+scid)
	mustRead(t, video, wireVideoHeaderLen, "the session header")

	var last uint64
	for i := 0; i < passes*len(packets); i++ {
		pass, within := i/len(packets), i%len(packets)
		ph := mustRead(t, video, wirePacketHeaderLen,
			fmt.Sprintf("pass %d packet %d's header", pass, within))
		meta := binary.BigEndian.Uint64(ph[0:8])
		size := binary.BigEndian.Uint32(ph[8:12])
		body := mustRead(t, video, int(size), "the payload")

		want := packets[within]
		if !bytes.Equal(body, want.Data) {
			t.Fatalf("pass %d packet %d carried %q, want %q — a loop replays the same clip from "+
				"packet zero, config packet included, which is what a real server sends for a "+
				"decoder that joined late", pass, within, body, want.Data)
		}
		if got := meta&wireFlagConfig != 0; got != want.Config {
			t.Fatalf("pass %d packet %d: config=%t, want %t", pass, within, got, want.Config)
		}
		if got := meta&wireFlagKeyFrame != 0; got != want.KeyFrame {
			t.Fatalf("pass %d packet %d: keyframe=%t, want %t", pass, within, got, want.KeyFrame)
		}

		pts := meta & wirePTSMask
		if i > 0 && pts < last {
			t.Fatalf("pass %d packet %d is timestamped %dµs after a packet at %dµs — the stream's "+
				"clock went backwards at the loop point, and a decoder told that the next few "+
				"seconds already happened drops or refuses all of them",
				pass, within, pts, last)
		}
		// The splice itself: the first packet of a pass has to be strictly later
		// than the last packet of the one before, or two passes overlap in time
		// and one instant carries two different frames.
		if within == 0 && pass > 0 && pts <= last {
			t.Fatalf("pass %d opens at %dµs and pass %d ended at %dµs — one whole pass has to be "+
				"worth more than nothing, or the loop point is a stutter",
				pass, pts, pass-1, last)
		}
		last = pts
	}

	// And the loop is still something that ends: closing the viewer's socket
	// stops it, rather than leaving the handler writing into a dead connection
	// for the life of the server.
	if err := video.c.Close(); err != nil {
		t.Fatalf("closing the video socket: %v", err)
	}
	waitFor(t, func() bool { return s.Stats().Streams == 0 },
		func() string { return fmt.Sprintf("%d stream handlers still running", s.Stats().Streams) })
}

// TestBothVideoLoopAndVideoEOFIsRefusedWhereTheMistakeIs keeps a contradictory
// configuration from being resolved by a coin flip the caller cannot see.
//
// One of the two says the screen never ends and the other says it ends after the
// last packet. Both precedence rules are defensible, neither is guessable from
// the call site, and a fixture that picked one would hand a test a stream it did
// not describe — which then passes, or hangs, for a reason nowhere near the line
// that caused it. A panic's stack trace names that line.
//
// Falsify: replace the panic in ScrcpyFixture with a precedence rule.
func TestBothVideoLoopAndVideoEOFIsRefusedWhereTheMistakeIs(t *testing.T) {
	t.Parallel()

	s := Start(t)
	got := func() (r any) {
		defer func() { r = recover() }()
		s.Apply(ScrcpyFixture(ScrcpyConfig{Devpath: "usb:6-9.1", VideoLoop: true, VideoEOF: true}))
		return nil
	}()
	if got == nil {
		t.Fatalf("a fixture asking for both VideoLoop and VideoEOF was installed without complaint; " +
			"whichever of the two lost, the test that wrote it is now asserting against a stream it " +
			"never described")
	}
	msg := fmt.Sprint(got)
	if !strings.Contains(msg, "VideoLoop") || !strings.Contains(msg, "VideoEOF") {
		t.Fatalf("the refusal reads %q; it has to name both fields, because whoever reads this panic "+
			"is looking for which two lines of their own setup disagree", msg)
	}
}

// ---------------------------------------------------------------------
// Which socket is which
// ---------------------------------------------------------------------

// TestAControlSocketIsRefusedWhileTheVideoSocketIsUnsettled is the deterministic
// half of the role-assignment fix.
//
// scrcpy publishes ONE abstract socket and the client connects it twice, video
// first. The protocol carries no field distinguishing the two, so order is
// genuinely the only signal there is — and in this fake each connection arrives
// on its own goroutine, so "order" used to mean "whichever goroutine reached a
// counter first". Two sockets opened at the same instant therefore swapped roles
// about half the time, and a swap's symptom is a video socket that produces no
// bytes at all, because the control handler writes nothing.
//
// What the fix adds is a gate: a second role is not handed out until the first is
// live, where live means its video handler has written the session header. A
// client that connects in order cannot see the gate, because its second
// connection is not sent until the first one answered. This test holds the gate
// open on purpose — a megabytes-long VideoPrefix parks the video handler ahead of
// the header — and then knocks, which is the only way to observe the gate without
// racing for it.
//
// Falsify: delete the `racing` check in serveSocket. The second connection then
// becomes the control socket, is served in silence, and the request log records
// no refusal at all.
func TestAControlSocketIsRefusedWhileTheVideoSocketIsUnsettled(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:6-2.1"
		// A short prefix, purely so that reading it proves the video role WAS
		// claimed — the handler is running and writing. What holds the handler
		// before its header is the channel below, NOT an unread backlog: an
		// earlier version of this test used a megabyte of prefix against a
		// pinned receive buffer and lost the bet on Linux, inside the image
		// build, while passing here.
		prefix = 64
	)
	hold := make(chan struct{})
	s := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:     devpath,
		VideoPrefix: bytes.Repeat([]byte{0xab}, prefix),
		VideoHold:   hold,
	}))
	scid, _ := s.ScrcpySCID(devpath)
	service := "localabstract:scrcpy_" + scid
	dev := s.scrcpyFor(devpath)

	video := streamWire(t, s, devpath, service)
	mustRead(t, video, prefix, "the video prefix")
	if videoIsLive(dev) {
		t.Fatalf("the video socket settled while its header was still held; the gate this test " +
			"is about was already open and nothing below it proves anything")
	}

	second := streamWire(t, s, devpath, service)
	if got := readAllWithin(t, second, time.Second); len(got) != 0 {
		t.Fatalf("a second socket opened before the first had settled was served %d bytes — the two "+
			"roles are told apart by connection order alone, so serving this one means the fake "+
			"guessed which of them the client meant", len(got))
	}
	reply := lastReply(t, s, service)
	if !strings.HasPrefix(reply, "ERROR: ") || !strings.Contains(reply, "session header") {
		t.Fatalf("the overlapping connection was recorded as %q; it should say that the first socket "+
			"had not answered yet, because that is the client's bug and the client is who reads "+
			"this", reply)
	}

	// The refusal did not spend the control role. Release the header, and the
	// NEXT connection is the control socket it was always going to be —
	// otherwise one client's mistake would cost the session a socket nobody ever
	// used.
	hold <- struct{}{}
	mustRead(t, video, wireVideoHeaderLen, "the session header")
	waitFor(t, func() bool { return videoIsLive(dev) },
		func() string { return "the video socket never settled after its header went out" })

	control := streamWire(t, s, devpath, service)
	tap := touchMessage(0, 1, 2, 1080, 2400)
	if _, err := control.c.Write(tap); err != nil {
		t.Fatalf("writing to the socket that should be the control socket: %v", err)
	}
	if err := control.c.Close(); err != nil {
		t.Fatalf("closing the control socket: %v", err)
	}
	waitFor(t, func() bool { return bytes.Equal(s.ControlBytes(devpath), tap) },
		func() string {
			return fmt.Sprintf("the control socket recorded %x, want %x", s.ControlBytes(devpath), tap)
		})
}

// TestTwoSocketsRacingNeverBothGetTheScreen is the racing half, and it states the
// one thing that is true however the scheduler orders two simultaneous
// connections: exactly one of them is the video socket.
//
// WHICH one is the scheduler's choice and always will be. A real handset resolves
// it the same way — it serves whichever connection its listener is handed first —
// so a fake promising more would be promising something the thing it imitates
// does not do. What it must never do is serve TWO video streams off one published
// socket, or none: a client that got two would be decoding its own control
// channel, and a client that got none would be watching a black rectangle with no
// error recorded anywhere.
//
// Falsify: make serveSocket read the counter, release the lock, and increment it
// afterwards.
func TestTwoSocketsRacingNeverBothGetTheScreen(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:6-3.1"
		tries   = 8
	)
	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: devpath, VideoEOF: true}))
	scid, _ := s.ScrcpySCID(devpath)
	service := "localabstract:scrcpy_" + scid

	for try := 0; try < tries; try++ {
		// A fresh session per attempt, so every race starts from an unclaimed
		// pair rather than from whatever the last one left behind.
		spawn := streamWire(t, s, devpath, spawnService(scid))
		readShellPacket(t, spawn)

		// The transports are switched first, in this goroutine, so that the only
		// thing left to race is the one frame that names the socket. Opening the
		// whole connection from a goroutine would put two ADB round trips in
		// front of the race and make it one the scheduler almost always decides
		// the same way — and would put this package's Fatalf-on-failure helpers
		// on a goroutine that is not the test's.
		var ws [2]*wire
		for i := range ws {
			ws[i] = dial(t, s)
			ws[i].okBare("host:transport:" + devpath)
		}
		start := make(chan struct{})
		var sent sync.WaitGroup
		for _, w := range ws {
			sent.Add(1)
			go func(w *wire) {
				defer sent.Done()
				<-start
				w.send(service)
			}(w)
		}
		close(start)
		sent.Wait()

		screens := 0
		for i, w := range ws {
			if st := w.status(); st != "OKAY" {
				t.Fatalf("attempt %d: connection %d was answered %q; the fake answers every device "+
					"service with OKAY and refuses it afterwards, so a FAIL here is a different bug",
					try, i, st)
			}
			// A deadline, because "served nothing" and "is the control socket"
			// look identical from out here without one: the control handler reads
			// forever and writes nothing, so it closes only when the client does.
			out := readAllWithin(t, w, 500*time.Millisecond)
			if len(out) == 0 {
				continue
			}
			screens++
			if len(out) < wireVideoHeaderLen {
				t.Fatalf("attempt %d: a socket was served %d bytes, which is neither a session "+
					"header nor nothing — a half-written header is a stream no client can start "+
					"from", try, len(out))
			}
			if codec := binary.BigEndian.Uint32(out[0:4]); codec != wireCodecH264 {
				t.Fatalf("attempt %d: the served socket opened with codec %#08x, want h264",
					try, codec)
			}
		}
		if screens != 1 {
			t.Fatalf("attempt %d: %d of two simultaneous connections were served the screen, want "+
				"exactly one. Two means the fake answered one published socket with two video "+
				"streams, and the client is decoding its own control channel; none means it "+
				"answered with nothing, and the viewer is looking at a black rectangle with no "+
				"error recorded anywhere", try, screens)
		}
		if err := spawn.c.Close(); err != nil {
			t.Fatalf("closing the spawn: %v", err)
		}
	}
}

// TestASecondSpawnGetsACleanPairOfSockets is the respawn bug, which came with a
// misleading error message attached.
//
// The socket counter belonged to the device rather than to the session, so it
// only ever went up. The first session took counts zero and one; the SECOND — a
// reconnecting viewer, a fence bump, a lease handover, none of them rare — found
// it at two and had every connection refused by the arm that exists to catch a
// client reconnecting to a listener that is gone. The refusal blamed the client
// for what the fixture had done, which is the worst kind of fixture bug: the
// error message points away from the error.
//
// Falsify: remove the `d.sockets = 0` reset in serveSpawn.
func TestASecondSpawnGetsACleanPairOfSockets(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-4.1"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:  devpath,
		Packets:  []ScrcpyPacket{{PTS: 7, KeyFrame: true, Data: []byte("FRAME")}},
		VideoEOF: true,
	}))
	service := "localabstract:scrcpy_" + testSCID

	for session := 1; session <= 2; session++ {
		spawn := streamWire(t, s, devpath, spawnService(testSCID))
		readShellPacket(t, spawn)

		video := streamWire(t, s, devpath, service)
		frames, err := io.ReadAll(video.br)
		if err != nil {
			t.Fatalf("session %d: reading the video socket: %v", session, err)
		}
		want := wireVideoHeaderLen + wirePacketHeaderLen + len("FRAME")
		if len(frames) != want {
			t.Fatalf("session %d served %d bytes on its video socket, want %d. A session is a server "+
				"process and a server process publishes its own pair of sockets, so a device whose "+
				"counter only goes upwards refuses the second viewer and then blames the viewer "+
				"(%q)", session, len(frames), want, lastReply(t, s, service))
		}

		control := streamWire(t, s, devpath, service)
		msg := keycodeMessage(0, uint32(session))
		if _, err := control.c.Write(msg); err != nil {
			t.Fatalf("session %d: writing to the control socket: %v", session, err)
		}
		if err := control.c.Close(); err != nil {
			t.Fatalf("session %d: closing the control socket: %v", session, err)
		}
		waitFor(t, func() bool { return len(s.ControlWrites(devpath)) == session },
			func() string {
				return fmt.Sprintf("session %d: ControlWrites = %x", session, s.ControlWrites(devpath))
			})

		if err := spawn.c.Close(); err != nil {
			t.Fatalf("session %d: closing the spawn: %v", session, err)
		}
		waitFor(t, func() bool { return s.Stats().Streams == 0 },
			func() string {
				return fmt.Sprintf("session %d: %d handlers still running", session, s.Stats().Streams)
			})
	}
}

// TestAVideoSocketThatDiesBeforeItsHeaderGivesTheRoleBack is the trap the gate
// would otherwise be.
//
// The video role is spent the moment serveSocket hands it out, and only a
// successful header write can mark it live. A client that goes away in between —
// a closed tab, a lease that moved, a proxy that hiccuped, all ordinary — leaves
// the session with one role handed out and no live video, which is exactly the
// state serveSocket treats as a race. Every connection after that is refused,
// including the client's own honest retry of the video socket, and including the
// retry after that: the session is wedged until a respawn, and the refusal blames
// the client for opening two sockets at once while it has none open at all.
//
// A DIRECT CALL AND NOT A SOCKET, which is a deliberate exception to this file's
// habit of reading the wire, and it was arrived at the hard way.
//
// The state to arrange is "the video handler returned without ever writing its
// session header", and over TCP that is not a state a test can ARRANGE — it can
// only hope for it. Holding the handler behind a megabyte of unread VideoPrefix
// and then resetting the connection looks deterministic and is not: whether the
// handler's write fails depends on whether the kernel swallowed the megabytes
// still owed before the reset landed, and on this loopback it did, about one run
// in ten, even with the receive window pinned at eight kilobytes. The test then
// failed on an assertion about something that had never happened — which is
// worse than useless, because the failure describes a fixture bug that is not
// there.
//
// A writer that returns an error is not a race. What is lost by dropping the
// socket is the transport, and the transport is not what this claims anything
// about: the wire-level gate is asserted next door in
// TestAControlSocketIsRefusedWhileTheVideoSocketIsUnsettled, where the premise —
// "the handler has not written its header YET" — is one a test can hold, because
// it does not require the write to fail.
//
// Falsify: remove the two videoGaveUp calls from serveVideo.
func TestAVideoSocketThatDiesBeforeItsHeaderGivesTheRoleBack(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-8.1"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:  devpath,
		Packets:  []ScrcpyPacket{{PTS: 1, KeyFrame: true, Data: []byte("FRAME")}},
		VideoEOF: true,
	}))
	dev := s.scrcpyFor(devpath)
	scid, _ := s.ScrcpySCID(devpath)
	service := "localabstract:scrcpy_" + scid

	// The viewer that went away: every write it is handed fails, which is what
	// the fixture sees when a tab closes between the OKAY and the header.
	if err := dev.serveSocket(deadSession(devpath, service)); err != nil {
		t.Fatalf("a video socket whose client had already gone was reported as a fixture failure: "+
			"%v — the far side leaving is the ordinary way a screen stream ends", err)
	}
	if got := scrcpySockets(dev); got != 0 {
		t.Fatalf("the session still has %d role(s) handed out after its video socket died before "+
			"writing a header, want 0. Nothing will ever mark that role live, so every connection "+
			"from here — including the client's own retry of the VIDEO socket — is refused as a "+
			"race, and only a respawn recovers", got)
	}

	// And the retry gets the screen, which is the consequence somebody would
	// actually notice.
	var served bytes.Buffer
	if err := dev.serveSocket(recordingSession(devpath, service, &served)); err != nil {
		t.Fatalf("the retry was refused: %v", err)
	}
	got := served.Bytes()
	if len(got) < wireVideoHeaderLen {
		t.Fatalf("the retry was served %d bytes, which is not a session header — it was given some "+
			"role other than the video socket the session never handed out", len(got))
	}
	if codec := binary.BigEndian.Uint32(got[0:4]); codec != wireCodecH264 {
		t.Fatalf("the retry's first four bytes are %#08x, want the h264 codec id", codec)
	}
}

// TestARespawnsGateIsNotOpenedByThePreviousSessionsVideoSocket is the fix to the
// fix, and the reason the session generation exists at all.
//
// A spawn resets the gate, but it cannot reach into a handler that is already
// running. The previous session's video socket may still be parked inside its
// header write with no idea it has been superseded — nothing tells it — and when
// that write finally lands it marks a video socket live. Without a generation
// stamp it marks the NEW session's, which nobody has connected. The new session
// then begins with its gate already open, and two of its sockets opened at once
// are assigned by scheduler order again: the swap the gate was added to prevent,
// reintroduced by the repair of a different bug.
//
// Falsify: drop the `d.session == gen` check in videoWentLive.
func TestARespawnsGateIsNotOpenedByThePreviousSessionsVideoSocket(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:6-8.2"
		prefix  = 64
		frame   = "FRAME"
	)
	// Held rather than buried behind an unread backlog, for the reason on
	// ScrcpyConfig.VideoHold: a receive buffer is not a synchronisation
	// primitive, and betting on one failed on Linux while passing here.
	hold := make(chan struct{})
	s := Start(t, ScrcpyFixture(ScrcpyConfig{
		Devpath:     devpath,
		VideoPrefix: bytes.Repeat([]byte{0xab}, prefix),
		VideoHold:   hold,
		Packets:     []ScrcpyPacket{{PTS: 1, KeyFrame: true, Data: []byte(frame)}},
	}))
	service := "localabstract:scrcpy_" + testSCID
	dev := s.scrcpyFor(devpath)

	first := streamWire(t, s, devpath, spawnService(testSCID))
	readShellPacket(t, first)
	stale := streamWire(t, s, devpath, service)
	mustRead(t, stale, prefix, "the first session's video prefix")

	// The respawn. From here the old handler belongs to a session that is over,
	// and it does not know that.
	second := streamWire(t, s, devpath, spawnService(testSCID))
	readShellPacket(t, second)

	// Let the superseded handler finish its header and get all the way past the
	// point where it would mark a video socket live. Reading its first PACKET is
	// what proves it got there — the header write returning is not observable
	// from out here, and a sleep would be a guess.
	hold <- struct{}{}
	mustRead(t, stale, wireVideoHeaderLen, "the first session's session header")
	mustRead(t, stale, wirePacketHeaderLen, "the first session's packet header")
	mustRead(t, stale, len(frame), "the first session's packet payload")

	if videoIsLive(dev) {
		t.Fatalf("the new session's video socket is marked live and nobody has connected one — a " +
			"handler from the session before it wrote the flag on its way past. The new session's " +
			"gate is open from the instant it began, so its two sockets are back to being assigned " +
			"by whichever goroutine reaches the counter first")
	}

	// And the consequence, on the wire: the new session's gate really does still
	// hold, so a socket opened while its video socket is unsettled is refused.
	fresh := streamWire(t, s, devpath, service)
	mustRead(t, fresh, prefix, "the new session's video prefix")
	overlapping := streamWire(t, s, devpath, service)
	if got := readAllWithin(t, overlapping, time.Second); len(got) != 0 {
		t.Fatalf("the new session served %d bytes to a socket opened before its video socket had "+
			"answered", len(got))
	}
	if reply := lastReply(t, s, service); !strings.Contains(reply, "session header") {
		t.Fatalf("the new session recorded %q for an overlapping connection, want the refusal that "+
			"names the ordering", reply)
	}
}

// ---------------------------------------------------------------------
// Four claims that used to survive being deleted
// ---------------------------------------------------------------------

// TestTheFlagBitsSurviveATimestampThatWouldCollideWithThem pins the packet
// header's bit split, which nothing pinned before: every test in this package
// used timestamps small enough that a mask one bit wide in either direction
// produced the same answer, and a config-only and a keyframe-only packet whose
// flags were exchanged still each had exactly one flag set.
//
// So the timestamps here are chosen to collide. A PTS with every bit of the
// field set catches a mask that is too narrow or too wide; asserting the whole
// eight bytes against a literal catches an exchange of the two flags.
//
// Falsify: swap scrcpyFlagConfig and scrcpyFlagKeyFrame, or write the mask as
// scrcpyFlagConfig-1 or as scrcpyFlagKeyFrame-1.
func TestTheFlagBitsSurviveATimestampThatWouldCollideWithThem(t *testing.T) {
	t.Parallel()

	// The timestamp field filled to its brim, so that a mask off by one bit
	// either clears a bit of a real timestamp or leaves a flag bit standing in
	// one.
	const fullSessionPTS = uint64(1)<<61 - 1 // all 61 bits

	cases := []struct {
		name    string
		packets []ScrcpyPacket
		want    []uint64 // the whole top eight bytes, as a literal
	}{
		{
			name: "flags at bits 62 and 61, under a session header",
			packets: []ScrcpyPacket{
				{PTS: fullSessionPTS, Config: true},
				{PTS: fullSessionPTS, KeyFrame: true},
				{PTS: ^uint64(0)},
			},
			want: []uint64{
				// The session bit stays CLEAR on every packet: a config packet
				// that set it would be read as a geometry, and the reader would
				// take the payload length for a video height.
				0x5fffffffffffffff,
				0x3fffffffffffffff,
				0x1fffffffffffffff,
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			devpath := fmt.Sprintf("usb:6-5.%d", i+1)
			for j := range tc.packets {
				tc.packets[j].Data = []byte{byte(j)}
			}
			s := Start(t, ScrcpyFixture(ScrcpyConfig{
				Devpath:  devpath,
				Packets:  tc.packets,
				VideoEOF: true,
			}))
			scid, _ := s.ScrcpySCID(devpath)
			video := streamWire(t, s, devpath, "localabstract:scrcpy_"+scid)

			id := mustRead(t, video, 4, "the codec id")
			if got := binary.BigEndian.Uint32(id); got != wireCodecH264 {
				t.Fatalf("codec id = %#08x, want h264", got)
			}
			sh := mustRead(t, video, wirePacketHeaderLen, "the session header")
			if got := binary.BigEndian.Uint64(sh[0:8]); got>>63 != 1 {
				t.Fatalf("the session header's top eight bytes are %#016x; the top bit is what "+
					"says this is a geometry and not a frame, and a reader that does not see "+
					"it resynchronises inside a picture", got)
			}

			for j, want := range tc.want {
				ph := mustRead(t, video, wirePacketHeaderLen, fmt.Sprintf("packet %d's header", j))
				if got := binary.BigEndian.Uint64(ph[0:8]); got != want {
					t.Fatalf("packet %d's flags and timestamp went out as %#016x, want %#016x. "+
						"Every bit of this word is load-bearing: the two flags tell a decoder what "+
						"it may start from and what configures it, and the timestamp under them "+
						"tells it when. A mask one bit out turns the top of a timestamp into a "+
						"flag, or a flag into 2^61 microseconds",
						j, got, want)
				}
				mustRead(t, video, int(binary.BigEndian.Uint32(ph[8:12])), "the payload")
			}
		})
	}
}

// TestTheSpawnedServerOutlivesItsFirstOutput pins the park at the end of
// serveSpawn, which nothing pinned: every test read the banner and moved on, so a
// handler that announced itself and then returned would have passed all of them.
//
// The park is what makes the fixture a PROCESS rather than a message. A real
// server jar runs until something kills it, and a client watches its stdout for
// the lifetime of the session; a fixture whose spawn returned after one line
// would let a client that never noticed the death pass, and would turn "the test
// forgot to close the spawn" from a hung Close into a silently truncated session.
//
// Falsify: replace `<-sess.Done; return nil` at the end of serveSpawn with
// `return nil`. The connection then closes behind the banner and the read below
// comes back io.EOF instead of timing out.
func TestTheSpawnedServerOutlivesItsFirstOutput(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-6.1"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: devpath, VideoEOF: true}))

	spawn := streamWire(t, s, devpath, spawnService(testSCID))
	id, banner := readShellPacket(t, spawn)
	if id != shellPacketStdout || banner == "" {
		t.Fatalf("the server announced itself as id=%d payload=%q", id, banner)
	}

	// A deadline rather than a sleep-and-look, because what is being asserted is
	// that NOTHING happens: the read has to be the thing that waits. A timeout
	// here is the pass, because a timeout means the socket is still open with a
	// process on the other end of it.
	if err := spawn.c.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	var one [1]byte
	_, err := spawn.br.Read(one[:])
	var ne net.Error
	if !asNetError(err, &ne) || !ne.Timeout() {
		t.Fatalf("half a second after its banner the spawn stream returned %v, want a read timeout. "+
			"The jar is a process: it runs until it is killed, and a fixture whose spawn ends after "+
			"one line of output lets a client that never noticed its server die pass", err)
	}
	if got := s.Stats().Streams; got < 1 {
		t.Fatalf("%d stream handlers are running; the spawned server is supposed to be one of them", got)
	}
}

// TestTheInventedSerialCarriesNoPunctuation pins scrcpySerial's sanitisation,
// which nothing pinned: no test had ever looked at the serial the fixture invents
// for a device it had to add itself.
//
// A devpath is full of characters a serial may not contain. The colon is the one
// that matters, because adb's own target syntax is colon-delimited and `adb -s`
// on a serial carrying one addresses a device that does not exist — so the
// failure is not a malformed string, it is a command that quietly goes somewhere
// else.
//
// Falsify: return "SCRCPY_" + devpath from scrcpySerial.
func TestTheInventedSerialCarriesNoPunctuation(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"usb:5-1.1":   "SCRCPY_usb_5_1_1",
		"usb:1-2.3/4": "SCRCPY_usb_1_2_3_4",
		"3-1.4":       "SCRCPY_3_1_4",
	}
	for devpath, want := range cases {
		if got := scrcpySerial(devpath); got != want {
			t.Fatalf("the serial invented for %s is %q, want %q — a serial carrying the devpath's "+
				"punctuation is not a serial: adb's target syntax is colon-delimited, so `-s %s` "+
				"addresses something that is not this device and says nothing about why",
				devpath, got, want, got)
		}
	}

	// And the fixture uses it, so the property holds of a device a test can
	// actually reach rather than only of a helper.
	const devpath = "usb:6-7.1"
	s := Start(t, ScrcpyFixture(ScrcpyConfig{Devpath: devpath, VideoEOF: true}))
	d, ok := s.Device(devpath)
	if !ok {
		t.Fatalf("the fixture did not install %s", devpath)
	}
	if d.Serial != "SCRCPY_usb_6_7_1" {
		t.Fatalf("the installed device's serial is %q; the fixture invented it and did not "+
			"sanitise it", d.Serial)
	}
	if strings.ContainsAny(d.Serial, ":/-. ") {
		t.Fatalf("the installed device's serial %q contains a character a target string cannot "+
			"carry", d.Serial)
	}
}

// ---------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------

// errGone is what a write to a viewer that has gone away returns. A named value
// rather than an anonymous one so that a failure printing it says what happened
// rather than "write error".
var errGone = errors.New("fakeadb test: the viewer went away")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errGone }

// deadSession is a StreamSession whose client is already gone: every write
// fails, immediately and without ambiguity.
//
// This is the one thing a real socket cannot be made to be on demand — see the
// note on TestAVideoSocketThatDiesBeforeItsHeaderGivesTheRoleBack — and it is the
// premise of the only assertion in this file that needs it.
func deadSession(devpath, service string) *StreamSession {
	return &StreamSession{
		Devpath: devpath,
		Service: service,
		Reader:  bytes.NewReader(nil),
		Writer:  failingWriter{},
		Done:    make(chan struct{}),
	}
}

// recordingSession is a StreamSession that keeps what the fixture writes to it,
// so a direct call can be read back the same way a socket would have been.
func recordingSession(devpath, service string, into *bytes.Buffer) *StreamSession {
	return &StreamSession{
		Devpath: devpath,
		Service: service,
		Reader:  bytes.NewReader(nil),
		Writer:  into,
		Done:    make(chan struct{}),
	}
}

// scrcpySockets is how many socket roles the device's current session has handed
// out. Reaching into the fixture's own bookkeeping, because that bookkeeping IS
// the claim being made — a role that was spent and not returned is not visible on
// the wire until the next client is refused for it.
func scrcpySockets(d *scrcpyDevice) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sockets
}

// videoIsLive reports whether a scripted device's video socket has got its
// session header out, which is what releases the control socket.
//
// A test reaching into the fixture's own state rather than inferring it from the
// wire, because what it needs to know is a PRECONDITION of the test rather than
// a claim of it. An inference that was wrong would leave a test that passes
// without exercising the thing it names.
func videoIsLive(d *scrcpyDevice) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.videoLive
}

// asNetError is errors.As for a net.Error, spelled out so the deadline
// assertions above read as one condition.
func asNetError(err error, out *net.Error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		*out = ne
		return true
	}
	return false
}

// readAllWithin reads a connection to its end, or until budget runs out.
//
// A DEADLINE RATHER THAN io.ReadAll, because one of the two questions this file
// asks — "was this connection served nothing at all" — has no answer without
// one. A connection that became the CONTROL socket looks exactly like a
// connection that was refused: both produce no bytes. The difference is that the
// control handler reads forever and writes nothing, so it closes only when the
// client does, and io.ReadAll on it never returns. Without the deadline the test
// that asked would hang until the package timeout instead of failing with a
// message, which is the difference between a ten-second red and a ten-minute
// mystery — and it is exactly what happens when one of these assertions is
// falsified, so the deadline is load-bearing for the falsification and not only
// for the pass.
//
// The read error is deliberately discarded. What the callers assert on is the
// BYTES and not the manner of death, for the reason
// TestScrcpySocketNobodyPublishedIsRefused sets out at length: whether a refused
// socket ends as a reset or as a clean EOF is TCP timing.
func readAllWithin(tb testing.TB, w *wire, budget time.Duration) []byte {
	tb.Helper()
	if err := w.c.SetReadDeadline(time.Now().Add(budget)); err != nil {
		tb.Fatalf("setting a read deadline: %v", err)
	}
	out, _ := io.ReadAll(w.br)
	// Back to the whole-exchange guard dial installed, so a later read on this
	// wire is still bounded by something.
	if err := w.c.SetReadDeadline(time.Now().Add(wireDeadline)); err != nil {
		tb.Fatalf("restoring the read deadline: %v", err)
	}
	return out
}
