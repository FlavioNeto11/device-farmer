package reaper

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain.
//
// Two properties of that arrangement are load-bearing:
//
//   - Without DATABASE_URL every SQL-backed test SKIPS. `go test ./...` has to
//     be green on a laptop with no Postgres, or the suite stops being run at
//     all — and a suite nobody runs is worse than no suite. The pure-Go
//     invariants in invariants_test.go still execute everywhere.
//
//   - The database is created fresh per run and dropped afterwards. The reaper
//     sweeps GLOBALLY: farm.lease_reclaim has no tenant, pool or device filter,
//     because a reaper that could be pointed at a subset of the farm is a
//     reaper somebody will eventually point at the wrong subset. So a test's
//     leftovers are the next test's casualties, and resetSchema below runs
//     between tests for the same reason.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver. goose speaks database/sql, so
	// the migration step needs it; the tests themselves use pgxpool.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/migrations"
)

// testPool is the pool for the scratch database, or nil when DATABASE_URL was
// unset and every SQL-backed test must skip.
var testPool *pgxpool.Pool

// setupLockKey serialises scratch-database creation across packages.
//
// migrations/00002_lease.sql creates CLUSTER-WIDE roles (farm_reaper and
// friends) behind an "IF NOT EXISTS" check. `go test ./...` runs packages
// concurrently, so two suites migrating at the same instant can both see the
// role missing and both try to create it, and one of them gets
// duplicate_object. The lock is taken on the ADMIN database — the one every
// suite connects to in order to issue CREATE DATABASE — because advisory locks
// are scoped to a database and that is the only database the suites share.
const setupLockKey int64 = 0x64665f74657374 // "df_test"

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite owns the scratch database's whole lifetime. It returns an exit code
// rather than calling os.Exit itself so that its deferred teardown actually
// runs: os.Exit does not unwind the stack, and a leaked scratch database per
// test run is how a developer's cluster fills up.
func runSuite(m *testing.M) (code int) {
	base := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if base == "" {
		return m.Run()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A misconfigured DATABASE_URL is a failure, never a skip. Skipping here
	// would mean a CI job that quietly tests nothing while reporting success.
	admin, err := sql.Open("pgx", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	// One connection, because the advisory lock below is session-scoped and a
	// second pooled connection would not hold it.
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := scratchName("df_reaper_test")
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "reaper tests: create scratch database (the role needs CREATEDB): %v\n", err)
		return 1
	}
	defer func() {
		// Bounded, because pgxpool.Close waits for every checked-out connection
		// to come back and a leaked one would turn a failing suite into a
		// hanging one. Dropping the database with FORCE severs whatever is left.
		if testPool != nil {
			closed := make(chan struct{})
			go func() { testPool.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(15 * time.Second):
				fmt.Fprintln(os.Stderr,
					"reaper tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		// FORCE because a test that leaked a connection must not leak a
		// database too. The context may already be spent, so use a fresh one.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "reaper tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: release setup lock: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: %v\n", migrateErr)
		return 1
	}

	pool, err := openTestPool(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reaper tests: %v\n", err)
		return 1
	}
	testPool = pool

	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set, the same bytes the shipped
// binary carries. Pointing the tests at the .sql files on disk instead would
// let the tests pass against a schema no deployment ever gets.
func migrateScratch(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open scratch database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	goose.SetBaseFS(migrations.Goose())
	goose.SetLogger(quietGooseLogger{})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("migrate scratch database: %w", err)
	}
	return nil
}

func openTestPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse scratch DSN: %w", err)
	}
	// Leader election checks out a connection and KEEPS it, and several tests
	// run two reapers at once, so the pool must comfortably exceed the number
	// of leadership connections in flight or the tests deadlock on a property
	// they are supposed to be measuring.
	pc.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("connect to scratch database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping scratch database: %w", err)
	}
	return pool, nil
}

type quietGooseLogger struct{}

func (quietGooseLogger) Printf(string, ...any) {}
func (quietGooseLogger) Fatalf(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "goose: "+format+"\n", v...)
}

func scratchName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano()%1_000_000)
}

// dsnForDatabase rewrites the database name in a libpq URL.
func dsnForDatabase(base, name string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL is not a URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("DATABASE_URL must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ---------------------------------------------------------------------------
// Per-test scaffolding
// ---------------------------------------------------------------------------

// requireDB returns the scratch pool or skips. Every SQL-backed test starts
// with this line.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed reaper tests")
	}
	return testPool
}

// resetSchema empties every table the tests write to and puts farm.reaper_state
// back to its migration-time defaults.
//
// The two reference tables seeded by the migrations (recovery_tiers,
// step_kinds) are left alone: they are schema, not test data, and truncating
// them would break anything that resolves a recovery tier.
func resetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const q = `
DO $$
DECLARE tables text;
BEGIN
  SELECT string_agg(format('%I.%I', schemaname, tablename), ', ')
    INTO tables
    FROM pg_tables
   WHERE schemaname = 'farm'
     AND tablename NOT IN ('reaper_state', 'recovery_tiers', 'step_kinds');
  IF tables IS NOT NULL THEN
    EXECUTE 'TRUNCATE ' || tables || ' RESTART IDENTITY CASCADE';
  END IF;
END $$;`
	if _, err := pool.Exec(ctx, q); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	// quiesce_until = now() leaves the gate OPEN by default, so a test that
	// wants it closed has to say so. The reverse default would let a test pass
	// because nothing ever swept. The same goes for a standing refusal.
	if _, err := pool.Exec(ctx,
		`UPDATE farm.reaper_state SET quiesce_until = now(), armed_at = now(), enabled = true,
		        last_refusal = NULL, last_refusal_at = NULL`); err != nil {
		t.Fatalf("reset reaper_state: %v", err)
	}
}

// logRecorder captures slog records so a test can assert on the SEVERITY of
// what a loop said, not only on what it did. "This outcome must not be logged
// loudly" is a real requirement here: an operator who learns to ignore these
// logs stops seeing the ones that matter.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

func (h *logRecorder) logger() *slog.Logger { return slog.New(h) }

// atOrAbove returns the messages logged at or above level.
func (h *logRecorder) atOrAbove(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level >= level {
			out = append(out, r.Level.String()+": "+r.Message)
		}
	}
	return out
}

func (h *logRecorder) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = nil
}

// ---------------------------------------------------------------------------
// Fixtures
//
// Rows are inserted directly rather than through the enrolment functions: a
// test that has to drive four subsystems to reach the state it wants tests
// those four subsystems, and reports their failures as reaper failures.
// ---------------------------------------------------------------------------

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context

	poolID   string
	hostID   string
	hubID    int64
	tenantID string
	queueID  string

	seq int // unique suffixes within one test
}

func newFixture(t *testing.T, pool *pgxpool.Pool) *fixture {
	t.Helper()
	resetSchema(t, pool)

	f := &fixture{
		t: t, pool: pool, ctx: context.Background(),
		poolID: "p", hostID: "h1", tenantID: "t1", queueID: "q1",
	}
	f.exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	f.exec(`INSERT INTO farm.tenants (id, name) VALUES ($1, $1)`, f.tenantID)
	f.exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, f.queueID, f.tenantID)
	f.hostID, f.hubID = f.newHost("h1", "enabled")

	// Every default watched component has beaten once. Since migration 00012
	// a reaper refuses to arm otherwise, and a test about the sweep, the gate
	// or leadership must not fail on that refusal — the test about the
	// refusal itself watches a name nothing here beats.
	for _, c := range lease.ReaperComponents {
		f.beat(c, 0)
	}
	return f
}

// newHost creates a host with one hub. adminState is the operator's drain
// switch: 'enabled', 'draining' or 'disabled'.
func (f *fixture) newHost(id, adminState string) (string, int64) {
	f.t.Helper()
	f.exec(`INSERT INTO farm.hosts (id, adb_endpoint, admin_state) VALUES ($1, $2, $3)`,
		id, "127.0.0.1:5037", adminState)
	var hubID int64
	f.scan(&hubID,
		`INSERT INTO farm.hubs (host_id, usb_path, port_count) VALUES ($1, '1-1', 16) RETURNING id`, id)
	return id, hubID
}

type deviceOpts struct {
	hostID string
	hubID  int64

	adminState string // farm.devices.admin_state, default 'enabled'
	adbState   string // farm.device_runtime.adb_state, default 'device'
	health     string // farm.device_runtime.health, default 'healthy'
	slotState  string // farm.slots.state, default 'active'

	// rearmIn offsets farm.slots.rearm_at from the server's now(). Positive
	// means the slot is still in its post-reclaim fence quarantine.
	rearmIn time.Duration

	poolID string // default the fixture pool
}

// device creates a slot and a device sitting in it, plus the health row the
// allocator reads. Defaults describe a device that is schedulable right now, so
// a test states only the one fact it is about.
func (f *fixture) device(opts deviceOpts) string {
	f.t.Helper()
	f.seq++
	n := f.seq

	host := opts.hostID
	hub := opts.hubID
	if host == "" {
		host, hub = f.hostID, f.hubID
	}
	slotState := or(opts.slotState, "active")
	adminState := or(opts.adminState, "enabled")
	adbState := or(opts.adbState, "device")
	health := or(opts.health, "healthy")
	poolID := or(opts.poolID, f.poolID)

	var slotID int64
	f.scan(&slotID, `
INSERT INTO farm.slots (host_id, hub_id, port_number, usb_path, topo_path, state, rearm_at)
VALUES ($1, $2, $3, $4, $5::ltree, $6, now() + $7::interval)
RETURNING id`,
		host, hub, n, fmt.Sprintf("1-1.%d", n),
		fmt.Sprintf("%s.p1_%d", sanitizeLabel(host), n), slotState, interval(opts.rearmIn))

	var deviceID string
	f.scan(&deviceID, `
INSERT INTO farm.devices (farm_uid, pool_id, host_id, current_slot_id, admin_state, model)
VALUES ($1, $2, $3, $4, $5, 'Test Device')
RETURNING id::text`,
		fmt.Sprintf("df-%032x", n), poolID, host, slotID, adminState)

	f.exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, last_seen_at)
VALUES ($1::uuid, $2, $3, $4, $5, now())`,
		deviceID, host, slotID, adbState, health)

	return deviceID
}

type jobOpts struct {
	state     string // default 'queued'
	protected bool
	poolID    string
	queueID   string
	tenantID  string

	// maxRuntime is the ONLY user-supplied clock allowed to end a lease
	// automatically. Nil leaves it NULL.
	maxRuntime *time.Duration

	// expectedDuration over 30 minutes makes the lease protected inside
	// farm.lease_acquire; long jobs are held and paged, never reclaimed.
	expectedDuration *time.Duration

	pinDevice string // farm.jobs.pin_device
	createdAt time.Duration
}

func (f *fixture) job(opts jobOpts) string {
	f.t.Helper()
	var id string
	f.scan(&id, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, protected,
                       max_runtime, expected_duration, pin_device, created_at)
VALUES ($1, $2, $3, $4, $5, $6::interval, $7::interval,
        NULLIF($8, '')::uuid, now() + $9::interval)
RETURNING id::text`,
		or(opts.tenantID, f.tenantID), or(opts.queueID, f.queueID), or(opts.poolID, f.poolID),
		or(opts.state, "queued"), opts.protected,
		nullableInterval(opts.maxRuntime), nullableInterval(opts.expectedDuration),
		opts.pinDevice, interval(opts.createdAt))
	return id
}

// leaseOpts positions a lease in time. Every offset is relative to the SERVER's
// now(): no test sends an instant it computed locally, for the same reason no
// production path does.
type leaseOpts struct {
	state     string // 'held' or 'suspect'
	protected bool

	acquiredIn     time.Duration // negative = in the past
	heartbeatIn    time.Duration
	expiresIn      time.Duration
	reclaimableIn  time.Duration
	witnessIn      *time.Duration
	witnessExtends int
}

type seededLease struct {
	id     string
	fence  int64
	device string
	job    string

	// holderInstance is what farm.lease_renew matches on, so a test that wants
	// to prove a heartbeat still heals a suspect lease needs it.
	holderInstance string
}

// seedLease writes a lease row directly, which is the only way to place one in
// the past.
//
// farm.trg_leases_guard forbids moving expires_at or reclaimable_at BACKWARDS,
// so a lease created through farm.lease_acquire (deadlines at least ten minutes
// out, per the CHECK on jobs.ttl) can never be backdated by an UPDATE. That
// guard is correct and is itself an invariant worth having; the tests work with
// it by writing the row they want in the first place. The AFTER INSERT trigger
// still runs, so devices.current_lease_id and fence_floor are maintained
// exactly as in production.
func (f *fixture) seedLease(deviceID, jobID string, opts leaseOpts) seededLease {
	f.t.Helper()
	holderInstance, err := lease.NewHolderInstance()
	if err != nil {
		f.t.Fatalf("mint holder instance: %v", err)
	}

	var out seededLease
	row := f.pool.QueryRow(f.ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, state, protected,
                         ttl, grace, acquired_at, heartbeat_at,
                         expires_at, reclaimable_at, witness_at, witness_extensions)
SELECT d.id, d.current_slot_id, j.id, j.tenant_id, j.queue_id,
       'test-holder', $3::uuid, $4::text, $5::boolean,
       j.ttl, j.grace,
       now() + $6::interval, now() + $7::interval,
       now() + $8::interval, now() + $9::interval,
       CASE WHEN $10::interval IS NULL THEN NULL ELSE now() + $10::interval END,
       $11::int
  FROM farm.devices d, farm.jobs j
 WHERE d.id = $1::uuid AND j.id = $2::uuid
RETURNING id::text, fence`,
		deviceID, jobID, holderInstance, or(opts.state, "held"), opts.protected,
		interval(opts.acquiredIn), interval(opts.heartbeatIn),
		interval(opts.expiresIn), interval(opts.reclaimableIn),
		nullableInterval(opts.witnessIn), opts.witnessExtends)
	if err := row.Scan(&out.id, &out.fence); err != nil {
		f.t.Fatalf("seed lease: %v", err)
	}
	out.device, out.job, out.holderInstance = deviceID, jobID, holderInstance
	return out
}

// beat writes a component heartbeat AGED by the given offset. A stale beat is
// how a test says "the control plane was down for this long".
func (f *fixture) beat(component string, ago time.Duration) {
	f.t.Helper()
	f.exec(`
INSERT INTO farm.component_heartbeat (component, beat_at)
VALUES ($1, now() - $2::interval)
ON CONFLICT (component) DO UPDATE SET beat_at = EXCLUDED.beat_at`,
		component, interval(ago))
}

// openReclaimGate puts farm.reaper_state where a reclaim is permitted and
// removes any recorded outage.
//
// It exists because arming legitimately CLOSES the gate: a test that wants to
// observe a reclaim must first undo the protection the arm just installed, and
// doing that explicitly keeps "the reaper did not reclaim" from silently
// meaning "the reaper could never have reclaimed".
func (f *fixture) openReclaimGate() {
	f.t.Helper()
	f.exec(`UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 second', enabled = true,
	               last_refusal = NULL, last_refusal_at = NULL`)
	f.exec(`DELETE FROM farm.control_plane_gap`)
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func (f *fixture) scan(dest any, sql string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(dest); err != nil {
		f.t.Fatalf("query %.60q: %v", sql, err)
	}
}

// leaseState reads the current state and release reason of one lease.
func (f *fixture) leaseState(leaseID string) (state string, reason *string) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx,
		`SELECT state, release_reason FROM farm.leases WHERE id = $1::uuid`, leaseID).
		Scan(&state, &reason); err != nil {
		f.t.Fatalf("read lease %s: %v", leaseID, err)
	}
	return state, reason
}

// newReaper builds a Reaper wired to the scratch pool with a lock key derived
// from the test name, so two tests can never fight over leadership.
//
// The cleanup is not a nicety. A leader keeps a pool connection checked out for
// as long as it leads, and pgxpool.Close blocks until every connection is back;
// a test that fails before its own defer runs would otherwise hang the whole
// suite in TestMain instead of reporting the failure it just found.
func (f *fixture) newReaper(rec *logRecorder) *Reaper {
	f.t.Helper()
	return f.newReaperWatching(rec, nil)
}

// newReaperWatching is newReaper with an explicit farm.reaper_arm component
// list; nil takes the package default.
func (f *fixture) newReaperWatching(rec *logRecorder, components []string) *Reaper {
	f.t.Helper()
	r, err := New(Config{
		Pool:       f.pool,
		Store:      lease.NewStore(f.pool),
		Components: components,
		LockKey:    lockKeyFor(f.t.Name()),
		Logger:     rec.logger(),
	})
	if err != nil {
		f.t.Fatalf("new reaper: %v", err)
	}
	f.t.Cleanup(func() { r.lead.release(context.Background()) })
	return r
}

// lockKeyFor derives a stable advisory-lock key from a test name (FNV-1a).
func lockKeyFor(name string) int64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= prime64
	}
	// Clear the sign bit: the key is only ever compared for equality, and a
	// negative key is harder to recognise in pg_locks.
	return int64(h & 0x7fffffffffffffff)
}

// advisoryLockHolders counts the sessions holding the given 64-bit advisory
// lock. It reads pg_locks from a DIFFERENT session, which is the only way to
// check that leadership is a fact in the database rather than a boolean in Go.
func (f *fixture) advisoryLockHolders(key int64) int {
	f.t.Helper()
	var n int
	f.scan(&n, `
SELECT count(*) FROM pg_locks
 WHERE locktype = 'advisory'
   AND ((classid::bigint << 32) | (objid::bigint & 4294967295)) = $1
   AND granted`, key)
	return n
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// interval renders a duration as a Postgres interval literal, sign preserved.
// It is a DURATION and never an instant: nothing here tells Postgres what time
// this machine thinks it is.
func interval(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Microsecond), 10) + " microseconds"
}

func nullableInterval(d *time.Duration) *string {
	if d == nil {
		return nil
	}
	s := interval(*d)
	return &s
}

func dur(d time.Duration) *time.Duration { return &d }

// sanitizeLabel makes an ltree label out of an identifier: ltree accepts only
// letters, digits and underscores between dots.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
