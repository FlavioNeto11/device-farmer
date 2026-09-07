package screen

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
)

// Device is everything a screen session may do to a handset.
//
// It is this narrow on purpose, following the precedent of runner.Conn: a
// session cannot renew a lease, release one, report health or read a device
// row, because it has no method that could. Whoever hands one of these in
// decides which device it reaches; a Session holds no devpath of its own to
// redirect, so a bug here cannot become a session on somebody else's phone.
type Device interface {
	// OpenService starts one device-side service and returns the duplex
	// stream. Closing the stream is enough to release it.
	OpenService(ctx context.Context, service string) (io.ReadWriteCloser, error)

	// Push writes r to remote on the device.
	Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error
}

// EnsureJar puts the server jar at remote, or reports why it could not.
//
// It is a callback rather than a method on Device for the reason
// artifacts.PushFunc is one: the decision "has this blob already been pushed to
// this device" lives in a ledger in PostgreSQL, and a package that reached into
// that ledger could not be tested without it. The caller owns the ledger; this
// package owns the path, which it computes from the jar's content id so that two
// jars cannot collide and the same jar is never pushed twice.
type EnsureJar func(ctx context.Context, remote string) error

// Options configure one session.
type Options struct {
	// DeviceID is the farm's uuid for the device. It is carried so that audit
	// and event rows can name it; nothing in this package reads it.
	DeviceID string

	// JarID is the content id of the server jar: the first scrcpy.JarIDLen hex
	// characters of its sha256. Build it with [JarIDFromSHA].
	JarID string

	// Version is the server's own version string, passed to it as an argument
	// so that a jar/command-line mismatch reports itself as a named error on
	// the handset's stderr instead of as a stream of bytes no decoder opens.
	Version string

	// MaxSize is the longest edge of the encoded video, in pixels.
	MaxSize int

	// EnsureJar is called once, before the server is started.
	EnsureJar EnsureJar

	// SpawnTimeout bounds the whole of steps one through four in the package
	// doc: the push, the shell service, and the two socket connects with their
	// retries. It does NOT bound the session. Zero takes DefaultSpawnTimeout.
	SpawnTimeout time.Duration
}

const (
	// DefaultSpawnTimeout bounds starting a session. Thirty seconds is chosen
	// against the slowest step, which is the push: a jar is a few hundred
	// kilobytes over a USB hub shared with every other device on it, and a hub
	// in the middle of somebody else's APK install is slow rather than broken.
	DefaultSpawnTimeout = 30 * time.Second

	// socketRetryInterval and socketRetryBudget bound the gap between the
	// server being started and its abstract socket existing.
	//
	// The protocol offers nothing better than polling here: the shell stream is
	// open, but the server logs its readiness as free text that this package
	// deliberately does not parse — a readiness check that scraped a log
	// message would break on a server release that reworded it, and break
	// silently, as a session that times out on a handset that is working.
	socketRetryInterval = 100 * time.Millisecond
	socketRetryBudget   = 10 * time.Second

	// serverLogCap bounds what is kept from the server's own output. It is a
	// diagnostic, not a log: the most recent few kilobytes are what an operator
	// needs to see "version mismatch" or "class not found", and keeping more
	// would mean a session's memory grew with the phone's chattiness.
	serverLogCap = 8 << 10
)

// Errors callers distinguish.
var (
	// ErrSessionExists means this device already has a live session. It is not
	// a transport failure and must not be retried against the same device.
	ErrSessionExists = errors.New("screen: this device already has a live session")

	// ErrTooManySessions means this process is at its configured cap.
	ErrTooManySessions = errors.New("screen: this process is at its session cap")

	// ErrClosed means the manager is shutting down.
	ErrClosed = errors.New("screen: the session manager is shut down")

	// ErrNoSession means the named session is not in this process. With more
	// than one api replica that is an ordinary outcome, not an error in the
	// caller: the session lives in exactly one of them.
	ErrNoSession = errors.New("screen: no such session in this process")
)

// SocketError is the failure that has to be distinguished by hand, because the
// two halves of it send an operator to different places: the server never
// started, or the server started and never published its socket.
type SocketError struct {
	Socket string
	Waited time.Duration
	// ServerLog is whatever the handset wrote on the shell stream while we
	// waited. It is the only evidence about which failure this was, and it is
	// carried rather than logged so the caller can put it in a refusal an
	// operator reads.
	ServerLog string
	Err       error
}

func (e *SocketError) Error() string {
	if e.ServerLog == "" {
		return fmt.Sprintf("screen: %s did not appear within %s and the server said nothing at all, "+
			"which is what a jar that failed to start looks like: %v", e.Socket, e.Waited, e.Err)
	}
	return fmt.Sprintf("screen: %s did not appear within %s; the server said: %s (%v)",
		e.Socket, e.Waited, e.ServerLog, e.Err)
}

func (e *SocketError) Unwrap() error { return e.Err }

// JarIDFromSHA turns a full sha256 digest in hex — which is how an artifact is
// named in farm.artifacts and in FARM_SCREEN_SERVER_SHA — into the content id
// that names the jar on the device.
//
// It exists so that no caller does the truncation by hand. A jar id that does
// not match the digest of the pushed bytes is a CLASSPATH pointing at a file
// that is not there, and the symptom is a server that exits silently.
func JarIDFromSHA(sha string) (string, error) {
	sum, err := hex.DecodeString(sha)
	if err != nil || len(sum) != 32 {
		return "", fmt.Errorf("screen: %q is not a sha256 in hex; it names no artifact", sha)
	}
	var fixed [32]byte
	copy(fixed[:], sum)
	return scrcpy.JarIDFromDigest(fixed), nil
}

// Manager holds the sessions in one process.
//
// It is keyed two ways because two different questions are asked of it: an
// input request arrives with a session id, and an Open arrives with a device.
// Keeping both maps in step under one mutex is what makes "one session per
// device" a property rather than a hope.
type Manager struct {
	mu       sync.Mutex
	byID     map[string]*Session
	byDevice map[string]*Session
	closed   bool

	max int
	log *slog.Logger
}

// NewManager returns a manager that admits at most max concurrent sessions.
func NewManager(max int, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if max < 1 {
		// A cap below one would make the feature permanently unavailable while
		// reporting itself configured, which is the worst of both. Callers get
		// their configuration validated in internal/config; this is the floor
		// for a caller that built a Manager by hand.
		max = 1
	}
	return &Manager{
		byID:     make(map[string]*Session),
		byDevice: make(map[string]*Session),
		max:      max,
		log:      log,
	}
}

// Len reports how many sessions are live. It is what a metric reads.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// Lookup finds a session by id.
func (m *Manager) Lookup(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok {
		return nil, ErrNoSession
	}
	return s, nil
}

// Close ends every session and refuses new ones.
//
// It is called from the api's shutdown path, and it must be: a long-lived
// response that nothing closes makes http.Server.Shutdown wait out the whole
// drain grace period on every deploy. Closing the sockets here is what lets the
// handler loops return.
//
// It ends no lease. A farm that restarts its api has not released a device.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	live := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		live = append(live, s)
	}
	m.mu.Unlock()

	for _, s := range live {
		s.Close()
	}
}

// Open starts a session on dev.
//
// On success the session is registered and the caller owns it: it must Close it
// when the response it is feeding ends. On failure nothing is registered and
// every socket this function opened is closed, including the ones that
// succeeded — a half-started session would hold an encoder on a phone with
// nothing in this process able to name it.
func (m *Manager) Open(ctx context.Context, dev Device, o Options) (*Session, error) {
	if o.SpawnTimeout <= 0 {
		o.SpawnTimeout = DefaultSpawnTimeout
	}

	spawn, err := buildSpawn(o)
	if err != nil {
		return nil, err
	}
	jarPath, err := spawn.JarPath()
	if err != nil {
		return nil, err
	}
	service, err := spawn.Service()
	if err != nil {
		return nil, err
	}
	socket, err := spawn.Socket()
	if err != nil {
		return nil, err
	}

	// Reserve the device BEFORE any I/O. Two concurrent Opens on one device
	// that both dialled first would both push, both start a server, and the
	// second would take the first's encoder away — and the loser would be a
	// session somebody was already watching.
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	if err := m.reserve(o.DeviceID, id); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			m.release(o.DeviceID, id)
		}
	}()

	// The spawn context bounds starting up. The SESSION context is separate and
	// is not derived from the request: a session outlives the HTTP request that
	// created it in the sense that matters here — its sockets must not be
	// cancelled by a ctx the caller is about to let go of.
	spawnCtx, cancelSpawn := context.WithTimeout(ctx, o.SpawnTimeout)
	defer cancelSpawn()

	if o.EnsureJar != nil {
		if err := o.EnsureJar(spawnCtx, jarPath); err != nil {
			return nil, fmt.Errorf("screen: the server jar did not reach the device: %w", err)
		}
	}

	sessCtx, cancelSession := context.WithCancel(context.WithoutCancel(ctx))
	s := &Session{
		ID:       id,
		DeviceID: o.DeviceID,
		Socket:   socket,
		Version:  o.Version,
		cancel:   cancelSession,
		done:     make(chan struct{}),
		log:      m.log.With("session", id, "device", o.DeviceID),
	}
	defer func() {
		if !ok {
			s.Close()
		}
	}()

	// Step 2: the server. Its stream stays open for the whole session and is
	// drained for the whole session — see drainServerLog.
	s.spawn, err = dev.OpenService(sessCtx, service)
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("screen: the server could not be started: %w", err)
	}
	go s.drainServerLog()

	// Step 3: video. The retry is the published-socket race, not a retry of a
	// refusal: a fence that says no says no the first time, and retrying it
	// would turn one refused connection into a burst of them in the proxy's
	// audit log.
	s.video, err = connectSocket(sessCtx, spawnCtx, dev, socket)
	if err != nil {
		cancelSession()
		return nil, &SocketError{Socket: socket, Waited: socketRetryBudget, ServerLog: s.ServerLog(), Err: err}
	}

	// The sixteen fixed-width bytes, captured verbatim so the caller can hand
	// the client the same bytes the phone sent rather than a re-encoding of
	// this package's idea of them. The third word of the session header has
	// bits this package does not interpret; re-serialising from Session would
	// drop them.
	var pre bytes.Buffer
	vr, err := scrcpy.NewReader(io.TeeReader(s.video, &pre))
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("screen: the video socket opened but is not a scrcpy stream: %w", err)
	}
	unit, err := vr.Next()
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("screen: the video stream ended before its session header: %w", err)
	}
	if unit.Kind != scrcpy.KindSession {
		cancelSession()
		return nil, fmt.Errorf("screen: the video stream opened with a frame rather than a session " +
			"header, so the server was started without the metadata this path needs; the " +
			"command line and the jar disagree")
	}
	sc, err := scrcpy.ScreenFromSession(unit.Session)
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("screen: the session header does not describe a frame input can be "+
			"placed on: %w", err)
	}
	s.Codec = vr.Codec()
	s.Width, s.Height = unit.Session.Width, unit.Session.Height
	s.screen = sc
	s.preamble = pre.Bytes()

	// Step 4: control. Second, always, because the server hands the first
	// connection to the encoder and the protocol has no field that would let
	// this be discovered rather than ordered.
	s.control, err = connectSocket(sessCtx, spawnCtx, dev, socket)
	if err != nil {
		cancelSession()
		return nil, &SocketError{Socket: socket, Waited: socketRetryBudget, ServerLog: s.ServerLog(), Err: err}
	}

	m.register(id, s)
	ok = true
	return s, nil
}

// buildSpawn assembles the command line.
//
// The argument list is the one this path needs and nothing else, and each entry
// is here for a reason worth reading:
//
//   - video_codec=h264 because WebCodecs decodes Annex-B H.264 in every browser
//     this dashboard supports, with no transcode anywhere. h265 would be
//     smaller and is not universally decodable in a browser.
//   - max_size bounds the encoder on the phone.
//   - audio=false because nothing in this path carries audio and a server
//     started with audio on spends a second socket and a second encoder on
//     bytes nobody reads.
//   - control=true because the point of this feature is a human's finger.
//   - tunnel_forward=true so the server LISTENS on the abstract socket and this
//     process connects in. The alternative has the phone dial out through a
//     reverse tunnel, which is a second thing for the fence proxy to admit and
//     a direction this path does not need.
//   - send_device_meta=false and send_dummy_byte=false because this package
//     reads the stream with internal/scrcpy, whose framing begins at the codec
//     id. A device-name block or a dummy byte in front of it would be read as a
//     codec id and rejected as an unknown codec.
//   - cleanup=true so a server whose sockets go away exits. It is what makes
//     teardown "close three sockets" rather than "close three sockets and then
//     go find a process", and going to find a process would mean a pkill that
//     killed somebody else's session on the same handset.
func buildSpawn(o Options) (scrcpy.Spawn, error) {
	scid, err := newSCID()
	if err != nil {
		return scrcpy.Spawn{}, err
	}
	if o.MaxSize <= 0 {
		return scrcpy.Spawn{}, fmt.Errorf("screen: MaxSize must be positive; it bounds the encoder "+
			"on the handset and %d would ask for a frame with no pixels", o.MaxSize)
	}
	return scrcpy.Spawn{
		JarID:   o.JarID,
		Version: o.Version,
		SCID:    scid,
		Args: []scrcpy.Arg{
			{Key: "video_codec", Value: "h264"},
			{Key: "max_size", Value: fmt.Sprint(o.MaxSize)},
			{Key: "audio", Value: "false"},
			{Key: "control", Value: "true"},
			{Key: "tunnel_forward", Value: "true"},
			{Key: "send_device_meta", Value: "false"},
			{Key: "send_dummy_byte", Value: "false"},
			{Key: "cleanup", Value: "true"},
		},
	}, nil
}

// connectSocket opens the abstract socket, retrying only the gap between the
// server being started and the socket existing.
//
// IT TAKES TWO CONTEXTS AND THE DIFFERENCE IS THE WHOLE FUNCTION. An adbwire
// stream's lifetime IS the context it was opened with — cancelling that ctx is
// how a caller unblocks a read on a silent device — so the socket must be opened
// with the SESSION's context. It was opened with the spawn context once, and the
// symptom was precise and baffling: the sixteen preamble bytes arrived, the
// response carried a correct session header and a correct frame size, and then
// nothing ever again, because Open's deferred cancel killed the stream the
// instant it returned.
//
// try bounds the RETRYING — how long to keep knocking while the server publishes
// its socket — and dying with it must not take the socket down with it.
func connectSocket(sessCtx, try context.Context, dev Device, socket string) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(socketRetryBudget)
	var last error
	for {
		st, err := dev.OpenService(sessCtx, socket)
		if err == nil {
			return st, nil
		}
		last = err
		if try.Err() != nil {
			// The caller's budget ran out, or the request was abandoned. Report
			// the context's reason rather than the last connect failure: "the
			// spawn timeout elapsed" is the true statement and the connect
			// error is a symptom of it.
			return nil, errors.Join(try.Err(), last)
		}
		if !time.Now().Before(deadline) {
			return nil, last
		}
		select {
		case <-time.After(socketRetryInterval):
		case <-try.Done():
			return nil, errors.Join(try.Err(), last)
		}
	}
}

func (m *Manager) reserve(deviceID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, busy := m.byDevice[deviceID]; busy {
		return ErrSessionExists
	}
	if len(m.byDevice) >= m.max {
		return ErrTooManySessions
	}
	// A placeholder under the device key, with no Session behind it yet. The
	// alternative — reserving after the sockets are up — leaves a window in
	// which two requests both push a jar and start two servers.
	m.byDevice[deviceID] = nil
	m.byID[id] = nil
	return nil
}

func (m *Manager) register(id string, s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[id] = s
	m.byDevice[s.DeviceID] = s
	s.forget = func() { m.release(s.DeviceID, id) }
}

func (m *Manager) release(deviceID, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, id)
	delete(m.byDevice, deviceID)
}

// Session is one live screen.
type Session struct {
	ID       string
	DeviceID string
	Socket   string
	Version  string

	Codec         scrcpy.CodecID
	Width, Height uint32

	screen   scrcpy.Screen
	preamble []byte

	spawn   io.ReadWriteCloser
	video   io.ReadWriteCloser
	control io.ReadWriteCloser

	// controlMu serialises writes to the control socket. The socket carries
	// length-delimited messages with no sequence number, so two goroutines
	// writing at once do not produce two messages in some order — they produce
	// one corrupt message and a decoder on the phone that is now out of step
	// with every message after it.
	controlMu sync.Mutex

	inputs atomic.Int64

	logMu     sync.Mutex
	serverLog []byte

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	forget    func()
	log       *slog.Logger
}

// Preamble is the sixteen bytes already read off the video socket: the codec id
// and the session header, verbatim.
//
// The caller writes these to its client before splicing [Session.Video], so
// that what arrives at a decoder is byte-for-byte what the phone produced.
func (s *Session) Preamble() []byte { return s.preamble }

// Video is the rest of the video stream, unparsed.
//
// Read it with a buffer of your own and do not look inside. Every length in
// there was chosen by a handset, and this process serves the lease renewals for
// the whole farm; see the package doc.
func (s *Session) Video() io.Reader { return s.video }

// Frame reports the encoded video's dimensions, which is the coordinate space
// input is placed in.
//
// These are NOT the device's pixels. They are what the encoder produced after
// max_size scaling and after whatever rotation the device was in when the
// session started. A caller that sends device pixels taps the wrong place on
// every scaled stream, which is all of them.
func (s *Session) Frame() (width, height uint32) { return s.Width, s.Height }

// Inputs reports how many input messages this session has delivered.
//
// It is a count and not a log, and that is a deliberate limit on what this
// feature can answer afterwards. A row per touch would be thousands of rows per
// minute, so "what did this person do with the phone" is NOT answerable. "Who
// held this device, from when to when, with input enabled, and how many
// messages they sent" is. That is the honest ceiling and the audit row says so
// in those terms rather than implying more.
func (s *Session) Inputs() int64 { return s.inputs.Load() }

// ServerLog returns what the handset has written on the shell stream, most
// recent bytes last, bounded.
func (s *Session) ServerLog() string {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	return string(bytes.TrimSpace(s.serverLog))
}

// Done is closed when the session has ended, whatever ended it.
//
// Nothing that selects on this may conclude anything about a lease. It says a
// socket is gone.
func (s *Session) Done() <-chan struct{} { return s.done }

// Send delivers input messages to the handset, in order, as one write.
//
// One write rather than one per event because the control socket is a byte
// stream: a batch that left this process as several writes could interleave
// with a concurrent batch at the kernel, and a half-written touch is a phone
// that thinks a finger is still down.
//
// A coordinate outside the frame is refused and NOTHING is sent — not the
// offending event and not the ones before it in the batch. Sending the prefix
// would leave a pointer down on the device with no matching up, which is a
// phone that behaves as though somebody is still holding it.
func (s *Session) Send(events ...Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	select {
	case <-s.done:
		return 0, ErrNoSession
	default:
	}

	buf := make([]byte, 0, 64*len(events))
	for i, e := range events {
		var err error
		buf, err = e.appendTo(buf, s.screen)
		if err != nil {
			return 0, fmt.Errorf("screen: event %d of %d was refused and none of the batch was "+
				"sent: %w", i+1, len(events), err)
		}
	}

	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if _, err := s.control.Write(buf); err != nil {
		// A dead control socket is a dead socket. It is not evidence about the
		// lease, the device's health or the job: see the package doc.
		return 0, fmt.Errorf("screen: the control socket did not take %d event(s): %w", len(events), err)
	}
	s.inputs.Add(int64(len(events)))
	return len(events), nil
}

// Close ends the session.
//
// It closes three sockets and runs no command on the device — see the package
// doc for why there is no kill step. It is safe to call from any goroutine and
// from more than one, which matters because the manager's shutdown path and the
// handler feeding the response both call it.
//
// IT ENDS NO LEASE. There is no code path from here to farm.leases, and adding
// one would be the idle timeout the founding invariant forbids.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.forget != nil {
			s.forget()
		}
		// Sockets first, so a read blocked on the video stream or a write
		// blocked on control returns before anything waits on it.
		var errs []error
		for _, c := range []io.Closer{s.video, s.control, s.spawn} {
			if c != nil {
				if err := c.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if s.cancel != nil {
			s.cancel()
		}
		close(s.done)
		if len(errs) > 0 {
			// Logged rather than returned: every caller of Close is in a defer
			// or a shutdown loop, and a socket that failed to close on the way
			// out is not something any of them can act on.
			s.log.Debug("screen session closed with socket errors",
				"err", errors.Join(errs...))
		}
	})
	return nil
}

// drainServerLog reads the shell stream for the whole session.
//
// THIS GOROUTINE IS NOT OPTIONAL AND IT IS NOT A DIAGNOSTIC LUXURY. The server
// on the handset writes to this stream. A reader that stops reading fills the
// socket buffer, the server's next write blocks, and a blocked server stops
// producing frames — so the video freezes, and nothing anywhere says why. This
// function exists so that the answer to "why did the picture stop" is never
// "because nobody was reading the log".
//
// It returns when the stream ends, which Close makes happen.
func (s *Session) drainServerLog() {
	r := adbwire.NewShellPacketReader(s.spawn)
	for {
		id, payload, err := r.Next()
		if err != nil {
			return
		}
		if len(payload) == 0 {
			continue
		}
		switch id {
		case adbwire.ShellStdout, adbwire.ShellStderr:
			s.note(payload)
		case adbwire.ShellExit:
			// The server exited. The session's video is over, but say so by
			// ending the session rather than by interpreting the exit code: a
			// non-zero exit from a server whose sockets we just closed is the
			// normal shutdown path, and treating it as a fault would file an
			// error against every clean teardown.
			s.note([]byte(fmt.Sprintf("\n[server exited, status frame %v]\n", payload)))
			return
		}
	}
}

// note appends to the bounded server log, keeping the most recent bytes.
func (s *Session) note(b []byte) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.serverLog = append(s.serverLog, b...)
	if len(s.serverLog) > serverLogCap {
		// Keep the TAIL. The interesting line in a server's output is the last
		// one, because that is the one it managed to write before it stopped.
		s.serverLog = append(s.serverLog[:0], s.serverLog[len(s.serverLog)-serverLogCap:]...)
	}
}

// newSessionID mints an opaque id.
//
// Opaque rather than the device id because the id travels in a response header
// and comes back in a request body, and an id that named the device would let a
// caller aim an input batch at a device by editing a string rather than by
// holding a session on it. Sixteen bytes of crypto/rand is unguessable; the
// session is still authorised by the operator role on every request, so this is
// defence in depth rather than the authorisation itself.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("screen: could not mint a session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// newSCID mints the abstract socket's name.
//
// Random rather than sequential because the name is a string in an abstract
// socket namespace shared by everything on the device: a predictable name is
// one another process can publish first, and the symptom would be this process
// connecting to somebody else's socket and calling it a screen.
func newSCID() (scrcpy.SCID, error) {
	var b [4]byte
	for range 8 {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("screen: could not mint a socket id: %w", err)
		}
		// Masked into range rather than reduced modulo, because MaxSCID is
		// 2^31-1 and masking the top bit off is exact. Zero is invalid and is
		// redrawn rather than nudged to one, so that every id is equally
		// likely and none is the value a bug produces.
		id := scrcpy.SCID(binary.BigEndian.Uint32(b[:]) & uint32(scrcpy.MaxSCID))
		if id.Valid() {
			return id, nil
		}
	}
	return 0, errors.New("screen: could not mint a valid socket id")
}

// Bind adapts an ADB client to one device.
//
// It is the production [Device], and it exists here rather than in the api so
// that the devpath is captured once, at the point where the device row was read,
// and is then unreachable: a Session holds no field naming a device, so no later
// bug in a handler can aim a session at a different phone.
func Bind(cli *adbwire.Client, devpath string) Device { return boundDevice{cli: cli, devpath: devpath} }

type boundDevice struct {
	cli     *adbwire.Client
	devpath string
}

func (b boundDevice) OpenService(ctx context.Context, service string) (io.ReadWriteCloser, error) {
	return b.cli.OpenService(ctx, b.devpath, service)
}

func (b boundDevice) Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error {
	return b.cli.Push(ctx, b.devpath, r, remote, mode)
}
