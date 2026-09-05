package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// The step executors, driven by a fake Conn.
//
// The distinction every test below turns on is the one the package is built
// around: an ERROR means the attempt to run the step did not complete, which
// usually means the wire, and the wire is retried inside the lease. A Failure
// means the step ran and the device said no, which no retry will change. Get
// those the wrong way round and a bumped cable becomes a failed job — which is
// DeviceFarmer/STF #663, one step at a time.

func testEnv(dev Conn, mutate func(*env)) *env {
	e := &env{
		dev:       dev,
		log:       quietLogger(),
		place:     placement(),
		attempt:   1,
		workDir:   "/data/local/tmp/device-farmer/job-1",
		detach:    "nohup setsid",
		maxOutput: 4096,
		poll:      time.Millisecond,
		callTO:    time.Second,
	}
	if mutate != nil {
		mutate(e)
	}
	return e
}

func stepCtx(tb testing.TB) context.Context {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	tb.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// shell
// ---------------------------------------------------------------------------

func TestShellStepSeparatesAVerdictFromTheWire(t *testing.T) {
	t.Parallel()

	st := jobspec.Step{ID: "run", Payload: jobspec.Shell{Command: "am instrument -w com.example/.Runner"}}

	t.Run("the command ran and passed", func(t *testing.T) {
		e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return ShellOutput{Stdout: []byte("OK (12 tests)\n"), Exited: true}, nil
		}}, nil)
		res, err := execShell(stepCtx(t), e, st)
		if err != nil {
			t.Fatalf("execShell: %v", err)
		}
		if res.Failure != "" {
			t.Fatalf("Failure = %q, want none", res.Failure)
		}
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("ExitCode = %v, want 0", res.ExitCode)
		}
		if !strings.Contains(res.Output, "OK (12 tests)") {
			t.Fatalf("Output = %q", res.Output)
		}
	})

	t.Run("the device said no", func(t *testing.T) {
		e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return ShellOutput{Stdout: []byte("FAILURES!!!\n"), ExitCode: 1, Exited: true}, nil
		}}, nil)
		res, err := execShell(stepCtx(t), e, st)
		if err != nil {
			t.Fatalf("a non-zero exit was reported as an error, so it would be RETRIED: %v", err)
		}
		if res.Failure == "" {
			t.Fatal("a failing command produced no Failure")
		}
		if !strings.Contains(res.Failure, "exit status 1") {
			t.Fatalf("Failure = %q, want it to name the status", res.Failure)
		}
	})

	t.Run("the spec decides which exits are a success", func(t *testing.T) {
		e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return ShellOutput{ExitCode: 7, Exited: true}, nil
		}}, func(e *env) { e.spec = jobspec.Spec{DefaultExpectExit: []int{0, 7}} })
		res, err := execShell(stepCtx(t), e, st)
		if err != nil {
			t.Fatalf("execShell: %v", err)
		}
		if res.Failure != "" {
			t.Fatalf("Failure = %q; the spec declared 7 a success", res.Failure)
		}
	})

	// The case that matters most. ExitCode is zero because NOTHING SET IT: the
	// stream ended before the device reported a status. Reading that as
	// "exited 0" is how a bumped cable becomes a green test run.
	t.Run("a stream that ended without an exit status is the wire, not a pass", func(t *testing.T) {
		e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return ShellOutput{Stdout: []byte("half of the out")}, nil
		}}, nil)
		res, err := execShell(stepCtx(t), e, st)
		if err == nil {
			t.Fatalf("a truncated stream was reported as success: %+v", res)
		}
		if !strings.Contains(err.Error(), "without an exit status") {
			t.Fatalf("err = %v, want it to name the missing exit status", err)
		}
		if !isRetryable(err) {
			t.Fatal("a truncated stream was marked permanent; the step would fail instead of reconnecting")
		}
	})

	t.Run("a socket error travels back unwrapped so it is retried", func(t *testing.T) {
		want := transportErr("write tcp: broken pipe")
		e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return ShellOutput{}, want
		}}, nil)
		_, err := execShell(stepCtx(t), e, st)
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want the transport error itself", err)
		}
		if !isRetryable(err) {
			t.Fatal("a socket error was marked permanent")
		}
	})
}

// ---------------------------------------------------------------------------
// wait_for — a poll that fails on the wire is COUNTED AND IGNORED
// ---------------------------------------------------------------------------

// This is the whole point of asking the device about a file the device wrote:
// the connection may come and go while the answer sits there waiting to be
// read. The only thing that ends the wait unsatisfied is the condition's own
// timeout, which the spec wrote down.
func TestWaitForIgnoresTransportBlipsAndKeepsWaiting(t *testing.T) {
	t.Parallel()

	const blips = 6
	dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
		if call <= blips {
			return ShellOutput{}, transportErr("device offline")
		}
		return ShellOutput{Stdout: []byte("done\n"), Exited: true}, nil
	}}
	e := testEnv(dev, nil)
	st := jobspec.Step{ID: "wait", Payload: jobspec.WaitFor{
		Probe:    "[ -f /data/local/tmp/soak.rc ]",
		Interval: jobspec.Duration(time.Millisecond),
		Timeout:  jobspec.Duration(10 * time.Second),
	}}

	res, err := execWaitFor(stepCtx(t), e, st)
	if err != nil {
		t.Fatalf("execWaitFor ended on a transport failure: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("Failure = %q; the condition was met", res.Failure)
	}
	if got := res.Detail["poll_blips"]; got != blips {
		t.Fatalf("poll_blips = %v, want %d: the blips must be counted, not fatal", got, blips)
	}
	if got := res.Detail["polls"]; got != blips+1 {
		t.Fatalf("polls = %v, want %d", got, blips+1)
	}
}

// A condition that never becomes true is a VERDICT about the device, not a
// transport failure, so it comes back as a Failure and is not retried.
func TestWaitForReportsAnUnmetConditionAsAVerdict(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{Stdout: []byte("not yet\n"), ExitCode: 1, Exited: true}, nil
	}}
	e := testEnv(dev, nil)
	st := jobspec.Step{ID: "wait", Payload: jobspec.WaitFor{
		Probe:    "[ -f /data/local/tmp/soak.rc ]",
		Interval: jobspec.Duration(time.Millisecond),
		Timeout:  jobspec.Duration(40 * time.Millisecond),
	}}

	res, err := execWaitFor(stepCtx(t), e, st)
	if err != nil {
		t.Fatalf("an unmet condition was reported as an error: %v", err)
	}
	if !strings.Contains(res.Failure, "condition not met within") {
		t.Fatalf("Failure = %q", res.Failure)
	}
	// The operator reading this row has to be able to tell "the condition never
	// became true" from "we could barely reach the device".
	if !strings.Contains(res.Failure, e2eProbe(st)) {
		t.Fatalf("Failure = %q, want the probe quoted so it can be run by hand", res.Failure)
	}
}

// A wait spent entirely unable to reach the device still ends as a verdict —
// with the blip count in it, because "the condition never became true" and "we
// could barely reach the device for the whole wait" are two completely
// different things to go and look at.
func TestWaitForSaysWhenItCouldNotReachTheDeviceAtAll(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{}, transportErr("connection reset by peer")
	}}
	e := testEnv(dev, nil)
	st := jobspec.Step{ID: "wait", Payload: jobspec.WaitFor{
		Probe:    "[ -f /done ]",
		Interval: jobspec.Duration(time.Millisecond),
		Timeout:  jobspec.Duration(40 * time.Millisecond),
	}}

	res, err := execWaitFor(stepCtx(t), e, st)
	if err != nil {
		t.Fatalf("execWaitFor: %v", err)
	}
	if !strings.Contains(res.Failure, "could not reach the device") {
		t.Fatalf("Failure = %q, want the blip count spelled out", res.Failure)
	}
	if got, ok := res.Detail["poll_blips"].(int); !ok || got == 0 {
		t.Fatalf("poll_blips = %v, want the blips counted", res.Detail["poll_blips"])
	}
}

// Losing the lease ends the wait immediately and with the fencing cause, not
// with a verdict about the device: the device is not ours to have an opinion
// about any more.
func TestWaitForStopsAtOnceWhenTheStepIsEnded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrMaxRuntime)

	dev := &fakeConn{shell: func(c context.Context, _ int, _ string) (ShellOutput, error) {
		return ShellOutput{}, c.Err()
	}}
	e := testEnv(dev, nil)
	st := jobspec.Step{ID: "wait", Payload: jobspec.WaitFor{
		Probe:    "[ -f /done ]",
		Interval: jobspec.Duration(time.Millisecond),
		Timeout:  jobspec.Duration(10 * time.Second),
	}}

	_, err := execWaitFor(ctx, e, st)
	if !errors.Is(err, ErrMaxRuntime) {
		t.Fatalf("err = %v, want the run's own cause", err)
	}
}

// A probe that answers while the condition's deadline is expiring is still an
// answer. Testing the deadline first would report "condition not met" about a
// device that had in fact got there — the one wrong verdict a wait_for can
// produce.
func TestWaitForHonoursAProbeThatAnsweredAtTheDeadline(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(c context.Context, _ int, _ string) (ShellOutput, error) {
		// Answer yes only after the condition's own deadline has passed.
		<-c.Done()
		return ShellOutput{Stdout: []byte("ready\n"), Exited: true}, nil
	}}
	e := testEnv(dev, nil)
	st := jobspec.Step{ID: "wait", Payload: jobspec.WaitFor{
		Probe:    "getprop sys.boot_completed",
		Interval: jobspec.Duration(time.Millisecond),
		Timeout:  jobspec.Duration(20 * time.Millisecond),
	}}

	res, err := execWaitFor(stepCtx(t), e, st)
	if err != nil {
		t.Fatalf("execWaitFor: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("Failure = %q; the probe answered and the answer was thrown away", res.Failure)
	}
}

// e2eProbe renders the probe the way execWaitFor quotes it into its message.
func e2eProbe(st jobspec.Step) string {
	p := st.Payload.(jobspec.WaitFor)
	return `"` + p.Probe + `"`
}

// ---------------------------------------------------------------------------
// shell_detached and re-attaching to it
// ---------------------------------------------------------------------------

func detachedStep() jobspec.Step {
	return jobspec.Step{ID: "soak", Payload: jobspec.ShellDetached{
		Command:    "sh /data/local/tmp/soak.sh",
		ResultPath: "/data/local/tmp/soak.rc",
		Handle:     "soak",
	}}
}

func TestDetachedStartHandsTheResultToTheDevice(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(_ context.Context, call int, cmd string) (ShellOutput, error) {
		if strings.HasPrefix(cmd, "cat ") {
			return okShell("4711\n"), nil
		}
		return okShell("started\n"), nil
	}}
	e := testEnv(dev, nil)

	res, err := execShellDetached(stepCtx(t), e, detachedStep())
	if err != nil {
		t.Fatalf("execShellDetached: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("Failure = %q", res.Failure)
	}
	if got := res.Detail["pid"]; got != "4711" {
		t.Fatalf("pid = %v, want the worker shell's own pid", got)
	}
	if got := res.Detail["result_path"]; got != "/data/local/tmp/soak.rc" {
		t.Fatalf("result_path = %v", got)
	}
	if got := res.Detail["log_path"]; got != "/data/local/tmp/soak.log" {
		t.Fatalf("log_path = %v", got)
	}

	start := dev.commands()[0]
	// Five load-bearing details, each one a way a six-hour run dies quietly.
	for _, want := range []string{
		`mkdir -p "/data/local/tmp" || exit 1`, // a directory we cannot write is fatal, not exit 0
		"nohup setsid sh -c",                   // the command outlives the shell's process group
		"( sh /data/local/tmp/soak.sh )",       // a SUBSHELL, so `exit $rc` cannot kill the wrapper
		`echo $? > "/data/local/tmp/soak.rc.tmp"`,
		`mv "/data/local/tmp/soak.rc.tmp" "/data/local/tmp/soak.rc"`, // published by an atomic rename
		"< /dev/null > /dev/null 2>&1 &",                             // nothing keeps the ADB stream open
	} {
		if !strings.Contains(start, want) {
			t.Fatalf("the start command is missing %q:\n%s", want, start)
		}
	}
	if !strings.Contains(start, "echo $$ > ") {
		t.Fatalf("the pid is not captured from inside the worker shell:\n%s", start)
	}
}

// A pid file that never appears is said out loud, because the innocent
// explanation (a phone slow to flush a line) and the alarming one (nothing is
// running at all) look identical in the step row.
func TestDetachedStartSaysSoWhenNoPidAppears(t *testing.T) {
	t.Parallel()

	logs := &logCapture{}
	dev := &fakeConn{shell: func(_ context.Context, _ int, cmd string) (ShellOutput, error) {
		if strings.HasPrefix(cmd, "cat ") {
			return okShell("/system/bin/sh: cat: not found\n"), nil
		}
		return okShell("started\n"), nil
	}}
	e := testEnv(dev, func(e *env) { e.log = logs.logger() })

	res, err := execShellDetached(stepCtx(t), e, detachedStep())
	if err != nil {
		t.Fatalf("execShellDetached: %v", err)
	}
	if res.Detail["pid"] != nil {
		t.Fatalf("pid = %v, want no pid rather than a line of shell noise in a jsonb column", res.Detail["pid"])
	}
	if res.Detail["pid_unknown"] != true {
		t.Fatal("pid_unknown was not recorded")
	}
	if logs.count("wrote no pid file") == 0 {
		t.Fatal("nothing warned that the detached command may never have run")
	}
}

// A start that the device refused is a verdict, not a retry: everything after
// the mkdir is backgrounded with its output discarded, so a directory the
// shell user cannot write is the one thing the start command can still report.
func TestDetachedStartReportsARefusedStartAsAFailure(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{Stderr: []byte("mkdir: '/nope': Permission denied\n"), ExitCode: 1, Exited: true}, nil
	}}
	e := testEnv(dev, nil)

	res, err := execShellDetached(stepCtx(t), e, detachedStep())
	if err != nil {
		t.Fatalf("a refused start was reported as an error, so it would be retried: %v", err)
	}
	if !strings.Contains(res.Failure, "could not start the detached command") {
		t.Fatalf("Failure = %q", res.Failure)
	}
}

// The re-attach probe answers one of three tokens. Anything else is an answer
// we did not understand — and the one reading it must NOT turn into is
// "absent", which would start a second copy of a command that may well be
// running right now.
func TestReattachNeverGuessesAbsentFromAnAnswerItDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		out         ShellOutput
		err         error
		wantResult  bool
		wantErr     bool
		wantRetried bool
		wantState   string
	}{
		{name: "the command finished while we were away", out: okShell("done 0\n"), wantResult: true, wantState: "done"},
		// "done" on its own is no longer an answer. A detached command that
		// started and then FAILED used to be indistinguishable from one that
		// succeeded, so the wrapper publishes the exit status with the token
		// and a bare "done" is now something we do not understand — which must
		// be retried, never read as absent.
		{
			name:        "done, with no exit status behind it",
			out:         okShell("done\n"),
			wantErr:     true,
			wantRetried: true,
		},
		{name: "the command is still running", out: okShell("running\n"), wantResult: true, wantState: "running"},
		{name: "the device carries no trace of it", out: okShell("absent\n")},
		{
			name:        "an adb banner demultiplexed into stdout",
			out:         okShell("* daemon started successfully\n"),
			wantErr:     true,
			wantRetried: true,
		},
		{
			name:        "a stream that ended before the status",
			out:         ShellOutput{Stdout: []byte("run")},
			wantErr:     true,
			wantRetried: true,
		},
		{
			name:        "the probe never reached the device",
			err:         transportErr("connection reset by peer"),
			wantErr:     true,
			wantRetried: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
				return tc.out, tc.err
			}}, nil)

			res, err := reattachDetached(stepCtx(t), e, detachedStep())
			switch {
			case tc.wantErr:
				if err == nil {
					t.Fatalf("probe answer was accepted; result = %+v", res)
				}
				if isRetryable(err) != tc.wantRetried {
					t.Fatalf("isRetryable(%v) = %t, want %t", err, isRetryable(err), tc.wantRetried)
				}
				if res != nil {
					t.Fatalf("an unusable answer produced a result: %+v", res)
				}
			case tc.wantResult:
				if err != nil || res == nil {
					t.Fatalf("res = %+v, err = %v", res, err)
				}
				if got := res.Detail["reattached_state"]; got != tc.wantState {
					t.Fatalf("reattached_state = %v, want %q", got, tc.wantState)
				}
			default:
				if err != nil || res != nil {
					t.Fatalf("res = %+v, err = %v; want (nil, nil) so the step simply runs", res, err)
				}
			}
		})
	}
}

// A resume onto a detached step probes INSIDE the retry loop, so a flaky link
// cannot answer "is this run already going on the device?" with "no".
func TestRunStepRetriesTheReattachProbeAndThenReattaches(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
		if call <= 2 {
			return ShellOutput{}, transportErr("device offline")
		}
		return okShell("running\n"), nil
	}}
	e := testEnv(dev, nil)

	res, retries, err := r.runStep(stepCtx(t), e, detachedStep(), true)
	if err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if retries != 2 {
		t.Fatalf("retries = %d, want 2", retries)
	}
	if res == nil || res.Detail["reattached_state"] != "running" {
		t.Fatalf("res = %+v, want a re-attach to the run already in progress", res)
	}
	for _, cmd := range dev.commands() {
		if strings.Contains(cmd, "nohup") {
			t.Fatalf("a second copy of the detached command was started: %s", cmd)
		}
	}
}

// When the device carries no trace of the earlier start, the previous process
// died before launching anything and the step simply runs.
func TestRunStepRunsTheStepWhenTheDeviceCarriesNoTraceOfIt(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	dev := &fakeConn{shell: func(_ context.Context, call int, cmd string) (ShellOutput, error) {
		switch {
		case call == 1:
			return okShell("absent\n"), nil
		case strings.HasPrefix(cmd, "cat "):
			return okShell("991\n"), nil
		default:
			return okShell("started\n"), nil
		}
	}}
	e := testEnv(dev, nil)

	res, _, err := r.runStep(stepCtx(t), e, detachedStep(), true)
	if err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if res.Detail["handle"] != "soak" || res.Detail["pid"] != "991" {
		t.Fatalf("the step did not actually start: %+v", res.Detail)
	}
	var started bool
	for _, cmd := range dev.commands() {
		if strings.Contains(cmd, "nohup setsid") {
			started = true
		}
	}
	if !started {
		t.Fatal("the detached command was never launched")
	}
}

// ---------------------------------------------------------------------------
// assert
// ---------------------------------------------------------------------------

func TestCompareIsAVerdictAboutTheDeviceAndAnErrorAboutTheSpec(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		got, want string
		op        jobspec.Operator
		ok        bool
	}{
		{"1", "1", jobspec.OpEQ, true},
		{"1", "2", jobspec.OpEQ, false},
		{"1", "2", jobspec.OpNE, true},
		{"boot_completed=1", "completed", jobspec.OpContains, true},
		{"boot_completed=1", "failed", jobspec.OpNotContains, true},
		{"android-14", `^android-\d+$`, jobspec.OpMatches, true},
		{"42", "41", jobspec.OpGT, true},
		{"42", "42", jobspec.OpGE, true},
		{"41", "42", jobspec.OpLT, true},
		{"42", "42", jobspec.OpLE, true},
		// The device answered something that is not a number where the spec
		// asked for one. That IS a failed assertion, and saying so is more
		// useful than a type error.
		{"not-a-number", "42", jobspec.OpGT, false},
	} {
		got, err := compare(tc.got, tc.op, tc.want)
		if err != nil {
			t.Fatalf("compare(%q, %s, %q): %v", tc.got, tc.op, tc.want, err)
		}
		if got != tc.ok {
			t.Fatalf("compare(%q, %s, %q) = %t, want %t", tc.got, tc.op, tc.want, got, tc.ok)
		}
	}

	// A broken comparison is the spec's problem, not the device's, and the two
	// call for different reactions from whoever reads the row.
	for _, tc := range []struct {
		name      string
		got, want string
		op        jobspec.Operator
	}{
		{"a pattern that does not compile", "x", "([", jobspec.OpMatches},
		{"a numeric operator against a non-numeric expectation", "42", "many", jobspec.OpGT},
		{"an operator nobody defined", "x", "y", jobspec.Operator("approximately")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compare(tc.got, tc.op, tc.want)
			if !errors.Is(err, ErrNotRetryable) {
				t.Fatalf("err = %v, want a non-retryable spec error", err)
			}
		})
	}
}

func TestAssertStepFailsTheJobWithoutRetrying(t *testing.T) {
	t.Parallel()

	st := jobspec.Step{ID: "check", Payload: jobspec.Assert{
		Probe: "getprop ro.build.version.sdk", Operator: jobspec.OpGE, Value: "33"}}

	e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return okShell("30\n"), nil
	}}, nil)
	res, err := execAssert(stepCtx(t), e, st)
	if err != nil {
		t.Fatalf("a failed assertion was reported as an error: %v", err)
	}
	if res.Failure == "" {
		t.Fatal("an assertion about the device produced no Failure")
	}
	if res.Detail["got"] != "30" || res.Detail["want"] != "33" {
		t.Fatalf("detail = %+v, want both sides recorded", res.Detail)
	}

	// A probe whose stream died is still the wire.
	e = testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{Stdout: []byte("3")}, nil
	}}, nil)
	if _, err := execAssert(stepCtx(t), e, st); err == nil || !isRetryable(err) {
		t.Fatalf("a truncated probe stream gave err = %v; want a retryable transport failure", err)
	}
}

// ---------------------------------------------------------------------------
// sleep
// ---------------------------------------------------------------------------

func TestSleepStepEndsWithTheRunsOwnCause(t *testing.T) {
	t.Parallel()

	st := jobspec.Step{ID: "settle", Payload: jobspec.Sleep{Duration: jobspec.Duration(5 * time.Millisecond)}}
	res, err := execSleep(stepCtx(t), testEnv(&fakeConn{}, nil), st)
	if err != nil || res.Detail["slept"] != "5ms" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}

	long := jobspec.Step{ID: "settle", Payload: jobspec.Sleep{Duration: jobspec.Duration(time.Hour)}}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errFencedForTest())
	if _, err := execSleep(ctx, testEnv(&fakeConn{}, nil), long); err == nil {
		t.Fatal("a sleep outlived the run that contained it")
	} else if !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("err = %v, want the run's own cause", err)
	}
}

func errFencedForTest() error { return errors.New("lease 1: lease: fenced") }

// ---------------------------------------------------------------------------
// push / install / uninstall / pull
// ---------------------------------------------------------------------------

// fakeArtifacts is the content-addressed store as the runner sees it.
type fakeArtifacts struct {
	ensure  func(deviceID, sha string, push artifacts.PushFunc) (artifacts.EnsureResult, error)
	put     func(r io.Reader, name string) (artifacts.PutResult, error)
	removed []string
	forgot  []string
	ensures int
}

func (f *fakeArtifacts) EnsureOnDevice(_ context.Context, deviceID, sha string, push artifacts.PushFunc) (artifacts.EnsureResult, error) {
	f.ensures++
	if f.ensure == nil {
		return artifacts.EnsureResult{
			Artifact: artifacts.Artifact{SHA256: sha, Name: "payload.bin", Size: 12},
			Pushed:   true,
		}, nil
	}
	return f.ensure(deviceID, sha, push)
}

func (f *fakeArtifacts) MarkRemoved(_ context.Context, _, sha, _ string) (bool, error) {
	f.removed = append(f.removed, sha)
	return true, nil
}

func (f *fakeArtifacts) ForgetDevice(_ context.Context, deviceID, _ string) (int64, error) {
	f.forgot = append(f.forgot, deviceID)
	return 3, nil
}

func (f *fakeArtifacts) Put(_ context.Context, r io.Reader, _ artifacts.Kind, name string, _ ...artifacts.PutOption) (artifacts.PutResult, error) {
	if f.put == nil {
		n, err := io.Copy(io.Discard, r)
		return artifacts.PutResult{Artifact: artifacts.Artifact{Name: name, Size: n, SHA256: "deadbeef"}, Inserted: true}, err
	}
	return f.put(r, name)
}

var _ Artifacts = (*fakeArtifacts)(nil)

// A step that names content nobody uploaded must fail with that sentence, not
// with a timeout half an hour later: the runner's default is to retry, and
// retrying a digest that does not exist sends an operator to look at cables.
func TestArtifactErrorsThatRetryingCannotFixAreNotRetried(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want bool // NotRetryable
	}{
		{"nobody uploaded it", artifacts.ErrNotFound, true},
		{"the row exists and the bytes do not", artifacts.ErrBlobMissing, true},
		{"the device is not in farm.devices", artifacts.ErrUnknownDevice, true},
		{"the content is over the limit", artifacts.ErrTooLarge, true},
		{"the bytes do not hash to their name", &artifacts.CorruptError{SHA: "abc", Got: "def"}, true},
		{"the push died on the wire", transportErr("connection reset by peer"), false},
		{"the database blinked", errors.New("closed pool"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyArtifactError(tc.err)
			if errors.Is(got, ErrNotRetryable) != tc.want {
				t.Fatalf("classifyArtifactError(%v) = %v; NotRetryable want %t", tc.err, got, tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("classifyArtifactError lost the cause: %v", got)
			}
		})
	}
	if classifyArtifactError(nil) != nil {
		t.Fatal("classifyArtifactError invented an error out of success")
	}
}

// A runner with no artifact store refuses the step outright rather than
// retrying it for half an hour.
func TestStepsThatNeedTheStoreRefuseWhenThereIsNone(t *testing.T) {
	t.Parallel()

	e := testEnv(&fakeConn{}, nil) // artifacts deliberately nil
	sha := strings.Repeat("a", 64)

	_, err := execPush(stepCtx(t), e, jobspec.Step{ID: "p",
		Payload: jobspec.Push{SHA256: sha, Dest: "/data/local/tmp/x"}})
	if !errors.Is(err, ErrNotRetryable) || !strings.Contains(err.Error(), "Config.Artifacts") {
		t.Fatalf("execPush err = %v, want a non-retryable refusal naming the fix", err)
	}

	_, err = execPull(stepCtx(t), e, jobspec.Step{ID: "q",
		Payload: jobspec.Pull{Path: "/data/local/tmp/x", Artifact: "x"}})
	if !errors.Is(err, ErrNotRetryable) || !strings.Contains(err.Error(), "Config.Artifacts") {
		t.Fatalf("execPull err = %v, want a non-retryable refusal naming the fix", err)
	}
}

// The ledger records that the CONTENT is on the device, not that it is at the
// path this step names. A skip is therefore verified, and a claim the device
// does not honour is retracted before the bytes are pushed for real —
// otherwise every later step trusts a file that is not there.
func TestPushVerifiesTheLedgersClaimAndRetractsItWhenItIsWrong(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("b", 64)
	store := &fakeArtifacts{}
	store.ensure = func(_, sha string, push artifacts.PushFunc) (artifacts.EnsureResult, error) {
		if store.ensures == 1 {
			// The ledger says it is already there. It is not.
			return artifacts.EnsureResult{
				Artifact:   artifacts.Artifact{SHA256: sha, Name: "app.bin", Size: 9},
				Pushed:     false,
				RemotePath: "/data/local/tmp/device-farmer/job-1/app.bin",
			}, nil
		}
		if _, err := push(context.Background(), artifacts.Artifact{SHA256: sha}, nil); err != nil {
			return artifacts.EnsureResult{}, err
		}
		return artifacts.EnsureResult{
			Artifact: artifacts.Artifact{SHA256: sha, Name: "app.bin", Size: 9}, Pushed: true,
		}, nil
	}

	var pushed bool
	dev := &fakeConn{
		shell: func(_ context.Context, _ int, cmd string) (ShellOutput, error) {
			switch {
			case strings.HasPrefix(cmd, "[ -f "):
				return okShell("no\n"), nil // not at the path the step wants
			case strings.Contains(cmd, "cp -f"):
				return ShellOutput{ExitCode: 1, Exited: true}, nil // nor anywhere else
			default:
				return okShell(""), nil
			}
		},
		push: func(context.Context, io.Reader, string, fs.FileMode) error { pushed = true; return nil },
	}
	e := testEnv(dev, func(e *env) { e.artifacts = store })

	res, err := execPush(stepCtx(t), e, jobspec.Step{ID: "p",
		Payload: jobspec.Push{SHA256: sha, Dest: "/data/local/tmp/app.bin"}})
	if err != nil {
		t.Fatalf("execPush: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("Failure = %q", res.Failure)
	}
	if len(store.removed) != 1 || store.removed[0] != sha {
		t.Fatalf("the stale 'present' claim was not retracted: %v", store.removed)
	}
	if !pushed {
		t.Fatal("the bytes were never pushed; every later step would trust a file that is not there")
	}
}

// pm install exits 0 while printing "Failure [INSTALL_FAILED_…]" on older
// builds. Trusting the exit status alone reports a green install of an app that
// is not on the device.
func TestInstallDoesNotTrustTheExitStatusAlone(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("c", 64)
	st := jobspec.Step{ID: "i", Payload: jobspec.Install{SHA256: sha, Reinstall: true, Grant: true}}

	for _, tc := range []struct {
		name        string
		out         ShellOutput
		wantFailure bool
	}{
		{"a real success", okShell("Success\n"), false},
		{"exit 0 and a Failure line", okShell("Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE]\n"), true},
		{"a non-zero exit", ShellOutput{Stdout: []byte("Error\n"), ExitCode: 1, Exited: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cmd string
			dev := &fakeConn{shell: func(_ context.Context, _ int, c string) (ShellOutput, error) {
				cmd = c
				return tc.out, nil
			}}
			e := testEnv(dev, func(e *env) { e.artifacts = &fakeArtifacts{} })

			res, err := execInstall(stepCtx(t), e, st)
			if err != nil {
				t.Fatalf("execInstall: %v", err)
			}
			if (res.Failure != "") != tc.wantFailure {
				t.Fatalf("Failure = %q, want failure = %t", res.Failure, tc.wantFailure)
			}
			if !strings.Contains(cmd, "pm install -r -g ") {
				t.Fatalf("install command = %q, want the spec's flags", cmd)
			}
			if !strings.Contains(cmd, sha+".apk") {
				t.Fatalf("install command = %q, want the staged path", cmd)
			}
		})
	}
}

// The step's contract is "this package is absent afterwards", and
// farm.step_kinds calls it idempotent on exactly that reading.
func TestUninstallTreatsAnAbsentPackageAsSuccess(t *testing.T) {
	t.Parallel()

	st := jobspec.Step{ID: "u", Payload: jobspec.Uninstall{Package: "com.example.app"}}

	for _, tc := range []struct {
		name        string
		out         ShellOutput
		wantFailure bool
	}{
		{"removed", okShell("Success\n"), false},
		{"never installed", ShellOutput{Stdout: []byte("Failure [DELETE_FAILED_INTERNAL_ERROR: not installed for 0]\n"), ExitCode: 1, Exited: true}, false},
		{"unknown package", ShellOutput{Stdout: []byte("Unknown package: com.example.app\n"), ExitCode: 1, Exited: true}, false},
		{"genuinely refused", ShellOutput{Stdout: []byte("Failure [DELETE_FAILED_DEVICE_POLICY_MANAGER]\n"), ExitCode: 1, Exited: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
				return tc.out, nil
			}}, nil)
			res, err := execUninstall(stepCtx(t), e, st)
			if err != nil {
				t.Fatalf("execUninstall: %v", err)
			}
			if (res.Failure != "") != tc.wantFailure {
				t.Fatalf("Failure = %q, want failure = %t", res.Failure, tc.wantFailure)
			}
		})
	}
}

// A pull streams straight from the device into the store: a two-gigabyte screen
// recording is a pipe, not an allocation, and a pull that died on the wire is
// still the wire.
func TestPullStreamsIntoTheStoreAndReportsAWireFailureAsOne(t *testing.T) {
	t.Parallel()

	st := jobspec.Step{ID: "g", Payload: jobspec.Pull{Path: "/sdcard/screen.mp4", Artifact: "screen.mp4"}}

	store := &fakeArtifacts{}
	dev := &fakeConn{pull: func(_ context.Context, _ string, w io.Writer) error {
		_, err := w.Write([]byte("0123456789"))
		return err
	}}
	e := testEnv(dev, func(e *env) { e.artifacts = store })

	res, err := execPull(stepCtx(t), e, st)
	if err != nil {
		t.Fatalf("execPull: %v", err)
	}
	if res.Detail["bytes"] != int64(10) || res.Detail["sha256"] != "deadbeef" {
		t.Fatalf("detail = %+v", res.Detail)
	}

	broken := transportErr("connection reset by peer")
	dev = &fakeConn{pull: func(context.Context, string, io.Writer) error { return broken }}
	e = testEnv(dev, func(e *env) { e.artifacts = store })
	if _, err := execPull(stepCtx(t), e, st); !errors.Is(err, broken) || !isRetryable(err) {
		t.Fatalf("err = %v, want the transport failure, retryable", err)
	}
}

// ---------------------------------------------------------------------------
// reset
// ---------------------------------------------------------------------------

// A reset that expands to nothing must say so. Reporting an unexplained green
// step would be a lie of omission: the operator who set reset_tier believes the
// device was cleaned, and it was not.
func TestResetSaysSoWhenItDidNothing(t *testing.T) {
	t.Parallel()

	logs := &logCapture{}
	dev := &fakeConn{}
	e := testEnv(dev, func(e *env) { e.log = logs.logger() }) // no profile

	res, err := execReset(stepCtx(t), e, jobspec.Step{ID: "r",
		Payload: jobspec.Reset{Tier: jobspec.TierSoft}})
	if err != nil {
		t.Fatalf("execReset: %v", err)
	}
	if !strings.Contains(res.Output, "expanded to no steps") {
		t.Fatalf("Output = %q", res.Output)
	}
	if logs.count("reset did nothing") == 0 {
		t.Fatal("a reset that cleaned nothing was silent")
	}
	if dev.calls() != 0 {
		t.Fatalf("a reset with nothing to do touched the device: %v", dev.commands())
	}
}

// Tier 'none' is the spec asking for no reset, and it must touch nothing.
func TestResetTierNoneTouchesNothing(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{}
	e := testEnv(dev, func(e *env) { e.artifacts = &fakeArtifacts{} })
	res, err := execReset(stepCtx(t), e, jobspec.Step{ID: "r",
		Payload: jobspec.Reset{Tier: jobspec.TierNone}})
	if err != nil {
		t.Fatalf("execReset: %v", err)
	}
	if res.Failure != "" || dev.calls() != 0 {
		t.Fatalf("res = %+v, commands = %v", res, dev.commands())
	}
	if got := res.Detail["tier"]; got != "none" {
		t.Fatalf("tier = %v", got)
	}
}

// The tier comes from the job when the step does not name one, so a spec that
// says "reset" gets the reset the job was configured for.
func TestResetFallsBackToTheJobsTier(t *testing.T) {
	t.Parallel()

	e := testEnv(&fakeConn{}, func(e *env) { e.resetTier = string(jobspec.TierNone) })
	res, err := execReset(stepCtx(t), e, jobspec.Step{ID: "r", Payload: jobspec.Reset{}})
	if err != nil {
		t.Fatalf("execReset: %v", err)
	}
	if got := res.Detail["tier"]; got != "none" {
		t.Fatalf("tier = %v, want the job's own tier", got)
	}
}

// A sub-step runs against an EMPTY spec, so a job that declares exit 1 a
// success for its own shell steps cannot thereby declare a failed `pm clear` a
// successful reset.
func TestResetSubStepsDoNotInheritTheJobsExpectations(t *testing.T) {
	t.Parallel()

	e := testEnv(&fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{ExitCode: 1, Exited: true}, nil
	}}, func(e *env) { e.spec = jobspec.Spec{DefaultExpectExit: []int{0, 1}} })

	sub := jobspec.Step{ID: "reset/clear", Payload: jobspec.Shell{Command: "pm clear com.example"}}
	res, err := e.runSub(stepCtx(t), sub)
	if err != nil {
		t.Fatalf("runSub: %v", err)
	}
	if res.Failure == "" {
		t.Fatal("the job's default_expect_exit leaked into a reset sub-step")
	}
}
