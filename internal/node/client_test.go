package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// A reference server for the contract in api.go
// ---------------------------------------------------------------------------

// nodeAPI answers the three node routes using only the exported pieces of the
// contract — Authorize, ValidateOp, StatusFor — so these tests exercise the
// agreement between the two halves rather than one implementation's habits.
// TestAgentServesTheContractRoutes drives the same client against the REAL
// [Agent.Handler] to keep this reference honest.
type nodeAPI struct {
	hostID string
	token  string

	// run decides what the hardware did. Nil means the rung worked.
	run func(OpRequest) error
	// block holds the response until the caller's request context ends, which
	// is what a client-side timeout against a long VBUS cycle looks like.
	block bool
	// status, when non-zero, is written instead of the contract's own mapping.
	// It exists for the answers only a broken agent or a proxy produces.
	status int
	body   string

	mu   sync.Mutex
	seen []OpRequest
}

func (s *nodeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathUSBReset, s.op)
	mux.HandleFunc("POST "+PathPortPower, s.op)
	mux.HandleFunc("GET "+PathHealth, s.health)
	return mux
}

func (s *nodeAPI) requests() []OpRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]OpRequest(nil), s.seen...)
}

func (s *nodeAPI) reply(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(OpResponse{OK: true})
		return
	}
	w.WriteHeader(StatusFor(err))
	_ = json.NewEncoder(w).Encode(OpResponse{
		Error: err.Error(), Refused: IsRefused(err), Reason: ReasonFor(err)})
}

func (s *nodeAPI) op(w http.ResponseWriter, r *http.Request) {
	if !Authorize(r, s.token) {
		s.reply(w, fmt.Errorf("node: %w", ErrUnauthorized))
		return
	}
	var req OpRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	// The real agent refuses unknown fields, so the reference does too: a
	// client that grows a field the agent has never heard of must fail here
	// and not in production.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.reply(w, fmt.Errorf("node: %w: %v", ErrMalformedRequest, err))
		return
	}

	s.mu.Lock()
	s.seen = append(s.seen, req)
	s.mu.Unlock()

	if err := ValidateOp(req, s.hostID); err != nil {
		s.reply(w, err)
		return
	}
	if s.block {
		<-r.Context().Done()
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.body)
		return
	}
	if s.run != nil {
		s.reply(w, s.run(req))
		return
	}
	s.reply(w, nil)
}

func (s *nodeAPI) health(w http.ResponseWriter, r *http.Request) {
	if !Authorize(r, s.token) {
		s.reply(w, fmt.Errorf("node: %w", ErrUnauthorized))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(HealthResponse{
		HostID: s.hostID, HostEpoch: 7, ADBServerUp: true, Platform: "linux/amd64"})
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

const (
	testHost  = "rack1-host-a"
	testToken = "s3cret-node-token"
	testPath  = "usb:3-1.4.2"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// serve starts the reference server and returns its base URL.
func serve(t *testing.T, s *nodeAPI) string {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// newClient builds a client that reaches endpoint for testHost. The retry
// timings are tiny because these tests care about the classification, not
// about how long a doomed dial is patient for.
func newClient(t *testing.T, endpoint string, tune ...func(*ClientConfig)) *Client {
	t.Helper()
	cfg := ClientConfig{
		Resolver:     StaticResolver{testHost: endpoint},
		Token:        testToken,
		DialRetries:  2,
		RetryBackoff: time.Millisecond,
		Logger:       quiet(),
	}
	for _, fn := range tune {
		fn(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// offlinePool is a pool that never connects. pgxpool.New does not dial —
// connections are created on first use and MinConns defaults to zero — so this
// gives the constructors the non-nil handle they require without a database.
// Every test that uses it exercises the HTTP surface only, which touches no
// SQL.
func offlinePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://farm@127.0.0.1:1/device_farmer?sslmode=disable")
	if err != nil {
		t.Fatalf("building an offline pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ---------------------------------------------------------------------------
// The six answers the ladder has to tell apart
// ---------------------------------------------------------------------------

func TestClientPerformsARungAndSaysSo(t *testing.T) {
	t.Parallel()

	var gotAuth, gotType, gotMethod, gotPath string
	api := &nodeAPI{hostID: testHost, token: testToken}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		gotMethod, gotPath = r.Method, r.URL.Path
		api.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	if err := newClient(t, srv.URL).USBReset(context.Background(), testHost, testPath); err != nil {
		t.Fatalf("USBReset: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != PathUSBReset {
		t.Errorf("tier 3 went to %s %s, want POST %s", gotMethod, gotPath, PathUSBReset)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("agent saw %d requests, want 1", len(reqs))
	}
	if reqs[0].HostID != testHost || reqs[0].Devpath != testPath {
		t.Errorf("agent saw %+v, want host %q devpath %q", reqs[0], testHost, testPath)
	}
}

func TestClientCarriesTheAcknowledgedPowerDomain(t *testing.T) {
	t.Parallel()

	// The agent is the last line on blast radius, so what the caller
	// authorised has to survive the wire intact: a domain member dropped in
	// transit is a phone taken down that nobody agreed to disturb.
	api := &nodeAPI{hostID: testHost, token: testToken}
	c := newClient(t, serve(t, api))
	domain := []string{"usb:3-1.4.1", "usb:3-1.4.3"}

	if err := c.PortPowerWithDomain(context.Background(), testHost, testPath, domain); err != nil {
		t.Fatalf("PortPowerWithDomain: %v", err)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("agent saw %d requests, want 1", len(reqs))
	}
	if strings.Join(reqs[0].Acknowledged, ",") != strings.Join(domain, ",") {
		t.Errorf("acknowledged domain arrived as %v, want %v", reqs[0].Acknowledged, domain)
	}

	// PortPower authorises the target alone, and the field must be absent
	// rather than empty so the agent's own check has nothing to misread.
	if err := c.PortPower(context.Background(), testHost, testPath); err != nil {
		t.Fatalf("PortPower: %v", err)
	}
	if ack := api.requests()[1].Acknowledged; len(ack) != 0 {
		t.Errorf("PortPower acknowledged %v; it authorises the target only", ack)
	}

	// A serial among the acknowledged devpaths authorises nothing: it matches
	// no port, so the agent counts that device as unacknowledged and refuses.
	// Caught here the message names the typo; left to travel it names a device
	// the caller is certain it already cleared.
	err := c.PortPowerWithDomain(context.Background(), testHost, testPath,
		[]string{"usb:3-1.4.1", "HT7A1B00123"})
	if !IsRefused(err) {
		t.Fatalf("a serial among the acknowledged devpaths must be refused; got %v", err)
	}
	if !strings.Contains(err.Error(), "HT7A1B00123") {
		t.Errorf("the refusal does not name the entry that authorises nothing: %v", err)
	}
	if len(api.requests()) != 2 {
		t.Errorf("a power cycle with an unusable acknowledgement was put on the wire")
	}
}

func TestClientReportsARefusalAsARefusal(t *testing.T) {
	t.Parallel()

	const reason = "cutting port 4 would darken usb:3-1.4.1, which nobody authorised"
	api := &nodeAPI{hostID: testHost, token: testToken,
		run: func(OpRequest) error { return fmt.Errorf("node: %w: %s", ErrRefused, reason) }}

	err := newClient(t, serve(t, api)).PortPower(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a refused power cycle returned nil; the ladder would record a rung that never happened")
	}
	if !IsRefused(err) {
		t.Errorf("IsRefused = false for %v", err)
	}
	if IsUnreachable(err) {
		t.Error("a refusal must not read as an unreachable agent: the agent answered")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("the agent's reason was lost; got %v", err)
	}
}

func TestClientReportsAnUnreachableAgentAsUnreachable(t *testing.T) {
	t.Parallel()

	// A listener that is gone: the dial is refused, so nothing was sent and
	// nothing is known about the hardware.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.URL
	dead.Close()

	err := newClient(t, addr).USBReset(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a dead agent returned nil")
	}
	if !IsUnreachable(err) {
		t.Errorf("IsUnreachable = false for %v", err)
	}
	if IsRefused(err) {
		t.Error("an unreachable agent is not a refusal: nobody declined anything")
	}
}

func TestClientTimeoutSaysTheHostIsProbablyStillWorking(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: testToken, block: true}
	c := newClient(t, serve(t, api), func(cfg *ClientConfig) {
		cfg.PowerTimeout = 150 * time.Millisecond
	})

	err := c.PortPower(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a client-side timeout returned nil")
	}
	if !IsUnreachable(err) || IsRefused(err) {
		t.Errorf("a timeout is a transport failure, not a refusal; got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the deadline is not visible in %v", err)
	}
	// The agent detaches hardware work from the request socket. An operator
	// reading this line must not conclude the port stayed dark.
	if !strings.Contains(err.Error(), "still being cycled") {
		t.Errorf("the timeout does not say the operation may still be running: %v", err)
	}
}

func TestClientCallerCancellationIsNotAVerdictOnTheHost(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: testToken, block: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	err := newClient(t, serve(t, api)).PortPower(ctx, testHost, testPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller should surface its own cancellation; got %v", err)
	}
	// The ladder maps a cancelled action onto "aborted". Calling it a refusal
	// or an unreachable host would be a claim about the far end that this
	// client never learned.
	if IsRefused(err) || IsUnreachable(err) {
		t.Errorf("cancellation was classified as a verdict about the agent: %v", err)
	}
}

func TestClientWrongTokenIsRefusedNotFailed(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: "the-agent-was-rotated-to-this"}
	err := newClient(t, serve(t, api)).USBReset(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a rejected token returned nil")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("ErrUnauthorized is not in %v", err)
	}
	// A token skew is a deployment fault. Recorded as a failed rung it would
	// push the ladder toward rebooting phones across the whole fleet.
	if !IsRefused(err) || IsUnreachable(err) {
		t.Errorf("a 401 must read as a refusal; got %v", err)
	}
	if len(api.requests()) != 0 {
		t.Error("the agent decoded a request it had not authenticated")
	}
}

func TestClientDevpathTheAgentRejects(t *testing.T) {
	t.Parallel()

	// The agent runs on one host, and "usb:3-1.4.2" is a real port on every
	// host in the fleet. A request that names another host is refused there,
	// not honoured against this rack's port.
	api := &nodeAPI{hostID: "rack9-host-z", token: testToken}
	c := newClient(t, serve(t, api))

	err := c.PortPower(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("the agent accepted a devpath belonging to another host")
	}
	if !IsRefused(err) || IsUnreachable(err) {
		t.Errorf("a misrouted devpath must be a refusal; got %v", err)
	}
	if !strings.Contains(err.Error(), "rack9-host-z") {
		t.Errorf("the refusal does not name the host that actually answered: %v", err)
	}
}

func TestClientRefusesANonPositionBeforeItReachesTheAgent(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: testToken}
	c := newClient(t, serve(t, api))

	// A serial names no port. Sending it to a process that has root on the USB
	// bus is not something to leave to the far end alone.
	err := c.USBReset(context.Background(), testHost, "HT7A1B00123")
	if !IsRefused(err) {
		t.Fatalf("a serial in a devpath field must be refused; got %v", err)
	}
	if len(api.requests()) != 0 {
		t.Error("a non-position devpath was put on the wire")
	}
}

func TestClientAgentFailureIsNeitherRefusalNorUnreachable(t *testing.T) {
	t.Parallel()

	// The one answer that means a rung was actually attempted and the device
	// did not come back. Only this may push the ladder to a harsher rung.
	api := &nodeAPI{hostID: testHost, token: testToken,
		run: func(OpRequest) error { return errors.New("node: the device did not re-enumerate") }}

	err := newClient(t, serve(t, api)).USBReset(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a failed rung returned nil")
	}
	if IsRefused(err) || IsUnreachable(err) {
		t.Errorf("an attempted-and-failed rung was misclassified: %v", err)
	}
	if !strings.Contains(err.Error(), "did not re-enumerate") {
		t.Errorf("the agent's reason was lost: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Retries, routing and resolution
// ---------------------------------------------------------------------------

// failFirstDial fails the first attempt the way a refused connection fails,
// then delegates. A dial that never connected sent nothing, which is the only
// case where retrying a power cycle cannot double it.
type failFirstDial struct {
	next     http.RoundTripper
	mu       sync.Mutex
	attempts int
}

func (f *failFirstDial) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.mu.Unlock()
	if n == 1 {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return f.next.RoundTrip(r)
}

func TestClientRetriesADialThatNeverConnected(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: testToken}
	tr := &failFirstDial{next: http.DefaultTransport}
	c := newClient(t, serve(t, api), func(cfg *ClientConfig) {
		cfg.HTTPClient = &http.Client{Transport: tr}
	})

	// An agent restarting underneath a rung must not be recorded as a host
	// that cannot be reached: the ladder answers that by escalating.
	if err := c.USBReset(context.Background(), testHost, testPath); err != nil {
		t.Fatalf("USBReset after one refused dial: %v", err)
	}
	if tr.attempts != 2 {
		t.Errorf("made %d attempts, want 2", tr.attempts)
	}
	if len(api.requests()) != 1 {
		t.Errorf("the agent saw %d requests; a retry must not double an operation",
			len(api.requests()))
	}
}

// failFirstRead fails the first attempt the way a connection that was already
// established dies: the request bytes may have been written, and the agent may
// already be part way through the operation.
type failFirstRead struct {
	next     http.RoundTripper
	mu       sync.Mutex
	attempts int
}

func (f *failFirstRead) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.mu.Unlock()
	if n == 1 {
		return nil, &net.OpError{Op: "read", Net: "tcp",
			Err: errors.New("connection reset by peer")}
	}
	return f.next.RoundTrip(r)
}

// TestClientNeverRepeatsAnOperationThatMayHaveBeenReceived is the other half of
// TestClientRetriesADialThatNeverConnected, and the more important half.
//
// A VBUS cycle is not idempotent. A failure that happened AFTER the connection
// was established leaves it unknown whether the agent got the request, so a
// second attempt can take a port down twice — the second time while the phone
// is part way back up, on a device that is very probably in the middle of
// somebody's job. Only a dial-phase failure proves nothing was sent, and only
// that one may be repeated.
//
// Without this test, widening [isDialFailure] to retry everything passes the
// entire suite.
func TestClientNeverRepeatsAnOperationThatMayHaveBeenReceived(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: testToken}
	tr := &failFirstRead{next: http.DefaultTransport}
	c := newClient(t, serve(t, api), func(cfg *ClientConfig) {
		cfg.HTTPClient = &http.Client{Transport: tr}
		cfg.DialRetries = 5
	})

	err := c.PortPower(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a power cycle that died mid-request was reported as performed")
	}
	if tr.attempts != 1 {
		t.Errorf("made %d attempts; a request that may already have reached the agent must "+
			"never be repeated, because the second one cuts the port again", tr.attempts)
	}
	// Nothing is known: the agent may have cycled the port, or may never have
	// seen the request. Either way this is not a failed rung.
	if !IsUnreachable(err) || IsRefused(err) {
		t.Errorf("a mid-request failure must read as an unreachable agent; got %v", err)
	}
}

// TestClientDoesNotFollowARedirect keeps the far end from choosing which host's
// ports get cut.
//
// net/http replays a POST body across a 307 or 308. That body is a power cycle,
// and the same devpath names a different physical port on every host, so an
// address arrived at by redirect is one this client cannot reason about at all.
func TestClientDoesNotFollowARedirect(t *testing.T) {
	t.Parallel()

	real := &nodeAPI{hostID: testHost, token: testToken}
	realURL := serve(t, real)

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, realURL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(front.Close)

	err := newClient(t, front.URL).PortPower(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a redirected power cycle was reported as performed")
	}
	if !IsRefused(err) || IsUnreachable(err) {
		t.Errorf("a redirecting endpoint is a recorded address to fix, not a dead host; "+
			"got %v", err)
	}
	if len(real.requests()) != 0 {
		t.Errorf("the power cycle was replayed at an address nobody recorded: %v",
			real.requests())
	}
}

// blockingResolver never answers, and then blames the host — which is exactly
// what a resolver reading a hung farm.hosts does if it looks no further than
// "the query returned an error". [EndpointResolver] is an interface a farm can
// implement for itself, so the client cannot assume the resolver got this
// right; it stands in front of one that got it wrong.
type blockingResolver struct{}

func (blockingResolver) NodeEndpoint(ctx context.Context, hostID string) (string, error) {
	<-ctx.Done()
	return "", fmt.Errorf("node: %w: reading the node endpoint for host %q: %w",
		ErrUnreachable, hostID, ctx.Err())
}

// TestClientCancellationDuringResolutionIsNotAVerdictOnTheHost extends the rule
// in TestClientCallerCancellationIsNotAVerdictOnTheHost to the half of the call
// that happens before any request goes out.
//
// A lookup the caller abandoned learned nothing about the host. Reported as
// [ErrUnreachable] it becomes "that host could not be reached" in the record —
// about a host nobody ever tried to reach — and the ladder reads it as a reason
// to stop trusting the hardware.
func TestClientCancellationDuringResolutionIsNotAVerdictOnTheHost(t *testing.T) {
	t.Parallel()

	c, err := NewClient(ClientConfig{
		Resolver: blockingResolver{}, Token: testToken, Logger: quiet()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	err = c.USBReset(ctx, testHost, testPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller should surface its own cancellation; got %v", err)
	}
	if IsRefused(err) || IsUnreachable(err) {
		t.Errorf("an abandoned lookup was classified as a verdict about the host: %v", err)
	}
}

func TestClientReadsHealthAndCatchesAStaleEndpoint(t *testing.T) {
	t.Parallel()

	api := &nodeAPI{hostID: testHost, token: testToken}
	url := serve(t, api)

	h, err := newClient(t, url).Health(context.Background(), testHost)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.HostID != testHost || h.HostEpoch != 7 || !h.ADBServerUp {
		t.Errorf("health decoded as %+v", h)
	}

	// The same agent, reached for a host it does not speak for: the recorded
	// endpoint is stale, and every devpath sent down it names a port on the
	// wrong machine.
	other, err := NewClient(ClientConfig{
		Resolver: StaticResolver{"rack9-host-z": url}, Token: testToken, Logger: quiet()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := other.Health(context.Background(), "rack9-host-z"); !IsRefused(err) {
		t.Fatalf("an agent answering for another host must be refused; got %v", err)
	}
}

func TestClientRefusesAHostWithNoAgent(t *testing.T) {
	t.Parallel()

	c, err := NewClient(ClientConfig{
		Resolver: StaticResolver{}, Token: testToken, Logger: quiet()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// "This farm has no agent on that host" is the same answer
	// internal/recovery gives when no HostRunner is wired at all. It is a
	// refusal, and above all it is not a failed rung.
	err = c.USBReset(context.Background(), testHost, testPath)
	if !IsRefused(err) || IsUnreachable(err) {
		t.Fatalf("an unconfigured host must be a refusal; got %v", err)
	}
}

// TestDBResolverTellsADatabaseFaultFromACancelledLookup pins the two ways a
// farm.hosts read can fail without producing an address.
//
// A database this control plane cannot reach says nothing about the host or the
// device, so it is [ErrUnreachable] — retry it, page whoever owns Postgres, and
// leave the phone alone. A lookup somebody cancelled says even less: a query
// does not cancel itself, so that error came from a decision made above, and
// dressing it up as an unreachable host writes a verdict into the record about
// a host nobody ever tried to reach.
func TestDBResolverTellsADatabaseFaultFromACancelledLookup(t *testing.T) {
	t.Parallel()

	r := NewDBResolver(offlinePool(t))

	// Nothing is listening on port 1. The pool dials on first use, so this is a
	// real connection failure against a real pgxpool.
	_, err := r.NodeEndpoint(context.Background(), testHost)
	if !IsUnreachable(err) || IsRefused(err) {
		t.Errorf("a database that cannot be reached must read as a transport failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "Postgres") {
		t.Errorf("the error does not tell an operator where to look: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.NodeEndpoint(ctx, testHost)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled lookup should surface its own cancellation; got %v", err)
	}
	if IsRefused(err) || IsUnreachable(err) {
		t.Errorf("a cancelled lookup was classified as a verdict about the host: %v", err)
	}
}

func TestNewClientDemandsWhatItCannotWorkWithout(t *testing.T) {
	t.Parallel()

	if _, err := NewClient(ClientConfig{Token: testToken}); err == nil {
		t.Error("a client without a resolver was accepted; it would have to guess addresses")
	}
	if _, err := NewClient(ClientConfig{Resolver: StaticResolver{}}); err == nil {
		t.Error("a client without a token was accepted; every node route is authenticated")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, raw, want string
		refused         bool
	}{
		{name: "bare host and port", raw: "10.4.2.7:8082", want: "http://10.4.2.7:8082"},
		{name: "explicit scheme", raw: "https://node-a:8443", want: "https://node-a:8443"},
		{name: "trailing slash", raw: "http://node-a:8082/", want: "http://node-a:8082"},
		{name: "proxy prefix", raw: "http://gw/hosts/a/", want: "http://gw/hosts/a"},
		{name: "empty", raw: "  ", refused: true},
		{name: "not http", raw: "ssh://node-a", refused: true},
		{name: "no authority", raw: "http:///node", refused: true},
		// Each of these would be silently dropped by the rebuild, which is how
		// a hardware request ends up at an address nobody wrote down.
		{name: "query", raw: "http://gw/hosts/a?tenant=b", refused: true},
		{name: "fragment", raw: "http://node-a:8082#frag", refused: true},
		{name: "embedded credentials", raw: "http://user:pw@node-a:8082", refused: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeEndpoint(testHost, tc.raw)
			if tc.refused {
				if !IsRefused(err) {
					t.Fatalf("normalizeEndpoint(%q) = %q, %v; want a refusal", tc.raw, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeEndpoint(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("normalizeEndpoint(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The contract against the real agent
// ---------------------------------------------------------------------------

func testAgent(t *testing.T, hostID string) *Agent {
	t.Helper()
	a, err := New(Config{
		Pool: offlinePool(t), HostID: hostID, Token: testToken, Logger: quiet()})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return a
}

// TestAgentServesTheContractRoutes drives the client against [Agent.Handler]
// itself. It is what keeps the path constants, the JSON tags and the status
// mapping in api.go from drifting away from the agent that has to honour them:
// a rename on either side turns up here rather than as a 404 that reads like a
// dead host.
func TestAgentServesTheContractRoutes(t *testing.T) {
	t.Parallel()

	h, err := testAgent(t, testHost).Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL)

	// Health proves GET /node/v1/health is routed and that HealthResponse
	// decodes what the agent actually writes.
	health, err := c.Health(context.Background(), testHost)
	if err != nil {
		t.Fatalf("Health against the real agent: %v", err)
	}
	if health.HostID != testHost {
		t.Errorf("health reported host %q, want %q", health.HostID, testHost)
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; health.Platform != want {
		t.Errorf("health reported platform %q, want %q", health.Platform, want)
	}

	// Both operation routes are reached, and the agent refuses a devpath
	// addressed to a different host — which is also proof the request body
	// decoded, since an unknown field would have been a 400 instead.
	other, err := NewClient(ClientConfig{
		Resolver: StaticResolver{"rack9-host-z": srv.URL}, Token: testToken, Logger: quiet()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, tc := range []struct {
		what string
		call func() error
	}{
		{"usb reset", func() error {
			return other.USBReset(context.Background(), "rack9-host-z", testPath)
		}},
		{"port power", func() error {
			return other.PortPower(context.Background(), "rack9-host-z", testPath)
		}},
	} {
		err := tc.call()
		if !IsRefused(err) || IsUnreachable(err) {
			t.Errorf("%s for another host: got %v, want a refusal", tc.what, err)
		}
		if !strings.Contains(err.Error(), testHost) {
			t.Errorf("%s refusal does not name the host that answered: %v", tc.what, err)
		}
	}
}

func TestClientTellsAWrongAddressFromADeadOne(t *testing.T) {
	t.Parallel()

	h, err := testAgent(t, testHost).Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Something answers, but it does not serve this API — a proxy prefix that
	// is wrong, an older agent, or an unrelated service on that port. A 404
	// must not be recorded as broken hardware.
	err = newClient(t, srv.URL+"/not-a-node").USBReset(context.Background(), testHost, testPath)
	if !IsRefused(err) || IsUnreachable(err) {
		t.Fatalf("a 404 from the far end: got %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), APIPrefix) {
		t.Errorf("the error does not name the API prefix that is missing: %v", err)
	}
}

// TestNewServesNoHTTPSurfaceWithoutAToken pins the deployment rule stated in
// api.go: an agent with no Addr is a legitimate deployment — discovery,
// enrollment and heartbeats still run, and the hardware rungs remain reachable
// in process — so it needs no credential. An agent that opens a port does,
// because those routes can cut power to ports holding live leases.
func TestNewServesNoHTTPSurfaceWithoutAToken(t *testing.T) {
	t.Parallel()

	pool := offlinePool(t)
	if _, err := New(Config{Pool: pool, HostID: testHost, Logger: quiet()}); err != nil {
		t.Fatalf("an agent that serves no endpoint must not need a token: %v", err)
	}
	if _, err := New(Config{Pool: pool, HostID: testHost, Addr: "127.0.0.1:0", Logger: quiet()}); err == nil {
		t.Fatal("an agent with an Addr and no token was accepted")
	}
	a, err := New(Config{Pool: pool, HostID: testHost, Logger: quiet()})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if _, err := a.Handler(); err == nil {
		t.Fatal("Handler built a mux with no token to check")
	}
}

// ---------------------------------------------------------------------------
// The contract's own guards
// ---------------------------------------------------------------------------

func TestValidateOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  OpRequest
		want error
	}{
		{"accepted", OpRequest{HostID: testHost, Devpath: testPath}, nil},
		{"no devpath", OpRequest{HostID: testHost}, ErrMalformedRequest},
		{"no host id", OpRequest{Devpath: testPath}, ErrMalformedRequest},
		{"a serial, not a position", OpRequest{HostID: testHost, Devpath: "HT7A1B00123"}, ErrRefused},
		{"traversal", OpRequest{HostID: testHost, Devpath: "usb:3-1/../../4-1"}, ErrRefused},
		{"another host", OpRequest{HostID: "rack9-host-z", Devpath: testPath}, ErrRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOp(tc.req, testHost)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateOp(%+v) = %v, want nil", tc.req, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateOp(%+v) = %v, want %v", tc.req, err, tc.want)
			}
			if got := StatusFor(err); got != StatusFor(tc.want) {
				t.Errorf("status %d for %v, want %d", got, err, StatusFor(tc.want))
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, header, want string
		ok                 bool
	}{
		{name: "correct", header: "Bearer " + testToken, want: testToken, ok: true},
		{name: "case insensitive scheme", header: "bearer " + testToken, want: testToken, ok: true},
		{name: "wrong token", header: "Bearer nope", want: testToken},
		{name: "no header", want: testToken},
		{name: "wrong scheme", header: "Basic " + testToken, want: testToken},
		{name: "raw token", header: testToken, want: testToken},
		// A server with no credential configured recognises nobody. Treating
		// an unset token as "anything goes" on routes that cut power to live
		// racks would be the worst possible default.
		{name: "no token configured", header: "Bearer " + testToken, want: ""},
		{name: "nothing at all", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost, PathUSBReset, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if got := Authorize(r, tc.want); got != tc.ok {
				t.Fatalf("Authorize(%q) = %v, want %v", tc.header, got, tc.ok)
			}
		})
	}
}

func TestClientDoesNotBelieveAnUnconfirmed200(t *testing.T) {
	t.Parallel()

	// A captive portal, an SSO login page, a proxy answering for a service
	// that is down. Reporting a rung as performed on the strength of a status
	// code the agent never wrote would be a success this client cannot prove.
	api := &nodeAPI{hostID: testHost, token: testToken,
		status: http.StatusOK, body: "<html>sign in to continue</html>"}

	err := newClient(t, serve(t, api)).USBReset(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("an unconfirmed 200 was reported as a completed rung")
	}
	if !IsUnreachable(err) || IsRefused(err) {
		t.Errorf("nothing was learned about the hardware here; got %v", err)
	}
}

// TestClientOnlyBelievesAFailedRungTheAgentSigned is the counterpart to
// TestClientDoesNotBelieveAnUnconfirmed200, in the direction that actually
// hurts.
//
// "The agent tried and the device did not come back" is the one answer the
// recovery ladder responds to by climbing — reboot, adb server restart,
// quarantine — so it is the one answer this client must be able to prove. The
// proof available is the reply body: [StatusFor] emits 200, 400, 401, 409, 500
// or 501 and nothing else, and every non-200 the agent writes carries its own
// reason as this API's JSON. A gateway status, or a 5xx with a proxy's HTML in
// it, is an intermediary standing where the agent should be, and a pod restart
// recorded as broken hardware is how a healthy phone gets rebooted out from
// under a six-hour job.
func TestClientOnlyBelievesAFailedRungTheAgentSigned(t *testing.T) {
	t.Parallel()

	agentJSON := `{"error":"the device did not re-enumerate after VBUS returned"}`

	tests := []struct {
		name        string
		status      int
		body        string
		want        string // the far end's own words, as an operator must see them
		unreachable bool   // nothing is known about the hardware
		refused     bool   // something declined; nothing was touched
	}{
		{name: "the agent's own 500", status: http.StatusInternalServerError,
			body: agentJSON, want: "did not re-enumerate"},
		{name: "a proxy's 500", status: http.StatusInternalServerError,
			body: "<html>internal error</html>", want: "internal error", unreachable: true},
		{name: "bad gateway", status: http.StatusBadGateway,
			body: "<html>502 upstream refused</html>", want: "502 upstream refused",
			unreachable: true},
		// A mesh with no healthy endpoint answers 503 in JSON of its own, which
		// this API's decoder happily reads. The status is what settles it: no
		// farmd-node ever writes one, so a body that looks like an agent's is
		// not enough to make this an attempted rung.
		{name: "service unavailable", status: http.StatusServiceUnavailable,
			body: `{"error":"no healthy upstream"}`, want: "no healthy upstream",
			unreachable: true},
		{name: "gateway timeout", status: http.StatusGatewayTimeout,
			body: "upstream timed out", want: "upstream timed out", unreachable: true},
		{name: "an auth gateway's 403", status: http.StatusForbidden,
			body: "<html>forbidden</html>", want: "forbidden", refused: true},
		{name: "a rate limiter", status: http.StatusTooManyRequests,
			body: "slow down", want: "slow down", refused: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &nodeAPI{hostID: testHost, token: testToken,
				status: tc.status, body: tc.body}
			err := newClient(t, serve(t, api)).PortPower(context.Background(), testHost, testPath)
			if err == nil {
				t.Fatalf("HTTP %d returned nil", tc.status)
			}
			if got := IsUnreachable(err); got != tc.unreachable {
				t.Errorf("IsUnreachable = %v, want %v, for %v", got, tc.unreachable, err)
			}
			if got := IsRefused(err); got != tc.refused {
				t.Errorf("IsRefused = %v, want %v, for %v", got, tc.refused, err)
			}
			// The reason is the sentence an operator reads at 3am. A body that
			// is not this API's JSON must still reach them, bounded and intact.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the far end's own words (%q) were dropped: %v", tc.want, err)
			}
		})
	}
}

// TestClientTruncatedAnswerIsNotAnAccusation covers the reply that begins to
// arrive and then stops — a pod killed mid-response, a load balancer draining.
//
// Read as an empty body this lands on the unconfirmed-200 path, whose message
// tells an operator that something other than a farmd-node is answering at that
// address. That sends them after a routing fault which does not exist, while
// the real event was a connection dying, and it is a claim this client cannot
// support either way.
func TestClientTruncatedAnswerIsNotAnAccusation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// A Content-Length that promises far more than what follows, and then
		// the socket goes away.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n" +
			"Content-Length: 4096\r\n\r\n{\"ok\":tr")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)

	err := newClient(t, srv.URL).USBReset(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a truncated answer was reported as a completed rung")
	}
	if !IsUnreachable(err) || IsRefused(err) {
		t.Errorf("a delivery that failed is not a verdict on the hardware; got %v", err)
	}
	if strings.Contains(err.Error(), "does not confirm the operation") {
		t.Errorf("a dying connection was reported as a wrong address: %v", err)
	}
	if !strings.Contains(err.Error(), "stopped sending") {
		t.Errorf("the error does not say the answer was cut off: %v", err)
	}
}
