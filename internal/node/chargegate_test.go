package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testToken and testHost live in client_test.go; this file shares them so the
// two halves of the node surface are exercised against one identity.

// ---------------------------------------------------------------------------
// A fake hardware half
// ---------------------------------------------------------------------------

type chargeCall struct {
	port chargePort
	on   bool
	ack  []string
}

// fakeChargeOps stands in for uhubctl and sysfs. Every test in this file runs
// against it, because the whole point of the seam is that the bookkeeping —
// deadlines, renewals, refusals, the dead-man's switch — is testable on a
// laptop with no hub attached.
type fakeChargeOps struct {
	mu    sync.Mutex
	calls []chargeCall

	// resolve and set override the defaults when non-nil.
	resolve func(devpath string) (chargePort, error)
	set     func(p chargePort, on bool, ack []string) (bool, error)

	// ons receives every power-on, so a test can wait for the dead-man's
	// switch instead of sleeping for a guessed interval.
	ons chan chargePort
}

func newFakeChargeOps() *fakeChargeOps {
	return &fakeChargeOps{ons: make(chan chargePort, 32)}
}

func (f *fakeChargeOps) resolveChargePort(devpath string) (chargePort, error) {
	if f.resolve != nil {
		return f.resolve(devpath)
	}
	return testResolve(devpath)
}

func (f *fakeChargeOps) setChargePower(_ context.Context, p chargePort, on bool, ack []string, _ opsConfig) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, chargeCall{port: p, on: on, ack: append([]string(nil), ack...)})
	f.mu.Unlock()
	if on {
		select {
		case f.ons <- p:
		default:
		}
	}
	if f.set != nil {
		return f.set(p, on, ack)
	}
	return true, nil
}

func (f *fakeChargeOps) snapshot() []chargeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chargeCall(nil), f.calls...)
}

func (f *fakeChargeOps) count(on bool) int {
	n := 0
	for _, c := range f.snapshot() {
		if c.on == on {
			n++
		}
	}
	return n
}

// testResolve mirrors what hubLocation does on Linux, so the keys the gate
// bookkeeping uses in these tests are shaped like the real ones.
func testResolve(devpath string) (chargePort, error) {
	p := strings.TrimPrefix(strings.TrimSpace(devpath), "usb:")
	i := strings.LastIndexAny(p, ".-")
	if i <= 0 {
		return chargePort{}, fmt.Errorf("node: %w: %q is not a USB position", ErrRefused, devpath)
	}
	port, err := strconv.Atoi(p[i+1:])
	if err != nil || port < 1 {
		return chargePort{}, fmt.Errorf("node: %w: %q does not end in a port number", ErrRefused, devpath)
	}
	return chargePort{Devpath: "usb:" + p, USBPath: p, Hub: p[:i], Port: port}, nil
}

// installChargeOps swaps the platform seam for the duration of one test. Tests
// in this file never call t.Parallel: the seam is a package variable, and a
// shared one swapped concurrently would be a race in the test harness rather
// than a finding about the code.
func installChargeOps(t *testing.T, f *fakeChargeOps) {
	t.Helper()
	prev := chargePlatform
	chargePlatform = f
	t.Cleanup(func() { chargePlatform = prev })
}

func newChargeGateAgent(t *testing.T, hostID string) *Agent {
	t.Helper()
	a, err := New(Config{
		Pool:   &pgxpool.Pool{}, // never used: nothing in this file touches the database
		HostID: hostID,
		Token:  testToken,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Gates live in a package-level registry keyed by agent. Release before
	// forgetting: releasing is what disarms the timers, and a timer left armed
	// would fire into the NEXT test's platform seam. This cleanup runs before
	// installChargeOps' own, so the fake is still in place here.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.ReleaseChargeGates(ctx, "test cleanup")
		forgetChargeGates(a)
	})
	return a
}

func newTestHandler(t *testing.T, a *Agent) http.Handler {
	t.Helper()
	h, err := a.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

// gateRequest posts to the charge-gate route. body may be a value to encode or
// a raw string for the malformed cases.
func gateRequest(t *testing.T, h http.Handler, method, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "/node/v1/charge-gate", rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeGate(t *testing.T, rec *httptest.ResponseRecorder) ChargeGate {
	t.Helper()
	var g ChargeGate
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode gate from %q: %v", rec.Body.String(), err)
	}
	return g
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// The set-point holds
// ---------------------------------------------------------------------------

func TestChargeGateHoldsThePortDark(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)

	before := time.Now()
	rec := gateRequest(t, h, http.MethodPost, testToken, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff,
		HoldSeconds: 60, Acknowledged: []string{"usb:3-1.5"}, Reason: "battery at 82%",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST charge-gate: status %d, body %s", rec.Code, rec.Body)
	}

	got := decodeGate(t, rec)
	if got.Power != ChargePowerOff || !got.Held {
		t.Errorf("gate reports power=%q held=%v, want off/held", got.Power, got.Held)
	}
	if got.HostID != testHost || got.Devpath != "usb:3-1.4" || got.Hub != "3-1" || got.Port != 4 {
		t.Errorf("gate addresses %+v, want host %s hub 3-1 port 4", got, testHost)
	}
	if got.ExpiresAt.Before(before.Add(59*time.Second)) || got.ExpiresAt.After(before.Add(75*time.Second)) {
		t.Errorf("deadline %s is not about 60s after %s", got.ExpiresAt, before)
	}
	if got.Reason != "battery at 82%" {
		t.Errorf("reason %q was not carried back", got.Reason)
	}

	calls := f.snapshot()
	if len(calls) != 1 || calls[0].on || calls[0].port.Hub != "3-1" || calls[0].port.Port != 4 {
		t.Fatalf("hardware calls %+v, want one power-off of hub 3-1 port 4", calls)
	}
	// The acknowledged list must reach the blast-radius check verbatim: it is
	// the caller's statement about which other devices may be disturbed.
	if len(calls[0].ack) != 1 || calls[0].ack[0] != "usb:3-1.5" {
		t.Errorf("acknowledged reached the platform as %v, want [usb:3-1.5]", calls[0].ack)
	}

	// And the gate is readable back, which is how a restarted control plane
	// finds out what it is responsible for.
	rec = gateRequest(t, h, http.MethodGet, testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET charge-gate: status %d, body %s", rec.Code, rec.Body)
	}
	var list chargeGateList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.HostID != testHost || len(list.Gates) != 1 || list.Gates[0].Devpath != "usb:3-1.4" {
		t.Fatalf("gate list %+v, want one gate on usb:3-1.4 for %s", list, testHost)
	}
}

func TestChargeGateDevpathIsCanonicalised(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)

	// The bare sysfs spelling and the adb spelling are the same port, so they
	// must be the same gate rather than two holds fighting over one switch.
	for _, devpath := range []string{"3-1.4", "usb:3-1.4"} {
		if _, err := a.SetChargeGate(context.Background(), ChargeGateRequest{
			HostID: testHost, Devpath: devpath, Power: ChargePowerOff, HoldSeconds: 60,
		}); err != nil {
			t.Fatalf("SetChargeGate(%q): %v", devpath, err)
		}
	}
	if gates := a.ChargeGates(); len(gates) != 1 || gates[0].Devpath != "usb:3-1.4" {
		t.Fatalf("gates %+v, want a single usb:3-1.4", gates)
	}
}

func TestChargeGateReleaseDropsTheHold(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	ctx := context.Background()

	if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	}); err != nil {
		t.Fatalf("assert gate: %v", err)
	}
	got, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOn,
	})
	if err != nil {
		t.Fatalf("release gate: %v", err)
	}
	if got.Power != ChargePowerOn || got.Held {
		t.Errorf("released gate reports power=%q held=%v, want on/not held", got.Power, got.Held)
	}
	if gates := a.ChargeGates(); len(gates) != 0 {
		t.Errorf("gates after release: %+v, want none", gates)
	}
	if f.count(true) != 1 {
		t.Errorf("power-on calls: %d, want 1", f.count(true))
	}
}

// ---------------------------------------------------------------------------
// The dead-man's switch
// ---------------------------------------------------------------------------

func TestChargeGateExpiryRestoresPower(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)

	if _, err := a.SetChargeGate(context.Background(), ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 0.15,
	}); err != nil {
		t.Fatalf("assert gate: %v", err)
	}

	select {
	case p := <-f.ons:
		if p.Hub != "3-1" || p.Port != 4 {
			t.Fatalf("the expiry restored %+v, want hub 3-1 port 4", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing restored power after the hold elapsed; a gate nobody renews " +
			"must not be able to hold a port dark forever")
	}
	waitFor(t, 5*time.Second, "the expired gate to be forgotten", func() bool {
		return len(a.ChargeGates()) == 0
	})
}

func TestChargeGateRenewalPostponesTheRestore(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	ctx := context.Background()

	first, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 0.4,
	})
	if err != nil {
		t.Fatalf("assert gate: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	second, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 30,
	})
	if err != nil {
		t.Fatalf("renew gate: %v", err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Errorf("renewal deadline %s did not move past %s", second.ExpiresAt, first.ExpiresAt)
	}

	// Well past the FIRST deadline, the port must still be dark: the timer the
	// renewal replaced must not be able to fire.
	time.Sleep(500 * time.Millisecond)
	if n := f.count(true); n != 0 {
		t.Fatalf("power was restored %d time(s) despite a renewal", n)
	}
	if gates := a.ChargeGates(); len(gates) != 1 || !gates[0].Held {
		t.Fatalf("gates after renewal: %+v, want one still held", gates)
	}
	// A renewal re-runs the whole hardware path, blast radius included, because
	// a phone may have been plugged into the domain since the last assertion.
	if n := f.count(false); n != 2 {
		t.Errorf("power-off calls: %d, want 2 (the renewal re-checks live sysfs)", n)
	}
}

func TestChargeGateLoopReleasesEverythingOnShutdown(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)

	for _, devpath := range []string{"usb:3-1.4", "usb:3-1.5"} {
		if _, err := a.SetChargeGate(context.Background(), ChargeGateRequest{
			HostID: testHost, Devpath: devpath, Power: ChargePowerOff, HoldSeconds: 600,
		}); err != nil {
			t.Fatalf("assert gate on %s: %v", devpath, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.chargeGateLoop(ctx) }()

	// The loop must do nothing at all while the agent is alive.
	time.Sleep(50 * time.Millisecond)
	if n := f.count(true); n != 0 {
		t.Fatalf("the gate loop restored power %d time(s) while the agent was running", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("chargeGateLoop returned %v; a failed restore is logged, not fatal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chargeGateLoop did not return after cancellation")
	}
	if n := f.count(true); n != 2 {
		t.Errorf("power-on calls on shutdown: %d, want 2 — a hold is only as alive as "+
			"the agent enforcing it", n)
	}
	if gates := a.ChargeGates(); len(gates) != 0 {
		t.Errorf("gates after shutdown: %+v, want none", gates)
	}
}

func TestChargeGateExpiryRetriesAFailedRestore(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)

	f.set = func(_ chargePort, on bool, _ []string) (bool, error) {
		if on {
			return true, errors.New("uhubctl: no response from hub")
		}
		return true, nil
	}

	if _, err := a.SetChargeGate(context.Background(), ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 0.1,
	}); err != nil {
		t.Fatalf("assert gate: %v", err)
	}

	// The restore fails, so the hold must NOT be dropped: dropping it would
	// disarm the only thing in this process still trying to give the port its
	// power back, and the port's state is no longer known.
	waitFor(t, 10*time.Second, "the failed restore to be reported", func() bool {
		return f.count(true) >= 1
	})
	waitFor(t, 10*time.Second, "the gate to be marked unknown", func() bool {
		gates := a.ChargeGates()
		return len(gates) == 1 && gates[0].Power == ChargePowerUnknown
	})
}

// The whole file fails towards power, so a RELEASE must not be refused over a
// field it ignores. A caller that reuses the struct it filled in for the
// off-gate and only flips Power would otherwise leave the port dark.
func TestChargeGateReleaseIgnoresTheHold(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	ctx := context.Background()

	if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	}); err != nil {
		t.Fatalf("assert gate: %v", err)
	}
	for _, hold := range []float64{0, 300, MaxChargeGateHold.Seconds() + 1, 1e300} {
		if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOn, HoldSeconds: hold,
		}); err != nil {
			t.Fatalf("release with hold_seconds=%v: %v", hold, err)
		}
	}
	if f.count(true) != 4 {
		t.Errorf("power-on calls: %d, want 4 — a release must never be refused over "+
			"hold_seconds", f.count(true))
	}
}

// Once the agent has begun handing its ports back, a new off-gate would install
// a hold whose only enforcer is a process that is about to exit.
func TestChargeGateRefusedOnceTheAgentIsStopping(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	ctx := context.Background()

	if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	}); err != nil {
		t.Fatalf("assert gate: %v", err)
	}

	stopping, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.chargeGateLoop(stopping); err != nil {
		t.Fatalf("chargeGateLoop: %v", err)
	}

	_, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("error %v for an off-gate after shutdown began, want a refusal", err)
	}
	if gates := a.ChargeGates(); len(gates) != 0 {
		t.Errorf("gates after a refused post-shutdown assertion: %+v", gates)
	}
	// A release is still welcome: it is the direction shutdown was going anyway.
	if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOn,
	}); err != nil {
		t.Errorf("release after shutdown began: %v", err)
	}
}

// Reading the gates must not conjure state for an agent that has none: the
// registry pins the agent, its pool and its logger for the life of the process.
func TestReadingGatesCreatesNoState(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)

	if gates := a.ChargeGates(); len(gates) != 0 {
		t.Fatalf("a fresh agent reports gates: %+v", gates)
	}
	a.ReleaseChargeGates(context.Background(), "nothing to do")

	chargeGatesMu.Lock()
	_, present := chargeGatesByAgent[a]
	chargeGatesMu.Unlock()
	if present {
		t.Error("a read-only call left state in the gate registry")
	}
}

// A recovery cycle ends with the port powered on. The gate must stop claiming a
// set-point it no longer controls rather than lying about it for half an hour.
func TestRecoveryCycleSupersedesTheGate(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	ctx := context.Background()

	assert := func() {
		t.Helper()
		if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
		}); err != nil {
			t.Fatalf("assert gate: %v", err)
		}
	}

	prev := platform
	t.Cleanup(func() { platform = prev })

	// A refused rung touched nothing, so it supersedes nothing.
	assert()
	platform = fakePlatform{portPowerErr: fmt.Errorf("node: %w: unacknowledged", ErrRefused)}
	if err := a.PortPower(ctx, testHost, "usb:3-1.4"); !errors.Is(err, ErrRefused) {
		t.Fatalf("PortPower: %v", err)
	}
	if gates := a.ChargeGates(); len(gates) != 1 {
		t.Fatalf("gates after a refused cycle: %+v, want the gate untouched", gates)
	}

	// A cycle that ran leaves the port powered, so the gate is dropped.
	platform = fakePlatform{}
	if err := a.PortPower(ctx, testHost, "usb:3-1.4"); err != nil {
		t.Fatalf("PortPower: %v", err)
	}
	if gates := a.ChargeGates(); len(gates) != 0 {
		t.Errorf("gates after a cycle: %+v, want none — tier 4 powered the port back on", gates)
	}
}

type fakePlatform struct{ portPowerErr, usbResetErr error }

func (fakePlatform) kernelRelease() (string, error) { return "6.8.0-45-generic", nil }

func (p fakePlatform) usbReset(context.Context, string, opsConfig) error { return p.usbResetErr }

func (p fakePlatform) portPower(context.Context, string, []string, opsConfig) error {
	return p.portPowerErr
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestChargeGateRefusesAnUnboundedHold(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)

	cases := []struct {
		name string
		req  ChargeGateRequest
	}{
		{"no hold at all", ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff}},
		{"negative hold", ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: -5}},
		{"past the cap", ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff,
			HoldSeconds: MaxChargeGateHold.Seconds() + 1}},
		{"absurd hold that would overflow a Duration", ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 1e300}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := gateRequest(t, h, http.MethodPost, testToken, tc.req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"refused":true`) {
				t.Errorf("body %s does not report a refusal", rec.Body)
			}
		})
	}
	if n := len(f.snapshot()); n != 0 {
		t.Errorf("the hardware was touched %d time(s) for requests that were refused", n)
	}
}

func TestChargeGateRefusesAnotherHostsDevpath(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)

	rec := gateRequest(t, h, http.MethodPost, testToken, ChargeGateRequest{
		HostID: "rack-b-host-09", Devpath: "usb:3-1.4",
		Power: ChargePowerOff, HoldSeconds: 60,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rack-b-host-09") ||
		!strings.Contains(rec.Body.String(), testHost) {
		t.Errorf("the refusal names neither rack: %s", rec.Body)
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("a misrouted request reached the hardware %d time(s)", n)
	}
}

// A refusal on RENEWAL is the dangerous case: the port is already dark, and a
// gate dropped here would take its deadline with it and leave nothing in the
// process that ever turns the port back on.
func TestChargeGateRefusedRenewalKeepsTheExistingHold(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	ctx := context.Background()

	first, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	})
	if err != nil {
		t.Fatalf("assert gate: %v", err)
	}

	// A phone appeared in the ganged domain since the last assertion.
	f.set = func(_ chargePort, _ bool, _ []string) (bool, error) {
		return false, fmt.Errorf("node: %w: one device nobody authorised", ErrRefused)
	}
	_, err = a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("renewal error %v, want a refusal", err)
	}

	gates := a.ChargeGates()
	if len(gates) != 1 || !gates[0].Held {
		t.Fatalf("gates after a refused renewal: %+v, want the original still held", gates)
	}
	if !gates[0].ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("the deadline moved to %s from %s; a refusal touched no hardware and "+
			"must not move the clock either", gates[0].ExpiresAt, first.ExpiresAt)
	}
}

// When the platform DID switch something and then failed, its own guard has
// already put power back, so the agent must stop claiming to hold the port.
func TestChargeGateHardwareFailureDropsTheHold(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)
	ctx := context.Background()

	if _, err := a.SetChargeGate(ctx, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	}); err != nil {
		t.Fatalf("assert gate: %v", err)
	}

	f.set = func(_ chargePort, _ bool, _ []string) (bool, error) {
		return true, errors.New("uhubctl -a off for hub 3-1 failed: exit status 1")
	}
	rec := gateRequest(t, h, http.MethodPost, testToken, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 for hardware that was attempted and failed: %s",
			rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"refused":true`) {
		t.Errorf("a hardware failure was reported as a refusal: %s", rec.Body)
	}
	if gates := a.ChargeGates(); len(gates) != 0 {
		t.Errorf("gates after a failed switch: %+v, want none — the platform guard has "+
			"already restored power", gates)
	}
}

func TestChargeGateUnsupportedIsNotImplemented(t *testing.T) {
	f := newFakeChargeOps()
	f.resolve = func(string) (chargePort, error) {
		return chargePort{}, fmt.Errorf("node: %w: no uhubctl here", ErrNotSupported)
	}
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)

	rec := gateRequest(t, h, http.MethodPost, testToken, ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 60,
	})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501: %s", rec.Code, rec.Body)
	}
}

// The portable fallback is what a Windows or macOS build answers with, and it
// has to be a refusal that names the platform rather than a silent success.
func TestUnsupportedChargeGateRefusesEverywhere(t *testing.T) {
	var ops chargeGateOps = unsupportedChargeGate{}

	if _, err := ops.resolveChargePort("usb:3-1.4"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("resolveChargePort error %v, want ErrNotSupported", err)
	}
	touched, err := ops.setChargePower(context.Background(), chargePort{Hub: "3-1", Port: 4},
		false, nil, opsConfig{})
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("setChargePower error %v, want ErrNotSupported", err)
	}
	if touched {
		t.Error("the unsupported platform claims to have touched hardware")
	}
	if !strings.Contains(err.Error(), "uhubctl") {
		t.Errorf("the refusal %q does not name what is missing", err)
	}
}

// ---------------------------------------------------------------------------
// The HTTP surface
// ---------------------------------------------------------------------------

func TestChargeGateRequiresTheBearerToken(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)

	body := ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 60,
	}
	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "not-the-token"},
		{"the right token with a typo", testToken + "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := gateRequest(t, h, http.MethodPost, tc.token, body); rec.Code != http.StatusUnauthorized {
				t.Errorf("POST status %d, want 401", rec.Code)
			}
			// Reading which ports are dark is a map of which phones are
			// unreachable, so the list is behind the same token.
			if rec := gateRequest(t, h, http.MethodGet, tc.token, nil); rec.Code != http.StatusUnauthorized {
				t.Errorf("GET status %d, want 401", rec.Code)
			}
		})
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("an unauthenticated request reached the hardware %d time(s)", n)
	}
}

func TestChargeGateRejectsMalformedRequests(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)
	h := newTestHandler(t, a)

	cases := []struct {
		name string
		body any
	}{
		{"not JSON at all", "{"},
		{"an unknown field", `{"host_id":"` + testHost + `","devpath":"usb:3-1.4",` +
			`"power":"off","hold_seconds":60,"force":true}`},
		{"no devpath", ChargeGateRequest{HostID: testHost, Power: ChargePowerOff, HoldSeconds: 60}},
		{"no host id", ChargeGateRequest{Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 60}},
		{"no power", ChargeGateRequest{HostID: testHost, Devpath: "usb:3-1.4", HoldSeconds: 60}},
		{"a power nobody defined", ChargeGateRequest{
			HostID: testHost, Devpath: "usb:3-1.4", Power: "cycle", HoldSeconds: 60}},
		{"a serial where a devpath belongs", ChargeGateRequest{
			HostID: testHost, Devpath: "", Power: ChargePowerOff, HoldSeconds: 60}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := gateRequest(t, h, http.MethodPost, testToken, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("a malformed request reached the hardware %d time(s)", n)
	}
}

// The endpoint's existing rule: once an operation is accepted it runs on the
// agent's budget, not on the request socket. A control plane that hangs up
// halfway must not leave a port half-switched.
func TestChargeGateSurvivesTheClientHangingUp(t *testing.T) {
	f := newFakeChargeOps()
	installChargeOps(t, f)
	a := newChargeGateAgent(t, testHost)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	f.set = func(_ chargePort, _ bool, _ []string) (bool, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return true, nil
	}

	srv := httptest.NewServer(newTestHandler(t, a))
	defer srv.Close()

	body, err := json.Marshal(ChargeGateRequest{
		HostID: testHost, Devpath: "usb:3-1.4", Power: ChargePowerOff, HoldSeconds: 300,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/node/v1/charge-gate", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)

	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		errc <- err
	}()

	<-started
	cancel() // the caller gives up mid-operation
	<-errc
	close(release)

	waitFor(t, 10*time.Second, "the gate to be established despite the hangup", func() bool {
		gates := a.ChargeGates()
		return len(gates) == 1 && gates[0].Held
	})
}

// ---------------------------------------------------------------------------
// The client
// ---------------------------------------------------------------------------
