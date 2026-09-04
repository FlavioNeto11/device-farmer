package adbwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults.
const (
	// DefaultEndpoint is where an adb server listens on the machine the
	// devices are plugged into.
	DefaultEndpoint = "127.0.0.1:5037"

	// DefaultCallTimeout bounds a one-shot host call that arrives with no
	// deadline of its own. Streaming calls are not bounded by it.
	DefaultCallTimeout = 10 * time.Second

	// DefaultMaxOutput caps captured command output.
	DefaultMaxOutput = 8 << 20
)

// TargetPrefix selects the host-service prefix used to address a physical
// position.
//
// Both forms carry a devpath, never a serial. They differ only in which of
// the ADB server's two parsers resolves it, and server builds differ in which
// one routes devpath matching, so the choice is configuration rather than
// semantics.
type TargetPrefix string

const (
	// PrefixUSB builds "host-usb:<devpath>:<cmd>".
	PrefixUSB TargetPrefix = "host-usb"

	// PrefixTargetName builds "host-serial:<devpath>:<cmd>". The devpath
	// occupies the target-name field, which the server's transport matcher
	// compares against a transport's devpath as well as its serial. Despite
	// the prefix's name this is still position addressing: no serial is
	// ever sent.
	PrefixTargetName TargetPrefix = "host-serial"
)

// Option configures a Client.
type Option func(*Client)

// WithDialer supplies the dialer used for every connection, so a deployment
// can pin a source address or a keep-alive policy.
func WithDialer(d *net.Dialer) Option {
	return func(c *Client) {
		if d != nil {
			c.dialer = d
		}
	}
}

// WithCallTimeout bounds one-shot host calls that carry no deadline. A
// non-positive value leaves such calls bounded only by their context.
func WithCallTimeout(d time.Duration) Option {
	return func(c *Client) { c.callTimeout = d }
}

// WithTargetPrefix selects the host-service prefix for position addressing.
func WithTargetPrefix(p TargetPrefix) Option {
	return func(c *Client) {
		if p != "" {
			c.targetPrefix = p
		}
	}
}

// WithLogger supplies the logger used for reconnect bookkeeping.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithBackoff sets the reconnect delay policy for [Client.TrackDevices].
func WithBackoff(b Backoff) Option {
	return func(c *Client) { c.backoff = b }
}

// WithMaxOutput caps the bytes [Client.Shell] will capture.
func WithMaxOutput(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxOutput = n
		}
	}
}

// Client talks to one ADB server.
//
// It holds no connection: every call opens its own, because the host protocol
// closes a connection after a single host service and because a shared socket
// would let one slow device stall every health probe on the host. Client is
// safe for concurrent use.
type Client struct {
	endpoint     string
	dialer       *net.Dialer
	callTimeout  time.Duration
	targetPrefix TargetPrefix
	maxOutput    int
	backoff      Backoff
	log          *slog.Logger
}

// New returns a Client for the ADB server at endpoint, or at
// [DefaultEndpoint] when endpoint is empty.
func New(endpoint string, opts ...Option) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	c := &Client{
		endpoint:     endpoint,
		dialer:       &net.Dialer{},
		callTimeout:  DefaultCallTimeout,
		targetPrefix: PrefixUSB,
		maxOutput:    DefaultMaxOutput,
		backoff:      Backoff{},
		log:          slog.Default(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Endpoint returns the ADB server address this client talks to.
func (c *Client) Endpoint() string { return c.endpoint }

func (c *Client) dial(ctx context.Context) (*conn, error) {
	return dialConn(ctx, c.dialer, c.endpoint, c.callTimeout)
}

// hostMessage runs a one-shot host service that answers with a single
// length-prefixed message.
func (c *Client) hostMessage(ctx context.Context, op, service, target string, bySerial bool) (string, error) {
	if err := validateServiceString(op, service); err != nil {
		return "", err
	}
	cn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cn.Close()
	cn.devpath = target

	if err := cn.writeMessage(ctx, op, service, c.callTimeout); err != nil {
		return "", err
	}
	if err := cn.readStatus(ctx, op, service, target, bySerial, c.callTimeout); err != nil {
		return "", err
	}
	return cn.readMessage(ctx, op, c.callTimeout)
}

// devpathService builds a position-addressed host service string.
func (c *Client) devpathService(devpath, cmd string) string {
	return string(c.targetPrefix) + ":" + devpath + ":" + cmd
}

// targetMessage runs a position-addressed host service. The devpath is
// validated before it is interpolated; see [ValidateDevpath] for why that is
// a safety property rather than input hygiene.
func (c *Client) targetMessage(ctx context.Context, op, devpath, cmd string) (string, error) {
	if err := ValidateDevpath(devpath); err != nil {
		return "", err
	}
	return c.hostMessage(ctx, op, c.devpathService(devpath, cmd), devpath, false)
}

// ControlCmd is a position-addressed maintenance verb: a host service that
// acts on one transport rather than reading from it.
//
// Every one of them is addressed by devpath, never by serial. Duplicate OEM
// serials are real, so a serial-addressed reset can land on a device that was
// working perfectly and is hours into somebody's work — a worse outcome than
// the fault being recovered from. See doc.go for why this package refuses to
// know anything more than that about who is using a device.
type ControlCmd string

const (
	// ControlReconnect re-handshakes one transport in place. The device node
	// is untouched, so this is the least disruptive thing that can be done.
	ControlReconnect ControlCmd = "reconnect"

	// ControlReconnectOffline asks the server to re-handshake transports it
	// has already given up on. Scoped to the addressed position.
	ControlReconnectOffline ControlCmd = "reconnect-offline"

	// ControlDetach drops the server's claim on the device without touching
	// the USB device node. Pair it with ControlAttach.
	ControlDetach ControlCmd = "detach"

	// ControlAttach re-claims a previously detached device.
	ControlAttach ControlCmd = "attach"
)

// Control runs a maintenance verb against one physical position and returns
// whatever the server replied.
//
// It reports a *ProtocolError when the server refuses (an older server may not
// implement the verb at all, which is a refusal and not a fault) and a
// *TransportError when the socket fails. A caller driving a recovery ladder
// needs that distinction: a refusal means "try the next rung", a transport
// failure means "the host itself is unreachable and no rung will help".
func (c *Client) Control(ctx context.Context, devpath string, cmd ControlCmd) (string, error) {
	if cmd == "" {
		return "", fmt.Errorf("adbwire: empty control command for %q", devpath)
	}
	return c.targetMessage(ctx, "control:"+string(cmd), devpath, string(cmd))
}

// ---------------------------------------------------------------------------
// Host services
// ---------------------------------------------------------------------------

// Version returns the ADB server's protocol version, from host:version. It is
// the cheapest proof that the server is alive and speaking.
func (c *Client) Version(ctx context.Context) (int, error) {
	const op = "version"
	msg, err := c.hostMessage(ctx, op, "host:version", "", false)
	if err != nil {
		return 0, err
	}
	v, perr := strconv.ParseUint(strings.TrimSpace(msg), 16, 32)
	if perr != nil {
		return 0, newTransportError(op, c.endpoint, "", KindFrame,
			fmt.Errorf("version %q is not hex: %w", msg, perr))
	}
	return int(v), nil
}

// Devices returns the current long-format listing, from host:devices-l.
//
// The long form is not optional here. The short form omits the devpath, and a
// listing without devpaths cannot be addressed safely on a farm where OEM
// serials collide, so this never falls back to it.
func (c *Client) Devices(ctx context.Context) (Snapshot, error) {
	const op = "devices"
	msg, err := c.hostMessage(ctx, op, "host:devices-l", "", false)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		At:       time.Now(),
		Endpoint: c.endpoint,
		Devices:  parseDeviceList(msg),
	}, nil
}

// ServerFeatures returns the feature set of the ADB server itself, from
// host:host-features.
//
// The bare "host:features" service is deliberately not exposed: it resolves
// to whichever single device the server feels like picking, which is exactly
// the ambiguity every call in this package refuses. Use [Client.Features] to
// ask about a position.
func (c *Client) ServerFeatures(ctx context.Context) ([]string, error) {
	msg, err := c.hostMessage(ctx, "server_features", "host:host-features", "", false)
	if err != nil {
		return nil, err
	}
	return splitFeatures(msg), nil
}

// Features returns the feature set negotiated with the device at devpath.
func (c *Client) Features(ctx context.Context, devpath string) ([]string, error) {
	msg, err := c.targetMessage(ctx, "features", devpath, "features")
	if err != nil {
		return nil, err
	}
	return splitFeatures(msg), nil
}

func splitFeatures(msg string) []string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}
	parts := strings.Split(msg, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// State returns the connection state of the device at devpath, from
// get-state.
//
// A NotFoundError from this call means the server has no transport at that
// position right now. It is one observation of the third clock — device
// health — and nothing more.
func (c *Client) State(ctx context.Context, devpath string) (ConnState, error) {
	msg, err := c.targetMessage(ctx, "get_state", devpath, "get-state")
	if err != nil {
		return StateUnknown, err
	}
	return ParseConnState(msg), nil
}

// SerialOf returns the serial the device at devpath reports.
//
// This is the safe direction — position to serial — and is how a serial gets
// into the database as an observation. The reverse direction is
// [Client.UnsafeBySerial] and carries that name for a reason.
func (c *Client) SerialOf(ctx context.Context, devpath string) (string, error) {
	msg, err := c.targetMessage(ctx, "get_serialno", devpath, "get-serialno")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(msg), nil
}

// Kill asks the server to exit, via host:kill.
//
// Some server builds answer OKAY and then exit; others drop the connection on
// the way out. A close observed here is therefore success, not a failure —
// the request was to make the server go away.
func (c *Client) Kill(ctx context.Context) error {
	const op = "kill"
	const svc = "host:kill"

	cn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cn.Close()

	if err := cn.writeMessage(ctx, op, svc, c.callTimeout); err != nil {
		if te, ok := AsTransport(err); ok && te.PeerClosed() {
			return nil
		}
		return err
	}
	if err := cn.readStatus(ctx, op, svc, "", false, c.callTimeout); err != nil {
		if te, ok := AsTransport(err); ok && te.PeerClosed() {
			return nil
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Device transports and streams
// ---------------------------------------------------------------------------

// Transport is a connection that has been switched to one device. It carries
// exactly one device-side service, because the host protocol gives the rest
// of the socket to that service.
type Transport struct {
	cn       *conn
	client   *Client
	ctx      context.Context
	devpath  string
	target   string
	bySerial bool
	stop     func() bool
	taken    bool
}

// Devpath returns the physical position this transport was opened against.
// It is empty only for a transport opened by [Client.UnsafeBySerial].
func (t *Transport) Devpath() string { return t.devpath }

// Close closes the underlying socket. It is safe to call after [Transport.Service]
// has handed the socket to a [Stream]; both closes act on the same socket and
// closing is idempotent.
func (t *Transport) Close() error {
	if t.stop != nil {
		t.stop()
	}
	return t.cn.Close()
}

// Transport opens a connection and switches it to the device at devpath,
// using host:transport:<devpath>.
//
// The context governs the LIFETIME of the transport and of any stream taken
// from it, not merely the switch: cancelling it closes the socket under an
// in-flight read. That is the correct shape for a supervisor that wants to
// hang up on a device, and it is the only thing in this package that can
// close a healthy socket.
func (c *Client) Transport(ctx context.Context, devpath string) (*Transport, error) {
	if err := ValidateDevpath(devpath); err != nil {
		return nil, err
	}
	return c.openTransport(ctx, devpath, devpath, false)
}

// UnsafeBySerial opens a transport addressed by OEM serial.
//
// It is unsafe because serials are not unique. STF's own README documents a
// device shipping with serial "0123456789ABCDEF"; two of those in one rack
// are indistinguishable, and the server answers such a lookup by picking one
// or by refusing. A recovery action that landed on the wrong clone would
// disrupt a device that is hours into somebody else's work.
//
// Permitted uses: interactive operator tooling, and first-contact enrolment
// of a device whose position is not yet recorded. Health checks, resets,
// reboots, power cycles and anything else that can disturb a running device
// MUST address a devpath instead.
func (c *Client) UnsafeBySerial(ctx context.Context, serial string) (*Transport, error) {
	const op = "transport_unsafe_by_serial"
	s := strings.TrimSpace(serial)
	if s == "" {
		return nil, &UsageError{Op: op, Detail: "empty serial"}
	}
	if strings.ContainsAny(s, "\x00\n\r ") {
		return nil, &UsageError{Op: op, Detail: "serial contains whitespace or control characters", Value: serial}
	}
	return c.openTransport(ctx, s, "", true)
}

func (c *Client) openTransport(ctx context.Context, target, devpath string, bySerial bool) (*Transport, error) {
	const op = "transport"

	svc := "host:transport:" + target
	if err := validateServiceString(op, svc); err != nil {
		return nil, err
	}
	cn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	cn.devpath = devpath

	if err := cn.writeMessage(ctx, op, svc, c.callTimeout); err != nil {
		cn.Close()
		return nil, err
	}
	if err := cn.readStatus(ctx, op, svc, target, bySerial, c.callTimeout); err != nil {
		cn.Close()
		return nil, err
	}

	// From here the socket belongs to the device, so its lifetime is the
	// caller's context rather than any per-call timeout.
	stop := context.AfterFunc(ctx, func() { _ = cn.Close() })

	return &Transport{
		cn:       cn,
		client:   c,
		ctx:      ctx,
		devpath:  devpath,
		target:   target,
		bySerial: bySerial,
		stop:     stop,
	}, nil
}

// Service starts a device-side service on this transport — "shell,v2,raw:ls",
// "raw:…", "sync:", "reboot:" — and returns the resulting bidirectional
// stream. It may be called once per transport.
func (t *Transport) Service(ctx context.Context, service string) (*Stream, error) {
	const op = "service"
	if t.taken {
		return nil, &UsageError{Op: op, Detail: "transport already carries a service", Value: t.devpath}
	}
	if err := validateServiceString(op, service); err != nil {
		return nil, err
	}
	// Claimed before the write, not after the reply. A request that failed
	// half-written left the socket desynchronised, and a caller retrying on
	// the same transport would send its second service string into the middle
	// of the first — which the server would answer for whatever it managed to
	// parse. One attempt per transport; a failure means dial again.
	t.taken = true

	if err := t.cn.writeMessage(ctx, op, service, t.client.callTimeout); err != nil {
		return nil, err
	}
	if err := t.cn.readStatus(ctx, op, service, t.target, t.bySerial, t.client.callTimeout); err != nil {
		return nil, err
	}

	addressing := "devpath"
	if t.bySerial {
		addressing = "serial_unsafe"
	}
	streamsOpenedTotal.WithLabelValues(addressing).Inc()

	return &Stream{
		cn:      t.cn,
		ctx:     t.ctx,
		devpath: t.devpath,
		service: service,
		stop:    t.stop,
	}, nil
}

// Stream is a running device-side service: a raw, bidirectional byte stream.
//
// The context captured when the transport was opened is stored here on
// purpose. A stream's lifetime IS that context — cancelling it closes the
// socket, which is how a caller interrupts a read that would otherwise block
// for as long as the device stays silent.
type Stream struct {
	cn      *conn
	ctx     context.Context
	devpath string
	service string
	stop    func() bool
	once    sync.Once
}

// Devpath returns the position this stream is attached to, empty for a stream
// opened by serial.
func (s *Stream) Devpath() string { return s.devpath }

// Service returns the device-side service string this stream is running.
func (s *Stream) Service() string { return s.service }

// Read implements io.Reader.
//
// io.EOF is returned unwrapped. Every helper in the standard library, io.Copy
// first among them, treats EOF as the ordinary end of a stream, and wrapping
// it would turn a completed command into an error.
func (s *Stream) Read(p []byte) (int, error) {
	n, err := s.cn.br.Read(p)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		// Normalised to the bare sentinel, not merely passed through:
		// io.Copy and most of the standard library compare against io.EOF
		// with ==, so a wrapped EOF would turn a completed command into a
		// failure.
		return n, io.EOF
	}
	return n, s.cn.wrap(s.ctx, "stream_read", KindRead, err)
}

// Write implements io.Writer.
func (s *Stream) Write(p []byte) (int, error) {
	n, err := s.cn.nc.Write(p)
	if err != nil {
		return n, s.cn.wrap(s.ctx, "stream_write", KindWrite, err)
	}
	return n, nil
}

// CloseWrite half-closes the stream, which is how a device-side service that
// consumes stdin is told there is no more input.
func (s *Stream) CloseWrite() error {
	cw, ok := s.cn.nc.(interface{ CloseWrite() error })
	if !ok {
		return &UsageError{Op: "close_write", Detail: "underlying connection does not support half-close"}
	}
	if err := cw.CloseWrite(); err != nil {
		return s.cn.wrap(s.ctx, "close_write", KindWrite, err)
	}
	return nil
}

// SetDeadline bounds subsequent reads and writes on this stream. A zero time
// removes the bound. Use it for per-read limits; use the context for the
// stream's overall lifetime.
func (s *Stream) SetDeadline(t time.Time) error {
	return s.cn.nc.SetDeadline(t)
}

// Close implements io.Closer and is safe to call from another goroutine to
// interrupt a blocked Read.
func (s *Stream) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
	})
	return s.cn.Close()
}

// OpenService opens a transport to devpath and starts one device-side
// service on it. The returned stream owns the socket; closing it is enough.
func (c *Client) OpenService(ctx context.Context, devpath, service string) (*Stream, error) {
	tr, err := c.Transport(ctx, devpath)
	if err != nil {
		return nil, err
	}
	st, err := tr.Service(ctx, service)
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	return st, nil
}

// ShellStream runs command under the shell v2 protocol and returns the raw
// framed stream, for callers that want to consume output as it arrives or to
// write to stdin. Use [NewShellPacketReader] to decode it.
func (c *Client) ShellStream(ctx context.Context, devpath, command string) (*Stream, error) {
	return c.OpenService(ctx, devpath, ShellService(command))
}

// Shell runs command on the device at devpath and returns its demultiplexed
// output and exit status.
func (c *Client) Shell(ctx context.Context, devpath, command string) (*ShellResult, error) {
	const op = "shell"

	st, err := c.ShellStream(ctx, devpath, command)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	res, derr := DrainShellV2(st, c.maxOutput)
	if derr == nil {
		return res, nil
	}
	if ce := contextError(ctx, op); ce != nil {
		return res, ce
	}
	// DrainShellV2 takes a plain io.Reader, so its own failures arrive
	// untyped: io.ReadFull yields a bare io.ErrUnexpectedEOF when the stream
	// is cut mid-frame, and the packet cap yields a bare fmt error. Both are
	// wire failures, and a wire failure that reaches a caller unclassified and
	// uncounted is a hole in exactly the classification this package exists to
	// provide. wrap maps ErrUnexpectedEOF onto KindPeerClosed and everything
	// else onto the KindFrame default.
	if !IsTransport(derr) && !IsCanceled(derr) {
		derr = st.cn.wrap(ctx, op, KindFrame, derr)
	}
	return res, derr
}

// ---------------------------------------------------------------------------
// track-devices
// ---------------------------------------------------------------------------

// Backoff is the reconnect delay policy for [Tracker]. Zero values take
// the package defaults.
type Backoff struct {
	// Min is the first delay and the floor for every later one.
	Min time.Duration
	// Max is the ceiling.
	Max time.Duration
	// Factor multiplies the delay on each consecutive failure.
	Factor float64
}

// delay returns the wait before attempt n (1-based).
//
// The jitter is not decoration. Every host in the farm runs one of these
// readers, and an adb server restart or a network blip hits all of them at
// once; without jitter they would reconnect in lockstep and keep doing so.
func (b Backoff) delay(attempt int) time.Duration {
	lo := b.Min
	if lo <= 0 {
		lo = 250 * time.Millisecond
	}
	hi := b.Max
	if hi <= 0 {
		hi = 30 * time.Second
	}
	if hi < lo {
		hi = lo
	}
	factor := b.Factor
	if factor < 1 {
		factor = 2
	}
	d := float64(lo)
	for i := 1; i < attempt && d < float64(hi); i++ {
		d *= factor
	}
	if d > float64(hi) {
		d = float64(hi)
	}
	return time.Duration(float64(lo) + rand.Float64()*(d-float64(lo)))
}

// Tracker is a long-lived reader of host:track-devices-l.
//
// The ADB server pushes a fresh, COMPLETE device list on every change and
// then stays silent — a rack that is not being touched produces no traffic
// for hours. The reader reconnects on its own with jittered backoff.
//
// What it does not do is as important. A dropped connection emits no
// snapshot. It does not synthesise an empty list, and it does not mark
// anything absent. Losing the socket is evidence about the socket; the last
// state the server actually reported stands until the server reports
// otherwise.
type Tracker struct {
	c      *Client
	ctx    context.Context
	cancel context.CancelFunc
	snaps  chan Snapshot
	done   chan struct{}

	mu      sync.Mutex
	lastErr error
	seq     uint64
}

// TrackDevices starts a tracker and returns immediately. The tracker runs
// until ctx is cancelled or [Tracker.Close] is called.
func (c *Client) TrackDevices(ctx context.Context) *Tracker {
	tctx, cancel := context.WithCancel(ctx)
	t := &Tracker{
		c:      c,
		ctx:    tctx,
		cancel: cancel,
		// Depth one, with the stale entry dropped on overflow: every
		// snapshot is a complete state, so the newest strictly
		// supersedes any the consumer has not taken yet. Blocking here
		// instead would let a slow consumer stall the socket and turn a
		// bookkeeping delay into an apparent host outage.
		snaps: make(chan Snapshot, 1),
		done:  make(chan struct{}),
	}
	go t.run()
	return t
}

// Snapshots returns the channel of device-list snapshots. It is closed when
// the tracker stops.
func (t *Tracker) Snapshots() <-chan Snapshot { return t.snaps }

// LastError returns the most recent failure the reader saw, or nil. It is
// diagnostic: a non-nil value means this process cannot currently see the ADB
// server, and says nothing about the devices attached to it.
func (t *Tracker) LastError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

// Close stops the tracker and waits for its goroutine to finish and its
// socket to be closed.
func (t *Tracker) Close() {
	t.cancel()
	<-t.done
}

func (t *Tracker) run() {
	defer close(t.done)
	defer close(t.snaps)

	attempt := 0
	for {
		if t.ctx.Err() != nil {
			return
		}
		got, err := t.session()
		if t.ctx.Err() != nil {
			return
		}
		if got > 0 {
			// The connection did useful work before it broke, so the
			// next failure starts the backoff over from Min.
			attempt = 0
		}
		t.setErr(err)
		attempt++
		trackerReconnectsTotal.Inc()
		wait := t.c.backoff.delay(attempt)
		t.c.log.Debug("adbwire: track-devices reader reconnecting",
			"endpoint", t.c.endpoint,
			"attempt", attempt,
			"wait", wait,
			"err", err)

		timer := time.NewTimer(wait)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// session holds one connection open for as long as the server keeps it, and
// returns how many snapshots it delivered along with the failure that ended
// it.
func (t *Tracker) session() (int, error) {
	const op = "track"
	const svc = "host:track-devices-l"

	cn, err := t.c.dial(t.ctx)
	if err != nil {
		return 0, err
	}
	defer cn.Close()

	if err := cn.writeMessage(t.ctx, op, svc, t.c.callTimeout); err != nil {
		return 0, err
	}
	// The long form is required, not preferred: a listing without devpaths
	// cannot be addressed safely here, so a server that refuses it is a
	// hard failure rather than a reason to fall back to serials.
	if err := cn.readStatus(t.ctx, op, svc, "", false, t.c.callTimeout); err != nil {
		return 0, err
	}

	n := 0
	for {
		// No fallback timeout: silence on this stream is the steady
		// state of a farm nobody is unplugging, not a symptom.
		payload, rerr := cn.readMessage(t.ctx, op, 0)
		if rerr != nil {
			return n, rerr
		}
		n++
		trackerSnapshotsTotal.Inc()
		t.emit(payload)
	}
}

func (t *Tracker) emit(payload string) {
	t.mu.Lock()
	t.seq++
	snap := Snapshot{
		At:       time.Now(),
		Endpoint: t.c.endpoint,
		Sequence: t.seq,
		Devices:  parseDeviceList(payload),
	}
	t.lastErr = nil
	t.mu.Unlock()

	for {
		select {
		case <-t.ctx.Done():
			return
		case t.snaps <- snap:
			return
		default:
		}
		// Discard the superseded snapshot and try again.
		select {
		case <-t.snaps:
		default:
		}
	}
}

func (t *Tracker) setErr(err error) {
	t.mu.Lock()
	t.lastErr = err
	t.mu.Unlock()
}
