package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// Device-side plumbing: the preparation round trip, the strings this package
// interpolates into a shell running on somebody's leased phone, and the
// sanitising that keeps a step row storable in Postgres.

// ---------------------------------------------------------------------------
// prepare
// ---------------------------------------------------------------------------

func TestPrepareProbesForSetsidAndCreatesTheScratchDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		stdout string
		want   string
	}{
		{"a device with setsid", "setsid\n", "nohup setsid"},
		{"a device without it", "nosetsid\n", "nohup"},
		// Guessing setsid onto a device that does not have it does not fail
		// loudly: the wrapper is backgrounded with its output discarded, so the
		// start command exits 0 while nothing runs at all, and the job then
		// waits out a whole wait_for timeout for a result nobody will write.
		// The fallback is therefore the WEAKER prefix, deliberately.
		{"an answer that is neither", "* daemon not running\n", "nohup"},
		{"no answer at all", "", "nohup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testRunner(t, nil)
			dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
				return okShell(tc.stdout), nil
			}}
			e := testEnv(dev, func(e *env) { e.detach = "" })

			if err := r.prepare(stepCtx(t), r.log, e, time.Second); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if e.detach != tc.want {
				t.Fatalf("detach = %q, want %q", e.detach, tc.want)
			}
			cmd := dev.commands()[0]
			if !strings.Contains(cmd, `mkdir -p "`+e.workDir+`" || exit 1`) {
				t.Fatalf("prepare command = %q, want a fatal mkdir of the scratch directory", cmd)
			}
			if !strings.Contains(cmd, "command -v setsid") {
				t.Fatalf("prepare command = %q, want the setsid probe in the same round trip", cmd)
			}
		})
	}
}

// A device that refuses is a placement problem, not a wire problem: retrying it
// would burn the step's budget to arrive at the same answer, and the message
// has to tell an operator what to change.
func TestPrepareRefusesADirectoryTheShellUserCannotWrite(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{Stderr: []byte("mkdir: Permission denied\n"), ExitCode: 1, Exited: true}, nil
	}}
	e := testEnv(dev, nil)

	err := r.prepare(stepCtx(t), r.log, e, time.Second)
	if !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("err = %v, want a non-retryable refusal", err)
	}
	if !strings.Contains(err.Error(), "Config.WorkRoot") {
		t.Fatalf("err = %v, want it to name the setting an operator would change", err)
	}
	if dev.calls() != 1 {
		t.Fatalf("a refusal was retried %d times", dev.calls()-1)
	}
}

// A device that is briefly unreachable at the start of a job is not a job
// failure: prepare is retried like any other device call, inside the lease.
func TestPrepareRetriesAnUnreachableDeviceInsideTheLease(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
		if call <= 3 {
			return ShellOutput{}, transportErr("device offline")
		}
		return okShell("setsid\n"), nil
	}}
	e := testEnv(dev, func(e *env) { e.detach = "" })

	if err := r.prepare(stepCtx(t), r.log, e, 10*time.Second); err != nil {
		t.Fatalf("prepare gave up on a device that came back: %v", err)
	}
	if dev.calls() != 4 {
		t.Fatalf("calls = %d, want 4", dev.calls())
	}
}

// A stream that ended before the device reported a status is a wire failure
// wearing a success's clothes. Reading it as "the directory exists" would let a
// bumped cable produce a work directory that is not there, and every later step
// would fail somewhere far away from the cause. The bound on retrying it is the
// step's own budget.
func TestPrepareNeverTakesATruncatedStreamForASuccess(t *testing.T) {
	t.Parallel()

	r, _ := testRunner(t, nil)
	dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
		return ShellOutput{Stdout: []byte("set")}, nil // no exit status, ever
	}}
	e := testEnv(dev, func(e *env) { e.detach = "" })

	err := r.prepare(stepCtx(t), r.log, e, 60*time.Millisecond)
	if !errors.Is(err, ErrStepTimeout) {
		t.Fatalf("err = %v, want the step's own budget to be what stopped it", err)
	}
	if dev.calls() < 2 {
		t.Fatalf("calls = %d; a truncated stream was accepted instead of retried", dev.calls())
	}
	if e.detach != "" {
		t.Fatalf("detach = %q, want nothing decided from a stream that never finished", e.detach)
	}
}

// ---------------------------------------------------------------------------
// Quoting: these strings run as shell on a device holding somebody's lease
// ---------------------------------------------------------------------------

func TestDoubleQuotingEscapesEverythingAQuotedWordStillInterprets(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"/data/local/tmp/x", `"/data/local/tmp/x"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"$HOME", `"\$HOME"`},
		{"`id`", "\"\\`id\\`\""},
		{"$(id)", `"\$(id)"`},
	} {
		if got := dq(tc.in); got != tc.want {
			t.Fatalf("dq(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellQuotingMakesAUserCommandOneLiteralWord(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"echo hi", `'echo hi'`},
		{"it's", `'it'\''s'`},
		{"$(id); rm -rf /", `'$(id); rm -rf /'`},
	} {
		if got := shQuote(tc.in); got != tc.want {
			t.Fatalf("shQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Device-side paths are built by concatenation and interpolated into a script,
// so this is what keeps a job id from becoming a command.
func TestHandlesAreReducedToCharactersNoShellInterprets(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"soak", "soak"},
		{"a b/c", "a-b-c"},
		{"; rm -rf /", "rm--rf"},
		{"$(id)", "id"},
		{"--edges--", "edges"},
	} {
		if got := sanitiseHandle(tc.in); got != tc.want {
			t.Fatalf("sanitiseHandle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := sanitiseHandle(strings.Repeat("x", 400)); len(got) != 96 {
		t.Fatalf("sanitiseHandle of a very long name is %d characters, want 96", len(got))
	}
}

// The scratch directory is keyed on the job id so two jobs sharing a device
// over time never read each other's files.
func TestDeviceWorkDirIsPerJobAndSafeToInterpolate(t *testing.T) {
	t.Parallel()

	got := deviceWorkDir("/data/local/tmp/device-farmer/", "9f2b/../etc")
	if got != "/data/local/tmp/device-farmer/9f2b-..-etc" {
		t.Fatalf("deviceWorkDir = %q", got)
	}
	if deviceWorkDir("/root", "a") == deviceWorkDir("/root", "b") {
		t.Fatal("two jobs share one scratch directory")
	}
}

func TestParseModeAcceptsOnlyOctal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want uint32
	}{
		{"", 0o644},
		{"   ", 0o644},
		{"0755", 0o755},
		{"0o755", 0o755},
		{"600", 0o600},
	} {
		got, err := parseMode(tc.in)
		if err != nil {
			t.Fatalf("parseMode(%q): %v", tc.in, err)
		}
		if uint32(got.Perm()) != tc.want {
			t.Fatalf("parseMode(%q) = %o, want %o", tc.in, got.Perm(), tc.want)
		}
	}
	for _, bad := range []string{"rwx", "0o9", "-1", "8"} {
		if _, err := parseMode(bad); err == nil {
			t.Fatalf("parseMode(%q) was accepted", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Storing what the device said
// ---------------------------------------------------------------------------

// Postgres rejects U+0000 outright, and the output of a process that crashed is
// full of them. Passing device bytes straight through would lose the whole row
// to a single NUL — and that row is the one an operator reads at 3am.
func TestCapturedOutputIsAlwaysStorable(t *testing.T) {
	t.Parallel()

	text, total, truncated := captureText([]byte("out\x00put"), []byte("err\x00or"), 4096)
	if strings.ContainsRune(text, 0) {
		t.Fatalf("a NUL survived into a text column: %q", text)
	}
	if !strings.Contains(text, "output") || !strings.Contains(text, "error") {
		t.Fatalf("text = %q, want both streams", text)
	}
	if !strings.Contains(text, stderrBanner) {
		t.Fatal("stderr was folded into stdout with no marker")
	}
	if total != 13 || truncated {
		t.Fatalf("total = %d, truncated = %t", total, truncated)
	}

	// Truncating by bytes can cut a rune in half; the fragment must become a
	// replacement character rather than a sequence Postgres refuses.
	text, _, _ = captureText([]byte("héllo"), nil, 2)
	if !utf8.ValidString(text) {
		t.Fatalf("captureText produced invalid UTF-8: %q", text)
	}
}

func TestCapturedOutputIsBoundedAndSaysSo(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("a", 10_000)
	text, total, truncated := captureText([]byte(big), []byte(big), 64)
	if !truncated {
		t.Fatal("20 KB of output was stored unbounded")
	}
	if total != 20_000 {
		t.Fatalf("total = %d, want the real size reported even though it was not stored", total)
	}
	if !strings.Contains(text, "truncated: 20000 bytes captured, 64 stored") {
		t.Fatalf("text = %q, want the drop spelled out", text)
	}

	// A limit of zero means no bound: the caller has already decided.
	text, _, truncated = captureText([]byte(big), nil, 0)
	if truncated || len(text) != len(big) {
		t.Fatalf("captureText with no limit truncated at %d bytes", len(text))
	}
}

func TestFirstLineIsOneStorableLine(t *testing.T) {
	t.Parallel()

	if got := firstLine("  Failure [X]\nstack\nmore\n"); got != "Failure [X]" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("a\x00b"); strings.ContainsRune(got, 0) {
		t.Fatalf("firstLine kept a NUL: %q", got)
	}
	long := firstLine(strings.Repeat("z", maxMessageLine+500))
	if len(long) > maxMessageLine+len("…") {
		t.Fatalf("firstLine returned %d bytes, want it bounded", len(long))
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatal("a truncated message did not say it was truncated")
	}
}

// ---------------------------------------------------------------------------
// Durable device state
// ---------------------------------------------------------------------------

// The row is written by the control plane, but it is still data arriving at a
// shell on a device somebody else's lease may depend on. "Trusted source" is
// not a reason to interpolate an unchecked string into a command.
func TestDeviceStateScriptRefusesAnythingItCannotSafelyRender(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"a namespace that is not a settings namespace", `{"settings":{"root":{"a":"1"}}}`, "global, system or secure"},
		{"a key that is not a settings key", `{"settings":{"global":{"a b;rm -rf /":"1"}}}`, "not a settings key"},
		{"a document shape nobody defined", `{"reboot":true}`, "device state document"},
		{"not a document at all", `[]`, "device state document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderDeviceState("dev-1", 4, []byte(tc.raw), "job x attempt 1")
			if !errors.Is(err, ErrNotRetryable) {
				t.Fatalf("err = %v, want a non-retryable refusal", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDeviceStateScriptQuotesValuesAndIsOrdered(t *testing.T) {
	t.Parallel()

	raw := `{"settings":{"system":{"screen_off_timeout":"600000"},` +
		`"global":{"window_animation_scale":"0","name":"it's fine"}},` +
		`"commands":["svc wifi enable","   "]}`

	script, err := renderDeviceState("dev-1", 7, []byte(raw), "job j attempt 2")
	if err != nil {
		t.Fatalf("renderDeviceState: %v", err)
	}
	if !strings.HasPrefix(script, "#!/system/bin/sh\n") {
		t.Fatalf("script has no shebang:\n%s", script)
	}
	// Whoever finds this file on a device at 3am should not have to guess which
	// run put it there, or how old the state inside it is.
	if !strings.Contains(script, "revision 7") || !strings.Contains(script, "job j attempt 2") {
		t.Fatalf("script header says nothing about its provenance:\n%s", script)
	}
	if !strings.Contains(script, `settings put global name 'it'\''s fine' || rc=1`) {
		t.Fatalf("a value with a quote in it was not quoted:\n%s", script)
	}
	if !strings.Contains(script, "svc wifi enable || rc=1") {
		t.Fatalf("commands were not applied:\n%s", script)
	}
	if strings.Count(script, "|| rc=1") != 4 {
		t.Fatalf("want 4 applied lines (3 settings + 1 command), got:\n%s", script)
	}
	if !strings.HasSuffix(script, "exit $rc\n") {
		t.Fatalf("the script does not report whether it worked:\n%s", script)
	}

	// Deterministic ordering, so the same row renders the same script and a
	// diff between two devices means something.
	again, err := renderDeviceState("dev-1", 7, []byte(raw), "job j attempt 2")
	if err != nil || again != script {
		t.Fatal("renderDeviceState is not deterministic")
	}
	if strings.Index(script, "settings put global") > strings.Index(script, "settings put system") {
		t.Fatalf("namespaces are not in a stable order:\n%s", script)
	}
}

// A device with nothing recorded still gets a script, and the script says why
// it is empty. The reset step that runs it fails loudly on a missing file,
// which is right: a missing script means the durable state was silently not
// reapplied.
func TestDeviceStateScriptExistsEvenWithNothingToApply(t *testing.T) {
	t.Parallel()

	script, err := renderDeviceState("dev-1", 0, []byte(`{}`), "job j attempt 1")
	if err != nil {
		t.Fatalf("renderDeviceState: %v", err)
	}
	if !strings.Contains(script, "no durable state is recorded") {
		t.Fatalf("script = %q", script)
	}
	if !strings.HasSuffix(script, "exit $rc\n") {
		t.Fatalf("script = %q", script)
	}
}

// The script is only written when the expansion actually runs it: one round
// trip is worth certainty on a medium or hard reset, and worth nothing on a
// soft one.
func TestTheDeviceStateScriptIsWrittenOnlyWhenTheResetRunsIt(t *testing.T) {
	t.Parallel()

	soft, err := jobspec.ResetSteps(jobspec.TierSoft, []string{"com.example.app"})
	if err != nil {
		t.Fatalf("ResetSteps(soft): %v", err)
	}
	if usesDeviceStateScript(soft) {
		t.Fatal("a soft reset would write the device state script")
	}

	medium, err := jobspec.ResetSteps(jobspec.TierMedium, []string{"com.example.app"})
	if err != nil {
		t.Fatalf("ResetSteps(medium): %v", err)
	}
	if !usesDeviceStateScript(medium) {
		t.Fatal("a medium reset would run the device state script without writing it")
	}
}
