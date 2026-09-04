package adbwire

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
	"github.com/prometheus/client_golang/prometheus"
)

// counterValue reads one child of a CounterVec.
//
// The counters in this package are process-global and the tests around them
// run in parallel, so every assertion built on this reads as "grew by at
// least": a counter only ever goes up, which makes the weaker claim the only
// one that is actually true under -race with parallel siblings.
//
// The child is gathered through a scratch registry rather than read with
// Counter.Write, because Write takes a *dto.Metric and naming that type would
// make github.com/prometheus/client_model a DIRECT requirement of this module.
// It is present only as an indirect one, so the import would leave go.mod out
// of sync with `go mod tidy` — and go.mod is not this package's to edit. Type
// inference carries the gathered protobuf values without naming them, so this
// route adds nothing to the module graph.
func counterValue(tb testing.TB, cv *prometheus.CounterVec, lvs ...string) float64 {
	tb.Helper()
	c, err := cv.GetMetricWithLabelValues(lvs...)
	if err != nil {
		tb.Fatalf("counter %v: %v", lvs, err)
	}
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		tb.Fatalf("registering counter %v for a read: %v", lvs, err)
	}
	families, err := reg.Gather()
	if err != nil {
		tb.Fatalf("gathering counter %v: %v", lvs, err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 {
		tb.Fatalf("counter %v gathered %d families; a single child must gather as exactly one metric", lvs, len(families))
	}
	return families[0].GetMetric()[0].GetCounter().GetValue()
}

func TestErrKindString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind ErrKind
		want string
	}{
		{KindUnknown, "unknown"},
		{KindDial, "dial"},
		{KindWrite, "write"},
		{KindRead, "read"},
		{KindTimeout, "timeout"},
		{KindPeerClosed, "peer_closed"},
		{KindLocalClosed, "local_closed"},
		{KindFrame, "frame"},
		{ErrKind(200), "unknown"},
	}
	seen := make(map[string]ErrKind, len(tests))
	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("ErrKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
		// The strings are metric label values. Two kinds sharing one would
		// silently merge two different host conditions into one time series.
		if prev, dup := seen[tc.want]; dup && prev != tc.kind && tc.want != "unknown" {
			t.Errorf("kinds %d and %d share the label %q", prev, tc.kind, tc.want)
		}
		seen[tc.want] = tc.kind
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		def  ErrKind
		want ErrKind
	}{
		{"nil", nil, KindRead, KindUnknown},
		// Checked before EOF: a cancelled call closes the socket from OUR
		// side, and filing that as a peer disappearance is a lie told to the
		// exact counter an operator reads to judge host health.
		{"our own close", net.ErrClosed, KindRead, KindLocalClosed},
		{"wrapped local close", fmt.Errorf("read: %w", net.ErrClosed), KindRead, KindLocalClosed},
		{"clean eof", io.EOF, KindRead, KindPeerClosed},
		{"truncated frame", io.ErrUnexpectedEOF, KindFrame, KindPeerClosed},
		{"econnreset", syscall.ECONNRESET, KindRead, KindPeerClosed},
		{"epipe", syscall.EPIPE, KindWrite, KindPeerClosed},
		{"econnaborted", syscall.ECONNABORTED, KindRead, KindPeerClosed},
		// syscall.ECONNRESET and friends are placeholder values on Windows, so
		// a real RST on a developer box arrives as a WSA errno instead. Without
		// this arm the #663 failure shape files itself as a generic read error
		// on half the fleet's toolchain.
		{"winsock reset", syscall.Errno(10054), KindRead, KindPeerClosed},
		{"winsock abort", syscall.Errno(10053), KindRead, KindPeerClosed},
		{"winsock shutdown", syscall.Errno(10058), KindRead, KindPeerClosed},
		{"deadline", os.ErrDeadlineExceeded, KindRead, KindTimeout},
		{"wrapped deadline", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, KindRead, KindTimeout},
		{"anything else falls back to the caller's default", errors.New("boom"), KindDial, KindDial},
		{"a plain unix errno is not a reset", syscall.Errno(2), KindRead, KindRead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(tc.err, tc.def); got != tc.want {
				t.Fatalf("classify(%v, %v) = %v, want %v", tc.err, tc.def, got, tc.want)
			}
		})
	}
}

func TestFailToErrorClassifiesTheServersOwnWords(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.2"

	tests := []struct {
		name     string
		reason   string
		target   string
		bySerial bool
		check    func(*testing.T, error)
	}{
		{
			name:     "duplicate serials are ambiguous, not missing",
			reason:   fakeadb.MsgAmbiguousTarget,
			target:   fakeadb.CloneSerial,
			bySerial: true,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrAmbiguousTarget) {
					t.Fatalf("%v does not match ErrAmbiguousTarget", err)
				}
				var ae *AmbiguousTargetError
				if !errors.As(err, &ae) {
					t.Fatalf("%T is not *AmbiguousTargetError", err)
				}
				if !ae.BySerial || ae.Target != fakeadb.CloneSerial {
					t.Fatalf("by_serial=%t target=%q — an operator reading the log must see the lookup was serial-addressed",
						ae.BySerial, ae.Target)
				}
			},
		},
		{
			name:   "more than one device/emulator",
			reason: fakeadb.MsgMultipleDevices,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrAmbiguousTarget) {
					t.Fatalf("%v does not match ErrAmbiguousTarget", err)
				}
			},
		},
		{
			name:   "named target absent",
			reason: fakeadb.MsgNotFound(devpath),
			target: devpath,
			check: func(t *testing.T, err error) {
				if !IsNotFound(err) {
					t.Fatalf("%v is not a not-found", err)
				}
				var nf *NotFoundError
				if !errors.As(err, &nf) || nf.Target != devpath || nf.BySerial {
					t.Fatalf("not-found recorded as %+v", nf)
				}
			},
		},
		{
			name:   "nothing attached at all",
			reason: fakeadb.MsgNoDevices,
			check: func(t *testing.T, err error) {
				if !IsNotFound(err) {
					t.Fatalf("%v is not a not-found", err)
				}
			},
		},
		{
			// A refusal this package does not recognise stays a refusal. It is
			// NOT guessed into absence: "the server said no" and "the device is
			// gone" lead an operator to different places.
			name:   "an offline device is a refusal, not an absence",
			reason: fakeadb.MsgDeviceOffline,
			target: devpath,
			check: func(t *testing.T, err error) {
				if IsNotFound(err) || errors.Is(err, ErrAmbiguousTarget) {
					t.Fatalf("%v was classified as absence or ambiguity", err)
				}
				var pe *ProtocolError
				if !errors.As(err, &pe) {
					t.Fatalf("%T is not *ProtocolError", err)
				}
				if pe.Reason != fakeadb.MsgDeviceOffline || pe.Devpath != devpath {
					t.Fatalf("protocol error lost the server's words: %+v", pe)
				}
				if !IsProtocol(err) || IsTransport(err) {
					t.Fatalf("a FAIL reply must be a protocol failure and never a transport one: %v", err)
				}
			},
		},
		{
			name:   "unauthorized",
			reason: fakeadb.MsgUnauthorized,
			target: devpath,
			check: func(t *testing.T, err error) {
				if !IsProtocol(err) {
					t.Fatalf("%v is not a protocol failure", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := counterValue(t, protocolFailuresTotal, "test", "ambiguous") +
				counterValue(t, protocolFailuresTotal, "test", "not_found") +
				counterValue(t, protocolFailuresTotal, "test", "refused")
			err := failToError("test", "host:svc", tc.target, tc.bySerial, tc.reason)
			if err == nil {
				t.Fatal("failToError returned nil for a FAIL reply")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error %q drops the server's own message %q", err, tc.reason)
			}
			tc.check(t, err)
			after := counterValue(t, protocolFailuresTotal, "test", "ambiguous") +
				counterValue(t, protocolFailuresTotal, "test", "not_found") +
				counterValue(t, protocolFailuresTotal, "test", "refused")
			if after < before+1 {
				t.Fatalf("protocol_failures_total went %v -> %v; a refusal must be counted", before, after)
			}
		})
	}
}

// readFailReasonFrom drives conn.readFailReason over a scripted byte string.
//
// The socket is a live pipe rather than a nil interface so the conn is well
// formed, but the bytes come from a strings.Reader: this path reads only the
// buffered reader, and giving it a source that ends in a real io.EOF is what
// lets the unframed arm — which reads to end of message — terminate without a
// helper goroutine that could outlive the test.
func readFailReasonFrom(tb testing.TB, wire string) string {
	tb.Helper()
	local, remote := net.Pipe()
	tb.Cleanup(func() {
		_ = local.Close()
		_ = remote.Close()
	})
	c := &conn{
		nc:       local,
		br:       bufio.NewReaderSize(strings.NewReader(wire), readBufSize),
		endpoint: "pipe",
	}
	return c.readFailReason()
}

// TestFailReasonIsNeverMasked covers the one function in this package that is
// allowed to swallow an error.
//
// readFailReason returns a best-effort string and no error on purpose: the
// refusal is carried by the FAIL status, and failing to read the diagnostic
// text after it must never turn a "the server said no" into a frame error
// against the host. Every ending below therefore has to produce something a
// human can read, and none of them may block or panic.
func TestFailReasonIsNeverMasked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wire string
		want string
	}{
		{"length-prefixed reason", "0005hello", "hello"},
		// Older servers write the text straight after FAIL with no length
		// prefix. The four bytes already consumed are the start of the message,
		// not a header, and dropping them would behead the reason.
		{"unframed reason from an older server", "device offline", "device offline"},
		{"nothing at all", "", "(no reason supplied)"},
		// The header promised sixteen bytes and the connection died after five.
		// What arrived is still the most useful thing anyone can say.
		{"truncated payload keeps what arrived", "0010short", "short"},
		{"truncated header is text, not a length", "ab", "ab"},
		// A well-formed empty reason. The refusal still stands; it is the FAIL
		// status, not this string, that carries it.
		{"well-formed empty reason", "0000", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := readFailReasonFrom(t, tc.wire); got != tc.want {
				t.Fatalf("readFailReason(%q) = %q, want %q", tc.wire, got, tc.want)
			}
		})
	}
}

// TestEmptyFailReasonStillClassifiesAsARefusal is the end-to-end half: a FAIL
// whose reason is empty must still reach the caller as a protocol refusal.
// Filing it as a transport failure would send an operator to look at the
// network for a host that answered perfectly well.
func TestEmptyFailReasonStillClassifiesAsARefusal(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t)
	cli := dialFake(t, srv)
	srv.FailNext("host:version", "")

	_, err := cli.Version(testContext(t))
	if !IsProtocol(err) {
		t.Fatalf("a FAIL with no reason = %v (%T), want a protocol failure", err, err)
	}
	if IsTransport(err) || IsCanceled(err) || IsNotFound(err) {
		t.Fatalf("a reasonless refusal was misclassified: %v", err)
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Service != "host:version" {
		t.Fatalf("protocol error recorded as %+v", pe)
	}
}

func TestNewTransportErrorCarriesPositionAndCounts(t *testing.T) {
	t.Parallel()

	const (
		op       = "test_transport_error"
		endpoint = "127.0.0.1:5037"
		devpath  = "usb:3-1.4.2"
	)
	before := counterValue(t, transportErrorsTotal, op, KindPeerClosed.String())

	start := time.Now()
	err := newTransportError(op, endpoint, devpath, KindPeerClosed, syscall.ECONNRESET)

	if !err.PeerClosed() {
		t.Error("PeerClosed() is false for KindPeerClosed")
	}
	if err.Timeout() {
		t.Error("Timeout() is true for KindPeerClosed")
	}
	if err.At.Before(start) {
		t.Errorf("At=%v predates the call at %v; a caller measuring how long a run of failures has lasted needs this", err.At, start)
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Error("the underlying socket error is not reachable through errors.Is")
	}
	for _, want := range []string{op, "peer_closed", endpoint, devpath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q omits %q", err, want)
		}
	}
	if after := counterValue(t, transportErrorsTotal, op, KindPeerClosed.String()); after < before+1 {
		t.Fatalf("transport_errors_total went %v -> %v; counting the blip is the only side effect this error may have", before, after)
	}
}

// TestIsCanceledDoesNotMistakeAnUnreachableHostForAShutdown pins the trap the
// helper exists for: a dial bounded by the per-call fallback comes back as a
// net error wrapping context.DeadlineExceeded, so a plain errors.Is would
// report a dead host as an orderly shutdown and suppress the alert.
func TestIsCanceledDoesNotMistakeAnUnreachableHostForAShutdown(t *testing.T) {
	t.Parallel()

	dialTimeout := newTransportError("dial", "10.0.0.1:5037", "", KindTimeout,
		fmt.Errorf("dial tcp: %w", context.DeadlineExceeded))
	if IsCanceled(dialTimeout) {
		t.Fatal("a transport failure wrapping context.DeadlineExceeded was reported as cancellation")
	}
	if !IsTransport(dialTimeout) {
		t.Fatal("a transport failure is not a transport failure")
	}

	cancelled := &CanceledError{Op: "version", Err: context.Canceled}
	if !IsCanceled(cancelled) {
		t.Fatal("a CanceledError is not reported as cancellation")
	}
	if IsTransport(cancelled) {
		t.Fatal("a CanceledError was reported as a transport failure; every clean shutdown would look like a host outage")
	}
	if !errors.Is(cancelled, context.Canceled) {
		t.Fatal("CanceledError does not unwrap to context.Canceled")
	}
	if !IsCanceled(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)) {
		t.Fatal("a bare deadline that is not a transport failure is not cancellation")
	}
	if IsCanceled(nil) {
		t.Fatal("nil is cancellation")
	}
}

func TestContextError(t *testing.T) {
	t.Parallel()

	if err := contextError(context.Background(), "op"); err != nil {
		t.Fatalf("a live context produced %v", err)
	}
	// Callers on this path are wire helpers, and one of them reaching here
	// with no context must not take the process down on the way to reporting
	// a socket failure.
	var missing context.Context
	if err := contextError(missing, "op"); err != nil {
		t.Fatalf("a nil context produced %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := contextError(ctx, "version")
	var ce *CanceledError
	if !errors.As(err, &ce) || ce.Op != "version" {
		t.Fatalf("contextError on a cancelled context = %v (%T)", err, err)
	}
	if !strings.Contains(ce.Error(), "version") {
		t.Fatalf("error %q does not name the operation that was cut short", ce)
	}
}

func TestNotFoundErrorNamesTheAddressingMode(t *testing.T) {
	t.Parallel()

	byPosition := &NotFoundError{Target: "usb:3-1.4.2", Reason: "device not found"}
	if !strings.Contains(byPosition.Error(), "devpath=") {
		t.Fatalf("%q does not say the lookup was position-addressed", byPosition)
	}
	bySerial := &NotFoundError{Target: fakeadb.CloneSerial, BySerial: true, Reason: "device not found"}
	if !strings.Contains(bySerial.Error(), "serial(unsafe)") {
		t.Fatalf("%q does not warn that the answer may be ambiguous as well as absent", bySerial)
	}
	if !errors.Is(bySerial, ErrNotFound) || !errors.Is(byPosition, ErrNotFound) {
		t.Fatal("NotFoundError does not match ErrNotFound")
	}
}

func TestUsageErrorMatchesOnlyItsOwnSentinel(t *testing.T) {
	t.Parallel()

	devpathErr := ValidateDevpath("usb:3-1.4.2:reboot")
	if !errors.Is(devpathErr, ErrInvalidDevpath) {
		t.Fatalf("%v does not match ErrInvalidDevpath", devpathErr)
	}
	if errors.Is(devpathErr, ErrNotFound) || errors.Is(devpathErr, ErrAmbiguousTarget) {
		t.Fatal("a malformed devpath matched an unrelated sentinel")
	}
	// A UsageError built without a sentinel must not match one by accident.
	plain := &UsageError{Op: "service", Detail: "empty service string"}
	if errors.Is(plain, ErrInvalidDevpath) {
		t.Fatal("a sentinel-less UsageError matched ErrInvalidDevpath")
	}
	if !strings.Contains(devpathErr.Error(), "usb:3-1.4.2:reboot") {
		t.Fatalf("%q does not echo the offending value", devpathErr)
	}
}

// TestCollectorsDoNotSelfRegister proves two clients in one process cannot
// panic on a duplicate registration: nothing here registers itself, so the same
// collectors go into two independent registries without complaint.
func TestCollectorsDoNotSelfRegister(t *testing.T) {
	t.Parallel()

	cs := Collectors()
	if len(cs) == 0 {
		t.Fatal("Collectors() is empty; the hosting package would register nothing")
	}
	for i := 0; i < 2; i++ {
		reg := prometheus.NewRegistry()
		for _, c := range cs {
			if err := reg.Register(c); err != nil {
				t.Fatalf("registry %d: %v", i, err)
			}
		}
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range families {
			if !strings.HasPrefix(f.GetName(), "device_farmer_adbwire_") {
				t.Errorf("collector %q is outside this package's namespace", f.GetName())
			}
		}
	}
}
