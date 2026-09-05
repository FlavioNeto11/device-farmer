package reaper

// Structural invariants. These need no database, so they run on every machine
// and in every CI job, including the ones with no Postgres.
//
// They exist because the behavioural tests can only prove what the code does
// TODAY. A health check added to this loop tomorrow would be caught by
// TestDeviceHealthCannotEndALiveLease only if the author also happened to
// reproduce that exact scenario; the tests below fail on the edit itself.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// packageSources parses every non-test file of this package WITHOUT comments.
//
// Comments are excluded deliberately: the package doc names farm.device_runtime
// several times in order to forbid it, and a scan that could not tell a
// prohibition from a use would either fail on correct code or be relaxed until
// it caught nothing.
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

// stringLiterals returns every string literal in the file, unquoted where
// possible. SQL in this package is written as raw string literals.
func stringLiterals(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, lit.Value)
		}
		return true
	})
	return out
}

// TestReaperNamesDeviceRuntimeNowhere is the firewall expressed as a lint.
//
// farm.lease_reclaim cannot read device health because it runs as a role
// without the privilege. This loop has no such protection — it runs as whatever
// the deployment configured — so the rule for the Go side is simply that the
// table's name does not appear in it. A query that cannot be written cannot be
// added to the release path by accident.
func TestReaperNamesDeviceRuntimeNowhere(t *testing.T) {
	for name, f := range packageSources(t) {
		for _, lit := range stringLiterals(f) {
			if strings.Contains(lit, "device_runtime") {
				t.Errorf("%s: a string literal names farm.device_runtime:\n\t%s\n\n"+
					"Device health may not be an input to the release path. A broken device "+
					"is internal/recovery's problem; a broken device that is LEASED stays "+
					"with its holder while recovery acts on that holder's behalf.", name, lit)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && strings.Contains(strings.ToLower(id.Name), "deviceruntime") {
				t.Errorf("%s: identifier %q reintroduces device health into the reaper", name, id.Name)
			}
			return true
		})
	}
}

// leaseWriteRE matches SQL that would modify farm.leases outside the lease
// functions. \s+ rather than a single space so reformatted SQL is still caught.
var leaseWriteRE = regexp.MustCompile(`(?is)(insert\s+into|update|delete\s+from)\s+farm\.leases`)

// TestReaperWritesLeasesOnlyThroughTheLeaseFunctions.
//
// The row locks, the partial unique indexes, the guard trigger and the role
// switch that make an ending safe all live inside farm.lease_*. A direct UPDATE
// from here would bypass every one of them — including the trigger that refuses
// to move a deadline backwards, which is what protects a control-plane-gap
// refund from being silently erased.
func TestReaperWritesLeasesOnlyThroughTheLeaseFunctions(t *testing.T) {
	for name, f := range packageSources(t) {
		for _, lit := range stringLiterals(f) {
			if leaseWriteRE.MatchString(lit) {
				t.Errorf("%s: SQL writes farm.leases directly:\n\t%s\n\n"+
					"Every ending must go through farm.lease_release, farm.lease_reclaim or "+
					"farm.lease_expire_max_runtime.", name, lit)
			}
		}
	}
}

// TestReaperComponentIsOnTheRenewalPath.
//
// farm.reaper_arm computes the control-plane gap as the OLDEST heartbeat across
// the components it is given. A component that beats but is not in that set
// contributes nothing; a component in the set that never beats makes the arm
// refuse. The reaper's own name has to be in the default set, or a reaper
// outage — the one that most obviously must be refunded — is the one outage
// the refund cannot see.
func TestReaperComponentIsOnTheRenewalPath(t *testing.T) {
	if !slices.Contains(lease.ReaperComponents, DefaultComponent) {
		t.Errorf("lease.ReaperComponents = %v does not contain %q; a reaper outage would not "+
			"be refunded to any tenant", lease.ReaperComponents, DefaultComponent)
	}
}

// TestDefaultComponentsExcludeTheHealthPlane.
//
// The watchdog and the recovery ladder beat too, and their heartbeats are for
// operators to see a stalled loop. Putting them in the gap set would let a
// HEALTH-plane outage move lease deadlines, which is the very fusion of clocks
// this system exists to prevent — the shape of STF #663, arriving through the
// refund instead of through a socket.
func TestDefaultComponentsExcludeTheHealthPlane(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()
	for _, forbidden := range []string{"watchdog", "recovery"} {
		if slices.Contains(cfg.Components, forbidden) {
			t.Errorf("default gap components %v include %q: a health-plane outage would move "+
				"lease deadlines", cfg.Components, forbidden)
		}
	}
	if len(cfg.Components) == 0 {
		t.Error("default gap components are empty; farm.reaper_arm would compute no gap and " +
			"every outage would be charged to tenants")
	}
}

// TestApplyDefaultsNeverLeavesASilentlyInertLoop.
//
// A zero Batch reaches the SQL as LIMIT 0 and sweeps nothing while reporting
// success, and a zero Rearm makes a slot schedulable the instant its lease is
// reclaimed — before the previous holder's sockets are severed. Neither failure
// announces itself, so the defaults are the only thing standing in the way.
func TestApplyDefaultsNeverLeavesASilentlyInertLoop(t *testing.T) {
	cfg := Config{Batch: -1, Rearm: -1, Interval: -1, CallTimeout: -1, GapFloor: -1, CensusEvery: -1}
	cfg.applyDefaults()

	if cfg.Batch <= 0 {
		t.Errorf("Batch = %d; a non-positive batch reaches the SQL as LIMIT 0 and sweeps nothing", cfg.Batch)
	}
	if cfg.Rearm < lease.DefaultRearm {
		t.Errorf("Rearm = %s, want at least %s: it must exceed the node proxy's self-fence timeout",
			cfg.Rearm, lease.DefaultRearm)
	}
	if cfg.Interval <= 0 || cfg.CallTimeout <= 0 || cfg.GapFloor <= 0 || cfg.CensusEvery <= 0 {
		t.Errorf("a non-positive duration survived applyDefaults: %+v", cfg)
	}
	if cfg.LockKey == 0 {
		t.Error("LockKey = 0; every replica would elect itself")
	}
}

// TestJitterOnlyShortensTheInterval. Jitter exists to stop N replicas waking in
// lockstep. Lengthening the interval instead would silently slow the sweep, and
// a negative result would busy-loop.
func TestJitterOnlyShortensTheInterval(t *testing.T) {
	const base = 10_000
	for i := 0; i < 1000; i++ {
		got := jitter(base)
		if got > base {
			t.Fatalf("jitter(%d) = %d, longer than the interval", base, got)
		}
		if got <= 0 {
			t.Fatalf("jitter(%d) = %d, which would busy-loop", base, got)
		}
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %d, want 0", got)
	}
}

// TestPackageDocStillCarriesTheOrderingRule is a small guard on the one comment
// that explains why arm precedes sweep. It is cheap, and the alternative — a
// future reader reordering two lines that look interchangeable — is not.
func TestPackageDocStillCarriesTheOrderingRule(t *testing.T) {
	src, err := os.ReadFile("reaper.go")
	if err != nil {
		t.Fatalf("read reaper.go: %v", err)
	}
	if !strings.Contains(string(src), "ARM BEFORE SWEEPING") {
		t.Error("the ARM BEFORE SWEEPING note is gone from reaper.go; it is the only thing " +
			"telling the next reader that those two calls are not interchangeable")
	}
}
