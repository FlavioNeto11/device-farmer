package lease

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	// Registers the "pgx" database/sql driver. goose speaks database/sql, and
	// the scratch database has to be migrated before a pool is pointed at it.
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/migrations"
)

// =====================================================================
// Harness
//
// These tests run against a REAL PostgreSQL, because the thing under test is
// the boundary between Go and SQL: which outcome is zero rows, which outcome
// is an error, and which of the two is allowed to end a lease. A stubbed
// database can only assert what the stub was told to return, so it would
// prove the one property that matters — that ErrFenced is reachable ONLY from
// zero rows — by assumption.
//
// The database is a scratch one, created and migrated per run and dropped
// afterwards. It is never the demo database: these tests acquire, fence and
// release leases, and a test that can reach a live farm's leases is a test
// that can destroy somebody's six-hour job.
// =====================================================================

// testPool is the scratch database, or nil when DATABASE_URL was unset and the
// SQL-backed tests must skip.
var testPool *pgxpool.Pool

// fixtureSeq namespaces each fixture's rows. Tests share one scratch database
// and must not see each other's devices: a stray free handset in another
// test's pool would turn "no capacity" into a passing acquire.
var fixtureSeq atomic.Int64

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		// Say what to DO about it. The harness fails long before any test runs,
		// so this line is the only thing the reader gets.
		fmt.Fprintf(os.Stderr,
			"lease tests: %v\n"+
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
		// pure-Go ones still run, so "go test ./..." passes on a laptop with
		// no Postgres.
		return m.Run(), nil
	}

	// Set but broken is a different situation from unset, and it fails loudly.
	// Somebody who exported DATABASE_URL asked for these tests to run.
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
// The migrations come from the migrations package rather than from a path so
// the schema under test is the schema the binaries carry. A -dir lookup would
// pass here while a production image that forgot to COPY migrations/ failed in
// the cluster, which is the exact inversion a test is supposed to prevent.
func setupScratchDB(ctx context.Context, dsn string) (*pgxpool.Pool, func(), error) {
	adminCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", config.EnvDatabaseURL, err)
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, nil, fmt.Errorf("name scratch database: %w", err)
	}
	name := fmt.Sprintf("df_lease_test_%d_%s", os.Getpid(), hex.EncodeToString(suffix[:]))

	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", adminCfg.Database, err)
	}
	defer admin.Close(ctx)

	// The migration set creates farm_reaper, farm_scheduler and farm_watchdog,
	// and roles are cluster-wide rather than per-database: two test binaries
	// migrating their own scratch databases at once would both find the role
	// missing and both try to create it. A session advisory lock, held across
	// the create and the migrate, serialises them the same way cmd/farmd
	// serialises concurrent migrators.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", testMigrationLockKey); err != nil {
		return nil, nil, fmt.Errorf("take the test migration lock: %w", err)
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer ucancel()
		// Best effort: the lock is released with the session in any case.
		_, _ = admin.Exec(uctx, "SELECT pg_advisory_unlock($1)", testMigrationLockKey)
	}()

	// CREATE DATABASE cannot run inside a transaction block, so it goes out in
	// autocommit.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, nil, fmt.Errorf("create scratch database %s: %w", name, err)
	}

	drop := func() {
		// A fresh connection: the pool is already closed by the time this runs,
		// and DROP DATABASE cannot be issued from a session inside it.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		conn, err := pgx.ConnectConfig(dctx, adminCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lease tests: scratch database %s left behind: %v\n", name, err)
			return
		}
		defer conn.Close(dctx)
		// FORCE terminates anything still attached; a leaked connection must
		// not turn into a database that accumulates on the developer's server.
		if _, err := conn.Exec(dctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			fmt.Fprintf(os.Stderr, "lease tests: scratch database %s left behind: %v\n", name, err)
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
const testMigrationLockKey int64 = 0x6466_7465_7374_0001 // "df" + "test" + version

// gooseMu serialises use of goose's package-level state (base FS, dialect,
// logger), which is global to this process and would otherwise be reconfigured
// underneath a concurrent migration.
var gooseMu sync.Mutex

func migrateScratchDB(ctx context.Context, adminCfg *pgx.ConnConfig, name string) error {
	cfg := adminCfg.Copy()
	cfg.Database = name

	// Hands goose a database/sql DSN without rebuilding the connection string
	// by hand: the parsed config already carries the host, credentials and TLS
	// settings the operator configured.
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

// requireDB returns the scratch pool, or skips the test when there is none.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skipf("%s is not set; skipping the PostgreSQL-backed lease tests", config.EnvDatabaseURL)
	}
	return testPool
}

// fixture is one test's private slice of the farm: its own rack, host, hub,
// pool, tenant, queue, slots and devices. Nothing it creates is visible to the
// allocator running for another fixture, because every job is scheduled inside
// its own device pool.
type fixture struct {
	store    *Store
	pool     *pgxpool.Pool
	poolID   string
	tenantID string
	queueID  string
	hostID   string
	devices  int
}

// newFixture seeds n schedulable devices. n may be zero, which is how a job is
// given a pool with no capacity at all.
func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	p := requireDB(t)
	ctx := t.Context()

	seq := fixtureSeq.Add(1)
	tag := fmt.Sprintf("%04d", seq)
	f := &fixture{
		store:    NewStore(p),
		pool:     p,
		poolID:   "pool-" + tag,
		tenantID: "tenant-" + tag,
		queueID:  "queue-" + tag,
		hostID:   "host-" + tag,
		devices:  n,
	}

	// The statement goes into the failure, because a fixture that stops seeding
	// halfway leaves every test in this file failing for a reason that has
	// nothing to do with leases — most often a migration this scratch database
	// did not get, or a column the schema renamed underneath these inserts.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding fixture %s failed: %v\nstatement: %s\nargs: %v", f.hostID, err, q, args)
		}
	}

	exec(`INSERT INTO farm.racks (id) VALUES ($1)`, "rack-"+tag)
	exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ($1, $2, $3)`,
		f.hostID, "rack-"+tag, "127.0.0.1:5037")
	exec(`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3)`, f.hostID)
	exec(`INSERT INTO farm.power_domains (host_id, kind, control) VALUES ($1, 'per_port', 'uhubctl')`, f.hostID)
	exec(`INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
	      SELECT $1, c.id, '3-1', 8, true FROM farm.controllers c WHERE c.host_id = $1`, f.hostID)
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, f.tenantID)
	exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, f.queueID, f.tenantID)

	// Slots carry the USB position. Every device below is reached through one,
	// never through its serial.
	exec(`INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number,
	                              usb_path, topo_path, rack_slot)
	      SELECT $1, h.id, pd.id, g, '3-1.' || g,
	             ('x' || $2 || '.p' || g)::ltree, 'R-' || $2 || '-P' || g
	        FROM farm.hubs h, farm.power_domains pd, generate_series(1, $3::int) g
	       WHERE h.host_id = $1 AND pd.host_id = $1`, f.hostID, tag, n)

	// Two devices deliberately share an ADB serial, because duplicate OEM
	// serials are real. Identity here is the farm_uid and the slot.
	exec(`INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id,
	                                current_slot_id, manufacturer, model, sdk_int)
	      SELECT 'df-' || md5($1 || s.usb_path),
	             CASE WHEN s.port_number <= 2 THEN '0123456789ABCDEF'
	                  ELSE 'SER-' || s.port_number END,
	             $2, $1, s.id, 'Google', 'Pixel Test', 34
	        FROM farm.slots s WHERE s.host_id = $1`, f.hostID, f.poolID)

	exec(`INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
	      SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy'
	        FROM farm.devices d WHERE d.host_id = $1`, f.hostID)

	return f
}

// jobOpts are the two job columns that change what may later be done to the
// lease, and both are load-bearing for the invariant. protected withholds a
// lease from the reaper permanently — long jobs are held and a human paged
// instead — and max_runtime is the ONE user-supplied clock allowed to end a
// lease automatically.
type jobOpts struct {
	protected  bool
	maxRuntime time.Duration // zero leaves the column NULL: no automatic ending
}

// newJob inserts a queued job in this fixture's pool and returns its id.
func (f *fixture) newJob(t *testing.T) string {
	t.Helper()
	return f.newJobWith(t, jobOpts{})
}

func (f *fixture) newJobWith(t *testing.T, o jobOpts) string {
	t.Helper()
	// A duration, never an instant: the row records how long the job may run,
	// and Postgres decides when that has elapsed against its own now().
	var maxRuntime *string
	if o.maxRuntime > 0 {
		s := intervalArg(o.maxRuntime)
		maxRuntime = &s
	}
	var id string
	err := f.pool.QueryRow(t.Context(),
		`INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, protected, max_runtime)
		 VALUES ($1, $2, $3, $4, $5::interval) RETURNING id::text`,
		f.tenantID, f.queueID, f.poolID, o.protected, maxRuntime).Scan(&id)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return id
}

// acquire takes a lease for a fresh job and fails the test if none was
// available, since every caller of this helper needs a live lease to test with.
func (f *fixture) acquire(t *testing.T) (jobID string, l Lease) {
	t.Helper()
	return f.acquireWith(t, jobOpts{})
}

func (f *fixture) acquireWith(t *testing.T, o jobOpts) (jobID string, l Lease) {
	t.Helper()
	jobID = f.newJobWith(t, o)
	res, err := f.store.Acquire(t.Context(), jobID, "pod-"+jobID[:8], mustInstance(t))
	if err != nil {
		t.Fatalf("acquire %s: %v; this fixture seeded %d device(s) and earlier subtests may have "+
			"consumed or quarantined them", jobID, err, f.devices)
	}
	if res.Reattached {
		t.Fatalf("acquire %s: reattached to a lease that should not exist yet", jobID)
	}
	return jobID, res.Lease
}

// leaseState reads a lease's state and release reason straight from the table,
// so an assertion about what happened is never made from the return value of
// the call that claimed to make it happen.
func (f *fixture) leaseState(t *testing.T, leaseID string) (state string, reason *string) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(),
		`SELECT state, release_reason FROM farm.leases WHERE id = $1::uuid`, leaseID).
		Scan(&state, &reason)
	if err != nil {
		t.Fatalf("read lease %s: %v", leaseID, err)
	}
	return state, reason
}

// deviceBinding reads the two columns on farm.devices that decide whether a
// previous holder's sockets are still accepted: which lease the device is bound
// to, and the fence floor the host proxy compares against.
func (f *fixture) deviceBinding(t *testing.T, deviceID string) (currentLease *string, fenceFloor int64) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(),
		`SELECT current_lease_id::text, fence_floor FROM farm.devices WHERE id = $1::uuid`, deviceID).
		Scan(&currentLease, &fenceFloor)
	if err != nil {
		t.Fatalf("read device %s: %v", deviceID, err)
	}
	return currentLease, fenceFloor
}

// abandon puts a lease in the only state from which an automatic release is
// possible at all: silent for longer than TTL + grace, and swept to suspect.
//
// It returns the sweep's rows so a caller can assert on the alert as well as on
// the state, because suspect is an alert and the Protected flag on it is the
// difference between "the reaper will take this" and "page a human".
func (f *fixture) abandon(t *testing.T, leaseID string) []SuspectLease {
	t.Helper()
	// Past ttl (15m) + grace (30m) with room to spare, so the lease is due
	// rather than borderline against a clock that ticks during the test.
	f.backdateLease(t, leaseID, 50*time.Minute)

	suspects, err := f.store.MarkSuspect(t.Context(), DefaultSuspectBatch)
	if err != nil {
		t.Fatalf("mark suspect: %v", err)
	}
	if state, _ := f.leaseState(t, leaseID); state != "suspect" {
		t.Fatalf("lease %s is %q after the sweep, want suspect; every reclaim assertion made "+
			"against it would pass without testing anything", leaseID, state)
	}
	return suspects
}

// backdateLease moves a lease's clocks into the past to stand in for elapsed
// time.
//
// The guard trigger refuses to let a deadline move backwards — that refusal is
// what keeps a routine heartbeat from erasing a control-plane-gap refund — so
// the trigger is switched off for the length of one transaction. ALTER TABLE
// takes an ACCESS EXCLUSIVE lock that is held until commit, so no concurrent
// session can observe the table without its guard.
func (f *fixture) backdateLease(t *testing.T, leaseID string, by time.Duration) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed below; rollback is the failure path

	if _, err := tx.Exec(ctx, `ALTER TABLE farm.leases DISABLE TRIGGER leases_guard`); err != nil {
		t.Fatalf("disable the leases_guard trigger: %v\n"+
			"ALTER TABLE ... DISABLE TRIGGER needs ownership of farm.leases; run these tests as the "+
			"role that owns the schema, or check that migration 00002 still names the trigger "+
			"leases_guard", err)
	}
	// heartbeat_at moves too: a backdated deadline with a fresh heartbeat is a
	// state the server never produces.
	if _, err := tx.Exec(ctx,
		`UPDATE farm.leases
		    SET acquired_at    = acquired_at    - $2::interval,
		        heartbeat_at   = heartbeat_at   - $2::interval,
		        expires_at     = expires_at     - $2::interval,
		        reclaimable_at = reclaimable_at - $2::interval
		  WHERE id = $1::uuid`, leaseID, intervalArg(by)); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE farm.leases ENABLE TRIGGER leases_guard`); err != nil {
		t.Fatalf("enable guard: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}

// =====================================================================
// Acquire
// =====================================================================

// TestAcquireReattachesAtTheSameFence covers the case a Kubernetes cluster
// produces every day: the holder pod is evicted and its replacement asks for
// the same job's device. It must get the same lease, the same device and the
// SAME fence back, because the evicted pod's work may still be running detached
// on the handset and a bumped fence would fence the job out of its own device.
func TestAcquireReattachesAtTheSameFence(t *testing.T) {
	f := newFixture(t, 2)
	ctx := t.Context()

	jobID := f.newJob(t)
	first, err := f.store.Acquire(ctx, jobID, "pod-a", mustInstance(t))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Reattached {
		t.Fatal("first acquire reported a re-attach; there was nothing to re-attach to")
	}

	// A different pod, a different process incarnation, the same job.
	second, err := f.store.Acquire(ctx, jobID, "pod-b", mustInstance(t))
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if !second.Reattached {
		t.Error("second acquire did not report a re-attach; the replacement pod was handed a new allocation")
	}
	if second.Lease.ID != first.Lease.ID {
		t.Errorf("re-attach changed the lease: %s -> %s", first.Lease.ID, second.Lease.ID)
	}
	if second.Lease.DeviceID != first.Lease.DeviceID {
		t.Errorf("re-attach moved the job to another device: %s -> %s", first.Lease.DeviceID, second.Lease.DeviceID)
	}
	if second.Lease.Fence != first.Lease.Fence {
		t.Errorf("re-attach bumped the fence %d -> %d; that fences the job out of its own detached work",
			first.Lease.Fence, second.Lease.Fence)
	}

	// The re-attached holder owns the lease, so renewing at the shared fence
	// with the NEW instance works and with the old one does not.
	if _, err := f.store.Renew(ctx, second.Lease.ID, second.Lease.Fence, second.Lease.HolderInstance); err != nil {
		t.Errorf("renew after re-attach: %v", err)
	}
	_, err = f.store.Renew(ctx, first.Lease.ID, first.Lease.Fence, first.Lease.HolderInstance)
	if !errors.Is(err, ErrFenced) {
		t.Errorf("the evicted pod's instance renewed the re-attached lease: err = %v, want ErrFenced", err)
	}
}

// holderPrincipal reads the principal a lease is bound to, straight from the
// table, so an assertion about the binding is never made from the return value
// of the call that claimed to make it.
func (f *fixture) holderPrincipal(t *testing.T, leaseID string) *string {
	t.Helper()
	var p *string
	err := f.pool.QueryRow(t.Context(),
		`SELECT holder_principal FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&p)
	if err != nil {
		t.Fatalf("read holder_principal of %s: %v", leaseID, err)
	}
	return p
}

// TestAcquireAsAdmitsTheEvictionAndRefusesTheTakeover is the pair of properties
// migration 00009 has to hold at once.
//
// Re-attach is idempotent on job_id, and a job id is not a secret: the lease
// list, the fleet grid and the event stream all publish it. So before 00009 any
// caller holding a job id became that lease's holder and received the fence that
// authorises writes to the handset, while the rightful holder's next Renew
// matched nothing and reported ErrFenced — terminal — so it aborted a multi-hour
// run believing the system had done it correctly.
//
// The two callers are indistinguishable from what they present: both bring a new
// pod name and a freshly minted holder_instance. What separates them is the
// credential, which belongs to the WORKLOAD and not to the process that died —
// the replacement pod authenticates as the same principal, the thief does not.
func TestAcquireAsAdmitsTheEvictionAndRefusesTheTakeover(t *testing.T) {
	f := newFixture(t, 2)
	ctx := t.Context()
	owner := Caller{Tenant: f.tenantID, Principal: "ci-bot@" + f.tenantID}

	jobID := f.newJob(t)
	first, err := f.store.AcquireAs(ctx, jobID, "pod-a", mustInstance(t), owner)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if got := f.holderPrincipal(t, first.Lease.ID); got == nil || *got != owner.Principal {
		t.Fatalf("allocation did not bind the lease to its principal: %v", got)
	}

	// The eviction. New pod, new instance, same workload — and it must land on
	// the same lease at the SAME fence, because the job's own work may still be
	// running detached on the phone.
	second, err := f.store.AcquireAs(ctx, jobID, "pod-b", mustInstance(t), owner)
	if err != nil {
		t.Fatalf("the eviction re-attach was refused: %v", err)
	}
	if !second.Reattached || second.Lease.ID != first.Lease.ID {
		t.Fatalf("the replacement pod was handed a new allocation (reattached=%v, %s -> %s)",
			second.Reattached, first.Lease.ID, second.Lease.ID)
	}
	if second.Lease.Fence != first.Lease.Fence {
		t.Fatalf("re-attach bumped the fence %d -> %d; that fences the job out of its own detached work",
			first.Lease.Fence, second.Lease.Fence)
	}

	// The takeover. Same job id, same tenant, a different credential.
	_, err = f.store.AcquireAs(ctx, jobID, "thief-pod", mustInstance(t),
		Caller{Tenant: f.tenantID, Principal: "someone-else@" + f.tenantID})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a stranger presenting only the job id: err = %v, want ErrNotPermitted", err)
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Error("a permission failure was reported as a busy farm; the caller would retry forever and never say why")
	}
	if errors.Is(err, ErrFenced) {
		t.Error("a refused acquire was reported as fencing")
	}

	// THE INVARIANT. A lease ends when the job says so, when a user-written
	// deadline elapses, or when a human takes it back. A failed authorisation
	// check is none of the three, so the holder that was NOT displaced must
	// still be able to renew — at the same fence, with the instance it
	// re-attached with.
	state, reason := f.leaseState(t, first.Lease.ID)
	if state != "held" || reason != nil {
		t.Fatalf("the refusal ended the lease: state=%s reason=%v", state, reason)
	}
	if _, err := f.store.Renew(ctx, second.Lease.ID, second.Lease.Fence, second.Lease.HolderInstance); err != nil {
		t.Fatalf("the rightful holder was fenced by somebody else's refused acquire: %v", err)
	}
	if got := f.holderPrincipal(t, first.Lease.ID); got == nil || *got != owner.Principal {
		t.Errorf("the refusal rebound the lease: %v", got)
	}
}

// TestAcquireAsRefusesAnotherTenantsJob covers the gate that runs ahead of both
// phases: tenantScope() from the API, moved inside the transaction so it also
// covers callers that never pass through the HTTP handler.
func TestAcquireAsRefusesAnotherTenantsJob(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()

	jobID := f.newJob(t)
	stranger := Caller{Tenant: "some-other-tenant", Principal: "ci-bot@elsewhere"}

	// Allocation, not just re-attach: a confined caller has no business
	// allocating a device for a job it does not own either.
	_, err := f.store.AcquireAs(ctx, jobID, "pod-x", mustInstance(t), stranger)
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("allocate for another tenant's job: err = %v, want ErrNotPermitted", err)
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM farm.leases WHERE job_id = $1::uuid`, jobID).Scan(&n); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if n != 0 {
		t.Fatalf("the refused allocation still created %d lease(s)", n)
	}

	// And the re-attach half, against a live lease.
	live, err := f.store.AcquireAs(ctx, jobID, "pod-own", mustInstance(t),
		Caller{Tenant: f.tenantID, Principal: "ci-bot@" + f.tenantID})
	if err != nil {
		t.Fatalf("acquire as the owning tenant: %v", err)
	}
	if _, err := f.store.AcquireAs(ctx, jobID, "pod-x", mustInstance(t), stranger); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("re-attach from another tenant: err = %v, want ErrNotPermitted", err)
	}
	if _, err := f.store.Renew(ctx, live.Lease.ID, live.Lease.Fence, live.Lease.HolderInstance); err != nil {
		t.Fatalf("the holder was fenced by a cross-tenant refusal: %v", err)
	}
}

// TestControlPlaneLeasesHaveAnOwnerAndItIsNotTheCaller covers the attack that
// survives every other check if a lease may exist with no owner on the row.
//
// In the topology this repo ships, the scheduler places jobs and the jobrunner
// runs them, and neither holds an end-user credential. If those allocations left
// holder_principal NULL, every lease in a normal farm would be unowned and any
// authenticated caller who had read a job id off GET /api/v1/leases could
// re-attach it, install its own holder_instance, and fence the running jobrunner
// out with ErrFenced — the whole attack, still open. So an allocation with no
// caller binds the reserved ControlPlanePrincipal instead, and that name is
// refused if a caller tries to present it.
func TestControlPlaneLeasesHaveAnOwnerAndItIsNotTheCaller(t *testing.T) {
	f := newFixture(t, 2)
	ctx := t.Context()

	jobID := f.newJob(t)
	placed, err := f.store.Acquire(ctx, jobID, "scheduler-1", mustInstance(t))
	if err != nil {
		t.Fatalf("control-plane allocation: %v", err)
	}
	if got := f.holderPrincipal(t, placed.Lease.ID); got == nil || *got != ControlPlanePrincipal {
		t.Fatalf("a control-plane allocation left the lease unowned: %v", got)
	}

	// An authenticated caller may not re-attach it. This is the attack in one
	// line: the thief has the job id and a valid tenant token.
	_, err = f.store.AcquireAs(ctx, jobID, "thief-pod", mustInstance(t),
		Caller{Tenant: f.tenantID, Principal: "ci-bot@" + f.tenantID})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("an identified caller re-attached a lease the scheduler placed: err = %v, want ErrNotPermitted", err)
	}

	// Nor may it claim to BE the control plane.
	_, err = f.store.AcquireAs(ctx, jobID, "thief-pod", mustInstance(t),
		Caller{Tenant: f.tenantID, Principal: ControlPlanePrincipal})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a caller impersonated the control plane: err = %v, want ErrNotPermitted", err)
	}

	// The control plane itself still re-attaches, at the same fence: this is
	// the jobrunner picking up after the scheduler, and the jobrunner picking
	// up after its own eviction.
	again, err := f.store.Acquire(ctx, jobID, "jobrunner-1", mustInstance(t))
	if err != nil {
		t.Fatalf("the control plane could not re-attach its own lease: %v", err)
	}
	if !again.Reattached || again.Lease.ID != placed.Lease.ID || again.Lease.Fence != placed.Lease.Fence {
		t.Fatal("the control-plane re-attach did not land on the same lease at the same fence")
	}

	// And the refusals ended nothing: the holder of record can still renew.
	if _, err := f.store.Renew(ctx, again.Lease.ID, again.Lease.Fence, again.Lease.HolderInstance); err != nil {
		t.Fatalf("a refused acquire fenced the rightful holder: %v", err)
	}
}

// TestUnattributedReattachIsAdmittedButRecorded pins the limit this mechanism
// does not close, so that tightening it is a deliberate act rather than an
// accident.
//
// Store.Acquire presents no identity. SQL cannot tell an in-process loop from a
// caller that simply omitted the argument, and the scheduler and jobrunner both
// re-attach legitimately while holding no end-user credential — so an
// unattributed re-attach is admitted against any lease. What it may NOT do is
// take the lease away from the principal it belongs to, and it does not happen
// quietly: the handover ledger names it.
func TestUnattributedReattachIsAdmittedButRecorded(t *testing.T) {
	f := newFixture(t, 2)
	ctx := t.Context()
	owner := Caller{Tenant: f.tenantID, Principal: "ci-bot@" + f.tenantID}

	jobID := f.newJob(t)
	first, err := f.store.AcquireAs(ctx, jobID, "pod-a", mustInstance(t), owner)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The scheduler's re-attach of a job still marked queued.
	sched, err := f.store.Acquire(ctx, jobID, "scheduler-1", mustInstance(t))
	if err != nil {
		t.Fatalf("an unattributed control-plane re-attach was refused: %v", err)
	}
	if !sched.Reattached || sched.Lease.Fence != first.Lease.Fence {
		t.Fatal("the control-plane re-attach did not land on the same lease at the same fence")
	}
	if got := f.holderPrincipal(t, first.Lease.ID); got == nil || *got != owner.Principal {
		t.Fatalf("an unattributed re-attach rebound the lease: %v", got)
	}

	var class string
	err = f.pool.QueryRow(ctx, `
SELECT detail->>'authorised' FROM farm.events
 WHERE kind = 'lease_reattached' AND lease_id = $1::uuid
 ORDER BY at DESC, id DESC LIMIT 1`, first.Lease.ID).Scan(&class)
	if err != nil {
		t.Fatalf("read the handover ledger: %v", err)
	}
	if class != "unattributed" {
		t.Errorf("an unattributed re-attach was logged as %q", class)
	}
}

// TestPreMigrationLeaseIsAdoptedOnce covers the one population that carries no
// principal: leases that were already live when the column was added.
//
// Nothing recorded the acquiring identity before holder_principal existed, so
// there is nothing to backfill from, and refusing these would cost a running job
// its device the moment the upgrade landed. They are adoptable by the first
// identified caller and locked afterwards. Such a lease can no longer be
// produced by farm.lease_acquire, so this seeds one directly — which is exactly
// what the upgrade leaves behind.
func TestPreMigrationLeaseIsAdoptedOnce(t *testing.T) {
	f := newFixture(t, 2)
	ctx := t.Context()

	jobID := f.newJob(t)
	var (
		leaseID string
		fence   int64
	)
	err := f.pool.QueryRow(ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, ttl, grace, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, $1::uuid, $2, $3, 'pre-upgrade-pod', gen_random_uuid(),
       interval '15 minutes', interval '30 minutes',
       now() + interval '15 minutes', now() + interval '45 minutes'
  FROM farm.devices d
 WHERE d.host_id = $4 AND d.current_lease_id IS NULL
 LIMIT 1
RETURNING id::text, fence`, jobID, f.tenantID, f.queueID, f.hostID).Scan(&leaseID, &fence)
	if err != nil {
		t.Fatalf("seed a pre-migration lease: %v", err)
	}
	if got := f.holderPrincipal(t, leaseID); got != nil {
		t.Fatalf("the seeded lease was not unbound: %q", *got)
	}

	owner := Caller{Tenant: f.tenantID, Principal: "ci-bot@" + f.tenantID}
	adopted, err := f.store.AcquireAs(ctx, jobID, "pod-adopt", mustInstance(t), owner)
	if err != nil {
		t.Fatalf("adoption of a pre-migration lease: %v", err)
	}
	if adopted.Lease.ID != leaseID || adopted.Lease.Fence != fence {
		t.Fatal("adoption did not land on the same lease at the same fence")
	}
	if got := f.holderPrincipal(t, leaseID); got == nil || *got != owner.Principal {
		t.Fatalf("the first identified holder did not adopt the lease: %v", got)
	}

	_, err = f.store.AcquireAs(ctx, jobID, "pod-second", mustInstance(t),
		Caller{Tenant: f.tenantID, Principal: "other@" + f.tenantID})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a second principal took an adopted lease: err = %v, want ErrNotPermitted", err)
	}
}

// TestAcquireWithoutCapacityIsAnOrdinaryOutcome pins ErrNoCapacity as a
// scheduling result rather than a failure. Nothing is broken when the farm is
// busy, and nothing about it may look like fencing.
func TestAcquireWithoutCapacityIsAnOrdinaryOutcome(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()

	if _, l := f.acquire(t); l.ID == "" {
		t.Fatal("acquire returned an empty lease id")
	}

	// The pool's only device is now spoken for.
	starved := f.newJob(t)
	_, err := f.store.Acquire(ctx, starved, "pod-starved", mustInstance(t))
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("acquire against a full pool: err = %v, want ErrNoCapacity", err)
	}
	if errors.Is(err, ErrFenced) {
		t.Error("a busy farm reported fencing")
	}
	if errors.Is(err, ErrJobNotFound) {
		t.Error("a busy farm reported a missing job")
	}
}

// TestAcquireAgainstAnEmptyPoolIsNoCapacity covers the pool that has no devices
// at all, as opposed to a pool whose devices are all busy.
//
// The two reach ErrNoCapacity down different paths in farm.lease_acquire — the
// candidate query finds no row versus finds only rows it cannot take — and a
// scheduler that told them apart would be inventing a distinction the caller
// must not act on. Both mean re-queue.
func TestAcquireAgainstAnEmptyPoolIsNoCapacity(t *testing.T) {
	f := newFixture(t, 0)

	job := f.newJob(t)
	_, err := f.store.Acquire(t.Context(), job, "pod-empty", mustInstance(t))
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("acquire against a pool with no devices: err = %v, want ErrNoCapacity", err)
	}
	if errors.Is(err, ErrJobNotFound) {
		t.Error("an empty pool was reported as a missing job; the caller would stop re-queueing a job that is merely waiting")
	}
	if errors.Is(err, ErrFenced) {
		t.Error("an empty pool was reported as fencing")
	}
}

// TestAcquireUnknownJobIsJobNotFound distinguishes "this job does not exist"
// (a caller bug, SQLSTATE P0002) from "nothing is free" (a busy farm). Reading
// the first as the second would leave a mistyped job id retrying forever.
func TestAcquireUnknownJobIsJobNotFound(t *testing.T) {
	f := newFixture(t, 1)

	ghost, err := NewHolderInstance() // any well-formed UUID that names no job
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	_, err = f.store.Acquire(t.Context(), ghost, "pod-ghost", mustInstance(t))
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("acquire for an unknown job: err = %v, want ErrJobNotFound", err)
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Error("an unknown job was reported as a busy farm; the caller would retry forever")
	}
	if errors.Is(err, ErrFenced) {
		t.Error("an unknown job was reported as fencing")
	}
}

// =====================================================================
// Renew — the distinction the whole system rests on
// =====================================================================

// TestRenewZeroRowsIsFencedAndTerminal covers every way farm.lease_renew can
// match no row. All of them mean the same thing to the holder: the lease is
// gone and the device may already be running somebody else's job.
func TestRenewZeroRowsIsFencedAndTerminal(t *testing.T) {
	f := newFixture(t, 3)
	ctx := t.Context()

	t.Run("stale fence", func(t *testing.T) {
		_, l := f.acquire(t)
		_, err := f.store.Renew(ctx, l.ID, l.Fence+1, l.HolderInstance)
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("renew at the wrong fence: err = %v, want ErrFenced", err)
		}
	})

	t.Run("another process incarnation holds it", func(t *testing.T) {
		_, l := f.acquire(t)
		_, err := f.store.Renew(ctx, l.ID, l.Fence, mustInstance(t))
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("renew with a foreign holder instance: err = %v, want ErrFenced", err)
		}
	})

	t.Run("terminal lease stays terminal", func(t *testing.T) {
		_, l := f.acquire(t)
		released, err := f.store.Release(ctx, l.ID, l.Fence, ReasonCompleted, time.Second)
		if err != nil || !released {
			t.Fatalf("release: released = %v, err = %v", released, err)
		}

		// Fencing is not a transient state that a later attempt can recover
		// from. Every subsequent renewal must reach the same verdict, so a
		// holder that retried anyway learns nothing new and gains nothing.
		for attempt := 1; attempt <= 3; attempt++ {
			_, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
			if !errors.Is(err, ErrFenced) {
				t.Fatalf("renew %d of a released lease: err = %v, want ErrFenced", attempt, err)
			}
		}
	})
}

// TestRenewErrorsThatAreNotFencing is the test that would have caught
// DeviceFarmer/STF #663 in review.
//
// #663 is one fused decision: a transport failure was allowed to mean "the
// holder is gone", and a device was released mid-run after a ~90 minute
// ECONNRESET. Here the failures are on the CONTROL PLANE side rather than the
// ADB side, but the fusion would be the same mistake and the blast radius
// larger: every holder in the farm renews against the same Postgres, so a
// database that treats its own unavailability as fencing empties the farm.
//
// Every case below leaves the lease untouched at the server. Not one of them
// may satisfy errors.Is(err, ErrFenced).
func TestRenewErrorsThatAreNotFencing(t *testing.T) {
	t.Run("unreachable database", func(t *testing.T) {
		// Deliberately needs no live database: a dial failure is the one
		// transient error every machine can reproduce, and the property it
		// pins is the one that must never regress.
		store := NewStore(unreachablePool(t))

		_, err := store.Renew(t.Context(), "00000000-0000-0000-0000-000000000000", 1,
			"00000000-0000-0000-0000-000000000000")
		requireNotFenced(t, err, "a dial failure")
	})

	t.Run("cancelled context", func(t *testing.T) {
		f := newFixture(t, 1)
		_, l := f.acquire(t)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
		requireNotFenced(t, err, "a cancelled context")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled", err)
		}

		// The lease is untouched: the same holder renews it a moment later.
		if _, err := f.store.Renew(t.Context(), l.ID, l.Fence, l.HolderInstance); err != nil {
			t.Errorf("renew after a cancelled attempt: %v; the lease was not supposed to move", err)
		}
	})

	t.Run("closed pool", func(t *testing.T) {
		f := newFixture(t, 1)
		_, l := f.acquire(t)

		// A pool of its own, so closing it cannot disturb the other tests. This
		// stands in for a pod tearing its own connections down mid-flight.
		doomed := clonePool(t, f.pool, nil)
		store := NewStore(doomed)
		doomed.Close()

		_, err := store.Renew(t.Context(), l.ID, l.Fence, l.HolderInstance)
		requireNotFenced(t, err, "a closed connection pool")

		if _, err := f.store.Renew(t.Context(), l.ID, l.Fence, l.HolderInstance); err != nil {
			t.Errorf("renew over a healthy pool afterwards: %v; the lease was not supposed to move", err)
		}
	})

	t.Run("timed out attempt", func(t *testing.T) {
		f := newFixture(t, 1)
		_, l := f.acquire(t)

		// An expired deadline is what a wedged connection looks like to the
		// caller, and it is the shape of failure #663 mishandled.
		ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond)

		_, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
		requireNotFenced(t, err, "an expired deadline")

		if _, err := f.store.Renew(t.Context(), l.ID, l.Fence, l.HolderInstance); err != nil {
			t.Errorf("renew after a timed out attempt: %v; the lease was not supposed to move", err)
		}
	})
}

// requireNotFenced is the assertion #663 is about: this failure is not the loss
// of a device.
func requireNotFenced(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s produced no error at all", what)
	}
	if errors.Is(err, ErrFenced) {
		t.Fatalf("%s was reported as fencing (%v); a holder would abort a running job on it", what, err)
	}
}

// TestRenewMovesDeadlinesForwardOnly checks that a successful renewal reports
// server-computed deadlines and never pulls one backwards. The cached copy on
// the holder is only ever as good as this.
func TestRenewMovesDeadlinesForwardOnly(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()
	_, l := f.acquire(t)

	first, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if first.WasSuspect {
		t.Error("a freshly acquired lease renewed as suspect")
	}
	if !first.ReclaimableAt.After(first.ExpiresAt) {
		t.Errorf("reclaimable_at %s is not after expires_at %s; the grace band vanished",
			first.ReclaimableAt, first.ExpiresAt)
	}

	second, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
	if err != nil {
		t.Fatalf("second renew: %v", err)
	}
	if second.ExpiresAt.Before(first.ExpiresAt) {
		t.Errorf("expires_at moved backwards: %s -> %s", first.ExpiresAt, second.ExpiresAt)
	}
	if second.ReclaimableAt.Before(first.ReclaimableAt) {
		t.Errorf("reclaimable_at moved backwards: %s -> %s", first.ReclaimableAt, second.ReclaimableAt)
	}
}

// TestRenewSelfHealsSuspect proves that suspect is an alert and not a loss. A
// heartbeat arriving inside the grace band restores the lease at the SAME
// fence, on the same device, with no work lost.
func TestRenewSelfHealsSuspect(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()
	jobID, l := f.acquire(t)

	// Push the lease past its TTL but well inside its grace, then let the
	// sweeper flag it.
	f.backdateLease(t, l.ID, 20*time.Minute)
	suspects, err := f.store.MarkSuspect(ctx, DefaultSuspectBatch)
	if err != nil {
		t.Fatalf("mark suspect: %v", err)
	}
	flagged, ok := findSuspect(suspects, l.ID)
	if !ok {
		t.Fatalf("sweeper did not flag the overdue lease %s", l.ID)
	}
	if flagged.DeviceID != l.DeviceID || flagged.JobID != jobID {
		t.Errorf("the alert names device %s job %s, want %s and %s",
			flagged.DeviceID, flagged.JobID, l.DeviceID, jobID)
	}
	if flagged.Protected {
		t.Error("an ordinary lease was flagged Protected; the reaper would never take it and a human would be paged for nothing")
	}
	if state, _ := f.leaseState(t, l.ID); state != "suspect" {
		t.Fatalf("lease state = %q, want suspect", state)
	}

	// Entering suspect DOES NOTHING, and "nothing" is a claim about the device
	// rather than about the lease row. The device is still bound to this lease
	// and the fence floor has not moved, so the job's sockets are still accepted
	// at the host proxy while the alert is outstanding.
	binding, floor := f.deviceBinding(t, l.DeviceID)
	if binding == nil || *binding != l.ID {
		t.Errorf("devices.current_lease_id = %v after the sweep, want %s; the sweep unbound a device it only had to alert on",
			binding, l.ID)
	}
	if floor != l.Fence {
		t.Errorf("fence_floor = %d after the sweep, want the holder's own fence %d; the sweep fenced a job it only had to alert on",
			floor, l.Fence)
	}
	if _, reason := f.leaseState(t, l.ID); reason != nil {
		t.Errorf("release_reason = %q after the sweep; suspect released the lease", *reason)
	}

	res, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
	if err != nil {
		t.Fatalf("renew a suspect lease: %v; suspect is an alert, not an ending", err)
	}
	if !res.WasSuspect {
		t.Error("renewal did not report that it self-healed a suspect lease")
	}
	state, reason := f.leaseState(t, l.ID)
	if state != "held" {
		t.Errorf("lease state = %q after renewal, want held", state)
	}
	if reason != nil {
		t.Errorf("release_reason = %q; nothing released this lease", *reason)
	}
}

// findSuspect looks one lease up in a sweep's rows. Membership rather than
// equality, because farm.lease_mark_suspect is farm-wide: it flags every overdue
// lease in the scratch database, not only this fixture's.
func findSuspect(suspects []SuspectLease, id string) (SuspectLease, bool) {
	for _, s := range suspects {
		if s.LeaseID == id {
			return s, true
		}
	}
	return SuspectLease{}, false
}

// findReclaimed is the same lookup over a reclaim sweep.
func findReclaimed(reclaimed []ReclaimedLease, id string) (ReclaimedLease, bool) {
	for _, r := range reclaimed {
		if r.LeaseID == id {
			return r, true
		}
	}
	return ReclaimedLease{}, false
}

// =====================================================================
// Release
// =====================================================================

// TestReleaseWithStaleFenceIsRefused covers the process that woke up late. Its
// fence no longer matches, so its release must not end the lease that replaced
// it — and must not look like an error either, because being refused here is
// exactly what is supposed to happen to it.
func TestReleaseWithStaleFenceIsRefused(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()
	_, l := f.acquire(t)

	released, err := f.store.Release(ctx, l.ID, l.Fence+1, ReasonCompleted, time.Second)
	if err != nil {
		t.Fatalf("release at a stale fence: %v; a refusal is not an error", err)
	}
	if released {
		t.Fatal("a stale fence released the lease")
	}

	state, reason := f.leaseState(t, l.ID)
	if state != "held" {
		t.Errorf("lease state = %q, want held; the stale release took effect", state)
	}
	if reason != nil {
		t.Errorf("release_reason = %q, want none", *reason)
	}

	// The rightful holder is unaffected.
	if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); err != nil {
		t.Errorf("renew after a refused release: %v", err)
	}
}

// TestReleaseEndsTheLeaseAndIsIdempotent covers the normal end of a job, and
// the second call a retrying supervisor makes.
func TestReleaseEndsTheLeaseAndIsIdempotent(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()
	_, l := f.acquire(t)

	released, err := f.store.Release(ctx, l.ID, l.Fence, ReasonCompleted, time.Second)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released {
		t.Fatal("release reported that it changed nothing")
	}

	state, reason := f.leaseState(t, l.ID)
	if state != "released" {
		t.Errorf("lease state = %q, want released", state)
	}
	if reason == nil || *reason != string(ReasonCompleted) {
		t.Errorf("release_reason = %v, want %q", reason, ReasonCompleted)
	}

	// The fence floor is now above the fence we held, so a socket still
	// carrying it is refused at the host proxy rather than merely here.
	var floor int64
	if err := f.pool.QueryRow(ctx,
		`SELECT fence_floor FROM farm.devices WHERE id = $1::uuid`, l.DeviceID).Scan(&floor); err != nil {
		t.Fatalf("read fence floor: %v", err)
	}
	if floor <= l.Fence {
		t.Errorf("fence_floor = %d, want above the released fence %d", floor, l.Fence)
	}

	again, err := f.store.Release(ctx, l.ID, l.Fence, ReasonCompleted, time.Second)
	if err != nil {
		t.Fatalf("second release: %v; the idempotent case is not an error", err)
	}
	if again {
		t.Error("second release reported that it ended the lease a second time")
	}
}

// TestReleaseRefusesAConnectivityReason is the schema's half of the #663
// countermeasure, observed from Go.
//
// There is no release reason that describes connectivity, so "released because
// the transport dropped" cannot be written. The refusal comes from Postgres
// with SQLSTATE 23514 and surfaces as *CheckViolationError — deliberately not
// as a client-side validation, so the enforcement point stays the database and
// stays tested.
func TestReleaseRefusesAConnectivityReason(t *testing.T) {
	f := newFixture(t, 1)
	ctx := t.Context()
	_, l := f.acquire(t)

	for _, reason := range []ReleaseReason{"device_offline", "transport_error", "unreachable", "probe_failed"} {
		if reason.Valid() {
			t.Fatalf("%q is a permitted release reason; the domain grew a word for connectivity", reason)
		}

		released, err := f.store.Release(ctx, l.ID, l.Fence, reason, time.Second)
		if released {
			t.Fatalf("release with reason %q ended the lease", reason)
		}
		var check *CheckViolationError
		if !errors.As(err, &check) {
			t.Fatalf("release with reason %q: err = %v, want *CheckViolationError", reason, err)
		}
		if check.Reason != reason {
			t.Errorf("CheckViolationError.Reason = %q, want %q", check.Reason, reason)
		}
		if check.Op != "release" {
			t.Errorf("CheckViolationError.Op = %q, want %q", check.Op, "release")
		}
	}

	// The job is still holding its device, which is the entire point.
	if state, _ := f.leaseState(t, l.ID); state != "held" {
		t.Errorf("lease state = %q after four refused releases, want held", state)
	}
	if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); err != nil {
		t.Errorf("renew after refused releases: %v", err)
	}
}

// =====================================================================
// Witness
// =====================================================================

// TestWitnessExtendsIsCappedAndRefusalIsNotFencing covers the three things a
// caller has to know about on-device evidence: it buys time, it cannot buy
// unlimited time, and being refused it costs nothing.
func TestWitnessExtendsIsCappedAndRefusalIsNotFencing(t *testing.T) {
	f := newFixture(t, 3)
	ctx := t.Context()

	t.Run("extends reclaimable_at", func(t *testing.T) {
		_, l := f.acquire(t)
		// A witness pushes reclaimable_at to now()+grace, so it only moves
		// anything once the lease has run past its TTL — which is precisely
		// when a job that lost the control plane needs the room.
		f.backdateLease(t, l.ID, 20*time.Minute)

		before := f.reclaimableAt(t, l.ID)
		at, ok, err := f.store.Witness(ctx, l.ID, l.Fence, DefaultWitnessMaxExtensions)
		if err != nil {
			t.Fatalf("witness: %v", err)
		}
		if !ok {
			t.Fatal("witness was refused for a live lease inside its cap")
		}
		if !at.After(before) {
			t.Errorf("reclaimable_at %s did not move past %s", at, before)
		}
		if got := f.reclaimableAt(t, l.ID); !got.Equal(at) {
			t.Errorf("stored reclaimable_at %s does not match the reported %s", got, at)
		}
	})

	t.Run("capped, and the refusal is not fencing", func(t *testing.T) {
		_, l := f.acquire(t)
		f.backdateLease(t, l.ID, 20*time.Minute)

		// A cap of one, so the second consecutive witness is refused. The cap
		// is what stops a wedged agent from holding a handset forever on
		// device-side evidence alone.
		if _, ok, err := f.store.Witness(ctx, l.ID, l.Fence, 1); err != nil || !ok {
			t.Fatalf("first witness: ok = %v, err = %v", ok, err)
		}
		at, ok, err := f.store.Witness(ctx, l.ID, l.Fence, 1)
		// A refused witness is not an error at all, and above all it is not
		// ErrFenced: only Renew may report that. Asserting on nil rather than on
		// errors.Is covers both, and covers whatever third thing a future
		// refactor might decide to return here.
		if err != nil {
			t.Fatalf("witness past the cap returned an error: %v; being refused costs a job nothing, "+
				"and if that error were %v a holder would abort a running job on it", err, ErrFenced)
		}
		if ok {
			t.Fatal("witness past the cap was accepted")
		}
		if !at.IsZero() {
			t.Errorf("refused witness reported a deadline: %s", at)
		}
		if state, _ := f.leaseState(t, l.ID); state != "held" {
			t.Errorf("lease state = %q after a refused witness, want held", state)
		}
		if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); err != nil {
			t.Errorf("renew after a refused witness: %v; the lease was not supposed to move", err)
		}

		// A successful renewal resets the consecutive-extension counter, so the
		// witness protects the rest of the run rather than nothing.
		f.backdateLease(t, l.ID, 20*time.Minute)
		if _, ok, err := f.store.Witness(ctx, l.ID, l.Fence, 1); err != nil || !ok {
			t.Errorf("witness after a renewal reset the cap: ok = %v, err = %v", ok, err)
		}
	})

	t.Run("stale fence is refused, not fenced", func(t *testing.T) {
		_, l := f.acquire(t)
		f.backdateLease(t, l.ID, 20*time.Minute)

		at, ok, err := f.store.Witness(ctx, l.ID, l.Fence+1, DefaultWitnessMaxExtensions)
		if err != nil {
			t.Fatalf("witness at a stale fence returned an error: %v", err)
		}
		if ok {
			t.Fatal("a stale fence produced an accepted witness")
		}
		if !at.IsZero() {
			t.Errorf("refused witness reported a deadline: %s", at)
		}
		if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); err != nil {
			t.Errorf("renew after a stale witness: %v", err)
		}
	})
}

func (f *fixture) reclaimableAt(t *testing.T, leaseID string) time.Time {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT reclaimable_at FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&at); err != nil {
		t.Fatalf("read reclaimable_at: %v", err)
	}
	return at
}

// =====================================================================
// Reclaim — the only automatic release path, and the guards around it
// =====================================================================

// TestReclaimTakesOnlyAnAbandonedLeaseAndEveryGuardHolds is the counterpart to
// TestRenewErrorsThatAreNotFencing, seen from the reaper's side.
//
// Renew answers "may this holder keep its device?". Reclaim answers "may the
// farm take a device back without anybody asking?", and it is the only code in
// the system allowed to answer yes. Every guard below is a reason to answer no,
// and each one exists because answering yes wrongly does not cost one job — it
// costs every job in the farm at once, since every holder renews against the
// same Postgres and goes quiet together.
//
// Each negative case carries a POSITIVE CONTROL: a sibling lease in the same
// sweep that must be reclaimed. Without it, a reclaim that returned nothing at
// all — a quiesce window, a stray gap row, a broken function — would satisfy
// every "was not reclaimed" assertion here while proving nothing.
func TestReclaimTakesOnlyAnAbandonedLeaseAndEveryGuardHolds(t *testing.T) {
	f := newFixture(t, 8)
	ctx := t.Context()
	requireReaperMayAct(t, f.pool)

	t.Run("an abandoned unprotected lease is reclaimed, fenced and quarantined", func(t *testing.T) {
		jobID, l := f.acquire(t)
		f.abandon(t, l.ID)

		_, floorBefore := f.deviceBinding(t, l.DeviceID)
		if floorBefore != l.Fence {
			t.Fatalf("fence_floor = %d before the reclaim, want the holder's fence %d", floorBefore, l.Fence)
		}

		got, err := f.store.Reclaim(ctx, DefaultReclaimBatch, time.Second)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		r, ok := findReclaimed(got, l.ID)
		if !ok {
			t.Fatalf("a lease silent for longer than TTL+grace was not reclaimed; the reaper took %+v", got)
		}
		if r.DeviceID != l.DeviceID || r.JobID != jobID {
			t.Errorf("reclaimed row names device %s job %s, want %s and %s", r.DeviceID, r.JobID, l.DeviceID, jobID)
		}
		if r.OldFence != l.Fence {
			t.Errorf("OldFence = %d, want the fence the departed holder still believes it owns, %d", r.OldFence, l.Fence)
		}
		// The handover is only safe because the floor moved: a socket still
		// carrying OldFence is refused at the host proxy, not merely here.
		if r.NewFloor <= r.OldFence {
			t.Errorf("NewFloor = %d is not above OldFence = %d; the departed holder can still drive the device", r.NewFloor, r.OldFence)
		}
		binding, floor := f.deviceBinding(t, l.DeviceID)
		if binding != nil {
			t.Errorf("devices.current_lease_id = %s after the reclaim, want none", *binding)
		}
		if floor != r.NewFloor {
			t.Errorf("stored fence_floor %d does not match the reported NewFloor %d", floor, r.NewFloor)
		}

		state, reason := f.leaseState(t, l.ID)
		if state != "expired" {
			t.Errorf("lease state = %q, want expired", state)
		}
		if reason == nil || *reason != string(ReasonHolderExpired) {
			t.Errorf("release_reason = %v, want %q", reason, ReasonHolderExpired)
		}

		// And the departed holder learns of it the only way it is allowed to:
		// its next renewal comes back fenced.
		if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); !errors.Is(err, ErrFenced) {
			t.Errorf("renew after a reclaim: err = %v, want ErrFenced", err)
		}
	})

	t.Run("a protected lease is held and the human is paged instead", func(t *testing.T) {
		_, prot := f.acquireWith(t, jobOpts{protected: true})
		_, control := f.acquire(t)

		suspects := f.abandon(t, prot.ID)
		f.abandon(t, control.ID)

		// The alert must carry the flag, or the operator is never told that this
		// is the one lease nobody is coming to clean up automatically.
		flagged, ok := findSuspect(suspects, prot.ID)
		if !ok {
			t.Fatalf("the protected lease was not flagged suspect at all")
		}
		if !flagged.Protected {
			t.Error("the alert for a protected lease does not carry Protected; nobody would know to page a human")
		}

		got, err := f.store.Reclaim(ctx, DefaultReclaimBatch, time.Second)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if _, ok := findReclaimed(got, control.ID); !ok {
			t.Fatalf("the unprotected control lease was not reclaimed either, so this subtest proves "+
				"nothing about protection; the reaper took %+v", got)
		}
		if _, ok := findReclaimed(got, prot.ID); ok {
			t.Error("a protected lease was reclaimed; long jobs are held and a human paged, never taken")
		}

		if state, reason := f.leaseState(t, prot.ID); state != "suspect" || reason != nil {
			t.Errorf("protected lease is state %q reason %v, want suspect and no reason", state, reason)
		}
		// Nothing was lost: the job is still holding its device and a heartbeat
		// self-heals it at the same fence.
		if _, err := f.store.Renew(ctx, prot.ID, prot.Fence, prot.HolderInstance); err != nil {
			t.Errorf("renew a protected lease the reaper passed over: %v; the protection was worthless", err)
		}
	})

	t.Run("on-device evidence of the holder keeps the device", func(t *testing.T) {
		_, witnessed := f.acquire(t)
		_, control := f.acquire(t)
		f.abandon(t, witnessed.ID)
		f.abandon(t, control.ID)

		// The job lost the control plane but its own agent is demonstrably still
		// running on the handset. That is evidence about the HOLDER, and it is
		// the difference between a reaper that reclaims a live six-hour run and
		// one that does not.
		at, ok, err := f.store.Witness(ctx, witnessed.ID, witnessed.Fence, DefaultWitnessMaxExtensions)
		if err != nil || !ok {
			t.Fatalf("witness a suspect lease: ok = %v, err = %v", ok, err)
		}
		if !at.After(time.Now()) {
			t.Fatalf("the witness pushed reclaimable_at to %s, which is not in the future", at)
		}

		got, err := f.store.Reclaim(ctx, DefaultReclaimBatch, time.Second)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if _, ok := findReclaimed(got, control.ID); !ok {
			t.Fatalf("the unwitnessed control lease was not reclaimed either, so this subtest proves "+
				"nothing about the witness; the reaper took %+v", got)
		}
		if _, ok := findReclaimed(got, witnessed.ID); ok {
			t.Error("a witnessed lease was reclaimed; the job was demonstrably alive on the device")
		}
		if _, err := f.store.Renew(ctx, witnessed.ID, witnessed.Fence, witnessed.HolderInstance); err != nil {
			t.Errorf("renew a witnessed lease the reaper passed over: %v", err)
		}
	})

	// Deliberately last, and it restores what it changes. The gap and the
	// quiesce window it creates are farm-global and would gate every reclaim
	// above shut if they leaked, which is exactly the silent-pass this test
	// exists to rule out.
	t.Run("a control-plane outage is refunded, not charged to the tenant", func(t *testing.T) {
		isolateReaperGlobals(t, f.pool)

		_, a := f.acquire(t)
		_, b := f.acquire(t)
		f.abandon(t, a.ID)
		f.abandon(t, b.ID)

		// Both are genuinely due. Asserting it here is what makes the "nothing
		// was reclaimed" verdict below mean something.
		for _, l := range []Lease{a, b} {
			if due := f.reclaimableAt(t, l.ID); !due.Before(time.Now()) {
				t.Fatalf("lease %s is reclaimable at %s, still in the future; the refund below would prove nothing", l.ID, due)
			}
		}

		// Every component on the renewal path went quiet ten minutes ago. From
		// the holders' side this is indistinguishable from all of them dying at
		// once, which is precisely why the reaper may not read it that way.
		for _, c := range ReaperComponents {
			if err := f.store.ComponentBeat(ctx, c); err != nil {
				t.Fatalf("component beat %s: %v", c, err)
			}
		}
		backdateHeartbeats(t, f.pool, 10*time.Minute)

		armed, err := f.store.ReaperArm(ctx, ReaperComponents, DefaultGapFloor)
		if err != nil {
			t.Fatalf("reaper arm: %v", err)
		}
		if !armed.Armed {
			t.Fatalf("reaper arm refused with every component beating (unbeaten=%v); the refund below "+
				"would prove nothing", armed.Unbeaten)
		}
		if gap := armed.Gap; gap < 9*time.Minute {
			t.Fatalf("reaper arm reported a %s outage after ten minutes of silence across every component; "+
				"an unrecorded gap is a farm-wide reclaim at the moment of recovery", gap)
		}

		got, err := f.store.Reclaim(ctx, DefaultReclaimBatch, time.Second)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("the reaper took %d lease(s) immediately after the control plane came back: %+v; "+
				"this is the farm-wide reclaim the gap refund exists to prevent", len(got), got)
		}

		for _, l := range []Lease{a, b} {
			if state, reason := f.leaseState(t, l.ID); state == "expired" || reason != nil {
				t.Errorf("lease %s is state %q reason %v after an outage; our downtime was charged to the tenant",
					l.ID, state, reason)
			}
			if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); err != nil {
				t.Errorf("renew %s after a control-plane outage: %v; the job lost its device to our downtime", l.ID, err)
			}
		}
	})
}

// TestExpireMaxRuntimeEndsTheLeaseAndFencesLikeAnyOtherEnding covers the second
// of the two automatic endings, and the only one driven by a number the user
// wrote down themselves.
//
// It is an ending like any other, so it must fence like one: farm.lease_release
// and farm.lease_reclaim both raise devices.fence_floor and quarantine the slot,
// and a max_runtime expiry that skipped either would return a handset to the
// pool while the previous holder's fence was still accepted at the host proxy.
func TestExpireMaxRuntimeEndsTheLeaseAndFencesLikeAnyOtherEnding(t *testing.T) {
	f := newFixture(t, 2)
	ctx := t.Context()

	// A job whose author wrote down ten minutes, and one who wrote down nothing.
	// The second is the control: this sweep fires on a user-supplied clock and
	// on nothing else, so a lease with no such clock must survive it however
	// long it has been running.
	jobID, l := f.acquireWith(t, jobOpts{maxRuntime: 10 * time.Minute})
	_, control := f.acquire(t)
	f.backdateLease(t, l.ID, 20*time.Minute)
	f.backdateLease(t, control.ID, 20*time.Minute)

	_, floorBefore := f.deviceBinding(t, l.DeviceID)

	expired, err := f.store.ExpireMaxRuntime(ctx, DefaultReclaimBatch)
	if err != nil {
		t.Fatalf("expire max runtime: %v\n"+
			"migrations/00005_correctness.sql adds farm.lease_expire_max_runtime(int, interval) but never "+
			"drops the (int) overload it was written to replace, so Store.ExpireMaxRuntime's one-argument "+
			"call is ambiguous (SQLSTATE 42725) and max_runtime — one of the three endings the invariant "+
			"permits — never fires at all. Fix in internal/lease/store.go: call\n"+
			"  FROM farm.lease_expire_max_runtime($1::int, $2::interval) AS e\n"+
			"passing DefaultRearm, which resolves to the fencing version.", err)
	}

	var got *ExpiredLease
	for i := range expired {
		if expired[i].LeaseID == l.ID {
			got = &expired[i]
		}
		if expired[i].LeaseID == control.ID {
			t.Errorf("the sweep ended lease %s, whose job set no max_runtime; nothing but a user-supplied clock may end a lease here", control.ID)
		}
	}
	if got == nil {
		t.Fatalf("a lease that outran its job's max_runtime was not ended; the sweep returned %+v", expired)
	}
	if got.DeviceID != l.DeviceID || got.JobID != jobID {
		t.Errorf("expired row names device %s job %s, want %s and %s", got.DeviceID, got.JobID, l.DeviceID, jobID)
	}

	state, reason := f.leaseState(t, l.ID)
	if state != "expired" {
		t.Errorf("lease state = %q, want expired", state)
	}
	if reason == nil || *reason != string(ReasonMaxRuntime) {
		t.Errorf("release_reason = %v, want %q", reason, ReasonMaxRuntime)
	}

	binding, floor := f.deviceBinding(t, l.DeviceID)
	if binding != nil {
		t.Errorf("devices.current_lease_id = %s after the expiry, want none", *binding)
	}
	if floor <= floorBefore {
		t.Errorf("fence_floor = %d, unchanged from %d; the device went back into the pool while the "+
			"previous holder's fence was still accepted at the host proxy", floor, floorBefore)
	}
	if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); !errors.Is(err, ErrFenced) {
		t.Errorf("renew after a max_runtime expiry: err = %v, want ErrFenced", err)
	}

	// The control lease never had a clock on it and is untouched.
	if state, reason := f.leaseState(t, control.ID); state == "expired" || reason != nil {
		t.Errorf("the control lease is state %q reason %v, want it untouched", state, reason)
	}
}

// requireReaperMayAct fails the test when the farm-global state that gates
// EVERY reclaim is already closed.
//
// farm.reaper_state and farm.control_plane_gap are not fixture-scoped. One
// quiesce window, or one leftover gap row, makes farm.lease_reclaim return
// nothing whatsoever — and then every "this lease was NOT reclaimed" assertion
// passes while testing nothing at all. Checking first turns that silent
// agreement into a loud failure that names the cause.
func requireReaperMayAct(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var quiesced, enabled bool
	var gaps int
	err := pool.QueryRow(t.Context(),
		`SELECT r.quiesce_until > now(), r.enabled,
		        (SELECT count(*) FROM farm.control_plane_gap g
		          WHERE g.ended_at > now() - interval '6 hours')::int
		   FROM farm.reaper_state r`).Scan(&quiesced, &enabled, &gaps)
	if err != nil {
		t.Fatalf("read the reaper's global state: %v", err)
	}
	if quiesced || !enabled || gaps != 0 {
		t.Fatalf("the reaper cannot act (quiesced=%v enabled=%v gaps in the last six hours=%d), so nothing "+
			"below would be tested; an earlier test in this run armed the reaper or recorded a "+
			"control-plane gap and did not restore farm.reaper_state / farm.control_plane_gap",
			quiesced, enabled, gaps)
	}
}

// isolateReaperGlobals restores the farm-global tables the arm path writes.
//
// farm.reaper_arm records a control-plane gap and opens a quiesce window, and
// both outlive the test that caused them: a gap blocks the reclaim of every
// lease whose holder went quiet before it, for six hours. Leaving either behind
// would make every reclaim test that follows pass by doing nothing.
func isolateReaperGlobals(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	var highWater int64
	if err := pool.QueryRow(t.Context(),
		`SELECT COALESCE(max(id), 0) FROM farm.control_plane_gap`).Scan(&highWater); err != nil {
		t.Fatalf("read the control-plane gap high-water mark: %v", err)
	}

	// Detached from the test's context, which is already cancelled by the time
	// cleanups run.
	ctx := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// Back to the state migration 00001 leaves behind: armed at now(), so
		// the reaper may act immediately. Written as now() rather than as a
		// snapshotted instant, because no timestamp this process invented has
		// any business reaching the database.
		if _, err := pool.Exec(cctx,
			`UPDATE farm.reaper_state SET quiesce_until = now(), armed_at = now(),
			        last_refusal = NULL, last_refusal_at = NULL`); err != nil {
			t.Errorf("restore farm.reaper_state: %v; every reclaim test later in this run is now gated shut", err)
		}
		if _, err := pool.Exec(cctx,
			`DELETE FROM farm.control_plane_gap WHERE id > $1`, highWater); err != nil {
			t.Errorf("delete the control-plane gap this test recorded: %v; every reclaim test later in this run "+
				"will find its leases unreclaimable", err)
		}
		if _, err := pool.Exec(cctx,
			`DELETE FROM farm.component_heartbeat WHERE component = ANY($1)`, ReaperComponents); err != nil {
			t.Errorf("delete the component heartbeats this test wrote: %v; the next arm would measure an outage "+
				"from this test's clock", err)
		}
	})
}

// backdateHeartbeats moves every component's beat into the past, which is what a
// control-plane outage looks like to farm.reaper_arm. A relative interval, so
// the length of the outage is the only thing this process asserts and the
// instant it ended stays the server's.
func backdateHeartbeats(t *testing.T, pool *pgxpool.Pool, by time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE farm.component_heartbeat SET beat_at = beat_at - $1::interval
		  WHERE component = ANY($2)`, intervalArg(by), ReaperComponents); err != nil {
		t.Fatalf("backdate the component heartbeats: %v", err)
	}
}

// =====================================================================
// Shared helpers
// =====================================================================

func mustInstance(t *testing.T) string {
	t.Helper()
	id, err := NewHolderInstance()
	if err != nil {
		t.Fatalf("holder instance: %v", err)
	}
	return id
}

// clonePool opens a second pool with the same configuration, for tests that
// need to break a pool without breaking everyone else's. mutate, if given,
// edits the copied configuration before the pool is opened.
func clonePool(t *testing.T, src *pgxpool.Pool, mutate func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	cfg := src.Config().Copy()
	if mutate != nil {
		mutate(cfg)
	}
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("clone pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// unreachablePool returns a pool pointed at a port nothing is listening on.
//
// The port is obtained by binding and immediately releasing it, so the address
// is one the kernel just confirmed is free rather than one guessed to be.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a dead port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the dead port: %v", err)
	}

	cfg, err := pgxpool.ParseConfig("postgres://nobody@" + addr + "/nowhere?sslmode=disable")
	if err != nil {
		t.Fatalf("parse dead dsn: %v", err)
	}
	// pgxpool connects lazily, so this succeeds and the failure lands on the
	// first query — which is where a holder would meet it too.
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open dead pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}
