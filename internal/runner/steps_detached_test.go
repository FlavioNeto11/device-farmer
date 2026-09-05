package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// These tests are about one sentence: a detached command that starts and then
// fails must not look like one that succeeded.
//
// shell_detached is the mechanism that makes long work survive a partition. No
// socket is the source of truth; the device-side status file is. A mechanism
// whose status file is never READ proves nothing at all — a six-hour soak that
// died in minute three would be re-attached, announced, and passed.
//
// The second sentence they are about is the one that keeps the first from
// eating the invariant: a failure to READ the status file is not a verdict. It
// is a failure to ask, the file is still on the device, and the question is
// asked again on the next poll INSIDE the lease. Every test below that injects
// a transport fault is asserting that distinction, because getting it wrong in
// the other direction — an unreadable file aborting a job — is exactly the
// fusion of transport and outcome this package exists to prevent.
//
// The device is a real fake ADB server (test/fakeadb) driven through the real
// wire client (internal/adbwire), so the transport failures here are severed
// TCP connections rather than a stub returning a Go error.

const (
	testDevpath = "usb:3-1.1"
	testResult  = "/data/local/tmp/.farm/soak.result"
	testHandle  = "soak"
)

// Service prefixes of the three commands a detached step issues. The fake
// matches scripts and faults on a substring of the service string, and the
// service string is "shell,v2,raw:" followed by the command itself.
const (
	svcLaunch = "shell,v2,raw:mkdir -p" // detachedCommand
	svcPID    = "shell,v2,raw:cat "     // readPID
	svcProbe  = "shell,v2,raw:if [ -f"  // probeDetached
)

// adbConn is runner.Conn over one fake ADB server, and nothing else.
//
// It deliberately does not classify errors the way internal/jobrunner does:
// these tests want the wire's own failures to arrive at the runner exactly as
// the wire produced them, so that what is asserted is the runner's judgement
// and not the adapter's.
type adbConn struct {
	cli     *adbwire.Client
	devpath string
}

func (c adbConn) Shell(ctx context.Context, command string) (ShellOutput, error) {
	res, err := c.cli.Shell(ctx, c.devpath, command)
	if res == nil {
		return ShellOutput{ExitCode: -1}, err
	}
	return ShellOutput{
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		ExitCode:  res.ExitCode,
		Exited:    res.Exited,
		Truncated: res.Truncated,
	}, err
}

func (adbConn) Push(context.Context, io.Reader, string, fs.FileMode) error {
	return errors.New("push is not part of these tests")
}

func (adbConn) Pull(context.Context, string, io.Writer) error {
	return errors.New("pull is not part of these tests")
}

// shellV2 renders a scripted device answer as shell v2 frames: the payload the
// real protocol carries, including the exit frame. Scripting raw text instead
// would produce a stream with no exit status, which is a different test.
func shellV2(t *testing.T, stdout string, exit int) string {
	t.Helper()
	var b bytes.Buffer
	if stdout != "" {
		if err := adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout)); err != nil {
			t.Fatalf("frame stdout: %v", err)
		}
	}
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{byte(exit)}); err != nil {
		t.Fatalf("frame exit: %v", err)
	}
	return b.String()
}

// startFake brings up a fake ADB server with one device and the two answers
// every detached step needs before it reaches the status probe: a launch that
// succeeds, and a pid file. What each test varies is the probe's answer.
func startFake(t *testing.T) *fakeadb.Server {
	t.Helper()
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{
		Serial:  "TESTDEVICE1",
		Devpath: testDevpath,
	}))
	srv.Respond(testDevpath, svcLaunch, shellV2(t, "started\n", 0))
	srv.Respond(testDevpath, svcPID, shellV2(t, "4242\n", 0))
	return srv
}

func newDetachedEnv(t *testing.T, srv *fakeadb.Server) *env {
	t.Helper()
	return &env{
		dev:       adbConn{cli: adbwire.New(srv.Addr()), devpath: testDevpath},
		log:       slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		spec:      jobspec.New(),
		detach:    "nohup setsid",
		workDir:   "/data/local/tmp/.farm",
		maxOutput: 64 << 10,
		poll:      10 * time.Millisecond,
	}
}

func detachedSoakStep() jobspec.Step {
	return jobspec.Step{
		ID: "soak/start",
		Payload: jobspec.ShellDetached{
			Command:    "for i in 1 2 3; do sleep 1; done",
			ResultPath: testResult,
			Handle:     testHandle,
		},
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// The launch
// ---------------------------------------------------------------------------

// A command that starts and dies at once is the commonest way to write a
// broken shell_detached step: a binary that is not on the device, a script that
// exits on its first line. The wrapper still launches, so the LAUNCH exits 0
// and the step used to be reported as a success — leaving the wait_for after it
// to burn a six-hour timeout waiting for work that was already over.
func TestExecShellDetachedFailsWhenTheCommandAlreadyDied(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n127\n", 0))

	res, err := execShellDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("execShellDetached returned an error, which would be retried: %v", err)
	}
	if res == nil {
		t.Fatal("no result")
	}
	if res.Failure == "" {
		t.Fatalf("a command that exited 127 was reported as a success: %+v", res)
	}
	if !strings.Contains(res.Failure, "127") {
		t.Errorf("the failure does not name the exit status: %q", res.Failure)
	}
	if res.ExitCode == nil || *res.ExitCode != 127 {
		t.Errorf("exit code = %v, want 127", res.ExitCode)
	}
}

// The same probe, with a status the spec accepts, is not a failure — and the
// step still reports that the command is already over, because a wait_for that
// returns instantly afterwards should not look like a mystery.
func TestExecShellDetachedAcceptsAnImmediateSuccess(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n0\n", 0))

	res, err := execShellDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("execShellDetached: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("exit status 0 was reported as a failure: %q", res.Failure)
	}
	if res.Detail["already_finished"] != true {
		t.Errorf("detail does not record that the command had already finished: %+v", res.Detail)
	}
}

// THE INVARIANT, at the launch. The command is running on the device; the
// socket used to read its status died a millisecond later. Failing the step
// here would end a job because of a transport failure, which is the whole bug
// this system exists to prevent. The status file is on the device and the next
// thing to look — a wait_for, or a resume's re-attach — will find it.
func TestExecShellDetachedSurvivesAnUnreadableStatusFile(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n1\n", 0))
	srv.Inject(fakeadb.Fault{Match: "if [ -f", Kind: fakeadb.FaultReset})

	res, err := execShellDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("a severed status read failed the step: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("a severed status read became a job failure: %q", res.Failure)
	}
	if res.Detail["status_unknown"] != true {
		t.Errorf("detail does not record that the status could not be read: %+v", res.Detail)
	}
	if !strings.Contains(res.Output, "started detached") {
		t.Errorf("the step did not report the command as started: %q", res.Output)
	}
}

// ---------------------------------------------------------------------------
// The re-attach
// ---------------------------------------------------------------------------

// A resume that lands on a finished run must read the status the device
// published. This is the six-hour soak that died in minute three: before the
// status was consulted, this returned "re-attached (done)" and the job passed.
func TestReattachDetachedFailsWhenTheRunFailed(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n137\n", 0))

	res, err := reattachDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("reattachDetached returned an error, which would be retried: %v", err)
	}
	if res == nil {
		t.Fatal("no result: the run was treated as though the device carried no trace of it")
	}
	if res.Failure == "" {
		t.Fatalf("a run that exited 137 was re-attached as a success: %+v", res)
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
}

func TestReattachDetachedAcceptsARunThatSucceeded(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n0\n", 0))

	res, err := reattachDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("reattachDetached: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("exit status 0 was re-attached as a failure: %q", res.Failure)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
}

// A run still in progress has no status yet, and inventing one would be the
// same bug in the opposite direction.
func TestReattachDetachedLeavesARunningCommandAlone(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "running\n", 0))

	res, err := reattachDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("reattachDetached: %v", err)
	}
	if res == nil {
		t.Fatal("a running command was reported as absent, which would start a second copy of it")
	}
	if res.Failure != "" {
		t.Fatalf("a running command was reported as a failure: %q", res.Failure)
	}
	if res.ExitCode != nil {
		t.Errorf("a running command reported an exit status: %v", *res.ExitCode)
	}
	if res.Detail["reattached_state"] != "running" {
		t.Errorf("detail = %+v, want reattached_state running", res.Detail)
	}
}

// No marks on the device means the previous process died before launching
// anything, and the step simply runs.
func TestReattachDetachedReportsAnAbsentRun(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "absent\n", 0))

	res, err := reattachDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
	if err != nil {
		t.Fatalf("reattachDetached: %v", err)
	}
	if res != nil {
		t.Fatalf("an absent run produced a result: %+v", res)
	}
}

// THE INVARIANT, at the re-attach, and the reason the fix cannot simply be
// "read the file and fail if it does not read".
//
// The transport dies while the status is being read. That is not a verdict
// about the run: it is a question that was never asked. It must come back as a
// retryable error — the runner then asks again inside the lease it still
// holds — and the second half of this test is the property that makes that
// correct: the answer was on the device the whole time, so the very next poll
// gets it.
func TestReattachDetachedRetriesInsideTheLeaseWhenTheTransportDies(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n0\n", 0))
	srv.Inject(fakeadb.Fault{Match: "if [ -f", Kind: fakeadb.FaultReset, Times: 1})

	e := newDetachedEnv(t, srv)
	ctx := testCtx(t)

	res, err := reattachDetached(ctx, e, detachedSoakStep())
	if err == nil {
		t.Fatal("a severed status read produced a verdict instead of an error")
	}
	if res != nil {
		t.Fatalf("a severed status read produced a result: %+v", res)
	}
	if !isRetryable(err) {
		t.Fatalf("a severed status read was classified as final: %v", err)
	}

	// Next poll. Nothing about the wire changed what the device recorded.
	res, err = reattachDetached(ctx, e, detachedSoakStep())
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if res == nil || res.Failure != "" {
		t.Fatalf("the retry did not read the status the device had all along: %+v", res)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
}

// An answer nobody can parse is not "absent" and not "success". Reading it as
// absent would start a second copy of a command that may be running right now;
// reading it as success would pass a job whose outcome is unknown. It is asked
// again instead, and the step's own timeout — a number the user wrote down — is
// what ends the attempt if the device keeps answering nonsense.
func TestReattachDetachedRefusesAnAnswerItCannotParse(t *testing.T) {
	for _, answer := range []string{"done\n", "done\nnot-a-status\n", "", "OKAY\n"} {
		srv := startFake(t)
		srv.Respond(testDevpath, svcProbe, shellV2(t, answer, 0))

		res, err := reattachDetached(testCtx(t), newDetachedEnv(t, srv), detachedSoakStep())
		if err == nil {
			t.Fatalf("answer %q was accepted as a verdict: %+v", answer, res)
		}
		if res != nil {
			t.Fatalf("answer %q produced a result: %+v", answer, res)
		}
		if !isRetryable(err) {
			t.Fatalf("answer %q was classified as final: %v", answer, err)
		}
	}
}

// A spec that names its own success set governs a detached run too: the
// payload has no expect_exit of its own, so the spec-level default is the only
// statement its author can make about what counts as success.
func TestReattachDetachedHonoursTheSpecDefaultExpectExit(t *testing.T) {
	srv := startFake(t)
	srv.Respond(testDevpath, svcProbe, shellV2(t, "done\n1\n", 0))

	e := newDetachedEnv(t, srv)
	e.spec.DefaultExpectExit = []int{0, 1}

	res, err := reattachDetached(testCtx(t), e, detachedSoakStep())
	if err != nil {
		t.Fatalf("reattachDetached: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("exit status 1 failed a spec that accepts it: %q", res.Failure)
	}
}
