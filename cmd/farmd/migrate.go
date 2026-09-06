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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// against the same database would otherwise both try to CREATE SCHEMA farm and
// one would fail the deploy for no real reason.
//
// Against the SAME DATABASE, and no wider: pg_advisory_lock is scoped to the
// database the session is connected to, and this command is only ever given
// one DSN. It therefore cannot serialise the cluster-wide DDL in the migration
// set — the NOLOGIN roles of 00002_lease.sql and 00008_parked.sql, which every
// database in the cluster shares — because a migrator working on another
// database takes its lock there and the two never meet. Those statements are
// made collision-proof where they are issued instead; 00002_lease.sql explains
// the idiom, and it protects goose called from a test harness or a psql run
// too, neither of which comes through this function.
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

	// Before goose is asked anything at all: every action below reads the
	// version first, and reading it CREATES the version table when it is
	// missing — goose.GetDBVersion falls back to createVersionTable — so the
	// schema half of FARM_MIGRATIONS_TABLE has to exist by now. On a fresh
	// database it does not: migration 00001 is what creates farm, and it has
	// not run yet. That is what made FARM_MIGRATIONS_TABLE=farm.schema_version
	// work on an established farm and refuse on a new one, which is backwards
	// — the moment an operator picks the ledger's name is the moment they
	// create the database.
	//
	// status included, deliberately. It is read-only about the FARM, which is
	// what the lock comment above means, but it has never been read-only about
	// the ledger: goose creates the version table underneath it just the same.
	// Refusing to make room for that table on the one action that would still
	// go on to create it would mean status failing where up succeeds, with two
	// different answers to "where is this database's ledger".
	//
	// It is worth knowing what that costs, because status holds no lock. A
	// status whose ledger schema is missing, run at the same moment as an `up`
	// on the same database, can create schema farm inside the microseconds
	// between that migrator's CREATE SCHEMA IF NOT EXISTS probing the catalog
	// and inserting into it — IF NOT EXISTS is not atomic — and abort its
	// 00001. The migration is transactional and the migrator is a Job that
	// retries, so the cost is a retry; taking the lock here instead would cost
	// status its whole reason for existing, which is to answer while a
	// migration is in flight.
	if err := prepareMigrationLedger(ctx, db, cfg.MigrationsTable, stdout); err != nil {
		return err
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
		// A ledger inside the farm schema cannot survive its own rollback, and
		// -yes must not be read as consent to that: 00001's Down is
		// `DROP SCHEMA IF EXISTS farm CASCADE`, which takes the version table
		// with it, and goose then deletes its version row inside the same
		// transaction and gets 42P01. That migration alone rolls back, leaving
		// the database at version 1 with 00002 upward already undone — a
		// half-applied schema, which is the state 00001's own header exists to
		// prevent. Refused here, before anything is dropped, because there is
		// nothing useful to say once the run is halfway through.
		//
		// Only when a rollback would actually happen (target < current, which
		// implies the ledger already exists), so a `down-to 0` against a
		// database that has nothing to roll back stays the no-op it always
		// was, and no refusal is ever reported by a run that created something
		// on its way to it.
		if target < 1 && target < current {
			schema, err := ledgerSchema(ctx, db, cfg.MigrationsTable)
			if err != nil {
				return err
			}
			if schema == farmSchema {
				return fmt.Errorf(
					"refusing to migrate down to %d while the ledger lives in schema %s "+
						"(%s=%q).\n00001's Down drops schema %s CASCADE, which destroys the "+
						"version table itself, so goose cannot record the rollback it just "+
						"performed: the run stops at version 1 with everything above it "+
						"already undone.\nThere is no rollback past 00001 for a ledger kept "+
						"in this schema. Drop the database if the whole farm is meant to go. "+
						"A partial rollback keeps the ledger, but note that 00015's Down "+
						"revokes role memberships that are CLUSTER-wide: it stops every other "+
						"farm on this server from assuming its runtime roles.",
					target, farmSchema, config.EnvMigrationsTable, cfg.MigrationsTable,
					farmSchema)
			}
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

// farmSchema is the schema migration 00001 creates and its Down drops.
// farmSentinel is the first table 00001 creates inside it, and therefore the
// first thing a replay of 00001 would collide with.
//
// Two decisions turn on them — whether a database has already been migrated,
// and whether rolling back would destroy the ledger — and both are about where
// the ledger lives rather than about anything in the farm, which is why the
// names are spelled out here rather than derived from the migration files.
// TestFarmSchemaMatchesTheMigrations holds them against 00001's own statements,
// because the two guards below degrade to silence rather than to an error if
// they ever stop matching.
const (
	farmSchema   = "farm"
	farmSentinel = "farm.racks"
)

// prepareMigrationLedger makes sure the table named by FARM_MIGRATIONS_TABLE
// can be created where it was asked for, creating its schema when that is the
// only thing missing and refusing when creating it would start a SECOND
// ledger on a database that already has one.
//
// Creating the schema, rather than refusing with "create it first, here is the
// statement", is a choice between two defensible answers. The case against is
// that this is a write nobody asked for. The case that wins is that they did
// ask, in the only place the request can be made: this command's entire job is
// to take a database from nothing to a farm schema, four cluster roles and
// several dozen tables, and the single object it was refusing to create was
// the ledger that records it did so. CREATE SCHEMA is also strictly smaller
// than the next statement goose runs, and migration 00001 issues that very
// statement for farm moments later.
//
// The refusal survives where it is the better answer, and what it asks about
// is the LEDGER, not its schema. goose treats a database with no ledger as a
// database at version 0 and replays 00001 into it, so pointing this variable
// at a table that does not exist yet, on a database that has already been
// migrated, ends in 42P07 and leaves a second ledger behind. Whether the new
// ledger's schema happens to exist changes nothing about that — naming
// farm.schema_version on a farm migrated under the default ledger is the same
// mistake as naming ops.schema_version — which is why the schema half is not
// the question here.
//
// "Already migrated" is asked as "does farm.racks exist", not as "does schema
// farm hold anything", and the difference is a database an operator can no
// longer migrate at all. `migrate status` creates the ledger where it was
// pointed, so a status run against a fresh database with a qualified name
// leaves schema farm holding exactly one table — the ledger — with nothing
// applied. A guard that counted that as a farm would then refuse every value
// of the variable, including the default, on a database that has never run a
// migration. An empty farm schema is not a farm, and neither is one holding
// only a ledger; the sentinel is a table only 00001 creates.
//
// What the choice still costs is a typo on a database that IS fresh:
// FARM_MIGRATIONS_TABLE=farmm.schema_version leaves an empty farmm schema
// behind and puts the ledger in it, and nothing downstream contradicts that,
// because from goose's point of view a fresh ledger is a database at version
// 0. That is why a created schema is announced: one line naming a schema the
// operator did not mean to name is the cheapest place to catch it, and the
// only place before the migrations start running.
//
// The existence checks are not an optimisation. CREATE SCHEMA checks CREATE on
// the database before it checks whether the schema is already there, so
// issuing it unconditionally would make every ordinary migration of an
// established farm demand a privilege it has never needed. Reading the
// catalogs demands none.
func prepareMigrationLedger(ctx context.Context, db *sql.DB, table string, stdout io.Writer) error {
	quoted, schema, err := quoteMigrationsTable(table)
	if err != nil {
		return err
	}

	// One round trip for all three facts. to_regclass resolves a name the way
	// goose will — through search_path when it is unqualified — and returns
	// NULL rather than raising when any part of it is missing.
	var ledgerExists, migrated, schemaExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT to_regclass($1) IS NOT NULL,
		       to_regclass($2) IS NOT NULL,
		       EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $3)`,
		quoted, farmSentinel, schema).Scan(&ledgerExists, &migrated, &schemaExists); err != nil {
		return fmt.Errorf("looking for the migration ledger %s, named by %s (%q): %w",
			quoted, config.EnvMigrationsTable, table, err)
	}
	if ledgerExists {
		return nil
	}
	if migrated {
		return fmt.Errorf(
			"%s (%q) names a migration ledger this database does not have, and the database "+
				"has already been migrated: %s exists.\nStarting a ledger now would call a "+
				"migrated database version 0 and replay 00001 into tables that exist, which "+
				"fails on the first CREATE TABLE and leaves two ledgers behind.\nPoint %s at "+
				"the ledger this database already carries — %q unless it was moved — or look "+
				"for a typo in the one you set.",
			config.EnvMigrationsTable, table, farmSentinel,
			config.EnvMigrationsTable, config.DefaultMigrationsTable)
	}
	if schema == "" || schemaExists {
		// Nothing to create: either the name is unqualified, and goose will
		// resolve it through search_path exactly as the query above did, or
		// the schema is already there and only the table is missing — which
		// on a database with no farm in it is an ordinary first migration.
		return nil
	}

	// Quoted and concatenated, because a schema name is an identifier and not
	// a value. MigrationsTableParts has already refused every spelling
	// Postgres would fold differently, so Sanitize only adds the quotes.
	//
	// Plain CREATE SCHEMA rather than IF NOT EXISTS, so that reaching the
	// print below means this process is the one that created it. IF NOT
	// EXISTS would swallow a lost race silently and the line would claim a
	// creation that happened somewhere else, which is the one claim the typo
	// check above rests on.
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		// Losing a race for this schema produced the schema we wanted, which
		// is not a failure. Both codes are load-bearing, and the second is the
		// one a real race raises: 42P06 duplicate_schema once the other
		// session has committed, 23505 from pg_namespace's unique index while
		// it has not and this statement blocked on its uncommitted row. Both
		// are reachable from `status`, which deliberately takes no lock.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P06" || pgErr.Code == "23505") {
			return nil
		}
		return fmt.Errorf("creating schema %q, which %s (%q) names as the home of the "+
			"migration ledger: %w\nIf this user is not meant to hold CREATE on the "+
			"database, create the schema once by hand and re-run: CREATE SCHEMA %s;",
			schema, config.EnvMigrationsTable, table, err, quotedSchema)
	}
	fmt.Fprintf(stdout, "created schema %s, which %s names as the home of the migration ledger\n",
		schema, config.EnvMigrationsTable)
	return nil
}

// ledgerSchema reports the schema the migration ledger ACTUALLY lives in, or
// "" when the table does not exist.
//
// Asked of Postgres rather than parsed out of FARM_MIGRATIONS_TABLE, because
// the two can differ and the difference is invisible in the variable: an
// unqualified name is resolved through search_path, and a DSN carrying
// `options=-csearch_path%3Dfarm` — or a login role with that search_path set —
// puts a ledger called goose_db_version inside the farm schema. A guard that
// read the variable would not see that, and it is the case where the guard
// matters most.
func ledgerSchema(ctx context.Context, db *sql.DB, table string) (string, error) {
	quoted, _, err := quoteMigrationsTable(table)
	if err != nil {
		return "", err
	}
	var schema string
	err = db.QueryRowContext(ctx, `
		SELECT n.nspname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = to_regclass($1)`, quoted).Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("locating the migration ledger %s, named by %s (%q): %w",
			quoted, config.EnvMigrationsTable, table, err)
	}
	return schema, nil
}

// quoteMigrationsTable renders FARM_MIGRATIONS_TABLE as a quoted SQL
// identifier and hands back its schema half, "" for an unqualified name.
//
// The rule that matters — how the name splits, and which spellings are
// refused because goose folds them and a reader quotes them — lives in
// config.MigrationsTableParts and is shared with internal/api, which quotes
// the same value to read the applied version back. Only the two lines of
// quoting are restated here, and they are restated rather than exported
// because the api's copy also carries a Config it may have been handed in
// code; a shared helper would have to take both callers' fallbacks with it.
//
// Both cfg.MigrationsTable and -table have been through MigrationsTableParts
// already inside cfg.Validate, so the error is defensive. It is still
// returned rather than ignored: this is the function whose output is
// concatenated into DDL, and a name nobody validated is exactly what must
// never reach it.
func quoteMigrationsTable(table string) (quoted, schema string, err error) {
	schema, name, err := config.MigrationsTableParts(table)
	if err != nil {
		return "", "", fmt.Errorf("%s (%q) %w", config.EnvMigrationsTable, table, err)
	}
	if schema == "" {
		return pgx.Identifier{name}.Sanitize(), "", nil
	}
	return pgx.Identifier{schema, name}.Sanitize(), schema, nil
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

A session-scoped advisory lock serialises concurrent migrators of this
database, so it is safe to run this as a Kubernetes Job with more than one
replica or a retrying init container.

When FARM_MIGRATIONS_TABLE names a schema, that schema is created if it is
missing: goose writes its version table before the first migration runs, and
on a fresh database nothing else has created it yet. On a database that is
already a farm, a ledger that does not exist is refused instead — starting one
there would replay the whole set into tables that already exist.
`
