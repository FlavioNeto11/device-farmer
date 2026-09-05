package scheduler

// Structural and pure-logic invariants. No database, so these run everywhere,
// including on a machine with no Postgres.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// packageSources parses every non-test file of this package without comments,
// so a rule spelled out in prose is never mistaken for a violation of itself.
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

// leaseWriteRE matches SQL that would modify farm.leases outside the lease
// functions. \s+ rather than a single space so reformatted SQL is still caught.
var leaseWriteRE = regexp.MustCompile(`(?is)(insert\s+into|update|delete\s+from)\s+farm\.leases`)

// TestSchedulerWritesLeasesOnlyThroughTheLeaseFunctions.
//
// The partial unique index and the row locks that make a double grant
// impossible live INSIDE farm.lease_acquire. An INSERT written here would be
// outside the transaction that makes it correct, and the failure — two jobs on
// one phone — would not show up until two test suites started fighting over a
// device hours into a run.
func TestSchedulerWritesLeasesOnlyThroughTheLeaseFunctions(t *testing.T) {
	for name, f := range packageSources(t) {
		for _, lit := range stringLiterals(f) {
			if leaseWriteRE.MatchString(lit) {
				t.Errorf("%s: SQL writes farm.leases directly:\n\t%s\n\n"+
					"Every allocation goes through farm.lease_acquire and every ending "+
					"through farm.lease_release.", name, lit)
			}
		}
	}
}

// TestSchedulerImportsNoTransport.
//
// A socket error must have no parameter to travel through and therefore no path
// into lease state. internal/lease enforces this for itself; the scheduler is
// the other end of the same rule, because it is the loop that decides who holds
// what.
func TestSchedulerImportsNoTransport(t *testing.T) {
	for name, f := range packageSources(t) {
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "adbwire") || strings.Contains(path, "internal/node") {
				t.Errorf("%s imports %s: allocation must not be able to see a transport at all",
					name, path)
			}
		}
	}
}

// TestSchedulerComponentIsOnTheRenewalPath.
//
// The scheduler mints the leases whose deadlines the reaper enforces, so it is
// on the renewal path. A scheduler outage that is invisible to gap accounting
// is BLOCKER 8: the reaper's own heartbeat stays fresh, no outage is recorded,
// and after TTL+grace every unprotected lease in the farm is reclaimed.
func TestSchedulerComponentIsOnTheRenewalPath(t *testing.T) {
	if !slices.Contains(lease.ReaperComponents, DefaultComponent) {
		t.Errorf("lease.ReaperComponents = %v does not contain %q; a scheduler outage would "+
			"not be refunded to any tenant", lease.ReaperComponents, DefaultComponent)
	}
}

// TestApplyDefaultsProducesASaneLoop. None of these can end a lease, so the
// worst a bad value does is schedule slowly — except SlotRearm, which is passed
// to farm.lease_release on the unwind path and must outlast the node proxy's
// self-fence timeout or a new job lands on a device the old holder still holds
// a socket to.
func TestApplyDefaultsProducesASaneLoop(t *testing.T) {
	cfg := Config{Interval: 30 * time.Second} // IdleInterval left below Interval
	cfg.applyDefaults()

	if cfg.IdleInterval < cfg.Interval {
		t.Errorf("IdleInterval %s is shorter than Interval %s; backing off would poll faster "+
			"than not backing off", cfg.IdleInterval, cfg.Interval)
	}

	zero := Config{Batch: -1, JobBackoff: -1, JobBackoffMax: -1, SlotRearm: -1, CallTimeout: -1}
	zero.applyDefaults()
	if zero.Batch <= 0 {
		t.Errorf("Batch = %d; a non-positive LIMIT starves the whole queue", zero.Batch)
	}
	if zero.JobBackoffMax < zero.JobBackoff {
		t.Errorf("JobBackoffMax %s below JobBackoff %s", zero.JobBackoffMax, zero.JobBackoff)
	}
	if zero.SlotRearm < lease.DefaultRearm {
		t.Errorf("SlotRearm = %s, want at least %s: it must exceed the node proxy's self-fence "+
			"timeout", zero.SlotRearm, lease.DefaultRearm)
	}
	if zero.CallTimeout <= 0 {
		t.Error("CallTimeout = 0; an unbounded statement can wedge the cycle, and a wedged " +
			"scheduler stops beating — which is the control-plane gap the refund exists for")
	}
	if zero.LockKey == 0 {
		t.Error("LockKey = 0; every replica would elect itself")
	}
}

// TestWithinQuotaTreatsZeroAsUnlimited. 0 is the schema's "no cap" and is the
// default on both farm.tenants and farm.queues, so reading it as "zero devices
// allowed" would stop the entire farm the moment quotas were consulted.
func TestWithinQuotaTreatsZeroAsUnlimited(t *testing.T) {
	cases := []struct {
		name                  string
		tenantCap, queueCap   int64
		tenantHeld, queueHeld int64
		want                  bool
	}{
		{"no caps at all", 0, 0, 1000, 1000, true},
		{"tenant under its cap", 2, 0, 1, 0, true},
		{"tenant at its cap", 2, 0, 2, 0, false},
		{"tenant over its cap", 2, 0, 5, 0, false},
		{"queue at its cap", 0, 2, 0, 2, false},
		{"queue under, tenant at", 2, 5, 2, 0, false},
		{"both under", 5, 5, 4, 4, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate{
				tenantCap: tc.tenantCap, queueCap: tc.queueCap,
				tenantHeld: tc.tenantHeld, queueHeld: tc.queueHeld,
			}
			if got := c.withinQuota(); got != tc.want {
				t.Errorf("withinQuota() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeferralBackoffGrowsAndIsCapped. The backoff is what turns a full farm
// from a query storm into a poll; an uncapped one would turn a transient
// shortage into a job nobody retries for an hour.
func TestDeferralBackoffGrowsAndIsCapped(t *testing.T) {
	s := &Scheduler{
		cfg:      Config{JobBackoff: time.Second, JobBackoffMax: 8 * time.Second},
		deferred: map[string]*deferral{},
	}
	now := time.Now()

	var previous time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		s.defer_("job", now)
		wait := s.deferred["job"].until.Sub(now)

		if wait <= 0 {
			t.Fatalf("attempt %d: deferral of %s is already lapsed; the job would be retried "+
				"at full rate", attempt, wait)
		}
		if wait > s.cfg.JobBackoffMax {
			t.Fatalf("attempt %d: deferral of %s exceeds the cap of %s", attempt, wait, s.cfg.JobBackoffMax)
		}
		// Jitter only ever shortens, so growth is compared against the previous
		// floor rather than demanded exactly.
		if attempt > 1 && wait < previous*3/4 {
			t.Errorf("attempt %d: deferral shrank from %s to %s", attempt, previous, wait)
		}
		previous = wait
	}
}

// TestPruneDeferralsKeepsTheAttemptCountBriefly. An entry is kept for one
// further backoff window after it lapses, so a job that is polled again
// immediately keeps its attempt count instead of resetting to the floor on
// every cycle — which would defeat the backoff entirely.
func TestPruneDeferralsKeepsTheAttemptCountBriefly(t *testing.T) {
	s := &Scheduler{
		cfg:      Config{JobBackoff: time.Second, JobBackoffMax: time.Minute},
		deferred: map[string]*deferral{},
	}
	now := time.Now()

	s.deferred["just-lapsed"] = &deferral{until: now.Add(-time.Second), tries: 4}
	s.deferred["long-gone"] = &deferral{until: now.Add(-2 * time.Minute), tries: 4}
	s.deferred["in-force"] = &deferral{until: now.Add(time.Second), tries: 1}

	s.pruneDeferrals(now)

	if _, ok := s.deferred["long-gone"]; ok {
		t.Error("a deferral older than the whole backoff window was kept; the map is unbounded")
	}
	for _, id := range []string{"just-lapsed", "in-force"} {
		if _, ok := s.deferred[id]; !ok {
			t.Errorf("deferral %q was pruned too early; the job resets its backoff to the "+
				"floor on every cycle", id)
		}
	}
}

// TestJitterOnlyShortensTheInterval. Jitter spreads replica wake-ups. A value
// above the interval would silently slow the loop; a non-positive one would
// busy-loop against the database.
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

// TestSlotOfNamesTheAbsenceOfASlot. A lease need not be bound to a slot, and a
// log line reading "slot 0" would send a human to a physical position that
// belongs to a different phone.
func TestSlotOfNamesTheAbsenceOfASlot(t *testing.T) {
	if got := slotOf(lease.Lease{}); got != "none" {
		t.Errorf("slotOf(unbound) = %v, want %q", got, "none")
	}
	id := int64(7)
	if got := slotOf(lease.Lease{SlotID: &id}); got != id {
		t.Errorf("slotOf(slot 7) = %v, want 7", got)
	}
}
