// Package migrations carries the device-farmer schema into the binary.
//
// The .sql files in this directory are the authoritative definition of the
// system: farm.leases and the fence live in 00001_core.sql, the lease
// functions and the Postgres role firewall in 00002_lease.sql, and the
// recovery ladder, quarantines and operator audit in 00003_ops.sql. They are
// shared with psql, with CI, and with test/assertions.sql, so they stay on
// disk at the repository root rather than being copied under cmd/.
//
// This file is the bridge. An //go:embed pattern may not contain ".." and may
// not leave the package directory, so cmd/farmd cannot embed a directory that
// sits above it; a package declared *inside* migrations/ can, and its import
// path — github.com/flaviopadilha/device-farmer/migrations — makes the SQL
// available to any role that needs it. cmd/farmd wires the result into its
// migrationsFS variable, at which point "farmd migrate up" stops warning that
// it discovered its migrations on disk.
//
// # On-disk migrations are still supported
//
// Nothing here removes the filesystem path. "farmd migrate -dir ..." and
// FARM_MIGRATIONS_DIR take precedence over the embedded set precisely so a
// developer can edit a .sql file and re-run without rebuilding, and so an
// operator can apply a hotfix migration that is not in the shipped image.
// Production images use the embedded set: it cannot drift from the binary
// that reads it, and it cannot be left out of a COPY line.
//
// # Why this package refuses to be empty
//
// An empty migration set is the worst failure this code has, because it is
// the quiet one. goose asked for zero migrations applies zero migrations,
// reports success, and exits 0. The init container goes green, the API starts
// against a database with no farm schema, and the first symptom is a query
// error minutes later in a role nobody was watching. A missing image layer
// must look like a broken build, not like a clean deploy.
//
// The guard therefore has two halves. The compiler supplies the first: a
// //go:embed pattern that matches no files is a build error, so a binary in
// which this package compiled at all contains at least one .sql file. The
// init-time check below supplies the second, since "at least one file" is not
// the same as "the schema" — see mustEmbedded.
package migrations

import (
	"cmp"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// FS holds every .sql file in this directory, rooted at the directory itself,
// so FS.Open("00001_core.sql") works and goose's "." directory argument
// resolves to the migration set.
//
// It is exported as the concrete embed.FS rather than only through Goose()
// because cmd/farmd assigns it directly to an fs.FS variable, and because a
// caller that wants ReadFile without a type assertion should have it.
//
// The pattern is a wildcard, which means embed applies its own exclusion
// rule: files whose names begin with "." or "_" are skipped. A migration
// named _00004_thing.sql would be applied by psql and by "migrate -dir",
// and would be invisible to every production binary. Numeric-prefixed goose
// names never start with those characters, so the rule costs nothing as long
// as nobody works around a merge conflict by renaming a file.
//
//go:embed *.sql
var FS embed.FS

// First is the migration that creates schema farm. It is the sentinel for
// "this directory is the device-farmer migration set": a tree containing any
// other single .sql file is somebody else's directory, and a set that has
// lost this file cannot produce a working database no matter what else it
// contains.
const First = "00001_core.sql"

// embedded is the sorted list of embedded migration filenames, validated once
// at package initialisation. Package-level initialisation order is derived
// from the reference graph, and mustEmbedded reads FS, so the embedded data
// is in place before the check runs.
var embedded = mustEmbedded()

// Goose returns the migration set as an fs.FS, ready for goose.SetBaseFS.
// Paired with goose's "." directory argument it applies exactly the files
// compiled into this binary.
//
// The accessor exists so callers name their intent rather than reaching for
// the variable, and so this package can later serve the files from somewhere
// other than a bare embed.FS — a subdirectory, say — without touching them.
func Goose() fs.FS { return FS }

// Names returns the embedded migration filenames in goose's application
// order. It is diagnostic surface: "farmd migrate status" talks to a database,
// while this answers "what does this binary actually contain?" for a support
// dump or a build-provenance check, with no connection and no config.
//
// The returned slice is a copy; the validated set is not the caller's to
// reorder.
func Names() []string { return slices.Clone(embedded) }

// mustEmbedded validates the embedded set and returns it in application
// order, panicking on anything that would make a migration run lie.
//
// It panics rather than returning an error because every condition it detects
// is a defect in the build of the binary that is already running. There is no
// runtime remedy and no caller decision to make: the SQL cannot be fetched,
// re-embedded, or repaired. Failing in init means a broken image dies on its
// first process start with a message naming the cause, instead of reaching
// goose and reporting a successful migration to an empty schema. An error
// return would leave that outcome one ignored value away.
func mustEmbedded() []string {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		panic("device-farmer migrations: cannot read the embedded set: " + err.Error())
	}

	// version travels with the name because goose orders by the parsed number,
	// never by the filename; see the sort below.
	type migration struct {
		version int64
		name    string
	}
	found := make([]migration, 0, len(entries))
	byVersion := make(map[int64]string, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := gooseVersion(name)
		if err != nil {
			// goose would fail on this file too, but only after connecting to
			// Postgres and taking the advisory lock. Naming it here points at
			// the file rather than at a stack inside a dependency.
			panic(fmt.Sprintf("device-farmer migrations: embedded file %q is not a goose migration: %v", name, err))
		}
		// Two files sharing a version number have no defined order between
		// them, so which one the schema ends up reflecting depends on the
		// filesystem. That is a coin flip over the shape of farm.leases.
		if prev, dup := byVersion[v]; dup {
			panic(fmt.Sprintf("device-farmer migrations: %q and %q share version %d; migration order would be undefined", prev, name, v))
		}
		byVersion[v] = name
		found = append(found, migration{version: v, name: name})
	}

	if len(found) == 0 {
		// Unreachable from a normal build — //go:embed *.sql fails to compile
		// when nothing matches — but reachable if the pattern is ever widened
		// or the files are moved, which is exactly when the failure needs to
		// be loud rather than clever.
		panic("device-farmer migrations: the embedded set is empty; a build that migrates nothing would report success")
	}
	if !slices.ContainsFunc(found, func(m migration) bool { return m.name == First }) {
		panic(fmt.Sprintf("device-farmer migrations: embedded set has %d file(s) but not %s, so it does not create schema farm", len(found), First))
	}

	// Order by the parsed version, because that is what goose does
	// (sort.Sort over Migrations, comparing Version) and Names() claims to
	// report goose's application order. Sorting the filenames instead only
	// agrees while every prefix is the same width: mix this repo's 00001_ with
	// a goose-generated timestamp, or let a merge produce 9_ and 10_, and
	// lexical order puts "10_" before "9_" while goose applies 9 first. That
	// is a diagnostic surface disagreeing with the migrator about the order
	// the schema was built in, in the support dump written to explain a
	// migration that went wrong.
	slices.SortFunc(found, func(a, b migration) int { return cmp.Compare(a.version, b.version) })

	names := make([]string, len(found))
	for i, m := range found {
		names[i] = m.name
	}
	return names
}

// gooseVersion extracts the numeric prefix goose orders migrations by:
// "00002_lease.sql" is version 2. Version 0 is rejected because goose reserves
// it for the initial, unmigrated database.
func gooseVersion(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("no %q separating version from description", "_")
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("version prefix %q is not a number", prefix)
	}
	if v <= 0 {
		return 0, fmt.Errorf("version %d is not positive; goose reserves 0 for an unmigrated database", v)
	}
	return v, nil
}
