package ctl

// `ctl device screen` is the live screen without a browser.
//
// The dashboard decodes this same stream with WebCodecs, which makes the
// browser the only witness that the screen and input routes work. That is a
// bad place for the only witness to live: when an operator reports a black
// canvas there is no way to tell a server that never started from a frame a
// browser refused to decode, and nobody debugging at 3am wants the answer to
// depend on which Chrome shipped that week. This command is the second
// witness. It asks for the same bytes over the same route with the same
// credential, counts what arrives, and — with --tap — sends input back through
// the same session. A run of it that prints packets and key frames is evidence
// the API works; a run that prints nothing names which half did not.
//
// # It is a parser's client, not a second parser
//
// The framing is decoded by internal/scrcpy, never here. That package exists
// because one of the numbers on this wire is a u32 that says how much memory to
// allocate next, and it checks that number against scrcpy.MaxPacket BEFORE the
// allocation. A second copy of this loop in ctl would be a second place for
// that check to be forgotten, and the process it would be forgotten in is the
// operator's own terminal — a handset with a confused encoder should not be
// able to size a make() in it. So this file reads Units and counts them and
// owns no arithmetic on a length.
//
// # A dead stream says nothing about a lease
//
// THIS IS THE COMMENT THE PACKAGE DOC ASKS FOR, AND THIS COMMAND IS WHERE
// SOMEBODY WOULD BE TEMPTED. A screen stream is a socket carrying video off a
// phone. When it drops — a proxy idle timeout, a host agent restart, a cable —
// what ended is the socket. The lease is a row in Postgres with a fence and a
// deadline, and nothing about it follows from a read() that failed here: the
// holder still holds the device, the job is still running, the session may even
// still be open on the api replica. So a stream failure is reported as a
// transport failure and as nothing else. This command never calls revoke, never
// suggests the lease is gone, and never reports "the device went away" on the
// evidence of its own socket. ctl.go states the rule for the package; the
// reason it is restated here is that a video pipe dying FEELS like a device
// dying, and the next person to touch this file will feel it too.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
)

// screenMediaType is what GET /devices/{id}/screen answers with. It is sent as
// Accept as well as checked on the way back, because the one failure this
// catches is the interesting one: a 200 carrying a proxy's login page decodes
// as an unknown codec id, and "codec 0x3c21444f" is a far worse sentence to
// hand an operator than "something between here and the API answered with
// text/html".
const screenMediaType = "application/vnd.device-farmer.screen"

// The two response headers that carry the session's identity. The session id is
// not optional decoration: POST .../input will not accept an event without it,
// so a stream that arrives without this header can be watched and cannot be
// touched.
const (
	headerScreenSession = "X-Screen-Session"
	headerScreenDevice  = "X-Screen-Device"
)

// screenDefaultSeconds bounds a bare invocation.
//
// The stream has no end of its own — a phone keeps encoding until somebody
// stops asking — so a command with no default limit would hang the first
// operator who ran it to see whether the route works. Five seconds is long
// enough to contain a key frame at any frame rate a handset encodes at, and
// short enough that `ctl device screen X` is a question rather than a
// commitment.
const screenDefaultSeconds = 5

// screenProgressEvery is how often the running count is reprinted. It is a
// packet count rather than a clock so that the interval says something about
// the stream: lines arriving steadily mean frames are arriving steadily, and
// lines that stop while the command is still running are the encoder stalling
// rather than the terminal being quiet.
const screenProgressEvery = 30

// screenTapTimeout bounds one --tap. See sendTaps for why it is short: the
// video socket is not being read while a tap is in flight.
const screenTapTimeout = 10 * time.Second

// screenWriteBuffer is the size of the buffer over the output file. A packet is
// tens of kilobytes and they arrive at frame rate; writing each one straight
// through would be one syscall per frame for no reason.
const screenWriteBuffer = 256 << 10

// ---------------------------------------------------------------------------
// Flags
// ---------------------------------------------------------------------------

// screenPoint is one --tap, in VIDEO coordinates.
//
// Video, not device pixels, and the distinction is the whole reason this type
// has a comment. The coordinates scrcpy's input protocol accepts are in the
// space of the encoded frame — after --max-size scaling, cropping and the
// rotation filter — so a tap typed from a 1080x2400 phone's screenshot lands in
// the wrong place on a 1024-pixel stream, which is to say it lands somewhere
// plausible and wrong. The numbers to use are the ones this command prints as
// the video size.
type screenPoint struct {
	X int
	Y int
}

// tapList collects a repeated --tap.
type tapList []screenPoint

func (t *tapList) String() string {
	parts := make([]string, 0, len(*t))
	for _, p := range *t {
		parts = append(parts, fmt.Sprintf("%d,%d", p.X, p.Y))
	}
	return strings.Join(parts, " ")
}

// Set parses one X,Y pair.
//
// A negative coordinate is refused here rather than sent, because it is the
// only malformed tap this side can recognise with certainty: every frame starts
// at the origin, whatever its size, so -1 is a typo in every coordinate space
// there is. Everything else is the server's to judge against the frame it is
// actually encoding.
func (t *tapList) Set(v string) error {
	x, y, ok := strings.Cut(strings.TrimSpace(v), ",")
	if !ok {
		return fmt.Errorf("--tap takes X,Y in video coordinates, not %q", v)
	}
	px, errX := strconv.Atoi(strings.TrimSpace(x))
	py, errY := strconv.Atoi(strings.TrimSpace(y))
	if errX != nil || errY != nil {
		return fmt.Errorf("--tap takes two integers, not %q", v)
	}
	if px < 0 || py < 0 {
		return fmt.Errorf("--tap %q is outside every frame: a video coordinate is never negative", v)
	}
	*t = append(*t, screenPoint{X: px, Y: py})
	return nil
}

// ---------------------------------------------------------------------------
// The input route's request and reply
// ---------------------------------------------------------------------------

// screenInputEvent is one event in the body of POST /devices/{id}/input.
//
// Only the touch shape is modelled. The contract also carries key and scroll
// events, and this command synthesises neither: --tap is the flag that proves
// the input path end to end, and fields nothing here ever sets would be a
// schema this package has to keep in step with the server for no caller's
// benefit. The key and scroll shapes are the dashboard's to send.
type screenInputEvent struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	X      int    `json:"x"`
	Y      int    `json:"y"`

	// Pressure is 1 on the way down and 0 on the way up, which is what a real
	// finger reports and what scrcpy's own client sends. It is written
	// explicitly in both events rather than omitted when zero, so the document
	// on the wire says what was meant instead of relying on a decoder's
	// default.
	Pressure float64 `json:"pressure"`

	// PointerID names which finger. One tap is pointer 0 throughout: a down
	// and an up with different ids are two half-gestures, and the handset
	// would be left believing a finger is still on the glass.
	PointerID int `json:"pointer_id"`
}

// screenInputRequest is the body of POST /devices/{id}/input. The session id is
// the one from X-Screen-Session: input is addressed to a session, not to a
// device, which is what makes "this replica does not hold that session" a
// 409 an operator can act on rather than a tap that silently went nowhere.
type screenInputRequest struct {
	Session string             `json:"session"`
	Events  []screenInputEvent `json:"events"`
}

// screenInputResponse is its reply.
type screenInputResponse struct {
	Accepted int `json:"accepted"`
}

// screenTapResult is one tap's outcome, for the -o json summary.
type screenTapResult struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Accepted int    `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// The command
// ---------------------------------------------------------------------------

// screenRun is one invocation's settled intent, after flags are parsed and the
// operator has approved whatever needed approving.
type screenRun struct {
	// path is /api/v1/devices/{id}, already escaped. The stream hangs off
	// path+"/screen" and input off path+"/input", so both are addressed to the
	// same device by the same spelling the operator typed.
	path  string
	query url.Values

	outPath string
	seconds int
	frames  int
	taps    []screenPoint
}

func cmdDeviceScreen(ctx context.Context, s *session, args []string) error {
	fs := newFlags("device screen", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	force := fs.Bool("force", false, "open a screen even though the device holds a live lease")
	out := fs.String("out", "", "write the Annex-B elementary stream here; - is stdout, omitted records nothing")
	seconds := fs.Int("seconds", screenDefaultSeconds, "stop after this many seconds; 0 runs until interrupted")
	frames := fs.Int("frames", 0, "stop after this many packets; 0 is no limit. --frames and --seconds are both "+
		"limits, not alternatives: whichever is reached first stops the stream")
	maxSize := fs.Int("max-size", 0, "longest edge of the encoded video in pixels; 0 leaves the server's default")
	var taps tapList
	fs.Var(&taps, "tap", "X,Y in VIDEO coordinates, tapped once the first key frame arrives; repeatable")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl device screen <id|farm_uid> [--out f] [--seconds n] [--frames n] [--tap x,y]")
	}
	if *seconds < 0 || *frames < 0 {
		return usageErrf("--seconds and --frames count forwards; 0 means no limit, and a negative one means nothing")
	}
	if *maxSize < 0 {
		return usageErrf("--max-size is a pixel count, so %d is not one", *maxSize)
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if *out == "-" && e.format == FormatJSON {
		// Both want stdout, and a summary object interleaved with H.264 is
		// neither parseable nor playable. Naming the conflict beats emitting
		// the mixture and letting the operator discover it in ffplay.
		return usageErrf("--out - and -o json both write to stdout; write the video to a file, " +
			"or drop -o json and read the summary on stderr")
	}

	// The preflight is a read, so the operator sees the rack position and the
	// current holder BEFORE a server starts on the hardware. It is also what
	// decides whether this invocation is destructive at all: a screen on an
	// idle device disturbs nobody, and the same screen on a leased device is
	// exec's blast radius exactly — somebody's run, watched and touched
	// without their knowing.
	target, _, err := fetch[deviceResponse](ctx, e.client, apiPrefix+"/devices/"+url.PathEscape(rest[0]), nil)
	if err != nil {
		return err
	}
	d := target.Device
	path := apiPrefix + "/devices/" + url.PathEscape(rest[0])

	if d.Lease != nil || *force {
		if err := e.requireReason("device screen on a leased device"); err != nil {
			return err
		}
	}
	if d.Lease != nil {
		f := &Fields{}
		f.Add("rack slot", rackSlotOf(d.RackSlot))
		f.Add("farm uid", d.FarmUID)
		f.Add("host", dash(d.HostID))
		f.Add("adb devpath", dash(d.ADBDevpath))
		f.Gap()
		f.Add("live lease", dash(d.Lease.ID))
		f.Add("job", dash(d.Lease.JobID))
		f.Add("tenant", dash(d.Lease.TenantID))
		f.Add("holder", dash(d.Lease.Holder))
		f.Addf("protected", "%s", yesNo(d.Lease.Protected))
		if len(taps) > 0 {
			f.Addf("taps", "%d — input will be injected into that job's session", len(taps))
		}
		headline := fmt.Sprintf("Device %s holds a live lease. Without --force the API will refuse this.",
			rackSlotOf(d.RackSlot))
		if *force {
			headline = fmt.Sprintf("About to open a live screen on %s WHILE SOMEBODY'S JOB IS USING IT.\n"+
				"Starting the screen server runs a process on the handset, and --tap injects real input into\n"+
				"that run. The holder gets no signal that either happened. Nothing here ends their lease.",
				rackSlotOf(d.RackSlot))
		}
		if err := e.confirm(headline, f); err != nil {
			return err
		}
	}

	q := url.Values{}
	if *maxSize > 0 {
		q.Set("max_size", strconv.Itoa(*maxSize))
	}
	if *force {
		q.Set("force", "true")
	}
	setIf(q, "reason", e.reason)

	return e.runScreen(ctx, screenRun{
		path:    path,
		query:   q,
		outPath: *out,
		seconds: *seconds,
		frames:  *frames,
		taps:    taps,
	})
}

// ---------------------------------------------------------------------------
// The stream
// ---------------------------------------------------------------------------

// screenSink is where --out points: a buffered writer over a file or over
// stdout, and nil when --out was not given at all.
//
// It exists as a type rather than three local variables because of the
// truncate: the file is opened before the request and emptied only when the
// first payload is about to be written, and a rule with two halves that far
// apart in time needs one place to live. Every method tolerates a nil receiver,
// so the read loop does not branch on whether anything is being recorded.
type screenSink struct {
	w     *bufio.Writer
	file  *os.File
	name  string
	clean bool
}

// write appends one payload, emptying the file first if this is the first one.
func (s *screenSink) write(p []byte) error {
	if s == nil || s.w == nil {
		return nil
	}
	if s.file != nil && !s.clean {
		if err := s.file.Truncate(0); err != nil {
			return fmt.Errorf("empty %s before recording into it: %w", s.name, err)
		}
		s.clean = true
	}
	if _, err := s.w.Write(p); err != nil {
		return fmt.Errorf("write %s: %w", s.name, err)
	}
	return nil
}

// close flushes and closes. stdout is flushed and never closed: it is the
// process's, not this command's.
func (s *screenSink) close() error {
	if s == nil || s.w == nil {
		return nil
	}
	err := s.w.Flush()
	if err != nil {
		err = fmt.Errorf("write %s: %w", s.name, err)
	}
	if s.file != nil {
		if cerr := s.file.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", s.name, cerr)
		}
	}
	return err
}

// screenTally is what arrived, which is the whole result of this command.
type screenTally struct {
	packets   int
	keyFrames int
	configs   int
	headers   int
	bytes     int64

	width  uint32
	height uint32
	sized  bool

	firstPTS uint64
	lastPTS  uint64
	timed    int
}

// note records a packet. PTS is read only from displayable packets: scrcpy
// stamps a config packet with bits its own client discards, so folding one into
// the span would report a timeline the frames do not have.
func (t *screenTally) note(p scrcpy.Packet) {
	t.packets++
	t.bytes += int64(len(p.Payload))
	if p.KeyFrame {
		t.keyFrames++
	}
	if p.Config {
		t.configs++
		return
	}
	if t.timed == 0 {
		t.firstPTS = p.PTS
	}
	t.lastPTS = p.PTS
	t.timed++
}

// ptsSpan renders how much device time the displayable packets cover.
//
// It is not the wall time and the two are worth comparing: a five-second run
// that carries one second of PTS is a handset encoding at a fifth of the rate
// it was asked for, which is a finding about the phone rather than about the
// stream. millis() is deliberately not used — it rounds a sub-second span to
// whole seconds, and a sub-second span is exactly the case worth seeing.
func (t *screenTally) ptsSpan() string {
	if t.timed < 2 {
		return "—"
	}
	if t.lastPTS < t.firstPTS {
		// The device's clock went backwards between two packets. Reporting a
		// wrapped unsigned subtraction as a duration would print seventy
		// thousand years.
		return "out of order"
	}
	return fmt.Sprintf("%.3fs", float64(t.lastPTS-t.firstPTS)/1e6)
}

// screenSummary is the -o json rendering, on stdout.
//
// It is assembled here rather than passed through from the server because this
// command's result is not a server document: it is a measurement of a stream,
// and the only machine that can count what arrived is this one.
type screenSummary struct {
	Session        string            `json:"session"`
	DeviceID       string            `json:"device_id"`
	Codec          string            `json:"codec"`
	Width          uint32            `json:"width"`
	Height         uint32            `json:"height"`
	Packets        int               `json:"packets"`
	KeyFrames      int               `json:"key_frames"`
	ConfigPackets  int               `json:"config_packets"`
	SessionHeaders int               `json:"session_headers"`
	Bytes          int64             `json:"bytes"`
	WallMS         int64             `json:"wall_ms"`
	PTSSpanUS      uint64            `json:"pts_span_us"`
	Out            string            `json:"out,omitempty"`
	Taps           []screenTapResult `json:"taps,omitempty"`
}

// runScreen opens the stream, records it, taps it and reports it.
func (e *env) runScreen(ctx context.Context, r screenRun) (err error) {
	// The sink is opened BEFORE the request, and truncated only when the first
	// packet arrives. Both halves of that matter. A path that cannot be written
	// is an operator's typo, and discovering it after the stream is open means a
	// handset has started a screen server and an encoder for bytes that were
	// never going to land anywhere — so the open happens first. But a refusal
	// after that open must not cost the operator the recording they already
	// have: `ctl device screen X --out today.h264` answered 409
	// session_already_open would otherwise leave a zero-byte file where this
	// morning's capture was, which is a data loss the command was not asked
	// for. So the truncate waits for a byte that is actually going to land.
	var (
		sink    *screenSink
		outName string
	)
	switch {
	case r.outPath == "":
	case r.outPath == "-":
		sink = &screenSink{w: bufio.NewWriterSize(e.session.out, screenWriteBuffer), name: "stdout"}
		outName = "stdout"
	default:
		f, ferr := os.OpenFile(r.outPath, os.O_WRONLY|os.O_CREATE, 0o666)
		if ferr != nil {
			return fmt.Errorf("--out %s: %w", r.outPath, ferr)
		}
		sink = &screenSink{w: bufio.NewWriterSize(f, screenWriteBuffer), file: f, name: r.outPath}
		outName = r.outPath
	}
	defer func() {
		// A recording that was cut short is still a recording, so the buffer is
		// flushed on every path out of here — including the failures. A lost
		// flush would turn "the stream dropped after four seconds" into a file
		// missing its last quarter megabyte, with nothing on screen saying so.
		if cerr := sink.close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	// Our own clock gets its own context so that --seconds stops the stream
	// without looking like a failure, and so that the taps — which run on the
	// caller's context — are never cancelled by the video's deadline. An input
	// event that is abandoned half-sent is a finger the handset may believe is
	// still down.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopped, openLate atomic.Bool

	// The opening round trip is bounded by --timeout and NOT by --seconds.
	// --seconds is how much video to record, and the route has work to do before
	// the first byte of it exists: it pushes the server jar, starts a process on
	// the handset and opens two admissions. Letting a five-second recording
	// limit cancel that would fail a healthy device on a loaded farm — and,
	// worse, would cancel the request just as a 409 was arriving, turning the
	// refusal an operator has to read into "context canceled".
	openTimer := time.AfterFunc(e.client.timeout, func() {
		openLate.Store(true)
		cancel()
	})
	resp, err := openScreen(streamCtx, e.client, r.path+"/screen", r.query)
	openTimer.Stop()
	if err != nil {
		if openLate.Load() {
			// The raw error is "context canceled", which reads as a bug in ctl
			// rather than as the deadline it is. And what happened on the far
			// side is genuinely unknown: a session may have opened, in which
			// case it is the server's to close. Nothing here released anything.
			return fmt.Errorf("the screen route did not answer within --timeout %s: %w. Whether a "+
				"session opened on the far side is UNKNOWN — if one did it is the server's to close, "+
				"nothing was retried, and nothing here released anything", e.client.timeout, err)
		}
		// A refusal is an answer: the envelope's message is already in the
		// error, and screenRefusal adds what its detail says about what to do.
		return e.screenRefusal(err)
	}
	defer resp.Body.Close()
	if r.seconds > 0 {
		timer := time.AfterFunc(time.Duration(r.seconds)*time.Second, func() {
			stopped.Store(true)
			cancel()
		})
		defer timer.Stop()
	}
	// The clock starts at the response, not at the request: wall time is read
	// beside the packet count and the PTS span, and folding a slow start-up into
	// it would report a healthy encoder as a slow one.
	started := time.Now()
	if err := checkScreenMediaType(resp); err != nil {
		return err
	}

	sessionID := strings.TrimSpace(resp.Header.Get(headerScreenSession))
	deviceID := strings.TrimSpace(resp.Header.Get(headerScreenDevice))

	// maxResponseBody is deliberately not applied to this body. A screen stream
	// is unbounded by design — it ends when somebody stops watching — so a cap
	// on the total would be a cap on how long an operator may record. The bound
	// that matters is per packet, it belongs to internal/scrcpy, and it is
	// checked there before anything is allocated.
	reader, err := scrcpy.NewReader(resp.Body)
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return fmt.Errorf("the screen stream carried no codec id at all: the route answered 200 and "+
				"then wrote nothing. The session opened (%s) and the server on the handset never spoke; "+
				"nothing here released anything", firstNonEmpty(sessionID, "no session id"))
		case errors.Is(err, io.ErrUnexpectedEOF):
			return fmt.Errorf("the screen stream ended inside its four-byte codec id: %w. This is a "+
				"transport failure, and the socket is all that ended — no lease, job or session state "+
				"follows from it", err)
		}
		return fmt.Errorf("the screen stream did not announce a codec this build can read: %w", err)
	}

	head := &Fields{}
	head.Add("session", firstNonEmpty(sessionID, "— none sent; --tap cannot address this stream"))
	head.Add("device", firstNonEmpty(deviceID, "—"))
	head.Add("codec", reader.Codec().String())
	if err := head.Render(e.err); err != nil {
		return err
	}
	if len(r.taps) > 0 && sessionID == "" {
		e.warnf("the response carried no %s header, so the %s cannot be addressed to this session and "+
			"will not be sent", headerScreenSession, plural(len(r.taps), "tap", "taps"))
	}

	var (
		tally     screenTally
		tapped    bool
		tapRes    []screenTapResult
		streamErr error
	)
loop:
	for {
		unit, nerr := reader.Next()
		if nerr != nil {
			streamErr = nerr
			break
		}
		switch unit.Kind {
		case scrcpy.KindSession:
			tally.headers++
			// A session header arrives again on every rotation, and the size it
			// carries is the space --tap coordinates are measured in. Printing
			// each one is how an operator watching a phone turn knows that the
			// numbers they were about to type just changed.
			if tally.sized && (unit.Session.Width != tally.width || unit.Session.Height != tally.height) {
				e.warnf("the video size changed to %dx%d — a rotation, or a resize we asked for; "+
					"--tap coordinates are measured against this frame from here on",
					unit.Session.Width, unit.Session.Height)
			} else if !tally.sized {
				e.warnf("video: %dx%d", unit.Session.Width, unit.Session.Height)
			}
			tally.width, tally.height, tally.sized = unit.Session.Width, unit.Session.Height, true

		case scrcpy.KindPacket:
			tally.note(unit.Packet)
			// Payloads only, framing stripped. The config packet is written with
			// the rest because it carries the SPS and PPS: a file that skipped it
			// is an H.264 stream no decoder can start, which looks exactly like a
			// recording of a broken encoder.
			if werr := sink.write(unit.Packet.Payload); werr != nil {
				return werr
			}
			if tally.packets == 1 || tally.packets%screenProgressEvery == 0 {
				e.warnf("%d packet(s), %d key frame(s), %s", tally.packets, tally.keyFrames,
					bytesCell(tally.bytes))
			}
			if !tapped && unit.Packet.KeyFrame && len(r.taps) > 0 && sessionID != "" {
				// Taps wait for a key frame because before one there is nothing
				// decodable on the wire and therefore no evidence that the
				// screen an operator is aiming at is the screen the phone is
				// showing.
				tapped = true
				tapRes = e.sendTaps(ctx, r.path, sessionID, &tally, r.taps)
			}
			if r.frames > 0 && tally.packets >= r.frames {
				break loop
			}
		}
	}

	// A tap that never left is a finding, not a silence. The gate above opens
	// only on a key frame from a stream that named a session, so a window with
	// no key frame in it — or a response with no session header — would
	// otherwise end with an empty tap list, exit 0, and no line anywhere saying
	// that the input route was never called. This command exists to be the
	// witness for that route; a green tick having sent nothing is the one
	// outcome it must not produce.
	if len(r.taps) > 0 && !tapped {
		why := "no key frame arrived before the stream stopped, so there was never a frame to aim at"
		if sessionID == "" {
			why = "the stream carried no " + headerScreenSession + " header, so no input could be addressed to it"
		}
		e.warnf("%s NOT SENT: %s", plural(len(r.taps), "tap was", "taps were"), why)
		for _, p := range r.taps {
			tapRes = append(tapRes, screenTapResult{X: p.X, Y: p.Y, Error: why})
		}
	}

	wall := time.Since(started)
	e.warnf("%d packet(s), %d key frame(s), %d config packet(s), %s of payload in %s; PTS span %s",
		tally.packets, tally.keyFrames, tally.configs, bytesCell(tally.bytes),
		millis(wall.Milliseconds()), tally.ptsSpan())

	if outName != "" && tally.packets > 0 {
		// The one thing an operator will otherwise get wrong. An elementary
		// stream has no container and no frame rate in it, so a player given
		// the file with no hints either refuses it or plays it at whatever it
		// guesses. On stdout there is no file to name, so the hint is the pipe
		// it actually was: printing `ffplay -f h264 stdout` would send somebody
		// looking for a file that does not exist.
		if sink != nil && sink.file == nil {
			e.warnf("wrote a raw Annex-B elementary stream to stdout: no container, no frame rate, no " +
				"duration. Pipe it (`... --out - | ffplay -f h264 -i -`) or redirect it to a file and " +
				"play that with -f h264.")
		} else {
			e.warnf("wrote %s — a raw Annex-B elementary stream: no container, no frame rate, no duration.\n"+
				"  view:    ffplay -f h264 %s\n"+
				"  convert: ffmpeg -f h264 -r 12 -i %s out.mp4   (-r is yours to choose; the file does not say)",
				outName, outName, outName)
		}
	}

	if e.format == FormatJSON {
		var span uint64
		if tally.timed >= 2 && tally.lastPTS >= tally.firstPTS {
			span = tally.lastPTS - tally.firstPTS
		}
		if jerr := e.out.JSON(screenSummary{
			Session: sessionID, DeviceID: deviceID, Codec: reader.Codec().String(),
			Width: tally.width, Height: tally.height,
			Packets: tally.packets, KeyFrames: tally.keyFrames, ConfigPackets: tally.configs,
			SessionHeaders: tally.headers, Bytes: tally.bytes,
			WallMS: wall.Milliseconds(), PTSSpanUS: span,
			Out: outName, Taps: tapRes,
		}); jerr != nil {
			return jerr
		}
	}

	return screenOutcome(tally, tapRes, streamErr, stopped.Load() || ctx.Err() != nil, sessionID)
}

// screenOutcome turns a finished stream into this package's exit code.
//
// Three readings have to stay apart, because a script keys on them: video
// arrived (0), the remote refused (3, which never reaches here — roundTrip
// returns the refusal before a byte is read), and the transport failed (1). The
// fourth is the one a naive loop gets wrong: a stream with no packets in it.
// Nothing failed, nothing was refused, and nothing works either — the screen
// server started and produced no video — so exit 0 there would hand a CI job a
// green tick for a black screen. It is checked FIRST, before any of the ways the
// stream might have ended, because every one of those ways can end a stream that
// carried nothing and a clean ending is the most misleading of them.
//
// ours covers both the stop this command asked for — --seconds, --frames — and
// the operator's Ctrl-C. Neither is a failure: the read was cancelled because
// somebody wanted it cancelled, and with --seconds 0 an interrupt is the only
// documented way to stop. And neither says anything whatsoever about the lease.
func screenOutcome(t screenTally, taps []screenTapResult, streamErr error, ours bool, sessionID string) error {
	where := firstNonEmpty(sessionID, "no session id")
	switch {
	case t.packets == 0 && errors.Is(streamErr, io.EOF):
		return fmt.Errorf("the screen stream ended cleanly after zero packets: the server on the "+
			"handset started, the session opened (%s) and the encoder produced no video. That is a "+
			"device or an encoder to look at, not a transport to retry — and nothing here released "+
			"anything", where)
	case t.packets == 0 && ours:
		return fmt.Errorf("no packets arrived before this command's limit elapsed: the session opened "+
			"(%s), the stream stayed open and the encoder produced no video. Nothing here released "+
			"anything", where)
	case t.packets == 0 && streamErr != nil:
		return fmt.Errorf("transport failure before the first packet: %w. The session opened (%s) and "+
			"the socket is all that ended — no lease, job or session state follows from it, nothing was "+
			"retried, and nothing here released anything", streamErr, where)
	case t.packets == 0:
		return fmt.Errorf("no packets arrived and nothing ended the stream: the session opened (%s) and "+
			"produced no video", where)
	case ours:
		// The stop we asked for. reader.Next failed because the context was
		// cancelled under it, which is what cancelling it looks like.
	case errors.Is(streamErr, io.EOF):
		// The server closed a stream it had been feeding. Ordinary: the session
		// was closed on the other side, or the device stopped encoding.
	case isFramingFault(streamErr):
		// A length above scrcpy.MaxPacket, or a zero one. Calling this a
		// transport failure would send an operator to look at the network, and
		// the network delivered every byte it was given: what is wrong is the
		// bytes. internal/scrcpy says such a stream is not recoverable, so
		// re-running is the only move — but on the encoder's behaviour, not on
		// the socket's.
		return fmt.Errorf("the screen stream stopped making sense %s in: %w. Nothing was allocated "+
			"for it and nothing here released anything; this is the framing or the encoder, not the "+
			"socket", plural(t.packets, "packet", "packets"), streamErr)
	case streamErr != nil:
		return fmt.Errorf("transport failure %s into the screen stream: %w. The socket is all that "+
			"ended: no lease, job or session state follows from a dropped stream, nothing was retried, "+
			"and nothing here released anything", plural(t.packets, "packet", "packets"), streamErr)
	}

	failed := 0
	for _, r := range taps {
		if r.Error != "" {
			failed++
		}
	}
	if failed > 0 {
		// Partial, not failed: the video half of this route worked and is on
		// stdout or in a file, and what did not work is named above. Exit 1
		// would send a script to re-run the whole recording to recover from one
		// refused tap.
		return fmt.Errorf("%w: %d of %d tap(s) were not accepted; the video arrived and no lease was affected",
			ErrPartial, failed, len(taps))
	}
	return nil
}

// isFramingFault reports whether the stream broke its own rules rather than
// its socket. internal/scrcpy raises exactly two of these, and both mean the
// same thing: a length arrived that cannot be believed, and it was refused
// before it sized anything.
func isFramingFault(err error) bool {
	var big *scrcpy.PacketTooLargeError
	return errors.As(err, &big) || errors.Is(err, scrcpy.ErrEmptyPacket)
}

// openScreen issues the GET and hands back the response with its body open.
//
// It goes through the Client's own newRequest and roundTrip rather than a
// second http.Client, so the stream carries this invocation's bearer token and
// user agent and a non-2xx comes back already decoded as a *RemoteError — which
// is what makes a 409 on this route exit 3 like every other refusal in the
// package. There is no per-request deadline: a recording is bounded by
// --seconds and by the operator, not by the client's ordinary timeout.
func openScreen(ctx context.Context, c *Client, p string, q url.Values) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, p, q, nil, "")
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", screenMediaType)
	return c.roundTrip(req, p)
}

// checkScreenMediaType refuses a 200 that is not a screen stream.
func checkScreenMediaType(resp *http.Response) error {
	raw := resp.Header.Get("Content-Type")
	if raw == "" {
		// Nothing to check against. The codec id is the next thing read and it
		// is a far stricter test than a header, so this is not worth failing on.
		return nil
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil || mt != screenMediaType {
		return fmt.Errorf("the screen route answered %d with Content-Type %q, not %s: something between "+
			"here and the API answered instead of it — a proxy error page, a redirect to a login form, "+
			"or the wrong port", resp.StatusCode, clip(raw, 120), screenMediaType)
	}
	return nil
}

// screenRefusal prints what a refusal's detail says, then returns the error
// unchanged so its status still decides the exit code.
//
// The envelope's message is already rendered by *RemoteError. What it does not
// render is the detail, and on this route the detail is the entire actionable
// content: which env vars are missing, whose job is in the way, or that the
// screen an operator is trying to open is already open somewhere else.
func (e *env) screenRefusal(err error) error {
	e.explainRefusal(err)
	return err
}

// explainRefusal warns about whatever the envelope's detail explains, and says
// nothing at all about an error that is not the API's.
func (e *env) explainRefusal(err error) {
	var remote *RemoteError
	if !errors.As(err, &remote) {
		return
	}
	var detail struct {
		Reason    string `json:"reason"`
		Fault     string `json:"fault"`
		Remedy    string `json:"remedy"`
		LeaseID   string `json:"lease_id"`
		JobID     string `json:"job_id"`
		TenantID  string `json:"tenant_id"`
		Holder    string `json:"holder"`
		Protected bool   `json:"protected"`
	}
	if len(remote.Detail) > 0 {
		// A detail that does not fit this shape is not an error: the server is
		// free to add keys, and ignoring what does not decode is better than
		// refusing to explain the half that did.
		_ = json.Unmarshal(remote.Detail, &detail)
	}

	switch {
	case detail.Reason == "session_already_open":
		e.warnf("a screen session is already open on this device. One encoder, one session — scrcpy is " +
			"built that way — so somebody else (or a dashboard tab of yours) is watching it. Nothing was " +
			"disturbed and no lease was touched.")
	case detail.Reason == "session_not_here":
		e.warnf("this api replica does not hold that session, so the input went nowhere. The session " +
			"belongs to the replica that opened the stream; re-open the screen and send taps through the " +
			"stream you get back.")
	case detail.Fault == "configuration":
		e.warnf("this farm cannot serve a screen at all: %s", firstNonEmpty(detail.Remedy,
			"the server reported a configuration fault and named no remedy"))
		e.warnf("that is a farm to configure, not a device to look at — no handset was contacted.")
	case remote.Status == http.StatusConflict && detail.LeaseID != "":
		e.warnf("the device holds a live lease: lease %s, job %s, tenant %s, holder %s, protected %s.",
			dashOr(detail.LeaseID), dashOr(detail.JobID), dashOr(detail.TenantID), dashOr(detail.Holder),
			yesNo(detail.Protected))
		e.warnf("--force --reason ... opens it anyway, which puts a screen server on a phone somebody's " +
			"job is using. Their lease is untouched either way.")
	case remote.Code == "adb_error":
		e.warnf("the screen server could not be started on the handset. The API reached the device and " +
			"the device refused or failed; `ctl device <id>` shows its adb state and health, and nothing " +
			"here released anything.")
	case remote.Status == http.StatusForbidden:
		e.warnf("a screen is operator-only. A framebuffer shows whatever is on the glass, including " +
			"another tenant's app and their data, so there is no tenant-scoped version of this route to " +
			"ask for.")
	}
}

// dashOr renders an absent string from a JSON detail as visibly absent. dash()
// takes a pointer because a device field distinguishes null from empty; a
// detail key that did not decode gives neither, and an empty cell here would
// read as "this lease has no holder".
func dashOr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// sendTaps posts each --tap as a down and an up at the same point.
//
// One POST per tap, two events in it: that is what "immediately followed" has
// to mean on a route where each request is a round trip, and it is also what
// makes the reported outcome per tap rather than per batch. A tap whose down
// was accepted and whose up was not would leave a finger on the glass, so the
// pair travels together or not at all.
func (e *env) sendTaps(ctx context.Context, path, sessionID string, t *screenTally, taps []screenPoint) []screenTapResult {
	out := make([]screenTapResult, 0, len(taps))
	for i, p := range taps {
		res := screenTapResult{X: p.X, Y: p.Y}
		label := fmt.Sprintf("tap %d/%d at %d,%d", i+1, len(taps), p.X, p.Y)

		if t.sized && (p.X >= int(t.width) || p.Y >= int(t.height)) {
			// The server would answer 400 for this, and the reason it would
			// give is not the reason an operator needs. The frame size is right
			// here on this side, and the mistake it almost always is — device
			// pixels typed into a video coordinate — is worth naming instead of
			// round-tripping.
			res.Error = fmt.Sprintf("outside the %dx%d video frame", t.width, t.height)
			e.warnf("%s: NOT SENT — %s. --tap is in VIDEO coordinates, after --max-size scaling and "+
				"rotation, not device pixels.", label, res.Error)
			out = append(out, res)
			continue
		}

		// Each tap gets its own short deadline, under whatever --timeout allows.
		// This call is made from inside the read loop — synchronously, so that a
		// down and its up cannot be separated by a stream event — which means
		// the video socket goes undrained while it runs. A tap is one small POST
		// against a session this replica already holds; one that has not been
		// acknowledged in ten seconds is not going to be, and waiting the
		// client's full thirty for it would stall the recording this command was
		// run to make.
		tapCtx, cancel := context.WithTimeout(ctx, screenTapTimeout)
		reply, _, err := send[screenInputResponse](tapCtx, e.client, path+"/input", screenInputRequest{
			Session: sessionID,
			Events: []screenInputEvent{
				{Type: "touch", Action: "down", X: p.X, Y: p.Y, Pressure: 1, PointerID: 0},
				{Type: "touch", Action: "up", X: p.X, Y: p.Y, Pressure: 0, PointerID: 0},
			},
		})
		cancel()
		if err != nil {
			res.Error = err.Error()
			e.warnf("%s: %v", label, err)
			e.explainRefusal(err)
			out = append(out, res)
			continue
		}
		res.Accepted = reply.Accepted
		e.warnf("%s: accepted %d event(s)", label, reply.Accepted)
		out = append(out, res)
	}
	return out
}
