package node

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain — the same arrangement as
// internal/reaper, for the same two reasons: without DATABASE_URL every
// SQL-backed test SKIPS so `go test ./...` stays green on a laptop with no
// Postgres, and a set-but-broken DATABASE_URL fails loudly rather than
// quietly testing nothing.

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
	// Registers the "pgx" database/sql driver for goose; the tests themselves
	// use pgxpool.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/migrations"
)

// testPool is the scratch database's pool, or nil when DATABASE_URL was unset
// and every SQL-backed test must skip.
var testPool *pgxpool.Pool

// setupLockKey is the key internal/reaper and internal/scheduler take while
// creating and migrating their scratch databases. The migration set creates
// cluster-wide roles behind IF NOT EXISTS, and `go test ./...` runs packages
// concurrently, so sharing the key is what keeps two suites from racing to
// create the same role.
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
		fmt.Fprintf(os.Stderr, "node tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	// One connection: the advisory lock below is session-scoped.
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "node tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := fmt.Sprintf("df_node_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	dsn, err := dsnForDatabase(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node tests: %v\n", err)
		return 1
	}

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "node tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		_, _ = admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "node tests: create scratch database (the role needs CREATEDB): %v\n", err)
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
					"node tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "node tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "node tests: release setup lock: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "node tests: %v\n", migrateErr)
		return 1
	}

	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node tests: parse scratch DSN: %v\n", err)
		return 1
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node tests: connect to scratch database: %v\n", err)
		return 1
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "node tests: ping scratch database: %v\n", err)
		return 1
	}
	testPool = pool

	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set, the bytes the shipped
// binary carries, so the schema under test is the schema a deployment gets.
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
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed node tests")
	}
	return testPool
}
