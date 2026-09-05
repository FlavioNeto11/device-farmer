package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// The directory a push lands in is made right before the bytes move — and a
// device that refuses to make it is told apart from a wire that dropped.

// pushingStore is a store whose ledger never has the content: every ensure
// pushes, through the function the runner handed it.
func pushingStore() *fakeArtifacts {
	store := &fakeArtifacts{}
	store.ensure = func(_, sha string, push artifacts.PushFunc) (artifacts.EnsureResult, error) {
		if _, err := push(context.Background(), artifacts.Artifact{SHA256: sha}, nil); err != nil {
			return artifacts.EnsureResult{}, err
		}
		return artifacts.EnsureResult{
			Artifact: artifacts.Artifact{SHA256: sha, Name: "app.bin", Size: 9}, Pushed: true,
		}, nil
	}
	return store
}

func TestPushMakesTheDestinationDirectoryBeforeTheTransfer(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("d", 64)
	const dest = "/sdcard/Download/new dir/app.bin"

	var order []string
	dev := &fakeConn{}
	dev.shell = func(_ context.Context, _ int, cmd string) (ShellOutput, error) {
		order = append(order, cmd)
		return okShell(""), nil
	}
	dev.push = func(_ context.Context, _ io.Reader, remote string, _ fs.FileMode) error {
		order = append(order, "push "+remote)
		return nil
	}
	e := testEnv(dev, func(e *env) { e.artifacts = pushingStore() })

	res, err := execPush(stepCtx(t), e, jobspec.Step{ID: "p",
		Payload: jobspec.Push{SHA256: sha, Dest: dest}})
	if err != nil {
		t.Fatalf("execPush: %v", err)
	}
	if res.Failure != "" {
		t.Fatalf("Failure = %q", res.Failure)
	}
	want := []string{`mkdir -p "/sdcard/Download/new dir"`, "push " + dest}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Fatalf("device saw:\n%s\nwant, in this order:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
}

// A file at the root has no directory to make. Nothing else in the spec can
// produce this — Dest is validated absolute — but the branch exists and a
// round trip that does nothing is still a round trip.
func TestPushAtTheRootMakesNoDirectory(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{
		push: func(context.Context, io.Reader, string, fs.FileMode) error { return nil },
	}
	e := testEnv(dev, func(e *env) { e.artifacts = pushingStore() })
	if _, err := execPush(stepCtx(t), e, jobspec.Step{ID: "p",
		Payload: jobspec.Push{SHA256: strings.Repeat("e", 64), Dest: "/app.bin"}}); err != nil {
		t.Fatalf("execPush: %v", err)
	}
	for _, cmd := range dev.commands() {
		if strings.HasPrefix(cmd, "mkdir") {
			t.Fatalf("a push to the root tried to make a directory: %q", cmd)
		}
	}
}

// The classification is the point. A device that refused is a fact about the
// device and retrying it would burn the step's budget to hear it again; a
// stream that ended without a status is the wire, and the runner's default
// for the wire — retry inside the lease — must apply.
func TestPushKeepsARefusedMkdirApartFromADroppedOne(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("f", 64)
	const dest = "/system/priv-app/new/app.bin"
	step := jobspec.Step{ID: "p", Payload: jobspec.Push{SHA256: sha, Dest: dest}}

	run := func(t *testing.T, shell func() (ShellOutput, error)) (bool, error) {
		t.Helper()
		var pushed bool
		dev := &fakeConn{
			shell: func(context.Context, int, string) (ShellOutput, error) { return shell() },
			push:  func(context.Context, io.Reader, string, fs.FileMode) error { pushed = true; return nil },
		}
		e := testEnv(dev, func(e *env) { e.artifacts = pushingStore() })
		_, err := execPush(stepCtx(t), e, step)
		return pushed, err
	}

	t.Run("the device refused", func(t *testing.T) {
		t.Parallel()
		pushed, err := run(t, func() (ShellOutput, error) {
			return ShellOutput{Stderr: []byte("mkdir: '/system/priv-app/new': Read-only file system\n"),
				ExitCode: 1, Exited: true}, nil
		})
		if !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("err = %v, want a non-retryable refusal", err)
		}
		for _, want := range []string{"/system/priv-app/new", "Read-only file system", "exit status 1"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to carry %q", err, want)
			}
		}
		if pushed {
			t.Fatal("the bytes were pushed into a directory the device refused to make")
		}
	})

	t.Run("the stream ended without a status", func(t *testing.T) {
		t.Parallel()
		pushed, err := run(t, func() (ShellOutput, error) { return ShellOutput{}, nil })
		if err == nil {
			t.Fatal("a stream with no exit status was read as a directory that exists")
		}
		if errors.Is(err, ErrNotRetryable) {
			t.Fatalf("err = %v; a dropped stream is the wire and must stay retryable", err)
		}
		if pushed {
			t.Fatal("the bytes were pushed after a mkdir nobody saw finish")
		}
	})

	t.Run("the connection failed", func(t *testing.T) {
		t.Parallel()
		boom := transportErr("connection reset by peer")
		pushed, err := run(t, func() (ShellOutput, error) { return ShellOutput{}, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the connection's own error passed through", err)
		}
		if errors.Is(err, ErrNotRetryable) {
			t.Fatalf("err = %v; the adapter's classification was overruled", err)
		}
		if pushed {
			t.Fatal("the bytes were pushed after the connection failed")
		}
	})
}
