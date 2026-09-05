package demo

// The simulation is a HOLDER, and a holder that takes work it did not write
// releases somebody else's lease on the wrong clock.
//
// Its model of "how long this job takes" is the step COUNT (schedulableJobs
// reads jsonb_array_length(spec->'steps')), not what the steps say. For the
// jobs it generates that is exact enough. For a job an operator submitted
// through the API it is not: the real scheduler and the real jobrunner run in
// the same process — cmd/farmd's runDemo starts both — so both drivers took
// the job, the simulation released the lease on its own four-second timetable,
// and the operator's steps were left in farm.job_steps still marked 'running'
// under a log line reading "job complete, lease released".
//
// Observed live on 2026-09-05, on a demo built from this tree: a submitted
// 100-second sleep step was acquired at 17:14:16, re-attached by the real
// jobrunner at 17:14:17, and released by the simulation at 17:14:20.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver, which goose speaks.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/migrations"
)

// testPool is the pool for the scratch database, or nil when DATABASE_URL was
// unset and the SQL-backed tests must skip.
var testPool *pgxpool.Pool

// setupLockKey serialises scratch-database creation across packages: the
// migration set creates cluster-wide roles, so two suites migrating at once
// would both find a role missing and both try to create it.
const setupLockKey int64 = 0x6466_7465_7374_0001

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite owns the scratch database's whole lifetime. It returns an exit code
// rather than calling os.Exit itself so its deferred teardown actually runs.
func runSuite(m *testing.M) (code int) {
	base := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if base == "" {
		return m.Run()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A misconfigured DATABASE_URL is a failure, never a skip: skipping here
	// would mean a CI job that quietly tests nothing while reporting success.
	admin, err := sql.Open("pgx", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := fmt.Sprintf("df_demo_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "demo tests: create scratch database (the role needs CREATEDB): %v\n", err)
		return 1
	}
	defer func() {
		if testPool != nil {
			testPool.Close()
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "demo tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: setup lock was not given back: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "demo tests: %v\n", migrateErr)
		return 1
	}

	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: parse scratch DSN: %v\n", err)
		return 1
	}
	pc.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo tests: connect to scratch database: %v\n", err)
		return 1
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "demo tests: ping scratch database: %v\n", err)
		return 1
	}
	testPool = pool

	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set, the same bytes the shipped
// binary carries, so the tests cannot pass against a schema no deployment gets.
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

type quietGooseLogger struct{}

func (quietGooseLogger) Printf(string, ...any) {}
func (quietGooseLogger) Fatalf(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "goose: "+format+"\n", v...)
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

func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed demo tests")
	}
	return testPool
}

// TestTheSimulationSchedulesOnlyItsOwnJobs is the guard.
//
// Falsify: drop `AND j.created_by = $2` from schedulableJobs. The submitted
// job appears in the returned set and the subtest fails naming it.
func TestTheSimulationSchedulesOnlyItsOwnJobs(t *testing.T) {
	pool := requireDB(t)
	ctx := t.Context()

	seedJobFixture(t, pool)

	mine := insertQueuedJob(t, pool, feederCreatedBy)
	theirs := insertQueuedJob(t, pool, "alice@example.com")

	r := &Runner{pool: pool, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	jobs, err := r.schedulableJobs(ctx)
	if err != nil {
		t.Fatalf("schedulableJobs: %v", err)
	}

	got := map[string]bool{}
	for _, j := range jobs {
		got[j.id] = true
	}
	if !got[mine] {
		t.Errorf("the simulation did not offer itself the job it queued (%s); it would generate "+
			"traffic and then never run it", mine)
	}
	if got[theirs] {
		t.Errorf("the simulation offered itself job %s, which an operator submitted through the "+
			"API. The real scheduler and jobrunner run in this same process and will also take "+
			"it; whichever finishes its own model of the work first releases the lease, and the "+
			"other's steps are abandoned mid-flight in farm.job_steps.", theirs)
	}
}

// TestTheFeedersBacklogGateCountsOnlyItsOwnJobs guards the other half.
//
// Falsify: drop `AND created_by = $2` from feederBacklog; the count includes
// the submitted job and the test fails with 2 instead of 1.
func TestTheFeedersBacklogGateCountsOnlyItsOwnJobs(t *testing.T) {
	pool := requireDB(t)
	seedJobFixture(t, pool)

	before, err := (&Runner{pool: pool}).feederBacklog(t.Context())
	if err != nil {
		t.Fatalf("feederBacklog: %v", err)
	}
	insertQueuedJob(t, pool, "alice@example.com")

	after, err := (&Runner{pool: pool}).feederBacklog(t.Context())
	if err != nil {
		t.Fatalf("feederBacklog: %v", err)
	}
	if after != before {
		t.Errorf("a job submitted through the API moved the feeder's backlog from %d to %d. "+
			"Five of those stop the simulation generating traffic, and nothing on screen says why.",
			before, after)
	}
}

// seedJobFixture makes the tenant, pool and queue farm.jobs points at.
func seedJobFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO farm.tenants (id, name) VALUES ($1,$1)
	                   ON CONFLICT DO NOTHING`, DefaultTenant)
	mustExec(t, pool, `INSERT INTO farm.pools (id) VALUES ($1) ON CONFLICT DO NOTHING`, DefaultPool)
	mustExec(t, pool, `INSERT INTO farm.queues (id, tenant_id) VALUES ($1,$2)
	                   ON CONFLICT DO NOTHING`, DefaultQueue, DefaultTenant)
}

// insertQueuedJob files one queued job under createdBy and returns its id.
func insertQueuedJob(t *testing.T, pool *pgxpool.Pool, createdBy string) string {
	t.Helper()
	const spec = `{"version":1,"steps":[{"id":"a","kind":"sleep","sleep":{"duration":"100s"},"timeout":"150s"}]}`
	var id string
	err := pool.QueryRow(t.Context(), `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, spec, expected_duration, max_runtime,
                       ttl, grace, disruption_policy, created_by)
VALUES ($1,$2,$3,$4::jsonb, interval '2 minutes', interval '5 minutes',
        interval '10 minutes', interval '5 minutes', 'no_disruption', $5)
RETURNING id::text`, DefaultTenant, DefaultQueue, DefaultPool, spec, createdBy).Scan(&id)
	if err != nil {
		t.Fatalf("insert job created_by=%s: %v", createdBy, err)
	}
	return id
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %s: %v", sql, err)
	}
}
