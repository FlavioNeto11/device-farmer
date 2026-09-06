package main

import (
	"database/sql"
	"io"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/migrations"
)

// TestPrepareMigrationLedgerRefusesANameNoReaderCouldSpell covers what
// prepareMigrationLedger decides before it reaches Postgres, on the machine
// that has no Postgres: a laptop, and the `go` job in CI, which runs the whole
// suite with DATABASE_URL unset precisely so paths like this keep being run.
//
// The names below are the ones config.Validate refuses at boot. They are
// re-checked here because this is the function whose output is concatenated
// into DDL: quoting is what makes that safe, and quoting only makes it safe
// for a name the same split has already accepted.
//
// The handle points at a port nothing listens on. sql.Open does not dial, so
// a function that decides without asking the database returns as it should;
// one that reaches for a connection gets a refused dial and a test failure
// naming it — rather than the nil-pointer panic a nil *sql.DB would produce,
// which in a parallel test takes the whole package's results with it.
func TestPrepareMigrationLedgerRefusesANameNoReaderCouldSpell(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("pgx", "postgres://farm@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	for _, bad := range []string{"a.b.c", "farm.", ".schema_version", "Farm.schema_version", ""} {
		err := prepareMigrationLedger(t.Context(), db, bad, io.Discard)
		if err == nil {
			t.Errorf("prepareMigrationLedger(%q) = nil, want an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "FARM_MIGRATIONS_TABLE") {
			t.Errorf("prepareMigrationLedger(%q) = %v; the error must name the variable "+
				"an operator would have to change", bad, err)
		}
		if strings.Contains(err.Error(), "connect") || strings.Contains(err.Error(), "dial") {
			t.Errorf("prepareMigrationLedger(%q) dialled Postgres before refusing the name: %v",
				bad, err)
		}
	}
}

// TestQuoteMigrationsTableSplitsBeforeItQuotes pins the pairing the DDL below
// it depends on: a two-part name has to be quoted as TWO identifiers, because
// quoting it as one names a table nobody has — pgx.Identifier{"farm.x"} is
// "farm.x", a single identifier that happens to contain a dot.
func TestQuoteMigrationsTableSplitsBeforeItQuotes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in, quoted, schema string
	}{
		{"public.goose_db_version", `"public"."goose_db_version"`, "public"},
		{"farm.schema_version", `"farm"."schema_version"`, "farm"},
		{"goose_db_version", `"goose_db_version"`, ""},
	} {
		quoted, schema, err := quoteMigrationsTable(tc.in)
		if err != nil || quoted != tc.quoted || schema != tc.schema {
			t.Errorf("quoteMigrationsTable(%q) = (%q, %q, %v), want (%q, %q, nil)",
				tc.in, quoted, schema, err, tc.quoted, tc.schema)
		}
	}
}

// TestFarmSchemaMatchesTheMigrations holds the three names migrate.go spells
// out in Go against the migration that creates and drops what they name.
//
// Both ledger guards compare against these constants — the refusal to start a
// second ledger on a migrated database, and the refusal to roll back a ledger
// the rollback would destroy — and both fail OPEN if a name ever stops
// matching: the comparison goes false and the guard silently permits the thing
// it exists to stop. A rename in 00001 is the plausible way that happens, so
// the two files are tied together here rather than left to agree by habit.
func TestFarmSchemaMatchesTheMigrations(t *testing.T) {
	t.Parallel()

	// resolveMigrations looks for this name on disk while the embedded set is
	// keyed by the package's own constant. They are the same file and nothing
	// else says so.
	if firstMigration != migrations.First {
		t.Errorf("firstMigration = %q but migrations.First = %q; the on-disk fallback and "+
			"the embedded set would be looking for different files", firstMigration, migrations.First)
	}

	core, err := migrations.Goose().Open(migrations.First)
	if err != nil {
		t.Fatalf("opening %s out of the embedded set: %v", migrations.First, err)
	}
	defer core.Close()
	body, err := io.ReadAll(core)
	if err != nil {
		t.Fatalf("reading %s: %v", migrations.First, err)
	}

	create := "CREATE SCHEMA IF NOT EXISTS " + farmSchema + ";"
	if !strings.Contains(string(body), create) {
		t.Errorf("%s does not contain %q, so the farmSchema constant (%q) no longer names "+
			"the schema the migrations create; the ledger guards in migrate.go compare "+
			"against it and go quiet when it is wrong", migrations.First, create, farmSchema)
	}
	drop := "DROP SCHEMA IF EXISTS " + farmSchema + " CASCADE;"
	if !strings.Contains(string(body), drop) {
		t.Errorf("%s does not contain %q; the down-to guard refuses a rollback because that "+
			"statement destroys the ledger, and it is no longer there to destroy it",
			migrations.First, drop)
	}
	// The sentinel answers "has this database been migrated", so it has to be
	// a table 00001 creates and nothing else does. If it stops being created
	// there, the second-ledger guard stops firing and says nothing.
	sentinel := "CREATE TABLE " + farmSentinel + " ("
	if !strings.Contains(string(body), sentinel) {
		t.Errorf("%s does not contain %q, so farmSentinel (%q) is no longer a table this "+
			"migration creates and the second-ledger guard would never fire",
			migrations.First, sentinel, farmSentinel)
	}
}
