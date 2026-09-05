package api

// POST /api/v1/slots/{id}/power, against a real database and — where the case
// is about the wire — the real farmd-node agent served over httptest.
//
// Every case asserts on the farm.recovery_attempts row as much as on the status
// code, because the row is the bug this route had: an earlier version opened
// one at tier 4 for a host agent that never read the table, and the janitor
// closed every one of them as aborted. A row closed with what the agent
// actually answered — in the ladder's own vocabulary, so the two are
// indistinguishable to whoever reads the table — is the whole point.
//
// The fixture removes everything it seeded. TestSlotPowerFixtureLeavesNothing
// is the proof, and the recipe runs this package twice against one database
// to hold it.

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
// its own host id, and the fixture removes those when the test ends.
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
	rackID string
	poolID string
	tenant string
	queue  string
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
	f := &powerFixture{
		pool: pool, tag: tag,
		hostID: "pwr-host-" + tag, rackID: "pwr-rack-" + tag,
		poolID: "pwr-pool-" + tag, tenant: "pwr-tenant-" + tag, queue: "pwr-queue-" + tag,
	}
	// Registered first, so a fixture that fails half way through seeding is
	// still taken down.
	t.Cleanup(func() { f.remove(t) })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding %s: %v\nstatement: %s", f.hostID, err, q)
		}
	}
	exec(`INSERT INTO farm.racks (id) VALUES ($1)`, f.rackID)
	// An agent address is on record, as it is for any host whose agent has
	// enrolled: the route refuses to ask a host without one. The address
	// itself is never dialled by the in-process runners; the cases that use
	// the real client over HTTP replace it with the test server's.
	exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint, node_endpoint)
	      VALUES ($1, $2, '127.0.0.1:5037', $3)`,
		f.hostID, f.rackID, "http://pwr-agent-"+strings.ReplaceAll(tag, "_", "-")+".invalid")
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
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, f.tenant)
	exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, f.queue, f.tenant)

	if err := pool.QueryRow(ctx, `
SELECT t.id, t.adb_devpath, o.id, o.adb_devpath
  FROM farm.slots t JOIN farm.slots o ON o.host_id = t.host_id AND o.port_number = 2
 WHERE t.host_id = $1 AND t.port_number = 1`, f.hostID).
		Scan(&f.slotID, &f.devpath, &f.siblingID, &f.sibling); err != nil {
		t.Fatalf("reading the seeded slots for %s: %v", f.hostID, err)
	}
	return f
}

// remove deletes everything the fixture and the route wrote, children first.
// Rows the route writes — attempts, events, audit — are keyed by the fixture's
// slots and host, so they go too; nothing this package's tests write is left
// for the next run to trip over.
func (f *powerFixture) remove(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, st := range []struct {
		q   string
		arg any
	}{
		{`DELETE FROM farm.leases WHERE tenant_id = $1`, f.tenant},
		{`DELETE FROM farm.jobs WHERE tenant_id = $1`, f.tenant},
		{`DELETE FROM farm.recovery_attempts
		  WHERE host_id = $1
		     OR slot_id IN (SELECT id FROM farm.slots WHERE host_id = $1)
		     OR device_id IN (SELECT id FROM farm.devices WHERE host_id = $1)`, f.hostID},
		{`DELETE FROM farm.events
		  WHERE slot_id IN (SELECT id FROM farm.slots WHERE host_id = $1)
		     OR device_id IN (SELECT id FROM farm.devices WHERE host_id = $1)`, f.hostID},
		{`DELETE FROM farm.audit_log
		  WHERE subject IN (SELECT 'slot:' || id FROM farm.slots WHERE host_id = $1)`, f.hostID},
		{`DELETE FROM farm.devices WHERE host_id = $1`, f.hostID},
		{`DELETE FROM farm.queues WHERE tenant_id = $1`, f.tenant},
		{`DELETE FROM farm.tenants WHERE id = $1`, f.tenant},
		{`DELETE FROM farm.pools WHERE id = $1`, f.poolID},
		{`DELETE FROM farm.slots WHERE host_id = $1`, f.hostID},
		{`DELETE FROM farm.hosts WHERE id = $1`, f.hostID},
		{`DELETE FROM farm.racks WHERE id = $1`, f.rackID},
	} {
		if _, err := f.pool.Exec(ctx, st.q, st.arg); err != nil {
			t.Errorf("cleaning up %s: %v\nstatement: %s", f.hostID, err, st.q)
		}
	}
}

// device seeds a device sitting in slotID, with no lease.
func (f *powerFixture) device(t *testing.T, slotID int64) string {
	t.Helper()
	var deviceID string
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, manufacturer, model, sdk_int)
VALUES ('df-' || md5($1 || $4::bigint::text), 'SER-' || $1 || '-' || $4::bigint::text, $2, $3, $4::bigint,
        'Google', 'Pixel Test', 34)
RETURNING id::text`, f.tag, f.poolID, f.hostID, slotID).Scan(&deviceID); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	return deviceID
}

// leaseSibling puts a live lease with the given disruption policy on the OTHER
// position in the domain, so a refusal proves the check is domain-wide rather
// than per-slot.
func (f *powerFixture) leaseSibling(t *testing.T, policy string) string {
	t.Helper()
	ctx := context.Background()
	var jobID string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, started_at, spec, disruption_policy)
VALUES ($1, $2, $3, 'running', now(), '{"steps":[]}'::jsonb, $4)
RETURNING id::text`, f.tenant, f.queue, f.poolID, policy).Scan(&jobID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	deviceID := f.device(t, f.siblingID)
	// Written directly rather than through farm.lease_acquire: the handler's
	// contract is stated against the columns of farm.leases, and the sync
	// trigger sets devices.current_lease_id, which is what the domain query
	// joins on.
	var leaseID string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id, holder, holder_instance,
                         state, disruption_policy, ttl, grace, expires_at, reclaimable_at)
VALUES ($1::uuid, $2, $3::uuid, $4, $5, 'pod-test', gen_random_uuid(), 'held', $6,
        interval '15 minutes', interval '30 minutes',
        now() + interval '15 minutes', now() + interval '45 minutes')
RETURNING id::text`, deviceID, f.siblingID, jobID, f.tenant, f.queue, policy).Scan(&leaseID); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
	return leaseID
}

// forgetAgentAddress clears farm.hosts.node_endpoint for the fixture host.
func (f *powerFixture) forgetAgentAddress(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE farm.hosts SET node_endpoint = NULL WHERE id = $1`, f.hostID); err != nil {
		t.Fatalf("clearing the agent address: %v", err)
	}
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
	ID       int64
	DeviceID string
	Outcome  string
	Refusal  string
	Finished bool
	Detail   map[string]any
}

// disposition is what the row says happened, read the way the ladder and the
// UI read it.
func (a attemptRow) disposition() string {
	s, _ := a.Detail[recovery.DetailDisposition].(string)
	return s
}

// attemptsFor reads every tier-4 row the route wrote for slotID.
func attemptsFor(t *testing.T, pool *pgxpool.Pool, slotID int64) []attemptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT id, coalesce(device_id::text, ''), coalesce(outcome, ''), coalesce(refusal, ''),
       finished_at IS NOT NULL, detail
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
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.Outcome, &a.Refusal, &a.Finished, &a.Detail); err != nil {
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
func theOneAttempt(t *testing.T, pool *pgxpool.Pool, slotID int64, outcome recovery.Outcome) attemptRow {
	t.Helper()
	rows := attemptsFor(t, pool, slotID)
	if len(rows) != 1 {
		t.Fatalf("slot %d has %d tier-4 attempt rows, want exactly 1: %+v", slotID, len(rows), rows)
	}
	a := rows[0]
	if !a.Finished {
		t.Fatalf("the attempt row is still open; nothing but the janitor would ever close it: %+v", a)
	}
	if a.Outcome != string(outcome) {
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

// auditOutcomes counts slot.power audit rows for the slot by detail.outcome.
func auditOutcomes(t *testing.T, pool *pgxpool.Pool, slotID int64, outcome string) int {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM farm.audit_log
		WHERE action = 'slot.power' AND subject = $1 AND detail->>'outcome' = $2`,
		fmt.Sprintf("slot:%d", slotID), outcome)
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
	hostID       string
	devpath      string
	acknowledged []string
}

// gangedRunner stands in for a farmd-node agent on a hub whose ports share one
// VBUS: it refuses any cycle whose acknowledgement does not cover every other
// position it knows about, which is the check the real agent makes against
// sysfs. Once the blast radius clears it answers err — or, when hang is set,
// waits for the caller's context to run out and answers with that, which is
// what a runner ignoring its own budget looks like from here.
type gangedRunner struct {
	ports  []string
	err    error
	hang   bool
	budget time.Duration

	// entered is signalled once per call, before the call blocks on hold; a
	// test that needs the cycle to be IN FLIGHT waits on it. Both are optional.
	entered chan struct{}
	hold    chan struct{}

	mu    sync.Mutex
	calls []powerCall
}

var _ recovery.DomainPowerRunner = (*gangedRunner)(nil)

func (g *gangedRunner) USBReset(context.Context, string, string) error {
	return errors.New("USBReset is not under test here")
}

// PowerBudget is the runner's own deadline for one cycle; zero means it has
// none and the route falls back to its default.
func (g *gangedRunner) PowerBudget() time.Duration { return g.budget }

func (g *gangedRunner) PortPower(ctx context.Context, hostID, devpath string) error {
	return g.PortPowerWithDomain(ctx, hostID, devpath, nil)
}

func (g *gangedRunner) PortPowerWithDomain(ctx context.Context, hostID, devpath string, ack []string) error {
	g.mu.Lock()
	g.calls = append(g.calls, powerCall{
		hostID: hostID, devpath: devpath, acknowledged: append([]string(nil), ack...),
	})
	g.mu.Unlock()
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	if g.hold != nil {
		<-g.hold
	}
	for _, p := range g.ports {
		if p == devpath || slices.Contains(ack, p) {
			continue
		}
		return fmt.Errorf("node: %w: cycling %s would also darken %s, which nobody authorised",
			node.ErrRefused, devpath, p)
	}
	if g.hang {
		<-ctx.Done()
		return ctx.Err()
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

// liveAgent serves the real node.Agent for agentHostID over httptest, with the
// token the agent expects, and records its address as the fixture host's
// node_endpoint.
func liveAgent(t *testing.T, f *powerFixture, agentHostID, token string) string {
	t.Helper()
	agent, err := node.New(node.Config{
		Pool: f.pool, HostID: agentHostID, Token: token, Logger: quietLogger(),
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
	return srv.URL
}

// clientFor records endpoint as the fixture host's node_endpoint and returns
// the production client wired exactly as cmd/farmd wires it: a DBResolver over
// the same pool, speaking token.
func clientFor(t *testing.T, f *powerFixture, endpoint, token string) *node.Client {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE farm.hosts SET node_endpoint = $2 WHERE id = $1`, f.hostID, endpoint); err != nil {
		t.Fatalf("recording the agent endpoint: %v", err)
	}
	c, err := node.NewClient(node.ClientConfig{
		Resolver:     node.NewDBResolver(f.pool),
		Token:        token,
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
// Falsify: drop the hostRunner == nil arm in handleSlotPower.
func TestSlotPowerWithoutAHostAgentAnswers503AndOpensNoRow(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	s := newPowerServer(t, pool)

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", code, out)
	}
	errCode, msg, detail := errorEnvelope(t, out)
	if errCode != CodeUnavailable || !strings.Contains(msg, "no host agent") {
		t.Fatalf("error = %s %q, want %s naming the missing agent", errCode, msg, CodeUnavailable)
	}
	if detail["fault"] != faultConfiguration {
		t.Fatalf("detail.fault = %v, want %q", detail["fault"], faultConfiguration)
	}
	if rows := attemptsFor(t, pool, f.slotID); len(rows) != 0 {
		t.Fatalf("%d attempt row(s) were opened with nothing to close them: %+v", len(rows), rows)
	}
	if n := auditOutcomes(t, pool, f.slotID, "unavailable"); n != 1 {
		t.Fatalf("%d audit row(s) say the request was unavailable, want 1", n)
	}
}

// TestSlotPowerWithoutAnAgentAddressIsAConfigurationFault. A runner is wired,
// but the slot's host has no farm.hosts.node_endpoint: there is nobody to
// ask, and the answer is the same 503 as having no runner at all — not a 409
// "refused" row, which would say an agent looked at the port and declined.
//
// Falsify: drop the nodeEndpoint == nil arm in handleSlotPower.
func TestSlotPowerWithoutAnAgentAddressIsAConfigurationFault(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	f.forgetAgentAddress(t)
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}}
	s := newPowerServer(t, pool, WithHostRunner(g))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", code, out)
	}
	errCode, msg, detail := errorEnvelope(t, out)
	if errCode != CodeUnavailable || !strings.Contains(msg, "node_endpoint") {
		t.Fatalf("error = %s %q, want %s naming the missing address", errCode, msg, CodeUnavailable)
	}
	if detail["fault"] != faultConfiguration {
		t.Fatalf("detail.fault = %v, want %q", detail["fault"], faultConfiguration)
	}
	if rows := attemptsFor(t, pool, f.slotID); len(rows) != 0 {
		t.Fatalf("%d attempt row(s) for a host nobody could ask: %+v", len(rows), rows)
	}
	if calls := g.snapshot(); len(calls) != 0 {
		t.Fatalf("the runner was asked %d time(s) for a host with no address: %+v", len(calls), calls)
	}
	if n := auditOutcomes(t, pool, f.slotID, "unavailable"); n != 1 {
		t.Fatalf("%d audit row(s) say the request was unavailable, want 1", n)
	}
}

// TestSlotPowerCyclesTheDomainAndClosesItsOwnRow is the happy path this route
// never had: the agent is asked with every other position in the domain
// acknowledged, and the row is closed as recovered — by this handler, before it
// answers, with the honest note that ADB health is the watchdog's to confirm
// and that the row really did close.
//
// Falsify: remove the FinishAttempt call, or hand the agent nil for the
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
	if out["outcome"] != string(recovery.OutcomeRecovered) || out["confirmed"] != false {
		t.Fatalf("body = %v, want outcome recovered with confirmed=false", out)
	}
	if out["closed"] != true {
		t.Fatalf("body.closed = %v; the row was closed and the body must say so", out["closed"])
	}
	if out[recovery.DetailDisposition] != string(recovery.DispositionRecovered) {
		t.Fatalf("body.disposition = %v, want recovered", out[recovery.DetailDisposition])
	}
	if _, ok := out["attempt_id"]; !ok {
		t.Fatalf("the body does not name the attempt it closed: %v", out)
	}

	a := theOneAttempt(t, pool, f.slotID, recovery.OutcomeRecovered)
	if a.Refusal != "" {
		t.Fatalf("a recovered attempt carries a refusal: %q", a.Refusal)
	}
	if a.disposition() != string(recovery.DispositionRecovered) {
		t.Fatalf("row disposition = %q, want recovered; the ladder reads this key", a.disposition())
	}
	if got := stringsOf(a.Detail["acknowledged"]); !slices.Equal(got, []string{f.sibling}) {
		t.Fatalf("detail.acknowledged = %v, want [%s]", got, f.sibling)
	}
	if a.Detail["confirmed"] != false {
		t.Fatalf("detail.confirmed = %v; the API cannot see the device re-enumerate and must not claim to", a.Detail["confirmed"])
	}
	if want := powerCycleBudgetFor(g).String(); a.Detail["budget"] != want {
		t.Fatalf("detail.budget = %v, want %s", a.Detail["budget"], want)
	}

	calls := g.snapshot()
	if len(calls) != 1 {
		t.Fatalf("the agent was asked %d time(s), want exactly 1: %+v", len(calls), calls)
	}
	if c := calls[0]; c.hostID != f.hostID || c.devpath != f.devpath || !slices.Equal(c.acknowledged, []string{f.sibling}) {
		t.Fatalf("the agent was asked for %+v; want host %s, devpath %s, acknowledged [%s]",
			c, f.hostID, f.devpath, f.sibling)
	}

	if n := auditOutcomes(t, pool, f.slotID, string(recovery.OutcomeRecovered)); n != 1 {
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
// as 'refused' with the agent's reason and a note that the acknowledgement had
// nowhere to go, and reach the caller as 409. It is not a failed rung: the
// port was never touched.
//
// Falsify: file node.ErrRefused as failed in hostFaultOf, or drop the
// undeliverable note from cyclePortPower.
func TestSlotPowerRefusedByAGangedAgentWhenTheAcknowledgementCannotTravel(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}}
	s := newPowerServer(t, pool, WithHostRunner(targetOnlyRunner{g}))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
	errCode, msg, detail := errorEnvelope(t, out)
	if errCode != CodeConflict || !strings.Contains(msg, "nobody authorised") {
		t.Fatalf("error = %s %q, want %s carrying the agent's own reason", errCode, msg, CodeConflict)
	}
	if detail["closed"] != true || detail[recovery.DetailDisposition] != string(recovery.DispositionRefused) {
		t.Fatalf("error detail = %v, want closed=true and disposition refused", detail)
	}

	a := theOneAttempt(t, pool, f.slotID, recovery.OutcomeRefused)
	if !strings.Contains(a.Refusal, "nobody authorised") {
		t.Fatalf("refusal = %q, want the agent's reason", a.Refusal)
	}
	if a.disposition() != string(recovery.DispositionRefused) {
		t.Fatalf("row disposition = %q, want refused", a.disposition())
	}
	if note, _ := a.Detail["acknowledgement_undeliverable"].(string); !strings.Contains(note, "DomainPowerRunner") {
		t.Fatalf("detail.acknowledgement_undeliverable = %q; the row must say why the agent was asked for the target alone", note)
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
// Falsify: make hostFaultOf ignore node.ErrRefused.
func TestSlotPowerRecordsTheRealAgentsRefusal(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	endpoint := liveAgent(t, f, f.hostID+"-elsewhere", powerTestToken)
	s := newPowerServer(t, pool, WithHostRunner(clientFor(t, f, endpoint, powerTestToken)))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
	if errCode, msg, _ := errorEnvelope(t, out); errCode != CodeConflict || !strings.Contains(msg, "addressed to host") {
		t.Fatalf("error = %s %q, want %s carrying the agent's reason", errCode, msg, CodeConflict)
	}
	a := theOneAttempt(t, pool, f.slotID, recovery.OutcomeRefused)
	if !strings.Contains(a.Refusal, "addressed to host") {
		t.Fatalf("refusal = %q, want the agent's own sentence about the wrong host", a.Refusal)
	}
	if _, marked := a.Detail["fault"]; marked {
		t.Fatalf("the agent's own decline is marked as a configuration fault: %v", a.Detail)
	}
}

// TestSlotPowerTokenMismatchIsAConfigurationFaultNotARefusal. The real agent
// answers 401 to a client speaking the wrong token. Nothing about the port was
// decided; the control plane is misconfigured. That is 503 to the caller, and
// on the row — opened before the agent was asked, as every row is — the
// ladder's 'refused' with an explicit configuration marker, so nobody reading
// the timeline mistakes it for the agent declining this cycle.
//
// Falsify: drop the node.ErrUnauthorized arm of powerVerdict.
func TestSlotPowerTokenMismatchIsAConfigurationFaultNotARefusal(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	endpoint := liveAgent(t, f, f.hostID, powerTestToken)
	s := newPowerServer(t, pool, WithHostRunner(clientFor(t, f, endpoint, "not-"+powerTestToken)))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", code, out)
	}
	errCode, msg, detail := errorEnvelope(t, out)
	if errCode != CodeUnavailable || !strings.Contains(msg, "token") {
		t.Fatalf("error = %s %q, want %s naming the token", errCode, msg, CodeUnavailable)
	}
	if detail["fault"] != faultConfiguration {
		t.Fatalf("detail.fault = %v, want %q", detail["fault"], faultConfiguration)
	}
	a := theOneAttempt(t, pool, f.slotID, recovery.OutcomeRefused)
	if a.Detail["fault"] != faultConfiguration || a.disposition() != string(recovery.DispositionRefused) {
		t.Fatalf("row = %+v; want disposition refused with fault=configuration", a)
	}
	if !strings.Contains(a.Refusal, "401") {
		t.Fatalf("refusal = %q, want the client's own sentence about the 401", a.Refusal)
	}
	if n := auditOutcomes(t, pool, f.slotID, string(recovery.OutcomeRefused)); n != 1 {
		t.Fatalf("%d audit row(s) record the refusal, want 1", n)
	}
}

// TestSlotPowerWithAnUnreachableAgentIsUnreachableNotFailed. Nothing
// answered, so nothing is known about the port. The row reads exactly as the
// ladder would write it — outcome per recovery.DispositionUnreachable, with
// disposition 'unreachable' and a refusal saying the agent could not be
// reached — and never as 'failed', which would be evidence against a handset
// nobody touched.
//
// Falsify: file node.ErrUnreachable as failed in hostFaultOf.
func TestSlotPowerWithAnUnreachableAgentIsUnreachableNotFailed(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	// A listener closed before the call: the dial is refused at once.
	dead := httptest.NewServer(http.NotFoundHandler())
	endpoint := dead.URL
	dead.Close()
	s := newPowerServer(t, pool, WithHostRunner(clientFor(t, f, endpoint, powerTestToken)))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %v", code, out)
	}
	errCode, _, detail := errorEnvelope(t, out)
	if errCode != CodeHostAgent || detail[recovery.DetailDisposition] != string(recovery.DispositionUnreachable) {
		t.Fatalf("error = %s with detail %v, want %s and disposition unreachable", errCode, detail, CodeHostAgent)
	}
	a := theOneAttempt(t, pool, f.slotID, recovery.DispositionUnreachable.Outcome())
	if a.disposition() != string(recovery.DispositionUnreachable) {
		t.Fatalf("row disposition = %q, want unreachable: %+v", a.disposition(), a)
	}
	if !strings.Contains(a.Refusal, "could not be reached") {
		t.Fatalf("refusal = %q, want the ladder's sentence about an unreachable agent", a.Refusal)
	}
	if _, timedOut := a.Detail["timed_out"]; timedOut {
		t.Fatalf("a dial refused at once is marked as a timeout: %v", a.Detail)
	}
}

// TestSlotPowerBudgetElapsingIsRecordedAsUnreachable. The runner never
// answers; the route's own budget — the runner's, when it has one — runs out.
// That is a statement about the host, filed as the ladder files it: disposition
// unreachable, timed out, 504 to the caller.
//
// Falsify: drop the BudgetElapsed arm of powerVerdict, or ignore the runner's
// PowerBudget in powerCycleBudgetFor.
func TestSlotPowerBudgetElapsingIsRecordedAsUnreachable(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}, hang: true, budget: 20 * time.Millisecond}
	s := newPowerServer(t, pool, WithHostRunner(g))

	started := time.Now()
	code, out := postPower(t, s, f.slotID)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %v", code, out)
	}
	if took := time.Since(started); took > g.budget+powerBudgetGrace+5*time.Second {
		t.Fatalf("the route took %s against a %s budget; the runner's own budget was not honoured", took, g.budget)
	}
	errCode, msg, detail := errorEnvelope(t, out)
	if errCode != CodeTimeout || !strings.Contains(msg, g.budget.String()) {
		t.Fatalf("error = %s %q, want %s naming the %s budget", errCode, msg, CodeTimeout, g.budget)
	}
	if detail["timed_out"] != true {
		t.Fatalf("detail.timed_out = %v, want true", detail["timed_out"])
	}
	a := theOneAttempt(t, pool, f.slotID, recovery.DispositionUnreachable.Outcome())
	if a.disposition() != string(recovery.DispositionUnreachable) || a.Detail["timed_out"] != true {
		t.Fatalf("row = %+v; want disposition unreachable and timed_out", a)
	}
	if !strings.Contains(a.Refusal, "ran out of its action budget") {
		t.Fatalf("refusal = %q, want the ladder's sentence about the budget", a.Refusal)
	}
}

// TestSlotPowerRecordsAHardwareFailureAsFailed. The agent looked, cycled the
// port, and the device stayed dark. That is the one answer that may read as a
// failed rung: no refusal text, no configuration marker, and a 502 that says
// the hardware was tried.
//
// Falsify: answer 200 from the default arm of powerVerdict.
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
	if detail[recovery.DetailDisposition] != string(recovery.DispositionFailed) {
		t.Fatalf("a rung the agent performed is filed as %v, want failed", detail[recovery.DetailDisposition])
	}
	a := theOneAttempt(t, pool, f.slotID, recovery.OutcomeFailed)
	if a.Refusal != "" {
		t.Fatalf("a failed rung carries refusal text %q; the agent did not decline, it tried", a.Refusal)
	}
	if _, marked := a.Detail["fault"]; marked {
		t.Fatalf("a hardware failure is marked as a configuration fault: %v", a.Detail)
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
	a := theOneAttempt(t, pool, f.slotID, recovery.OutcomeRefused)
	if !strings.Contains(a.Refusal, "live lease") {
		t.Fatalf("refusal = %q, want it to name the live lease", a.Refusal)
	}
	// A row the route refuses before asking anyone carries the same key the
	// rows the agent's answer closes do, or the dashboard shows a tier-4
	// attempt whose verdict nobody wrote down.
	if a.disposition() != string(recovery.DispositionRefused) {
		t.Fatalf("row disposition = %q, want refused; every tier-4 row this route writes carries one",
			a.disposition())
	}
}

// TestSlotPowerRefusesASecondCycleWhileOneIsInFlight. Two operators press the
// button at once. The first opens the row and is cycling; the second must be
// told so — 409, naming the attempt — and must neither open a row nor reach
// the agent, or VBUS is cut twice. The row carries the device in the slot, so
// the ladder's own busy check sees it too.
//
// Falsify: drop the in-flight SELECT from openPowerAttempt, or write NULL for
// device_id there.
func TestSlotPowerRefusesASecondCycleWhileOneIsInFlight(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	deviceID := f.device(t, f.slotID)
	g := &gangedRunner{
		ports:   []string{f.devpath, f.sibling},
		entered: make(chan struct{}, 1),
		hold:    make(chan struct{}),
	}
	s := newPowerServer(t, pool, WithHostRunner(g))

	type answer struct {
		code int
		out  map[string]any
	}
	first := make(chan answer, 1)
	go func() {
		code, out := postPower(t, s, f.slotID)
		first <- answer{code, out}
	}()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first cycle never reached the runner")
	}

	// Now, with the first cycle in flight.
	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("second request: status = %d, want 409: %v", code, out)
	}
	errCode, msg, _ := errorEnvelope(t, out)
	rows := attemptsFor(t, pool, f.slotID)
	if len(rows) != 1 {
		t.Fatalf("%d attempt row(s) while one cycle is in flight, want exactly 1: %+v", len(rows), rows)
	}
	if errCode != CodeConflict || !strings.Contains(msg, "already in flight") ||
		!strings.Contains(msg, fmt.Sprintf("attempt %d", rows[0].ID)) {
		t.Fatalf("error = %s %q, want %s naming attempt %d as in flight", errCode, msg, CodeConflict, rows[0].ID)
	}
	if rows[0].DeviceID != deviceID {
		t.Fatalf("the open row carries device %q, want %s so the ladder's busy check can see it", rows[0].DeviceID, deviceID)
	}

	close(g.hold)
	select {
	case a := <-first:
		if a.code != http.StatusOK {
			t.Fatalf("first request: status = %d, want 200: %v", a.code, a.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first cycle never answered after the runner was released")
	}

	if calls := g.snapshot(); len(calls) != 1 {
		t.Fatalf("the runner was asked %d time(s), want exactly 1: %+v", len(calls), calls)
	}
	theOneAttempt(t, pool, f.slotID, recovery.OutcomeRecovered)
	if n := auditOutcomes(t, pool, f.slotID, string(recovery.OutcomeRefused)); n != 1 {
		t.Fatalf("%d audit row(s) record the in-flight refusal, want 1", n)
	}
}

// TestSlotPowerRefusesWhileTheLadderHasTheDeviceOpen. The recovery loop has an
// attempt open on the device in this slot — keyed by device, as its rows are.
// An operator's cycle landing in the middle of the ladder's rung is the same
// double reset as two operators, and is refused the same way.
//
// Falsify: drop the device_id arm of the in-flight predicate in openPowerAttempt.
func TestSlotPowerRefusesWhileTheLadderHasTheDeviceOpen(t *testing.T) {
	pool := powerDB(t)
	f := newPowerFixture(t, pool)
	deviceID := f.device(t, f.slotID)
	var ladderRow int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO farm.recovery_attempts (device_id, host_id, tier, detail)
VALUES ($1::uuid, $2, 2, '{"from":"the ladder"}'::jsonb)
RETURNING id`, deviceID, f.hostID).Scan(&ladderRow); err != nil {
		t.Fatalf("opening the ladder's row: %v", err)
	}
	g := &gangedRunner{ports: []string{f.devpath, f.sibling}}
	s := newPowerServer(t, pool, WithHostRunner(g))

	code, out := postPower(t, s, f.slotID)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
	if errCode, msg, _ := errorEnvelope(t, out); errCode != CodeConflict ||
		!strings.Contains(msg, fmt.Sprintf("attempt %d", ladderRow)) || !strings.Contains(msg, "tier 2") {
		t.Fatalf("error = %s %q, want %s naming the ladder's attempt %d at tier 2", errCode, msg, CodeConflict, ladderRow)
	}
	if rows := attemptsFor(t, pool, f.slotID); len(rows) != 0 {
		t.Fatalf("%d tier-4 row(s) opened under the ladder's open attempt: %+v", len(rows), rows)
	}
	if calls := g.snapshot(); len(calls) != 0 {
		t.Fatalf("the runner was asked %d time(s) under the ladder's open attempt", len(calls))
	}
}

// TestSlotPowerFixtureLeavesNothing is the cleanup proof: after a fixture that
// exercised the route has been torn down, nothing under its host id remains in
// any table the route or the fixture writes.
//
// Falsify: remove any statement from powerFixture.remove.
func TestSlotPowerFixtureLeavesNothing(t *testing.T) {
	pool := powerDB(t)
	var hostID, tenant string
	var slotID int64
	t.Run("seed and exercise", func(t *testing.T) {
		f := newPowerFixture(t, pool)
		f.leaseSibling(t, "allow_port_power_cycle")
		hostID, tenant, slotID = f.hostID, f.tenant, f.slotID
		s := newPowerServer(t, pool, WithHostRunner(&gangedRunner{ports: []string{f.devpath, f.sibling}}))
		if code, out := postPower(t, s, f.slotID); code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %v", code, out)
		}
	})
	for _, probe := range []struct {
		q   string
		arg any
	}{
		{`SELECT count(*) FROM farm.hosts WHERE id = $1`, hostID},
		{`SELECT count(*) FROM farm.slots WHERE host_id = $1`, hostID},
		{`SELECT count(*) FROM farm.hubs WHERE host_id = $1`, hostID},
		{`SELECT count(*) FROM farm.power_domains WHERE host_id = $1`, hostID},
		{`SELECT count(*) FROM farm.devices WHERE host_id = $1`, hostID},
		{`SELECT count(*) FROM farm.recovery_attempts WHERE host_id = $1`, hostID},
		{`SELECT count(*) FROM farm.leases WHERE tenant_id = $1`, tenant},
		{`SELECT count(*) FROM farm.jobs WHERE tenant_id = $1`, tenant},
		{`SELECT count(*) FROM farm.tenants WHERE id = $1`, tenant},
		{`SELECT count(*) FROM farm.recovery_attempts WHERE slot_id = $1`, slotID},
		{`SELECT count(*) FROM farm.events WHERE slot_id = $1`, slotID},
		{`SELECT count(*) FROM farm.audit_log WHERE subject = 'slot:' || $1::bigint`, slotID},
	} {
		if n := countRows(t, pool, probe.q, probe.arg); n != 0 {
			t.Errorf("%d row(s) left behind by %s", n, probe.q)
		}
	}
}

// ---------------------------------------------------------------------------
// Pure cases: no database
// ---------------------------------------------------------------------------

// TestSlotPowerBudgetCoversTheNodeClientsDeadline ties the route's budget to
// the node client's tier-4 deadline, from both directions: the production
// client states its own and is taken at its word, and the fallback for a runner
// that states none still covers it. The client is deliberately given longer
// than the agent's own opBudget, and a route budget shorter than the client's
// would file a cycle completing normally at eighty seconds as a timeout.
//
// Falsify: return recovery.DefaultActionTimeout alone from powerCycleBudgetFor,
// or drop node.Client.PowerBudget.
func TestSlotPowerBudgetCoversTheNodeClientsDeadline(t *testing.T) {
	if got := powerCycleBudgetFor(nil); got < node.DefaultPowerTimeout {
		t.Fatalf("fallback budget %s undercuts node.DefaultPowerTimeout %s", got, node.DefaultPowerTimeout)
	} else if got < recovery.DefaultActionTimeout {
		t.Fatalf("fallback budget %s undercuts recovery.DefaultActionTimeout %s", got, recovery.DefaultActionTimeout)
	}
	if got := powerCycleBudgetFor(&gangedRunner{}); got != powerCycleBudgetFor(nil) {
		t.Fatalf("a runner with no budget of its own got %s, want the fallback %s", got, powerCycleBudgetFor(nil))
	}
	if got := powerCycleBudgetFor(&gangedRunner{budget: 7 * time.Second}); got != 7*time.Second {
		t.Fatalf("a runner stating 7s got %s", got)
	}

	// The runner cmd/farmd actually wires. Its own figure is the one the route
	// must use: bounded any tighter, this process's context fires first and the
	// row records our clock running out instead of what the agent said.
	c, err := node.NewClient(node.ClientConfig{
		Resolver: node.StaticResolver{}, Token: powerTestToken, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("node.NewClient: %v", err)
	}
	if got := powerCycleBudgetFor(c); got != node.DefaultPowerTimeout {
		t.Fatalf("the production client's budget reads as %s, want its own %s",
			got, node.DefaultPowerTimeout)
	}
}

// TestWithHostRunnerIgnoresATypedNil. A nil *node.Client stored in the
// interface is not == nil, and a server holding one would call methods on it
// instead of answering that no agent is configured.
//
// Falsify: drop the reflect check from WithHostRunner.
func TestWithHostRunnerIgnoresATypedNil(t *testing.T) {
	s := &Server{}
	WithHostRunner((*node.Client)(nil))(s)
	if s.hostRunner != nil {
		t.Fatalf("a typed nil was stored as the host runner: %#v", s.hostRunner)
	}
	WithHostRunner(nil)(s)
	if s.hostRunner != nil {
		t.Fatalf("an untyped nil was stored as the host runner: %#v", s.hostRunner)
	}
	g := &gangedRunner{}
	WithHostRunner(g)(s)
	if s.hostRunner != recovery.HostRunner(g) {
		t.Fatalf("a real runner was not stored: %#v", s.hostRunner)
	}
}

// rungFault is a runner error that speaks recovery.RungFault rather than
// either package's sentinels.
type rungFault struct {
	msg                  string
	refused, unreachable bool
}

func (e rungFault) Error() string         { return e.msg }
func (e rungFault) RungRefused() bool     { return e.refused }
func (e rungFault) HostUnreachable() bool { return e.unreachable }

// TestPowerVerdictSpeaksTheLaddersVocabulary is the table: every shape of
// runner error, in internal/node's vocabulary and in internal/recovery's, and
// the outcome, disposition, status and marker each one earns. The outcome is
// always recovery's own mapping of the disposition — the row this route closes
// must read exactly like one the ladder closed.
//
// Falsify: swap any arm of powerVerdict.
func TestPowerVerdictSpeaksTheLaddersVocabulary(t *testing.T) {
	const budget = 3 * time.Minute
	cases := []struct {
		name        string
		err         error
		disposition recovery.Disposition
		status      int
		code        string
		fault       string
		timedOut    bool
		level       slog.Level
		metric      string
	}{
		{"success", nil, recovery.DispositionRecovered, http.StatusOK, "", "", false, slog.LevelInfo, "ok"},
		{"node refused", fmt.Errorf("node: %w: declined", node.ErrRefused),
			recovery.DispositionRefused, http.StatusConflict, CodeConflict, "", false, slog.LevelWarn, "refused"},
		{"recovery refused", fmt.Errorf("%w: declined", recovery.ErrRungRefused),
			recovery.DispositionRefused, http.StatusConflict, CodeConflict, "", false, slog.LevelWarn, "refused"},
		{"RungFault refused", rungFault{msg: "declined", refused: true},
			recovery.DispositionRefused, http.StatusConflict, CodeConflict, "", false, slog.LevelWarn, "refused"},
		{"node unreachable", fmt.Errorf("node: %w: dial tcp: connection refused", node.ErrUnreachable),
			recovery.DispositionUnreachable, http.StatusBadGateway, CodeHostAgent, "", false, slog.LevelWarn, "failed"},
		{"recovery unreachable", fmt.Errorf("%w: gone", recovery.ErrHostUnreachable),
			recovery.DispositionUnreachable, http.StatusBadGateway, CodeHostAgent, "", false, slog.LevelWarn, "failed"},
		{"RungFault unreachable", rungFault{msg: "gone", unreachable: true},
			recovery.DispositionUnreachable, http.StatusBadGateway, CodeHostAgent, "", false, slog.LevelWarn, "failed"},
		{"client budget elapsed, in its own words", fmt.Errorf("node: %w: no answer within this client's budget: %w",
			node.ErrUnreachable, context.DeadlineExceeded),
			recovery.DispositionUnreachable, http.StatusBadGateway, CodeHostAgent, "", false, slog.LevelWarn, "failed"},
		{"our budget elapsed", fmt.Errorf("cut short by the caller: %w", context.DeadlineExceeded),
			recovery.DispositionUnreachable, http.StatusGatewayTimeout, CodeTimeout, "", true, slog.LevelWarn, "failed"},
		{"token out of step", fmt.Errorf("node: %w: %w: 401", node.ErrRefused, node.ErrUnauthorized),
			recovery.DispositionRefused, http.StatusServiceUnavailable, CodeUnavailable, faultConfiguration, false, slog.LevelWarn, "unavailable"},
		{"platform cannot", fmt.Errorf("node: %w: %w: 501", node.ErrRefused, node.ErrNotSupported),
			recovery.DispositionRefused, http.StatusServiceUnavailable, CodeUnavailable, faultConfiguration, false, slog.LevelWarn, "unavailable"},
		{"hardware failed", errors.New("node: attempted and failed (HTTP 500): port stayed dark"),
			recovery.DispositionFailed, http.StatusBadGateway, CodeHostAgent, "", false, slog.LevelError, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := powerVerdict(tc.err, "port_power", "rack-a", "usb:3-1.4", budget)
			if v.disposition != tc.disposition {
				t.Errorf("disposition = %q, want %q", v.disposition, tc.disposition)
			}
			if v.outcome != tc.disposition.Outcome() {
				t.Errorf("outcome = %q, want recovery's own %q", v.outcome, tc.disposition.Outcome())
			}
			if v.status != tc.status || v.code != tc.code {
				t.Errorf("answer = %d %q, want %d %q", v.status, v.code, tc.status, tc.code)
			}
			if v.fault != tc.fault {
				t.Errorf("fault = %q, want %q", v.fault, tc.fault)
			}
			if v.timedOut != tc.timedOut {
				t.Errorf("timedOut = %v, want %v", v.timedOut, tc.timedOut)
			}
			if v.level != tc.level || v.metric != tc.metric {
				t.Errorf("level/metric = %v/%q, want %v/%q", v.level, v.metric, tc.level, tc.metric)
			}
			wantRefusal := tc.disposition == recovery.DispositionRefused || tc.disposition == recovery.DispositionUnreachable
			if (v.refusal != "") != wantRefusal {
				t.Errorf("refusal = %q; want one exactly when refused or unreachable", v.refusal)
			}
			if tc.err != nil && v.message == "" {
				t.Errorf("no message for the caller")
			}
			if tc.err != nil && wantRefusal && !strings.Contains(v.refusal, "port_power") {
				t.Errorf("refusal %q does not name the rung the way the ladder does", v.refusal)
			}
		})
	}
}
