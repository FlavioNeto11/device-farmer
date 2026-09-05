package adbwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrKind classifies a wire failure finely enough that a caller can decide
// whether to retry, how long to wait, and what to page about — using nothing
// but transport facts. There is deliberately no verdict here: this type says
// what the socket did, never what should be done about the work running on
// the other end of it.
type ErrKind uint8

const (
	// KindUnknown is an unclassified failure. Treat it as hostile.
	KindUnknown ErrKind = iota
	// KindDial means the ADB server endpoint could not be reached at all.
	// The phones are almost certainly fine; the server or the host is not.
	KindDial
	// KindWrite means a request could not be handed to the kernel.
	KindWrite
	// KindRead means a response could not be read, for a reason that is
	// neither a timeout nor a clean close.
	KindRead
	// KindTimeout means the deadline elapsed with the peer silent.
	KindTimeout
	// KindPeerClosed means the far side went away: EOF, ECONNRESET, EPIPE.
	// This is the shape of the failure in STF #663, and it is exactly as
	// inert here as every other kind: it names what the socket did and
	// stops there.
	KindPeerClosed
	// KindLocalClosed means our own side closed the socket, normally
	// because the caller's context was cancelled.
	KindLocalClosed
	// KindFrame means the peer is not speaking the host protocol: a length
	// prefix that is not hex, a status that is neither OKAY nor FAIL, a
	// truncated payload. The connection is desynchronised and unusable;
	// only a fresh one can recover.
	KindFrame
)

// String implements fmt.Stringer.
func (k ErrKind) String() string {
	switch k {
	case KindDial:
		return "dial"
	case KindWrite:
		return "write"
	case KindRead:
		return "read"
	case KindTimeout:
		return "timeout"
	case KindPeerClosed:
		return "peer_closed"
	case KindLocalClosed:
		return "local_closed"
	case KindFrame:
		return "frame"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Metrics.
//
// Incrementing one of these counters is the ONLY side effect any error in
// this package is permitted to have. The increments live inside the error
// constructors so that "a transport error bumps a counter and is returned"
// is structural rather than a rule someone has to remember.
//
// These are not registered here. A hosting package registers Collectors()
// against whichever registry it owns.
// ---------------------------------------------------------------------------

var (
	transportErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "transport_errors_total",
		Help:      "ADB server socket failures, by operation and kind. Diagnostic only; see the package doc for what this counter may and may not influence.",
	}, []string{"op", "kind"})

	protocolFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "protocol_failures_total",
		Help:      "FAIL responses from the ADB server, by operation and classified reason.",
	}, []string{"op", "class"})

	dialsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "dials_total",
		Help:      "Connections opened to the ADB server.",
	})

	streamsOpenedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "streams_opened_total",
		Help:      "Device-side service streams opened, by addressing mode.",
	}, []string{"addressing"})

	trackerReconnectsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "tracker_reconnects_total",
		Help:      "Reconnect attempts made by the track-devices reader.",
	})

	trackerSnapshotsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "tracker_snapshots_total",
		Help:      "Device-list snapshots received from the ADB server.",
	})

	admissionFramesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "device_farmer",
		Subsystem: "adbwire",
		Name:      "admission_frames_total",
		Help:      "Admission preambles announced to a host's proxy over TLS, by claimed class. Zero on a plain-TCP deployment.",
	}, []string{"class"})
)

// Collectors returns this package's metrics for registration by whichever
// package owns the process registry. Nothing here self-registers, so two
// clients in one process cannot panic on a duplicate registration.
func Collectors() []prometheus.Collector {
	// A CounterVec with no children exports nothing, so the classes this
	// package can name are pre-created: a plain-TCP deployment then shows the
	// series at zero rather than showing no series, which is the difference
	// between "off" and "not scraped". The class that carries a device token
	// is named by the package that owns it and appears on its first frame.
	for _, class := range []string{AdmissionClassMaintenance, AdmissionClassEnroll} {
		admissionFramesTotal.WithLabelValues(class)
	}
	return []prometheus.Collector{
		transportErrorsTotal,
		protocolFailuresTotal,
		dialsTotal,
		streamsOpenedTotal,
		trackerReconnectsTotal,
		trackerSnapshotsTotal,
		admissionFramesTotal,
	}
}

// ---------------------------------------------------------------------------
// (a) transport / socket failure
// ---------------------------------------------------------------------------

// TransportError is a failure of the wire itself: the ADB server could not be
// reached, or the connection to it broke. It carries everything a caller needs
// to classify a blip — which operation, which endpoint, which physical
// position, what the socket did, and when — and deliberately carries no
// judgement about the work running on the device.
type TransportError struct {
	// Op is the wire operation, e.g. "dial", "devices", "track", "read".
	Op string
	// Endpoint is the ADB server address the failure happened against.
	Endpoint string
	// Devpath is the USB tree position, when the connection had already
	// been switched to a device. Empty for host-level operations.
	Devpath string
	// Kind is the socket-level classification.
	Kind ErrKind
	// At is when the failure was observed, so a caller can measure how long
	// a run of failures has lasted without keeping its own bookkeeping.
	At time.Time
	// Err is the underlying error from the net package or the kernel.
	Err error
}

func newTransportError(op, endpoint, devpath string, kind ErrKind, err error) *TransportError {
	transportErrorsTotal.WithLabelValues(op, kind.String()).Inc()
	return &TransportError{
		Op:       op,
		Endpoint: endpoint,
		Devpath:  devpath,
		Kind:     kind,
		At:       time.Now(),
		Err:      err,
	}
}

// Error implements error.
func (e *TransportError) Error() string {
	var b strings.Builder
	b.WriteString("adbwire: ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Kind.String())
	if e.Endpoint != "" {
		b.WriteString(" endpoint=")
		b.WriteString(e.Endpoint)
	}
	if e.Devpath != "" {
		b.WriteString(" devpath=")
		b.WriteString(e.Devpath)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying socket error for errors.Is against
// syscall.ECONNRESET and friends.
func (e *TransportError) Unwrap() error { return e.Err }

// Timeout reports whether the deadline elapsed. It also satisfies the
// informal net.Error shape callers already reach for.
func (e *TransportError) Timeout() bool { return e.Kind == KindTimeout }

// PeerClosed reports whether the far side hung up. This is the #663 signal.
// It means the socket is gone. It does not mean the phone is gone, and it
// certainly does not mean the phone is available to somebody else.
func (e *TransportError) PeerClosed() bool { return e.Kind == KindPeerClosed }

// ---------------------------------------------------------------------------
// (b) protocol FAIL
// ---------------------------------------------------------------------------

// ProtocolError is a well-formed FAIL response: the connection worked and the
// ADB server refused, in its own words.
type ProtocolError struct {
	// Op is the wire operation.
	Op string
	// Service is the request that was refused, e.g. "host-usb:usb:3-1.4.2:get-state".
	Service string
	// Devpath is the intended physical position, when there was one.
	Devpath string
	// Reason is the server's own message, verbatim.
	Reason string
}

// Error implements error.
func (e *ProtocolError) Error() string {
	if e.Devpath != "" {
		return fmt.Sprintf("adbwire: %s: server refused %q for devpath=%s: %s",
			e.Op, e.Service, e.Devpath, e.Reason)
	}
	return fmt.Sprintf("adbwire: %s: server refused %q: %s", e.Op, e.Service, e.Reason)
}

// ---------------------------------------------------------------------------
// (c) device not found, and its dangerous cousin
// ---------------------------------------------------------------------------

// ErrNotFound matches any NotFoundError under errors.Is.
var ErrNotFound = errors.New("adbwire: target not present on the adb server")

// NotFoundError means the ADB server has no transport matching the target.
//
// It says nothing about whether the device exists, is booting, is wedged, or
// was simply not enumerated yet. A caller that wants to act on absence must
// establish that separately and over time; one lookup returning this error is
// a single observation, not a state.
type NotFoundError struct {
	// Target is the string that failed to match: a devpath, or a serial if
	// the caller used the unsafe path.
	Target string
	// BySerial records that the lookup was serial-addressed, so an operator
	// reading the log knows the answer may be ambiguous as well as absent.
	BySerial bool
	// Reason is the server's own message.
	Reason string
}

// Error implements error.
func (e *NotFoundError) Error() string {
	mode := "devpath"
	if e.BySerial {
		mode = "serial(unsafe)"
	}
	return fmt.Sprintf("adbwire: no transport for %s=%q: %s", mode, e.Target, e.Reason)
}

// Is lets errors.Is(err, ErrNotFound) work.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// ErrAmbiguousTarget matches any AmbiguousTargetError under errors.Is.
var ErrAmbiguousTarget = errors.New("adbwire: target matched more than one transport")

// AmbiguousTargetError means the target string matched several transports.
//
// This is the failure mode duplicate OEM serials produce, and it is the
// reason every position-addressed call in this package takes a devpath. A
// devpath is a position in the USB tree and cannot be ambiguous; a serial
// routinely is. Seeing this error from a devpath-addressed call means the ADB
// server's view of the USB topology is broken and the host needs a human.
type AmbiguousTargetError struct {
	// Target is the string that matched too much.
	Target string
	// BySerial records whether the caller used the unsafe path.
	BySerial bool
	// Reason is the server's own message.
	Reason string
}

// Error implements error.
func (e *AmbiguousTargetError) Error() string {
	return fmt.Sprintf("adbwire: target %q is ambiguous (by_serial=%t): %s",
		e.Target, e.BySerial, e.Reason)
}

// Is lets errors.Is(err, ErrAmbiguousTarget) work.
func (e *AmbiguousTargetError) Is(target error) bool { return target == ErrAmbiguousTarget }

// failToError turns a FAIL reason into the most specific typed error it
// supports, and counts it. The ADB server's refusals are free text, so this
// matches on substrings the upstream sources have used for years and falls
// back to a plain ProtocolError rather than guessing.
func failToError(op, service, target string, bySerial bool, reason string) error {
	low := strings.ToLower(reason)
	switch {
	case strings.Contains(low, "more than one"), strings.Contains(low, "ambiguous"):
		protocolFailuresTotal.WithLabelValues(op, "ambiguous").Inc()
		return &AmbiguousTargetError{Target: target, BySerial: bySerial, Reason: reason}
	case strings.Contains(low, "not found"),
		strings.Contains(low, "no devices"),
		strings.Contains(low, "no such device"),
		strings.Contains(low, "no device"):
		protocolFailuresTotal.WithLabelValues(op, "not_found").Inc()
		return &NotFoundError{Target: target, BySerial: bySerial, Reason: reason}
	default:
		protocolFailuresTotal.WithLabelValues(op, "refused").Inc()
		return &ProtocolError{Op: op, Service: service, Devpath: target, Reason: reason}
	}
}

// ---------------------------------------------------------------------------
// (d) context cancellation
// ---------------------------------------------------------------------------

// CanceledError means the caller's own context ended the operation. It is
// reported separately from TransportError because the two demand opposite
// responses: a cancelled call is the caller getting what it asked for, while
// a transport failure is the world misbehaving. Conflating them makes every
// shutdown look like a host outage in the metrics.
type CanceledError struct {
	// Op is the wire operation that was cut short.
	Op string
	// Err is context.Canceled or context.DeadlineExceeded.
	Err error
}

// Error implements error.
func (e *CanceledError) Error() string {
	return fmt.Sprintf("adbwire: %s: %v", e.Op, e.Err)
}

// Unwrap exposes context.Canceled / context.DeadlineExceeded to errors.Is.
func (e *CanceledError) Unwrap() error { return e.Err }

// contextError returns a *CanceledError when ctx is what actually stopped the
// operation, and nil otherwise. Cancellation is checked first everywhere in
// this package: cancelling a call closes the socket under the read, so the
// raw error would otherwise be indistinguishable from a genuine peer
// disappearance and would inflate the transport counters on every clean
// shutdown.
func contextError(ctx context.Context, op string) error {
	if ctx == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return &CanceledError{Op: op, Err: cerr}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Caller mistakes
// ---------------------------------------------------------------------------

// ErrInvalidDevpath matches any UsageError raised by devpath validation.
var ErrInvalidDevpath = errors.New("adbwire: invalid devpath")

// UsageError is a programming mistake caught before anything touched the
// wire: a malformed devpath, a request larger than the 4-hex-digit length
// prefix can describe, a service string containing a NUL.
//
// Devpath validation in particular is a safety check, not a nicety. The
// devpath is interpolated into a colon-delimited service string, so an
// unvalidated one could retarget the request at a different device.
type UsageError struct {
	// Op is the operation that was refused.
	Op string
	// Detail explains what was wrong with the input.
	Detail string
	// Value is the offending input, when it is safe and useful to echo.
	Value string
	// kind lets errors.Is match a sentinel.
	kind error
}

// Error implements error.
func (e *UsageError) Error() string {
	if e.Value != "" {
		return fmt.Sprintf("adbwire: %s: %s: %q", e.Op, e.Detail, e.Value)
	}
	return fmt.Sprintf("adbwire: %s: %s", e.Op, e.Detail)
}

// Is lets errors.Is match the sentinel this UsageError was built with.
func (e *UsageError) Is(target error) bool { return e.kind != nil && target == e.kind }

// ---------------------------------------------------------------------------
// Classification helpers
// ---------------------------------------------------------------------------

// AsTransport extracts a *TransportError from an error chain.
func AsTransport(err error) (*TransportError, bool) {
	var te *TransportError
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}

// IsTransport reports whether err is (or wraps) a socket-level failure.
//
// A true answer means: retry, reconnect, alert if it persists. It never means
// the device on the far end has become available to anyone else.
func IsTransport(err error) bool {
	_, ok := AsTransport(err)
	return ok
}

// IsProtocol reports whether the ADB server answered FAIL.
func IsProtocol(err error) bool {
	var pe *ProtocolError
	return errors.As(err, &pe)
}

// IsNotFound reports whether the target was absent from the ADB server.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsCanceled reports whether the CALLER'S context ended the operation.
//
// A *TransportError is never cancellation, however it unwraps. A dial bounded
// by the per-call fallback timeout comes back as a net error wrapping
// context.DeadlineExceeded, so a plain errors.Is would report an unreachable
// ADB server as an orderly shutdown and hide a host outage from the very check
// a caller uses to decide whether to alert.
func IsCanceled(err error) bool {
	var ce *CanceledError
	if errors.As(err, &ce) {
		return true
	}
	if IsTransport(err) {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classify maps a raw net/syscall error onto an ErrKind, falling back to def
// when nothing more specific applies.
//
// net.ErrClosed is checked before EOF because a cancelled call closes the
// socket from our side, and reporting that as a peer disappearance would be a
// lie told to the very metric an operator uses to judge host health.
func classify(err error, def ErrKind) ErrKind {
	switch {
	case err == nil:
		return KindUnknown
	case errors.Is(err, net.ErrClosed):
		return KindLocalClosed
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return KindPeerClosed
	case errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNABORTED),
		errors.Is(err, syscall.EPIPE):
		return KindPeerClosed
	case isWinsockReset(err):
		return KindPeerClosed
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return KindTimeout
	}
	return def
}

// Winsock reports a reset abort with WSA-prefixed codes that syscall.ECONNRESET
// and friends do not match: on Windows those portable names are placeholder
// values, and the real mapping lives in golang.org/x/sys/windows, which this
// module deliberately does not depend on.
//
// The numbers are a frozen part of the Winsock ABI, and they are far above the
// highest errno any Unix defines, so this cannot misfire on the Linux nodes.
// It matters because a hard RST — the STF #663 failure shape — would otherwise
// be filed as a generic read error on a Windows developer box, and the one
// counter an operator reads to tell "the hub is shedding sockets" from
// "something else went wrong" would be wrong on half the fleet's toolchain.
const (
	wsaeConnAborted = syscall.Errno(10053)
	wsaeConnReset   = syscall.Errno(10054)
	wsaeShutdown    = syscall.Errno(10058)
)

func isWinsockReset(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == wsaeConnReset || errno == wsaeConnAborted || errno == wsaeShutdown
}
