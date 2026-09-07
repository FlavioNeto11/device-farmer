package api

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain, on the same terms as
// internal/watchdog's and internal/reaper's.
//
// This package was the last one that did not, and the difference was not
// academic. Every SQL-backed test here used to open a pool straight onto
// DATABASE_URL and write its fixtures into whatever database that named. Point
// DATABASE_URL at a running farm — which is the obvious thing to do while
// developing against one — run `go test ./...`, and the farm acquires rows it
// was never told about:
//
//   - powerFixture seeds a host per subtest, ids like pwr-host-<pid>_<n>
//     (ops_test.go), each with a rack, a controller, a power domain, a hub and
//     two slots.
//   - newScopeFixture and newStepFixture seed hosts u6h<sfx> and u9h<sfx> and,
//     with them, farm.pools rows (tenant_scope_db_test.go, jobsteps_test.go).
//
// The fixtures do delete their own rows, and TestSlotPowerFixtureLeavesNothing
// proves it, but "deleted afterwards" is not the same as "never there". A live
// `farmd watchdog` in that farm sees the host rows while they exist and starts
// a host reader for each of them, against hardware that does not exist. And
// test/assertions.sql, which inserts its own pool and expects to be the only
// writer, then fails with
//
//	duplicate key value violates unique constraint "pools_pkey"
//
// because a test run got there first. Neither failure points at this package;
// both are somebody spending an afternoon on a farm that appears to have grown
// phantom hosts. A database this suite created and drops cannot do either.
//
// Two properties of the arrangement are load bearing:
//
//   - Without DATABASE_URL every SQL-backed test SKIPS, through requireDB
//     below. The `go` CI job runs the whole suite with DATABASE_URL unset on
//     purpose — see .github/workflows/ci.yml — because a suite that needs
//     Postgres to be green on a laptop is a suite that stops being run. The
//     wire-level and pure tests in this package still execute everywhere.
//
//   - A DATABASE_URL that is set and unusable is a FAILURE, never a skip.
//     Somebody asked for these to run; a suite that quietly tests nothing
//     while reporting success is the worst outcome available here.
//
// The scratch database is created fresh per run and dropped afterwards, FORCE
// included, so a leaked connection cannot leak a database.

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

// testDSN is the scratch database's own DSN, empty alongside a nil testPool.
// It exists for the one caller that cannot share testPool: oneSessionPool in
// capabilities_test.go needs a pool pinned to a single connection so that a
// TEMP table it creates is visible to the query that reads it back.
var testDSN string

// setupLockKey serialises scratch-database creation across packages.
//
// migrations/00002_lease.sql creates CLUSTER-WIDE roles behind an
// "IF NOT EXISTS" check. `go test ./...` runs packages concurrently, so two
// suites migrating at the same instant can both see a role missing and both
// try to create it, and one of them gets duplicate_object. The lock is taken
// on the ADMIN database — the one every suite connects to in order to issue
// CREATE DATABASE — because advisory locks are scoped to a database and that
// is the only database the suites share. Same key as the other suites, on
// purpose: a key private to this package would serialise nothing.
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

	admin, err := sql.Open("pgx", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	// One connection, because the advisory lock below is session-scoped and a
	// second pooled connection would not hold it.
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "api tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := fmt.Sprintf("df_api_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "api tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "api tests: create scratch database (the role needs CREATEDB): %v\n", err)
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
					"api tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		// FORCE because a test that leaked a connection must not leak a
		// database too. The context above may already be spent, so use a fresh
		// one: a teardown that cannot run is the leak it exists to prevent.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "api tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "api tests: release setup lock: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "api tests: %v\n", migrateErr)
		return 1
	}

	pool, err := openTestPool(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api tests: %v\n", err)
		return 1
	}
	testPool, testDSN = pool, dsn

	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set, the same bytes the shipped
// binary carries. Pointing the tests at the .sql files on disk instead would
// let them pass against a schema no deployment ever gets — and most of what
// this package asserts is what the SQL functions and views of those migrations
// return, so that distinction is the value of the suite.
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
	// Comfortably more than any one case needs. A single test here can have a
	// fixture, one or more full Servers built by New, and an in-process
	// farmd-node agent all issuing queries at once, and a route handler that
	// has opened a transaction holds its connection until it commits. A pool
	// that runs dry does not fail loudly: the next Acquire blocks until the
	// request context expires, and the case reports a timeout that reads like
	// a bug in the route it was testing. The default is max(4, NumCPU), which
	// on a small CI runner is four.
	pc.MaxConns = 16
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

// requireDB hands back the scratch pool, or skips the calling test when there
// is none. Every SQL-backed test in this package goes through here rather than
// reading DATABASE_URL itself: that variable names the database the suite was
// allowed to CREATE DATABASE from, and is never the database a test writes to.
//
// The pool is SHARED and outlives the caller, so no test may close it — not
// through t.Cleanup(pool.Close) either, which is what each of these helpers
// used to do when the pool was its own. A closed shared pool does not fail
// where it was closed: it fails in whichever case runs next, as a pile of
// "closed pool" errors from a test that did nothing wrong. TestMain closes it
// once, on the way to dropping the database.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed api tests")
	}
	return testPool
}
