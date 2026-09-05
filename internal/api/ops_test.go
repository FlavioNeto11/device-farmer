package api

// POST /api/v1/slots/{id}/power, against a real database and — where the case
// is about the wire — the real farmd-node agent served over httptest.
//
// Every case asserts on the farm.recovery_attempts row as much as on the status
// code, because the row is the bug this route had: an earlier version opened
// one at tier 4 for a host agent that never read the table, and the janitor
// closed every one of them as aborted. A row closed with what the agent
// actually answered is the whole point.

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
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/node"
	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

const powerTestToken = "t0ken-for-the-slot-power-tests"

var powerFixtureSeq atomic.Int64

// powerDB connects to DATABASE_URL, or skips. The database is expected to be
// migrated; nothing here is created outside the rows the fixture seeds under
// its own host id.
func powerDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
	if dsn == "" {
		t.Skipf("%s is not set; skipping the PostgreSQL-backed slot power tests", config.EnvDatabaseURL)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// powerFixture is one GANGED power domain: a hub whose ports share a single
// VBUS, with two positions in it. Cycling either darkens the other, which is
// precisely the shape the acknowledgement exists for.
type powerFixture struct {
	pool *pgxpool.Pool
	tag  string

	hostID string
	// slotID and devpath are the target; siblingID and sibling the other
	// position in the same domain.
	slotID    int64
	devpath   string
	siblingID int64
	sibling   string
}

func newPowerFixture(t *testing.T, pool *pgxpool.Pool) *powerFixture {
	t.Helper()
	ctx := context.Background()
	// Digits and underscores only: the tag is also an ltree label.
	tag := fmt.Sprintf("%d_%04d", os.Getpid(), powerFixtureSeq.Add(1))
	f := &powerFixture{pool: pool, tag: tag, hostID: "pwr-host-" + tag}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding %s: %v\nstatement: %s", f.hostID, err, q)
		}
	}
	exec(`INSERT INTO farm.racks (id) VALUES ($1)`, "pwr-rack-"+tag)
	exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ($1, $2, '127.0.0.1:5037')`,
		f.hostID, "pwr-rack-"+tag)
	exec(`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3)`, f.hostID)
	exec(`INSERT INTO farm.power_domains (host_id, kind, control) VALUES ($1, 'ganged', 'uhubctl')`, f.hostID)
	exec(`INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
	      SELECT $1, c.id, '3-1', 4, true FROM farm.controllers c WHERE c.host_id = $1`, f.hostID)
	for port := 1; port <= 2; port++ {
		p := strconv.Itoa(port)
		exec(`INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path, topo_path, rack_slot)
		      SELECT $1, h.id, pd.id, $2::int, '3-1.' || $2, ('pwr' || $3 || '.p' || $2)::ltree, 'R-' || $3 || '-P' || $2
		        FROM farm.hubs h, farm.power_domains pd
		       WHERE h.host_id = $1 AND pd.host_id = $1`, f.hostID, p, tag)
	}
	if err := pool.QueryRow(ctx, `
SELECT t.id, t.adb_devpath, o.id, o.adb_devpath
  FROM farm.slots t JOIN farm.slots o ON o.host_id = t.host_id AND o.port_number = 2
 WHERE t.host_id = $1 AND t.port_number = 1`, f.hostID).
		Scan(&f.slotID, &f.devpath, &f.siblingID, &f.sibling); err != nil {
		t.Fatalf("reading the seeded slots for %s: %v", f.hostID, err)
	}
	return f
}

// leaseSibling puts a live lease with the given disruption policy on the OTHER
// position in the domain, so a refusal proves the check is domain-wide rather
// than per-slot.
func (f *powerFixture) leaseSibling(t *testing.T, policy string) string {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding a lease for %s: %v\nstatement: %s", f.hostID, err, q)
		}
	}
	poolID, tenantID, queueID := "pwr-pool-"+f.tag, "pwr-tenant-"+f.tag, "pwr-queue-"+f.tag
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, poolID)
	exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, tenantID)
	exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, queueID, tenantID)

	var jobID, deviceID, leaseID string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, started_at, spec, disruption_policy)
VALUES ($1, $2, $3, 'running', now(), '{"steps":[]}'::jsonb, $4)
RETURNING id::text`, tenantID, queueID, poolID, policy).Scan(&jobID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, manufacturer, model, sdk_int)
VALUES ('df-' || md5($1), 'SER-' || $1, $2, $3, $4, 'Google', 'Pixel Test', 34)
RETURNING id::text`, f.tag, poolID, f.hostID, f.siblingID).Scan(&deviceID); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	// Written directly rather than through farm.lease_acquire: the handler's
	// contract is stated against the columns of farm.leases, and the sync
	// trigger sets devices.current_lease_id, which is what the domain query
	// joins on.
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id, holder, holder_instance,
                         state, disruption_policy, ttl, grace, expires_at, reclaimable_at)
VALUES ($1::uuid, $2, $3::uuid, $4, $5, 'pod-test', gen_random_uuid(), 'held', $6,
        interval '15 minutes', interval '30 minutes',
        now() + interval '15 minutes', now() + interval '45 minutes')
RETURNING id::text`, deviceID, f.siblingID, jobID, tenantID, queueID, policy).Scan(&leaseID); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
	return leaseID
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newPowerServer builds a real Server over pool with an open authenticator, so
// the route is reached exactly as the router mounts it.
func newPowerServer(t *testing.T, pool *pgxpool.Pool, opts ...Option) *Server {
	t.Helper()
	base := []Option{WithLogger(quietLogger()), WithAuthenticator(NewAllowAll(quietLogger(), "alice"))}
	s, err := New(&config.Config{APIAddr: "127.0.0.1:0", Component: "api"}, pool, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

// postPower performs the request and decodes whatever came back.
func postPower(t *testing.T, s *Server, slotID int64) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"reason": "verifying " + t.Name()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/slots/%d/power", slotID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response %d is not JSON: %v\n%s", rec.Code, err, rec.Body.String())
	}
	return rec.Code, out
}

// errorEnvelope pulls code, message and detail out of the error body.
func errorEnvelope(t *testing.T, out map[string]any) (code, message string, detail map[string]any) {
	t.Helper()
	env, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %v", out)
	}
	code, _ = env["code"].(string)
	message, _ = env["message"].(string)
	detail, _ = env["detail"].(map[string]any)
	return code, message, detail
}

type attemptRow struct {
	Outcome  string
	Refusal  string
	Finished bool
	Detail   map[string]any
}

// attemptsFor reads every tier-4 row the route wrote for slotID.
func attemptsFor(t *testing.T, pool *pgxpool.Pool, slotID int64) []attemptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT coalesce(outcome, ''), coalesce(refusal, ''), finished_at IS NOT NULL, detail
  FROM farm.recovery_attempts
 WHERE slot_id = $1 AND tier = $2
 ORDER BY id`, slotID, powerCycleTier)
	if err != nil {
		t.Fatalf("reading attempts: %v", err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.Outcome, &a.Refusal, &a.Finished, &a.Detail); err != nil {
			t.Fatalf("scanning attempt: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading attempts: %v", err)
	}
	return out
}

// theOneAttempt asserts exactly one row exists, closed, with the outcome.
func theOneAttempt(t *testing.T, pool *pgxpool.Pool, slotID int64, outcome string) attemptRow {
	t.Helper()
	rows := attemptsFor(t, pool, slotID)
	if len(rows) != 1 {
		t.Fatalf("slot %d has %d tier-4 attempt rows, want exactly 1: %+v", slotID, len(rows), rows)
	}
	a := rows[0]
	if !a.Finished {
		t.Fatalf("the attempt row is still open; nothing but the janitor would ever close it: %+v", a)
	}
	if a.Outcome != outcome {
		t.Fatalf("attempt outcome = %q, want %q: %+v", a.Outcome, outcome, a)
	}
	return a
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func stringsOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

type powerCall struct {
	hostID, devpath string
	acknowledged    []string
}

// gangedRunner stands in for a farmd-node agent on a hub whose ports share one
// VBUS: it refuses any cycle whose acknowledgement does not cover every other
// position it knows about, which is the check the real agent makes against
// sysfs. Once the blast radius clears it answers err.
type gangedRunner struct {
	ports []string
	err   error

	mu    sync.Mutex
	calls []powerCall
}

var _ recovery.DomainPowerRunner = (*gangedRunner)(nil)

func (g *gangedRunner) USBReset(context.Context, string, string) error {
	return errors.New("USBReset is not under test here")
}

func (g *gangedRunner) PortPower(ctx context.Context, hostID, devpath string) error {
	return g.PortPowerWithDomain(ctx, hostID, devpath, nil)
}

func (g *gangedRunner) PortPowerWithDomain(_ context.Context, hostID, devpath string, ack []string) error {
	g.mu.Lock()
	g.calls = append(g.calls, powerCall{hostID, devpath, append([]string(nil), ack...)})
	g.mu.Unlock()
	for _, p := range g.ports {
		if p == devpath || slices.Contains(ack, p) {
			continue
		}
		return fmt.Errorf("node: %w: cycling %s would also darken %s, which nobody authorised",
			node.ErrRefused, devpath, p)
	}
	return g.err
}

func (g *gangedRunner) snapshot() []powerCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]powerCall(nil), g.calls...)
}

// targetOnlyRunner is a host runner that predates recovery.DomainPowerRunner.
// It is a distinct type with only the two HostRunner methods, so the handler's
// type assertion fails and the acknowledgement has no way to travel.
type targetOnlyRunner struct{ inner *gangedRunner }

var _ recovery.HostRunner = targetOnlyRunner{}

func (r targetOnlyRunner) USBReset(ctx context.Context, hostID, devpath string) error {
	return r.inner.USBReset(ctx, hostID, devpath)
}

func (r targetOnlyRunner) PortPower(ctx context.Context, hostID, devpath string) error {
	return r.inner.PortPower(ctx, hostID, devpath)
}

// liveAgentClient serves the real node.Agent for hostID over httptest, records
// its address as the fixture host's node_endpoint, and returns the production
// client wired exactly as cmd/farmd wires it: a DBResolver over the same pool.
func liveAgentClient(t *testing.T, f *powerFixture, agentHostID string) *node.Client {
	t.Helper()
	agent, err := node.New(node.Config{
		Pool: f.pool, HostID: agentHostID, Token: powerTestToken, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	h, err := agent.Handler()
	if err != nil {
		t.Fatalf("agent.Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return clientFor(t, f, srv.URL)
}

func clientFor(t *testing.T, f *powerFixture, endpoint string) *node.Client {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE farm.hosts SET node_endpoint = $2 WHERE id = $1`, f.hostID, endpoint); err != nil {
		t.Fatalf("recording the agent endpoint: %v", err)
	}
	c, err := node.NewClient(node.ClientConfig{
		Resolver:     node.NewDBResolver(f.pool),
		Token:        powerTestToken,
		DialRetries:  1,
		RetryBackoff: time.Millisecond,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("node.NewClient: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Cases
// ---------------------------------------------------------------------------

// TestSlotPowerWithoutAHostAgentAnswers503AndOpensNoRow. A farm with no agent
// cannot cycle a port, and must say so without writing an attempt: a row
// opened here would only ever be closed by the janitor, as aborted, which
// claims a process died when in truth nothing was ever asked.
//
// Falsify: drop the hostRunner == nil guard in handleSlotPower.
func TestSlotPowerWithoutAHostAgentAnswers503AndOpensNoRow(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	s := newPowerServer(t, pool)

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", code, out)
	}
	errCode, msg, _ := errorEnvelope(t, out)
	if errCode != CodeUnavailable || !strings.Contains(msg, "no host agent") {
		t.Fatalf("error = %s %q, want %s naming the missing agent", errCode, msg, CodeUnavailable)
	}
	if rows := attemptsFor(t, pool, f.slotID); len(rows) != 0 {
		t.Fatalf("%d attempt row(s) were opened with nothing to close them: %+v", len(rows), rows)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM farm.audit_log
		WHERE action = 'slot.power' AND subject = $1 AND detail->>'outcome' = 'unavailable'`,
		fmt.Sprintf("slot:%d", f.slotID)); n != 1 {
		t.Fatalf("%d audit row(s) say the request was unavailable, want 1", n)
	}
}

// TestSlotPowerCyclesTheDomainAndClosesItsOwnRow is the happy path this route
// never had: the agent is asked with every other position in the domain
// acknowledged, and the row is closed as recovered — by this handler, before it
// answers, with the honest note that ADB health is the watchdog's to confirm.
//
// Falsify: remove the closeRecoveryAttempt call, or hand the agent nil for the
// acknowledgement.
func TestSlotPowerCyclesTheDomainAndClosesItsOwnRow(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}}
	s := newPowerServer(t, pool, WithHostRunner(g))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, out)
	}
	if out["outcome"] != "recovered" || out["confirmed"] != false {
		t.Fatalf("body = %v, want outcome recovered with confirmed=false", out)
	}
	if _, ok := out["attempt_id"]; !ok {
		t.Fatalf("the body does not name the attempt it closed: %v", out)
	}

	a := theOneAttempt(t, pool, f.slotID, "recovered")
	if a.Refusal != "" {
		t.Fatalf("a recovered attempt carries a refusal: %q", a.Refusal)
	}
	if got := stringsOf(a.Detail["acknowledged"]); !slices.Equal(got, []string{f.sibling}) {
		t.Fatalf("detail.acknowledged = %v, want [%s]", got, f.sibling)
	}
	if a.Detail["confirmed"] != false {
		t.Fatalf("detail.confirmed = %v; the API cannot see the device re-enumerate and must not claim to", a.Detail["confirmed"])
	}

	calls := g.snapshot()
	if len(calls) != 1 {
		t.Fatalf("the agent was asked %d time(s), want exactly 1: %+v", len(calls), calls)
	}
	if c := calls[0]; c.hostID != f.hostID || c.devpath != f.devpath || !slices.Equal(c.acknowledged, []string{f.sibling}) {
		t.Fatalf("the agent was asked for %+v; want host %s, devpath %s, acknowledged [%s]",
			c, f.hostID, f.devpath, f.sibling)
	}

	subject := fmt.Sprintf("slot:%d", f.slotID)
	if n := countRows(t, pool, `SELECT count(*) FROM farm.audit_log
		WHERE action = 'slot.power' AND subject = $1 AND detail->>'outcome' = 'recovered'`, subject); n != 1 {
		t.Fatalf("%d audit row(s) record the recovered cycle, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM farm.events
		WHERE kind = 'slot_power_recovered' AND slot_id = $1`, f.slotID); n != 1 {
		t.Fatalf("%d slot_power_recovered event(s), want 1", n)
	}
}

// TestSlotPowerRefusedByAGangedAgentWhenTheAcknowledgementCannotTravel. A
// runner with only the two HostRunner methods is asked for the target alone,
// and an agent on a ganged hub refuses — and that refusal must land in the row
// as 'refused' with the agent's reason, and reach the caller as 409. It is not
// a failed rung: the port was never touched.
//
// Falsify: file node.ErrRefused as "failed" in classifyPowerCycle.
func TestSlotPowerRefusedByAGangedAgentWhenTheAcknowledgementCannotTravel(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}}
	s := newPowerServer(t, pool, WithHostRunner(targetOnlyRunner{g}))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
	errCode, msg, _ := errorEnvelope(t, out)
	if errCode != CodeConflict || !strings.Contains(msg, "nobody authorised") {
		t.Fatalf("error = %s %q, want %s carrying the agent's own reason", errCode, msg, CodeConflict)
	}

	a := theOneAttempt(t, pool, f.slotID, "refused")
	if !strings.Contains(a.Refusal, "nobody authorised") {
		t.Fatalf("refusal = %q, want the agent's reason", a.Refusal)
	}
	if calls := g.snapshot(); len(calls) != 1 || calls[0].acknowledged != nil {
		t.Fatalf("calls = %+v; a target-only runner must be asked once, with no acknowledgement", calls)
	}
}

// TestSlotPowerRecordsTheRealAgentsRefusal drives the production client
// through the production resolver into the real agent over HTTP. The agent
// speaks for a different host than the slot's, which is its 409 — the same path
// a blast-radius refusal takes on a Linux host — and the row must say so.
//
// Falsify: make classifyPowerCycle ignore node.ErrRefused.
func TestSlotPowerRecordsTheRealAgentsRefusal(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	client := liveAgentClient(t, f, f.hostID+"-elsewhere")
	s := newPowerServer(t, pool, WithHostRunner(client))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
	if errCode, msg, _ := errorEnvelope(t, out); errCode != CodeConflict || !strings.Contains(msg, "addressed to host") {
		t.Fatalf("error = %s %q, want %s carrying the agent's reason", errCode, msg, CodeConflict)
	}
	a := theOneAttempt(t, pool, f.slotID, "refused")
	if !strings.Contains(a.Refusal, "addressed to host") {
		t.Fatalf("refusal = %q, want the agent's own sentence about the wrong host", a.Refusal)
	}
}

// TestSlotPowerWithAnUnreachableAgentIsFailedNotRefused. Nothing answered, so
// nothing is known about the port: the row closes as failed with
// detail.unreachable, never as refused — a refusal says the agent looked and
// declined, and it did not look.
//
// Falsify: drop the unreachable branch of classifyPowerCycle.
func TestSlotPowerWithAnUnreachableAgentIsFailedNotRefused(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	// A listener closed before the call: the dial is refused at once.
	dead := httptest.NewServer(http.NotFoundHandler())
	endpoint := dead.URL
	dead.Close()
	s := newPowerServer(t, pool, WithHostRunner(clientFor(t, f, endpoint)))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %v", code, out)
	}
	errCode, _, detail := errorEnvelope(t, out)
	if errCode != CodeHostAgent || detail["unreachable"] != true {
		t.Fatalf("error = %s with detail %v, want %s and unreachable=true", errCode, detail, CodeHostAgent)
	}
	a := theOneAttempt(t, pool, f.slotID, "failed")
	if a.Detail["unreachable"] != true || a.Refusal != "" {
		t.Fatalf("row = %+v; want unreachable=true and no refusal text", a)
	}
}

// TestSlotPowerRecordsAHardwareFailureAsFailed. The agent looked, cycled the
// port, and the device stayed dark. That is the one answer that may read as a
// failed rung: no refusal text, no unreachable flag, and a 502 that says the
// hardware was tried.
//
// Falsify: answer 200 from the last branch of classifyPowerCycle.
func TestSlotPowerRecordsAHardwareFailureAsFailed(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	g := &gangedRunner{
		ports: []string{f.devpath, f.sibling},
		err:   errors.New("the port was cycled and nothing enumerated within 20s"),
	}
	s := newPowerServer(t, pool, WithHostRunner(g))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %v", code, out)
	}
	errCode, msg, detail := errorEnvelope(t, out)
	if errCode != CodeHostAgent || !strings.Contains(msg, "did not come back") {
		t.Fatalf("error = %s %q, want %s saying the device did not come back", errCode, msg, CodeHostAgent)
	}
	if _, marked := detail["unreachable"]; marked {
		t.Fatalf("a rung the agent performed is marked unreachable: %v", detail)
	}
	a := theOneAttempt(t, pool, f.slotID, "failed")
	if a.Refusal != "" {
		t.Fatalf("a failed rung carries refusal text %q; the agent did not decline, it tried", a.Refusal)
	}
	if got, _ := a.Detail["error"].(string); !strings.Contains(got, "nothing enumerated") {
		t.Fatalf("detail.error = %q, want the agent's own sentence", got)
	}
	if len(g.snapshot()) != 1 {
		t.Fatalf("the agent was asked %d time(s), want exactly 1", len(g.snapshot()))
	}
}

// TestSlotPowerStillRefusesForALeaseThatForbidsIt guards the refusal this
// endpoint exists for: a live no_disruption lease anywhere in the domain — here
// on the OTHER position — stops the cycle before any agent is asked.
//
// Falsify: invert the policyRank comparison that collects offenders.
func TestSlotPowerStillRefusesForALeaseThatForbidsIt(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	leaseID := f.leaseSibling(t, "no_disruption")
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}}
	s := newPowerServer(t, pool, WithHostRunner(g))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
	errCode, _, detail := errorEnvelope(t, out)
	if errCode != CodeDisruptionRefused {
		t.Fatalf("error code = %s, want %s", errCode, CodeDisruptionRefused)
	}
	offenders, _ := detail["offenders"].([]any)
	if len(offenders) != 1 {
		t.Fatalf("offenders = %v, want the one lease on the sibling", detail["offenders"])
	}
	if o, _ := offenders[0].(map[string]any); o["lease_id"] != leaseID {
		t.Fatalf("offender = %v, want lease %s", o, leaseID)
	}
	if calls := g.snapshot(); len(calls) != 0 {
		t.Fatalf("the agent was asked %d time(s) despite the refusal: %+v", len(calls), calls)
	}
	a := theOneAttempt(t, pool, f.slotID, "refused")
	if !strings.Contains(a.Refusal, "live lease") {
		t.Fatalf("refusal = %q, want it to name the live lease", a.Refusal)
	}
}
