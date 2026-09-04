// The client half of the node API: a [recovery.HostRunner] that reaches a
// farmd-node agent over HTTP.
//
// # Why this exists
//
// internal/recovery runs in the control plane. Tiers 1, 2, 5 and 7 are ADB
// verbs it can perform from anywhere; tiers 3 and 4 need /dev/bus/usb and a
// hub, which exist only on the device host. With no HostRunner wired in,
// internal/recovery/adbactuator.go refuses those two rungs on every attempt,
// forever, and the ladder climbs past a repairable USB link straight to
// rebooting the phone. [Client] is what makes those rungs reachable on bare
// metal: the ladder holds it, the agent holds the hardware.
//
// # The three answers this client must keep apart
//
// An operator reads these differently and the ladder records them
// differently, so they are never folded together:
//
//	nil              the rung was performed on the hardware
//	IsRefused(err)   the agent declined; nothing was touched, retrying changes nothing
//	IsUnreachable    no answer arrived; nothing is known about the hardware
//	anything else    the agent tried and the device did not come back
//
// The distinction that matters most is the third against the fourth. "The
// agent is down" recorded as "the rung failed" teaches the recovery ladder
// that the hardware is unrecoverable, and the ladder answers an unrecoverable
// device by escalating — reboot, then adb server restart, then quarantine — on
// a phone that is very probably three hours into somebody's job and whose only
// actual problem is that a pod restarted.
//
// Because that fourth answer is the destructive one, this client claims it
// only where it can be shown. An agent writes its own reason into the reply
// body — see [OpResponse] — so a 5xx carrying this API's JSON is the agent
// saying it tried, and a 5xx carrying anything else was written by whatever
// stands between here and the agent. The same evidence rule already governs
// the other direction, where a 200 without that confirmation is not believed
// to be a completed rung either. Neither direction is a guess: an answer this
// client cannot attribute to the agent is reported as [ErrUnreachable], which
// asks an operator to look rather than asking the ladder to escalate.
//
// # A caller's own decision is not a verdict about a host
//
// The caller's context ending — an action budget in the ladder, a shutdown,
// an operator hitting cancel — is reported as exactly that, wrapping
// context.Canceled or context.DeadlineExceeded and wrapping NEITHER sentinel.
// It applies to the whole call and not merely to the HTTP round trip: a lookup
// abandoned before an address was even resolved learned nothing about the host,
// and saying otherwise would put a claim about hardware into the record on the
// strength of a decision made here.
//
// # Nothing here ends a lease
//
// Not a dial failure, not a timeout, not a 500, not a host that has vanished
// from farm.hosts. This client reports what happened and returns. A lease ends
// when the job says so, when a deadline a human wrote down elapses, or when a
// human takes it back — and none of those sentences mentions a socket.

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// The ladder holds this type through the interface, so a signature drift is a
// compile error here rather than a tier-3 refusal discovered at 3am.
var _ recovery.HostRunner = (*Client)(nil)

// Client defaults.
const (
	// DefaultResetTimeout bounds one tier-3 call, and DefaultPowerTimeout one
	// tier-4 call.
	//
	// Both are deliberately longer than the agent's own [Agent.opBudget],
	// which is CallTimeout + PowerOffSettle + PortReturnTimeout + OpGrace and
	// comes to 73 seconds on stock settings. The agent detaches a running
	// operation from the request socket precisely so a client hanging up
	// cannot abort a VBUS cycle mid-settle; a client deadline SHORTER than the
	// agent's budget would therefore produce timeouts for rungs that were
	// completing normally, which is the same lie by a different route. If the
	// agent's settle or return timeouts are raised on a rack, raise these too.
	DefaultResetTimeout = 90 * time.Second
	DefaultPowerTimeout = 3 * time.Minute

	// DefaultResolveTimeout bounds the farm.hosts read that turns a host id
	// into an endpoint, and — as a separate budget of the same length — the
	// health round trip that follows it. Separate on purpose: sharing one
	// budget lets a slow database eat the time the agent was going to be given
	// and then report the agent as unreachable, which is a claim about a host
	// derived from the state of Postgres.
	DefaultResolveTimeout = DefaultCallTimeout

	// DefaultDialRetries is the total number of attempts made when — and only
	// when — the failure happened in the dial phase. See [isDialFailure].
	DefaultDialRetries = 3

	// DefaultRetryBackoff is the pause before the second attempt; it doubles.
	// Two extra attempts at 500ms and 1s cover an agent being restarted
	// underneath a rung, which is the common case this retry exists for.
	DefaultRetryBackoff = 500 * time.Millisecond

	// maxNodeBody bounds how much of an answer is read. The agent's replies
	// are a few hundred bytes; anything larger is a proxy's error page, and
	// buffering a megabyte of HTML to put in a log line helps nobody.
	maxNodeBody = 64 << 10

	// pgUndefinedColumn is SQLSTATE 42703. See [DBResolver.NodeEndpoint].
	pgUndefinedColumn = "42703"
)

// EndpointResolver reports the base URL of a host's node agent —
// "http://10.4.2.7:8082", or a bare "10.4.2.7:8082".
//
// It is an interface because where a farm records that address is a deployment
// decision: [DBResolver] reads it from farm.hosts, [StaticResolver] carries it
// in configuration. Neither is asked to decide whether the rung is allowed;
// resolution answers where, and only the agent answers whether.
type EndpointResolver interface {
	NodeEndpoint(ctx context.Context, hostID string) (string, error)
}

// ClientConfig is the client's wiring. Resolver and Token are required.
type ClientConfig struct {
	Resolver EndpointResolver

	// Token is the bearer token every route requires, including health.
	Token string

	ResetTimeout   time.Duration
	PowerTimeout   time.Duration
	ResolveTimeout time.Duration

	// HTTPClient overrides the transport. Its own Timeout field is left alone:
	// a per-call deadline lives on the context so the two operations can carry
	// the very different budgets they actually need.
	//
	// It is copied rather than used directly, and the copy gets a CheckRedirect
	// that refuses to follow redirects unless this one already has a policy of
	// its own — see [refuseRedirect]. Supply a CheckRedirect here only if
	// something really must let the far end choose which host's ports get cut.
	HTTPClient *http.Client

	// DialRetries and RetryBackoff govern retries of dial-phase failures only.
	DialRetries  int
	RetryBackoff time.Duration

	Logger *slog.Logger
}

// Client calls a farmd-node agent's HTTP surface.
type Client struct {
	resolve      EndpointResolver
	token        string
	hc           *http.Client
	reset        time.Duration
	power        time.Duration
	resolveFor   time.Duration
	dialRetries  int
	retryBackoff time.Duration
	log          *slog.Logger
}

// NewClient validates cfg and returns a client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Resolver == nil {
		return nil, errors.New("node: ClientConfig.Resolver is required; without one this " +
			"client has no way to find the agent for a host, and a guessed address is a " +
			"power cycle aimed at whatever happens to be listening there")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("node: ClientConfig.Token is required; every node route " +
			"including health is authenticated, so a client without a token produces " +
			"nothing but 401s and would report a whole fleet as refusing tiers 3 and 4")
	}

	c := &Client{
		resolve:      cfg.Resolver,
		token:        cfg.Token,
		reset:        cfg.ResetTimeout,
		power:        cfg.PowerTimeout,
		resolveFor:   cfg.ResolveTimeout,
		dialRetries:  cfg.DialRetries,
		retryBackoff: cfg.RetryBackoff,
		log:          cfg.Logger,
	}
	// A copy, never the caller's own value: the redirect policy below is not
	// this client's to impose on an *http.Client somebody else also uses, and
	// mutating a shared one would change how an unrelated caller's requests
	// behave.
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		v := *cfg.HTTPClient
		hc = &v
	}
	if hc.CheckRedirect == nil {
		hc.CheckRedirect = refuseRedirect
	}
	c.hc = hc

	if c.reset <= 0 {
		c.reset = DefaultResetTimeout
	}
	if c.power <= 0 {
		c.power = DefaultPowerTimeout
	}
	if c.resolveFor <= 0 {
		c.resolveFor = DefaultResolveTimeout
	}
	if c.dialRetries <= 0 {
		c.dialRetries = DefaultDialRetries
	}
	if c.retryBackoff <= 0 {
		c.retryBackoff = DefaultRetryBackoff
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// refuseRedirect stops the far end from choosing where a hardware request goes.
//
// A 307 or 308 makes net/http replay the POST — body and all — at an address
// nobody recorded, and the body of that POST cuts power to a USB port. Which
// port that is depends entirely on which host answers, so an address this
// client was redirected to is an address it cannot reason about at all. The
// refusal names the destination so an operator can see what is doing the
// redirecting; the fix is to record the agent's real address rather than a
// front door that forwards to it.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("node: %w: the recorded endpoint redirects to %s, and this client "+
		"does not follow redirects on a request that can cut power to a port; record the "+
		"agent's own address for this host instead of one that forwards to it",
		ErrRefused, req.URL.Redacted())
}

// USBReset performs recovery tier 3 on a remote host: USBDEVFS_RESET on the
// device node behind devpath. The device keeps its lease, its fence and its
// job throughout; this re-enumerates a USB link and nothing else.
func (c *Client) USBReset(ctx context.Context, hostID, devpath string) error {
	return c.op(ctx, PathUSBReset, "USBDEVFS_RESET",
		OpRequest{HostID: hostID, Devpath: devpath}, c.reset)
}

// PortPower performs recovery tier 4 on a remote host: cut VBUS to the port
// behind devpath, wait, restore it.
//
// It authorises the disturbance of THAT device only. On a hub without per-port
// switching the agent refuses rather than taking the rest of the power domain
// down as a side effect; use [Client.PortPowerWithDomain] when the caller has
// genuinely checked policy for every device in the domain.
func (c *Client) PortPower(ctx context.Context, hostID, devpath string) error {
	return c.PortPowerWithDomain(ctx, hostID, devpath, nil)
}

// PortPowerWithDomain is PortPower with an explicit list of other devpaths the
// caller has checked and is willing to disturb. The agent still refuses if the
// domain holds anything that is neither the target nor acknowledged.
func (c *Client) PortPowerWithDomain(ctx context.Context, hostID, devpath string, acknowledged []string) error {
	return c.op(ctx, PathPortPower, "VBUS power cycle",
		OpRequest{HostID: hostID, Devpath: devpath, Acknowledged: acknowledged}, c.power)
}

// Health reads GET /node/v1/health.
//
// An agent that answers with a different host_id than the one asked for is a
// routing fault — a stale endpoint, a reused address, a rack re-cabled into a
// different agent — and is reported as a refusal rather than as health, because
// the next call down that address would be a power cycle on the wrong host.
func (c *Client) Health(ctx context.Context, hostID string) (HealthResponse, error) {
	var out HealthResponse
	if strings.TrimSpace(hostID) == "" {
		return out, fmt.Errorf("node: %w: health needs the host id it is asking about",
			ErrMalformedRequest)
	}

	// Resolution and the round trip get separate budgets of the same length.
	// Nested inside one, a slow farm.hosts read would spend the agent's share
	// of the clock and the agent would then be reported unreachable without
	// ever having been asked.
	base, err := c.endpoint(ctx, ctx, hostID)
	if err != nil {
		return out, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.resolveFor)
	defer cancel()

	resp, err := c.send(callCtx, http.MethodGet, base+PathHealth, nil)
	if err != nil {
		return out, c.transportError(ctx, callCtx, "health", hostID, "", base, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNodeBody))
	if readErr != nil {
		return out, c.answerCutShort(ctx, callCtx, "health", hostID, "", base,
			resp.StatusCode, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return out, c.statusError("health", hostID, "", base, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("node: %w: the agent at %s answered health with something "+
			"that is not this API's JSON: %w", ErrUnreachable, base, err)
	}
	if out.HostID != hostID {
		return out, fmt.Errorf("node: %w: the agent at %s speaks for host %q, but this "+
			"call was routed there for host %q; the recorded endpoint is stale and every "+
			"devpath sent down it would name a port on the wrong host",
			ErrRefused, base, out.HostID, hostID)
	}
	return out, nil
}

// op performs one hardware operation end to end.
func (c *Client) op(ctx context.Context, path, what string, req OpRequest, timeout time.Duration) error {
	// The wire always carries the host id. The in-process seam may leave it
	// empty because a HostRunner call reaches exactly the agent it was handed
	// to; a call that is about to cross a network may not, and an empty one is
	// refused here rather than sent for the agent to reject — a rung that
	// cannot be routed was not attempted, and must not be recorded as failed
	// hardware.
	if strings.TrimSpace(req.HostID) == "" {
		return fmt.Errorf("node: %w: %s for %s was asked of a remote agent without a host "+
			"id; the same devpath names a different physical port on every host, so there "+
			"is nowhere safe to send this", ErrRefused, what, req.Devpath)
	}
	// Refusing a devpath that is not a USB position before it is sent is not a
	// substitute for the agent's own check — the agent re-checks everything,
	// because the caller is the party that might be wrong — but it keeps a
	// serial that wandered in where a devpath belongs from ever reaching a
	// process that has root on the USB bus.
	if _, err := DevpathPosition(req.Devpath); err != nil {
		return err
	}
	// An acknowledgement the agent cannot match to a real port authorises
	// nothing. Left to travel, it produces a refusal naming a device the caller
	// believes it already cleared, which reads as a bug in the agent rather
	// than as a typo in the domain — so it is caught here, where the message
	// can name the entry.
	for _, ack := range req.Acknowledged {
		if _, err := DevpathPosition(ack); err != nil {
			return fmt.Errorf("node: %w: %s for %s names %q among the devices whose "+
				"disturbance the caller has authorised, and that is not a USB position, so "+
				"it would authorise nothing and the agent would refuse the cycle: %w",
				ErrRefused, what, req.Devpath, ack, err)
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base, err := c.endpoint(ctx, callCtx, req.HostID)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("node: encoding the %s request for %s: %w", what, req.Devpath, err)
	}

	c.log.Info("asking a host agent to perform a recovery rung",
		"what", what, "host", req.HostID, "devpath", req.Devpath,
		"agent", base, "budget", timeout)

	resp, err := c.send(callCtx, http.MethodPost, base+path, payload)
	if err != nil {
		return c.transportError(ctx, callCtx, what, req.HostID, req.Devpath, base, err)
	}
	defer resp.Body.Close()

	// An answer that stops arriving part way through is a transport failure and
	// nothing else. Reading it as an empty body would put this call on the
	// "unconfirmed 200" path below, whose message accuses the address of not
	// being an agent — sending an operator after a routing fault that does not
	// exist while the real event was a connection dying mid-reply.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNodeBody))
	if readErr != nil {
		return c.answerCutShort(ctx, callCtx, what, req.HostID, req.Devpath, base,
			resp.StatusCode, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return c.statusError(what, req.HostID, req.Devpath, base, resp.StatusCode, body)
	}

	// A 200 is not on its own evidence that a rung happened. A captive portal,
	// an SSO redirect that landed on a login page, a proxy answering for a
	// service that is down: all of them can produce one. The agent confirms the
	// operation in the body, and without that confirmation the honest answer is
	// that nothing is known — never a success this client cannot prove.
	var out OpResponse
	if err := json.Unmarshal(body, &out); err != nil || !out.OK {
		reason, _ := agentReason(body)
		return fmt.Errorf("node: %w: %s for %s on host %s got HTTP 200 from %s, but the "+
			"answer does not confirm the operation, so something other than a farmd-node "+
			"agent is very likely answering at that address; check what is listening there "+
			"before trusting any rung sent down it: %s",
			ErrUnreachable, what, req.Devpath, req.HostID, base, reason)
	}
	return nil
}

// endpoint resolves a host id to the base URL of its agent.
//
// callerCtx is the context the caller handed in; ctx is the one this call is
// running under, which is shorter. The two are separate arguments because a
// lookup that ends because the CALLER gave up must not be reported the way a
// lookup that ends because the database did — the first learned nothing and
// says so, the second is a genuine "nothing is known about this host".
func (c *Client) endpoint(callerCtx, ctx context.Context, hostID string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, c.resolveFor)
	defer cancel()

	raw, err := c.resolve.NodeEndpoint(rctx, hostID)
	if err != nil {
		if callerCtx.Err() != nil {
			return "", fmt.Errorf("node: finding the agent for host %s was cut short by the "+
				"caller before an address was resolved, so nothing was asked of that host and "+
				"nothing is claimed about it: %w", hostID, callerCtx.Err())
		}
		return "", err
	}
	return normalizeEndpoint(hostID, raw)
}

// send performs one HTTP round trip, retrying ONLY failures that prove nothing
// was sent.
//
// A power cycle is not idempotent: repeating one that may already be in flight
// takes a phone's port down a second time. A dial-phase failure is the one
// case where a retry is provably safe, because no bytes reached the agent —
// and it is also the common case worth surviving, since an agent being
// restarted under a rung would otherwise be recorded as an unreachable host
// and hand the ladder a reason to escalate.
func (c *Client) send(ctx context.Context, method, target string, payload []byte) (*http.Response, error) {
	backoff := c.retryBackoff
	var lastErr error

	for attempt := 1; ; attempt++ {
		var body io.Reader
		if payload != nil {
			// A fresh reader per attempt: the previous one is drained.
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, fmt.Errorf("node: %s %s: %w", method, target, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.hc.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt >= c.dialRetries || !isDialFailure(err) || ctx.Err() != nil {
			return nil, lastErr
		}
		c.log.Warn("a host agent did not accept a connection; retrying, because a dial "+
			"that never connected sent nothing and a second attempt cannot double an "+
			"operation", "agent", target, "attempt", attempt, "in", backoff, "err", err)

		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// isDialFailure reports whether err happened while opening the connection.
//
// net.OpError carries the phase in Op, and "dial" means the TCP connection was
// never established: the request bytes were not written and the agent has no
// record of them. Every other transport failure — a reset mid-request, a reply
// that never finished — leaves it genuinely unknown whether the hardware was
// touched, and those are reported rather than repeated.
func isDialFailure(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

// transportError classifies a failure that produced no HTTP answer.
func (c *Client) transportError(callerCtx, callCtx context.Context, what, hostID, devpath, base string, err error) error {
	where := subject(hostID, devpath)

	// This client's own redirect policy, not the network. It is already a
	// complete refusal with its own reason, and re-wrapping it as an
	// unreachable agent would replace "the recorded address forwards somewhere"
	// with "the host is down" — two different things to go and fix.
	if IsRefused(err) {
		return err
	}

	// The caller's own context ended first: an action budget in the ladder, or
	// a shutdown. That is the caller's decision and is reported as such — not
	// as an unreachable agent, which would be a claim about the host.
	if callerCtx.Err() != nil {
		return fmt.Errorf("node: %s for %s on host %s was cut short by the caller before "+
			"the agent at %s answered; the agent runs the operation on its own budget and "+
			"may well be doing it right now: %w", what, where, hostID, base, callerCtx.Err())
	}
	if callCtx.Err() != nil {
		return fmt.Errorf("node: %w: %s for %s on host %s got no answer from the agent at "+
			"%s within this client's budget; the agent detaches hardware work from the "+
			"request socket, so the port is very probably still being cycled and this is "+
			"not evidence of a failed rung: %w", ErrUnreachable, what, where, hostID, base,
			callCtx.Err())
	}

	c.log.Warn("a host agent could not be reached; the devices on that host keep their "+
		"leases and their jobs, and nothing about their hardware is known from this",
		"what", what, "host", hostID, "devpath", devpath, "agent", base, "err", err)
	return fmt.Errorf("node: %w: %s for %s on host %s never reached the agent at %s; check "+
		"that farmd-node is running and reachable there before treating this device as "+
		"unrepairable: %w", ErrUnreachable, what, where, hostID, base, err)
}

// answerCutShort classifies a reply that began to arrive and then stopped.
//
// Whether the rung ran is genuinely unknown here: the agent had already
// decided its answer, and only the delivery failed. Unknown is reported as
// [ErrUnreachable] — never as a failed rung, which is the answer that sends
// the ladder up a tier.
func (c *Client) answerCutShort(callerCtx, callCtx context.Context, what, hostID, devpath, base string, status int, err error) error {
	where := subject(hostID, devpath)

	if callerCtx.Err() != nil {
		return fmt.Errorf("node: %s for %s on host %s was cut short by the caller while the "+
			"agent at %s was still sending its answer (HTTP %d); the agent runs the operation "+
			"on its own budget and may well be doing it right now: %w",
			what, where, hostID, base, status, callerCtx.Err())
	}
	if callCtx.Err() != nil {
		return fmt.Errorf("node: %w: the answer to %s for %s on host %s (HTTP %d from %s) had "+
			"not finished arriving within this client's budget; the agent detaches hardware "+
			"work from the request socket, so this is not evidence of a failed rung: %w",
			ErrUnreachable, what, where, hostID, status, base, callCtx.Err())
	}

	c.log.Warn("a host agent's answer stopped arriving part way through; nothing about that "+
		"device's hardware is known from this, and its lease is untouched",
		"what", what, "host", hostID, "devpath", devpath, "agent", base,
		"status", status, "err", err)
	return fmt.Errorf("node: %w: the agent at %s answered %s for %s on host %s with HTTP %d "+
		"and then stopped sending; whether the rung ran is unknown, so read farmd-node's own "+
		"log on that host rather than escalating this device: %w",
		ErrUnreachable, base, what, where, hostID, status, err)
}

// statusError turns a non-200 answer into the error the ladder should record.
//
// It is the client half of the table in api.go, read back the other way, and
// its shape follows from one question: did THE AGENT write this answer?
//
//   - 4xx. Somebody understood the request and declined it. Nothing was
//     touched and repeating it unchanged gets the same answer, whether the
//     decliner was the agent (400, 401, 409, 501 in the table) or an auth
//     gateway in front of it. Every 4xx is therefore a refusal.
//   - 502, 503, 504. Gateway statuses. The agent never writes one — see
//     [StatusFor], which emits 200, 400, 401, 409, 500 or 501 and nothing else
//     — so these were written by something that could not reach the agent.
//     Nothing is known about the hardware.
//   - Any other 5xx. The agent's own answer IF the body is this API's JSON
//     carrying a reason, which is the only evidence available that the agent
//     wrote it. That, and only that, is the attempted-and-failed rung which
//     wraps neither sentinel and which the ladder answers by escalating. The
//     same status with a proxy's HTML in it is an intermediary talking, and
//     recording that as broken hardware is exactly how a pod restart gets a
//     healthy phone rebooted out from under a six-hour job.
func (c *Client) statusError(what, hostID, devpath, base string, status int, body []byte) error {
	where := subject(hostID, devpath)
	detail, fromAgent := agentReason(body)

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("node: %w: %w: the agent at %s answered 401 for %s on %s; the "+
			"rung was not attempted, and a token is out of step between this control plane "+
			"and that host — re-sync the node token on both sides, and expect every tier 3 "+
			"and tier 4 on this host to be refused until you do", ErrRefused, ErrUnauthorized,
			base, what, where)

	case http.StatusBadRequest:
		return fmt.Errorf("node: %w: %w: the agent at %s could not read the %s request for "+
			"%s: %s", ErrRefused, ErrMalformedRequest, base, what, where, detail)

	case http.StatusNotFound:
		// Something answered but does not serve this route. Treating it as a
		// refusal keeps a version skew, or an unrelated service on that port,
		// from being recorded as broken hardware.
		return fmt.Errorf("node: %w: the agent at %s does not serve %s; either it is an "+
			"older farmd-node than this control plane, or that address is not a node agent "+
			"at all — check the endpoint recorded for host %s: %s",
			ErrRefused, base, APIPrefix, hostID, detail)

	case http.StatusConflict:
		return fmt.Errorf("node: %w: the agent on host %s declined %s for %s: %s",
			ErrRefused, hostID, what, where, detail)

	case http.StatusNotImplemented:
		return fmt.Errorf("node: %w: %w: the agent on host %s cannot perform %s on this "+
			"platform: %s", ErrRefused, ErrNotSupported, hostID, what, detail)

	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return fmt.Errorf("node: %w: %s for %s on host %s got HTTP %d from %s, which is a "+
			"gateway's answer and one no farmd-node ever writes, so the request stopped in "+
			"front of the agent and the hardware was never touched; get farmd-node answering "+
			"on that host again rather than escalating this device: %s",
			ErrUnreachable, what, where, hostID, status, base, detail)
	}

	if status >= 400 && status < 500 {
		// An unenumerated 4xx: an auth proxy, a method the far end does not
		// route, a rate limiter. Something declined; nothing was attempted.
		return fmt.Errorf("node: %w: %s for %s on host %s was declined with HTTP %d by %s "+
			"without reaching a farmd-node route; check what is answering at that address "+
			"for host %s: %s", ErrRefused, what, where, hostID, status, base, hostID, detail)
	}
	if !fromAgent {
		return fmt.Errorf("node: %w: %s for %s on host %s got HTTP %d from %s, but the body "+
			"is not this API's JSON, so the answer was written by something between here and "+
			"the agent and says nothing about the hardware; check what is answering at that "+
			"address before treating this device as unrepairable: %s",
			ErrUnreachable, what, where, hostID, status, base, detail)
	}

	// The agent reached the hardware and the hardware did not come back. This
	// is the ONLY answer that should read as a failed rung, and it deliberately
	// wraps neither ErrRefused nor ErrUnreachable.
	return fmt.Errorf("node: %s for %s on host %s was attempted by the agent at %s and "+
		"failed (HTTP %d): %s", what, where, hostID, base, status, detail)
}

// subject names what a message is about: the port when there is one, and the
// host when the call was not about a port at all.
func subject(hostID, devpath string) string {
	if devpath == "" {
		return "host " + hostID
	}
	return devpath
}

// agentReason pulls the agent's own reason out of a reply, falling back to a
// bounded snippet of whatever did arrive. The reason is the sentence an
// operator actually reads at 3am; losing it to a decode error would leave them
// with a status code and nothing else.
//
// The second return says whether the reply was this API's JSON carrying a
// reason — that is, whether a farmd-node wrote it. [Client.statusError] turns
// on that answer, because a 5xx from the agent and a 5xx from a proxy standing
// where the agent should be are opposite verdicts about a device.
func agentReason(body []byte) (string, bool) {
	var out OpResponse
	if err := json.Unmarshal(body, &out); err == nil && out.Error != "" {
		return out.Error, true
	}
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		return "the agent sent no reason", false
	}
	if len(snippet) > 512 {
		snippet = snippet[:512] + "…"
	}
	return snippet, false
}

// normalizeEndpoint turns a recorded endpoint into a base URL.
//
// A bare "10.4.2.7:8082" is accepted and read as http, because that is how an
// operator writes a host:port and how farm.hosts.adb_endpoint is already
// spelled. Anything that cannot be parsed into a scheme and an authority is
// refused rather than guessed at: the next request down this URL cuts power to
// a port.
func normalizeEndpoint(hostID, raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("node: %w: host %q has no node agent endpoint recorded, so "+
			"recovery tiers 3 and 4 cannot be reached for it", ErrRefused, hostID)
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("node: %w: the node endpoint recorded for host %q (%q) is "+
			"not a URL: %w", ErrRefused, hostID, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("node: %w: the node endpoint recorded for host %q (%q) uses "+
			"scheme %q; the node API is http or https", ErrRefused, hostID, raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("node: %w: the node endpoint recorded for host %q (%q) names "+
			"no host", ErrRefused, hostID, raw)
	}
	// A query, a fragment or embedded credentials would all be dropped by the
	// rebuild below. Silently sending a hardware request to an address that is
	// not the one somebody wrote down is the failure this whole function
	// exists to prevent, so a value carrying any of them is refused instead.
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("node: %w: the node endpoint recorded for host %q (%q) carries "+
			"a query or fragment; the node API is addressed by scheme, host and an optional "+
			"proxy path prefix, and the rest cannot be honoured — record the base address "+
			"alone", ErrRefused, hostID, raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("node: %w: the node endpoint recorded for host %q carries "+
			"embedded credentials; the node API authenticates with the bearer token in "+
			"ClientConfig.Token, and credentials left in the address would be dropped here "+
			"and every rung on this host would answer 401", ErrRefused, hostID)
	}
	// A path is kept so an agent behind a reverse proxy prefix works, but the
	// trailing slash goes: the route constants begin with one.
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/"), nil
}

// ---------------------------------------------------------------------------
// Resolvers
// ---------------------------------------------------------------------------

// DBResolver reads a host's node endpoint from farm.hosts.
type DBResolver struct{ pool *pgxpool.Pool }

// NewDBResolver returns a resolver backed by pool.
func NewDBResolver(pool *pgxpool.Pool) *DBResolver { return &DBResolver{pool: pool} }

// NodeEndpoint reads farm.hosts.node_endpoint.
//
// The column is nullable and empty for every host in a farm that has no host
// agent, and that is a refusal rather than an error: "this farm has no agent
// on host X, so tiers 3 and 4 cannot be performed there" is exactly what
// internal/recovery already says when no HostRunner is wired at all, and it
// must not read as a failed rung.
//
// A missing column gets its own sentence. This resolver is ahead of the schema
// — the column arrives in a migration this package does not own — and an
// operator who sees "column does not exist" with no further explanation has to
// go and find out why; one that names the migration does not.
func (r *DBResolver) NodeEndpoint(ctx context.Context, hostID string) (string, error) {
	const q = `SELECT node_endpoint FROM farm.hosts WHERE id = $1`

	// Nullable, so a pointer: a host with no agent is the ordinary case in a
	// farm whose control plane performs every rung it can over ADB.
	var endpoint *string
	err := r.pool.QueryRow(ctx, q, hostID).Scan(&endpoint)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("node: %w: host %q is not in farm.hosts, so there is no "+
			"agent address to send a hardware rung to", ErrRefused, hostID)

	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedColumn {
			return "", fmt.Errorf("node: %w: farm.hosts has no node_endpoint column, so no "+
				"host agent can be addressed on this deployment; the migration that adds "+
				"it is what turns recovery tiers 3 and 4 on: %w", ErrRefused, err)
		}
		// Somebody cancelled this lookup. A query does not cancel itself, so
		// this is a decision made above — and a decision made here is not
		// evidence about a database, let alone about a host. Wrapping it in
		// ErrUnreachable would put "that host could not be reached" into the
		// record for a host nobody ever tried to reach.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return "", fmt.Errorf("node: reading the node endpoint for host %q was cancelled "+
				"before farm.hosts answered; nothing is claimed about that host: %w", hostID, err)
		}
		// The database being briefly unreachable says nothing about the host or
		// the device. Reported as a transport failure so the ladder retries
		// rather than concluding the hardware is beyond repair.
		return "", fmt.Errorf("node: %w: reading the node endpoint for host %q from "+
			"farm.hosts: %w; this is a database fault, not a verdict on that host — fix "+
			"the control plane's connection to Postgres", ErrUnreachable, hostID, err)
	}

	if endpoint == nil {
		return "", fmt.Errorf("node: %w: host %q has no node_endpoint recorded; this farm "+
			"has no agent on that host, and tiers 3 and 4 are refused there rather than "+
			"faked", ErrRefused, hostID)
	}
	return *endpoint, nil
}

// StaticResolver resolves endpoints from a map of host id to address.
//
// It is for the deployment that carries agent addresses in configuration
// rather than in the database — including every deployment made before the
// farm.hosts column exists — and for tests.
type StaticResolver map[string]string

// NodeEndpoint returns the configured address for hostID.
func (s StaticResolver) NodeEndpoint(_ context.Context, hostID string) (string, error) {
	if ep, ok := s[hostID]; ok {
		return ep, nil
	}
	return "", fmt.Errorf("node: %w: no node agent address is configured for host %q, so "+
		"recovery tiers 3 and 4 cannot be reached for it", ErrRefused, hostID)
}
