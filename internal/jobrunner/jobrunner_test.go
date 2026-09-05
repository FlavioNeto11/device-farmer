package jobrunner

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/runner"
)

// What this loop may end, and what it may not.
//
// Exactly one thing releases a lease in this package: an Outcome that names a
// ReleaseReason. This loop adds no reason of its own from anything it observes
// about a socket, a device or a host — and before every release it asks the
// holder whether it still owns the lease at all, because a re-attach hands the
// replacement the SAME fence and the database therefore cannot tell the two
// placements apart.

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeLease is the two-method view of a holder that ending a lease uses.
type fakeLease struct {
	mu       sync.Mutex
	fenced   bool
	released bool
	err      error
	calls    []releaseCall
}

type releaseCall struct {
	reason lease.ReleaseReason
	rearm  time.Duration
	live   bool // the context handed to Release had not been cancelled
}

func (f *fakeLease) Fenced() bool { return f.fenced }

func (f *fakeLease) Release(ctx context.Context, reason lease.ReleaseReason, rearm time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, releaseCall{reason: reason, rearm: rearm, live: ctx.Err() == nil})
	return f.released, f.err
}

func (f *fakeLease) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ leaseHolder = (*fakeLease)(nil)

// logCapture keeps the loop's log lines, including their attributes: several of
// the decisions below are observable only as the line an operator would read.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }
func (c *logCapture) logger() *slog.Logger               { return slog.New(c) }

func (c *logCapture) count(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

// attrs returns the attributes of the first line containing substr.
func (c *logCapture) attrs(substr string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		if !strings.Contains(r.Message, substr) {
			continue
		}
		out := make(map[string]string, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			out[a.Key] = a.Value.String()
			return true
		})
		return out, true
	}
	return nil, false
}

// blindHolder is a real renewal loop over a database that is not there, which
// is the situation the witness exists for and the one every "an outage is not
// a verdict" assertion in this package is made about.
func blindHolder(t *testing.T) *lease.Holder {
	t.Helper()
	l := lease.Lease{
		ID: "11111111-1111-4111-8111-111111111111", DeviceID: "dev-1", JobID: "job-1",
		Fence: 5, Holder: "farmd-0", HolderInstance: "22222222-2222-4222-8222-222222222222",
	}
	h := lease.NewHolder(context.Background(), lease.NewStore(unreachablePool(t)), l, lease.HolderConfig{
		Interval:     2 * time.Millisecond,
		RenewTimeout: time.Second,
		RetryBase:    time.Millisecond,
		RetryMax:     2 * time.Millisecond,
		Logger:       quietLogger(),
	})
	t.Cleanup(h.Stop)
	return h
}

// waitFor polls until cond holds, and fails the test if it never does. The
// conditions are about accumulated state — "three renewals have failed" —
// not about a single event, which is why it polls rather than listens.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// unreachablePool is a real pool that will never connect. Every statement it is
// handed fails with a dial error rather than a panic, which is what lets a test
// see whether a statement was attempted at all — the difference between "this
// loop wrote a verdict" and "this loop wrote nothing", which is the whole
// question on a fenced path.
func unreachablePool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	p, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		tb.Fatalf("building the unreachable pool: %v", err)
	}
	tb.Cleanup(p.Close)
	return p
}

// testLoop builds a JobRunner without New, whose validation needs wiring this
// package's decisions do not.
func testLoop(tb testing.TB, mutate func(*Config)) (*JobRunner, *logCapture) {
	tb.Helper()

	logs := &logCapture{}
	cfg := Config{
		Pool:      unreachablePool(tb),
		Logger:    logs.logger(),
		Holder:    "farmd-0",
		SlotRearm: 35 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.applyDefaults()

	jr := &JobRunner{
		cfg:      cfg,
		log:      cfg.Logger,
		busy:     make(map[string]struct{}),
		deferred: make(map[string]*deferral),
	}
	jr.claims = &claimLocks{pool: cfg.Pool, log: jr.log, timeout: cfg.CallTimeout, held: make(map[string]int64)}
	return jr, logs
}

// ---------------------------------------------------------------------------
// 3. A fenced process must not release the lease it lost
// ---------------------------------------------------------------------------

// This is the one line standing between a stale process and somebody else's
// running job.
//
// It is tempting to assume the database catches it: every mutating lease
// function matches on (id, fence). It does not. farm.lease_acquire re-attaches
// at the SAME fence — deliberately, so a pod eviction costs nothing — so a
// process fenced by its own replacement still presents a fence that MATCHES.
// Its release would be accepted and the device handed away mid-run.
func TestAFencedProcessReleasesNothing(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	h := &fakeLease{fenced: true, released: true}

	jr.releaseLease(context.Background(), jr.log, h, lease.ReasonCompleted)

	if n := h.count(); n != 0 {
		t.Fatalf("released a lease this process no longer owns (%d call(s)); "+
			"the device would be taken from whoever does own it", n)
	}
	if logs.count("refusing to release a lease this process was fenced out of") == 0 {
		t.Fatal("the refusal was silent; an operator would never know it happened")
	}
}

// The release that does happen carries the reason the JOB produced and the
// rearm window the deployment configured — the window that must outlast the
// node proxy's self-fence so the previous holder's sockets are certainly
// severed before the slot is scheduled again.
func TestReleaseCarriesTheJobsReasonAndTheConfiguredRearm(t *testing.T) {
	t.Parallel()

	jr, _ := testLoop(t, func(c *Config) { c.SlotRearm = 42 * time.Second })
	h := &fakeLease{released: true}

	jr.releaseLease(context.Background(), jr.log, h, lease.ReasonFailed)

	if h.count() != 1 {
		t.Fatalf("Release called %d times, want 1", h.count())
	}
	got := h.calls[0]
	if got.reason != lease.ReasonFailed {
		t.Fatalf("reason = %q, want the job's own reason", got.reason)
	}
	if got.rearm != 42*time.Second {
		t.Fatalf("rearm = %s, want the configured window", got.rearm)
	}
}

// A release is issued precisely when the run is being torn down, which is when
// the caller's context is dead. Deriving the release from it would abort the
// very call that hands the device back, parking it until the reaper takes it
// TTL+grace later.
func TestReleaseSurvivesTheCancellationThatEndedTheWork(t *testing.T) {
	t.Parallel()

	jr, _ := testLoop(t, nil)
	h := &fakeLease{released: true}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jr.releaseLease(ctx, jr.log, h, lease.ReasonCompleted)

	if h.count() != 1 {
		t.Fatalf("Release called %d times, want 1 even though the caller's context was dead", h.count())
	}
	if !h.calls[0].live {
		t.Fatal("the release inherited the cancellation that ended the run; the device would sit parked")
	}
}

// farm.leases.release_reason has no connectivity value, so a release naming one
// raises check_violation instead of quietly destroying hours of work. That
// refusal is the caller's bug and is logged as one, never retried.
func TestADatabaseRefusalOfAReleaseReasonIsLoggedAsACallerBug(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	h := &fakeLease{err: &lease.CheckViolationError{
		Op: "release", Reason: lease.ReleaseReason("device_offline"),
		Constraint: "leases_release_reason_check", Message: "new row violates check constraint",
	}}

	jr.releaseLease(context.Background(), jr.log, h, lease.ReleaseReason("device_offline"))

	if logs.count("this is a bug in the caller") == 0 {
		t.Fatal("a refused release reason was logged as a transient fault")
	}
}

// A release that simply failed leaves the device to the reaper, and says so.
func TestAFailedReleaseSaysWhoWillCleanUp(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	h := &fakeLease{err: errors.New("closed pool")}

	jr.releaseLease(context.Background(), jr.log, h, lease.ReasonCompleted)

	if logs.count("the reaper will reclaim it after ttl+grace") == 0 {
		t.Fatal("a failed release was silent about what happens to the device")
	}
}

// The reason comes from the JOB, never from anything observed about the device.
func TestUnwindTakesItsReasonFromTheJob(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state string
		want  lease.ReleaseReason
	}{
		{"cancelled", lease.ReasonJobCancelled},
		{"failed", lease.ReasonFailed},
		{"succeeded", lease.ReasonCompleted},
	} {
		t.Run(tc.state, func(t *testing.T) {
			jr, _ := testLoop(t, nil)
			h := &fakeLease{released: true}
			jr.unwind(context.Background(), jr.log, h, tc.state)

			if h.count() != 1 {
				t.Fatalf("Release called %d times, want 1", h.count())
			}
			if h.calls[0].reason != tc.want {
				t.Fatalf("reason = %q, want %q", h.calls[0].reason, tc.want)
			}
			if !h.calls[0].reason.Valid() {
				t.Fatalf("reason %q is not one of the seven the schema permits", h.calls[0].reason)
			}
		})
	}

	// 'running', 'queued', 'allocating': the job has not ended, so neither has
	// its lease.
	for _, state := range []string{"running", "queued", "allocating", ""} {
		jr, _ := testLoop(t, nil)
		h := &fakeLease{released: true}
		jr.unwind(context.Background(), jr.log, h, state)
		if h.count() != 0 {
			t.Fatalf("state %q released a lease belonging to a job that has not ended", state)
		}
	}

	// And a fenced holder releases nothing, whatever the job says.
	jr, _ := testLoop(t, nil)
	h := &fakeLease{fenced: true, released: true}
	jr.unwind(context.Background(), jr.log, h, "succeeded")
	if h.count() != 0 {
		t.Fatal("a fenced process released a lease because the job had succeeded")
	}
}

// ---------------------------------------------------------------------------
// The verdict written to farm.jobs
// ---------------------------------------------------------------------------

// finalize is a SAFETY NET, not the primary path: the runner writes its own
// verdict first. What it must never do is write one for a lease that is no
// longer ours — a re-attach preserves the fence, so the SQL guard cannot catch
// that and only the holder knows.
func TestFinalizeWritesNothingWhenTheLeaseIsNotOurs(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	p := runner.Placement{JobID: "job-1", Fence: 9}
	job := jobRow{State: "running", Attempt: 1, MaxAttempts: 3}

	jr.finalize(context.Background(), jr.log, p, job,
		runner.Outcome{State: runner.StateFailed, Fenced: true, Attempt: 1, Error: "step 2 failed"})

	if n := logs.count("could not write the job's state"); n != 0 {
		t.Fatalf("a fenced process attempted %d write(s) to a job somebody else is running", n)
	}
}

// An abandoned attempt — SIGTERM, a node drain, a preemption — belongs to
// whoever picks the job up next. A process that has just been told to stop does
// not get the last word.
func TestFinalizeWritesNothingForAnAbandonedAttempt(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	p := runner.Placement{JobID: "job-1", Fence: 9}
	job := jobRow{State: "running", Attempt: 1, MaxAttempts: 3}

	jr.finalize(context.Background(), jr.log, p, job,
		runner.Outcome{State: runner.StateAbandoned, Attempt: 1, Error: "SIGTERM"})

	if n := logs.count("could not write the job's state"); n != 0 {
		t.Fatalf("an abandoned attempt wrote %d verdict(s)", n)
	}
}

// The same failure four times on one device is a device problem; four failures
// on four devices is a job problem, and farm.job_attempts is the table that
// tells them apart. A job with attempts left therefore goes back on the QUEUE,
// so the scheduler places it somewhere else.
func TestFinalizeReQueuesAFailureThatStillHasAttemptsLeft(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		job   jobRow
		out   runner.Outcome
		state string
	}{
		{
			name:  "a failure with budget left goes back on the queue",
			job:   jobRow{Attempt: 1, MaxAttempts: 3},
			out:   runner.Outcome{State: runner.StateFailed, Attempt: 1},
			state: "queued",
		},
		{
			name:  "a failure on the last attempt is final",
			job:   jobRow{Attempt: 3, MaxAttempts: 3},
			out:   runner.Outcome{State: runner.StateFailed, Attempt: 3},
			state: "failed",
		},
		{
			name: "an attempt the runner never claimed falls back to the job's own counter",
			job:  jobRow{Attempt: 3, MaxAttempts: 3},
			out:  runner.Outcome{State: runner.StateFailed}, // Attempt 0
			// The runner never got far enough to claim one, so the job's
			// counter decides — and it says this was the last attempt.
			state: "failed",
		},
		{
			name:  "success",
			job:   jobRow{Attempt: 1, MaxAttempts: 3},
			out:   runner.Outcome{State: runner.StateSucceeded, Attempt: 1},
			state: "succeeded",
		},
		{
			name:  "a human cancelled it",
			job:   jobRow{Attempt: 1, MaxAttempts: 3},
			out:   runner.Outcome{State: runner.StateCancelled, Attempt: 1},
			state: "cancelled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jr, logs := testLoop(t, nil)
			p := runner.Placement{JobID: "job-1", Fence: 9}

			jr.finalize(context.Background(), jr.log, p, tc.job, tc.out)

			attrs, ok := logs.attrs("could not write the job's state")
			if !ok {
				t.Fatal("no write was attempted at all")
			}
			if attrs["state"] != tc.state {
				t.Fatalf("wrote state %q, want %q", attrs["state"], tc.state)
			}
		})
	}
}

// Bookkeeping must outlive the cancellation that ended the work. A job left
// 'running' after the process that ran it is gone is a job nothing recovers.
func TestTheVerdictIsWrittenEvenWhenTheRunWasCancelled(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jr.finalize(ctx, jr.log, runner.Placement{JobID: "job-1", Fence: 9},
		jobRow{Attempt: 1, MaxAttempts: 3},
		runner.Outcome{State: runner.StateSucceeded, Attempt: 1})

	attrs, ok := logs.attrs("could not write the job's state")
	if !ok {
		t.Fatal("the verdict was never attempted because the run's context had ended")
	}
	// The statement reached the wire and failed there, rather than being
	// refused locally by the dead context it descended from.
	if strings.Contains(attrs["err"], "context canceled") {
		t.Fatalf("the write inherited the cancellation that ended the run: %s", attrs["err"])
	}
}

// A job in state 'running' with no live lease is nobody's to release, because
// there is nothing to release. It goes back on the queue.
func TestAnOrphanedPlacementIsReturnedToTheQueue(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)
	jr.requeueOrphan(context.Background(), jr.log, "job-1")

	attrs, ok := logs.attrs("could not write the job's state")
	if !ok {
		t.Fatal("no write was attempted")
	}
	if attrs["state"] != "queued" {
		t.Fatalf("state = %q, want queued", attrs["state"])
	}
	// The write it attempts carries fence 0, which never exists —
	// farm.fence_seq starts at 1 — so writeJob's guard reads as "write only if
	// this job has NO live lease at all", which is precisely the condition
	// being repaired. Nothing is released, because there is nothing to release.
}

// ---------------------------------------------------------------------------
// A database outage is not a fencing event
// ---------------------------------------------------------------------------

// The guard every release site consults is lease.Holder.Fenced(), and it must
// answer "no" while the database is unreachable. Answering "yes" would abandon
// running jobs on a blip; answering "yes" and then releasing would be #663 with
// the control plane as the new trigger.
func TestADatabaseOutageNeitherFencesNorCancelsTheJob(t *testing.T) {
	t.Parallel()

	h := blindHolder(t)
	waitFor(t, "three renewals to fail on the wire", func() bool { return h.Stats().ConsecutiveFailures >= 3 })
	if h.Fenced() {
		t.Fatal("a database outage was reported as fencing; every running job would be abandoned")
	}
	if err := h.Context().Err(); err != nil {
		t.Fatalf("a database outage cancelled the job's context: %v", context.Cause(h.Context()))
	}
	if h.Stats().Renewals != 0 {
		t.Fatalf("Renewals = %d against a database that never answered", h.Stats().Renewals)
	}

	// And the loop's own guard, driven by the real holder through the same
	// interface every release site uses.
	jr, logs := testLoop(t, nil)
	jr.releaseLease(context.Background(), jr.log, h, lease.ReasonCompleted)
	if logs.count("refusing to release a lease this process was fenced out of") != 0 {
		t.Fatal("the loop refused to release because the database was down")
	}
}

// ---------------------------------------------------------------------------
// Construction and defaults. None of these can end a lease; the worst a bad
// value can do is start work slowly.
// ---------------------------------------------------------------------------

func TestNewRequiresTheWiringItCannotInvent(t *testing.T) {
	t.Parallel()

	pool := unreachablePool(t)
	store := lease.NewStore(pool)
	exec := stubExecutor{}

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no pool", Config{}, "Pool"},
		{"no store", Config{Pool: pool}, "Store"},
		{"no runner", Config{Pool: pool, Store: store}, "Runner"},
		{"no holder name", Config{Pool: pool, Store: store, Runner: exec}, "Holder is required"},
		{"no holder instance", Config{Pool: pool, Store: store, Runner: exec, Holder: "farmd-0"}, "HolderInstance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("New accepted an incomplete Config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to name %q", err, tc.want)
			}
		})
	}

	jr, err := New(Config{
		Pool: pool, Store: store, Runner: exec,
		Holder: "farmd-0", HolderInstance: "22222222-2222-4222-8222-222222222222",
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if jr.cfg.Dial == nil {
		t.Fatal("New left no Dialer; every placement would be unaddressable")
	}
	if jr.cfg.Component != DefaultComponent {
		t.Fatalf("Component = %q; a jobrunner outage invisible to the reaper's gap accounting "+
			"is what lets it reclaim leases whose holders were never given a chance to renew", jr.cfg.Component)
	}
}

type stubExecutor struct{}

func (stubExecutor) Run(context.Context, runner.Holder, runner.Placement, runner.Conn) (runner.Outcome, error) {
	return runner.Outcome{}, nil
}

// The takeover window asks "has this lease missed several renewals?", so it is
// expressed in renewal intervals. An operator who slows renewal down must not
// thereby invite a takeover of a lease that is renewing perfectly well.
func TestTakeoverNeverFallsBelowThreeRenewalIntervals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		interval time.Duration
		takeover time.Duration
	}{
		{"defaults", 0, 0},
		{"an operator slowed renewal right down", 5 * time.Minute, 0},
		{"an operator asked for a very short takeover", 2 * time.Minute, time.Second},
		{"a negative takeover", time.Minute, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Takeover: tc.takeover}
			cfg.HolderConfig.Interval = tc.interval
			cfg.applyDefaults()

			interval := tc.interval
			if interval <= 0 {
				interval = lease.DefaultRenewInterval
			}
			if cfg.Takeover < 3*interval {
				t.Fatalf("Takeover = %s, below three renewal intervals of %s", cfg.Takeover, interval)
			}
			// Far below the minimum lease TTL, so a replacement resumes long
			// before the lease is even marked suspect.
			if cfg.Takeover >= lease.DefaultTTL && tc.interval == 0 {
				t.Fatalf("Takeover = %s, not comfortably inside the TTL", cfg.Takeover)
			}
		})
	}
}

func TestDefaultsFillInEveryBoundTheLoopNeeds(t *testing.T) {
	t.Parallel()

	for _, cfg := range []Config{
		{},
		{Concurrency: -1, Batch: -1, Interval: -1, IdleInterval: -1, CallTimeout: -1, JobBackoff: -1, JobBackoffMax: -1, SlotRearm: -1},
		{Interval: time.Minute},        // idle must not end up shorter than the busy poll
		{JobBackoff: 10 * time.Minute}, // nor the cap shorter than the floor
	} {
		got := cfg
		got.applyDefaults()
		if got.Concurrency <= 0 || got.Batch <= 0 || got.Interval <= 0 || got.CallTimeout <= 0 {
			t.Fatalf("applyDefaults left a non-positive bound: %+v", got)
		}
		if got.IdleInterval < got.Interval {
			t.Fatalf("IdleInterval %s < Interval %s", got.IdleInterval, got.Interval)
		}
		if got.JobBackoffMax < got.JobBackoff {
			t.Fatalf("JobBackoffMax %s < JobBackoff %s", got.JobBackoffMax, got.JobBackoff)
		}
		if got.SlotRearm <= 0 {
			t.Fatalf("SlotRearm = %s; a slot re-armed instantly can be scheduled while the "+
				"previous holder's sockets are still open", got.SlotRearm)
		}
		if got.Logger == nil || got.Component == "" {
			t.Fatalf("applyDefaults left Logger or Component empty: %+v", got)
		}
	}
}

// ---------------------------------------------------------------------------
// Local scheduling memory. None of it is authoritative about anything.
// ---------------------------------------------------------------------------

func TestAJobIsStartedOnceAndBackedOffAfterAFailureToStart(t *testing.T) {
	t.Parallel()

	jr, _ := testLoop(t, func(c *Config) {
		c.JobBackoff = 50 * time.Millisecond
		c.JobBackoffMax = 400 * time.Millisecond
	})

	if !jr.reserve("job-1") {
		t.Fatal("a free job could not be reserved")
	}
	if jr.reserve("job-1") {
		t.Fatal("one job was reserved twice; two processes would drive one phone")
	}
	if jr.inFlight() != 1 {
		t.Fatalf("inFlight = %d, want 1", jr.inFlight())
	}
	jr.unreserve("job-1")
	if jr.inFlight() != 0 {
		t.Fatalf("inFlight = %d after unreserve", jr.inFlight())
	}

	// A job this loop could not START is deferred, and the wait grows.
	jr.defer_("job-1")
	if jr.reserve("job-1") {
		t.Fatal("a deferred job was picked up again immediately")
	}

	var last time.Duration
	for i := range 8 {
		jr.mu.Lock()
		d := jr.deferred["job-1"]
		wait := time.Until(d.until)
		jr.mu.Unlock()
		// The cap is jittered by up to a tenth either way, which is the point of
		// it: N replicas must not wake in lockstep after a restart.
		if wait > jr.cfg.JobBackoffMax+jr.cfg.JobBackoffMax/10 {
			t.Fatalf("backoff %s exceeds the jittered cap %s", wait, jr.cfg.JobBackoffMax)
		}
		if i > 0 && wait < last/2 {
			t.Fatalf("backoff shrank from %s to %s", last, wait)
		}
		last = wait
		jr.defer_("job-1")
	}
	if last < jr.cfg.JobBackoff {
		t.Fatalf("backoff settled at %s, below the floor %s", last, jr.cfg.JobBackoff)
	}
}

// The map is bounded, and a lapsed entry is kept for one further window so a
// job polled again immediately does not reset its backoff to the floor.
func TestDeferralsAreBoundedWithoutForgettingTooSoon(t *testing.T) {
	t.Parallel()

	jr, _ := testLoop(t, func(c *Config) {
		c.JobBackoff = time.Millisecond
		c.JobBackoffMax = 10 * time.Millisecond
	})

	jr.defer_("recent")
	jr.mu.Lock()
	jr.deferred["ancient"] = &deferral{until: time.Now().Add(-time.Hour), tries: 4}
	jr.mu.Unlock()

	jr.pruneDeferrals(time.Now())

	jr.mu.Lock()
	_, keptRecent := jr.deferred["recent"]
	_, keptAncient := jr.deferred["ancient"]
	n := len(jr.deferred)
	jr.mu.Unlock()

	if !keptRecent {
		t.Fatal("a live deferral was pruned; the job would retry at the floor immediately")
	}
	if keptAncient {
		t.Fatal("a long-lapsed deferral was kept; the map grows for the life of the process")
	}
	if n != 1 {
		t.Fatalf("deferred holds %d entries, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// The claim lock
// ---------------------------------------------------------------------------

// pg_try_advisory_lock is RE-ENTRANT within a session: a second successful call
// stacks a second reference needing a second unlock, and give() issues exactly
// one. A stranded claim key is a job no replica in the farm can ever start
// again — so a key we already hold is answered from the map, without a second
// lock.
func TestTakingAClaimWeAlreadyHoldNeverStacksASecondLock(t *testing.T) {
	t.Parallel()

	jr, _ := testLoop(t, nil)
	jr.claims.held["job-1"] = claimKey("job-1")

	// The pool is unreachable, so a call that reached the database at all would
	// fail. This one must not reach it.
	ok, err := jr.claims.take(context.Background(), "job-1")
	if err != nil || !ok {
		t.Fatalf("take = %t, %v; want a claim we already hold answered from memory", ok, err)
	}

	// A key we do NOT hold has to go and ask, and says so when it cannot.
	if _, err := jr.claims.take(context.Background(), "job-2"); err == nil {
		t.Fatal("take reported a claim it never managed to ask about")
	}
}

// Giving back a claim nobody took is a no-op, and close() clears the map even
// when there was never a session.
func TestGivingBackAClaimNobodyTookIsHarmless(t *testing.T) {
	t.Parallel()

	jr, _ := testLoop(t, nil)
	jr.claims.give("never-claimed")
	jr.finishClaim("never-claimed")

	jr.claims.held["job-9"] = claimKey("job-9")
	jr.claims.close()
	if len(jr.claims.held) != 0 {
		t.Fatalf("close left %d claim(s) behind", len(jr.claims.held))
	}
}

// The one-argument advisory space is shared with the scheduler's and the
// reaper's leadership keys, so the prefix is not decoration: it makes a
// collision with either of those fixed constants a 2^-63 event.
func TestClaimKeysAreStableAndNamespaced(t *testing.T) {
	t.Parallel()

	a := claimKey("11111111-1111-4111-8111-111111111111")
	if a != claimKey("11111111-1111-4111-8111-111111111111") {
		t.Fatal("claimKey is not stable; a restart would fail to recognise its own locks")
	}
	if a == claimKey("11111111-1111-4111-8111-111111111112") {
		t.Fatal("two jobs share one claim key")
	}
	// The namespace prefix is what the guarantee rests on.
	bare := fnv.New64a()
	_, _ = bare.Write([]byte("11111111-1111-4111-8111-111111111111"))
	if a == int64(bare.Sum64()) {
		t.Fatal("claimKey hashes the bare job id, with no namespace to keep it away from " +
			"the scheduler's and the reaper's leadership keys")
	}
}

// ---------------------------------------------------------------------------
// Small helpers with sharp edges
// ---------------------------------------------------------------------------

// A duration, sent as text and cast server-side. Nothing here tells Postgres
// what time this process thinks it is: pod clock skew must not be able to move
// a lease deadline.
func TestIntervalArgSendsADurationAndNeverAnInstant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{3 * time.Minute, "180000000 microseconds"},
		{1500 * time.Nanosecond, "1 microseconds"},
		{0, "0 microseconds"},
		{-time.Hour, "0 microseconds"},
	} {
		if got := intervalArg(tc.in); got != tc.want {
			t.Fatalf("intervalArg(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Timers are jittered so N replicas sharing one database do not wake in
// lockstep after a restart.
func TestJitterSpreadsWithoutDrifting(t *testing.T) {
	t.Parallel()

	const base = time.Second
	seen := make(map[time.Duration]bool)
	for range 200 {
		d := jitter(base)
		if d < base-base/10 || d > base+base/10 {
			t.Fatalf("jitter(%s) = %s, outside ±10%%", base, d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("jitter produced %d distinct delays; replicas would still wake together", len(seen))
	}
	if jitter(0) != 0 || jitter(-time.Second) != -time.Second {
		t.Fatal("jitter invented a delay out of a non-positive one")
	}
}

func TestDetailAndErrorTextAreAlwaysStorable(t *testing.T) {
	t.Parallel()

	if jsonOrEmpty(nil) != "{}" || jsonOrEmpty(map[string]any{}) != "{}" {
		t.Fatal("an empty detail map must still be valid jsonb")
	}
	if got := jsonOrEmpty(map[string]any{"reason": "no devpath"}); got != `{"reason":"no devpath"}` {
		t.Fatalf("jsonOrEmpty = %q", got)
	}
	// A detail map that will not marshal must not cost the row that carries the
	// actual result.
	got := jsonOrEmpty(map[string]any{"bad": make(chan int)})
	if !strings.Contains(got, "detail_marshal_error") {
		t.Fatalf("jsonOrEmpty = %q, want a valid jsonb document", got)
	}

	if nullString("") != nil {
		t.Fatal("an empty string was written as text rather than NULL")
	}
	if s := nullString("boom"); s == nil || *s != "boom" {
		t.Fatalf("nullString = %v", s)
	}
}

// A panic anywhere in a job's goroutine must not reach the runtime: every OTHER
// job on this replica renews its lease from this same process, and letting it
// die would stop all of those heartbeats at once — one job's bug destroying
// unrelated multi-hour work.
func TestAPanicIsContainedToTheJobThatRaisedIt(t *testing.T) {
	t.Parallel()

	jr, logs := testLoop(t, nil)

	func() {
		defer jr.recoverRun(jr.log)
		panic("a step executor did something unforgivable")
	}()

	if logs.count("PANIC while running a job") == 0 {
		t.Fatal("a contained panic was silent; farm_jobrunner_panics_total is meant to be alerted on")
	}
	attrs, _ := logs.attrs("PANIC while running a job")
	if !strings.Contains(attrs["panic"], "unforgivable") {
		t.Fatalf("the panic value was not recorded: %v", attrs)
	}
	if attrs["stack"] == "" {
		t.Fatal("no stack was recorded; the bug would be unfindable")
	}

	// And a goroutine that did not panic must not be reported as one.
	jr2, logs2 := testLoop(t, nil)
	func() { defer jr2.recoverRun(jr2.log) }()
	if logs2.count("PANIC") != 0 {
		t.Fatal("recoverRun reported a panic that never happened")
	}
}
