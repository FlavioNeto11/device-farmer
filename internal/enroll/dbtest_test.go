package enroll

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain.
//
// Two properties of that arrangement are load-bearing:
//
//   - Without DATABASE_URL every SQL-backed test SKIPS. `go test ./...` has to
//     be green on a laptop with no Postgres, or the suite stops being run at
//     all. The pure-Go tests and the ones against the fake ADB server still
//     execute everywhere.
//
//   - The database is created fresh per run and dropped afterwards, and the
//     schema is emptied between tests. farm.resolve_device matches on brand,
//     fingerprint and serial across the WHOLE table — that is its job — so a
//     phone left behind by one test would be "recognised" by the next.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
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

// setupLockKey serialises scratch-database creation across packages.
//
// migrations/00002_lease.sql creates CLUSTER-WIDE roles behind an "IF NOT
// EXISTS" check. `go test ./...` runs packages concurrently, so two suites
// migrating at the same instant can both see a role missing and both try to
// create it. The lock is taken on the ADMIN database, the only one every suite
// shares, and the key is the same one the other packages' harnesses use.
const setupLockKey int64 = 0x64665f74657374 // "df_test"

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite owns the scratch database's whole lifetime. It returns an exit code
// rather than calling os.Exit itself so that its deferred teardown runs.
func runSuite(m *testing.M) (code int) {
	base := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if base == "" {
		return m.Run()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A misconfigured DATABASE_URL is a failure, never a skip: somebody who
	// exported it asked for these tests to run.
	admin, err := sql.Open("pgx", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	// One connection, because the advisory lock below is session-scoped.
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := scratchName("df_enroll_test")
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "enroll tests: create scratch database (the role needs CREATEDB): %v\n", err)
		return 1
	}
	defer func() {
		if testPool != nil {
			closed := make(chan struct{})
			go func() { testPool.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(15 * time.Second):
				fmt.Fprintln(os.Stderr,
					"enroll tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		// FORCE because a test that leaked a connection must not leak a
		// database too. The context may already be spent, so use a fresh one.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "enroll tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: release setup lock: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: %v\n", migrateErr)
		return 1
	}

	pool, err := openTestPool(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "enroll tests: %v\n", err)
		return 1
	}
	testPool = pool

	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set, the same bytes the shipped
// binary carries, so the schema under test is the one a deployment gets.
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
	// Probes run DefaultConcurrency at a time and each records its sighting
	// as it finishes; the pool must not be what serialises them.
	pc.MaxConns = 8
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

// requireDB returns the scratch pool or skips. Every SQL-backed test starts
// with this line.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed enroll tests")
	}
	return testPool
}

// resetSchema empties every table the tests write to. The reference tables
// seeded by the migrations are schema, not test data, and are left alone.
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
}
