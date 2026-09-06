package runner

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// These tests are about the THIRD read of a detached command's exit status,
// and about why the first two were not enough.
//
// steps_detached_test.go covers the other two: execShellDetached probes once,
// milliseconds after the launch, and reattachDetached probes once when a
// resume lands on a step that was in flight. Both are single moments. The
// ordinary lifecycle of a soak — launch, wait four hours, finish, no eviction
// and no resume — passes through neither of them again, so a run that started
// cleanly and was then killed published its 137 into a file on the phone that
// nothing ever read. The step went green, and so did the wait_for after it,
// because the wait was written the only way it could be written: an operator's
// `test -f /…/soak.result`, which becomes true for 137 exactly as eagerly as
// it does for 0.
//
// A wait_for that names the detached handle is the third read. The device and
// the wire below these tests are the real fake ADB server and the real wire
// client, as in the file above; what they add is a device whose answer CHANGES
// over the life of the wait, because a status that is only ever read once
// cannot tell a late failure from a long run.

// svcTestF is the service prefix of the operator-written probe the reference
// spec used to carry, so one test can drive both forms of wait_for against one
// simulated device and compare their verdicts.
const svcTestF = "shell,v2,raw:test -f"

// svcWorker is the service prefix of the liveness check the wait makes when
// the status probe keeps answering "running": is the worker shell still in
// /proc, or did something kill it without letting it publish a status?
const svcWorker = "shell,v2,raw:p=$(cat"

// lateFinish scripts a detached run that is still going for `running` status
// probes and only then publishes code.
//
// The gap this file is about lives in the word "then": every read before that
// point says "running", so no single probe — least of all the one taken at
// launch — can see what the command eventually did.
func lateFinish(t *testing.T, srv *fakeadb.Server, running, code int) {
	t.Helper()
	// Both answers are framed here, on the test's own goroutine: shellV2 calls
	// t.Fatalf, and the closure below runs on the fake server's goroutine,
	// where a Fatalf is a race rather than a failed test.
	stillRunning := shellV2(t, "running\n", 0)
	finished := shellV2(t, fmt.Sprintf("done\n%d\n", code), 0)

	var mu sync.Mutex
	seen := 0
	srv.RespondWith(testDevpath, svcProbe, func(string) string {
		mu.Lock()
		defer mu.Unlock()
		seen++
		if seen <= running {
			return stillRunning
		}
		return finished
	})
}

// lateFile scripts the same device's answer to `test -f <result>`: the file is
// missing for `absent` polls and present afterwards. It says nothing at all
// about what the file CONTAINS, which is exactly the operator probe's problem.
func lateFile(t *testing.T, srv *fakeadb.Server, absent int) {
	t.Helper()
	missing := shellV2(t, "", 1)
	present := shellV2(t, "", 0)

	var mu sync.Mutex
	seen := 0
	srv.RespondWith(testDevpath, svcTestF, func(string) string {
		mu.Lock()
		defer mu.Unlock()
		seen++
		if seen <= absent {
			return missing
		}
		return present
	})
}

// soakEnv is an env whose spec actually contains the detached step. That is
// how a wait_for naming a handle finds the device-side paths it has to read:
// the waiting step carries only the token.
func soakEnv(t *testing.T, srv *fakeadb.Server) *env {
	t.Helper()
	e := newDetachedEnv(t, srv)
	e.spec = jobspec.New(detachedSoakStep())
	return e
}

func waitOnHandle(timeout time.Duration) jobspec.Step {
	return jobspec.Step{
		ID: "soak/await",
		Payload: jobspec.WaitFor{
			Handle:   testHandle,
			Interval: jobspec.Duration(5 * time.Millisecond),
			Timeout:  jobspec.Duration(timeout),
		},
	}
}

func waitOnProbe(timeout time.Duration) jobspec.Step {
	return jobspec.Step{
		ID: "soak/await",
		Payload: jobspec.WaitFor{
			Probe:    "test -f " + testResult,
			Interval: jobspec.Duration(5 * time.Millisecond),
			Timeout:  jobspec.Duration(timeout),
		},
	}
}

// THE FALSIFICATION. One device, one soak, one late kill — and the two forms
// of wait_for reaching opposite verdicts about it.
//
// The operator probe is the one the reference spec shipped. It asks whether
// the status file exists, the wrapper publishes 137 as promptly as it
// publishes 0, and the step therefore goes green over a run that was killed.
// Naming the handle makes the runner read that status and compare it, and the
// same device now fails the same job.
func TestWaitForOnAProbeMissesTheLateFailureAHandleCatches(t *testing.T) {
	// The soak dies after two probes, so neither the launch probe nor any
	// single early read could have seen it.
	const stillRunning = 2

	t.Run("the operator probe reports success", func(t *testing.T) {
		srv := startFake(t)
		lateFinish(t, srv, stillRunning, 137)
		lateFile(t, srv, stillRunning)

		res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnProbe(2*time.Second))
		if err != nil {
			t.Fatalf("execWaitFor: %v", err)
		}
		if res.Failure != "" {
			t.Fatalf("the probe form judges its own probe and nothing else; "+
				"this case is what pins that: %q", res.Failure)
		}
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("exit code = %v, want 0 — the probe judged `test -f`, not the soak", res.ExitCode)
		}
	})

	t.Run("the handle reports the kill", func(t *testing.T) {
		srv := startFake(t)
		lateFinish(t, srv, stillRunning, 137)
		lateFile(t, srv, stillRunning)

		res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(2*time.Second))
		if err != nil {
			t.Fatalf("execWaitFor returned an error, which would be retried: %v", err)
		}
		if res.Failure == "" {
			t.Fatalf("a soak that exited 137 mid-wait was reported as a success: %+v", res)
		}
		if !strings.Contains(res.Failure, "137") {
			t.Errorf("the failure does not name the exit status: %q", res.Failure)
		}
		if res.ExitCode == nil || *res.ExitCode != 137 {
			t.Errorf("exit code = %v, want 137", res.ExitCode)
		}
		if res.Detail["exit_code"] != 137 {
			t.Errorf("detail does not carry the exit status: %+v", res.Detail)
		}
	})
}

// The whole lifecycle in order, which is the shape of the bug: the launch
// probe sees a command that is still running and reports the step green —
// correctly — and the wait after it is what eventually reads the 137.
func TestTheDetachedLaunchPassesAndTheWaitFails(t *testing.T) {
	srv := startFake(t)
	lateFinish(t, srv, 2, 137)
	e := soakEnv(t, srv)
	ctx := testCtx(t)

	start, err := execShellDetached(ctx, e, detachedSoakStep())
	if err != nil {
		t.Fatalf("execShellDetached: %v", err)
	}
	if start.Failure != "" {
		t.Fatalf("a command that was still running was failed at launch: %q", start.Failure)
	}

	wait, err := execWaitFor(ctx, e, waitOnHandle(2*time.Second))
	if err != nil {
		t.Fatalf("execWaitFor: %v", err)
	}
	if wait.Failure == "" {
		t.Fatalf("the wait passed a soak that exited 137: %+v", wait)
	}
}

func TestWaitForOnAHandleAcceptsARunThatFinishedWell(t *testing.T) {
	srv := startFake(t)
	lateFinish(t, srv, 2, 0)

	res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(2*time.Second))
	if err != nil {
		t.Fatalf("execWaitFor: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("exit status 0 was reported as a failure: %q", res.Failure)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if res.Detail["polls"] == nil || res.Detail["poll_blips"] == nil {
		t.Errorf("detail does not record the poll counts: %+v", res.Detail)
	}
}

// A spec that names its own success set governs the wait too, because it is
// the same run being judged. shell_detached has no expect_exit of its own, so
// a detached command must not be judged differently depending on which step
// happened to read the status it published.
func TestWaitForOnAHandleHonoursTheSpecDefaultExpectExit(t *testing.T) {
	srv := startFake(t)
	lateFinish(t, srv, 1, 1)

	e := soakEnv(t, srv)
	e.spec.DefaultExpectExit = []int{0, 1}

	res, err := execWaitFor(testCtx(t), e, waitOnHandle(2*time.Second))
	if err != nil {
		t.Fatalf("execWaitFor: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("exit status 1 failed a spec that accepts it: %q", res.Failure)
	}
}

// A command that never finishes is a verdict, not an error: the condition the
// spec wrote down did not hold in the time the spec allowed. The message has
// to say which of the device's answers it kept getting, because "the soak
// needs longer" and "nothing ever started" are otherwise the same timeout.
func TestWaitForOnAHandleReportsAnUnfinishedRunAsAVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		hint   string
	}{
		{"still running", "running\n", "still running"},
		{"no trace at all", "absent\n", "no trace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFake(t)
			srv.Respond(testDevpath, svcProbe, shellV2(t, tc.answer, 0))

			// A second, not a hundred milliseconds: the first status read has
			// to dial the fake, negotiate and read a framed reply, and a
			// deadline that can expire before it lands would make this test
			// fail on a loaded machine rather than on a regression.
			res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(time.Second))
			if err != nil {
				t.Fatalf("an unmet condition came back as an error, which would be retried: %v", err)
			}
			if res.Failure == "" {
				t.Fatalf("a command that never finished was reported as a success: %+v", res)
			}
			if res.ExitCode != nil {
				t.Errorf("a command that never finished reported an exit status: %v", *res.ExitCode)
			}
			if !strings.Contains(res.Failure, tc.hint) {
				t.Errorf("the failure does not say what the device kept answering (%q): %q", tc.hint, res.Failure)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The worker that vanished
// ---------------------------------------------------------------------------
//
// "Running" is inferred from the LOG file, which the launch's redirection
// creates and nothing ever removes. A worker killed from outside — the OOM
// killer taking the whole process group, a reboot, an `am kill` — therefore
// leaves exactly the marks of a soak in its third hour. Without a second
// opinion the wait cannot tell them apart and burns the whole six hours its
// author wrote for the soak.

// A command whose worker shell is gone from /proc, with no status published,
// has no verdict to read and never will. That is a statement about the WORK —
// something ended it — so it is a step failure and not an error to retry.
func TestWaitForOnAHandleFailsWhenTheWorkerVanished(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "running\n", 0))
	srv.Respond(testDevpath, svcWorker, shellV2(t, "gone 4242\n", 0))

	res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(5*time.Second))
	if err != nil {
		t.Fatalf("a vanished worker came back as an error, which would be retried: %v", err)
	}
	if res.Failure == "" {
		t.Fatalf("a worker that is gone with no status published was reported as a success: %+v", res)
	}
	if !strings.Contains(res.Failure, "4242") {
		t.Errorf("the failure does not name the pid it checked: %q", res.Failure)
	}
	if res.Detail["worker_gone"] != true {
		t.Errorf("detail does not record that the worker was gone: %+v", res.Detail)
	}
	if res.ExitCode != nil {
		t.Errorf("a run that published no status reported an exit status: %v", *res.ExitCode)
	}
}

// The race the confirming read exists for. The worker publishes its status and
// THEN exits, so between finding /proc empty and concluding anything there is
// one round trip in which the status can land — and when it does, the run has
// a verdict and the verdict is what counts.
func TestWaitForOnAHandleTakesAStatusThatLandedAsTheWorkerExited(t *testing.T) {
	srv := startFake(t)
	// First status read says running; the confirming read, after /proc came
	// back empty, finds the published 0.
	lateFinish(t, srv, 1, 0)
	srv.Respond(testDevpath, svcWorker, shellV2(t, "gone 4242\n", 0))

	res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(5*time.Second))
	if err != nil {
		t.Fatalf("execWaitFor: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("a run that published 0 as it exited was failed as vanished: %q", res.Failure)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want the 0 the worker published on its way out", res.ExitCode)
	}
}

// The dangerous direction. A worker that IS in /proc, and a check that could
// not answer at all, must both leave the wait waiting — a soak failed because
// /proc was unreadable would be the same fusion of "I could not see" with "it
// is broken" that the whole package exists to prevent.
func TestWaitForOnAHandleKeepsWaitingWhenTheWorkerIsAliveOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"alive", "alive 4242\n"},
		{"no pid was ever written", "nopid\n"},
		{"proc says nothing", "noproc\n"},
		{"an answer nobody can parse", "OKAY\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFake(t)
			srv.Respond(testDevpath, svcProbe, shellV2(t, "running\n", 0))
			srv.Respond(testDevpath, svcWorker, shellV2(t, tc.answer, 0))

			res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(time.Second))
			if err != nil {
				t.Fatalf("execWaitFor: %v", err)
			}
			if res.Detail["worker_gone"] != nil {
				t.Fatalf("answer %q was read as a vanished worker: %+v", tc.answer, res.Detail)
			}
			if !strings.Contains(res.Failure, "still running") {
				t.Errorf("the wait ended saying something other than 'still running': %q", res.Failure)
			}
		})
	}
}

// THE INVARIANT, in the wait. The connection dies while the status is being
// read; the status is a file on the device and is unaffected by that. Ending
// the job here would kill a six-hour run over a socket, which is the whole
// defect this package exists to prevent — so the read is simply asked again,
// inside the lease, and the answer was there the entire time.
func TestWaitForOnAHandleKeepsWaitingThroughTransportFailures(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n0\n", 0))
	srv.Inject(fakeadb.Fault{Match: "if [ -f", Kind: fakeadb.FaultReset, Times: 3})

	res, err := execWaitFor(testCtx(t), soakEnv(t, srv), waitOnHandle(5*time.Second))
	if err != nil {
		t.Fatalf("severed status reads ended the wait: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("severed status reads became a job failure: %q", res.Failure)
	}
	if blips, _ := res.Detail["poll_blips"].(int); blips < 3 {
		t.Errorf("poll_blips = %v, want at least the 3 severed reads: %+v",
			res.Detail["poll_blips"], res.Detail)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want the 0 the device had all along", res.ExitCode)
	}
}

// A wait naming a handle no shell_detached step declares cannot be answered by
// any device, so it must not be retried against one. jobspec refuses such a
// spec outright; this pins what the runner does with a document that never
// went through it.
func TestWaitForOnAHandleRefusesASpecThatDeclaresNoSuchRun(t *testing.T) {
	srv := startFake(t)
	e := newDetachedEnv(t, srv) // an empty spec: nothing declares "soak"

	res, err := execWaitFor(testCtx(t), e, waitOnHandle(2*time.Second))
	if err == nil {
		t.Fatalf("an unresolvable handle produced a verdict: %+v", res)
	}
	if isRetryable(err) {
		t.Fatalf("an unresolvable handle was retried against a device: %v", err)
	}
	if !strings.Contains(err.Error(), testHandle) {
		t.Errorf("the error does not name the handle: %v", err)
	}
}
