package node

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// recorder captures what the agent logged and at which level. The level is the
// point: this package's contract is that the healthy path is SILENT above
// info, because a warning an operator sees on every healthy cycle is a warning
// they stop reading before the cycle that matters.
type recorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
	return nil
}

// New calls Logger.With, so both of these have to answer with something that
// still records. The attributes themselves are not under test.
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

// above returns every record logged at or above lvl.
func (r *recorder) above(lvl slog.Level) []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, rec := range r.records {
		if rec.Level >= lvl {
			out = append(out, rec)
		}
	}
	return out
}

// messages renders the records for a failure message.
func messages(recs []slog.Record) []string {
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.Level.String()+": "+rec.Message)
	}
	return out
}

// attr reads one attribute off a record, so a test can assert on the delay the
// agent actually scheduled rather than on the prose around it.
func attr(rec slog.Record, key string) (slog.Value, bool) {
	var (
		val   slog.Value
		found bool
	)
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val, found = a.Value, true
			return false
		}
		return true
	})
	return val, found
}

// waitAbove blocks until at least one record at or above lvl has been logged.
func (r *recorder) waitAbove(t *testing.T, lvl slog.Level, timeout time.Duration) []slog.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if recs := r.above(lvl); len(recs) > 0 {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing was logged at %s or above within %s", lvl, timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newTestAgent builds an agent whose loops can be driven directly.
//
// The pool is a zero *pgxpool.Pool: New only stores it, and every loop these
// tests exercise is one that never reaches the database, so a unit test of the
// agent's scheduling does not need Postgres running.
func newTestAgent(t *testing.T, cfg Config) (*Agent, *recorder) {
	t.Helper()
	rec := &recorder{}
	if cfg.Pool == nil {
		cfg.Pool = &pgxpool.Pool{}
	}
	cfg.Logger = slog.New(rec)

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return a, rec
}

// ---------------------------------------------------------------------------
// The heartbeat key is per host
// ---------------------------------------------------------------------------

// TestHeartbeatComponentNamesTheHost pins the fix for a farm that misreports
// itself: farm.component_heartbeat is keyed by component, so two agents that
// beat under one name write the SAME row. Each overwrite keeps the row fresh
// for the host that is dead, an operator reads one healthy agent while a rack
// sits dark, and any gap accounting pointed at that name measures nothing.
//
// A full two-host run cannot be staged here — farmd node needs a Linux host
// with real USB paths — so the assertion is made where the name is decided.
func TestHeartbeatComponentNamesTheHost(t *testing.T) {
	one, _ := newTestAgent(t, Config{HostID: "rack1-host"})
	two, _ := newTestAgent(t, Config{HostID: "rack2-host"})

	if one.cfg.Component == two.cfg.Component {
		t.Fatalf("two hosts share the heartbeat key %q; each agent's beat would hide "+
			"the other host's silence", one.cfg.Component)
	}
	if want := DefaultComponentPrefix + "rack1-host"; one.cfg.Component != want {
		t.Errorf("component = %q, want %q", one.cfg.Component, want)
	}
	if !strings.Contains(two.cfg.Component, "rack2-host") {
		t.Errorf("component %q does not name the host it beats for", two.cfg.Component)
	}
}

// TestNewRefusesAHeartbeatNameSharedBetweenHosts covers the way the bug
// actually arrived: not through the default, which was always per-host, but
// through a caller passing a constant. The refusal is at construction because
// the alternative is a farm that looks healthy while it is not, and nothing
// downstream can tell the difference.
func TestNewRefusesAHeartbeatNameSharedBetweenHosts(t *testing.T) {
	_, err := New(Config{Pool: &pgxpool.Pool{}, HostID: "rack1-host", Component: "node"})
	if err == nil {
		t.Fatal("New accepted the constant component \"node\"; two hosts wired this way " +
			"overwrite one heartbeat row and the farm cannot tell which agent is alive")
	}
	// The message has to carry the host, because the operator reading it is
	// looking at one host's logs and needs to know what to write instead.
	if !strings.Contains(err.Error(), "rack1-host") {
		t.Errorf("refusal does not name the host: %v", err)
	}

	// An explicitly spelled per-host name is still allowed: the check is about
	// distinguishing hosts, not about owning the naming scheme.
	if _, err := New(Config{
		Pool: &pgxpool.Pool{}, HostID: "rack1-host", Component: "node:rack1-host",
	}); err != nil {
		t.Errorf("New rejected a per-host component name: %v", err)
	}
}

// TestComponentNamesTheHostExactly covers the two ways a nearly-right name
// still collides. Host ids nest — "h01" and "h01-spare" are both plausible in
// one rack — so a name is only this host's when the id ends it after a
// separator. And a name is only what it looks like once it is trimmed: the
// value validated has to be the value stored, or the check and the heartbeat
// row disagree about what was written.
func TestComponentNamesTheHostExactly(t *testing.T) {
	for _, tc := range []struct {
		host, component string
		want            bool
	}{
		{"h01", "node:h01", true},
		{"h01", "h01", true},
		{"h01", "farmd-node/h01", true},
		// The nesting case: this name belongs to h01-spare, and honouring it
		// for h01 puts two hosts on one heartbeat row.
		{"h01", "node:h01-spare", false},
		{"h01", "node:h01x", false},
		// The reverse nesting, which a bare suffix test would also miss.
		{"1", "node:h1", false},
		{"h01", "node", false},
	} {
		if got := namesHost(tc.component, tc.host); got != tc.want {
			t.Errorf("namesHost(%q, %q) = %v, want %v", tc.component, tc.host, got, tc.want)
		}
	}

	// Whitespace must not be a way past the check, and must not survive into
	// the key either: a padded name is not the name the dashboard matches on.
	padded, _ := newTestAgent(t, Config{HostID: " rack1-host ", Component: " node:rack1-host "})
	if want := "node:rack1-host"; padded.cfg.Component != want {
		t.Errorf("component = %q, want %q — the stored key is not the validated one",
			padded.cfg.Component, want)
	}

	// A name that is only whitespace is no name at all, and must fall through
	// to the per-host default rather than becoming a key every host shares.
	blank, _ := newTestAgent(t, Config{HostID: "rack1-host", Component: "   "})
	if want := DefaultComponentPrefix + "rack1-host"; blank.cfg.Component != want {
		t.Errorf("component = %q, want the per-host default %q", blank.cfg.Component, want)
	}
}

// ---------------------------------------------------------------------------
// Enrollment scheduling
// ---------------------------------------------------------------------------

// TestEnrollLoopIsSilentWhileEnrollmentKeepsRunning is the healthy path:
// Config.Enroll runs until its context ends, the way enroll.Run does. The loop
// must call it once, never restart it, never escalate its backoff, and never
// log above info — including on the way out, where the enrollment function
// returns nil precisely because it was asked to stop.
func TestEnrollLoopIsSilentWhileEnrollmentKeepsRunning(t *testing.T) {
	var calls atomic.Int64
	entered := make(chan struct{})

	a, rec := newTestAgent(t, Config{
		HostID: "rack1-host",
		Enroll: func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-ctx.Done()
			// enroll.Run's contract: a cancelled context is a clean stop, not
			// a fault.
			return nil
		},
	})
	// The loop waits for the host row before it enrolls anything; nothing here
	// registers one.
	close(a.registered)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.enrollLoop(ctx) }()

	<-entered
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enrollLoop returned %v on a clean cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("enrollLoop did not return after its context was cancelled")
	}

	// One call, no restart. A second call would mean the loop treated a
	// continuous enrollment as something to supervise on a timer.
	if got := calls.Load(); got != 1 {
		t.Errorf("enrollment was entered %d times, want 1", got)
	}
	if recs := rec.above(slog.LevelWarn); len(recs) > 0 {
		t.Errorf("a healthy enrollment logged at warn or above: %v", messages(recs))
	}
}

// TestEnrollLoopDiagnosesAPassWiredIntoAContinuousSlot covers the wiring
// mistake this loop exists to survive: a function that performs ONE pass and
// returns (enroll.EnrollOnce) hits the fault path on every healthy cycle, so
// the farm enrolls on a decaying schedule while warning that something is
// wrong. The loop cannot repair the wiring, but the warning it prints must say
// what to change — an alert nobody can act on is an alert nobody reads.
func TestEnrollLoopDiagnosesAPassWiredIntoAContinuousSlot(t *testing.T) {
	a, rec := newTestAgent(t, Config{
		HostID: "rack1-host",
		// One pass, then a clean return, while the context is still live.
		Enroll: func(context.Context) error { return nil },
	})
	close(a.registered)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.enrollLoop(ctx) }()

	recs := rec.waitAbove(t, slog.LevelWarn, 10*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("enrollLoop did not return after its context was cancelled")
	}

	msg := recs[0].Message
	for _, want := range []string{"enroll.Run", "enroll.EnrollOnce"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the warning does not name %s, so it does not say what to fix: %q",
				want, msg)
		}
	}
	// The first restart is scheduled at the base delay. Escalation belongs to
	// repeated faults; a farm must not start out waiting the worst case.
	if v, ok := attr(recs[0], "in"); !ok {
		t.Error("the warning does not say when enrollment will be restarted")
	} else if got := v.Duration(); got != DefaultEnrollBackoff {
		t.Errorf("first restart scheduled in %s, want %s", got, DefaultEnrollBackoff)
	}
}

// TestEnrollLoopRestartsEnrollmentThatFailed guards the other direction: the
// fix for the mis-wiring must not turn the loop into one that shrugs at a
// genuine fault. An enrollment that returns an ERROR is still restarted, still
// reported, and still delayed rather than spun.
func TestEnrollLoopRestartsEnrollmentThatFailed(t *testing.T) {
	var calls atomic.Int64
	a, rec := newTestAgent(t, Config{
		HostID: "rack1-host",
		Enroll: func(context.Context) error {
			calls.Add(1)
			return context.DeadlineExceeded
		},
	})
	close(a.registered)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.enrollLoop(ctx) }()

	recs := rec.waitAbove(t, slog.LevelWarn, 10*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("enrollLoop did not return after its context was cancelled")
	}

	if got := calls.Load(); got < 1 {
		t.Fatalf("enrollment was entered %d times", got)
	}
	if msg := recs[0].Message; !strings.Contains(msg, "error") {
		t.Errorf("a failed enrollment was not reported as a failure: %q", msg)
	}
	if v, ok := attr(recs[0], "in"); !ok || v.Duration() <= 0 {
		t.Errorf("a failed enrollment was rescheduled with no delay, which spins: %v", v)
	}
}
