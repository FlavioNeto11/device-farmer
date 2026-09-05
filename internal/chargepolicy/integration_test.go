package chargepolicy

// The loop end to end: a scratch Postgres with the real farm.device_park and
// farm.device_unpark, the real node.Client resolving farm.hosts.node_endpoint
// through node.DBResolver, and an HTTP agent standing where `farmd node`
// would.
//
// The agent is a stub rather than the real node.Agent because the real one
// switches VBUS through a platform seam that is unexported and, off Linux,
// refuses every gate as unsupported — internal/node's own tests fake that seam
// from inside the package. What the stub keeps is the CONTRACT the client is
// written against: the bearer token, the host check, the hold cap, the ganged
// acknowledgement, the envelope the listing comes back in, and a gate that is
// reported held until an "on" releases it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/node"
)

const stubToken = "t0ken"

// ---------------------------------------------------------------------------
// The agent
// ---------------------------------------------------------------------------

type stubAgent struct {
	host string

	mu    sync.Mutex
	gates map[string]node.ChargeGate
	sets  []node.ChargeGateRequest
	// domain lists every position of one ganged domain: an off-gate on any
	// of them is refused unless every OTHER one is acknowledged, which is the
	// rule the real agent's blast-radius check enforces against live sysfs.
	domain []string
	// refuseOff makes every off-gate a 409, the way a hub without per-port
	// switching or a pre-6.0 kernel answers.
	refuseOff bool
}

func newStubAgent(host string) *stubAgent {
	return &stubAgent{host: host, gates: make(map[string]node.ChargeGate)}
}

func (s *stubAgent) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+stubToken {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorised"})
			return false
		}
		return true
	}
	refuse := func(w http.ResponseWriter, why string) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": why, "refused": true})
	}

	mux.HandleFunc("GET "+node.PathChargeGate, func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		s.mu.Lock()
		gates := make([]node.ChargeGate, 0, len(s.gates))
		for _, g := range s.gates {
			gates = append(gates, g)
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"host_id": s.host, "gates": gates})
	})

	mux.HandleFunc("POST "+node.PathChargeGate, func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var req node.ChargeGateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if req.HostID != s.host {
			refuse(w, "addressed to host "+req.HostID+" but this agent runs on "+s.host)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sets = append(s.sets, req)

		switch req.Power {
		case node.ChargePowerOn:
			delete(s.gates, req.Devpath)
			writeJSON(w, http.StatusOK, node.ChargeGate{
				HostID: s.host, Devpath: req.Devpath, Power: node.ChargePowerOn, Reason: req.Reason})
		case node.ChargePowerOff:
			if req.HoldSeconds <= 0 || req.HoldSeconds > node.MaxChargeGateHold.Seconds() {
				refuse(w, fmt.Sprintf("hold_seconds %v is outside (0, %s]", req.HoldSeconds, node.MaxChargeGateHold))
				return
			}
			if s.refuseOff {
				refuse(w, "this hub cannot switch one port")
				return
			}
			if slices.Contains(s.domain, req.Devpath) {
				for _, sib := range s.domain {
					if sib != req.Devpath && !slices.Contains(req.Acknowledged, sib) {
						refuse(w, "ganged: "+sib+" shares this port's power and was not acknowledged")
						return
					}
				}
			}
			g := node.ChargeGate{
				HostID: s.host, Devpath: req.Devpath, Power: node.ChargePowerOff, Held: true,
				ExpiresAt:    time.Now().Add(time.Duration(req.HoldSeconds * float64(time.Second))).UTC(),
				Reason:       req.Reason,
				Acknowledged: req.Acknowledged,
			}
			s.gates[req.Devpath] = g
			writeJSON(w, http.StatusOK, g)
		default:
			refuse(w, "power must be on or off")
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *stubAgent) held(devpath string) (node.ChargeGate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gates[devpath]
	return g, ok
}

// offSets returns every off-gate asserted on devpath, in order.
func (s *stubAgent) offSets(devpath string) []node.ChargeGateRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []node.ChargeGateRequest
	for _, r := range s.sets {
		if r.Devpath == devpath && r.Power == node.ChargePowerOff {
			out = append(out, r)
		}
	}
	return out
}

func (s *stubAgent) setCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sets)
}

// metricValue reads one counter or gauge through a scratch registry — the
// route internal/adbwire's tests take, and for the same reason: Metric.Write
// takes a *dto.Metric, and naming that type would make client_model a direct
// requirement of a module whose go.mod is not this package's to edit.
func metricValue(tb testing.TB, c prometheus.Collector) float64 {
	tb.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		tb.Fatalf("registering a metric for a read: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		tb.Fatalf("gathering a metric: %v", err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 {
		tb.Fatalf("gathered %d families; one metric must gather as exactly one", len(families))
	}
	m := families[0].GetMetric()[0]
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}

// ---------------------------------------------------------------------------
// Log capture
// ---------------------------------------------------------------------------

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.lines = append(s.lines, string(p))
	s.mu.Unlock()
	return len(p), nil
}

func (s *logSink) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (s *logSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	tag    string
	seq    int64
	hostID string
	poolID string
	domain int64
	agent  *stubAgent
	srv    *httptest.Server
	logs   *logSink
}

// newFixture empties the schema and seeds one host with one hub on one power
// domain of the given kind. withAgent records the stub's address as the
// host's node_endpoint; without it the host is what every simulated host is.
func newFixture(t *testing.T, kind string, withAgent bool) *fixture {
	t.Helper()
	pool := requireDB(t)
	resetSchema(t, pool)
	ctx := t.Context()

	seq := fixtureSeq.Add(1)
	tag := fmt.Sprintf("u8t%04d", seq)
	f := &fixture{
		t: t, pool: pool, tag: tag, seq: seq,
		hostID: "host-" + tag, poolID: "pool-" + tag,
		logs: &logSink{},
	}
	f.agent = newStubAgent(f.hostID)
	f.srv = httptest.NewServer(f.agent.handler())
	t.Cleanup(f.srv.Close)

	var endpoint *string
	if withAgent {
		endpoint = &f.srv.URL
	}
	control := "uhubctl"
	if kind == "none" {
		control = "none"
	}

	f.exec(`INSERT INTO farm.racks (id) VALUES ($1)`, "rack-"+tag)
	f.exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint, node_endpoint)
	        VALUES ($1, $2, '127.0.0.1:5037', $3)`, f.hostID, "rack-"+tag, endpoint)
	f.exec(`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3)`, f.hostID)
	if err := pool.QueryRow(ctx, `INSERT INTO farm.power_domains (host_id, kind, control)
	        VALUES ($1, $2, $3) RETURNING id`, f.hostID, kind, control).Scan(&f.domain); err != nil {
		t.Fatalf("seed power domain: %v", err)
	}
	f.exec(`INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
	        SELECT $1, c.id, '3-1', 8, true FROM farm.controllers c WHERE c.host_id = $1`, f.hostID)
	f.exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	f.exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, "tenant-"+tag)
	f.exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, "queue-"+tag, "tenant-"+tag)
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(), q, args...); err != nil {
		f.t.Fatalf("%v\nstatement: %s\nargs: %v", err, q, args)
	}
}

// device enrols a device on the fixture's hub at port, with a battery reading.
func (f *fixture) device(port, battery int) (id, devpath string) {
	f.t.Helper()
	err := f.pool.QueryRow(f.t.Context(), `
WITH s AS (
  INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path, topo_path, rack_slot)
  SELECT $1, h.id, $2, $3::int, '3-1.' || ($3::int)::text,
         ('x' || $4 || '.p' || ($3::int)::text)::ltree, 'R-' || $4 || '-P' || ($3::int)::text
    FROM farm.hubs h WHERE h.host_id = $1
  RETURNING id, adb_devpath
), d AS (
  INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id,
                            manufacturer, model, sdk_int)
  SELECT 'df-' || md5($4 || '/' || ($3::int)::text), 'SER-' || $4 || '-' || ($3::int)::text,
         $5, $1, s.id, 'Google', 'Pixel Test', 34
    FROM s
  RETURNING id, current_slot_id
), r AS (
  INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, battery_pct)
  SELECT d.id, $1, d.current_slot_id, 'device', 'healthy', $6::int
    FROM d
  RETURNING device_id
)
SELECT r.device_id::text, s.adb_devpath FROM r, s`,
		f.hostID, f.domain, port, f.tag, f.poolID, battery).Scan(&id, &devpath)
	if err != nil {
		f.t.Fatalf("enrol device on port %d: %v", port, err)
	}
	return id, devpath
}

func (f *fixture) setBattery(id string, pct int) {
	f.t.Helper()
	f.exec(`UPDATE farm.device_runtime SET battery_pct = $2 WHERE device_id = $1::uuid`, id, pct)
}

// lease gives a device a live lease the way the janitor's tests do — a row
// written directly, because these tests state the policy's contract against
// the columns of farm.devices, not against the allocator.
func (f *fixture) lease(id string) {
	f.t.Helper()
	var jobID string
	if err := f.pool.QueryRow(f.t.Context(), `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, attempt, max_attempts, started_at, spec)
VALUES ($1, $2, $3, 'running', 1, 1, now(), '{"steps":[]}'::jsonb)
RETURNING id::text`, "tenant-"+f.tag, "queue-"+f.tag, f.poolID).Scan(&jobID); err != nil {
		f.t.Fatalf("insert job: %v", err)
	}
	f.exec(`
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, state, ttl, grace, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, $2::uuid, $3, $4, 'pod-test', gen_random_uuid(), 'held',
       interval '15 minutes', interval '30 minutes',
       now() + interval '15 minutes', now() + interval '45 minutes'
  FROM farm.devices d WHERE d.id = $1::uuid`, id, jobID, "tenant-"+f.tag, "queue-"+f.tag)
	var leased bool
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT current_lease_id IS NOT NULL FROM farm.devices WHERE id = $1::uuid`, id).Scan(&leased); err != nil {
		f.t.Fatalf("read current_lease_id: %v", err)
	}
	if !leased {
		f.t.Fatal("the lease row did not set devices.current_lease_id; the fixture is wrong")
	}
}

type devState struct {
	admin, gate, parkedBy, closedBy, closeReason string
	parked, auto                                 bool
	parkRows                                     int
}

func (f *fixture) state(id string) devState {
	f.t.Helper()
	var s devState
	err := f.pool.QueryRow(f.t.Context(), `
SELECT d.admin_state, COALESCE(r.charge_gate, ''),
       p.id IS NOT NULL, COALESCE(p.auto, false), COALESCE(p.opened_by, ''),
       COALESCE((SELECT closed_by FROM farm.device_parks x
                  WHERE x.device_id = d.id AND x.closed_at IS NOT NULL
                  ORDER BY x.closed_at DESC LIMIT 1), ''),
       COALESCE((SELECT close_reason FROM farm.device_parks x
                  WHERE x.device_id = d.id AND x.closed_at IS NOT NULL
                  ORDER BY x.closed_at DESC LIMIT 1), ''),
       (SELECT count(*) FROM farm.device_parks x WHERE x.device_id = d.id)
  FROM farm.devices d
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.device_parks p   ON p.device_id = d.id AND p.closed_at IS NULL
 WHERE d.id = $1::uuid`, id).Scan(&s.admin, &s.gate, &s.parked, &s.auto, &s.parkedBy,
		&s.closedBy, &s.closeReason, &s.parkRows)
	if err != nil {
		f.t.Fatalf("read device state: %v", err)
	}
	return s
}

// policy builds a Policy over the real node client, which resolves the
// fixture's host through farm.hosts exactly as the shipped binary does.
func (f *fixture) policy(tweaks ...func(*Config)) *Policy {
	f.t.Helper()
	c, err := node.NewClient(node.ClientConfig{
		Resolver:     node.NewDBResolver(f.pool),
		Token:        stubToken,
		DialRetries:  1,
		RetryBackoff: time.Millisecond,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		f.t.Fatalf("NewClient: %v", err)
	}
	cfg := Config{
		Pool:        f.pool,
		Gates:       c,
		Interval:    time.Minute,
		CallTimeout: 10 * time.Second,
		LockKey:     DefaultLockKey + f.seq,
		Logger:      f.logs.logger(),
	}
	for _, tw := range tweaks {
		tw(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	f.t.Cleanup(func() { p.lead.release(context.Background()) })
	return p
}

func (f *fixture) cycle(p *Policy) {
	f.t.Helper()
	p.cycle(f.t.Context())
}

func (f *fixture) expect(id string, want devState, what string) {
	f.t.Helper()
	got := f.state(id)
	if got.admin != want.admin || got.gate != want.gate || got.parked != want.parked ||
		got.auto != want.auto || got.parkedBy != want.parkedBy {
		f.t.Fatalf("%s: device state = %+v, want admin=%q gate=%q parked=%v auto=%v by=%q",
			what, got, want.admin, want.gate, want.parked, want.auto, want.parkedBy)
	}
}

var (
	heldByUs = devState{admin: "parked", gate: "off", parked: true, auto: true, parkedBy: DefaultActor}
	free     = devState{admin: "enabled", gate: "on"}
	untouch  = devState{admin: "enabled", gate: ""}
)

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

// TestParkAndGateAboveTheCeilingThenReleaseAtTheFloor is the loop's contract
// in one run: an idle device at 95% is parked under this loop's actor and its
// port asserted dark through the real client; at 35% the gate comes off, the
// park is closed by the same actor, and the column says the port is on.
//
// Falsify: make writeGate a no-op (charge_gate stays empty), or drop the
// unparkAll call from the verdictRelease arm of execute (the device stays
// parked at 35%).
func TestParkAndGateAboveTheCeilingThenReleaseAtTheFloor(t *testing.T) {
	f := newFixture(t, "per_port", true)
	id, devpath := f.device(1, 95)
	p := f.policy()

	f.cycle(p)
	if !p.lead.held {
		t.Fatal("the only policy in the farm did not take leadership")
	}
	f.expect(id, heldByUs, "after the first cycle at 95%")

	g, ok := f.agent.held(devpath)
	if !ok {
		t.Fatalf("the agent holds no gate on %s", devpath)
	}
	if !strings.HasPrefix(g.Reason, reasonPrefix) || !strings.Contains(g.Reason, "95%") {
		t.Fatalf("gate reason = %q, want this loop's prefix and the reading", g.Reason)
	}
	sets := f.agent.offSets(devpath)
	if len(sets) != 1 {
		t.Fatalf("%d off-gate(s) asserted, want 1", len(sets))
	}
	if sets[0].HoldSeconds != (2 * time.Minute).Seconds() {
		t.Fatalf("hold = %vs, want two intervals (120s)", sets[0].HoldSeconds)
	}
	if sets[0].HoldSeconds >= node.MaxChargeGateHold.Seconds() {
		t.Fatalf("hold %vs is not below the agent's cap", sets[0].HoldSeconds)
	}
	if len(sets[0].Acknowledged) != 0 {
		t.Fatalf("a per-port gate acknowledged %v; there is nothing to acknowledge", sets[0].Acknowledged)
	}

	var reason string
	if err := f.pool.QueryRow(t.Context(), `SELECT reason FROM farm.device_parks
	        WHERE device_id = $1::uuid AND closed_at IS NULL`, id).Scan(&reason); err != nil {
		t.Fatalf("read park: %v", err)
	}
	if !strings.HasPrefix(reason, reasonPrefix) {
		t.Fatalf("park reason = %q, want this loop's prefix", reason)
	}

	var beat bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM farm.component_heartbeat
	        WHERE component = $1 AND beat_at > now() - interval '1 minute')`, DefaultComponent).Scan(&beat); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if !beat {
		t.Fatalf("no fresh %q row in farm.component_heartbeat", DefaultComponent)
	}

	// The watchdog read it again — a peek, a bench charger, a hub that keeps
	// its data lines up — and it is at the floor.
	f.setBattery(id, 35)
	f.cycle(p)
	s := f.state(id)
	f.expect(id, free, "after the cycle at 35%")
	if s.closedBy != DefaultActor || !strings.Contains(s.closeReason, "35%") {
		t.Fatalf("park closed by %q for %q, want %s and the reading", s.closedBy, s.closeReason, DefaultActor)
	}
	if _, ok := f.agent.held(devpath); ok {
		t.Fatalf("the agent still holds a gate on %s after the release", devpath)
	}
}

// TestAHeldGateIsRenewedEveryCycleUnderTheCap. The renewal is the proof of
// life; every cycle re-asserts the same hold, and the park is not churned
// while it does.
//
// Falsify: in execute's verdictRenew arm, skip p.assert.
func TestAHeldGateIsRenewedEveryCycleUnderTheCap(t *testing.T) {
	f := newFixture(t, "per_port", true)
	id, devpath := f.device(1, 92)
	p := f.policy()

	for range 3 {
		f.cycle(p)
	}
	sets := f.agent.offSets(devpath)
	if len(sets) != 3 {
		t.Fatalf("%d off-gate(s) over three cycles, want 3 (one assertion, two renewals)", len(sets))
	}
	for i, s := range sets {
		if s.HoldSeconds != p.hold.Seconds() || s.HoldSeconds > node.MaxChargeGateHold.Seconds() {
			t.Fatalf("assertion %d: hold %vs, want %s and under the cap", i, s.HoldSeconds, p.hold)
		}
	}
	st := f.state(id)
	f.expect(id, heldByUs, "after three cycles")
	if st.parkRows != 1 {
		t.Fatalf("%d park rows for one hold, want 1: renewals must not churn the ledger", st.parkRows)
	}
	if got := metricValue(t, parkedGauge); got != 1 {
		t.Fatalf("farm_chargepolicy_parked_devices = %v, want 1", got)
	}
}

// TestAHostWithoutAnAgentIsWarnedAboutOnce. A farm without host agents is
// the ordinary demo shape: the loop beats, says once per host that it cannot
// hold a port there, counts the device it would have held, and parks nothing.
//
// Falsify: replace warnOnce with p.log.Warn in listGates' no-agent branch;
// three cycles then log three lines.
func TestAHostWithoutAnAgentIsWarnedAboutOnce(t *testing.T) {
	f := newFixture(t, "per_port", false)
	id, _ := f.device(1, 95)
	p := f.policy()

	before := metricValue(t, skippedTotal.WithLabelValues("no_agent"))
	for range 3 {
		f.cycle(p)
	}
	if n := f.logs.count("no node agent endpoint"); n != 1 {
		t.Fatalf("the no-agent warning was logged %d time(s) over three cycles, want exactly 1", n)
	}
	if n := f.logs.count(f.hostID); n < 1 {
		t.Fatal("the warning does not name the host")
	}
	f.expect(id, untouch, "with no agent")
	if got := metricValue(t, skippedTotal.WithLabelValues("no_agent")) - before; got != 3 {
		t.Fatalf("farm_chargepolicy_skipped_total{reason=no_agent} rose by %v, want 3", got)
	}
	if f.agent.setCount() != 0 {
		t.Fatal("the stub was called although the host has no endpoint recorded")
	}
}

// TestAHumansDecisionWins. A device a human parked is left exactly as they
// left it; a human closing this loop's park ends the hold, and the device is
// not parked again inside the cooldown even though the reading that earned
// the park is still on the row.
//
// Falsify: in decide, put every parked device under `ours` (`case d.Parked:`)
// — a gate is then asserted on alice's device; or disable the cooldown check
// (`if cooldown && false`) — the device is re-parked the cycle after the
// human freed it.
func TestAHumansDecisionWins(t *testing.T) {
	f := newFixture(t, "per_port", true)
	theirs, theirPath := f.device(1, 97)
	mine, minePath := f.device(2, 96)
	f.exec(`SELECT farm.device_park($1::uuid, 'alice', 'bench test', false)`, theirs)
	p := f.policy()

	f.cycle(p)
	f.expect(theirs, devState{admin: "parked", gate: "", parked: true, auto: false, parkedBy: "alice"},
		"alice's park after a cycle")
	if _, ok := f.agent.held(theirPath); ok {
		t.Fatal("a gate was asserted on a device a human parked")
	}
	f.expect(mine, heldByUs, "the idle device beside it")

	// Alice needs the second phone now.
	f.exec(`SELECT farm.device_unpark($1::uuid, 'alice', 'need it', false)`, mine)
	f.cycle(p)
	if _, ok := f.agent.held(minePath); ok {
		t.Fatal("the gate stayed on after a human closed the park under it")
	}
	f.expect(mine, free, "after alice unparked it")

	// Still 96% on the row; still not parked, and counted as why.
	before := metricValue(t, skippedTotal.WithLabelValues("human_unparked"))
	f.cycle(p)
	f.expect(mine, free, "one cycle into the cooldown")
	if got := metricValue(t, skippedTotal.WithLabelValues("human_unparked")) - before; got != 1 {
		t.Fatalf("skipped{human_unparked} rose by %v, want 1", got)
	}
	// And alice's own park is still hers.
	if s := f.state(theirs); !s.parked || s.parkedBy != "alice" {
		t.Fatalf("alice's park = %+v after three cycles", s)
	}
}

// TestAGangedDomainIsHeldOnlyWhenEveryNeighbourIsIdle. On a shared switch
// the domain is the unit: both devices are parked, the gate is anchored at
// the hottest and acknowledges the other position, and the domain is released
// when its lowest device reaches the floor. With a leased neighbour nothing
// is parked and the agent is not asked.
//
// Falsify: send nil as Acknowledged in assert; the stub refuses and the parks
// are rolled back.
func TestAGangedDomainIsHeldOnlyWhenEveryNeighbourIsIdle(t *testing.T) {
	f := newFixture(t, "ganged", true)
	hot, hotPath := f.device(1, 95)
	warm, warmPath := f.device(2, 70)
	f.agent.domain = []string{hotPath, warmPath}
	p := f.policy()

	f.cycle(p)
	f.expect(hot, heldByUs, "the hot device")
	f.expect(warm, heldByUs, "its neighbour at 70%")
	g, ok := f.agent.held(hotPath)
	if !ok {
		t.Fatalf("no gate on the hottest position %s", hotPath)
	}
	if !slices.Contains(g.Acknowledged, warmPath) {
		t.Fatalf("acknowledged = %v, want the neighbour %s", g.Acknowledged, warmPath)
	}
	if _, ok := f.agent.held(warmPath); ok {
		t.Fatal("a second gate was asserted on a domain one switch controls")
	}

	// The hot one drains to 50 — still above the floor — the warm one to 40.
	f.setBattery(hot, 50)
	f.setBattery(warm, 40)
	f.cycle(p)
	f.expect(hot, free, "the domain once its lowest device reached the floor")
	f.expect(warm, free, "the neighbour at the floor")
	if _, ok := f.agent.held(hotPath); ok {
		t.Fatal("the domain's gate stayed on after the release")
	}

	// A leased neighbour makes the whole domain untouchable.
	f2 := newFixture(t, "ganged", true)
	idle, _ := f2.device(1, 99)
	busy, _ := f2.device(2, 55)
	f2.lease(busy)
	p2 := f2.policy()
	f2.cycle(p2)
	f2.expect(idle, untouch, "an idle device sharing a switch with a leased one")
	if f2.agent.setCount() != 0 {
		t.Fatal("the agent was asked to gate a domain with a live lease in it")
	}
}

// TestARefusedGateClosesThePark. A park with no gate under it is an
// unschedulable phone that is charging, the worst of both; when the agent
// refuses the assertion the park is rolled back in the same cycle.
//
// Falsify: hand unparkAll an empty list after a failed assert in execute
// (`toClose[:0]`).
func TestARefusedGateClosesThePark(t *testing.T) {
	f := newFixture(t, "per_port", true)
	f.agent.refuseOff = true
	id, _ := f.device(1, 95)
	p := f.policy()

	before := metricValue(t, actionsTotal.WithLabelValues("gate", "refused"))
	f.cycle(p)
	s := f.state(id)
	if s.parked || s.admin != "enabled" {
		t.Fatalf("device left %+v after a refused gate; the park must be rolled back", s)
	}
	if s.parkRows != 1 || !strings.Contains(s.closeReason, "could not be asserted") {
		t.Fatalf("ledger = %+v, want one park closed because the gate could not be asserted", s)
	}
	if s.gate == node.ChargePowerOff {
		t.Fatal("charge_gate says off for a port the agent refused to switch")
	}
	if got := metricValue(t, actionsTotal.WithLabelValues("gate", "refused")) - before; got != 1 {
		t.Fatalf("actions{gate,refused} rose by %v, want 1", got)
	}
}

// TestAStandbyDoesNotAct. Two replicas, one advisory lock: the second beats
// and decides nothing, however overdue the fleet looks to it.
//
// Falsify: return true from leadership.ensure when the lock is taken.
func TestAStandbyDoesNotAct(t *testing.T) {
	f := newFixture(t, "per_port", true)
	id, _ := f.device(1, 95)
	leader := f.policy()
	standby := f.policy()

	f.cycle(leader)
	f.expect(id, heldByUs, "after the leader's cycle")

	f.setBattery(id, 35)
	f.cycle(standby)
	if standby.lead.held {
		t.Fatal("two replicas hold the same advisory lock")
	}
	f.expect(id, heldByUs, "after the standby's cycle: it must not have released anything")

	f.cycle(leader)
	f.expect(id, free, "after the leader's next cycle")
}

// TestAnUnreachableAgentLeavesTheParkAndMarksTheGateUnknown. Nothing is
// known about a port whose agent does not answer, so the park stays — the
// device must not be handed to the scheduler while it may be dark — and the
// column says so. When the agent is back the hold is renewed as if nothing
// happened.
//
// Falsify: in cycle, disable the `!ok` guard (`if false && ok`) so an unlisted
// host is decided on an empty gate list; the park is then closed while the
// port is still dark.
func TestAnUnreachableAgentLeavesTheParkAndMarksTheGateUnknown(t *testing.T) {
	f := newFixture(t, "per_port", true)
	id, devpath := f.device(1, 95)
	p := f.policy()

	f.cycle(p)
	f.expect(id, heldByUs, "with the agent up")

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	f.exec(`UPDATE farm.hosts SET node_endpoint = $2 WHERE id = $1`, f.hostID, deadURL)

	f.cycle(p)
	f.expect(id, devState{admin: "parked", gate: "unknown", parked: true, auto: true, parkedBy: DefaultActor},
		"with the agent unreachable")
	if n := f.logs.count("did not list its charge gates"); n != 1 {
		t.Fatalf("unreachable warning logged %d time(s), want 1", n)
	}

	f.exec(`UPDATE farm.hosts SET node_endpoint = $2 WHERE id = $1`, f.hostID, f.srv.URL)
	f.cycle(p)
	f.expect(id, heldByUs, "with the agent back")
	if len(f.agent.offSets(devpath)) != 2 {
		t.Fatalf("%d off-gates, want the original and one renewal", len(f.agent.offSets(devpath)))
	}
}

// TestAStrayGateIsReleased. The agent reports a gate of this loop's on a
// position no device is held under — the device was retired while the port
// was dark. The gate is handed back; a gate that is not this loop's is left
// to whoever placed it.
//
// Falsify: delete the releaseStrays call at the end of cycle.
func TestAStrayGateIsReleased(t *testing.T) {
	f := newFixture(t, "per_port", true)
	id, devpath := f.device(1, 95)
	p := f.policy()
	f.cycle(p)
	f.expect(id, heldByUs, "held")

	// Somebody else's hold on another port, placed by hand.
	f.agent.mu.Lock()
	f.agent.gates["usb:3-1.7"] = node.ChargeGate{HostID: f.hostID, Devpath: "usb:3-1.7",
		Power: node.ChargePowerOff, Held: true, Reason: "operator: bench"}
	f.agent.mu.Unlock()

	// The device leaves the farm for good, park and all.
	f.exec(`SELECT farm.device_unpark($1::uuid, 'alice', 'retiring', false)`, id)
	f.exec(`UPDATE farm.devices SET admin_state = 'retired', current_slot_id = NULL WHERE id = $1::uuid`, id)

	f.cycle(p)
	if _, ok := f.agent.held(devpath); ok {
		t.Fatalf("the gate on %s is still held with no device under it", devpath)
	}
	if _, ok := f.agent.held("usb:3-1.7"); !ok {
		t.Fatal("the operator's own gate was released by the policy")
	}
}
