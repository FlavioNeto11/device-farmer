package scrcpy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func packageSources(t *testing.T) map[string]*ast.File {
	t.Helper()

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	out := make(map[string]*ast.File)
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatal("no package sources found; this test would pass vacuously")
	}
	return out
}

// TestPackageIsOnlyAProtocol.
//
// This package is pure encode and decode. It has no socket, no clock, no
// database and no idea what a lease is, and the reason to pin that with a test
// rather than a paragraph is that the pressure to break it will be real and
// will arrive as a convenience: a helper that dials the device, a retry loop
// with a timer in it, a check that the lease is still held before a tap.
//
// Every one of those belongs to a caller. What this package buys by refusing
// them is that its whole surface is testable as golden bytes, with no fake
// transport and no DATABASE_URL — which is why the length rule in video.go can
// be proved rather than argued.
//
// admission_test.go does import internal/fenceproxy, deliberately, and this
// test does not look at _test.go files for exactly that reason: the coupling
// between the spawn command and the proxy's whitelist is an assertion, not a
// dependency.
func TestPackageIsOnlyAProtocol(t *testing.T) {
	banned := map[string]string{
		"net":              "a protocol package that can dial is a protocol package with a timeout policy",
		"net/http":         "nothing here serves or requests anything",
		"os":               "the jar arrives as bytes; opening a file is the caller's business",
		"database/sql":     "this package has never heard of a device row",
		"time":             "a clock here would be a retry or a deadline, and both belong to the caller",
		"context":          "cancellation belongs to whoever owns the socket",
		"log/slog":         "an encoder that logs has an opinion about how loud its caller should be",
		"internal/api":     "the api imports this, not the other way round",
		"internal/adbwire": "the transport carries these bytes; it must not be reachable from them",
		"internal/lease":   "a tap is not a lease decision",
		"internal/fenceproxy": "the coupling to the proxy's whitelist is an assertion in " +
			"admission_test.go, not a production dependency",
		"github.com/jackc":   "no database driver",
		"github.com/pressly": "no migrations",
	}

	for name, f := range packageSources(t) {
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for bad, why := range banned {
				// The module-qualified forms too: an internal package is
				// imported as github.com/…/internal/fenceproxy, so a bare
				// equality check would let every one of those through.
				if path == bad || strings.HasPrefix(path, bad+"/") ||
					strings.HasSuffix(path, "/"+bad) || strings.Contains(path, "/"+bad+"/") {
					t.Errorf("%s imports %s: %s", name, path, why)
				}
			}
		}
	}
}

// TestNoTodosOrPlaceholders.
//
// A protocol implementation with a hole in it is worse than one that is absent:
// the caller writes against it, the hole ships, and the failure surfaces as a
// phone doing something nobody asked for. If a message is not encoded here, it
// is not mentioned here either.
func TestNoTodosOrPlaceholders(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, group := range f.Comments {
			for _, c := range group.List {
				for _, marker := range []string{"TODO", "FIXME", "XXX", "not implemented"} {
					if strings.Contains(c.Text, marker) {
						t.Errorf("%s carries a %s: %s", name, marker, strings.TrimSpace(c.Text))
					}
				}
			}
		}
	}
}
