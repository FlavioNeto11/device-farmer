package node

// The client half of the charge gate, tested against the real agent.
//
// These do not stub the far side. The gate's whole safety argument lives in
// the agent — the deadline it enforces from its own clock, the blast-radius
// check it makes against live sysfs, the restore it performs when nobody
// renews — so a client test that invents an obliging server proves only that
// two pieces of this file agree with each other.
//
// What is faked is one layer lower: chargePlatform, the hardware. Everything
// above it is the code that ships.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// liveGateClient starts a real Agent, serves its handler, and returns a client
// pointed at it.
func liveGateClient(t *testing.T) (*Client, *fakeChargeOps) {
	t.Helper()

	f := newFakeChargeOps()
	installChargeOps(t, f)

	a := newChargeGateAgent(t, testHost)
	srv := httptest.NewServer(newTestHandler(t, a))
	t.Cleanup(srv.Close)

	return newClient(t, srv.URL), f
}

// TestClientHoldsAndReleasesThroughTheRealAgent is the round trip that matters:
// the control plane asks for darkness, the agent grants it with a deadline of
// its own choosing, and the release comes back through the same verb.
//
// Falsify: have SetChargeGate send ChargePowerOn regardless of the request.
func TestClientHoldsAndReleasesThroughTheRealAgent(t *testing.T) {
	c, f := liveGateClient(t)
	ctx := context.Background()

	gate, err := c.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: testPath,
		Power: ChargePowerOff, HoldSeconds: 30,
		Reason: "battery above the ceiling",
	})
	if err != nil {
		t.Fatalf("SetChargeGate: %v", err)
	}
	if gate.Power != ChargePowerOff || !gate.Held {
		t.Fatalf("gate = %+v, want an off-gate that is held", gate)
	}
	if gate.ExpiresAt.IsZero() {
		t.Fatal("an off-gate came back with no deadline; nothing would ever restore this port")
	}
	if f.count(false) != 1 {
		t.Fatalf("the hardware saw %d power-off call(s), want exactly 1: %+v",
			f.count(false), f.snapshot())
	}

	// Listing reads the AGENT's state, which is the only place that knows what
	// survived contact with the hardware.
	gates, err := c.ChargeGates(ctx, testHost)
	if err != nil {
		t.Fatalf("ChargeGates: %v", err)
	}
	if len(gates) != 1 || gates[0].Devpath != testPath {
		t.Fatalf("gates = %+v, want exactly the one just asserted", gates)
	}

	if _, err := c.ReleaseChargeGate(ctx, testHost, testPath, "policy cleared"); err != nil {
		t.Fatalf("ReleaseChargeGate: %v", err)
	}
	if f.count(true) != 1 {
		t.Fatalf("the hardware saw %d power-on call(s) after a release, want exactly 1: %+v",
			f.count(true), f.snapshot())
	}
	if gates, err := c.ChargeGates(ctx, testHost); err != nil || len(gates) != 0 {
		t.Fatalf("gates = %+v, err = %v; a released gate must not still be held", gates, err)
	}
}

// TestAMalformedGateNeverLeavesTheProcess. The same devpath names a different
// physical port on every host in the fleet, and this verb starves the port it
// reaches — so a request that cannot be routed must be refused here, not sent
// for the agent to reject. A gate rejected by the wrong agent has already told
// that agent about a port it does not own.
//
// Falsify: delete the three guards at the top of SetChargeGate.
func TestAMalformedGateNeverLeavesTheProcess(t *testing.T) {
	var reached int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached++
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL)

	for _, tc := range []struct {
		name string
		req  ChargeGateRequest
	}{
		{"no host", ChargeGateRequest{Devpath: testPath, Power: ChargePowerOff}},
		{"no devpath", ChargeGateRequest{HostID: testHost, Power: ChargePowerOff}},
		{"no power", ChargeGateRequest{HostID: testHost, Devpath: testPath}},
		{"a power value that is neither", ChargeGateRequest{
			HostID: testHost, Devpath: testPath, Power: "maybe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.SetChargeGate(context.Background(), tc.req); err == nil {
				t.Fatal("a malformed gate was accepted")
			} else if !errors.Is(err, ErrMalformedRequest) {
				t.Fatalf("err = %v, want ErrMalformedRequest", err)
			}
		})
	}
	if reached != 0 {
		t.Fatalf("%d malformed request(s) crossed the network", reached)
	}
	if _, err := c.ChargeGates(context.Background(), " "); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("listing without a host id: err = %v, want ErrMalformedRequest", err)
	}
}

// TestAnUnreachableAgentIsNotARefusedGate. The two answers send a policy loop
// in opposite directions: a refusal is the hardware saying no and no retry
// changes it; unreachable says nothing about the port at all. Folding them
// together either hammers a hub that will never comply, or abandons a hub that
// is fine and lets the battery charge to 100% because a pod restarted.
//
// Falsify: return a plain error from chargeGateCall's send branch instead of
// routing it through transportError.
func TestAnUnreachableAgentIsNotARefusedGate(t *testing.T) {
	// A listener that is closed before the call: the dial is refused
	// immediately rather than hanging.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	c := newClient(t, url)
	_, err := c.SetChargeGate(context.Background(), ChargeGateRequest{
		HostID: testHost, Devpath: testPath, Power: ChargePowerOff, HoldSeconds: 60,
	})
	if err == nil {
		t.Fatal("a gate against a dead agent reported success")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Fatalf("an unreachable agent was reported as a refusal: %v", err)
	}
}

// TestAGateFromTheWrongHostIsRefused. A stale endpoint means the next renewal
// holds somebody else's rack dark while this caller's own gate quietly expires
// and the phone it believes it is protecting charges to full.
//
// Falsify: drop the gate.HostID != hostID check in chargeGateCall.
func TestAGateFromTheWrongHostIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ChargeGate{
			HostID: "some-other-rack", Devpath: testPath,
			Power: ChargePowerOff, Held: true,
			ExpiresAt: time.Now().Add(time.Minute).UTC(),
		})
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL)
	_, err := c.SetChargeGate(context.Background(), ChargeGateRequest{
		HostID: testHost, Devpath: testPath, Power: ChargePowerOff, HoldSeconds: 60,
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "some-other-rack") {
		t.Errorf("the error does not name the host that answered: %v", err)
	}
}
