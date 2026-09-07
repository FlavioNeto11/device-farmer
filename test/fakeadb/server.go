package fakeadb

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultHostVersion is what host:version reports, as the real adb server does:
// the version number rendered as four hex digits ("0029"). Clients parse it
// base 16, so a decimal-formatted fake would be silently misread as 41 -> 65.
const DefaultHostVersion = 41

// maxPayload is the largest payload the 4-hex-digit length header can express.
const maxPayload = 0xffff

// noSerial is what adb prints for a transport whose serial it has not read.
// The placeholder matters: an empty column would leave the state in field one
// and make the whole line parse as something other than a device, so the
// handset would not appear as unhealthy — it would not appear at all.
const noSerial = "(no serial number)"

// Error strings the real adb server returns. Exported so a test can assert on
// classification without copying magic strings into two places.
const (
	MsgAmbiguousTarget = "more than one device"          // a target matched >1 transport
	MsgMultipleDevices = "more than one device/emulator" // no target, >1 transport
	MsgNoDevices       = "no devices/emulators found"
	MsgDeviceOffline   = "device offline"
	MsgUnauthorized    = "device unauthorized"
)

// MsgNotFound renders adb's not-found error for a target.
func MsgNotFound(target string) string { return fmt.Sprintf("device '%s' not found", target) }

// DefaultFeatures returns the feature set a modern adbd advertises. A fresh
// slice each call: callers mutate what they are handed.
func DefaultFeatures() []string {
	return []string{
		"shell_v2", "cmd", "stat_v2", "ls_v2", "fixed_push_mkdir", "apex", "abb",
		"fixed_push_symlink_timestamp", "abb_exec", "remount_shell", "track_app",
		"sendrecv_v2", "sendrecv_v2_brotli", "sendrecv_v2_lz4", "sendrecv_v2_zstd",
		"sendrecv_v2_dry_run_send",
	}
}

// State is an ADB connection state as it appears on the wire. The values are a
// subset of the farm.device_runtime.adb_state CHECK list, so a state observed
// here can be written to that column verbatim. State is a string type: any
// other member of that CHECK list may be used directly, e.g. State("rescue").
type State string

const (
	StateDevice        State = "device"
	StateOffline       State = "offline"
	StateUnauthorized  State = "unauthorized"
	StateAuthorizing   State = "authorizing"
	StateConnecting    State = "connecting"
	StateNoPermissions State = "no_permissions"
	StateBootloader    State = "bootloader"
	StateRecovery      State = "recovery"
	StateSideload      State = "sideload"

	// StateAbsent is not a wire state. A device in it stays in the scripted
	// table but vanishes from every listing and stops matching every target,
	// modelling a handset that fell off the USB bus while the control plane
	// still has a row — and, crucially, still has a live lease — for it.
	StateAbsent State = "absent"
)

// Device is one scripted entry in the fake's device table.
type Device struct {
	// Serial is the OEM-reported serial. It is an observation, not an
	// identity: farm.devices.adb_serial is deliberately not UNIQUE because
	// duplicate OEM serials ship in the real world. Two Devices may share one.
	Serial string

	// Devpath is the physical USB position, "usb:<bus>-<port>[.<port>...]",
	// the string farm.slots.adb_devpath generates. It is this package's
	// primary key: Add, Remove, SetState and Update all address by devpath.
	Devpath string

	Model    string // "model:" in the long listing; non-alphanumerics are sanitised to '_'
	Product  string // "product:"
	Codename string // "device:"

	// State defaults to StateDevice when zero.
	State State

	// TransportID is assigned on Add when zero. adb reuses small integers
	// across server restarts, so this number is meaningless without the
	// host epoch that minted it — see farm.hosts.host_epoch.
	TransportID int64

	// Features overrides DefaultFeatures for host:features addressed here.
	Features []string
}

// Request is one host request the server received, in arrival order.
type Request struct {
	At      time.Time
	Service string    // the service string exactly as framed by the client
	Target  string    // the address the client used: serial, devpath or transport-id:N
	Devpath string    // the device the request actually reached; empty if none did
	Reply   string    // "OKAY", "FAIL: <msg>", "RESET", "HANG" or "ERROR: <msg>"
	Fault   FaultKind // the injected fault applied, if any

	// Preamble is the fence-proxy admission frame that arrived on the same
	// connection before this request, exactly as framed; empty when the
	// client sent none. See PreamblePrefix.
	Preamble string
}

// PreamblePrefix opens the admission frame a client sends to the fence proxy
// before its first host request: "fence:v1 class=lease devpath=... fence=...".
//
// This fake stands in for the proxy and the ADB server together, so it does
// what the proxy does with that frame — consumes it before the ordinary
// protocol begins — and records it on the request that followed, where a test
// can assert on it. A real ADB server would answer FAIL "unknown host service";
// a fake that did the same could not tell a client that sends the preamble
// correctly from one that does not send it at all.
const PreamblePrefix = "fence:v1"

// Stats are cheap counters a test can assert on.
type Stats struct {
	Accepted  int // connections accepted
	Requests  int // host requests routed
	Faults    int // injected faults applied
	Trackers  int // host:track-devices streams currently open
	Snapshots int // device-list snapshots pushed to trackers
	Streams   int // duplex device services currently held by a stream handler
}

// Server is a fake ADB host server.
type Server struct {
	ln   net.Listener
	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	devices  []Device
	nextTID  int64
	gen      uint64        // device-table generation; bumped on every mutation
	changed  chan struct{} // closed and replaced on every mutation
	faults   []*faultRule
	scripts  []script
	streams  []streamScript
	scrcpy   map[string]*scrcpyDevice
	requests []*Request
	conns    map[net.Conn]struct{}
	stats    Stats
	version  int
	features []string
}

// New starts a server on an ephemeral loopback port and applies fixtures.
// Callers outside a test must Close it; inside a test prefer Start.
func New(fixtures ...Fixture) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fakeadb: listen: %w", err)
	}
	return start(ln, fixtures...), nil
}

// NewTLS is New behind a TLS listener, standing in for the fence proxy that
// fronts a host's ADB server. cfg decides what the client must present; a
// config that requires and verifies a client certificate is how a test proves
// the client handed one over.
func NewTLS(cfg *tls.Config, fixtures ...Fixture) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("fakeadb: NewTLS needs a TLS configuration")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fakeadb: listen: %w", err)
	}
	return start(tls.NewListener(ln, cfg), fixtures...), nil
}

// StartTLS is NewTLS for tests, with the same cleanup as Start.
func StartTLS(tb testing.TB, cfg *tls.Config, fixtures ...Fixture) *Server {
	tb.Helper()
	s, err := NewTLS(cfg, fixtures...)
	if err != nil {
		tb.Fatalf("fakeadb: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

func start(ln net.Listener, fixtures ...Fixture) *Server {
	s := &Server{
		ln:       ln,
		done:     make(chan struct{}),
		changed:  make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
		nextTID:  1,
		gen:      1, // trackers start at 0, so the first snapshot always ships
		version:  DefaultHostVersion,
		features: DefaultFeatures(),
	}
	s.wg.Add(1)
	go s.accept()
	s.Apply(fixtures...)
	return s
}

// Start is New for tests: it fails the test on error and registers cleanup, so
// the listener, every open connection and every scripted goroutine are gone
// before the test returns.
func Start(tb testing.TB, fixtures ...Fixture) *Server {
	tb.Helper()
	s, err := New(fixtures...)
	if err != nil {
		tb.Fatalf("fakeadb: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

// Addr is the host:port to dial, e.g. "127.0.0.1:53412".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the listener, severs every open connection and waits for every
// goroutine the server started. It is idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// SeverAll RSTs every connection currently open but keeps listening. This is
// the adb-server-restart shape: every socket in the farm dies at the same
// instant, which under STF #663 is a mass release and here must be nothing but
// a reconnect.
func (s *Server) SeverAll() int {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()
	for _, c := range conns {
		sever(c)
	}
	return len(conns)
}

// SetHostVersion changes what host:version reports.
func (s *Server) SetHostVersion(v int) {
	s.mu.Lock()
	s.version = v
	s.mu.Unlock()
}

// SetFeatures changes the default feature set reported for devices that carry
// no Features of their own, and for host:host-features.
func (s *Server) SetFeatures(f ...string) {
	s.mu.Lock()
	s.features = append([]string(nil), f...)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------
// Device table
// ---------------------------------------------------------------------

// Add installs a device and returns it as stored, with TransportID filled in.
// Devpath is mandatory and unique: it is the physical position, and two rows
// claiming one position is a bug in the test, not a scenario.
func (s *Server) Add(d Device) Device {
	if d.Devpath == "" {
		panic("fakeadb: Device.Devpath is required — the physical position is the key")
	}
	if d.State == "" {
		d.State = StateDevice
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.devices {
		if e.Devpath == d.Devpath {
			panic("fakeadb: duplicate devpath " + d.Devpath)
		}
	}
	if d.TransportID == 0 {
		d.TransportID = s.nextTID
		s.nextTID++
	}
	// Copy the caller's slice: keeping the header would let a test mutate a
	// device's features from outside the lock while a connection goroutine
	// reads them, which is a data race in the harness rather than in the
	// code under test — the worst kind to debug.
	d.Features = append([]string(nil), d.Features...)
	s.devices = append(s.devices, d)
	s.bumpLocked()
	return d
}

// Remove deletes a device outright — the cable was pulled and the row is gone.
// Prefer SetState(devpath, StateAbsent) when the control plane should still
// believe the device exists.
func (s *Server) Remove(devpath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.devices {
		if d.Devpath == devpath {
			s.devices = append(s.devices[:i], s.devices[i+1:]...)
			s.bumpLocked()
			return true
		}
	}
	return false
}

// SetState changes a device's wire state and wakes every tracker.
func (s *Server) SetState(devpath string, st State) bool {
	return s.Update(devpath, func(d *Device) { d.State = st })
}

// Update mutates a device in place under the server lock and wakes every
// tracker. mutate must not call back into the server.
func (s *Server) Update(devpath string, mutate func(*Device)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.devices {
		if s.devices[i].Devpath == devpath {
			mutate(&s.devices[i])
			s.bumpLocked()
			return true
		}
	}
	return false
}

// Devices returns a snapshot of the table, including absent devices.
func (s *Server) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Device(nil), s.devices...)
}

// Device returns one entry by devpath.
func (s *Server) Device(devpath string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devices {
		if d.Devpath == devpath {
			return d, true
		}
	}
	return Device{}, false
}

// Clear empties the device table, pushing an empty snapshot to every tracker.
func (s *Server) Clear() {
	s.mu.Lock()
	s.devices = nil
	s.bumpLocked()
	s.mu.Unlock()
}

// bumpLocked advances the table generation and wakes everyone waiting on a
// change. Callers must hold s.mu.
func (s *Server) bumpLocked() {
	s.gen++
	close(s.changed)
	s.changed = make(chan struct{})
}

// ---------------------------------------------------------------------
// Observation
// ---------------------------------------------------------------------

// Requests returns every host request received, in arrival order. An entry is
// appended the moment the request arrives and filled in as it is answered, so
// one that has not been answered yet carries an empty Reply. A streaming
// request is a single entry whose Reply reflects its most recent write, which
// is why snapshot counting belongs to Stats and not to this log. A duplex
// stream keeps its OKAY until it ends badly: "RESET" once a handler severs
// it, "ERROR: <msg>" when the handler itself failed.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, 0, len(s.requests))
	for _, r := range s.requests {
		out = append(out, *r)
	}
	return out
}

// RequestsTo returns the requests that reached the device at devpath. This is
// how a test proves a devpath-addressed command landed on exactly one of two
// clones sharing a serial.
func (s *Server) RequestsTo(devpath string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Devpath == devpath {
			out = append(out, r)
		}
	}
	return out
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *Server) record(rec *Request) {
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.stats.Requests++
	s.mu.Unlock()
}

// note mutates an already-recorded Request under the lock.
func (s *Server) note(rec *Request, f func(*Request)) {
	s.mu.Lock()
	f(rec)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------

func (s *Server) list(long bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(long)
}

// listLocked renders host:devices / host:devices-l output. The long form
// mirrors adb's append_transport: "%-22s %s" then the devpath under an empty
// key, then product/model/device, then transport_id last so a parser scanning
// backwards from the newline can always find it.
//
// Identity fields are omitted for a device that is not in state "device",
// because adb has not read its properties yet. A parser that assumes they are
// present is broken, and this fake exists to catch that.
func (s *Server) listLocked(long bool) string {
	var b strings.Builder
	for _, d := range s.devices {
		if d.State == StateAbsent {
			continue
		}
		serial := d.Serial
		if serial == "" {
			serial = noSerial
		}
		if !long {
			fmt.Fprintf(&b, "%s\t%s\n", serial, d.State)
			continue
		}
		fmt.Fprintf(&b, "%-22s %s", serial, d.State)
		appendField(&b, "", d.Devpath, false)
		if d.State == StateDevice {
			appendField(&b, "product:", d.Product, false)
			appendField(&b, "model:", d.Model, true)
			appendField(&b, "device:", d.Codename, false)
		}
		fmt.Fprintf(&b, " transport_id:%d\n", d.TransportID)
	}
	return b.String()
}

func appendField(b *strings.Builder, key, value string, sanitize bool) {
	if value == "" {
		return
	}
	b.WriteByte(' ')
	b.WriteString(key)
	if !sanitize {
		b.WriteString(value)
		return
	}
	// adb sanitises the model field so that whitespace cannot be mistaken for
	// a field separator: "Pixel 6a" goes out as "Pixel_6a".
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
}

// ---------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------

func readFrame(r io.Reader) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	if err != nil {
		return "", fmt.Errorf("fakeadb: malformed length header %q: %w", string(hdr[:]), err)
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// frame renders a length-prefixed payload. Longer payloads than the header can
// express are truncated: the real protocol cannot say more either.
func frame(payload string) []byte {
	if len(payload) > maxPayload {
		payload = payload[:maxPayload]
	}
	return []byte(fmt.Sprintf("%04x%s", len(payload), payload))
}

func okayFrame(payload string) []byte { return append([]byte("OKAY"), frame(payload)...) }

func failBytes(msg string) []byte { return append([]byte("FAIL"), frame(msg)...) }

func summarize(b []byte) string {
	if len(b) < 4 {
		return string(b)
	}
	switch string(b[:4]) {
	case "OKAY":
		return "OKAY"
	case "FAIL":
		if len(b) > 8 {
			return "FAIL: " + string(b[8:])
		}
		return "FAIL"
	}
	return string(b[:4])
}

func writeAll(c net.Conn, b []byte) error {
	for len(b) > 0 {
		n, err := c.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// sever drops the connection with a TCP RST rather than a FIN, so the peer sees
// ECONNRESET instead of a clean EOF. SO_LINGER 0 is what makes the difference,
// and the difference is the whole point: a clean EOF is easy to handle, a reset
// mid-read is the failure that STF #663 mishandles.
func sever(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}

// ---------------------------------------------------------------------
// Connection handling
// ---------------------------------------------------------------------

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.stats.Accepted++
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *Server) drop(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

// serve handles one connection. The real adb server closes the socket after a
// completed host request; only transport switches and track-devices keep it,
// and this fake keeps the same shape so a client that assumes otherwise fails
// here rather than against hardware.
func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	defer s.drop(c)
	br := bufio.NewReader(c)
	svc, err := readFrame(br)
	if err != nil {
		return
	}
	// The admission frame, when there is one, is the first thing on the
	// connection and is never answered; the request it protects follows it.
	var pre string
	if strings.HasPrefix(svc, PreamblePrefix) {
		pre = svc
		if svc, err = readFrame(br); err != nil {
			return
		}
	}
	s.route(c, br, svc, pre)
}

func (s *Server) route(c net.Conn, br *bufio.Reader, svc, pre string) {
	tgt, cmd, ok := splitService(svc)
	// Published only once every field set outside the lock is set: after
	// record, the entry belongs to s.mu and is edited through note.
	rec := &Request{At: time.Now(), Service: svc, Target: tgt.String(), Preamble: pre}
	s.record(rec)
	if !ok {
		s.finish(c, rec, failBytes("unknown host service "+svc), s.takeFault(svc, ""))
		return
	}

	switch {
	case cmd == "version":
		s.finish(c, rec, okayFrame(fmt.Sprintf("%04x", s.hostVersion())), s.takeFault(svc, tgt.text))

	case cmd == "host-features":
		// host:host-features is the SERVER's feature set. "features" without
		// the prefix is the DEVICE's and needs a transport — a distinction
		// clients get wrong.
		s.finish(c, rec, okayFrame(strings.Join(s.defaultFeatures(), ",")), s.takeFault(svc, tgt.text))

	case cmd == "kill":
		// Answered but not obeyed: a test that kills the fake by accident
		// would take its own listener down mid-run. The request is recorded
		// so an assertion can still see it happened.
		s.finish(c, rec, []byte("OKAY"), s.takeFault(svc, tgt.text))

	case cmd == "devices":
		s.finish(c, rec, okayFrame(s.list(false)), s.takeFault(svc, tgt.text))

	case cmd == "devices-l":
		s.finish(c, rec, okayFrame(s.list(true)), s.takeFault(svc, tgt.text))

	case cmd == "track-devices", cmd == "track-devices-l":
		s.track(c, br, rec, svc, strings.HasSuffix(cmd, "-l"))

	case cmd == "transport-any", cmd == "transport-usb", cmd == "transport-local",
		strings.HasPrefix(cmd, "transport:"), strings.HasPrefix(cmd, "transport-id:"):
		s.transport(c, br, rec, svc, tgt, cmd)

	case strings.HasPrefix(cmd, "wait-for-"):
		s.waitFor(c, br, rec, svc, tgt, cmd)

	default:
		s.deviceQuery(c, rec, svc, tgt, cmd)
	}
}

func (s *Server) hostVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *Server) defaultFeatures() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.features...)
}

// finish writes one complete reply, honouring an injected fault. It reports
// whether the connection survived and the reply was delivered in full.
func (s *Server) finish(c net.Conn, rec *Request, body []byte, f *Fault) bool {
	if f == nil {
		s.note(rec, func(r *Request) { r.Reply = summarize(body) })
		return writeAll(c, body) == nil
	}

	s.note(rec, func(r *Request) { r.Fault = f.Kind })
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-s.done:
			return false
		}
	}

	switch f.Kind {
	case FaultFail:
		msg := f.Message
		if msg == "" {
			msg = "scripted failure"
		}
		body = failBytes(msg)
		s.note(rec, func(r *Request) { r.Reply = summarize(body) })
		return writeAll(c, body) == nil

	case FaultHang:
		// Write nothing at all. The caller's context deadline is the only
		// thing that ends this, which is precisely what we want to test.
		s.note(rec, func(r *Request) { r.Reply = "HANG" })
		s.hang(c)
		return false

	case FaultReset:
		n := f.AfterBytes
		if n > len(body) {
			n = len(body)
		}
		if n > 0 {
			_ = writeAll(c, body[:n])
		}
		s.note(rec, func(r *Request) { r.Reply = "RESET" })
		sever(c)
		return false
	}

	s.note(rec, func(r *Request) { r.Reply = summarize(body) })
	return writeAll(c, body) == nil
}

// watchPeer signals on the returned channel once the client has gone away —
// EOF, a reset, or any other read failure.
//
// The long-lived services (track-devices, wait-for-*) otherwise only discover
// a dead socket the next time they try to write, which on a quiet farm may be
// never: SeverAll would leave a tracker parked forever, still counted in
// Stats.Trackers, and a test looping over sever-and-reconnect would leak a
// goroutine per iteration while reporting that nothing had happened. Noticing
// the hang-up is exactly what makes SeverAll model an adb server restart.
//
// The reader is drained rather than merely polled because a client is free to
// pipeline bytes behind its request; discarding them keeps the socket from
// filling and stalling the peer.
func (s *Server) watchPeer(r io.Reader) <-chan struct{} {
	gone := make(chan struct{})
	// Safe against Close: this runs inside a connection goroutine already
	// counted in s.wg, so the counter cannot be at zero here, and the copy
	// ends when serve's deferred drop closes the socket.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(gone)
		_, _ = io.Copy(io.Discard, r)
	}()
	return gone
}

// hang holds the connection open, writing nothing, until the client gives up
// or the server shuts down.
func (s *Server) hang(c net.Conn) {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, c)
	}()
	select {
	case <-gone:
	case <-s.done:
		_ = c.Close()
		<-gone
	}
}

// ---------------------------------------------------------------------
// host:track-devices
// ---------------------------------------------------------------------

// track streams a fresh device-list snapshot on every table mutation. The
// stream is OKAY once, then bare length-prefixed payloads forever — there is no
// second OKAY, and a client that waits for one hangs against real adb too.
//
// The OKAY rides out with the first snapshot rather than ahead of it, so that a
// fault opportunity is exactly one snapshot: Fault.Skip counts device-list
// updates, which is what a test asking for "die on the third update" means.
func (s *Server) track(c net.Conn, br *bufio.Reader, rec *Request, svc string, long bool) {
	s.mu.Lock()
	s.stats.Trackers++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stats.Trackers--
		s.mu.Unlock()
	}()

	gone := s.watchPeer(br)

	var last uint64
	opened := false
	for {
		s.mu.Lock()
		gen := s.gen
		payload := s.listLocked(long)
		wait := s.changed
		s.mu.Unlock()

		if gen != last {
			body := frame(payload)
			if !opened {
				body = append([]byte("OKAY"), body...)
			}
			f := s.takeFault(svc, "")
			if f != nil && opened && f.Kind == FaultFail {
				// A FAIL mid-stream is not expressible: the status was sent
				// long ago. Sever instead, which is what the client would see
				// from an adb server that died holding the socket.
				cut := *f
				cut.Kind = FaultReset
				cut.AfterBytes = 0
				f = &cut
			}
			if !s.finish(c, rec, body, f) {
				return
			}
			s.mu.Lock()
			s.stats.Snapshots++
			s.mu.Unlock()
			opened = true
			last = gen
		}

		select {
		case <-wait:
		case <-gone:
			return
		case <-s.done:
			return
		}
	}
}

// ---------------------------------------------------------------------
// Transports and device-scoped requests
// ---------------------------------------------------------------------

func (s *Server) transport(c net.Conn, br *bufio.Reader, rec *Request, svc string, tgt target, cmd string) {
	want := tgt
	switch {
	case strings.HasPrefix(cmd, "transport-id:"):
		id, err := strconv.ParseInt(cmd[len("transport-id:"):], 10, 64)
		if err != nil {
			s.finish(c, rec, failBytes("bad transport id"), nil)
			return
		}
		want = target{tid: id}
	case strings.HasPrefix(cmd, "transport:"):
		want = target{text: cmd[len("transport:"):]}
	case cmd == "transport-usb":
		want = target{usbOnly: true}
	}
	s.note(rec, func(r *Request) { r.Target = want.String() })

	d, err := s.one(want)
	f := s.takeFault(svc, faultKey(want, d, err))
	if err != nil {
		s.finish(c, rec, failBytes(err.Error()), f)
		return
	}
	if d.State != StateDevice {
		// acquire_one_transport refuses a transport switch to a device that
		// is not up. Note what this is NOT: evidence about a lease.
		s.finish(c, rec, failBytes(stateError(d.State)), f)
		return
	}
	s.note(rec, func(r *Request) { r.Devpath = d.Devpath })
	if !s.finish(c, rec, []byte("OKAY"), f) {
		return
	}
	s.deviceStream(c, br, d)
}

// deviceStream serves the one device service that follows a transport switch.
// After its OKAY the bytes are raw: no framing, no length, just the stream
// until close — which is why a reset here is indistinguishable from a device
// dying, and why adbwire must classify it as transport-only.
//
// Stream scripts are consulted FIRST, and a match still goes out through
// finish for its OKAY rather than writing four bytes directly. That is what
// keeps failure injection whole: a FaultFail on this service string refuses a
// duplex service in the server's own words, a FaultReset severs it before the
// handler ever sees the socket, and a FaultHang swallows it. None of that
// would happen if a duplex match jumped the fault check on its way to the
// handler, and the hole would only show up in the one test that needed it.
func (s *Server) deviceStream(c net.Conn, br *bufio.Reader, d Device) {
	svc, err := readFrame(br)
	if err != nil {
		return
	}
	rec := &Request{At: time.Now(), Service: svc, Target: d.Devpath, Devpath: d.Devpath}
	s.record(rec)
	f := s.takeFault(svc, d.Devpath)

	if h, ok := s.matchStream(d, svc); ok {
		// Two conditions, not one, because finish reports whether the
		// CONNECTION survived and a FAIL is something a connection survives.
		// Handing the handler a socket the server has just refused would
		// write a screen stream on top of a refusal — which is not a shape
		// any client is built to parse, and would have made every
		// fault-injection test on a duplex service quietly meaningless. Only
		// a fault that left the reply alone still leaves a service to run.
		if !s.finish(c, rec, []byte("OKAY"), f) || (f != nil && f.Kind != FaultNone) {
			return
		}
		s.runStream(c, br, rec, d.Devpath, svc, h)
		return
	}
	s.finish(c, rec, append([]byte("OKAY"), s.scriptFor(d, svc)...), f)
}

func (s *Server) deviceQuery(c net.Conn, rec *Request, svc string, tgt target, cmd string) {
	d, err := s.one(tgt)
	f := s.takeFault(svc, faultKey(tgt, d, err))
	if err != nil {
		s.finish(c, rec, failBytes(err.Error()), f)
		return
	}
	s.note(rec, func(r *Request) { r.Devpath = d.Devpath })

	var body []byte
	switch cmd {
	case "get-state":
		// Deliberately answered for offline and unauthorized devices too:
		// "what state is it in" is a health question, and health questions
		// are always answerable.
		body = okayFrame(string(d.State))
	case "get-serialno":
		body = okayOrUnknown(d.Serial)
	case "get-devpath":
		body = okayOrUnknown(d.Devpath)
	case "get-product":
		body = okayOrUnknown(d.Product)
	case "features":
		if d.State != StateDevice {
			body = failBytes(stateError(d.State))
			break
		}
		feats := d.Features
		if len(feats) == 0 {
			feats = s.defaultFeatures()
		}
		body = okayFrame(strings.Join(feats, ","))
	case "reconnect":
		body = okayFrame(fmt.Sprintf("reconnecting %s [%s]\n", d.Serial, d.State))
	default:
		body = failBytes("unknown host service " + svc)
	}
	s.finish(c, rec, body, f)
}

func okayOrUnknown(v string) []byte {
	if v == "" {
		return failBytes("unknown")
	}
	return okayFrame(v)
}

// waitFor implements host:wait-for-<transport>-<state>. The protocol is OKAY
// immediately (request accepted), then a second bare OKAY once the condition
// holds. Nothing here is on a timer: the caller's context is the deadline.
func (s *Server) waitFor(c net.Conn, br *bufio.Reader, rec *Request, svc string, tgt target, cmd string) {
	spec := cmd[len("wait-for-"):]
	i := strings.Index(spec, "-")
	if i < 0 {
		s.finish(c, rec, failBytes("unknown host service "+svc), nil)
		return
	}
	want := State(spec[i+1:])

	if !s.finish(c, rec, []byte("OKAY"), s.takeFault(svc, tgt.text)) {
		return
	}

	gone := s.watchPeer(br)

	for {
		// The generation channel is captured BEFORE the condition is
		// evaluated, so a mutation racing the check wakes the next select
		// instead of being lost between the two.
		s.mu.Lock()
		wait := s.changed
		s.mu.Unlock()

		// "disconnect" is the absence of a device, so it is answered from
		// the match set; every other state needs one resolved device, and
		// the devpath is recorded only when there is one to record.
		if want == "disconnect" {
			if len(s.match(tgt)) == 0 {
				_ = writeAll(c, []byte("OKAY"))
				return
			}
		} else if d, err := s.one(tgt); err == nil && d.State == want {
			s.note(rec, func(r *Request) { r.Devpath = d.Devpath })
			_ = writeAll(c, []byte("OKAY"))
			return
		}

		select {
		case <-wait:
		case <-gone:
			return
		case <-s.done:
			return
		}
	}
}

// ---------------------------------------------------------------------
// Target resolution
// ---------------------------------------------------------------------

// target is a parsed device address.
type target struct {
	text    string // serial or devpath, exactly as the client wrote it
	tid     int64  // transport id, when addressed that way
	usbOnly bool   // host-usb: / transport-usb; every fake device is USB
}

func (t target) String() string {
	if t.tid > 0 {
		return fmt.Sprintf("transport-id:%d", t.tid)
	}
	return t.text
}

// match returns every visible device the target selects. Matching mirrors
// atransport::MatchesTarget: serial OR devpath. Two clones sharing a serial
// both match — that ambiguity is a feature of the protocol and a hazard of the
// farm, and TwoClonesFixture exists to hold it under a microscope.
func (s *Server) match(t target) []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Device
	for _, d := range s.devices {
		if d.State == StateAbsent {
			continue
		}
		switch {
		case t.tid > 0:
			if d.TransportID == t.tid {
				out = append(out, d)
			}
		case t.text == "":
			out = append(out, d)
		case d.Serial == t.text, d.Devpath == t.text:
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) one(t target) (Device, error) {
	m := s.match(t)
	switch {
	case len(m) == 1:
		return m[0], nil
	case len(m) == 0:
		if t.tid > 0 {
			return Device{}, fmt.Errorf("no device with transport id %d", t.tid)
		}
		if t.text != "" {
			return Device{}, errors.New(MsgNotFound(t.text))
		}
		return Device{}, errors.New(MsgNoDevices)
	default:
		if t.text != "" || t.tid > 0 {
			return Device{}, errors.New(MsgAmbiguousTarget)
		}
		return Device{}, errors.New(MsgMultipleDevices)
	}
}

func stateError(st State) string {
	if st == StateUnauthorized {
		return MsgUnauthorized
	}
	return MsgDeviceOffline
}

// faultKey is what Fault.Devpath is compared against: the device the request
// actually resolved to, or the literal target when it resolved to nothing.
func faultKey(t target, d Device, err error) string {
	if err == nil && d.Devpath != "" {
		return d.Devpath
	}
	return t.text
}

// ---------------------------------------------------------------------
// Service parsing
// ---------------------------------------------------------------------

// devpathTargetRE matches a leading devpath followed by ':'. The body is the
// same shape farm.slots.usb_path enforces, prefixed with "usb:". Splitting on
// the first colon — which is what a naive parser does — would tear
// "usb:3-1.4.1" in half and address a device that does not exist.
var devpathTargetRE = regexp.MustCompile(`^usb:[0-9]+-[0-9]+(?:\.[0-9]+)*:`)

// hostPortTargetRE matches a leading "<host>:<port>:" for TCP-attached devices.
var hostPortTargetRE = regexp.MustCompile(`^[^:]+:[0-9]{1,5}:`)

// splitService parses a host service string into an address and a command.
//
//	host:<cmd>                       — any device / no device
//	host-serial:<serial>:<cmd>       — addressed by observed serial (ambiguous!)
//	host-usb:<devpath>:<cmd>         — addressed by physical position
//	host-transport-id:<id>:<cmd>     — addressed by transport id
//	host-local:<cmd>
func splitService(svc string) (target, string, bool) {
	switch {
	case strings.HasPrefix(svc, "host-transport-id:"):
		rest := svc[len("host-transport-id:"):]
		i := strings.Index(rest, ":")
		if i < 0 {
			return target{}, "", false
		}
		id, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil {
			return target{}, "", false
		}
		return target{tid: id}, rest[i+1:], true

	case strings.HasPrefix(svc, "host-serial:"):
		t, cmd := splitTarget(svc[len("host-serial:"):])
		return target{text: t}, cmd, true

	case strings.HasPrefix(svc, "host-usb:"):
		t, cmd := splitTarget(svc[len("host-usb:"):])
		return target{text: t, usbOnly: true}, cmd, true

	case strings.HasPrefix(svc, "host-local:"):
		t, cmd := splitTarget(svc[len("host-local:"):])
		return target{text: t}, cmd, true

	case strings.HasPrefix(svc, "host:"):
		return target{}, svc[len("host:"):], true
	}
	return target{}, "", false
}

// splitTarget peels a device address off the front of "<address>:<command>".
// A devpath and a TCP address both contain colons, so the leading form is
// recognised by shape before falling back to the first colon.
func splitTarget(rest string) (string, string) {
	if m := devpathTargetRE.FindString(rest); m != "" {
		return m[:len(m)-1], rest[len(m):]
	}
	if m := hostPortTargetRE.FindString(rest); m != "" {
		return m[:len(m)-1], rest[len(m):]
	}
	i := strings.Index(rest, ":")
	if i < 0 {
		// No address at all: "host-usb:transport-any" style, scoped by
		// transport type rather than by device.
		return "", rest
	}
	return rest[:i], rest[i+1:]
}
