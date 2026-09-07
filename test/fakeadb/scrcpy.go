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

// scrcpyVideoHeaderLen is the session header: codec id, width, height, each a
// big-endian uint32.
const scrcpyVideoHeaderLen = 12

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

	// Data is the payload, Annex-B H.264 in the real thing and arbitrary
	// bytes here: this fixture never decodes anything, and a test that
	// asserts on recognisable bytes is easier to read than one that ships a
	// real frame nobody can eyeball.
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
	VideoEOF bool

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
	mu           sync.Mutex
	cfg          ScrcpyConfig
	scid         string
	spawns       []string
	sockets      int
	controlBytes []byte
}

func (d *scrcpyDevice) config() ScrcpyConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// serveSpawn answers the app_process command line.
func (d *scrcpyDevice) serveSpawn(sess *StreamSession) error {
	cmd := strings.TrimPrefix(sess.Service, "shell,v2,raw:")

	d.mu.Lock()
	d.spawns = append(d.spawns, cmd)
	if scid, ok := scrcpyArg(cmd, "scid"); ok {
		d.scid = scid
	}
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

// serveSocket answers a connection to an abstract socket. The first goes to
// the video stream and the second to the control stream, which is the order
// scrcpy's own client connects them in.
func (d *scrcpyDevice) serveSocket(sess *StreamSession) error {
	name := strings.TrimPrefix(sess.Service, "localabstract:")

	d.mu.Lock()
	want := "scrcpy_" + d.scid
	n := d.sockets
	if name == want {
		d.sockets++
	}
	d.mu.Unlock()

	if name != want {
		return fmt.Errorf("fakeadb: %s has no abstract socket %q; this session published %q",
			sess.Devpath, name, want)
	}

	switch n {
	case 0:
		return d.serveVideo(sess)
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
func (d *scrcpyDevice) serveVideo(sess *StreamSession) error {
	cfg := d.config()

	if len(cfg.VideoPrefix) > 0 {
		if _, err := sess.Write(cfg.VideoPrefix); err != nil {
			return nil
		}
	}

	var hdr [scrcpyVideoHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[0:4], cfg.Codec)
	binary.BigEndian.PutUint32(hdr[4:8], cfg.Width)
	binary.BigEndian.PutUint32(hdr[8:12], cfg.Height)
	if _, err := sess.Write(hdr[:]); err != nil {
		return nil
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
		meta := p.PTS & scrcpyPTSMask
		if p.Config {
			meta |= scrcpyFlagConfig
		}
		if p.KeyFrame {
			meta |= scrcpyFlagKeyFrame
		}
		binary.BigEndian.PutUint64(ph[0:8], meta)
		binary.BigEndian.PutUint32(ph[8:12], uint32(len(p.Data)))
		// Header and payload go out as two writes on purpose. A client that
		// only works when both arrive in one read is a client that works
		// against this fixture and not against a phone.
		if _, err := sess.Write(ph[:]); err != nil {
			return nil
		}
		if _, err := sess.Write(p.Data); err != nil {
			return nil
		}
	}

	if cfg.VideoEOF {
		return nil
	}
	<-sess.Done
	return nil
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
