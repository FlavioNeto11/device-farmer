package janitor

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	// Registers the "pgx" database/sql driver as a side effect. goose speaks
	// database/sql, and the scratch database has to be migrated before a pool
	// is pointed at it.
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/recovery"
	"github.com/flaviopadilha/device-farmer/migrations"
)

// =====================================================================
// Harness
//
// These tests run against a REAL PostgreSQL because everything under test is a
// WHERE clause. The whole question this package answers — "is anything still
// executing this row?" — is asked in SQL, so a stubbed database could only
// prove that the stub returned what it was told to.
//
// The database is a scratch one, created and migrated per run and dropped
// afterwards. It is NEVER the demo database: these tests deliberately
// manufacture dead leases and abandoned attempts, and a sweeper test that could
// reach a live farm is a test that could close the records of somebody's
// six-hour job.
//
// # Why the sweeping tests do not run in parallel
//
// A sweep is table-wide. Every fixture builds a Janitor of its own, on its own
// advisory-lock key, so under t.Parallel() they are all leaders at once and each
// one sweeps every other fixture's rows. That breaks these tests in two ways,
// and the second is the one that matters:
//
//   - a fixture is built one INSERT at a time, and between "job at 'running'"
//     and "its lease exists" the row is a textbook orphan. Another test's
//     janitor requeues it — correctly — and the test that was still writing its
//     fixture then fails on a state it never asked for. That was observed, not
//     imagined;
//   - every assertion of the form "the sweep closed this row" degrades to "some
//     sweep closed this row". A guard could be deleted from the janitor under
//     test and the row would still be closed by somebody else's.
//
// So any test that runs a cycle runs alone: Go starts top-level tests one at a
// time and a parallel test parks itself immediately, so a test that never calls
// t.Parallel() has the database to itself. The tests that only read the schema
// or validate a Config keep t.Parallel(), because they sweep nothing.
//
// Rows outlive their test, which is deliberate: a later test's janitor sees
// every earlier fixture's live placement and has to leave it alone, so the
// protection is re-asserted for free on every subsequent cycle.
// =====================================================================

var testPool *pgxpool.Pool

// fixtureSeq namespaces each fixture's rows and each Janitor's advisory lock
// key, so tests never see each other's jobs and never queue behind each other's
// leader election.
var fixtureSeq atomic.Int64

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"janitor tests: %v\n"+
				"These tests need a PostgreSQL they may CREATE DATABASE on; they never touch the\n"+
				"database named in the DSN itself. Either point %s at a scratch server and\n"+
				"grant CREATEDB to that role, or unset %s to skip the SQL-backed tests.\n",
			err, config.EnvDatabaseURL, config.EnvDatabaseURL)
		os.Exit(1)
	}
	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	dsn := os.Getenv(config.EnvDatabaseURL)
	if dsn == "" {
		// No database configured: the SQL-backed tests skip themselves and the
		// source-scan barrier test still runs, so `go test ./...` passes on a
		// laptop with no Postgres.
		return m.Run(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, drop, err := setupScratchDB(ctx, dsn)
	if err != nil {
		return 0, err
	}
	testPool = pool

	code := m.Run()

	testPool = nil
	pool.Close()
	drop()
	return code, nil
}

// setupScratchDB creates a database of its own, migrates it with the embedded
// migration set, and returns a pool onto it plus a function that drops it.
//
// The migrations come from the migrations package rather than from a path, so
// the schema under test is the schema the binaries carry.
func setupScratchDB(ctx context.Context, dsn string) (*pgxpool.Pool, func(), error) {
	adminCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", config.EnvDatabaseURL, err)
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, nil, fmt.Errorf("name scratch database: %w", err)
	}
	name := fmt.Sprintf("df_janitor_test_%d_%s", os.Getpid(), hex.EncodeToString(suffix[:]))

	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", adminCfg.Database, err)
	}
	defer admin.Close(ctx)

	// The migration set creates cluster-wide roles, so two test binaries
	// migrating their own scratch databases at once would both find a role
	// missing and both try to create it. A session advisory lock held across
	// the create and the migrate serialises them.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", testMigrationLockKey); err != nil {
		return nil, nil, fmt.Errorf("take the test migration lock: %w", err)
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer ucancel()
		_, _ = admin.Exec(uctx, "SELECT pg_advisory_unlock($1)", testMigrationLockKey)
	}()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, nil, fmt.Errorf("create scratch database %s: %w", name, err)
	}

	drop := func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		conn, err := pgx.ConnectConfig(dctx, adminCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "janitor tests: scratch database %s left behind: %v\n", name, err)
			return
		}
		defer conn.Close(dctx)
		if _, err := conn.Exec(dctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			fmt.Fprintf(os.Stderr, "janitor tests: scratch database %s left behind: %v\n", name, err)
		}
	}

	if err := migrateScratchDB(ctx, adminCfg, name); err != nil {
		drop()
		return nil, nil, err
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		drop()
		return nil, nil, fmt.Errorf("parse %s: %w", config.EnvDatabaseURL, err)
	}
	poolCfg.ConnConfig.Database = name
	// Every Janitor under test parks one connection on its leadership lock for
	// the length of its subtest, and the subtests run in parallel.
	poolCfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		drop()
		return nil, nil, fmt.Errorf("open pool on %s: %w", name, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		drop()
		return nil, nil, fmt.Errorf("ping %s: %w", name, err)
	}
	return pool, drop, nil
}

// testMigrationLockKey guards scratch-database creation across processes. It is
// deliberately NOT cmd/farmd's key: a test run must not block on, or block, an
// operator migrating a real database.
const testMigrationLockKey int64 = 0x6466_7465_7374_0001

// gooseMu serialises use of goose's package-level state, which is global to the
// process.
var gooseMu sync.Mutex

func migrateScratchDB(ctx context.Context, adminCfg *pgx.ConnConfig, name string) error {
	cfg := adminCfg.Copy()
	cfg.Database = name

	sqlDSN := stdlib.RegisterConnConfig(cfg)
	defer stdlib.UnregisterConnConfig(sqlDSN)

	db, err := sql.Open("pgx", sqlDSN)
	if err != nil {
		return fmt.Errorf("open %s for migration: %w", name, err)
	}
	defer db.Close()

	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(migrations.Goose())
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("migrate %s: %w", name, err)
	}
	return nil
}

func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skipf("%s is not set; skipping the PostgreSQL-backed janitor tests", config.EnvDatabaseURL)
	}
	return testPool
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// fixture is one test's private slice of the farm. Nothing it creates is
// visible to another fixture's assertions, because every job, lease and step it
// makes hangs off its own tenant and host.
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool

	tag      string
	poolID   string
	tenantID string
	queueID  string
	hostID   string
	deviceID string

	lockKey int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	p := requireDB(t)
	ctx := t.Context()

	seq := fixtureSeq.Add(1)
	tag := fmt.Sprintf("%04d", seq)
	f := &fixture{
		t: t, pool: p, tag: tag,
		poolID:   "pool-" + tag,
		tenantID: "tenant-" + tag,
		queueID:  "queue-" + tag,
		hostID:   "host-" + tag,
		// A key per fixture: leader election is a real advisory lock, and one
		// shared key would serialise every parallel subtest behind the first.
		lockKey: DefaultLockKey + seq,
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding fixture %s failed: %v\nstatement: %s\nargs: %v", f.hostID, err, q, args)
		}
	}

	exec(`INSERT INTO farm.racks (id) VALUES ($1)`, "rack-"+tag)
	exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ($1, $2, '127.0.0.1:5037')`,
		f.hostID, "rack-"+tag)
	exec(`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3)`, f.hostID)
	exec(`INSERT INTO farm.power_domains (host_id, kind, control) VALUES ($1, 'per_port', 'uhubctl')`, f.hostID)
	exec(`INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
	      SELECT $1, c.id, '3-1', 8, true FROM farm.controllers c WHERE c.host_id = $1`, f.hostID)
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, f.tenantID)
	exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, f.queueID, f.tenantID)
	exec(`INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number,
	                              usb_path, topo_path, rack_slot)
	      SELECT $1, h.id, pd.id, 1, '3-1.1', ('x' || $2 || '.p1')::ltree, 'R-' || $2 || '-P1'
	        FROM farm.hubs h, farm.power_domains pd
	       WHERE h.host_id = $1 AND pd.host_id = $1`, f.hostID, tag)

	if err := p.QueryRow(ctx,
		`INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id,
		                           manufacturer, model, sdk_int)
		 SELECT 'df-' || md5($1), 'SER-' || $1, $2, $3, s.id, 'Google', 'Pixel Test', 34
		   FROM farm.slots s WHERE s.host_id = $3
		 RETURNING id::text`, tag, f.poolID, f.hostID).Scan(&f.deviceID); err != nil {
		t.Fatalf("seeding device for %s: %v", f.hostID, err)
	}
	exec(`INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
	      SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy'
	        FROM farm.devices d WHERE d.id = $1::uuid`, f.deviceID)

	return f
}

// newJob inserts a job already at 'running', which is where every orphan this
// package sweeps was left.
func (f *fixture) newJob(t *testing.T, attempt, maxAttempts int) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(),
		`INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, attempt, max_attempts,
		                        started_at, spec)
		 VALUES ($1, $2, $3, 'running', $4, $5, now(), '{"steps":[]}'::jsonb)
		 RETURNING id::text`,
		f.tenantID, f.queueID, f.poolID, attempt, maxAttempts).Scan(&id)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return id
}

// newLease writes a lease row directly.
//
// It does NOT go through farm.lease_acquire, and that is deliberate: these
// tests need leases in states an allocator will not produce on demand
// ('expired' with a released_at in the past, 'suspect' without waiting out a
// TTL), and the janitor's whole contract is stated against the columns of
// farm.leases rather than against the function that writes them.
func (f *fixture) newLease(t *testing.T, jobID, state string, releasedAgo time.Duration) (id string, fence int64) {
	t.Helper()

	var released any
	var reason any
	if state == "expired" || state == "released" {
		released = fmt.Sprintf("%d microseconds", int64(releasedAgo/time.Microsecond))
		reason = "holder_expired"
	}

	err := f.pool.QueryRow(t.Context(), `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, state,
                         ttl, grace, expires_at, reclaimable_at,
                         released_at, release_reason)
SELECT d.id, d.current_slot_id, $1::uuid, $2, $3,
       'pod-test', gen_random_uuid(), $4,
       interval '15 minutes', interval '30 minutes',
       now() + interval '15 minutes', now() + interval '45 minutes',
       CASE WHEN $5::interval IS NULL THEN NULL ELSE now() - $5::interval END,
       $6::text
  FROM farm.devices d WHERE d.id = $7::uuid
RETURNING id::text, fence`,
		jobID, f.tenantID, f.queueID, state, released, reason, f.deviceID).Scan(&id, &fence)
	if err != nil {
		t.Fatalf("insert %s lease for job %s: %v", state, jobID, err)
	}
	return id, fence
}

// newAttempt opens a farm.job_attempts row, backdated by startedAgo.
func (f *fixture) newAttempt(t *testing.T, jobID string, attempt int, leaseID *string, fence int64, startedAgo time.Duration) int64 {
	t.Helper()
	var id int64
	err := f.pool.QueryRow(t.Context(), `
INSERT INTO farm.job_attempts (job_id, attempt, device_id, lease_id, fence, started_at)
VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, now() - $6::interval)
RETURNING id`,
		jobID, attempt, f.deviceID, leaseID, fence,
		fmt.Sprintf("%d microseconds", int64(startedAgo/time.Microsecond))).Scan(&id)
	if err != nil {
		t.Fatalf("insert attempt %d for job %s: %v", attempt, jobID, err)
	}
	return id
}

// newRunningStep writes the row this package exists to close: a step marked
// 'running' whose writer may or may not still be alive.
func (f *fixture) newRunningStep(t *testing.T, jobID string, attempt, index int, kind string, startedAgo time.Duration) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state, started_at)
VALUES ($1::uuid, $2, $3, $4, $5, 'running', now() - $6::interval)`,
		jobID, attempt, index, fmt.Sprintf("s/%03d", index), kind,
		fmt.Sprintf("%d microseconds", int64(startedAgo/time.Microsecond)))
	if err != nil {
		t.Fatalf("insert step %d for job %s: %v", index, jobID, err)
	}
}

// newQueuedJob inserts a job that has never been placed: no lease, no attempt,
// no start.
func (f *fixture) newQueuedJob(t *testing.T) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(),
		`INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, spec)
		 VALUES ($1, $2, $3, 'queued', '{"steps":[]}'::jsonb)
		 RETURNING id::text`,
		f.tenantID, f.queueID, f.poolID).Scan(&id)
	if err != nil {
		t.Fatalf("insert queued job: %v", err)
	}
	return id
}

// newPlannedStep writes a step the way a future "show me the plan before it
// runs" feature would: 'pending', never started, with no attempt row behind it.
func (f *fixture) newPlannedStep(t *testing.T, jobID string, index int, kind string) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state)
VALUES ($1::uuid, 1, $2, $3, $4, 'pending')`,
		jobID, index, fmt.Sprintf("s/%03d", index), kind); err != nil {
		t.Fatalf("insert planned step %d for job %s: %v", index, jobID, err)
	}
}

// janitor builds a Janitor on this fixture's own lock key and guarantees the
// leadership connection goes back to the pool when the test ends.
func (f *fixture) janitor(t *testing.T) *Janitor {
	t.Helper()
	return f.janitorWith(t)
}

// janitorWith is the same, with the config handed to each tweak first. Tests
// that need a different batch or a logger they can read use it; nothing may
// tweak away the liveness guards, which are not configuration.
func (f *fixture) janitorWith(t *testing.T, tweaks ...func(*Config)) *Janitor {
	t.Helper()
	cfg := Config{
		Pool:    f.pool,
		LockKey: f.lockKey,
		// Zero settle: these tests backdate their fixtures explicitly, and a
		// window that silently protected everything would make every assertion
		// below pass for the wrong reason.
		Settle: time.Nanosecond,
		// The verdict pass is spaced out in production because a healthy farm
		// pays its full cost; a test runs one cycle and asserts on it, so here
		// it is due every time. Left at the default, every such assertion after
		// the first cycle would be an assertion about a pass that did not run.
		VerdictInterval: time.Nanosecond,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	j, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { j.lead.release(context.Background()) })
	return j
}

// sweep runs one full cycle and fails the test if this Janitor never became the
// leader — an assertion made against a cycle that swept nothing because it was
// a standby would be an assertion about nothing.
func (f *fixture) sweep(t *testing.T, j *Janitor) {
	t.Helper()
	j.cycle(t.Context())
	if !j.lead.held {
		t.Fatal("the janitor under test never took its advisory lock, so the cycle did nothing; " +
			"every assertion after this line would pass without exercising a sweep")
	}
}

// ---------------------------------------------------------------------------
// Readers
// ---------------------------------------------------------------------------

func (f *fixture) stepState(t *testing.T, jobID string, attempt, index int) (state string, errText *string, finished *time.Time) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(),
		`SELECT state, error, finished_at FROM farm.job_steps
		  WHERE job_id = $1::uuid AND attempt = $2 AND step_index = $3`,
		jobID, attempt, index).Scan(&state, &errText, &finished)
	if err != nil {
		t.Fatalf("read step %d of job %s: %v", index, jobID, err)
	}
	return state, errText, finished
}

func (f *fixture) attemptState(t *testing.T, id int64) (outcome *string, finished *time.Time) {
	t.Helper()
	if err := f.pool.QueryRow(t.Context(),
		`SELECT outcome, finished_at FROM farm.job_attempts WHERE id = $1`, id).
		Scan(&outcome, &finished); err != nil {
		t.Fatalf("read attempt row %d: %v", id, err)
	}
	return outcome, finished
}

func (f *fixture) jobState(t *testing.T, jobID string) (state string, errText *string, finished *time.Time) {
	t.Helper()
	if err := f.pool.QueryRow(t.Context(),
		`SELECT state, error, finished_at FROM farm.jobs WHERE id = $1::uuid`, jobID).
		Scan(&state, &errText, &finished); err != nil {
		t.Fatalf("read job %s: %v", jobID, err)
	}
	return state, errText, finished
}

// ---------------------------------------------------------------------------
// THE IMPORTANT ONE
// ---------------------------------------------------------------------------

// TestLiveLeaseProtectsALongRunningStep is the test this package is for.
//
// A shell_detached step that has been running for six hours under a live lease
// is not a fault. It is the thing the farm exists to make possible, and closing
// its row would be DeviceFarmer/STF issue #663 wearing a different hat: the
// control plane deciding from a clock, with no evidence, that a holder must be
// dead. Worse than cosmetically: the replacement process re-attaches to the same
// lease at the same fence and internal/runner resumes THAT attempt number, so
// the row this sweep would have closed is the row the resume is about to write.
//
// The subtest named "falsified" is what makes the first one mean something. It
// removes the liveness guard from the scan and asserts the step then does show
// up as sweepable, proving the guard — and not some incidental clause — is what
// protects it.
func TestLiveLeaseProtectsALongRunningStep(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 1, 3)
	leaseID, fence := f.newLease(t, jobID, "held", 0)
	attemptID := f.newAttempt(t, jobID, 1, &leaseID, fence, 6*time.Hour)
	f.newRunningStep(t, jobID, 1, 0, "shell_detached", 6*time.Hour)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.stepState(t, jobID, 1, 0); state != "running" {
		t.Fatalf("a six-hour shell_detached step under a LIVE lease is now %q, want running; "+
			"this is issue #663 with a different label on it", state)
	}
	if outcome, finished := f.attemptState(t, attemptID); outcome != nil || finished != nil {
		t.Fatalf("the attempt of a live placement was closed: outcome=%v finished=%v", outcome, finished)
	}
	if state, _, _ := f.jobState(t, jobID); state != "running" {
		t.Fatalf("job of a live placement is %q, want running", state)
	}

	t.Run("falsified", func(t *testing.T) {
		// Exactly the guard, removed and nothing else. If the constant ever
		// stops appearing in the scan verbatim, this fails loudly rather than
		// silently testing an unmodified query.
		blind := strings.Replace(stepScan, stepLiveGuard, "", 1)
		if blind == stepScan {
			t.Fatal("stepLiveGuard no longer appears in stepScan verbatim; this falsification " +
				"is comparing a query against itself and proves nothing")
		}

		rows, err := f.pool.Query(t.Context(), blind, pgInterval(time.Nanosecond), 100)
		if err != nil {
			t.Fatalf("run the falsified scan: %v", err)
		}
		defer rows.Close()

		found := false
		for rows.Next() {
			var jid, stepID, kind, lease, lstate, lreason string
			var attempt int
			var index int32
			if err := rows.Scan(&jid, &attempt, &index, &stepID, &kind, &lease, &lstate, &lreason); err != nil {
				t.Fatalf("scan the falsified result: %v", err)
			}
			if jid == jobID {
				found = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("run the falsified scan: %v", err)
		}
		if !found {
			t.Fatal("with the liveness guard removed the live six-hour step STILL did not appear " +
				"as sweepable, so the guard is not what is protecting it and the test above " +
				"would pass with the guard deleted")
		}
	})
}

// TestSuspectLeaseIsStillLive pins the other half of the liveness definition.
//
// 'suspect' means a holder has gone quiet for longer than its TTL. It releases
// nothing, it is an alert, and one heartbeat returns it to 'held'. Reading it as
// dead here would let a ten-minute partition close the records of a run that is
// still going — which is the precise shape of #663.
func TestSuspectLeaseIsStillLive(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 1, 3)
	leaseID, fence := f.newLease(t, jobID, "suspect", 0)
	attemptID := f.newAttempt(t, jobID, 1, &leaseID, fence, 2*time.Hour)
	f.newRunningStep(t, jobID, 1, 0, "shell", 2*time.Hour)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.stepState(t, jobID, 1, 0); state != "running" {
		t.Fatalf("step under a SUSPECT lease is %q, want running; suspect releases nothing "+
			"and a single heartbeat heals it", state)
	}
	if outcome, _ := f.attemptState(t, attemptID); outcome != nil {
		t.Fatalf("attempt under a suspect lease was closed as %q", *outcome)
	}
}

// TestAStepNoPlacementEverTouchedIsLeftAlone guards the other end of the same
// rule as the two tests above.
//
// "Aborted: the process running this step is gone" is a claim about a process.
// A step with no start of its own and no attempt row has no process to be gone
// — nothing ever placed it — so the claim would be false, and the row it would
// be written on is a job's PLAN, not its history.
//
// No writer produces such a row today. The schema permits it and the obvious
// next feature produces it: writing the steps out as 'pending' at submit time
// so the UI can show them before a device is chosen. Those rows sit under a
// queued job with no lease, which every other clause in the scan reads as an
// orphan — so without the evidence clause this sweeper would abort a whole
// job's plan on its first cycle, before the job ever ran, and the job would
// then be placed onto a plan it had already been told was aborted.
func TestAStepNoPlacementEverTouchedIsLeftAlone(t *testing.T) {
	f := newFixture(t)

	jobID := f.newQueuedJob(t)
	f.newPlannedStep(t, jobID, 0, "install")
	f.newPlannedStep(t, jobID, 1, "shell")

	f.sweep(t, f.janitor(t))

	for _, idx := range []int{0, 1} {
		state, errText, finished := f.stepState(t, jobID, 1, idx)
		if state != "pending" || finished != nil {
			t.Fatalf("planned step %d of a job that was never placed is state=%q finished=%v, "+
				"want pending and unfinished; nothing has ever run it, so nothing about it "+
				"can have died", idx, state, finished)
		}
		if errText != nil && *errText != "" {
			t.Fatalf("planned step %d was given an error (%q) by a sweeper that has no "+
				"evidence about it at all", idx, *errText)
		}
	}
	if state, _, _ := f.jobState(t, jobID); state != "queued" {
		t.Fatalf("a queued job is %q after a sweep, want queued", state)
	}
}

// ---------------------------------------------------------------------------
// Genuine orphans
// ---------------------------------------------------------------------------

// TestGenuineOrphanIsClosedAndRequeued is the eviction case: a jobrunner killed
// mid-step, its lease long since reclaimed. Nothing polls a job like this —
// internal/jobrunner joins farm.leases on the live states — so without this
// sweep it executes a step forever.
func TestGenuineOrphanIsClosedAndRequeued(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 1, 3)
	leaseID, fence := f.newLease(t, jobID, "expired", 20*time.Minute)
	attemptID := f.newAttempt(t, jobID, 1, &leaseID, fence, time.Hour)
	f.newRunningStep(t, jobID, 1, 0, "install", time.Hour)
	f.newRunningStep(t, jobID, 1, 1, "shell", time.Hour)

	f.sweep(t, f.janitor(t))

	for _, idx := range []int{0, 1} {
		state, errText, finished := f.stepState(t, jobID, 1, idx)
		if state != "aborted" {
			t.Fatalf("orphaned step %d is %q, want aborted", idx, state)
		}
		if finished == nil {
			t.Fatalf("orphaned step %d was aborted with no finished_at; the row still reads "+
				"as in flight to every dashboard", idx)
		}
		if errText == nil || *errText == "" {
			t.Fatalf("orphaned step %d was aborted with no explanation; an operator at 3am "+
				"gets a state change and no reason", idx)
		}
		if !strings.Contains(*errText, leaseID) {
			t.Fatalf("step %d's error does not name the lease that ended (%s): %q",
				idx, leaseID, *errText)
		}
	}

	outcome, finished := f.attemptState(t, attemptID)
	if outcome == nil || *outcome != "abandoned" {
		t.Fatalf("attempt outcome is %v, want abandoned; 'failed' would count this against the "+
			"job and the device, and nothing here is evidence about either", outcome)
	}
	if finished == nil {
		t.Fatal("the attempt was given an outcome but left open")
	}

	// attempt 1 of 3: the work is retryable, which is the entire point of
	// abandoning it rather than failing the job.
	state, errText, jobFinished := f.jobState(t, jobID)
	if state != "queued" {
		t.Fatalf("job with attempts left is %q after the sweep, want queued", state)
	}
	if jobFinished != nil {
		t.Fatalf("a re-queued job was stamped finished at %v", jobFinished)
	}
	if errText == nil || *errText == "" {
		t.Fatal("a re-queued job carries no explanation of why it went back on the queue")
	}
}

// TestOrphanWithNoAttemptsLeftFails is the same repair at the other end of the
// user's own max_attempts. The budget is theirs; the sweeper spends the last of
// it and stops, rather than cycling the job through devices forever.
func TestOrphanWithNoAttemptsLeftFails(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 2, 2)
	leaseID, fence := f.newLease(t, jobID, "expired", 20*time.Minute)
	attemptID := f.newAttempt(t, jobID, 2, &leaseID, fence, time.Hour)
	f.newRunningStep(t, jobID, 2, 0, "shell", time.Hour)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.stepState(t, jobID, 2, 0); state != "aborted" {
		t.Fatalf("orphaned step is %q, want aborted", state)
	}
	if outcome, _ := f.attemptState(t, attemptID); outcome == nil || *outcome != "abandoned" {
		t.Fatalf("attempt outcome is %v, want abandoned", outcome)
	}

	state, _, finished := f.jobState(t, jobID)
	if state != "failed" {
		t.Fatalf("job at attempt 2 of max_attempts 2 is %q after the sweep, want failed", state)
	}
	if finished == nil {
		t.Fatal("a failed job was left without a finished_at; finished_at minus started_at is " +
			"how long a device was busy, and a fleet that cannot answer that cannot tell a " +
			"slow job from a wedged one")
	}
}

// TestNewerPlacementDoesNotProtectAnOldAttempt covers the second half of the
// orphan rule. The job HAS a live lease — it was placed again on another device
// — but the dead attempt's own lease is terminal, so its rows are history the
// new placement will never touch and nothing will ever resume them.
//
// The job itself must be left alone: it is running somewhere.
func TestNewerPlacementDoesNotProtectAnOldAttempt(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 2, 3)

	// Attempt 1's lease ended; attempt 2 holds a live one. Only one live lease
	// per job is possible, so the dead one is written first.
	oldLease, oldFence := f.newLease(t, jobID, "expired", 30*time.Minute)
	oldAttempt := f.newAttempt(t, jobID, 1, &oldLease, oldFence, 2*time.Hour)
	f.newRunningStep(t, jobID, 1, 0, "shell", 2*time.Hour)

	newLease, newFence := f.newLease(t, jobID, "held", 0)
	newAttempt := f.newAttempt(t, jobID, 2, &newLease, newFence, 5*time.Minute)
	f.newRunningStep(t, jobID, 2, 0, "shell_detached", 5*time.Minute)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.stepState(t, jobID, 1, 0); state != "aborted" {
		t.Fatalf("the abandoned attempt's step is %q, want aborted; its lease is terminal and "+
			"the newer placement will never write to attempt 1's rows", state)
	}
	if outcome, _ := f.attemptState(t, oldAttempt); outcome == nil || *outcome != "abandoned" {
		t.Fatalf("the abandoned attempt's outcome is %v, want abandoned", outcome)
	}

	if state, _, _ := f.stepState(t, jobID, 2, 0); state != "running" {
		t.Fatalf("the LIVE placement's step is %q, want running", state)
	}
	if outcome, _ := f.attemptState(t, newAttempt); outcome != nil {
		t.Fatalf("the live placement's attempt was closed as %q", *outcome)
	}
	if state, _, _ := f.jobState(t, jobID); state != "running" {
		t.Fatalf("a job with a live lease is %q after the sweep, want running", state)
	}
}

// TestOpenAttemptWithoutStepsIsClosed covers the process that died between its
// last step and its closeAttempt: every step terminal, the attempt open
// forever, and the job stuck at 'running' behind it.
func TestOpenAttemptWithoutStepsIsClosed(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 1, 3)
	leaseID, fence := f.newLease(t, jobID, "released", 30*time.Minute)
	attemptID := f.newAttempt(t, jobID, 1, &leaseID, fence, time.Hour)

	f.sweep(t, f.janitor(t))

	outcome, finished := f.attemptState(t, attemptID)
	if outcome == nil || *outcome != "abandoned" || finished == nil {
		t.Fatalf("an attempt open under a released lease was left open: outcome=%v finished=%v",
			outcome, finished)
	}
	if state, _, _ := f.jobState(t, jobID); state != "queued" {
		t.Fatalf("job is %q after its only attempt was abandoned, want queued", state)
	}
}

// TestJobRunningWithNoLeaseIsRequeued is the plainest form of the disease: no
// steps, no attempts, no lease, and a job that reads as executing forever
// because the one loop that would finish it only polls jobs that still have a
// lease.
func TestJobRunningWithNoLeaseIsRequeued(t *testing.T) {
	f := newFixture(t)

	jobID := f.newJob(t, 1, 3)
	f.newLease(t, jobID, "expired", time.Hour)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.jobState(t, jobID); state != "queued" {
		t.Fatalf("a job running nothing at all is %q, want queued", state)
	}
}

// ---------------------------------------------------------------------------
// The barrier
// ---------------------------------------------------------------------------

// TestSweepNeverWritesLeases is the assertion that keeps this package on the
// right side of the invariant.
//
// Every lease row is snapshotted with its xmin — the transaction that last
// wrote it — so a no-op UPDATE that changed no value is still caught. The sweep
// run in the middle is a real one: it closes an orphan, abandons its attempt and
// re-queues the job, against a fixture that also holds a live lease and a
// terminal one. If any of that reached farm.leases, the xmin of the row it
// touched moves.
func TestSweepNeverWritesLeases(t *testing.T) {
	f := newFixture(t)

	// An orphan to sweep...
	orphanJob := f.newJob(t, 1, 3)
	deadLease, deadFence := f.newLease(t, orphanJob, "expired", 30*time.Minute)
	f.newAttempt(t, orphanJob, 1, &deadLease, deadFence, time.Hour)
	f.newRunningStep(t, orphanJob, 1, 0, "install", time.Hour)

	// ...beside a live placement the sweep must not touch either.
	liveJob := f.newJob(t, 1, 3)
	liveLeaseID, liveFence := f.newLease(t, liveJob, "held", 0)
	f.newAttempt(t, liveJob, 1, &liveLeaseID, liveFence, 6*time.Hour)
	f.newRunningStep(t, liveJob, 1, 0, "shell_detached", 6*time.Hour)

	before := f.leaseSnapshot(t)
	if len(before) != 2 {
		t.Fatalf("the snapshot holds %d leases, want 2; the assertion below would be vacuous", len(before))
	}

	f.sweep(t, f.janitor(t))

	// The sweep must actually have done something, or "farm.leases is
	// unchanged" is true of a loop that did nothing at all.
	if state, _, _ := f.stepState(t, orphanJob, 1, 0); state != "aborted" {
		t.Fatalf("the orphan was not swept (step is %q), so this test proves nothing about "+
			"what a sweep does to farm.leases", state)
	}

	after := f.leaseSnapshot(t)
	if len(after) != len(before) {
		t.Fatalf("farm.leases went from %d rows to %d; this loop may not insert or delete leases",
			len(before), len(after))
	}
	for id, row := range before {
		got, ok := after[id]
		if !ok {
			t.Fatalf("lease %s disappeared during a sweep", id)
		}
		if got != row {
			t.Fatalf("lease %s was WRITTEN by the sweeper.\n before: %s\n  after: %s\n"+
				"Only internal/lease and internal/reaper may end a lease, and this loop may not "+
				"write farm.leases at all.", id, row, got)
		}
	}
}

// leaseSnapshot reads every lease with its row version, so an UPDATE that
// changed nothing is still visible as a change.
func (f *fixture) leaseSnapshot(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
SELECT l.id::text,
       l.xmin::text || '|' || l.state || '|' || l.fence::text || '|' ||
       COALESCE(l.release_reason, '-') || '|' ||
       COALESCE(l.released_at::text, '-') || '|' ||
       l.expires_at::text || '|' || l.reclaimable_at::text || '|' ||
       l.heartbeat_at::text || '|' || l.heartbeat_seq::text || '|' ||
       l.holder || '|' || l.holder_instance::text || '|' || l.holder_epoch::text || '|' ||
       l.protected::text || '|' || COALESCE(l.witness_at::text, '-') || '|' ||
       l.witness_extensions::text
  FROM farm.leases l
 WHERE l.tenant_id = $1
 ORDER BY l.id`, f.tenantID)
	if err != nil {
		t.Fatalf("snapshot farm.leases: %v", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var id, blob string
		if err := rows.Scan(&id, &blob); err != nil {
			t.Fatalf("snapshot farm.leases: %v", err)
		}
		out[id] = blob
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot farm.leases: %v", err)
	}
	return out
}

// TestPackageCannotEndALease enforces the structural half of the same barrier,
// by reading the source.
//
// The runtime test above proves one sweep wrote no lease. This one proves the
// package has no way to write one at all: no import of internal/lease (whose
// Store is the door to Release and Reclaim), no import of internal/reaper, and
// no statement in the file that names farm.leases as the target of a write. A
// future change cannot merge past it without seeing it.
//
// _test.go files are exempt for the reason the same check in internal/adbwire
// gives: this file has to name what it forbids in order to go looking for it.
func TestPackageCannotEndALease(t *testing.T) {
	t.Parallel()

	forbiddenImports := []string{
		"device-farmer/internal/lease",
		"device-farmer/internal/reaper",
	}
	// The write forms Postgres offers against a named table. UPDATE and DELETE
	// are matched with their target keyword so `UPDATE farm.jobs ... FROM
	// farm.leases` would not trip the check while `UPDATE farm.leases` does.
	forbiddenWrites := regexp.MustCompile(
		`(?is)(update\s+farm\.leases|delete\s+from\s+farm\.leases|insert\s+into\s+farm\.leases|` +
			`truncate\s+.*farm\.leases|farm\.lease_(release|reclaim|acquire|renew|expire|witness))`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		scanned++
		text := string(src)
		for _, imp := range forbiddenImports {
			if strings.Contains(text, imp) {
				t.Errorf("%s imports %q; a sweeper must not be able to end a lease, "+
					"even transitively", name, imp)
			}
		}
		if m := forbiddenWrites.FindString(text); m != "" {
			t.Errorf("%s writes farm.leases (%q). This loop reads leases to decide what is "+
				"already over; ending one belongs to internal/lease and internal/reaper alone.",
				name, strings.Join(strings.Fields(m), " "))
		}
	}

	if scanned == 0 {
		t.Fatal("the barrier scan read no production files; it is asserting nothing")
	}
}

// ---------------------------------------------------------------------------
// The other two tables
// ---------------------------------------------------------------------------

// TestBulkTargetsOfADeadRunAreClosed covers the bulk half. Bulk commands run on
// goroutines inside the API process, so a killed pod leaves both the run and its
// targets exactly where they stood.
func TestBulkTargetsOfADeadRunAreClosed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	var runID string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.bulk_runs (created_by, command, timeout, state, created_at)
VALUES ('tester', 'getprop ro.build.id', interval '60 seconds', 'running', now() - interval '1 hour')
RETURNING id::text`).Scan(&runID); err != nil {
		t.Fatalf("insert bulk run: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO farm.bulk_targets (run_id, device_id, state, started_at)
VALUES ($1::uuid, $2::uuid, 'running', now() - interval '1 hour')`, runID, f.deviceID); err != nil {
		t.Fatalf("insert bulk target: %v", err)
	}

	f.sweep(t, f.janitor(t))

	var state string
	var errText *string
	var finished *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT state, error, finished_at FROM farm.bulk_targets
		  WHERE run_id = $1::uuid AND device_id = $2::uuid`, runID, f.deviceID).
		Scan(&state, &errText, &finished); err != nil {
		t.Fatalf("read bulk target: %v", err)
	}
	if state != "error" || finished == nil {
		t.Fatalf("a bulk target running an hour past its run's own 60s timeout is state=%q "+
			"finished=%v; the executor's context expired long ago", state, finished)
	}
	if errText == nil || *errText == "" {
		t.Fatal("the closed bulk target carries no explanation")
	}

	// The run is finished too, or the operator's page still shows 0 pending,
	// 0 running and a state of 'running' — the same lie in a different table.
	var runState string
	if err := f.pool.QueryRow(ctx,
		`SELECT state FROM farm.bulk_runs WHERE id = $1::uuid`, runID).Scan(&runState); err != nil {
		t.Fatalf("read bulk run: %v", err)
	}
	if runState != "cancelled" {
		t.Fatalf("bulk run with nothing outstanding is %q, want cancelled", runState)
	}
}

// TestStaleRecoveryAttemptIsClosed is not cosmetic. internal/recovery refuses
// every new rung on a device while an attempt row on it is open and younger than
// its stale threshold, so a ladder killed mid-action blocks recovery on that
// phone until this sweep runs.
func TestStaleRecoveryAttemptIsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	var fresh, stale int64
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.recovery_attempts (device_id, host_id, tier, started_at)
VALUES ($1::uuid, $2, 1, now())
RETURNING id`, f.deviceID, f.hostID).Scan(&fresh); err != nil {
		t.Fatalf("insert fresh recovery attempt: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
INSERT INTO farm.recovery_attempts (device_id, host_id, tier, started_at)
VALUES ($1::uuid, $2, 2, now() - interval '2 hours')
RETURNING id`, f.deviceID, f.hostID).Scan(&stale); err != nil {
		t.Fatalf("insert stale recovery attempt: %v", err)
	}

	f.sweep(t, f.janitor(t))

	var outcome *string
	var finished *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT outcome, finished_at FROM farm.recovery_attempts WHERE id = $1`, stale).
		Scan(&outcome, &finished); err != nil {
		t.Fatalf("read the stale recovery attempt: %v", err)
	}
	if outcome == nil || *outcome != "aborted" || finished == nil {
		t.Fatalf("a recovery attempt open for two hours is outcome=%v finished=%v, want aborted "+
			"with a finish; it is blocking every further rung on this device", outcome, finished)
	}

	// An attempt started a moment ago belongs to a ladder that is very probably
	// still climbing. Taking a rung away from it is its own kind of harm.
	if err := f.pool.QueryRow(ctx,
		`SELECT outcome, finished_at FROM farm.recovery_attempts WHERE id = $1`, fresh).
		Scan(&outcome, &finished); err != nil {
		t.Fatalf("read the fresh recovery attempt: %v", err)
	}
	if outcome != nil || finished != nil {
		t.Fatalf("a recovery attempt started this second was closed: outcome=%v finished=%v",
			outcome, finished)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestNewRequiresAPool(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New succeeded without a Pool")
	}
}

// TestDefaultsMatchTheLadder pins the one number this package must not choose
// for itself: internal/recovery's budget check reads an open attempt as "this
// device is busy" for exactly recovery.DefaultStaleAttempt, and a janitor that
// swept sooner would unblock a rung the ladder still believes it owns.
func TestDefaultsMatchTheLadder(t *testing.T) {
	t.Parallel()
	c := Config{}
	c.applyDefaults()
	if c.RecoveryStale != recovery.DefaultStaleAttempt {
		t.Fatalf("RecoveryStale defaults to %s, want %s (internal/recovery.DefaultStaleAttempt); "+
			"a hardcoded copy here would drift away from the ladder's own budget check",
			c.RecoveryStale, recovery.DefaultStaleAttempt)
	}
	if c.Component != "janitor" {
		t.Fatalf("Component defaults to %q, want janitor", c.Component)
	}
}

// ---------------------------------------------------------------------------
// Bulk: the half of the repair that was missing
// ---------------------------------------------------------------------------

// newDevice adds another device on this fixture's host.
//
// farm.bulk_targets is keyed on (run_id, device_id), so a run with more than
// one target needs more than one device — and the shapes that matter here are
// exactly the ones with a mix of targets under a single run.
func (f *fixture) newDevice(t *testing.T, port int) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(), `
WITH s AS (
  INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number,
                          usb_path, topo_path, rack_slot)
  SELECT $1, h.id, pd.id, $2::int, '3-1.' || ($2::int)::text,
         ('x' || $3 || '.p' || ($2::int)::text)::ltree, 'R-' || $3 || '-P' || ($2::int)::text
    FROM farm.hubs h, farm.power_domains pd
   WHERE h.host_id = $1 AND pd.host_id = $1
  RETURNING id
)
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id,
                          manufacturer, model, sdk_int)
SELECT 'df-' || md5($3 || '/' || ($2::int)::text), 'SER-' || $3 || '-' || ($2::int)::text,
       $4, $1, s.id, 'Google', 'Pixel Test', 34
  FROM s
RETURNING id::text`, f.hostID, port, f.tag, f.poolID).Scan(&id)
	if err != nil {
		t.Fatalf("insert device on port %d of %s: %v", port, f.hostID, err)
	}
	return id
}

// newBulkRun writes a run row, backdated by createdAgo.
func (f *fixture) newBulkRun(t *testing.T, state string, timeout, createdAgo time.Duration) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(), `
INSERT INTO farm.bulk_runs (created_by, command, timeout, state, created_at, finished_at)
VALUES ('tester', 'getprop ro.build.id', $1::interval, $2,
        now() - $3::interval,
        CASE WHEN $2 = 'running' THEN NULL ELSE now() - $3::interval END)
RETURNING id::text`,
		pgInterval(timeout), state, pgInterval(createdAgo)).Scan(&id)
	if err != nil {
		t.Fatalf("insert bulk run: %v", err)
	}
	return id
}

// newBulkTarget writes one target. A 'pending' target has never been marked, so
// its started_at is NULL exactly as internal/api leaves it.
func (f *fixture) newBulkTarget(t *testing.T, runID, deviceID, state string, startedAgo time.Duration) {
	t.Helper()
	var started any
	if state == "running" {
		started = pgInterval(startedAgo)
	}
	if _, err := f.pool.Exec(t.Context(), `
INSERT INTO farm.bulk_targets (run_id, device_id, state, started_at)
VALUES ($1::uuid, $2::uuid, $3,
        CASE WHEN $4::interval IS NULL THEN NULL ELSE now() - $4::interval END)`,
		runID, deviceID, state, started); err != nil {
		t.Fatalf("insert %s bulk target: %v", state, err)
	}
}

func (f *fixture) bulkTargetState(t *testing.T, runID, deviceID string) (state string, errText *string, finished *time.Time) {
	t.Helper()
	if err := f.pool.QueryRow(t.Context(),
		`SELECT state, error, finished_at FROM farm.bulk_targets
		  WHERE run_id = $1::uuid AND device_id = $2::uuid`, runID, deviceID).
		Scan(&state, &errText, &finished); err != nil {
		t.Fatalf("read bulk target %s/%s: %v", runID, deviceID, err)
	}
	return state, errText, finished
}

func (f *fixture) bulkRunState(t *testing.T, runID string) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT state FROM farm.bulk_runs WHERE id = $1::uuid`, runID).Scan(&state); err != nil {
		t.Fatalf("read bulk run %s: %v", runID, err)
	}
	return state
}

// TestPendingTargetsOfADeadBulkRunAreClosed is the deadlock this sweep exists
// to break, and the shape a SIGKILLed API process actually leaves behind.
//
// A killed pod does not leave a tidy run with every target started. It leaves a
// few targets 'running' and the rest of the queue 'pending' behind the per-hub
// concurrency cap. Closing only the running ones is half a repair that cannot
// finish: bulkRunClose refuses to move a run while any target of it is
// outstanding, so the run — and the operator's page — reads 'running' forever
// over a queue that nothing will ever execute.
func TestPendingTargetsOfADeadBulkRunAreClosed(t *testing.T) {
	f := newFixture(t)
	queued := f.newDevice(t, 2)

	// Timeout 60s, created an hour ago: the interrupted target's own context
	// expired fifty-nine minutes ago and nothing has moved since.
	runID := f.newBulkRun(t, "running", 60*time.Second, time.Hour)
	f.newBulkTarget(t, runID, f.deviceID, "running", time.Hour)
	f.newBulkTarget(t, runID, queued, "pending", 0)

	f.sweep(t, f.janitor(t))

	// internal/api's own two words: a command that was interrupted is an error,
	// a command that never started was skipped. Reporting the queue as 'error'
	// would tell an operator that sixty devices failed when not one of them was
	// touched.
	state, errText, finished := f.bulkTargetState(t, runID, f.deviceID)
	if state != "error" || finished == nil {
		t.Fatalf("the interrupted target is state=%q finished=%v, want error with a finish",
			state, finished)
	}
	if errText == nil || *errText == "" {
		t.Fatal("the interrupted target carries no explanation")
	}

	state, errText, finished = f.bulkTargetState(t, runID, queued)
	if state != "skipped" || finished == nil {
		t.Fatalf("the queued target of a dead run is state=%q finished=%v, want skipped with a "+
			"finish; nothing will ever execute it and the run cannot close while it stands",
			state, finished)
	}
	if errText == nil || *errText == "" {
		t.Fatal("the skipped target carries no explanation")
	}

	if got := f.bulkRunState(t, runID); got != "cancelled" {
		t.Fatalf("the run is %q after every target of it was closed, want cancelled; a run stuck "+
			"at 'running' over an empty queue is the exact lie this loop exists to remove", got)
	}
}

// TestALiveBulkRunKeepsItsQueue is the other side, and it is the one that
// matters: a run working through its targets must not have its queue taken away
// because the queue is long.
//
// The subtest named "falsified" proves which clause does the protecting. It
// removes bulkMotionGuard and asserts the pending target then IS closed, so the
// assertion above cannot keep passing with the guard deleted.
func TestALiveBulkRunKeepsItsQueue(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	queued := f.newDevice(t, 2)

	// An old run — three times its own timeout in age, so nothing but the
	// motion guard is holding the queue — with a target that started five
	// seconds ago. The executor is demonstrably alive: it is holding a slot
	// right now.
	//
	// The timeout is an hour rather than a minute so the live target cannot age
	// past it while the rest of the suite runs and turn this into a test about
	// how long CI took.
	runID := f.newBulkRun(t, "running", time.Hour, 3*time.Hour)
	f.newBulkTarget(t, runID, f.deviceID, "running", 5*time.Second)
	f.newBulkTarget(t, runID, queued, "pending", 0)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.bulkTargetState(t, runID, f.deviceID); state != "running" {
		t.Fatalf("a target five seconds into a one-hour timeout is %q, want running", state)
	}
	if state, _, _ := f.bulkTargetState(t, runID, queued); state != "pending" {
		t.Fatalf("the queue of a live run is %q, want pending; a target waiting behind the "+
			"per-hub cap is not an orphan, and there is no honest clock on that wait", state)
	}
	if got := f.bulkRunState(t, runID); got != "running" {
		t.Fatalf("a live run is %q, want running", got)
	}

	t.Run("falsified", func(t *testing.T) {
		blind := strings.Replace(bulkTargetSweep, bulkMotionGuard, "", 1)
		if blind == bulkTargetSweep {
			t.Fatal("bulkMotionGuard no longer appears in bulkTargetSweep verbatim; this " +
				"falsification is running the unmodified statement and proves nothing")
		}

		// Rolled back: this is an experiment on the guard, not a sweep.
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the falsification transaction: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := tx.Exec(ctx, blind, pgInterval(time.Nanosecond), "falsified", 100); err != nil {
			t.Fatalf("run the falsified sweep: %v", err)
		}
		var state string
		if err := tx.QueryRow(ctx,
			`SELECT state FROM farm.bulk_targets WHERE run_id = $1::uuid AND device_id = $2::uuid`,
			runID, queued).Scan(&state); err != nil {
			t.Fatalf("read the falsified target: %v", err)
		}
		if state != "skipped" {
			t.Fatalf("with the motion guard removed the queued target is still %q, so the guard "+
				"is not what protects it and the assertion above would pass without it", state)
		}
	})
}

// TestBulkSweepIsBounded pins the batch limit onto the bulk statements.
//
// DefaultBatch exists because a control plane that has been down for a day
// comes back to thousands of orphans, and one unbounded UPDATE takes a row lock
// on every one of them in a single transaction — against the same table
// internal/api writes results to, out of the same pool the lease renewal path
// borrows from.
func TestBulkSweepIsBounded(t *testing.T) {
	f := newFixture(t)

	// Three dead runs, one target each, swept by a janitor allowed two rows.
	runs := make([]string, 3)
	devices := []string{f.deviceID, f.newDevice(t, 2), f.newDevice(t, 3)}
	for i := range runs {
		runs[i] = f.newBulkRun(t, "running", 60*time.Second, time.Hour)
		f.newBulkTarget(t, runs[i], devices[i], "running", time.Hour)
	}

	j := f.janitorWith(t, func(c *Config) { c.Batch = 2 })
	f.sweep(t, j)

	closed := 0
	for i, run := range runs {
		if state, _, _ := f.bulkTargetState(t, run, devices[i]); state != "running" {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("a cycle with Batch=2 closed %d of 3 orphaned bulk targets, want 2; the bulk "+
			"statements are not bounded by the batch every other pass respects", closed)
	}

	// And the next cycle finishes them, so bounding a repair delays it rather
	// than dropping it.
	f.sweep(t, j)
	for i, run := range runs {
		if state, _, _ := f.bulkTargetState(t, run, devices[i]); state == "running" {
			t.Fatalf("orphaned bulk target %d is still running after a second cycle; a batch "+
				"bound that never catches up is a leak, not a bound", i)
		}
	}
}

// TestBulkRunCloseIsBounded pins the same limit onto the run statement.
//
// A run with no targets at all is the shape a selector that matched nothing
// leaves behind when the pod dies before finishBulkRun: immediately closable,
// and nothing else has to happen first. Three of them make the bound visible on
// its own rather than through whatever the target pass did in the same cycle.
func TestBulkRunCloseIsBounded(t *testing.T) {
	f := newFixture(t)

	// Distinct ages, because the statement closes the oldest first and a tie
	// would make "which two" a question about physical row order.
	runs := []string{
		f.newBulkRun(t, "running", 60*time.Second, 3*time.Hour),
		f.newBulkRun(t, "running", 60*time.Second, 2*time.Hour),
		f.newBulkRun(t, "running", 60*time.Second, time.Hour),
	}

	j := f.janitorWith(t, func(c *Config) { c.Batch = 2 })
	f.sweep(t, j)

	if got := f.bulkRunState(t, runs[0]); got != "cancelled" {
		t.Fatalf("the oldest empty run is %q, want cancelled", got)
	}
	if got := f.bulkRunState(t, runs[1]); got != "cancelled" {
		t.Fatalf("the second-oldest empty run is %q, want cancelled", got)
	}
	if got := f.bulkRunState(t, runs[2]); got != "running" {
		t.Fatalf("a cycle with Batch=2 closed a third run (%q); the run statement is not "+
			"bounded by the batch", got)
	}

	f.sweep(t, j)
	if got := f.bulkRunState(t, runs[2]); got != "cancelled" {
		t.Fatalf("the third empty run is still %q after a second cycle; a batch bound that "+
			"never catches up is a leak, not a bound", got)
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

// warnCounter counts warn-and-above records without keeping them.
type warnCounter struct{ n atomic.Int64 }

func (h *warnCounter) Enabled(context.Context, slog.Level) bool { return true }

func (h *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.n.Add(1)
	}
	return nil
}
func (h *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCounter) WithGroup(string) slog.Handler      { return h }

// TestShutdownIsNotASweepFailure keeps a rolling deploy from looking like a
// database outage.
//
// Every statement in a cycle fails at once when the context is cancelled. Left
// unguarded that is a burst of warnings and six increments of the counter whose
// only job is to say something is wrong — on every deploy, several times a day.
// An alert that fires that often is an alert nobody reads on the day a runner
// really is dying mid-write.
func TestShutdownIsNotASweepFailure(t *testing.T) {
	f := newFixture(t)

	// A real orphan, so the "no warnings" half below cannot pass merely because
	// there was nothing to do.
	jobID := f.newJob(t, 1, 3)
	leaseID, fence := f.newLease(t, jobID, "expired", 30*time.Minute)
	f.newAttempt(t, jobID, 1, &leaseID, fence, time.Hour)
	f.newRunningStep(t, jobID, 1, 0, "install", time.Hour)

	h := &warnCounter{}
	j := f.janitorWith(t, func(c *Config) { c.Logger = slog.New(h) })

	f.sweep(t, j)
	if h.n.Load() == 0 {
		t.Fatal("a cycle that closed a genuine orphan logged nothing at warn level, so this " +
			"handler is not seeing the loop's output and the assertion below is vacuous")
	}

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	h.n.Store(0)

	if n := j.cycle(dead); n != 0 {
		t.Fatalf("a cycle on a cancelled context closed %d rows; nothing is swept on the way out", n)
	}
	// The cycle must also not stand down. Leadership is held on a session
	// connection, and pinging it on a dead context fails, so a cycle that goes
	// through the motions during a shutdown hands the lock to a standby it is
	// about to release anyway.
	if !j.lead.held {
		t.Fatal("a cycle on a cancelled context gave up leadership; the only thing that buys " +
			"is a lock handover to a replica that is one poll away from taking it properly")
	}

	// And each pass on its own. The cycle above stops at the leadership check
	// long before it reaches them, so asserting only through cycle() would
	// leave every sweep's error path untested — the exact shape of a test that
	// keeps passing after the thing it names is deleted.
	passes := []struct {
		name string
		fn   func(context.Context) int
	}{
		{"steps", j.sweepSteps},
		{"attempts", j.sweepAttempts},
		{"jobs", j.sweepJobs},
		{"bulk targets", j.sweepBulkTargets},
		{"bulk runs", j.sweepBulkRuns},
		{"recovery attempts", j.sweepRecoveryAttempts},
	}
	for _, p := range passes {
		if n := p.fn(dead); n != 0 {
			t.Fatalf("the %s sweep closed %d rows on a cancelled context", p.name, n)
		}
	}

	if got := h.n.Load(); got != 0 {
		t.Fatalf("shutting down produced %d warnings across %d passes; a cancelled context is "+
			"this process stopping, not a sweep that failed", got, len(passes))
	}
}

// ---------------------------------------------------------------------------
// The vocabulary this loop reads
// ---------------------------------------------------------------------------

// TestLiveLeaseStatesAreTheWholeVocabulary is a tripwire on the one assumption
// this package cannot check at runtime.
//
// liveLease hardcodes ('held','suspect') as LIVE and reads everything else in
// farm.leases as over. That is correct for the four states the schema has
// today. A fifth state added by a future migration would be read here as "this
// lease is finished" the moment it merged — silently, with no test failing —
// and a sweeper that treats a live state as dead is #663 again. So the schema's
// own CHECK constraint is read back and compared: adding a state makes THIS
// fail, in the package that has to decide what the new state means.
func TestLiveLeaseStatesAreTheWholeVocabulary(t *testing.T) {
	t.Parallel()
	p := requireDB(t)

	var def string
	err := p.QueryRow(t.Context(), `
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
 WHERE c.conrelid = 'farm.leases'::regclass
   AND c.contype = 'c'
   AND pg_get_constraintdef(c.oid) LIKE '%state = ANY%'`).Scan(&def)
	if err != nil {
		t.Fatalf("read the farm.leases state constraint: %v", err)
	}

	literal := regexp.MustCompile(`'([a-z_]+)'::text`)
	got := map[string]bool{}
	for _, m := range literal.FindAllStringSubmatch(def, -1) {
		got[m[1]] = true
	}
	want := map[string]bool{"held": true, "suspect": true, "expired": true, "released": true}

	if len(got) != len(want) {
		t.Fatalf("farm.leases.state allows %v; this package reads exactly %v and treats "+
			"everything outside 'held'/'suspect' as a lease that is over.\nconstraint: %s",
			keysOf(got), keysOf(want), def)
	}
	for s := range want {
		if !got[s] {
			t.Fatalf("farm.leases.state no longer allows %q, which liveLease names as live.\n"+
				"constraint: %s", s, def)
		}
	}

	// And the live half of that vocabulary is what liveLease actually says.
	for _, s := range []string{"held", "suspect"} {
		if !strings.Contains(liveLease, "'"+s+"'") {
			t.Fatalf("liveLease does not name %q as live: %s", s, liveLease)
		}
	}
	for _, s := range []string{"expired", "released"} {
		if strings.Contains(liveLease, "'"+s+"'") {
			t.Fatalf("liveLease names the terminal state %q as live: %s", s, liveLease)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestNewRejectsAPoolThatCannotSweep closes a silent wedge.
//
// Leadership parks one connection on its advisory lock for the life of the
// process. With a pool of one, that connection IS the pool: every sweep then
// waits out CallTimeout for a connection that cannot come back, and the only
// symptom is "could not scan" every thirty seconds forever while the janitor
// closes nothing at all.
func TestNewRejectsAPoolThatCannotSweep(t *testing.T) {
	t.Parallel()
	p := requireDB(t)

	cfg := p.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open a single-connection pool: %v", err)
	}
	defer one.Close()

	if _, err := New(Config{Pool: one}); err == nil {
		t.Fatal("New accepted a pool of one connection; the leadership session holds it " +
			"forever and every sweep would then time out waiting for it")
	}
}
