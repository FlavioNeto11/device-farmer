package lease

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =====================================================================
// The renewal loop
//
// Most of what follows drives the loop with a stub Store, because the property
// under test is a decision about TIMING and BRANCHING that a real database
// cannot be made to produce on demand: "stay alive across forty consecutive
// failures" needs forty failures in a few milliseconds, and "abort the instant
// the answer is zero rows" needs the zero-rows answer to arrive on the second
// attempt and not the first. Against real SQL those are a sleep and a race.
//
// What the stub cannot prove is that ErrFenced only ever comes from zero rows;
// store_test.go proves that against real SQL, and TestHolderAgainstPostgres at
// the bottom of this file joins the two halves end to end.
// =====================================================================

// fakeOps is a stub Store. NewHolder takes a concrete *Store, so the holder
// reaches it through the unexported leaseOps interface that exists for exactly
// this purpose.
type fakeOps struct {
	// Set before the holder starts and never afterwards; the renewal goroutine
	// reads them.
	//
	// renewFn receives the context the holder built for this attempt, because
	// one of the properties under test is a fact about that context: a renewal
	// that wedges must be cut off by RenewTimeout rather than eating the whole
	// interval.
	renewFn   func(ctx context.Context, call int) (RenewResult, error)
	releaseFn func(call int) (bool, error)
	witnessFn func(call int) (time.Time, bool, error)
	onRelease func()

	mu        sync.Mutex
	renews    []renewCall
	releases  []releaseCall
	witnesses []witnessCall
}

type renewCall struct {
	leaseID        string
	fence          int64
	holderInstance string

	// budget is how long this attempt had left when the store was entered, and
	// zero when the holder imposed no deadline at all. A holder that passed its
	// own context straight through would leave one wedged connection consuming
	// the whole renewal interval, which is silence at the server.
	budget time.Duration
}

// witnessCall records what the holder actually presented to the store. The
// fence matters because a witness carrying the wrong one is refused server-side
// and buys the job nothing; maxExtensions matters because a holder that sent
// zero would have the store quietly substitute its own default, so an operator
// who tightened the cap would have tightened nothing.
type witnessCall struct {
	leaseID       string
	fence         int64
	maxExtensions int
}

type releaseCall struct {
	// ctxErr is the state of the context the holder handed the store. It must
	// be nil even when the caller passed a context that the holder itself has
	// just cancelled: a release that aborts leaves the device parked until the
	// reaper takes it TTL+grace later.
	ctxErr  error
	leaseID string
	fence   int64
	reason  ReleaseReason
	rearm   time.Duration
}

func (f *fakeOps) Renew(ctx context.Context, leaseID string, fence int64, holderInstance string) (RenewResult, error) {
	var budget time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}

	f.mu.Lock()
	f.renews = append(f.renews, renewCall{
		leaseID: leaseID, fence: fence, holderInstance: holderInstance, budget: budget,
	})
	call := len(f.renews)
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return RenewResult{}, fmt.Errorf("lease: renew %s: %w", leaseID, err)
	}
	if f.renewFn == nil {
		return okRenew(), nil
	}
	return f.renewFn(ctx, call)
}

func (f *fakeOps) Release(ctx context.Context, leaseID string, fence int64, reason ReleaseReason, rearm time.Duration) (bool, error) {
	if f.onRelease != nil {
		f.onRelease()
	}
	f.mu.Lock()
	f.releases = append(f.releases, releaseCall{
		ctxErr: ctx.Err(), leaseID: leaseID, fence: fence, reason: reason, rearm: rearm,
	})
	call := len(f.releases)
	f.mu.Unlock()

	if f.releaseFn == nil {
		return true, nil
	}
	return f.releaseFn(call)
}

func (f *fakeOps) Witness(_ context.Context, leaseID string, fence int64, maxExtensions int) (time.Time, bool, error) {
	f.mu.Lock()
	f.witnesses = append(f.witnesses, witnessCall{
		leaseID: leaseID, fence: fence, maxExtensions: maxExtensions,
	})
	call := len(f.witnesses)
	f.mu.Unlock()

	if f.witnessFn == nil {
		return time.Now().Add(30 * time.Minute), true, nil
	}
	return f.witnessFn(call)
}

func (f *fakeOps) renewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.renews)
}

func (f *fakeOps) renewCalls() []renewCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]renewCall(nil), f.renews...)
}

func (f *fakeOps) lastRenew() (renewCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.renews) == 0 {
		return renewCall{}, false
	}
	return f.renews[len(f.renews)-1], true
}

func (f *fakeOps) releaseCalls() []releaseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]releaseCall(nil), f.releases...)
}

func (f *fakeOps) witnessCalls() []witnessCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]witnessCall(nil), f.witnesses...)
}

var _ leaseOps = (*fakeOps)(nil)

// okRenew is what a healthy Postgres returns: deadlines computed server-side,
// comfortably in the future.
func okRenew() RenewResult {
	now := time.Now()
	return RenewResult{
		ExpiresAt:     now.Add(15 * time.Minute),
		ReclaimableAt: now.Add(45 * time.Minute),
	}
}

// fenced is what Store.Renew returns when farm.lease_renew matched no row.
func fenced(leaseID string, fence int64) error {
	return fmt.Errorf("lease: renew %s at fence %d: %w", leaseID, fence, ErrFenced)
}

// transient is what Store.Renew returns when Postgres was briefly unreachable.
// It is deliberately built the same way — a wrapped transport error — so the
// only thing separating it from fenced() in the loop's eyes is the sentinel.
func transient(leaseID string) error {
	return fmt.Errorf("lease: renew %s: %w", leaseID,
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
}

// testLease is the lease every stub-driven test holds.
func testLease() Lease {
	return Lease{
		ID:             "5f2ab1c6-0f3a-4a2f-9d1b-2c7e5a0d4411",
		DeviceID:       "8c31a7d2-4b6e-4f0a-9c22-3d5b7f1e6002",
		JobID:          "1d9f4e70-2a5c-4c8b-8f31-6b0c9a2d3300",
		Fence:          4242,
		Holder:         "farm-runner-7f9c",
		HolderInstance: "a41c9e88-6d2b-4f13-9a70-51c8b3d02244",
		ExpiresAt:      time.Now().Add(15 * time.Minute),
		ReclaimableAt:  time.Now().Add(45 * time.Minute),
	}
}

// quietConfig keeps the fast test cadence and sends the holder's logs nowhere;
// the loop logs an error line on every fencing, and several tests cause one on
// purpose.
func quietConfig(interval time.Duration) HolderConfig {
	return HolderConfig{
		Interval:       interval,
		RenewTimeout:   2 * time.Second,
		RetryBase:      time.Millisecond,
		RetryMax:       3 * time.Millisecond,
		ReleaseTimeout: 2 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// startHolder starts a renewal loop over a stub Store.
//
// NewHolder takes a concrete *Store on purpose — nothing outside this package
// should be able to substitute the thing that decides fencing — so a stub-driven
// loop has to be assembled here, exactly as NewHolder assembles it. Keeping the
// two in step is the point: a field NewHolder starts setting and this helper
// does not will show up as a zero value in a failing test rather than as a
// quietly divergent test-only holder.
func startHolder(t *testing.T, ops leaseOps, l Lease, cfg HolderConfig) *Holder {
	t.Helper()
	cfg.applyDefaults()

	ctx, cancel := context.WithCancelCause(t.Context())
	h := &Holder{
		store:    ops,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		loopDone: make(chan struct{}),
		lease:    l,
	}
	go h.run()
	t.Cleanup(h.Stop)
	return h
}

// waitFor polls until cond holds, and fails the test if it never does. Polling
// rather than a channel because the conditions are about accumulated state —
// "five renewals have happened" — not about a single event.
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

func waitDone(t *testing.T, h *Holder, what string) {
	t.Helper()
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// =====================================================================
// The two branches
// =====================================================================

// TestHolderKeepsContextAliveWhileRenewalsSucceed is the steady state: the job
// runs, the loop asks Postgres whether the holder still exists, and nothing
// about that question ever touches the work.
func TestHolderKeepsContextAliveWhileRenewalsSucceed(t *testing.T) {
	fake := &fakeOps{}
	l := testLease()
	h := startHolder(t, fake, l, quietConfig(time.Millisecond))

	waitFor(t, "five successful renewals", func() bool { return h.Stats().Renewals >= 5 })

	if err := h.Context().Err(); err != nil {
		t.Fatalf("context died under successful renewals: %v (cause %v)", err, context.Cause(h.Context()))
	}
	if h.Fenced() {
		t.Error("Fenced() is true while every renewal succeeded")
	}
	if err := h.Err(); err != nil {
		t.Errorf("Err() = %v, want nil while the lease is held", err)
	}

	// The loop presents the identity it holds on every attempt: all three of
	// (id, fence, holder_instance) are matched server-side.
	got, ok := fake.lastRenew()
	if !ok {
		t.Fatal("no renewal was issued")
	}
	if got.leaseID != l.ID || got.fence != l.Fence || got.holderInstance != l.HolderInstance {
		t.Errorf("renew called with %+v, want id %s fence %d instance %s",
			got, l.ID, l.Fence, l.HolderInstance)
	}

	// Deadlines are the server's, folded into the cached copy.
	stats := h.Stats()
	if !stats.ExpiresAt.After(time.Now()) {
		t.Errorf("cached expires_at %s is not in the future", stats.ExpiresAt)
	}
	if stats.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", stats.ConsecutiveFailures)
	}
	if len(fake.releaseCalls()) != 0 {
		t.Error("a healthy holder released its lease")
	}
}

// TestHolderFencedCancelsContext is the terminal branch. Zero rows means the
// device belongs to somebody else now, so the context that every ADB socket,
// subprocess and log tailer derives from is cancelled at once.
func TestHolderFencedCancelsContext(t *testing.T) {
	l := testLease()

	var fencedHook atomic.Int64
	cfg := quietConfig(time.Millisecond)
	cfg.Hooks.OnFenced = func(Lease) { fencedHook.Add(1) }

	fake := &fakeOps{
		// The first renewal succeeds so the failure under test is the fencing
		// itself and not a holder that never got going.
		renewFn: func(_ context.Context, call int) (RenewResult, error) {
			if call == 1 {
				return okRenew(), nil
			}
			return RenewResult{}, fenced(l.ID, l.Fence)
		},
	}
	h := startHolder(t, fake, l, cfg)

	waitDone(t, h, "the holder's context to be cancelled by fencing")

	if !h.Fenced() {
		t.Error("Fenced() = false after farm.lease_renew returned zero rows")
	}
	if err := h.Err(); !errors.Is(err, ErrFenced) {
		t.Errorf("Err() = %v, want it to wrap ErrFenced", err)
	}
	if got := fencedHook.Load(); got != 1 {
		t.Errorf("OnFenced fired %d times, want exactly 1", got)
	}

	// A fenced holder must not release: the lease it would name is already
	// somebody else's, and releasing on their fence is refused anyway. Ending
	// the job is the whole response.
	if calls := fake.releaseCalls(); len(calls) != 0 {
		t.Errorf("a fenced holder issued %d release(s): %+v", len(calls), calls)
	}

	// The loop stops asking. Nothing about a fenced lease is retryable.
	after := fake.renewCount()
	time.Sleep(20 * time.Millisecond)
	if got := fake.renewCount(); got != after {
		t.Errorf("renewals continued after fencing: %d -> %d", after, got)
	}
}

// TestHolderSurvivesALongTransientOutage is the property that makes a
// ten-minute database blip survivable, and the inverse of DeviceFarmer/STF
// #663.
//
// #663 releases a device mid-run because a transport failure was read as the
// holder's death. Here the control plane is the thing that fails, over and over,
// and the answer is the same every time: the lease is untouched, no deadline
// moved, the device is still ours. Retry, and do not cancel.
func TestHolderSurvivesALongTransientOutage(t *testing.T) {
	l := testLease()

	// The premise, asserted rather than assumed: this error is not fencing.
	if err := transient(l.ID); errors.Is(err, ErrFenced) {
		t.Fatalf("the stub's transient error satisfies errors.Is(err, ErrFenced): %v", err)
	}

	var down atomic.Bool
	down.Store(true)

	type failure struct {
		attempt int
		retryIn time.Duration
	}
	var (
		mu       sync.Mutex
		failures []failure
	)

	cfg := quietConfig(time.Millisecond)
	cfg.Hooks.OnTransientError = func(_ Lease, attempt int, _ error, retryIn time.Duration) {
		mu.Lock()
		failures = append(failures, failure{attempt: attempt, retryIn: retryIn})
		mu.Unlock()
	}

	fake := &fakeOps{
		renewFn: func(context.Context, int) (RenewResult, error) {
			if down.Load() {
				return RenewResult{}, transient(l.ID)
			}
			return okRenew(), nil
		},
	}
	h := startHolder(t, fake, l, cfg)

	// Ride out an outage many times longer than the renewal interval, checking
	// on every poll that the job is still running.
	failureCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(failures)
	}
	waitFor(t, "the control plane to fail thirty times running", func() bool {
		if err := h.Context().Err(); err != nil {
			t.Fatalf("the holder cancelled its context on a transient failure: %v (cause %v)",
				err, context.Cause(h.Context()))
		}
		return failureCount() >= 30
	})

	if h.Fenced() {
		t.Fatal("Fenced() = true after transport failures; a database blip cost a device")
	}
	if err := h.Err(); err != nil {
		t.Fatalf("Err() = %v during an outage, want nil", err)
	}
	if stats := h.Stats(); stats.Renewals != 0 || stats.ConsecutiveFailures < 30 {
		t.Errorf("stats = %+v, want zero renewals and at least thirty consecutive failures", stats)
	}
	if calls := fake.releaseCalls(); len(calls) != 0 {
		t.Errorf("the holder released its lease during an outage: %+v", calls)
	}

	// The failure counter is consecutive and monotonic, and every retry is
	// bounded by RetryMax so a recovering database is not stampeded.
	mu.Lock()
	snapshot := append([]failure(nil), failures...)
	mu.Unlock()
	for i, f := range snapshot {
		if f.attempt != i+1 {
			t.Fatalf("failure %d reported attempt %d; the consecutive count is wrong", i+1, f.attempt)
		}
		if f.retryIn <= 0 || f.retryIn > cfg.RetryMax {
			t.Fatalf("failure %d retries in %s, outside (0, %s]", i+1, f.retryIn, cfg.RetryMax)
		}
	}

	// Postgres comes back. The same lease, at the same fence, resumes.
	down.Store(false)
	waitFor(t, "renewals to resume", func() bool { return h.Stats().Renewals >= 2 })

	if err := h.Context().Err(); err != nil {
		t.Fatalf("context died after recovery: %v", err)
	}
	if got := h.Stats().ConsecutiveFailures; got != 0 {
		t.Errorf("consecutive failures = %d after recovery, want 0", got)
	}
	if got, _ := fake.lastRenew(); got.fence != l.Fence {
		t.Errorf("renewed at fence %d after recovery, want the fence we held, %d", got.fence, l.Fence)
	}
}

// TestHolderStopDoesNotRelease covers SIGTERM, node drain, preemption and spot
// reclaim — every way a pod goes away without its job being over.
//
// The lease, the device and the fence must survive, so the replacement pod can
// re-attach by job id and resume from the checkpoint. Releasing here would hand
// a half-finished job's handset to somebody else.
func TestHolderStopDoesNotRelease(t *testing.T) {
	fake := &fakeOps{}
	h := startHolder(t, fake, testLease(), quietConfig(time.Millisecond))

	waitFor(t, "the loop to start renewing", func() bool { return h.Stats().Renewals >= 1 })
	h.Stop()

	if calls := fake.releaseCalls(); len(calls) != 0 {
		t.Fatalf("Stop() released the lease: %+v", calls)
	}
	if err := h.Context().Err(); err == nil {
		t.Fatal("Stop() left the context alive")
	}
	if cause := context.Cause(h.Context()); !errors.Is(cause, ErrHolderStopped) {
		t.Errorf("cancellation cause = %v, want ErrHolderStopped", cause)
	}
	if h.Fenced() {
		t.Error("Fenced() = true after an orderly Stop; the supervisor would report a lost device")
	}

	// Stop joins the renewal goroutine, so nothing is in flight afterwards.
	after := fake.renewCount()
	time.Sleep(20 * time.Millisecond)
	if got := fake.renewCount(); got != after {
		t.Errorf("a renewal ran after Stop returned: %d -> %d", after, got)
	}

	h.Stop() // idempotent
}

// TestHolderReleaseUsesTheFenceItHolds pins the three things a release has to
// get right: the fence it presents, the ordering against its own cancellation,
// and surviving that cancellation.
func TestHolderReleaseUsesTheFenceItHolds(t *testing.T) {
	l := testLease()

	var (
		h                *Holder
		cancelledFirst   bool
		observedTheOrder bool
	)
	fake := &fakeOps{
		onRelease: func() {
			// Runs on the caller's goroutine, inside Holder.Release, before the
			// store call is recorded.
			cancelledFirst = h.Context().Err() != nil
			observedTheOrder = true
		},
	}
	h = startHolder(t, fake, l, quietConfig(time.Millisecond))
	waitFor(t, "the loop to start renewing", func() bool { return h.Stats().Renewals >= 1 })

	// Deliberately the holder's own context: that is what a job supervisor has
	// to hand, and Release cancels it before issuing the release.
	released, err := h.Release(h.Context(), ReasonCompleted, 40*time.Second)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released {
		t.Error("release reported that it changed nothing")
	}

	calls := fake.releaseCalls()
	if len(calls) != 1 {
		t.Fatalf("release issued %d store calls, want 1", len(calls))
	}
	got := calls[0]
	if got.leaseID != l.ID {
		t.Errorf("released lease %s, want %s", got.leaseID, l.ID)
	}
	if got.fence != l.Fence {
		t.Errorf("released at fence %d, want the fence the holder holds, %d", got.fence, l.Fence)
	}
	if got.reason != ReasonCompleted {
		t.Errorf("released with reason %q, want %q", got.reason, ReasonCompleted)
	}
	if got.rearm != 40*time.Second {
		t.Errorf("rearm = %s, want 40s", got.rearm)
	}
	// Local work is severed BEFORE the device can be handed on, and the release
	// still reaches the wire afterwards. Both halves, or the device is parked
	// until the reaper takes it TTL+grace later.
	if !observedTheOrder || !cancelledFirst {
		t.Error("the holder's context was not cancelled before the release was issued")
	}
	if got.ctxErr != nil {
		t.Errorf("the store was called with an already-cancelled context (%v); the release would never reach Postgres", got.ctxErr)
	}

	if cause := context.Cause(h.Context()); !errors.Is(cause, ErrHolderReleased) {
		t.Errorf("cancellation cause = %v, want ErrHolderReleased", cause)
	}
	if h.Fenced() {
		t.Error("Fenced() = true after a deliberate release")
	}
}

// TestHolderReleaseSurvivesASpentDeadline covers the most ordinary way a job is
// torn down: its own deadline just elapsed, and the context it hands Release is
// already past it. Re-imposing that deadline would kill the release before it
// reached the wire and park the device for TTL+grace.
func TestHolderReleaseSurvivesASpentDeadline(t *testing.T) {
	fake := &fakeOps{}
	h := startHolder(t, fake, testLease(), quietConfig(time.Millisecond))

	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	if _, err := h.Release(ctx, ReasonFailed, time.Second); err != nil {
		t.Fatalf("release with a spent deadline: %v", err)
	}
	calls := fake.releaseCalls()
	if len(calls) != 1 {
		t.Fatalf("release issued %d store calls, want 1", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Errorf("the store was called with an expired context (%v)", calls[0].ctxErr)
	}
	if calls[0].reason != ReasonFailed {
		t.Errorf("reason = %q, want %q", calls[0].reason, ReasonFailed)
	}
}

// TestHolderFencedVerdictSurvivesAConcurrentStop covers the race the `fenced`
// field exists for. Whoever wins stopOnce sets the cancellation cause, but the
// answer to "did I lose the device?" must not depend on that: a supervisor that
// reads "we shut down tidily" when the device was actually taken will log a
// clean exit over a run whose handset now belongs to somebody else.
func TestHolderFencedVerdictSurvivesAConcurrentStop(t *testing.T) {
	// The dangerous ordering, pinned rather than raced for.
	//
	// The verdict is produced by a loop whose context is ALREADY cancelled, so
	// the shutdown path and the fencing path are both live at once and the
	// shutdown path is the one that got there first. Leaving this to a genuine
	// race would make the test agree with broken code on any machine where the
	// timing happened to fall the other way, so the ordering is established by
	// waiting on the holder's own Done channel before the stub answers.
	t.Run("the shutdown lands first", func(t *testing.T) {
		l := testLease()
		answer := make(chan struct{})
		fake := &fakeOps{
			renewFn: func(context.Context, int) (RenewResult, error) {
				<-answer
				return RenewResult{}, fenced(l.ID, l.Fence)
			},
		}
		h := startHolder(t, fake, l, quietConfig(time.Millisecond))
		waitFor(t, "the loop to reach the stub", func() bool { return fake.renewCount() >= 1 })

		// Stop cancels and then joins the loop, and the loop is parked in the
		// stub, so Stop cannot return until this goroutine's own signal below.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Stop()
		}()

		<-h.Done() // the cancellation has landed; only now may the stub answer
		close(answer)
		wg.Wait()

		waitDone(t, h, "the holder to finish")
		if !h.Fenced() {
			t.Error("Fenced() = false; a Stop that reached the holder first hid the fact that the device was lost")
		}
		if cause := context.Cause(h.Context()); !errors.Is(cause, ErrHolderStopped) {
			t.Errorf("cancellation cause = %v, want ErrHolderStopped; Stop won stopOnce", cause)
		}
	})

	// And the genuinely concurrent version, which is what actually happens in a
	// pod that is being drained at the moment its lease is revoked.
	t.Run("stop and fencing race", func(t *testing.T) {
		l := testLease()
		release := make(chan struct{})
		fake := &fakeOps{
			renewFn: func(context.Context, int) (RenewResult, error) {
				<-release
				return RenewResult{}, fenced(l.ID, l.Fence)
			},
		}
		h := startHolder(t, fake, l, quietConfig(time.Millisecond))

		waitFor(t, "the loop to reach the stub", func() bool { return fake.renewCount() >= 1 })

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(release)
			h.Stop()
		}()
		wg.Wait()

		waitDone(t, h, "the holder to finish")
		if !h.Fenced() {
			t.Error("Fenced() = false; a concurrent Stop hid the fact that the device was lost")
		}
	})
}

// TestHolderBoundsEachAttemptAndAWedgedOneIsTransient covers the connection
// that neither answers nor fails — a half-open socket to a database that went
// away without an RST, which is the control-plane twin of the ECONNRESET in
// DeviceFarmer/STF #663.
//
// Two things must hold, and they pull in opposite directions. The attempt has
// to be abandoned, or one wedged connection consumes renewal after renewal and
// the server hears nothing until the lease goes suspect. And abandoning it must
// not be mistaken for fencing, because the lease is untouched and the job is
// still running.
func TestHolderBoundsEachAttemptAndAWedgedOneIsTransient(t *testing.T) {
	l := testLease()

	var fencedHook, transientHook atomic.Int64
	cfg := quietConfig(time.Millisecond)
	cfg.RenewTimeout = 40 * time.Millisecond
	cfg.Hooks.OnFenced = func(Lease) { fencedHook.Add(1) }
	cfg.Hooks.OnTransientError = func(Lease, int, error, time.Duration) { transientHook.Add(1) }

	fake := &fakeOps{
		renewFn: func(ctx context.Context, _ int) (RenewResult, error) {
			// Never answers. The only way out is the holder's own deadline.
			<-ctx.Done()
			return RenewResult{}, fmt.Errorf("lease: renew %s: %w", l.ID, ctx.Err())
		},
	}
	h := startHolder(t, fake, l, cfg)

	waitFor(t, "three wedged attempts to be abandoned and retried", func() bool {
		return transientHook.Load() >= 3
	})

	// The job is untouched. context.DeadlineExceeded is not evidence that
	// somebody else owns the device.
	if err := h.Context().Err(); err != nil {
		t.Fatalf("the holder cancelled its context on a wedged renewal: %v (cause %v)",
			err, context.Cause(h.Context()))
	}
	if h.Fenced() {
		t.Fatal("Fenced() = true after wedged renewals; a half-open connection cost a device")
	}
	if got := fencedHook.Load(); got != 0 {
		t.Errorf("OnFenced fired %d times for wedged renewals, want 0", got)
	}
	if got := h.Stats().Renewals; got != 0 {
		t.Errorf("renewals = %d, want 0; nothing was ever acknowledged by the server", got)
	}

	// Every attempt carried a deadline of its own, inside RenewTimeout. This is
	// the half that keeps a wedged connection from turning into silence: without
	// it the attempt would inherit only the holder's context, which lives as
	// long as the job.
	for i, c := range fake.renewCalls() {
		if c.budget <= 0 {
			t.Fatalf("renewal %d was issued with no deadline of its own; one wedged connection would consume the whole renewal interval", i+1)
		}
		if c.budget > cfg.RenewTimeout {
			t.Fatalf("renewal %d had %s of budget, more than RenewTimeout %s", i+1, c.budget, cfg.RenewTimeout)
		}
	}
}

// TestHolderWitnessOnlyMovesTheDeadlineOutward covers the holder's cached copy
// of reclaimable_at against on-device evidence.
//
// The cache mirrors the server's GREATEST(): a witness may push a deadline out
// and may never pull one in. A refused witness changes nothing at all and is
// not fencing — only Renew reports that.
func TestHolderWitnessOnlyMovesTheDeadlineOutward(t *testing.T) {
	l := testLease()
	far := l.ReclaimableAt.Add(20 * time.Minute)

	// A cap deliberately unlike the package default, so that a holder which
	// dropped the configured value and let the store substitute its own default
	// is visibly wrong rather than accidentally right.
	const extensionCap = DefaultWitnessMaxExtensions - 9

	fake := &fakeOps{
		witnessFn: func(call int) (time.Time, bool, error) {
			switch call {
			case 1:
				return far, true, nil
			case 2:
				// The server clamped: a stale reading cannot pull the deadline back.
				return l.ReclaimableAt.Add(-time.Hour), true, nil
			default:
				// Refused: the extension cap is spent, or the fence is stale.
				return time.Time{}, false, nil
			}
		},
	}
	cfg := quietConfig(time.Hour) // no renewals; the witness is the subject
	cfg.WitnessMaxExtensions = extensionCap
	h := startHolder(t, fake, l, cfg)

	at, ok, err := h.Witness(t.Context())
	if err != nil || !ok {
		t.Fatalf("first witness: ok = %v, err = %v", ok, err)
	}
	if !at.Equal(far) {
		t.Errorf("witness reported %s, want %s", at, far)
	}
	if got := h.Lease().ReclaimableAt; !got.Equal(far) {
		t.Errorf("cached reclaimable_at = %s, want %s", got, far)
	}

	if _, _, err := h.Witness(t.Context()); err != nil {
		t.Fatalf("second witness: %v", err)
	}
	if got := h.Lease().ReclaimableAt; !got.Equal(far) {
		t.Errorf("cached reclaimable_at moved backwards to %s, want %s", got, far)
	}

	_, ok, err = h.Witness(t.Context())
	// Nil rather than errors.Is: being refused is not an error of any kind, so
	// asserting on the sentinel alone would let a future refactor return some
	// other error here and still pass.
	if err != nil {
		t.Fatalf("refused witness returned an error: %v; being refused costs a job nothing, and if "+
			"that error were %v the holder would abort a running job on it", err, ErrFenced)
	}
	if ok {
		t.Fatal("the refused witness was reported as accepted")
	}
	if h.Fenced() || h.Context().Err() != nil {
		t.Error("a refused witness ended the job")
	}

	// What the holder actually presented. Both values are matched server-side
	// and neither is inferable from the return value above: a witness at the
	// wrong fence is refused and buys the job nothing, and a maxExtensions of
	// zero has Store.Witness substitute DefaultWitnessMaxExtensions, so an
	// operator who tightened the cap would silently have tightened nothing.
	calls := fake.witnessCalls()
	if len(calls) != 3 {
		t.Fatalf("the store saw %d witness calls, want 3", len(calls))
	}
	for i, c := range calls {
		if c.leaseID != l.ID {
			t.Errorf("witness %d named lease %s, want %s", i+1, c.leaseID, l.ID)
		}
		if c.fence != l.Fence {
			t.Errorf("witness %d presented fence %d, want the fence the holder holds, %d", i+1, c.fence, l.Fence)
		}
		if c.maxExtensions != extensionCap {
			t.Errorf("witness %d presented maxExtensions %d, want the configured cap %d; the store would substitute its own default for a zero",
				i+1, c.maxExtensions, extensionCap)
		}
	}
}

// TestStubHolderIsAssembledLikeTheRealOne guards the one structural risk in
// this file.
//
// startHolder builds a Holder by hand so the loop can be driven by a stub, and
// a field that NewHolder starts setting but startHolder does not would leave
// every stub-driven test above exercising a subtly different object — most
// likely a nil one that panics, but possibly a zero value that quietly changes
// a branch. Comparing which fields are set, rather than trusting that somebody
// remembers to update both, is the difference between that claim being true and
// merely intended.
//
// Zero-ness is the comparison rather than equality because ids, contexts and
// channels are legitimately different between two holders; what must match is
// which fields a constructor is responsible for.
func TestStubHolderIsAssembledLikeTheRealOne(t *testing.T) {
	// An hour-long interval, so neither loop renews and neither touches the
	// mutex or the counters while they are being compared. The real holder is
	// pointed at a pool with nothing behind it: it is never asked a question.
	cfg := quietConfig(time.Hour)

	viaConstructor := NewHolder(t.Context(), NewStore(unreachablePool(t)), testLease(), cfg)
	t.Cleanup(viaConstructor.Stop)
	viaStub := startHolder(t, &fakeOps{}, testLease(), cfg)

	rv := reflect.ValueOf(viaConstructor).Elem()
	sv := reflect.ValueOf(viaStub).Elem()
	for i := range rv.NumField() {
		name := rv.Type().Field(i).Name
		if got, want := sv.Field(i).IsZero(), rv.Field(i).IsZero(); got != want {
			t.Errorf("Holder.%s: set by startHolder = %v, set by NewHolder = %v; "+
				"the stub-driven tests are exercising a differently assembled holder",
				name, !got, !want)
		}
	}
}

// =====================================================================
// End to end, against real SQL
// =====================================================================

// TestHolderAgainstPostgres runs the loop over a real Store and a real lease.
// It is the path the stub cannot cover: that the zero-rows verdict the SQL
// actually produces is the one the loop reads as fencing, and that an ordinary
// stop leaves a real row in farm.leases untouched.
func TestHolderAgainstPostgres(t *testing.T) {
	// One device per subtest: a released device is quarantined for its slot's
	// rearm window, so re-using one would make the next subtest race a timer
	// rather than test what it says it tests.
	f := newFixture(t, 6)

	t.Run("renews a live lease", func(t *testing.T) {
		ctx := t.Context()
		_, l := f.acquire(t)

		h := NewHolder(ctx, f.store, l, quietConfig(20*time.Millisecond))
		t.Cleanup(h.Stop)

		waitFor(t, "three real renewals", func() bool { return h.Stats().Renewals >= 3 })
		if err := h.Context().Err(); err != nil {
			t.Fatalf("context died while Postgres was healthy: %v", err)
		}

		var seq int64
		var state string
		if err := f.pool.QueryRow(ctx,
			`SELECT heartbeat_seq, state FROM farm.leases WHERE id = $1::uuid`, l.ID).
			Scan(&seq, &state); err != nil {
			t.Fatalf("read lease: %v", err)
		}
		if seq < 3 {
			t.Errorf("heartbeat_seq = %d after three renewals, want at least 3", seq)
		}
		if state != "held" {
			t.Errorf("lease state = %q, want held", state)
		}
	})

	t.Run("stop leaves the lease held", func(t *testing.T) {
		ctx := t.Context()
		_, l := f.acquire(t)

		h := NewHolder(ctx, f.store, l, quietConfig(20*time.Millisecond))
		waitFor(t, "a real renewal", func() bool { return h.Stats().Renewals >= 1 })
		h.Stop()

		if state, reason := f.leaseState(t, l.ID); state != "held" || reason != nil {
			t.Fatalf("after Stop the lease is state %q reason %v, want held and no reason", state, reason)
		}
		// The replacement pod's view: the lease is still there, at the same
		// fence, ready to be re-attached by job id.
		if _, err := f.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance); err != nil {
			t.Errorf("renew after Stop: %v; the evicted pod took the device with it", err)
		}
	})

	t.Run("an operator revoke fences the holder", func(t *testing.T) {
		ctx := t.Context()
		_, l := f.acquire(t)

		h := NewHolder(ctx, f.store, l, quietConfig(20*time.Millisecond))
		t.Cleanup(h.Stop)
		waitFor(t, "a real renewal", func() bool { return h.Stats().Renewals >= 1 })

		// A human takes the device back. This is one of the three endings the
		// invariant permits, and the holder must find out on its next renewal.
		released, err := f.store.Release(ctx, l.ID, l.Fence, ReasonOperatorRevoked, time.Second)
		if err != nil || !released {
			t.Fatalf("operator release: released = %v, err = %v", released, err)
		}

		waitDone(t, h, "the holder to notice it was fenced")
		if !h.Fenced() {
			t.Error("Fenced() = false after the lease was revoked underneath the holder")
		}
		if err := h.Err(); !errors.Is(err, ErrFenced) {
			t.Errorf("Err() = %v, want it to wrap ErrFenced", err)
		}
	})

	t.Run("release ends the lease", func(t *testing.T) {
		ctx := t.Context()
		_, l := f.acquire(t)

		h := NewHolder(ctx, f.store, l, quietConfig(20*time.Millisecond))
		waitFor(t, "a real renewal", func() bool { return h.Stats().Renewals >= 1 })

		released, err := h.Release(h.Context(), ReasonCompleted, time.Second)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if !released {
			t.Fatal("release reported that it changed nothing")
		}

		state, reason := f.leaseState(t, l.ID)
		if state != "released" {
			t.Errorf("lease state = %q, want released", state)
		}
		if reason == nil || *reason != string(ReasonCompleted) {
			t.Errorf("release_reason = %v, want %q", reason, ReasonCompleted)
		}
		if h.Fenced() {
			t.Error("Fenced() = true after the holder released its own lease")
		}
		if cause := context.Cause(h.Context()); !errors.Is(cause, ErrHolderReleased) {
			t.Errorf("cancellation cause = %v, want ErrHolderReleased", cause)
		}
	})

	t.Run("a connectivity reason is refused and the job keeps its device", func(t *testing.T) {
		ctx := t.Context()
		_, l := f.acquire(t)

		h := NewHolder(ctx, f.store, l, quietConfig(20*time.Millisecond))
		t.Cleanup(h.Stop)
		waitFor(t, "a real renewal", func() bool { return h.Stats().Renewals >= 1 })

		// The holder has already stopped renewing by the time the database
		// refuses the reason, so this lease is now the reaper's problem rather
		// than a live one — but the row must still say the job held it, and it
		// must carry no release reason at all.
		_, err := h.Release(h.Context(), ReleaseReason("device_offline"), time.Second)
		var check *CheckViolationError
		if !errors.As(err, &check) {
			t.Fatalf("release with a connectivity reason: err = %v, want *CheckViolationError", err)
		}
		if state, reason := f.leaseState(t, l.ID); state != "held" || reason != nil {
			t.Errorf("lease is state %q reason %v, want held and no reason", state, reason)
		}
	})
}
