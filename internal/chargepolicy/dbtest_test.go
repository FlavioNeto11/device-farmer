package chargepolicy

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain — the same arrangement as
// internal/reaper, for the same two reasons:
//
//   - Without DATABASE_URL every SQL-backed test SKIPS, so `go test ./...` is
//     green on a laptop with no Postgres and the decision table in
//     policy_test.go still runs everywhere.
//
//   - The scan is GLOBAL — the policy reads every non-retired device in the
//     farm — so one test's leftovers are the next test's candidates. Every
//     fixture starts by emptying the schema, and no test that runs a cycle
//     calls t.Parallel.

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
	// Registers the "pgx" database/sql driver for goose.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/migrations"
)

// testPool is the pool for the scratch database, or nil when DATABASE_URL was
// unset and every SQL-backed test must skip.
var testPool *pgxpool.Pool

// fixtureSeq namespaces each fixture's rows and its advisory lock key.
var fixtureSeq atomic.Int64

// setupLockKey serialises scratch-database creation across packages: the
// migrations create cluster-wide roles behind IF NOT EXISTS checks, and two
// suites migrating at once can both see a role missing. Same key as every
// other suite, on the admin database they all share.
const setupLockKey int64 = 0x64665f74657374 // "df_test"

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) (code int) {
	base := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if base == "" {
		return m.Run()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := fmt.Sprintf("df_chargepolicy_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "chargepolicy tests: create scratch database (the role needs CREATEDB): %v\n", err)
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
					"chargepolicy tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "chargepolicy tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: release setup lock: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: %v\n", migrateErr)
		return 1
	}

	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: parse scratch DSN: %v\n", err)
		return 1
	}
	// Leader election pins one connection per Policy and a test runs two
	// of them at once, so the pool must comfortably exceed that.
	pc.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chargepolicy tests: connect to scratch database: %v\n", err)
		return 1
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "chargepolicy tests: ping scratch database: %v\n", err)
		return 1
	}
	testPool = pool

	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set — the same bytes the
// shipped binary carries — so the schema under test is the one a deployment
// gets.
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

// requireDB returns the scratch pool or skips.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed charge policy tests")
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
