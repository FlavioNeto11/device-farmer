package runner

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain — the same arrangement as
// internal/scheduler and internal/reaper, for the same two reasons:
//
//   - Without DATABASE_URL every SQL-backed test SKIPS. `go test ./...` has to
//     be green on a laptop with no Postgres, or the suite stops being run at
//     all. Everything else in this package still executes everywhere.
//
//   - The database is created fresh per run and dropped afterwards. The
//     runner's rows are keyed by job, and each test seeds its own job, so no
//     reset between tests is needed: two tests never see each other's steps.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver. goose speaks database/sql, so
	// the migration step needs it; the tests themselves use pgxpool.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/migrations"
)

// testPool is the pool for the scratch database, or nil when DATABASE_URL was
// unset and every SQL-backed test must skip.
var testPool *pgxpool.Pool

// setupLockKey serialises scratch-database creation across packages: the
// migrations create cluster-wide roles behind IF NOT EXISTS, and two suites
// migrating at once can both see a role missing. The same key every suite
// uses, taken on the admin database they all share.
const setupLockKey int64 = 0x64665f74657374 // "df_test"

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
		fmt.Fprintf(os.Stderr, "runner tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "runner tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := fmt.Sprintf("df_runner_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "runner tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "runner tests: create scratch database (the role needs CREATEDB): %v\n", err)
		return 1
	}
	defer func() {
		if testPool != nil {
			closed := make(chan struct{})
			go func() { testPool.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(15 * time.Second):
				fmt.Fprintln(os.Stderr, "runner tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "runner tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "runner tests: setup lock was not given back: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "runner tests: %v\n", migrateErr)
		return 1
	}

	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner tests: parse scratch DSN: %v\n", err)
		return 1
	}
	pc.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner tests: connect to scratch database: %v\n", err)
		return 1
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "runner tests: ping scratch database: %v\n", err)
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

// requireDB returns the scratch pool or skips. Every SQL-backed test starts
// with this line.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed runner tests")
	}
	return testPool
}

// jobFixture is the least a farm.job_steps row can hang off: a tenant, a
// queue, a pool and one running job. Ids carry a per-test sequence so tests
// in the same run never share a row.
type jobFixture struct {
	tenantID, queueID, poolID string
	jobID                     string
}

var fixtureSeq atomic.Int64

func newJobFixture(t *testing.T, pool *pgxpool.Pool) *jobFixture {
	t.Helper()
	ctx := t.Context()
	tag := fmt.Sprintf("%04d", fixtureSeq.Add(1))
	f := &jobFixture{tenantID: "tenant-" + tag, queueID: "queue-" + tag, poolID: "pool-" + tag}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding fixture %s failed: %v\nstatement: %s", tag, err, q)
		}
	}
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, f.tenantID)
	exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, f.queueID, f.tenantID)
	if err := pool.QueryRow(ctx,
		`INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, attempt, max_attempts, started_at, spec)
		 VALUES ($1, $2, $3, 'running', 1, 3, now(), '{"steps":[]}'::jsonb)
		 RETURNING id::text`,
		f.tenantID, f.queueID, f.poolID).Scan(&f.jobID); err != nil {
		t.Fatalf("seeding job for fixture %s: %v", tag, err)
	}
	return f
}
