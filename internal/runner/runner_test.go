package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// The invariant every test in this package is written against:
//
//	A lease ends when the job says so, when a deadline the user wrote down
//	elapses, or when a human takes it back. NOTHING ELSE.
//
// DeviceFarmer/STF issue #663 releases a device mid-run after a ~90-minute
// ECONNRESET and destroys multi-hour work. The tests below exist to make the
// same mistake impossible here, so each one names the behaviour it protects
// rather than the function it calls.

// testTimeout bounds a test that has gone wrong. Nothing on a healthy path
// waits for it.
const testTimeout = 30 * time.Second

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// logCapture keeps a runner's log lines so a test can assert on the one line an
// operator is meant to read at 3am. slog.Handler is four methods, so a
// hand-rolled one is cheaper than a dependency.
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

func (c *logCapture) logger() *slog.Logger { return slog.New(c) }

// count returns how many captured lines contain substr.
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

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeConn is runner.Conn without a phone, a hub or an adb server.
//
// Conn is three methods precisely so this is possible: the branch that decides
// "retry inside the lease" versus "fail the step" is the most consequential one
// in the package, and a branch that needs hardware is a branch nobody tests.
type fakeConn struct {
	mu sync.Mutex

	// shell answers one command. nil means "exit 0, no output".
	shell func(ctx context.Context, call int, command string) (ShellOutput, error)
	push  func(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error
	pull  func(ctx context.Context, remote string, w io.Writer) error

	cmds []string
}

func (f *fakeConn) Shell(ctx context.Context, command string) (ShellOutput, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, command)
	n := len(f.cmds)
	fn := f.shell
	f.mu.Unlock()

	if fn == nil {
		return ShellOutput{Exited: true}, nil
	}
	return fn(ctx, n, command)
}

func (f *fakeConn) Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error {
	if f.push == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	return f.push(ctx, r, remote, mode)
}

func (f *fakeConn) Pull(ctx context.Context, remote string, w io.Writer) error {
	if f.pull == nil {
		return nil
	}
	return f.pull(ctx, remote, w)
}

func (f *fakeConn) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeConn) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmds)
}

// okShell is the shell answer of a device that ran the command and exited 0.
func okShell(stdout string) ShellOutput {
	return ShellOutput{Stdout: []byte(stdout), Exited: true}
}

// fakeHolder is the two-method view of a lease the runner is allowed to have.
type fakeHolder struct {
	ctx    context.Context
	fenced bool
}

func (h fakeHolder) Context() context.Context { return h.ctx }
func (h fakeHolder) Fenced() bool             { return h.fenced }

var _ Holder = fakeHolder{}

// testRunner builds a Runner without New, which requires a pool. Every test in
// this package exercises decision-making rather than bookkeeping, and a runner
// that never records anything needs no database to decide correctly.
func testRunner(tb testing.TB, mutate func(*Config)) (*Runner, *logCapture) {
	tb.Helper()

	logs := &logCapture{}
	cfg := Config{
		Logger:    logs.logger(),
		RetryBase: time.Millisecond,
		RetryMax:  4 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.applyDefaults()
	return &Runner{cfg: cfg, log: cfg.Logger}, logs
}

// transportErr is what a broken wire looks like coming out of a Conn: an
// ordinary, unclassified Go error. The runner's default for one of these is to
// retry, and that default is the invariant.
func transportErr(msg string) error { return errors.New(msg) }

// ---------------------------------------------------------------------------
// 1. A transport failure inside a step is retried INSIDE the lease
// ---------------------------------------------------------------------------

// A step whose ADB call fails is retried while the job still holds the device.
// The retry must be silent as far as the lease is concerned and loud in the
// log, because the repeated line is what an operator reads when a hub flaps —
// and its absence from the lease log is the proof that the flapping cost the
// job nothing.
func TestTransportFailureIsRetriedInsideTheLeaseAndDoesNotFailTheJob(t *testing.T) {
	t.Parallel()

	r, logs := testRunner(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	const failures = 5
	calls := 0
	retries, err := r.retry(ctx, r.log, "step-under-test", func(context.Context) error {
		calls++
		if calls <= failures {
			return transportErr("read tcp 127.0.0.1:5037: connection reset by peer")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry returned %v; a transport failure must not end the step", err)
	}
	if retries != failures {
		t.Fatalf("retries = %d, want %d", retries, failures)
	}
	if calls != failures+1 {
		t.Fatalf("fn called %d times, want %d", calls, failures+1)
	}
	if ctx.Err() != nil {
		t.Fatal("the run context was cancelled by a transport failure")
	}
	// The line an operator reads. It says INSIDE the lease and NOT failed on
	// purpose; a retry that is invisible in the log is a farm nobody can debug.
	if got := logs.count("retrying INSIDE the lease (job NOT failed)"); got != failures {
		t.Fatalf("logged the retry line %d times, want %d", got, failures)
	}
}

// The retry loop has no attempt cap, deliberately: the bound is the step's own
// timeout, which the user wrote down. A hard-coded "three tries" would end a
// job because a USB hub took four seconds too long to come back. This proves
// the bound exists and that it is the step's deadline.
func TestRetryIsBoundedByTheStepsOwnTimeoutAndNothingElse(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	parent, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// The step's own budget, exactly as execute derives it.
	step, cancelStep := context.WithTimeoutCause(parent, 80*time.Millisecond, ErrStepTimeout)
	defer cancelStep()

	start := time.Now()
	calls := 0
	retries, err := r.retry(step, r.log, "forever", func(context.Context) error {
		calls++
		return transportErr("device offline")
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStepTimeout) {
		t.Fatalf("retry ended with %v, want ErrStepTimeout: only the step's own clock may stop it", err)
	}
	if calls < 2 || retries < 1 {
		t.Fatalf("gave up after %d call(s), %d retries; the loop must keep trying inside the step", calls, retries)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("gave up after %s, well before the step's 80ms budget", elapsed)
	}
	if parent.Err() != nil {
		t.Fatal("a step timeout cancelled the RUN context; only the step should have ended")
	}
}

// A protocol refusal is not the wire. Retrying it burns the step's budget to
// arrive at the same answer, so it stops the loop at once.
func TestRetryStopsAtANonRetryableError(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	calls := 0
	retries, err := r.retry(context.Background(), r.log, "refused", func(context.Context) error {
		calls++
		return NotRetryable(errors.New("pm: unknown package"))
	})
	if !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("err = %v, want it to wrap ErrNotRetryable", err)
	}
	if !strings.Contains(err.Error(), "unknown package") {
		t.Fatalf("err = %v, want the underlying cause preserved", err)
	}
	if calls != 1 || retries != 0 {
		t.Fatalf("calls = %d, retries = %d; want 1 and 0", calls, retries)
	}
}

// Losing the lease is the one error that must never be retried: the device
// belongs to somebody else now and must not be written to at all.
func TestRetryNeverRetriesFencing(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	calls := 0
	retries, err := r.retry(context.Background(), r.log, "fenced", func(context.Context) error {
		calls++
		return fmt.Errorf("lease 8ab1: %w", lease.ErrFenced)
	})
	if !errors.Is(err, lease.ErrFenced) {
		t.Fatalf("err = %v, want ErrFenced returned unchanged", err)
	}
	if calls != 1 || retries != 0 {
		t.Fatalf("wrote to a device we no longer hold: calls = %d, retries = %d", calls, retries)
	}
}

// A run that ends while a retry is sleeping reports the CAUSE, not a bare
// context error: the cause is what classifyAbort branches on, and the three
// causes call for three different endings.
func TestRetryReportsTheAbortCauseWhenTheRunEndsDuringBackoff(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, func(c *Config) {
		c.RetryBase = 5 * time.Second // long enough that we are certainly asleep
		c.RetryMax = 5 * time.Second
	})

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	done := make(chan error, 1)
	go func() {
		_, err := r.retry(ctx, r.log, "sleeping", func(context.Context) error {
			return transportErr("broken pipe")
		})
		done <- err
	}()

	// Let the first attempt fail and the backoff timer start.
	time.Sleep(20 * time.Millisecond)
	cancel(fmt.Errorf("lease 9cd2: %w", lease.ErrFenced))

	select {
	case err := <-done:
		if !errors.Is(err, lease.ErrFenced) {
			t.Fatalf("retry returned %v, want the fencing cause", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("retry did not notice the cancellation while backing off")
	}
}

// Backoff is bounded and jittered. The jitter matters at fleet scale: an adb
// server restart hits every runner on the host at once, and unjittered retries
// would keep hitting it in lockstep exactly while it tries to come back.
func TestBackoffStaysInsideItsWindowAndVaries(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, func(c *Config) {
		c.RetryBase = 10 * time.Millisecond
		c.RetryMax = 80 * time.Millisecond
	})

	seen := make(map[time.Duration]bool)
	for attempt := range 200 {
		d := r.backoff(attempt) // includes attempt 0, which must not underflow
		if d <= 0 || d > r.cfg.RetryMax {
			t.Fatalf("backoff(%d) = %s, outside (0, %s]", attempt, d, r.cfg.RetryMax)
		}
		if attempt >= 20 {
			// Long past the cap: the window is the top half of RetryMax.
			if d < r.cfg.RetryMax/2 {
				t.Fatalf("backoff(%d) = %s, below half the cap", attempt, d)
			}
			seen[d] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("backoff produced %d distinct capped delays; it is not jittered", len(seen))
	}
}

// A cap below the floor is not a value to honour: it would make backoff return
// a delay shorter than the floor the same operator asked for.
func TestConfigDefaultsNeverProduceACapBelowTheFloor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"zero", Config{}},
		{"cap unset", Config{RetryBase: 2 * time.Second}},
		{"floor raised past the default cap", Config{RetryBase: 90 * time.Second}},
		{"cap below floor", Config{RetryBase: 5 * time.Second, RetryMax: time.Second}},
		{"negatives", Config{RetryBase: -1, RetryMax: -1, StepTimeout: -1, MaxOutput: -1, CallTimeout: -1, PollInterval: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.applyDefaults()
			if cfg.RetryMax < cfg.RetryBase {
				t.Fatalf("RetryMax %s < RetryBase %s", cfg.RetryMax, cfg.RetryBase)
			}
			if cfg.StepTimeout <= 0 || cfg.PollInterval <= 0 || cfg.CallTimeout <= 0 || cfg.MaxOutput <= 0 {
				t.Fatalf("applyDefaults left a non-positive bound: %+v", cfg)
			}
			if cfg.WorkRoot == "" || cfg.Logger == nil {
				t.Fatalf("applyDefaults left WorkRoot or Logger empty: %+v", cfg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. ONLY lease.ErrFenced is fencing
// ---------------------------------------------------------------------------

// Everything that is not a deliberate refusal is transport noise and is
// retried. This is the default that keeps a cable, a hub or an adb server
// restart from ending a six-hour job — and every entry below must also be
// something errors.Is refuses to call fencing.
func TestOnlyFencingIsFencingAndEverythingElseIsRetried(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want bool // isRetryable
	}{
		{"a cancelled context from inside the transport", context.Canceled, true},
		{"a dial deadline from inside the transport", context.DeadlineExceeded, true},
		{"a closed connection pool", errors.New("closed pool"), true},
		{"a dial failure", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"an unexpected EOF mid-frame", io.ErrUnexpectedEOF, true},
		{"an unclassified device error", errors.New("device offline"), true},
		{"a wrapped transport failure", fmt.Errorf("shell: %w", io.ErrUnexpectedEOF), true},

		{"a protocol refusal", NotRetryable(errors.New("no such package")), false},
		{"the step's own timeout", fmt.Errorf("step: %w", ErrStepTimeout), false},
		{"the job's max_runtime", fmt.Errorf("run: %w", ErrMaxRuntime), false},
		{"losing the lease", fmt.Errorf("lease 1: %w", lease.ErrFenced), false},
		{"no error at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Fatalf("isRetryable(%v) = %t, want %t", tc.err, got, tc.want)
			}
			// The second half, and the one that matters most: nothing above
			// except the lease error may be MISTAKEN for fencing. A false
			// positive here abandons a run that still owns its device; a false
			// negative writes to a device somebody else now holds.
			wantFenced := tc.name == "losing the lease"
			if got := errors.Is(tc.err, lease.ErrFenced); got != wantFenced {
				t.Fatalf("errors.Is(%v, lease.ErrFenced) = %t, want %t", tc.err, got, wantFenced)
			}
		})
	}
}

// An adapter may say more than "this failed" by implementing Retryable() bool.
func TestRetryableInterfaceOverridesTheDefault(t *testing.T) {
	t.Parallel()

	if isRetryable(retryableErr{false}) {
		t.Fatal("an error that says Retryable() == false was retried")
	}
	if !isRetryable(retryableErr{true}) {
		t.Fatal("an error that says Retryable() == true was not retried")
	}
	if !isRetryable(fmt.Errorf("wrapped: %w", retryableErr{true})) {
		t.Fatal("errors.As did not reach a wrapped Retryable()")
	}
}

type retryableErr struct{ ok bool }

func (e retryableErr) Error() string   { return fmt.Sprintf("scripted retryable=%t", e.ok) }
func (e retryableErr) Retryable() bool { return e.ok }

// NotRetryable(nil) must stay nil: a helper that manufactures an error out of
// success would fail steps that worked.
func TestNotRetryableOfNilIsNil(t *testing.T) {
	t.Parallel()
	if err := NotRetryable(nil); err != nil {
		t.Fatalf("NotRetryable(nil) = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// The three endings, which must never be collapsed
// ---------------------------------------------------------------------------

// classifyAbort turns a cancelled run context into an ending. Fencing abandons
// and releases NOTHING; max_runtime fails and releases with the user's own
// reason; anything else abandons and releases nothing either. Swapping any two
// of these is DeviceFarmer/STF #663.
func TestClassifyAbortKeepsTheThreeEndingsApart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		cause         error
		holderFenced  bool
		wantState     State
		wantFenced    bool
		wantRelease   lease.ReleaseReason
		wantPermanent bool
	}{
		{
			name:        "the lease was fenced",
			cause:       fmt.Errorf("lease 7: %w", lease.ErrFenced),
			wantState:   StateAbandoned,
			wantFenced:  true,
			wantRelease: "", // releasing would take the device from its new holder
		},
		{
			name: "the holder knows it was fenced even when another cause won the race",
			// A Stop or a Release landing microseconds before the loop notices
			// the fencing sets a different cause. The holder's own record is
			// the tie-breaker, and it must win.
			cause:        context.Canceled,
			holderFenced: true,
			wantState:    StateAbandoned,
			wantFenced:   true,
			wantRelease:  "",
		},
		{
			name:          "the job's max_runtime elapsed",
			cause:         ErrMaxRuntime,
			wantState:     StateFailed,
			wantRelease:   lease.ReasonMaxRuntime,
			wantPermanent: true,
		},
		{
			name:        "SIGTERM, a node drain, a preemption",
			cause:       context.Canceled,
			wantState:   StateAbandoned,
			wantRelease: "", // the replacement process re-attaches to this lease
		},
		{
			name:        "the database went away underneath us",
			cause:       errors.New("closed pool"),
			wantState:   StateAbandoned,
			wantRelease: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testRunner(t, nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(tc.cause)

			h := fakeHolder{ctx: ctx, fenced: tc.holderFenced}
			var out Outcome
			res := r.classifyAbort(ctx, h, r.log, context.Cause(ctx), &out, "step 3")

			if out.State != tc.wantState {
				t.Fatalf("State = %q, want %q", out.State, tc.wantState)
			}
			if out.Fenced != tc.wantFenced {
				t.Fatalf("Fenced = %t, want %t", out.Fenced, tc.wantFenced)
			}
			if out.ReleaseReason != tc.wantRelease {
				t.Fatalf("ReleaseReason = %q, want %q", out.ReleaseReason, tc.wantRelease)
			}
			if res.permanent != tc.wantPermanent {
				t.Fatalf("permanent = %t, want %t", res.permanent, tc.wantPermanent)
			}
			if out.Error == "" {
				t.Fatal("no reason recorded; the attempt row would say nothing")
			}
			if !strings.Contains(out.Error, "step 3") {
				t.Fatalf("Error = %q, want it to name where the run stopped", out.Error)
			}
		})
	}
}

// abortErr reports the cause only once the context has actually ended, and
// isAbort keeps "the run was ended" separate from "this step ended badly" — a
// step timeout leaves the run context live.
func TestAbortErrAndIsAbortSeparateTheRunFromTheStep(t *testing.T) {
	t.Parallel()

	run, cancelRun := context.WithCancelCause(context.Background())
	defer cancelRun(nil)

	if abortErr(run) != nil || isAbort(run) {
		t.Fatal("a live run context reported an abort")
	}

	step, cancelStep := context.WithTimeoutCause(run, time.Millisecond, ErrStepTimeout)
	defer cancelStep()
	<-step.Done()

	if isAbort(run) {
		t.Fatal("a step timeout was mistaken for the run ending")
	}
	if !errors.Is(abortErr(step), ErrStepTimeout) {
		t.Fatalf("abortErr(step) = %v, want ErrStepTimeout", abortErr(step))
	}

	cancelRun(ErrMaxRuntime)
	if !isAbort(run) || !errors.Is(abortErr(run), ErrMaxRuntime) {
		t.Fatalf("abortErr(run) = %v, want ErrMaxRuntime", abortErr(run))
	}
}

// ---------------------------------------------------------------------------
// Placement and construction
// ---------------------------------------------------------------------------

// Work is addressed by USB position and never by serial: duplicate OEM serials
// are real, and a serial-addressed command can land on a healthy device three
// hours into somebody else's run. A placement with no devpath is refused.
func TestPlacementRefusesAnythingItCannotAddressByPosition(t *testing.T) {
	t.Parallel()

	full := Placement{JobID: "j", DeviceID: "d", Devpath: "usb:3-1.4", Fence: 7}
	if err := full.validate(); err != nil {
		t.Fatalf("a complete placement was refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		want string
		mut  func(*Placement)
	}{
		{"no job", "job id", func(p *Placement) { p.JobID = "" }},
		{"no device", "device id", func(p *Placement) { p.DeviceID = "" }},
		{"no devpath", "never by serial", func(p *Placement) { p.Devpath = "" }},
		{"no fence", "fence", func(p *Placement) { p.Fence = 0 }},
		{"a fence below the sequence floor", "fence", func(p *Placement) { p.Fence = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := full
			tc.mut(&p)
			err := p.validate()
			if err == nil {
				t.Fatalf("validate accepted %+v", p)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A runner that cannot record is a runner nobody can debug at 3am.
func TestNewRefusesARunnerWithNoDatabase(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Logger: quietLogger()}); err == nil {
		t.Fatal("New accepted a Config with no pool")
	}
}

// Run refuses work it could not have cancelled, and work it cannot address.
// Each of these returns before the database is touched, which is why a runner
// with no pool can prove it.
func TestRunRefusesUncancellableOrUnaddressableWork(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	ctx := context.Background()
	good := Placement{JobID: "j", DeviceID: "d", Devpath: "usb:1-1", Fence: 1}

	if _, err := r.Run(ctx, nil, good, &fakeConn{}); err == nil {
		t.Fatal("Run accepted a nil holder; it must not run work it cannot have cancelled")
	}
	h := fakeHolder{ctx: ctx}
	if _, err := r.Run(ctx, h, good, nil); err == nil {
		t.Fatal("Run accepted a nil device connection")
	}
	if _, err := r.Run(ctx, h, Placement{JobID: "j", DeviceID: "d", Fence: 1}, &fakeConn{}); err == nil {
		t.Fatal("Run accepted a placement with no devpath")
	}
}

// A non-nil error from Run means the runner could not do its own bookkeeping —
// the database was unreachable, the job does not exist. It does NOT mean the
// job failed, and the Outcome must name no release reason, so a caller that
// releases only on a named reason cannot turn an infrastructure blip into a
// lost device.
func TestABookkeepingFailureIsNeverAVerdictOnTheJob(t *testing.T) {
	t.Parallel()

	// A real pool that will never connect: every statement fails on the wire.
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("building the unreachable pool: %v", err)
	}
	defer pool.Close()

	r, err := New(Config{Pool: pool, Logger: quietLogger(), CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	dev := &fakeConn{}
	out, err := r.Run(ctx, fakeHolder{ctx: ctx},
		Placement{JobID: "11111111-1111-4111-8111-111111111111", DeviceID: "d", Devpath: "usb:3-1.4", Fence: 7},
		dev)

	if err == nil {
		t.Fatal("Run reported success against a database it could not reach")
	}
	if out.ReleaseReason != "" {
		t.Fatalf("ReleaseReason = %q; a database blip would cost the job its device", out.ReleaseReason)
	}
	if out.Fenced || out.State != "" {
		t.Fatalf("out = %+v, want no verdict at all", out)
	}
	if dev.calls() != 0 {
		t.Fatalf("the device was touched %d time(s) before the job was even read", dev.calls())
	}
}

// The step's budget is the job author's number, not this binary's constant.
func TestStepTimeoutPrefersTheSpecOverTheDefault(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, func(c *Config) { c.StepTimeout = 7 * time.Minute })
	spec := jobspec.Spec{DefaultTimeout: jobspec.Duration(2 * time.Minute)}
	own := jobspec.Step{ID: "a", Timeout: jobspec.Duration(90 * time.Second)}
	inherited := jobspec.Step{ID: "b"}

	if got := r.stepTimeout(spec, own); got != 90*time.Second {
		t.Fatalf("stepTimeout(own) = %s, want 90s", got)
	}
	if got := r.stepTimeout(spec, inherited); got != 2*time.Minute {
		t.Fatalf("stepTimeout(inherited) = %s, want the spec default", got)
	}
	if got := r.stepTimeout(jobspec.Spec{}, inherited); got != 7*time.Minute {
		t.Fatalf("stepTimeout(no spec value) = %s, want the runner default", got)
	}
}

// The database owns the step vocabulary. A runner whose migrations are behind
// refuses the job instead of half-executing it — and it refuses BEFORE the
// device is touched, which is the whole point of asking.
func TestUnknownStepKindIsRefusedAgainstTheDatabaseVocabulary(t *testing.T) {
	t.Parallel()

	spec := jobspec.New(
		jobspec.Step{ID: "one", Payload: jobspec.Shell{Command: "true"}},
		jobspec.Step{ID: "two", Payload: jobspec.Sleep{Duration: jobspec.Duration(time.Second)}},
	)
	full := map[jobspec.Kind]kindInfo{
		jobspec.KindShell: {},
		jobspec.KindSleep: {Idempotent: true},
	}
	if err := checkKindsAgainstSchema(spec, full); err != nil {
		t.Fatalf("a spec whose kinds are all known was refused: %v", err)
	}

	behind := map[jobspec.Kind]kindInfo{jobspec.KindShell: {}}
	err := checkKindsAgainstSchema(spec, behind)
	if err == nil {
		t.Fatal("a spec naming a kind the database does not know was accepted")
	}
	if !strings.Contains(err.Error(), "farm.step_kinds") || !strings.Contains(err.Error(), "two") {
		t.Fatalf("error = %q, want it to name the step and the table", err)
	}
}

// There is exactly one executor arm per row of farm.step_kinds, and no
// eleventh. A kind the vocabulary knows and the dispatch table does not would
// be a step that fails after the job was accepted.
func TestEveryVocabularyKindHasAnExecutor(t *testing.T) {
	t.Parallel()

	for _, info := range jobspec.Kinds() {
		if _, ok := executorFor(info.Kind); !ok {
			t.Fatalf("farm.step_kinds knows %q but the runner has no executor for it", info.Kind)
		}
	}
	if _, ok := executorFor(jobspec.Kind("teleport")); ok {
		t.Fatal("executorFor invented an executor for a kind that does not exist")
	}
}
