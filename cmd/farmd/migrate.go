package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	// Registers the "pgx" database/sql driver. goose speaks database/sql, so
	// this is the one place in the binary that opens a non-pool connection.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// migrationsFS is the source of migration SQL, rooted at the directory that
// directly contains the *.sql files.
//
// It is a package-level variable rather than a //go:embed declaration in this
// file because an embed pattern may not contain ".." and may not leave the
// package directory, while the migrations are a repository-root artifact
// shared with psql, CI and the schema tests. A companion file gives farmd a
// self-contained binary without touching anything here:
//
//	// migrations/embed.go
//	package migrations
//
//	import "embed"
//
//	//go:embed *.sql
//	var FS embed.FS
//
//	// cmd/farmd/embed.go
//	package main
//
//	import "github.com/flaviopadilha/device-farmer/migrations"
//
//	func init() { migrationsFS = migrations.FS }
//
// Until then resolveMigrations falls back to the working tree and says so,
// loudly, because "the container image forgot to COPY migrations/" must not
// look like "there is nothing to migrate".
var migrationsFS fs.FS

// firstMigration is the sentinel used to recognise a migrations directory.
const firstMigration = "00001_core.sql"

// migrationLockKey guards concurrent migrators. Two farmd migrate Jobs racing
// in the same cluster would otherwise both try to CREATE SCHEMA farm and one
// would fail the deploy for no real reason.
const migrationLockKey int64 = 0x6466_6d69_6772_0001 // "df" + "migr" + version

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("farmd migrate", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var (
		dsn     = fset.String("dsn", "", "Postgres DSN; overrides "+config.EnvDatabaseURL)
		dir     = fset.String("dir", "", "directory holding the .sql migrations; overrides "+config.EnvMigrationsDir)
		table   = fset.String("table", "", "goose version table; overrides "+config.EnvMigrationsTable)
		timeout = fset.Duration("timeout", 10*time.Minute, "overall deadline for the migration run")
		lockFor = fset.Duration("lock-timeout", 2*time.Minute, "how long to wait for the migration advisory lock")
		confirm = fset.Bool("yes", false, "confirm a destructive down-to")
	)
	fset.Usage = func() {
		fmt.Fprint(stderr, migrateUsage)
		fmt.Fprintln(stderr, "\nFlags:")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // asking for help is not a failure
		}
		return errUsage
	}

	rest := fset.Args()
	if len(rest) == 0 {
		fset.Usage()
		return errUsage
	}
	action := rest[0]
	rest = rest[1:]

	// Reject an unknown action before loading config or dialling Postgres, so
	// a typo reports the typo rather than a connection timeout.
	switch action {
	case "up", "down-to", "status":
	default:
		fmt.Fprintf(stderr, "farmd migrate: unknown action %q\n\n", action)
		fset.Usage()
		return errUsage
	}

	// The stdlib flag package stops at the first non-flag argument, so flags
	// written after the action ("migrate down-to 1 -yes") are still sitting in
	// rest. Consume the action's own operand, then re-parse the remainder.
	var target int64
	if action == "down-to" {
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "farmd migrate down-to: missing target version")
			return errUsage
		}
		v, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			// A mistyped operand is a usage error, not a runtime failure. It
			// is the shape "farmd migrate down-to -yes 1" takes, and exit 2
			// tells the operator to re-read the line they typed.
			fmt.Fprintf(stderr, "farmd migrate down-to: %q is not a migration version\n\n", rest[0])
			fset.Usage()
			return errUsage
		}
		target = v
		rest = rest[1:]
	}
	if len(rest) > 0 {
		if err := fset.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return errUsage
		}
		if fset.NArg() > 0 {
			fmt.Fprintf(stderr, "farmd migrate: unexpected argument %q\n", fset.Arg(0))
			return errUsage
		}
	}

	// Non-positive deadlines make context.WithTimeout return an
	// already-expired context, so every migration would fail with a
	// deadline-exceeded error that names neither the flag nor the cause.
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "farmd migrate: -timeout must be positive")
		return errUsage
	}
	if *lockFor < 0 {
		fmt.Fprintln(stderr, "farmd migrate: -lock-timeout must not be negative")
		return errUsage
	}

	// WithoutDatabase, because -dsn is allowed to be the only source of a DSN.
	// The emptiness check below is the one that matters here.
	cfg, err := config.Load("migrate", config.WithoutDatabase())
	if err != nil {
		return err
	}
	if *dsn != "" {
		cfg.DatabaseURL = *dsn
	}
	if *dir != "" {
		cfg.MigrationsDir = *dir
	}
	if *table != "" {
		cfg.MigrationsTable = *table
	}
	if cfg.DatabaseURL == "" {
		return fmt.Errorf("no database: set %s or pass -dsn", config.EnvDatabaseURL)
	}
	// Re-validate: a DSN that arrived via -dsn has not been through Validate.
	if err := cfg.Validate(); err != nil {
		return err
	}

	src, origin, err := resolveMigrations(cfg.MigrationsDir)
	if err != nil {
		return err
	}
	// Warn only when the directory was GUESSED. An operator who passed -dir or
	// set FARM_MIGRATIONS_DIR already knows the binary is not self-contained,
	// and a warning they cannot act on trains them to ignore the ones they can.
	if migrationsFS == nil && cfg.MigrationsDir == "" {
		fmt.Fprintf(stderr,
			"warning: migrations were discovered on disk (%s) rather than embedded.\n"+
				"         This binary is not self-contained; make sure the image ships them.\n", origin)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	goose.SetBaseFS(src)
	goose.SetLogger(gooseLogger{out: stdout})
	goose.SetTableName(cfg.MigrationsTable)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.RedactedDatabaseURL(), err)
	}
	defer db.Close()
	// One connection for the advisory lock, one for goose itself.
	db.SetMaxOpenConns(2)

	pingCtx, pingCancel := context.WithTimeout(ctx, cfg.DBConnectTimeout)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("connect to %s: %w", cfg.RedactedDatabaseURL(), err)
	}

	// status is read-only and must stay usable while another migrator holds
	// the lock; that is exactly when an operator wants to look.
	if action != "status" {
		unlock, err := lockMigrations(ctx, db, *lockFor)
		if err != nil {
			return err
		}
		defer unlock()
	}

	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	switch action {
	case "up":
		if err := goose.UpContext(ctx, db, "."); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
	case "down-to":
		// Down migrations here drop farm.leases. A lease row is the record
		// that a device is spoken for; losing it silently is the failure this
		// project exists to avoid, so the operator has to say so out loud.
		if target < current && !*confirm {
			return fmt.Errorf(
				"refusing to migrate down from version %d to %d without -yes.\n"+
					"Rolling this schema back destroys farm.leases, and with it the record of\n"+
					"which jobs currently own which devices. Live jobs keep running on the\n"+
					"phones; nothing in the control plane will know. Re-run with -yes if that\n"+
					"is genuinely what you want.", current, target)
		}
		if err := goose.DownToContext(ctx, db, ".", target); err != nil {
			return fmt.Errorf("migrate down-to %d: %w", target, err)
		}
	case "status":
		if err := goose.StatusContext(ctx, db, "."); err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
	default:
		// Unreachable: action was validated before any I/O happened.
		return fmt.Errorf("internal: unhandled migrate action %q", action)
	}

	final, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	fmt.Fprintf(stdout, "schema version: %d (was %d), source %s\n", final, current, origin)
	return nil
}

// lockMigrations takes a session-scoped advisory lock. The lock lives on one
// dedicated *sql.Conn because pg_advisory_lock is per-session, not per-
// transaction, and a pooled handle would unlock on a connection we no longer
// own.
func lockMigrations(ctx context.Context, db *sql.DB, wait time.Duration) (func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration lock: %w", err)
	}

	deadline := time.Now().Add(wait)
	for {
		var got bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&got); err != nil {
			conn.Close()
			return nil, fmt.Errorf("migration lock: %w", err)
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			conn.Close()
			return nil, fmt.Errorf(
				"another migrator has held the schema lock for longer than %s; "+
					"if no other farmd migrate is running, check for a stuck session in pg_locks", wait)
		}
		select {
		case <-ctx.Done():
			conn.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	return func() {
		// Best effort: a lost unlock is released when the session ends anyway.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
		conn.Close()
	}, nil
}

// resolveMigrations returns an fs.FS rooted at the migration files plus a
// human-readable description of where they came from.
func resolveMigrations(explicitDir string) (fs.FS, string, error) {
	if explicitDir != "" {
		if err := hasMigrations(os.DirFS(explicitDir)); err != nil {
			return nil, "", fmt.Errorf("migrations directory %s: %w", explicitDir, err)
		}
		return os.DirFS(explicitDir), explicitDir, nil
	}
	if migrationsFS != nil {
		if err := hasMigrations(migrationsFS); err != nil {
			return nil, "", fmt.Errorf("embedded migrations: %w", err)
		}
		return migrationsFS, "embedded", nil
	}
	for _, dir := range candidateDirs() {
		if hasMigrations(os.DirFS(dir)) == nil {
			return os.DirFS(dir), dir, nil
		}
	}
	return nil, "", fmt.Errorf(
		"no migrations found: set %s or pass -dir. Looked for %s under the working "+
			"directory, its parents, and the directory holding this binary",
		config.EnvMigrationsDir, firstMigration)
}

// candidateDirs lists the places a migrations directory plausibly sits: next
// to the binary in a container, and up the tree from a developer's shell.
func candidateDirs() []string {
	var roots []string
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}

	var out []string
	for _, root := range roots {
		cur := root
		for i := 0; i < 4; i++ {
			out = append(out, filepath.Join(cur, "migrations"))
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}
	return out
}

func hasMigrations(fsys fs.FS) error {
	f, err := fsys.Open(firstMigration)
	if err != nil {
		return fmt.Errorf("does not contain %s: %w", firstMigration, err)
	}
	return f.Close()
}

// gooseLogger adapts goose's logger to our streams. goose's contract is that
// Fatalf terminates, so it does.
//
// goose is written against log.Logger, which supplies a trailing newline when
// the format lacks one. fmt.Fprintf does not, so the newline is added here or
// goose's own un-terminated lines run into ours.
type gooseLogger struct{ out io.Writer }

func (l gooseLogger) Printf(format string, v ...any) {
	fmt.Fprint(l.out, terminate(fmt.Sprintf(format, v...)))
}

func (l gooseLogger) Fatalf(format string, v ...any) {
	fmt.Fprint(os.Stderr, terminate("goose: "+fmt.Sprintf(format, v...)))
	os.Exit(exitFailure)
}

func terminate(s string) string {
	if s == "" || s[len(s)-1] == '\n' {
		return s
	}
	return s + "\n"
}

var _ goose.Logger = gooseLogger{}

// errUsage is returned for anything the user typed wrong; main turns it into
// the usage exit code without printing a second, redundant message.
var errUsage = errors.New("usage")

const migrateUsage = `Usage: farmd migrate <up | down-to VERSION | status> [flags]

  up               apply every migration that has not been applied
  down-to VERSION  roll back to VERSION (requires -yes when it moves backwards)
  status           list migrations and whether they are applied

A session-scoped advisory lock serialises concurrent migrators, so it is safe
to run this as a Kubernetes Job with more than one replica or a retrying
init container.
`
