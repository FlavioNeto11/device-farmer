package e2e

// The migration ledger's own home.
//
// FARM_MIGRATIONS_TABLE names goose's version table, and the capability probe
// in internal/api reads the applied schema version out of it. Pointing it
// inside the farm schema — the obvious value, and the one that keeps a
// deployment's bookkeeping in one namespace — used to work against an
// established farm and refuse against a new one: goose writes its version
// table BEFORE it runs migration 00001, and 00001 is what creates the schema,
// so `migrate up` died with SQLSTATE 3F000 on exactly the databases where an
// operator picks the name.
//
// This scenario is the fresh half, the half that was broken. It runs the
// SHIPPED binary rather than goose in this process, because the fix is in the
// binary and a test that called goose directly would prove nothing about what
// an init container does.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/migrations"
)

func TestMigrateIntoAQualifiedLedgerOnAFreshDatabase(t *testing.T) {
	if adminDSN == "" {
		t.Skip("DATABASE_URL is not set; skipping the end-to-end scenarios")
	}
	ctx := t.Context()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connecting to %s to create a scratch database: %v", config.EnvDatabaseURL, err)
	}
	t.Cleanup(func() {
		// WithoutCancel: t.Context() is already cancelled by the time cleanup
		// runs, and a scratch database that outlives its test is one the next
		// run has to sweep.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = admin.Close(dctx)
	})

	name := scratchName(t)
	// The same key every other suite takes, on the database they all share:
	// creating and migrating a scratch database touches CLUSTER-wide roles,
	// and this scenario migrates from empty like the rest of them.
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		t.Fatalf("taking the shared setup lock: %v", err)
	}
	// Registered before the first thing that can Fatal while it is held, and
	// idempotent so the happy path can still give it back early. Every suite
	// in this repository queues behind this one key, so a t.Fatalf between
	// here and the explicit release would not fail one test — it would stall
	// every other package's TestMain until this process exited.
	var unlockOnce sync.Once
	unlock := func() {
		unlockOnce.Do(func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if _, err := admin.Exec(uctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
				t.Errorf("the shared setup lock was not given back: %v", err)
			}
		})
	}
	t.Cleanup(unlock)

	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		t.Fatalf("creating the scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(dctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping the scratch database %s: %v", name, err)
		}
	})

	dsn := dsnFor(t, adminDSN, name)
	env := append(cleanEnv(), config.EnvMigrationsTable+"=farm.schema_version")

	// status first, and on the fresh database on purpose. It is the action an
	// operator reaches for before they migrate anything, it fails in exactly
	// the same place — goose reads the version, and reading it creates the
	// version table — and it is the one action that does its half of this
	// without the advisory lock.
	stat, statErr, statCode, err := runBinary(t, 3*time.Minute, env, "migrate", "status", "-dsn", dsn)
	if err != nil {
		t.Fatalf("farmd migrate status: %v\n%s", err, strings.TrimSpace(statErr))
	}
	if statCode != 0 {
		t.Fatalf("farmd migrate status exited %d with %s=farm.schema_version against a fresh "+
			"database; this was SQLSTATE 3F000 before the migrator made room for the ledger:"+
			"\n%s\n%s", statCode, config.EnvMigrationsTable,
			strings.TrimSpace(stat), strings.TrimSpace(statErr))
	}
	// The announcement is the only thing standing between an operator and a
	// typo: a misspelled schema half creates an empty schema and starts a
	// second, parallel ledger in it, and every later run then agrees the
	// database is at version 0. If this line ever stops being printed, that
	// mistake becomes silent.
	if !strings.Contains(stat, "created schema farm") {
		t.Errorf("migrate status created the schema without saying so; stdout was:\n%s", stat)
	}

	out, errOut, code, err := runBinary(t, 3*time.Minute, env, "migrate", "up", "-dsn", dsn)
	unlock()
	if err != nil {
		t.Fatalf("farmd migrate up: %v\n%s", err, strings.TrimSpace(errOut))
	}
	if code != 0 {
		t.Fatalf("farmd migrate up exited %d with %s=farm.schema_version against a fresh "+
			"database; before the migrator created the schema this was SQLSTATE 3F000:\n%s\n%s",
			code, config.EnvMigrationsTable, strings.TrimSpace(out), strings.TrimSpace(errOut))
	}
	// And having been created once, it is not announced again. The line means
	// "this run created something"; printed on every run it would mean nothing.
	if strings.Contains(out, "created schema") {
		t.Errorf("migrate up announced a schema creation that status had already done:\n%s", out)
	}

	scratch, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to the migrated scratch database: %v", err)
	}
	defer scratch.Close(ctx)

	// COALESCE, because an empty ledger is one of the shapes this scenario is
	// hunting: max() over no rows is NULL, and a NULL scanned into an int64
	// would report a type error instead of the version floor's own message.
	var applied int64
	if err := scratch.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM farm.schema_version`).Scan(&applied); err != nil {
		t.Fatalf("reading the ledger out of farm.schema_version: %v", err)
	}
	// Compared against the set this binary carries, not against a number
	// written here. A floor of "20 or more" would stop asserting anything the
	// moment the set grew past it: a run that applied half the migrations
	// would still clear it.
	if want := topEmbeddedVersion(t); applied != want {
		t.Errorf("farm.schema_version reports version %d; the embedded set ends at %d, so "+
			"the ledger and the schema disagree about what was applied", applied, want)
	}
	// Nothing may have landed in the default location. A run that quietly
	// created public.goose_db_version as well would leave two ledgers, and the
	// capability probe and the migrator would each read a different one.
	var defaultLedger *string
	if err := scratch.QueryRow(ctx,
		`SELECT to_regclass('public.goose_db_version')::text`).Scan(&defaultLedger); err != nil {
		t.Fatalf("looking for the default ledger: %v", err)
	}
	if defaultLedger != nil {
		t.Errorf("public.goose_db_version exists as well as farm.schema_version; the ledger "+
			"is in two places and %s decides only one of them", config.EnvMigrationsTable)
	}

	// Re-running is a no-op that still says nothing about creating anything.
	out2, errOut2, code2, err := runBinary(t, 3*time.Minute, env, "migrate", "up", "-dsn", dsn)
	if err != nil || code2 != 0 {
		t.Fatalf("second farmd migrate up: err=%v exit=%d\n%s\n%s",
			err, code2, strings.TrimSpace(out2), strings.TrimSpace(errOut2))
	}
	if strings.Contains(out2, "created schema") {
		t.Errorf("the second migrate up announced a schema creation against a database that "+
			"already had one:\n%s", out2)
	}

	// The rollback that would destroy this ledger is refused before it starts.
	// 00001's Down drops schema farm CASCADE, which takes farm.schema_version
	// with it, and goose then cannot record the rollback it just performed:
	// the run stops at version 1 with everything above it already undone.
	down, downErr, downCode, err := runBinary(t, 3*time.Minute, env,
		"migrate", "down-to", "0", "-yes", "-dsn", dsn)
	if err != nil {
		t.Fatalf("farmd migrate down-to 0: %v", err)
	}
	if downCode == 0 {
		t.Errorf("migrate down-to 0 was allowed while the ledger lives in the schema that "+
			"rollback drops:\n%s", strings.TrimSpace(down))
	}
	if !strings.Contains(downErr, "version 1") {
		t.Errorf("the down-to refusal does not say what it prevents; stderr was:\n%s",
			strings.TrimSpace(downErr))
	}
	var stillApplied int64
	if err := scratch.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM farm.schema_version`).Scan(&stillApplied); err != nil {
		t.Fatalf("reading the ledger after the refused rollback: %v", err)
	}
	if stillApplied != applied {
		t.Errorf("the refused rollback moved the schema from version %d to %d; a refusal must "+
			"not roll anything back", applied, stillApplied)
	}

	// The other half of the bargain. Making the migrator create a missing
	// ledger schema is only safe while it refuses to do so on a database that
	// is already a farm: there, a missing ledger means goose would start
	// counting at 0 and replay 00001 into tables that exist, and the operator
	// would meet that as 42P07 on farm.racks three migrations later. A typo in
	// the variable is the common way to arrive here.
	typo := append(cleanEnv(), config.EnvMigrationsTable+"=ops.schema_version")
	out3, errOut3, code3, err := runBinary(t, 3*time.Minute, typo, "migrate", "up", "-dsn", dsn)
	if err != nil {
		t.Fatalf("farmd migrate up with a ledger schema that does not exist: %v", err)
	}
	if code3 == 0 {
		t.Errorf("migrate up accepted %s=ops.schema_version against an already-migrated "+
			"database; it should refuse rather than start a second ledger:\n%s",
			config.EnvMigrationsTable, strings.TrimSpace(out3))
	}
	if !strings.Contains(errOut3, "two ledgers") {
		t.Errorf("the refusal does not explain itself; stderr was:\n%s", strings.TrimSpace(errOut3))
	}
	var stray string
	switch err := scratch.QueryRow(ctx,
		`SELECT nspname FROM pg_namespace WHERE nspname = 'ops'`).Scan(&stray); {
	case errors.Is(err, pgx.ErrNoRows):
		// What a refusal means: nothing was created on the way to it.
	case err != nil:
		t.Fatalf("looking for a stray ops schema: %v", err)
	default:
		t.Errorf("the refused run left an empty %q schema behind", stray)
	}
}

// topEmbeddedVersion is the version the last migration in the shipped set
// carries, parsed the way goose parses it: the numeric prefix of the filename.
// migrations.Names() is already sorted into goose's application order.
func topEmbeddedVersion(t *testing.T) int64 {
	t.Helper()
	names := migrations.Names()
	if len(names) == 0 {
		t.Fatal("the embedded migration set is empty")
	}
	last := names[len(names)-1]
	prefix, _, ok := strings.Cut(last, "_")
	if !ok {
		t.Fatalf("embedded migration %q has no version prefix", last)
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		t.Fatalf("embedded migration %q: version prefix %q is not a number", last, prefix)
	}
	return v
}
