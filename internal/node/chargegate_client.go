package node

// The control-plane half of the charge gate.
//
// The agent half is chargegate.go, and the split matters: the gate is a
// CONTINUOUS assertion held by an agent that will release it on its own if
// nobody renews. These three methods are how the asserter speaks, so they are
// written to make the renewal cheap and the failure modes distinguishable.
//
// The distinction the error vocabulary has to preserve is the same one the
// recovery ladder needs, for the same reason: a REFUSED gate (this hub cannot
// switch one port, this kernel re-powers behind our back, this devpath belongs
// to a different host) is a fact about the hardware that no retry will change,
// while an UNREACHABLE agent is a fact about the network that says nothing at
// all about the port. A policy loop that folded them together would either
// keep hammering a hub that will never comply, or give up on a hub that is
// fine and let a battery charge to 100% because a pod restarted.
//
// Nothing here can hold a port dark by itself. Every off-gate carries a
// deadline the agent enforces from its own clock, and if this process stops
// calling, the port comes back on. That is the whole safety argument, and it
// lives on the far side of the wire on purpose.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PathChargeGate is the gate verb. POST sets or renews one, GET lists what the
// agent is currently holding.
const PathChargeGate = APIPrefix + "/charge-gate"

// SetChargeGate asserts a power set-point for one port and holds it there.
//
// An off-gate is a lease on darkness, not a command: the returned ChargeGate
// carries the deadline the AGENT will restore power at, and keeping the port
// dark past it means calling again before then. Callers should renew well
// inside the window rather than at its edge — a renewal that arrives late is
// not an error, it is a port that already came back on.
func (c *Client) SetChargeGate(ctx context.Context, req ChargeGateRequest) (ChargeGate, error) {
	var out ChargeGate

	// Refused here rather than sent for the agent to reject. The same devpath
	// names a different physical port on every host in the fleet, and this verb
	// STARVES the port it hits: a misrouted gate does not reset the wrong rack,
	// it flattens the wrong rack's batteries over the next several hours.
	if strings.TrimSpace(req.HostID) == "" {
		return out, fmt.Errorf("node: %w: a charge gate must name the host it is meant for",
			ErrMalformedRequest)
	}
	if strings.TrimSpace(req.Devpath) == "" {
		return out, fmt.Errorf("node: %w: a charge gate is addressed by devpath, never by serial",
			ErrMalformedRequest)
	}
	if req.Power != ChargePowerOn && req.Power != ChargePowerOff {
		return out, fmt.Errorf("node: %w: power must be %q or %q, not %q",
			ErrMalformedRequest, ChargePowerOff, ChargePowerOn, req.Power)
	}
	return c.chargeGateCall(ctx, http.MethodPost, "charge gate", req.HostID, req.Devpath, req, &out)
}

// ReleaseChargeGate ends a hold and returns the port to its default powered
// state, without waiting for the deadline to do it.
//
// It is an ordinary set-point of "on" rather than a verb of its own, because a
// release and a renewal must travel the same path: an agent that accepted one
// and not the other would leave a caller unable to undo what it had just done.
func (c *Client) ReleaseChargeGate(ctx context.Context, hostID, devpath, reason string) (ChargeGate, error) {
	return c.SetChargeGate(ctx, ChargeGateRequest{
		HostID:  hostID,
		Devpath: devpath,
		Power:   ChargePowerOn,
		Reason:  reason,
	})
}

// ChargeGates lists what this agent is holding right now.
//
// Read from the agent rather than from the database on purpose. The database
// records what the control plane asked for; only the agent knows what survived
// — a hold dropped because the hardware refused it, superseded by a recovery
// cycle, or already expired because nobody renewed. Reconciling the two is how
// a policy loop notices it has been asserting into the void.
func (c *Client) ChargeGates(ctx context.Context, hostID string) ([]ChargeGate, error) {
	if strings.TrimSpace(hostID) == "" {
		return nil, fmt.Errorf("node: %w: listing gates needs the host id it is asking about",
			ErrMalformedRequest)
	}
	// The agent answers with an envelope naming itself, not a bare array, and
	// the name is checked for the same reason Health checks it: a stale
	// endpoint means this listing describes a different rack, and a policy loop
	// reconciling against it would release holds it never placed.
	var out chargeGateList
	if _, err := c.chargeGateCall(ctx, http.MethodGet, "charge gates", hostID, "", nil, &out); err != nil {
		return nil, err
	}
	if out.HostID != hostID {
		return nil, fmt.Errorf("node: %w: the agent reached for host %q listed the gates of "+
			"host %q; the recorded endpoint is stale and this listing describes different "+
			"hardware", ErrRefused, hostID, out.HostID)
	}
	return out.Gates, nil
}

// chargeGateCall is the round trip both verbs share.
//
// The typed error vocabulary is the whole point of routing them through one
// place: transportError, answerCutShort and statusError are the same three the
// recovery rungs use, so a gate refusal and a rung refusal are the same kind of
// thing to a caller that has to decide whether to try again.
func (c *Client) chargeGateCall(ctx context.Context, method, what, hostID, devpath string,
	body any, out any) (ChargeGate, error) {

	var gate ChargeGate

	// Resolution and the round trip get separate budgets of the same length,
	// for the reason Health states: nested inside one, a slow farm.hosts read
	// would spend the agent's share of the clock and the agent would then be
	// reported unreachable without ever having been asked.
	base, err := c.endpoint(ctx, ctx, hostID)
	if err != nil {
		return gate, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.resolveFor)
	defer cancel()

	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return gate, fmt.Errorf("node: %w: %s request will not marshal: %w",
				ErrMalformedRequest, what, err)
		}
	}

	target := base + PathChargeGate
	if method == http.MethodGet {
		target += "?host_id=" + hostID
	}

	resp, err := c.send(callCtx, method, target, payload)
	if err != nil {
		return gate, c.transportError(ctx, callCtx, what, hostID, devpath, base, err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNodeBody))
	if readErr != nil {
		return gate, c.answerCutShort(ctx, callCtx, what, hostID, devpath, base,
			resp.StatusCode, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return gate, c.statusError(what, hostID, devpath, base, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return gate, fmt.Errorf("node: %w: the agent at %s answered %s with something that "+
			"is not this API's JSON: %w", ErrUnreachable, base, what, err)
	}

	if g, ok := out.(*ChargeGate); ok {
		gate = *g
		// A gate that came back naming a different host is the routing fault
		// Health refuses for, and it is worse here: the next call down this
		// address renews a hold on somebody else's rack, and the phone this
		// caller believes it is protecting charges to full while its own gate
		// silently expires.
		if gate.HostID != hostID {
			return ChargeGate{}, fmt.Errorf("node: %w: the agent at %s speaks for host %q, "+
				"but this gate was routed there for host %q; the recorded endpoint is stale "+
				"and this hold is on the wrong hardware",
				ErrRefused, base, gate.HostID, hostID)
		}
	}
	return gate, nil
}
