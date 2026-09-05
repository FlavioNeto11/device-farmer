package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// ---------------------------------------------------------------------------
// The device, as narrowly as the runner needs it.
// ---------------------------------------------------------------------------

// Conn is everything the runner may do to a device.
//
// It is an interface, and it is this small, for two reasons. The first is
// testability: every executor below can be driven by a fake, so the branch
// that decides "retry inside the lease" versus "fail the step" is exercised
// without a phone, a hub or an adb server. The second is discipline — a Conn
// cannot renew a lease, cannot release one and cannot report health, because
// it has no method that could. A socket error has nowhere to travel except
// back to its caller as a Go error.
//
// Implementations MUST address the device by its USB position (devpath) and
// never by its ADB serial: duplicate OEM serials are real, and a
// serial-addressed command can land on a healthy device three hours into
// somebody else's run.
//
// Errors should be returned unwrapped for transport failures — they are
// retried inside the lease — and wrapped with [NotRetryable] for protocol
// refusals, which are not.
type Conn interface {
	// Shell runs one command to completion and returns its demultiplexed
	// output and exit status.
	Shell(ctx context.Context, command string) (ShellOutput, error)

	// Push writes the bytes of r to remote on the device.
	Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error

	// Pull copies remote off the device into w.
	Pull(ctx context.Context, remote string, w io.Writer) error
}

// ShellOutput is the result of one shell command. It mirrors what the ADB
// shell v2 protocol actually reports, including the case that matters most.
type ShellOutput struct {
	Stdout []byte
	Stderr []byte

	// ExitCode is the device-side status.
	ExitCode int

	// Exited distinguishes "exited with 0" from "the stream ended before the
	// device told us anything". The second is a wire failure wearing a
	// success's clothes, and reading it as success is how a bumped cable
	// becomes a green test run.
	Exited bool

	// Truncated records that the implementation stopped capturing output.
	Truncated bool
}

// Artifacts is the part of *artifacts.Store the runner uses.
//
// Content addressing is what makes a 60-device fleet affordable — a 200 MB APK
// pushed once and skipped 59 times — and farm.device_artifacts is the ledger
// that permits the skip. That ledger has exactly one writer, so the runner
// goes through this interface rather than touching the table.
type Artifacts interface {
	// EnsureOnDevice puts the content on the device unless the ledger already
	// says it is there, calling push when a transfer is actually needed.
	EnsureOnDevice(ctx context.Context, deviceID, sha string, push artifacts.PushFunc) (artifacts.EnsureResult, error)

	// MarkRemoved retracts a 'present' claim the device turned out not to
	// honour.
	MarkRemoved(ctx context.Context, deviceID, sha, reason string) (bool, error)

	// ForgetDevice drops every claim about a device, which is what a reset
	// that wipes packages owes the ledger.
	ForgetDevice(ctx context.Context, deviceID, reason string) (int64, error)

	// Put stores bytes whose digest is not known until they have all been
	// read — a file pulled off a device is the case that matters here.
	Put(ctx context.Context, r io.Reader, kind artifacts.Kind, name string, opts ...artifacts.PutOption) (artifacts.PutResult, error)
}

var _ Artifacts = (*artifacts.Store)(nil)

// ---------------------------------------------------------------------------
// Execution environment and results.
// ---------------------------------------------------------------------------

// env is everything an executor is given. It is per-attempt and is never
// shared between placements.
type env struct {
	dev       Conn
	artifacts Artifacts
	pool      *pgxpool.Pool
	log       *slog.Logger

	place     Placement
	attempt   int
	spec      jobspec.Spec
	resetTier string
	profileID string

	// workDir is the job's scratch directory on the DEVICE: staged APKs and
	// anything else a step needs a path for. Detached commands write to the
	// paths their own spec names.
	workDir string

	// detach is the prefix that daemonises a command — "nohup setsid" where
	// setsid exists, "nohup" where it does not. Probed once per attempt,
	// because guessing wrong means a six-hour run dies with its ADB socket.
	detach string

	maxOutput int
	poll      time.Duration
	callTO    time.Duration
}

// Result is what a step produced.
//
// The distinction between Failure and a returned error is the one that
// matters. Failure means the step ran and the device said no, which no retry
// will change. An error means the attempt to run it did not complete, which
// usually means the wire — and the wire is retried inside the lease.
type Result struct {
	ExitCode  *int
	Output    string
	Failure   string
	Detail    map[string]any
	Truncated bool
}

func (r *Result) detail() map[string]any {
	if r.Detail == nil {
		r.Detail = map[string]any{}
	}
	return r.Detail
}

type executor func(ctx context.Context, e *env, st jobspec.Step) (*Result, error)

// executorFor is the dispatch table: exactly one arm per row of
// farm.step_kinds, and no eleventh.
//
// It is a function rather than a package-level map because execReset runs the
// steps a reset tier expands into, and a map whose values reach back into the
// map is an initialisation cycle the compiler refuses.
func executorFor(kind jobspec.Kind) (executor, bool) {
	switch kind {
	case jobspec.KindPush:
		return execPush, true
	case jobspec.KindInstall:
		return execInstall, true
	case jobspec.KindUninstall:
		return execUninstall, true
	case jobspec.KindShell:
		return execShell, true
	case jobspec.KindShellDetached:
		return execShellDetached, true
	case jobspec.KindWaitFor:
		return execWaitFor, true
	case jobspec.KindPull:
		return execPull, true
	case jobspec.KindAssert:
		return execAssert, true
	case jobspec.KindReset:
		return execReset, true
	case jobspec.KindSleep:
		return execSleep, true
	default:
		return nil, false
	}
}

// payloadOf type-asserts a step's payload. A mismatch is unreachable through
// jobspec's unmarshaller, which refuses a step whose kind and payload
// disagree, so it is reported as the runner bug it would be rather than
// retried against a device.
func payloadOf[T jobspec.Payload](st jobspec.Step) (T, error) {
	p, ok := st.Payload.(T)
	if !ok {
		var zero T
		return zero, notRetryablef("step %q of kind %s carries a %T payload", st.ID, st.Kind(), st.Payload)
	}
	return p, nil
}

// stepDetail is the jsonb written with a step's row: enough for an operator to
// see what was asked for without opening the spec.
func stepDetail(st jobspec.Step) map[string]any {
	d := map[string]any{}
	switch p := st.Payload.(type) {
	case jobspec.Push:
		d["sha256"], d["dest"] = p.SHA256, p.Dest
	case jobspec.Install:
		d["sha256"], d["reinstall"], d["grant"] = p.SHA256, p.Reinstall, p.Grant
	case jobspec.Uninstall:
		d["package"] = p.Package
	case jobspec.Shell:
		d["command"] = p.Command
	case jobspec.ShellDetached:
		d["command"], d["result_path"], d["handle"] = p.Command, p.ResultPath, p.Handle
	case jobspec.WaitFor:
		d["probe"], d["wait_timeout"] = p.Probe, p.Timeout.String()
	case jobspec.Pull:
		d["path"], d["artifact"] = p.Path, p.Artifact
	case jobspec.Assert:
		d["probe"], d["op"], d["value"] = p.Probe, string(p.Operator), p.Value
	case jobspec.Reset:
		d["tier"] = string(p.Tier)
	case jobspec.Sleep:
		d["duration"] = p.Duration.String()
	}
	return d
}

// ---------------------------------------------------------------------------
// shell
// ---------------------------------------------------------------------------

// execShell runs a command inline and captures its output and exit status.
//
// A stream that ends without an exit status is reported as a transport error
// and is therefore retried, which means a shell step can in principle run
// twice on a broken wire. That is deliberate, and it is why farm.step_kinds
// marks 'shell' non-idempotent: inside a step the job still holds the device,
// we are certainly the only writer, and the command was started seconds ago;
// across a process death none of that is true, so a RESUME refuses to repeat
// one while a retry accepts it. Work whose repetition would actually hurt
// belongs in shell_detached, where the device owns the outcome.
func execShell(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Shell](st)
	if err != nil {
		return nil, err
	}
	out, err := e.dev.Shell(ctx, p.Command)
	if err != nil {
		return nil, err
	}
	if !out.Exited {
		return nil, errors.New("shell stream ended without an exit status")
	}
	res := shellResult(out, e.maxOutput)
	want := e.spec.ExpectExit(p)
	if !containsInt(want, out.ExitCode) {
		res.Failure = fmt.Sprintf("exit status %d (expected %s): %s",
			out.ExitCode, formatInts(want), firstLine(res.Output))
	}
	return res, nil
}

func shellResult(out ShellOutput, limit int) *Result {
	text, total, truncated := captureText(out.Stdout, out.Stderr, limit)
	code := out.ExitCode
	res := &Result{ExitCode: &code, Output: text, Truncated: truncated}
	d := res.detail()
	d["output_bytes"] = total
	d["output_truncated"] = truncated || out.Truncated
	return res
}

func containsInt(set []int, v int) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func formatInts(set []int) string {
	parts := make([]string, len(set))
	for i, v := range set {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// shell_detached — the step this whole package is shaped around
// ---------------------------------------------------------------------------

// detachedPaths are the device-side files that make a long command's outcome
// independent of every socket in the system.
//
// The spec names ResultPath and a Handle; the log and the pid file sit beside
// the result and are named by the handle, which is what lets a process that
// has never seen this run before find it again after a reconnect or a resume.
type detachedPaths struct {
	result string // exit status, published by rename
	tmp    string // exit status, being written
	log    string // stdout and stderr
	pid    string // the worker shell's own pid
	dir    string

	// handle is the spec's own token, unsanitised. The file names above are
	// built from the sanitised form; this one is what a message shows a human,
	// because an operator greps their spec for what they wrote, not for what
	// the filename sanitiser made of it.
	handle string
}

func detachedPathsFor(p jobspec.ShellDetached) detachedPaths {
	dir := path.Dir(p.ResultPath)
	h := sanitiseHandle(p.Handle)
	return detachedPaths{
		result: p.ResultPath,
		tmp:    p.ResultPath + ".tmp",
		log:    dir + "/" + h + ".log",
		pid:    dir + "/" + h + ".pid",
		dir:    dir,
		handle: p.Handle,
	}
}

// execShellDetached starts a command that outlives every socket in the system.
//
// The command is daemonised with nohup setsid, its stdout and stderr are
// redirected to a file on the DEVICE, and its exit status is written to the
// spec's result path by an atomic rename when it finishes. The ADB call
// returns as soon as the process is launched, so from that instant nothing
// about the run depends on a connection staying up: the host can lose the
// device for ten minutes, the pod can be evicted and replaced, and the work
// carries on. A later wait_for reads the answer off the device.
//
// This is the concrete countermeasure to DeviceFarmer/STF #663. There, a
// six-hour run died because a socket did. Here the socket is not the source of
// truth for anything.
func execShellDetached(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.ShellDetached](st)
	if err != nil {
		return nil, err
	}
	hp := detachedPathsFor(p)

	out, err := e.dev.Shell(ctx, detachedCommand(e.detach, hp, p.Command))
	if err != nil {
		return nil, err
	}
	if !out.Exited {
		return nil, errors.New("shell stream ended without an exit status while starting a detached command")
	}
	if out.ExitCode != 0 {
		res := shellResult(out, e.maxOutput)
		res.Failure = fmt.Sprintf(
			"could not start the detached command (exit status %d: %s); "+
				"check that the shell user can create %s — result_path's directory is where the log, "+
				"the pid file and the exit status all have to be written",
			out.ExitCode, firstLine(res.Output), hp.dir)
		return res, nil
	}

	res := &Result{}
	d := res.detail()
	d["handle"] = p.Handle
	d["result_path"] = hp.result
	d["log_path"] = hp.log
	d["pid_path"] = hp.pid
	d["detached_with"] = e.detach

	// The pid is written by the worker shell itself, so it names the process
	// actually doing the work rather than the setsid wrapper that has already
	// exited. Reading it is best effort: it exists only so a wait_for can
	// notice a process that vanished without writing a result, and a missing
	// pid simply means that check is unavailable.
	if pid := e.readPID(ctx, hp); pid != "" {
		d["pid"] = pid
	} else {
		d["pid_unknown"] = true
		// Said out loud, because the innocent explanation (a phone slow to
		// flush one line) and the alarming one (the wrapper never execed, so
		// nothing is running and the wait_for after this will burn its whole
		// timeout) look identical in the step row.
		e.log.Warn("detached command started but wrote no pid file; "+
			"if the wait_for after this one times out, read the log on the device to see whether it ever ran",
			"step", st.ID, "handle", p.Handle, "pid_path", hp.pid, "log_path", hp.log,
			"detached_with", e.detach)
	}
	res.Output = fmt.Sprintf("started detached: handle %s, log %s, result %s",
		p.Handle, hp.log, hp.result)

	// The launch is not the outcome, and until this probe existed nothing in
	// the runner ever asked for the outcome. The exit status checked above is
	// the WRAPPER's — "did the daemonising one-liner run" — and a command that
	// starts and then dies immediately (a binary that is not on the device, a
	// script that exits on its first line, a directory that is read-only for
	// the thing being run) leaves that status at 0 and publishes its own,
	// non-zero one milliseconds later. Nobody was reading it, so that job went
	// on to sit in its wait_for until a timeout written for a six-hour soak.
	//
	// A probe that cannot reach the device is NOT a failure here, and that
	// asymmetry is deliberate: the launch already exited 0, so the wrapper is
	// on the device with the work, and the status file it publishes will still
	// be there for the next thing that looks — a wait_for, or a resume's
	// re-attach. Failing a step that did exactly what it was asked to do
	// because a socket blinked immediately afterwards is the fusion of
	// transport and outcome this whole package exists to prevent.
	pr, perr := probeDetached(ctx, e, hp)
	switch {
	case perr != nil:
		d["status_unknown"] = true
		e.log.Warn("started the detached command but could not read its status file just afterwards; "+
			"the command is running and the DEVICE keeps the answer, so this is not a failure",
			"step", st.ID, "handle", p.Handle, "result_path", hp.result, "err", perr)
	case pr.state == detachedDone:
		// It finished between the launch returning and this probe. The result
		// file is this command's own: detachedCommand removes any earlier one
		// before it starts, in the same shell invocation that just exited 0.
		code := pr.exitCode
		res.ExitCode = &code
		d["already_finished"] = true
		d["exit_code"] = code
		want := detachedExpectExit(e)
		if !containsInt(want, code) {
			res.Failure = fmt.Sprintf(
				"the detached command finished immediately with exit status %d (expected %s); "+
					"it did start, so this is the command's own verdict, not a transport failure — "+
					"its output is on the device at %s",
				code, formatInts(want), hp.log)
			return res, nil
		}
		res.Output += fmt.Sprintf(" (already finished, exit status %d)", code)
	}
	return res, nil
}

// reattachDetached decides what to do when a resume lands on a shell_detached
// step that was in flight when the previous process died.
//
// It returns a non-nil Result when the earlier start left its marks on the
// device: the command either finished (the result file is there) or is still
// running (the log is there), and in both cases starting a second copy would
// be exactly the side effect the checkpoint exists to prevent. It returns
// (nil, nil) when the device carries no trace of it — the previous process
// died between writing the checkpoint and launching anything — and the step
// then simply runs.
//
// A run that finished while we were away is JUDGED, not merely noticed. Its
// exit status is the only thing anyone was waiting for, and re-attaching to a
// six-hour soak that died in minute three and reporting that as a re-attached
// step is a green job over a red run.
func reattachDetached(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.ShellDetached](st)
	if err != nil {
		return nil, err
	}
	hp := detachedPathsFor(p)

	pr, err := probeDetached(ctx, e, hp)
	if err != nil {
		// The question was never asked. It is asked again on the next pass of
		// the retry loop this function is called from, INSIDE the lease, and
		// the answer it is asking about is a file on the device that is not
		// going anywhere.
		return nil, err
	}

	if pr.state == detachedAbsent {
		return nil, nil
	}

	res := &Result{}
	d := res.detail()
	d["handle"] = p.Handle
	d["result_path"] = hp.result
	d["log_path"] = hp.log
	d["reattached_state"] = string(pr.state)

	if pr.state == detachedRunning {
		res.Output = fmt.Sprintf("re-attached to detached handle %s (running)", p.Handle)
		return res, nil
	}

	code := pr.exitCode
	res.ExitCode = &code
	d["exit_code"] = code
	res.Output = fmt.Sprintf("re-attached to detached handle %s: it had already finished with exit status %d",
		p.Handle, code)
	want := detachedExpectExit(e)
	if !containsInt(want, code) {
		// A verdict, not an error: the device ran the command and the command
		// said no. Retrying cannot change a status that is already written to
		// a file, and the step must not be repeated — its side effects have
		// all happened.
		res.Failure = fmt.Sprintf(
			"the detached command for handle %s finished with exit status %d (expected %s) "+
				"while this job was away; its output is on the device at %s",
			p.Handle, code, formatInts(want), hp.log)
	}
	return res, nil
}

// detachedState is what the device's marks say about one handle.
type detachedState string

const (
	detachedAbsent  detachedState = "absent"  // no trace: nothing was ever started
	detachedRunning detachedState = "running" // a log, but no published status
	detachedDone    detachedState = "done"    // a status file, and it has been read
)

// detachedProbe is one answer from [probeDetached]. exitCode is meaningful
// only when state is detachedDone.
type detachedProbe struct {
	state    detachedState
	exitCode int
}

// statusRe accepts what the wrapper writes into the result file and nothing
// else. The value comes from `echo $?` in a POSIX shell, so it is one to three
// digits; anything wider was not written by the command we started.
var statusRe = regexp.MustCompile(`^[0-9]{1,3}$`)

// maxProbeAnswer bounds the bytes of an answer that are parsed.
//
// A result file holds at most four bytes. Anything at that path longer than
// this is not this wrapper's work, and splitting a megabyte of somebody else's
// file into fields to discover that would be a pointless allocation about a
// device that is already misbehaving. The cap cannot hide a valid answer: a
// valid one is its first nine bytes.
const maxProbeAnswer = 512

// probeDetached asks the device what became of a handle, in one round trip.
//
// It is the function shell_detached turns on, and the reason it reads the
// status in the SAME command that finds the file is a race: a probe that
// answered "the result file is there" and a second call that read it could see
// the file removed in between, and would then have to invent a verdict for a
// state that never existed on the device. One question, one answer, one
// snapshot of the device's own filesystem.
//
// A failure here is a failure to ASK, never an answer. It comes back as an
// ordinary error so the caller retries it inside the lease — this is the whole
// design of shell_detached: the device owns the outcome, so nothing that
// happens to a socket can change what that outcome was, and the file will
// still be there on the next poll.
func probeDetached(ctx context.Context, e *env, hp detachedPaths) (detachedProbe, error) {
	out, err := e.dev.Shell(ctx, fmt.Sprintf(
		`if [ -f %s ]; then echo done; cat %s; elif [ -f %s ]; then echo running; else echo absent; fi`,
		dq(hp.result), dq(hp.result), dq(hp.log)))
	if err != nil {
		return detachedProbe{}, err
	}
	if !out.Exited {
		return detachedProbe{}, fmt.Errorf(
			"shell stream ended without an exit status while probing detached handle %s", hp.handle)
	}

	raw := out.Stdout
	if len(raw) > maxProbeAnswer {
		raw = raw[:maxProbeAnswer]
	}

	// Only these three shapes are answers. Anything else — a shell error, a
	// truncated read, an adb banner that got demultiplexed into stdout, a
	// result file something other than this wrapper wrote — is an answer we did
	// not understand, and the one reading it must not turn into is "absent",
	// which would start a second copy of a command that may well be running
	// right now. It is returned as an ordinary transport failure instead, so
	// the probe is simply asked again inside the lease; if the device keeps
	// answering nonsense, the step's own timeout ends it, which is the correct
	// outcome for "we cannot find out what became of our work".
	//
	// The answer is also never stored raw. It reaches farm.job_steps.detail,
	// which is jsonb, and Postgres rejects the escape Go emits for a NUL with
	// SQLSTATE 22P05 — one NUL out of a half-dead shell would cost the row that
	// says what happened. What is stored is a state token from the list above
	// and an integer that matched statusRe; the unparsed text appears only in
	// the error message below, through firstLine, which sanitises it.
	fields := strings.Fields(string(raw))
	switch {
	case len(fields) == 1 && fields[0] == string(detachedAbsent):
		return detachedProbe{state: detachedAbsent}, nil
	case len(fields) == 1 && fields[0] == string(detachedRunning):
		return detachedProbe{state: detachedRunning}, nil
	case len(fields) == 2 && fields[0] == string(detachedDone) && statusRe.MatchString(fields[1]):
		code, cerr := strconv.Atoi(fields[1])
		if cerr != nil {
			// Unreachable: statusRe admits at most three digits. Reported
			// rather than ignored, because a silent zero here would be a
			// failed run recorded as a successful one.
			return detachedProbe{}, fmt.Errorf(
				"detached handle %s published exit status %q, which does not parse: %w",
				hp.handle, fields[1], cerr)
		}
		return detachedProbe{state: detachedDone, exitCode: code}, nil
	default:
		return detachedProbe{}, fmt.Errorf(
			"the probe for detached handle %s answered %q, which is none of "+
				"absent, running, or done followed by an exit status",
			hp.handle, firstLine(string(raw)))
	}
}

// detachedExpectExit is the set of exit codes a detached run may end with and
// still be a success.
//
// jobspec.ShellDetached has no expect_exit of its own — the field exists on
// Shell and not on it — so the spec-level default is the only statement its
// author can make about what counts as success, and it is what applies here.
// Hard-coding a second meaning of success for detached runs would judge the
// same command differently depending on which step kind started it.
func detachedExpectExit(e *env) []int {
	return e.spec.ExpectExit(jobspec.Shell{})
}

var pidRe = regexp.MustCompile(`^[0-9]{1,10}$`)

// readPID fetches the worker shell's pid, giving the device a moment to write
// it. The file is created by the first statement of the detached script, so it
// normally exists at once; a loaded phone can be slower.
//
// The answer is required to be digits. It is stored in farm.job_steps.detail,
// which is jsonb: whatever a half-dead shell prints instead of a pid is not
// worth losing that row to, and "not a pid" is the same fact as "no pid".
func (e *env) readPID(ctx context.Context, hp detachedPaths) string {
	const attempts = 3
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(300 * time.Millisecond):
			}
		}
		out, err := e.dev.Shell(ctx, "cat "+dq(hp.pid)+" 2>/dev/null")
		if err == nil && out.Exited {
			if pid := strings.TrimSpace(string(out.Stdout)); pidRe.MatchString(pid) {
				return pid
			}
		}
	}
	return ""
}

// detachedCommand builds the one-liner that launches a command the device owns:
//
//	mkdir -p <dir> || exit 1; rm -f <result> <tmp> <pid>
//	nohup setsid sh -c '
//	   echo $$ > <pid>
//	   ( <command> ) > <log> 2>&1
//	   echo $? > <tmp>
//	   mv <tmp> <result>
//	' < /dev/null > /dev/null 2>&1 &
//
// Five details are load-bearing:
//
//   - The mkdir is fatal. The directory comes from the spec's result_path and
//     need not be the job's scratch directory, so it can be somewhere the
//     shell user cannot write. Everything after it is backgrounded with its
//     output discarded and the command ends in `echo started`, so a directory
//     that does not exist would otherwise produce an exit status of 0 for a
//     command that never ran — and the job would then sit in a wait_for until
//     its timeout, waiting for a result nothing is going to write.
//
//   - The command runs in a SUBSHELL, not a brace group. A brace group shares
//     the wrapper's shell, so a script ending in "exit $rc" — which is how
//     most test harnesses end — would exit the wrapper too, before the status
//     was ever recorded. The result file would then never appear, and a
//     six-hour run would be reported as a command that vanished.
//
//   - The exit status is written to a temporary file and RENAMED into place,
//     so a poll can never read a half-written status and conclude the command
//     exited 0 when it exited 137.
//
//   - The background job's stdin and stdout are detached from the ADB stream.
//     Without that the shell service stays open until the command finishes,
//     which is precisely the coupling this step exists to remove.
//
//   - The pid is captured with $$ from INSIDE the worker shell rather than
//     with $! from outside, because setsid's own process exits immediately and
//     its pid would say nothing about whether the work is alive.
func detachedCommand(prefix string, hp detachedPaths, command string) string {
	inner := strings.Join([]string{
		"echo $$ > " + dq(hp.pid),
		"( " + command + " ) > " + dq(hp.log) + " 2>&1",
		"echo $? > " + dq(hp.tmp),
		"mv " + dq(hp.tmp) + " " + dq(hp.result),
	}, "; ")

	return fmt.Sprintf("mkdir -p %s || exit 1; rm -f %s %s %s; %s sh -c %s < /dev/null > /dev/null 2>&1 & echo started",
		dq(hp.dir), dq(hp.result), dq(hp.tmp), dq(hp.pid), prefix, shQuote(inner))
}

// ---------------------------------------------------------------------------
// wait_for
// ---------------------------------------------------------------------------

// execWaitFor polls a device-side probe until it succeeds.
//
// A poll that fails on the wire is COUNTED AND IGNORED. That is the whole
// point of asking the device about a file the device wrote: the connection may
// come and go while the answer sits there waiting to be read. The only thing
// that ends the wait unsatisfied is the condition's own timeout, which the
// spec wrote down.
func execWaitFor(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.WaitFor](st)
	if err != nil {
		return nil, err
	}
	interval := p.Interval.Std()
	if interval <= 0 {
		interval = e.poll
	}

	// The condition's deadline is its own, and it is shorter than the step's.
	// When it expires the step has a verdict — "the device never got there" —
	// rather than an error, so it is not retried.
	wait := ctx
	if to := p.Timeout.Std(); to > 0 {
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	var last *Result
	polls, blips := 0, 0
	for {
		polls++
		out, err := e.dev.Shell(wait, p.Probe)
		switch {
		case err == nil && out.Exited:
			// A completed probe is an answer, and it stays an answer even if
			// the condition's deadline expired while it was in flight. Testing
			// the deadline first would throw away a probe that said yes and
			// report "condition not met" about a device that had in fact got
			// there — the one wrong verdict a wait_for can produce.
			last = shellResult(out, e.maxOutput)
			if out.ExitCode == 0 {
				d := last.detail()
				d["polls"], d["poll_blips"] = polls, blips
				return last, nil
			}
		case wait.Err() != nil:
			// A deadline expired while the probe was in flight, so the error
			// in hand describes a cancelled call rather than a device. The
			// select below turns it into the right verdict — do not mistake it
			// for a refusal.
		case err != nil && !isRetryable(err):
			return nil, err
		default:
			// Unreachable device, dropped stream: the probe did not happen.
			// The condition it was asking about is unaffected.
			blips++
			if blips%10 == 1 {
				reason := "the shell stream ended without an exit status"
				if err != nil {
					reason = err.Error()
				}
				e.log.Warn("wait_for could not reach the device; the condition is unaffected and the wait continues",
					"step", st.ID, "blips", blips, "reason", reason)
			}
		}

		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				// The STEP was ended (fencing, shutdown, step timeout), not
				// just this condition.
				return last, abortErr(ctx)
			}
			if last == nil {
				last = &Result{}
			}
			d := last.detail()
			d["polls"], d["poll_blips"] = polls, blips
			// The probe is quoted so an operator can run it by hand on the
			// device, and the blip count is quoted so they can tell "the
			// condition never became true" from "we could barely reach the
			// device for the whole wait" — the same message otherwise, two
			// completely different things to go and look at.
			last.Failure = fmt.Sprintf(
				"condition not met within %s: probe %q ran %d times, %d of which could not reach the device; last answer: %s",
				p.Timeout, p.Probe, polls, blips, firstLine(last.Output))
			return last, nil
		case <-time.After(interval):
		}
	}
}

// ---------------------------------------------------------------------------
// sleep
// ---------------------------------------------------------------------------

func execSleep(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Sleep](st)
	if err != nil {
		return nil, err
	}
	d := p.Duration.Std()
	select {
	case <-ctx.Done():
		return nil, abortErr(ctx)
	case <-time.After(d):
	}
	res := &Result{Output: fmt.Sprintf("slept %s", d)}
	res.detail()["slept"] = d.String()
	return res, nil
}

// ---------------------------------------------------------------------------
// push / install / uninstall
// ---------------------------------------------------------------------------

// execPush puts an artifact's bytes at a path on the device.
func execPush(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Push](st)
	if err != nil {
		return nil, err
	}
	mode, err := parseMode(p.Mode)
	if err != nil {
		return nil, notRetryablef("step %q: %v", st.ID, err)
	}

	ens, err := e.ensureAt(ctx, p.SHA256, p.Dest, mode)
	if err != nil {
		return nil, err
	}
	if p.Mode != "" {
		// The transfer's own mode is advisory on some backends, and a script
		// that arrives without its execute bit fails later in a way that looks
		// like a bug in the script. The mode is re-rendered from the value
		// parseMode accepted rather than interpolated from the spec, so what
		// reaches the device's shell is four octal digits and can be nothing
		// else.
		//
		// Its result is CHECKED. Discarding it would mean the one step whose
		// whole job is "this file is executable afterwards" reporting success
		// on a device where the chmod was refused — and the failure would then
		// surface several steps later as a permission denied nobody can trace
		// back to here.
		chmod, err := e.dev.Shell(ctx, fmt.Sprintf("chmod %04o %s", mode.Perm(), dq(p.Dest)))
		if err != nil {
			return nil, err
		}
		if !chmod.Exited {
			return nil, errors.New("shell stream ended without an exit status while setting the pushed file's mode")
		}
		if chmod.ExitCode != 0 {
			res := shellResult(chmod, e.maxOutput)
			res.Failure = fmt.Sprintf("pushed %s but could not set mode %s on it (exit status %d: %s)",
				p.Dest, p.Mode, chmod.ExitCode, firstLine(res.Output))
			res.detail()["dest"], res.detail()["sha256"] = p.Dest, p.SHA256
			return res, nil
		}
	}

	res := &Result{Output: fmt.Sprintf("%s (%d bytes) at %s",
		ens.Artifact.Name, ens.Artifact.Size, p.Dest)}
	d := res.detail()
	d["sha256"] = p.SHA256
	d["bytes"] = ens.Artifact.Size
	d["dest"] = p.Dest
	d["pushed"] = ens.Pushed
	return res, nil
}

// ensureAt gets the content onto the device AT THIS PATH.
//
// EnsureOnDevice deliberately trusts farm.device_artifacts and skips the
// transfer when the ledger says the content is already on the device — that
// skip is what makes a 200 MB APK across 60 devices affordable. But the ledger
// records that the CONTENT is on the device, not that it is at the path this
// step names, so a skip is verified here: present at the wanted path is done,
// present elsewhere is a device-side copy (which never crosses USB), and
// absent means the ledger is stale and is corrected before pushing for real.
func (e *env) ensureAt(ctx context.Context, sha, dest string, mode fs.FileMode) (artifacts.EnsureResult, error) {
	if e.artifacts == nil {
		return artifacts.EnsureResult{}, notRetryablef(
			"this runner was built without an artifact store, so it cannot provide %s; "+
				"wire Config.Artifacts before scheduling jobs whose specs push or install", sha)
	}
	push := func(ctx context.Context, a artifacts.Artifact, blob artifacts.Blob) (string, error) {
		if err := e.mkdirParent(ctx, dest); err != nil {
			return "", err
		}
		if err := e.dev.Push(ctx, blob, dest, mode); err != nil {
			return "", err
		}
		return dest, nil
	}

	ens, err := e.artifacts.EnsureOnDevice(ctx, e.place.DeviceID, sha, push)
	if err != nil {
		return ens, classifyArtifactError(err)
	}
	if ens.Pushed {
		return ens, nil
	}

	ok, err := e.fileExists(ctx, dest)
	if err != nil {
		return ens, err
	}
	if ok {
		return ens, nil
	}

	if ens.RemotePath != "" && ens.RemotePath != dest {
		if copied, err := e.copyOnDevice(ctx, ens.RemotePath, dest); err != nil {
			return ens, err
		} else if copied {
			e.log.Info("artifact was already on the device; copied it into place instead of re-pushing",
				"sha256", sha, "from", ens.RemotePath, "to", dest)
			ens.RemotePath = dest
			return ens, nil
		}
	}

	// The ledger claims content the device does not have. Retract the claim
	// and push for real; leaving it would make every later step trust a file
	// that is not there.
	e.log.Warn("device_artifacts claimed content the device does not have; re-pushing",
		"sha256", sha, "dest", dest, "claimed_path", ens.RemotePath)
	if _, err := e.artifacts.MarkRemoved(ctx, e.place.DeviceID, sha,
		"not present on the device when a step needed it"); err != nil {
		return ens, err
	}
	ens, err = e.artifacts.EnsureOnDevice(ctx, e.place.DeviceID, sha, push)
	return ens, classifyArtifactError(err)
}

// mkdirParent creates the directory a push will land in, immediately before
// the bytes move.
//
// The sync protocol's SEND opens its destination with O_CREAT and nothing
// more: a parent that is not there is answered with FAIL and the errno text,
// after the transfer has already been set up, and the step then reports a
// refused push where the real fact is "nobody made the directory". prepare's
// scratch directory covers an install's staging path; a push step names any
// absolute path its author chose, and a directory that does not exist yet is
// the ordinary state of a device that was just reset.
//
// The three ways this can end are kept apart, because the runner does
// different things with each:
//
//   - mkdir ran and exited non-zero — a read-only mount, a parent the shell
//     user cannot write — is the device saying no, and asking again would get
//     the same answer. It is NotRetryable and the message names the directory.
//   - The stream ended before the device reported a status is the wire, not
//     the device, and stays retryable. Reading it as "the directory is there"
//     would let a bumped cable produce a directory that is not, and the push
//     would then fail somewhere far from the cause.
//   - An error from the connection itself is passed through as the adapter
//     classified it.
func (e *env) mkdirParent(ctx context.Context, dest string) error {
	parent := path.Dir(dest)
	if parent == "/" || parent == "." {
		// The root is always there, and a bare name is a spec problem the
		// push itself reports.
		return nil
	}
	out, err := e.dev.Shell(ctx, "mkdir -p "+dq(parent))
	if err != nil {
		return err
	}
	if !out.Exited {
		return errors.New("shell stream ended without an exit status while creating the push's destination directory")
	}
	if out.ExitCode != 0 {
		return notRetryablef(
			"could not create %s for the push (exit status %d: %s); the shell user cannot write there, "+
				"so the step's dest must name a directory it can",
			parent, out.ExitCode, firstLine(string(out.Stderr)+string(out.Stdout)))
	}
	return nil
}

// classifyArtifactError separates the store's permanent refusals from its
// transient ones.
//
// The runner's default is to retry, which is right for a push that died on the
// wire and wrong for a digest nobody ever uploaded: without this, a spec that
// names a missing artifact would retry patiently until the step's timeout and
// then report a timeout, sending an operator to look at cables instead of at
// the upload that never happened.
func classifyArtifactError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifacts.ErrNotFound),
		errors.Is(err, artifacts.ErrBlobMissing),
		errors.Is(err, artifacts.ErrUnknownDevice),
		errors.Is(err, artifacts.ErrTooLarge):
		return NotRetryable(err)
	}
	var corrupt *artifacts.CorruptError
	if errors.As(err, &corrupt) {
		// Bytes that do not hash to their name are a lie, and repeating a
		// request for them cannot make them true.
		return NotRetryable(err)
	}
	return err
}

func (e *env) fileExists(ctx context.Context, p string) (bool, error) {
	out, err := e.dev.Shell(ctx, "[ -f "+dq(p)+" ] && echo yes || echo no")
	if err != nil {
		return false, err
	}
	if !out.Exited {
		return false, errors.New("shell stream ended without an exit status while testing for a file")
	}
	return strings.TrimSpace(string(out.Stdout)) == "yes", nil
}

func (e *env) copyOnDevice(ctx context.Context, from, to string) (bool, error) {
	out, err := e.dev.Shell(ctx, fmt.Sprintf("mkdir -p %s && cp -f %s %s",
		dq(path.Dir(to)), dq(from), dq(to)))
	if err != nil {
		return false, err
	}
	if !out.Exited {
		return false, errors.New("shell stream ended without an exit status while copying on the device")
	}
	return out.ExitCode == 0, nil
}

// execInstall stages an APK and installs it.
//
// The staged file is left in place on purpose: farm.device_artifacts now says
// this device holds those bytes, and deleting them would make the ledger lie
// to the next job that wants the same build. Removing them is a reset's job,
// which is also where the ledger is told.
//
// pm install is checked twice, because older Android builds exit 0 while
// printing "Failure [INSTALL_FAILED_…]" on stdout: trusting the exit status
// alone reports a green install of an app that is not on the device.
func execInstall(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Install](st)
	if err != nil {
		return nil, err
	}
	staged := e.workDir + "/" + p.SHA256 + ".apk"
	ens, err := e.ensureAt(ctx, p.SHA256, staged, 0o644)
	if err != nil {
		return nil, err
	}

	flags := make([]string, 0, 2)
	if p.Reinstall {
		flags = append(flags, "-r")
	}
	if p.Grant {
		flags = append(flags, "-g")
	}
	cmd := strings.TrimSpace("pm install "+strings.Join(flags, " ")) + " " + dq(staged)

	out, err := e.dev.Shell(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if !out.Exited {
		return nil, errors.New("shell stream ended without an exit status during pm install")
	}

	res := shellResult(out, e.maxOutput)
	d := res.detail()
	d["sha256"] = p.SHA256
	d["package"] = ens.Artifact.Package
	d["staged_at"] = staged
	d["pushed"] = ens.Pushed

	if out.ExitCode != 0 || strings.Contains(res.Output, "Failure") {
		res.Failure = fmt.Sprintf("pm install %s: exit status %d: %s",
			ens.Artifact.Name, out.ExitCode, firstLine(res.Output))
	}
	return res, nil
}

// execUninstall removes a package. Removing one that is not there is a
// success: the step's contract is "this package is absent afterwards", and
// farm.step_kinds calls it idempotent on exactly that reading.
func execUninstall(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Uninstall](st)
	if err != nil {
		return nil, err
	}
	out, err := e.dev.Shell(ctx, "pm uninstall "+dq(p.Package))
	if err != nil {
		return nil, err
	}
	if !out.Exited {
		return nil, errors.New("shell stream ended without an exit status during pm uninstall")
	}
	res := shellResult(out, e.maxOutput)
	res.detail()["package"] = p.Package

	if out.ExitCode == 0 && !strings.Contains(res.Output, "Failure") {
		return res, nil
	}
	lower := strings.ToLower(res.Output)
	if strings.Contains(lower, "not installed") || strings.Contains(lower, "unknown package") {
		res.detail()["already_absent"] = true
		res.Output = fmt.Sprintf("package %s was not installed", p.Package)
		return res, nil
	}
	res.Failure = fmt.Sprintf("pm uninstall %s: exit status %d: %s",
		p.Package, out.ExitCode, firstLine(res.Output))
	return res, nil
}

// ---------------------------------------------------------------------------
// pull
// ---------------------------------------------------------------------------

// execPull copies a file off the device and stores it as an artifact.
//
// Unlike push and install, this names content that does not exist yet: the
// digest is not known until the last byte has been read, so the bytes are
// streamed straight from the device into the content-addressed store and the
// resulting sha256 is recorded on the step row. Nothing is buffered in the
// runner — a two-gigabyte screen recording is a pipe, not an allocation.
func execPull(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Pull](st)
	if err != nil {
		return nil, err
	}
	if e.artifacts == nil {
		return nil, notRetryablef(
			"this runner was built without an artifact store, so %q pulled from %s has nowhere to go; "+
				"wire Config.Artifacts before scheduling jobs whose specs pull", p.Artifact, p.Path)
	}

	pr, pw := io.Pipe()
	pulled := make(chan error, 1)
	go func() {
		err := e.dev.Pull(ctx, p.Path, pw)
		// CloseWithError(nil) is a plain Close, so the reader sees EOF on
		// success and the device's own error on failure.
		_ = pw.CloseWithError(err)
		pulled <- err
	}()

	put, putErr := e.artifacts.Put(ctx, pr, artifacts.KindFile, p.Artifact)
	// Unblock the pulling goroutine if the store stopped reading early.
	_ = pr.CloseWithError(putErr)
	pullErr := <-pulled

	if pullErr != nil {
		return nil, pullErr
	}
	if putErr != nil {
		return nil, classifyArtifactError(fmt.Errorf("store %q pulled from %s: %w",
			p.Artifact, p.Path, putErr))
	}

	res := &Result{Output: fmt.Sprintf("pulled %s (%d bytes) as %s sha256:%s",
		p.Path, put.Size, p.Artifact, put.SHA256)}
	d := res.detail()
	d["path"] = p.Path
	d["artifact"] = p.Artifact
	d["sha256"] = put.SHA256
	d["bytes"] = put.Size
	d["deduplicated"] = !put.Inserted
	return res, nil
}

// ---------------------------------------------------------------------------
// assert
// ---------------------------------------------------------------------------

// execAssert runs a probe and fails the job unless it answers what the spec
// expects. Its failure is a verdict about the WORK, never about the wire, so
// it goes into Result.Failure and is not retried.
func execAssert(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Assert](st)
	if err != nil {
		return nil, err
	}
	out, err := e.dev.Shell(ctx, p.Probe)
	if err != nil {
		return nil, err
	}
	if !out.Exited {
		return nil, errors.New("shell stream ended without an exit status during assert")
	}
	res := shellResult(out, e.maxOutput)
	got := strings.TrimSpace(res.Output)

	ok, cmpErr := compare(got, p.Operator, p.Value)
	if cmpErr != nil {
		return nil, cmpErr
	}
	d := res.detail()
	d["got"] = got
	d["op"] = string(p.Operator)
	d["want"] = p.Value
	if !ok {
		res.Failure = fmt.Sprintf("assert %s: %q %s %q is false",
			st.ID, got, p.Operator, p.Value)
	}
	return res, nil
}

// compare implements the nine operators jobspec defines. A malformed operator
// or an unparseable number is a spec problem, not a device problem, so it is
// returned as a non-retryable error rather than as a failed assertion — "your
// comparison is broken" and "the device said the wrong thing" call for
// different reactions from whoever reads the row.
func compare(got string, op jobspec.Operator, want string) (bool, error) {
	switch op {
	case jobspec.OpEQ:
		return got == want, nil
	case jobspec.OpNE:
		return got != want, nil
	case jobspec.OpContains:
		return strings.Contains(got, want), nil
	case jobspec.OpNotContains:
		return !strings.Contains(got, want), nil
	case jobspec.OpMatches:
		re, err := regexp.Compile(want)
		if err != nil {
			return false, notRetryablef("assert pattern %q does not compile: %v", want, err)
		}
		return re.MatchString(got), nil
	}

	if !op.Numeric() {
		return false, notRetryablef("unknown assert operator %q", op)
	}
	lhs, err := strconv.ParseFloat(strings.TrimSpace(got), 64)
	if err != nil {
		// The device answered something that is not a number where the spec
		// asked for a numeric comparison. That IS a failed assertion, and
		// saying so is more useful than a type error.
		return false, nil
	}
	rhs, err := strconv.ParseFloat(strings.TrimSpace(want), 64)
	if err != nil {
		return false, notRetryablef("assert value %q is not a number for operator %s", want, op)
	}
	switch op {
	case jobspec.OpGT:
		return lhs > rhs, nil
	case jobspec.OpGE:
		return lhs >= rhs, nil
	case jobspec.OpLT:
		return lhs < rhs, nil
	case jobspec.OpLE:
		return lhs <= rhs, nil
	}
	return false, notRetryablef("unknown assert operator %q", op)
}

// ---------------------------------------------------------------------------
// reset
// ---------------------------------------------------------------------------

// execReset returns the device to a known state.
//
// What each tier MEANS is not decided here: jobspec.ResetSteps expands a tier
// into the ordered steps that perform it, from the profile's package list, and
// this executor runs that expansion. Keeping the meaning in one place is what
// lets the API show an operator exactly what a 'medium' reset will do to a
// device before anybody presses the button.
//
// Two things the expansion cannot do for itself are done here. The device-side
// state script has to be written before the step that runs it, because its
// content comes from farm.device_state and is not known when the spec is
// written. And a reset that removes packages invalidates the artifact ledger,
// which must be told — otherwise the next job skips a push for content this
// device no longer has.
func execReset(ctx context.Context, e *env, st jobspec.Step) (*Result, error) {
	p, err := payloadOf[jobspec.Reset](st)
	if err != nil {
		return nil, err
	}
	tier := p.Tier
	if tier == "" {
		tier = jobspec.ResetTier(e.resetTier)
	}

	res := &Result{}
	d := res.detail()
	d["tier"] = string(tier)

	if tier == jobspec.TierNone {
		res.Output = "reset tier 'none': the spec asked for no reset"
		return res, nil
	}

	pkgs, err := e.profilePackages(ctx)
	if err != nil {
		return nil, err
	}
	steps, err := jobspec.ResetSteps(tier, pkgs)
	if err != nil {
		return nil, notRetryablef("%v", err)
	}
	d["profile"] = e.profileID
	d["profile_packages"] = len(pkgs)
	d["sub_steps"] = len(steps)

	if len(steps) == 0 {
		// A soft reset of a job with no profile expands to nothing. Reporting
		// an unexplained green step would be a lie of omission: the operator
		// who set reset_tier believes the device was cleaned, and it was not.
		res.Output = fmt.Sprintf(
			"reset tier %s expanded to no steps: this job names no profile, "+
				"so there is no package list to clear — set farm.jobs.profile_id "+
				"if this device is supposed to be reset between jobs", tier)
		e.log.Warn("reset did nothing because the job has no profile",
			"tier", string(tier), "job_id", e.place.JobID)
		return res, nil
	}

	if usesDeviceStateScript(steps) {
		if err := e.writeDeviceStateScript(ctx); err != nil {
			return nil, err
		}
	}

	// The artifact ledger is told BEFORE the wipe, not after. Its whole value
	// is that a 'present' row can be trusted to skip a 200 MB transfer, so the
	// dangerous direction is a claim that outlives the content — and a reset
	// that fails halfway has still uninstalled whatever it got to. Retracting
	// first costs at most one redundant push; retracting last risks a later job
	// running against a file that is not there.
	if tier == jobspec.TierMedium || tier == jobspec.TierHard {
		n, ferr := e.artifactsForget(ctx, fmt.Sprintf("%s reset", tier))
		if ferr != nil {
			// The reset goes ahead anyway: refusing here would leave the device
			// dirty AND the ledger stale. The claims that survive are not fatal
			// either, because ensureAt verifies the file at the path it wants
			// before trusting a skip and retracts a claim it finds untrue — this
			// only costs one wasted verification per artifact on the next job.
			e.log.Error("could not retract this device's artifact claims before a reset; "+
				"the reset continues and stale 'present' rows will be corrected by the next step that needs them, "+
				"but check farm.device_artifacts for this device if pushes start looking wrong",
				"tier", string(tier), "device_id", e.place.DeviceID, "err", ferr)
			d["ledger_error"] = ferr.Error()
		} else {
			d["ledger_forgotten"] = n
		}
	}

	var trail strings.Builder
	for _, sub := range steps {
		budget := e.subTimeout(sub)
		subCtx, cancel := context.WithTimeoutCause(ctx, budget, ErrStepTimeout)
		out, err := e.runSub(subCtx, sub)
		cancel()

		switch {
		case err != nil && isAbort(ctx):
			return nil, abortErr(ctx)
		case err != nil && subCtx.Err() != nil:
			// The SUB-STEP's own deadline expired while the enclosing step is
			// still live. That is not transport noise and must not be laundered
			// into one: left unclassified it would be retried, and the reset
			// would silently run its whole expansion again — pm clear, uninstall
			// sweep, reboot — until the enclosing step's timeout, then report a
			// timeout instead of the sub-step that actually hung.
			return nil, notRetryablef(
				"reset tier %s: step %s did not finish within its %s budget on this device; "+
					"raise that step's timeout in jobspec.ResetSteps or look at why this device is that slow",
				tier, sub.ID, budget)
		case err != nil:
			// A transport failure here reaches the runner's retry loop, which
			// re-runs the whole reset. That is safe precisely because a reset
			// is idempotent — farm.step_kinds says so — and it is why the
			// expansion is a list of small steps rather than one long script.
			return nil, fmt.Errorf("reset tier %s, step %s: %w", tier, sub.ID, err)
		}
		fmt.Fprintf(&trail, "%s: %s\n", sub.ID, firstLine(out.Output))
		if out.Failure != "" && !sub.ContinueOnError {
			res.Output, _, _ = captureText([]byte(trail.String()), nil, e.maxOutput)
			res.Failure = fmt.Sprintf("reset tier %s failed at %s: %s", tier, sub.ID, out.Failure)
			return res, nil
		}
	}

	res.Output, _, _ = captureText([]byte(trail.String()), nil, e.maxOutput)
	return res, nil
}

// runSub executes one step of a reset expansion.
//
// The sub-step runs against a copy of the environment carrying an EMPTY spec,
// so the job's own default_expect_exit cannot reach it. A job that declares
// exit 1 a success for its own shell steps must not thereby declare a failed
// `pm clear` a successful reset — the expansion's expectations are its own.
func (e *env) runSub(ctx context.Context, sub jobspec.Step) (*Result, error) {
	exec, ok := executorFor(sub.Kind())
	if !ok {
		return nil, notRetryablef("reset expansion produced unknown kind %q", sub.Kind())
	}
	inner := *e
	inner.spec = jobspec.Spec{}
	res, err := exec(ctx, &inner, sub)
	if err != nil {
		return nil, err
	}
	if res == nil {
		res = &Result{}
	}
	return res, nil
}

func (e *env) subTimeout(sub jobspec.Step) time.Duration {
	if d := sub.Timeout.Std(); d > 0 {
		return d
	}
	return e.poll * 12
}

func (e *env) artifactsForget(ctx context.Context, reason string) (int64, error) {
	if e.artifacts == nil {
		return 0, nil
	}
	return e.artifacts.ForgetDevice(ctx, e.place.DeviceID, reason)
}

func usesDeviceStateScript(steps []jobspec.Step) bool {
	for _, st := range steps {
		if sh, ok := st.Payload.(jobspec.Shell); ok &&
			strings.Contains(sh.Command, jobspec.DeviceStateScript) {
			return true
		}
	}
	return false
}

func (e *env) profilePackages(ctx context.Context) ([]string, error) {
	if e.profileID == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, e.callTO)
	defer cancel()

	var pkgs []string
	err := e.pool.QueryRow(cctx,
		`SELECT COALESCE(p.packages, '{}') FROM farm.profiles p WHERE p.id = $1`,
		e.profileID).Scan(&pkgs)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, notRetryablef("job names profile %q, which does not exist", e.profileID)
	case err != nil:
		return nil, fmt.Errorf("read profile %s: %w", e.profileID, err)
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// ---------------------------------------------------------------------------
// Durable device state
// ---------------------------------------------------------------------------

// deviceState is the shape of farm.device_state.state.
//
// It is the durable, per-device configuration a reset must put back: the farm
// re-applies it rather than remembering how a device was set up by hand, which
// is what makes a device interchangeable after a wipe. Anything not in this
// shape is refused rather than ignored, because a silently skipped setting is
// a test suite that starts failing for reasons nobody can reproduce.
type deviceState struct {
	// Settings are Android settings, by namespace: global, system or secure.
	Settings map[string]map[string]string `json:"settings,omitempty"`

	// Commands are device-side shell lines applied after the settings, for
	// state that `settings put` cannot express.
	Commands []string `json:"commands,omitempty"`
}

var settingKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// writeDeviceStateScript materialises the script a medium or hard reset runs.
//
// It cannot be a push step: push names content by hash, and this content is
// not known until the device's current row has been read. The script always
// exists after this returns, even when there is no state to apply — the reset
// step that runs it fails loudly on a missing file, which is right, because a
// missing script would mean the durable state was silently not re-applied.
func (e *env) writeDeviceStateScript(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, e.callTO)
	defer cancel()

	var raw []byte
	var revision int64
	err := e.pool.QueryRow(cctx,
		`SELECT state, revision FROM farm.device_state WHERE device_id = $1::uuid`,
		e.place.DeviceID).Scan(&raw, &revision)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		raw, revision = []byte(`{}`), 0
	case err != nil:
		return fmt.Errorf("read device_state for %s: %w", e.place.DeviceID, err)
	}

	script, err := renderDeviceState(e.place.DeviceID, revision, raw,
		fmt.Sprintf("job %s attempt %d", e.place.JobID, e.attempt))
	if err != nil {
		return err
	}

	// jobspec.DeviceStateScript lives outside the job's scratch directory, so
	// prepare has not created its parent. adbd's sync service does create
	// missing parents on a SEND, but that is a property of one implementation
	// of Conn rather than of the interface, and a reset that silently fails to
	// reapply durable state is the exact outcome the vocabulary calls
	// unacceptable. One round trip on medium and hard resets buys certainty.
	dir := path.Dir(jobspec.DeviceStateScript)
	if out, err := e.dev.Shell(ctx, "mkdir -p "+dq(dir)); err != nil {
		return err
	} else if !out.Exited {
		return errors.New("shell stream ended without an exit status while creating the device state directory")
	} else if out.ExitCode != 0 {
		return notRetryablef("could not create %s on the device, so farm.device_state cannot be reapplied: %s",
			dir, firstLine(string(out.Stderr)+string(out.Stdout)))
	}

	if err := e.dev.Push(ctx, strings.NewReader(script), jobspec.DeviceStateScript, 0o755); err != nil {
		return err
	}
	e.log.Info("wrote the device state script",
		"path", jobspec.DeviceStateScript, "revision", revision, "bytes", len(script))
	return nil
}

// renderDeviceState turns a device_state row into a shell script.
//
// Keys are validated and values are quoted. The row is written by the control
// plane, but it is still data arriving at a shell on a device somebody else's
// lease may depend on, and "trusted source" is not a reason to interpolate an
// unchecked string into a command.
func renderDeviceState(deviceID string, revision int64, raw []byte, by string) (string, error) {
	var st deviceState
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return "", notRetryablef(
			"farm.device_state.state for %s is not a device state document (%v); "+
				"it may hold only \"settings\" (global/system/secure) and \"commands\" — "+
				"fix the row with farm.device_state_write rather than letting a reset guess at it",
			deviceID, err)
	}

	var b strings.Builder
	b.WriteString("#!/system/bin/sh\n")
	// Whoever finds this file on a device at 3am should not have to guess which
	// run put it there, or how old the state inside it is.
	fmt.Fprintf(&b, "# device-farmer: durable state for %s, revision %d, written by %s\n",
		deviceID, revision, by)
	b.WriteString("rc=0\n")

	namespaces := make([]string, 0, len(st.Settings))
	for ns := range st.Settings {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	applied := 0
	for _, ns := range namespaces {
		switch ns {
		case "global", "system", "secure":
		default:
			return "", notRetryablef("device_state for %s names settings namespace %q; it must be global, system or secure",
				deviceID, ns)
		}
		keys := make([]string, 0, len(st.Settings[ns]))
		for k := range st.Settings[ns] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !settingKeyRe.MatchString(k) {
				return "", notRetryablef("device_state for %s has setting key %q, which is not a settings key",
					deviceID, k)
			}
			fmt.Fprintf(&b, "settings put %s %s %s || rc=1\n", ns, k, shQuote(st.Settings[ns][k]))
			applied++
		}
	}
	for _, cmd := range st.Commands {
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		fmt.Fprintf(&b, "%s || rc=1\n", cmd)
		applied++
	}
	if applied == 0 {
		b.WriteString("# no durable state is recorded for this device\n")
	}
	b.WriteString("exit $rc\n")
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Device-side plumbing
// ---------------------------------------------------------------------------

// deviceWorkDir is the per-job scratch directory. It is keyed on the job id so
// that two jobs sharing a device over time never read each other's files.
func deviceWorkDir(root, jobID string) string {
	return strings.TrimRight(root, "/") + "/" + sanitiseHandle(jobID)
}

// prepareCommand creates the scratch directory and reports whether the device
// has setsid, in one round trip.
func prepareCommand(dir string) string {
	return fmt.Sprintf(
		"mkdir -p %s || exit 1; if command -v setsid >/dev/null 2>&1; then echo setsid; else echo nosetsid; fi",
		dq(dir))
}

// detachPrefix picks the strongest daemonising prefix the device supports.
//
// nohup alone survives the shell's exit; setsid additionally puts the command
// in a new session, so it survives the whole ADB shell process group going
// away — which is what happens when the transport drops. Devices without
// setsid still get nohup, and the step row records which was used, because
// "the job died when somebody bumped the cable" is otherwise a mystery.
//
// An answer that is neither token falls back to nohup rather than to setsid,
// and the asymmetry is deliberate. Guessing setsid onto a device that does not
// have it does not fail loudly: the wrapper is launched in the background with
// its output discarded, so `nohup setsid …` failing to exec leaves the start
// command exiting 0 while nothing runs at all, and the job then waits out a
// whole wait_for timeout for a result no process is ever going to write.
// Guessing nohup onto a device that does have setsid costs only the weaker
// guarantee.
func detachPrefix(probe string) string {
	switch {
	case strings.Contains(probe, "nosetsid"):
		return "nohup"
	case strings.Contains(probe, "setsid"):
		return "nohup setsid"
	default:
		return "nohup"
	}
}

var handleSafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitiseHandle reduces a name to characters that need no quoting in any
// shell. Device-side paths are built by concatenation and interpolated into a
// script, so this is what keeps a job id from becoming a command.
func sanitiseHandle(s string) string {
	s = handleSafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if len(s) > 96 {
		s = s[:96]
	}
	return s
}

// dqUnsafe are the four characters a double-quoted word still interprets in a
// POSIX shell.
var dqUnsafe = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"$", `\$`,
	"`", "\\`",
)

// dq double-quotes a device-side path.
//
// jobspec validates every path in a SPEC against a restricted alphabet, so
// most of what reaches here has nothing in it for a shell to expand. But not
// all of it does: Config.WorkRoot is operator-supplied, and ensureAt quotes a
// remote path read back out of farm.device_artifacts. Escaping the four
// characters a double-quoted word still interprets costs nothing and removes
// the question of whether every future caller checked its input first — the
// alternative is a command running as shell on a device that is holding
// somebody's six-hour lease.
func dq(s string) string { return `"` + dqUnsafe.Replace(s) + `"` }

// shQuote wraps an arbitrary string — including a user's command — so the
// device's shell sees it as one literal word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func parseMode(s string) (fs.FileMode, error) {
	if strings.TrimSpace(s) == "" {
		return 0o644, nil
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal", s)
	}
	return fs.FileMode(v) & fs.ModePerm, nil
}

const stderrBanner = "\n--- stderr ---\n"

// captureText bounds and sanitises device output for storage.
//
// The bound is applied to the BYTE SLICES, before anything is converted to a
// string. A single instrumented test can emit hundreds of megabytes on stdout,
// and building the whole thing as a Go string only to keep the first 64 KiB of
// it would copy every one of those bytes into the heap — twice, once for the
// conversion and once for the concatenation — on a host that is running sixty
// of these at a time.
func captureText(stdout, stderr []byte, limit int) (text string, total int, truncated bool) {
	total = len(stdout) + len(stderr)

	out, errOut := stdout, stderr
	if limit > 0 {
		if len(out) > limit {
			out, truncated = out[:limit], true
		}
		switch room := limit - len(out) - len(stderrBanner); {
		case len(errOut) == 0:
		case room <= 0:
			errOut, truncated = nil, true
		case len(errOut) > room:
			errOut, truncated = errOut[:room], true
		}
	}

	var b strings.Builder
	b.Grow(len(out) + len(errOut) + len(stderrBanner))
	b.Write(out)
	if len(errOut) > 0 {
		b.WriteString(stderrBanner)
		b.Write(errOut)
	}

	// Truncating by bytes can cut a rune in half; sanitiseText runs afterwards
	// so the fragment becomes a replacement character rather than a sequence
	// Postgres refuses.
	s := sanitiseText(b.String())
	if truncated {
		s += fmt.Sprintf("\n… [truncated: %d bytes captured, %d stored]", total, limit)
	}
	return s, total, truncated
}

// sanitiseText makes device bytes storable in a Postgres text column.
//
// Postgres rejects U+0000 outright — "invalid byte sequence for encoding UTF8:
// 0x00" — and the output of a process that crashed is full of them. Passing
// device bytes straight through would lose the whole row to a single NUL, and
// that row is the one an operator reads at 3am. Invalid UTF-8 goes the same
// way, for the same reason.
func sanitiseText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}
