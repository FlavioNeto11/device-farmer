package jobrunner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/runner"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// DeviceConn is the one place a socket error meets the runner's vocabulary, and
// the judgement it makes goes in exactly one direction: an error is RETRYABLE
// unless there is positive evidence that retrying is pointless.
//
// Getting that backwards is DeviceFarmer/STF #663 rebuilt one step at a time —
// a phone that was healthy four seconds later, a step failed, a job failed, a
// device released. Every test below is written against that.

const testTimeout = 30 * time.Second

func testContext(tb testing.TB) context.Context {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	tb.Cleanup(cancel)
	return ctx
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// shellReply frames a shell v2 answer the way a device would.
func shellReply(tb testing.TB, stdout, stderr string, code byte) string {
	tb.Helper()
	var b bytes.Buffer
	for _, p := range []struct {
		id      adbwire.ShellPacketID
		payload []byte
	}{
		{adbwire.ShellStdout, []byte(stdout)},
		{adbwire.ShellStderr, []byte(stderr)},
		{adbwire.ShellExit, []byte{code}},
	} {
		if len(p.payload) == 0 && p.id != adbwire.ShellExit {
			continue
		}
		if err := adbwire.WriteShellPacket(&b, p.id, p.payload); err != nil {
			tb.Fatalf("framing the scripted reply: %v", err)
		}
	}
	return b.String()
}

func deviceConn(tb testing.TB, endpoint, devpath string) *DeviceConn {
	tb.Helper()
	d, err := NewDeviceConn(adbwire.New(endpoint, adbwire.WithLogger(quietLogger())), devpath)
	if err != nil {
		tb.Fatalf("NewDeviceConn: %v", err)
	}
	return d
}

// retryable is the question every assertion below really asks: would the runner
// retry this inside the lease the job still holds?
func retryable(err error) bool {
	return err != nil && !errors.Is(err, runner.ErrNotRetryable)
}

// ---------------------------------------------------------------------------
// Position addressing
// ---------------------------------------------------------------------------

// The devpath is fixed at construction and carried on every call, so no call
// can be retargeted by anything the device reports about itself.
func TestDeviceConnRefusesAnythingThatIsNotAPosition(t *testing.T) {
	t.Parallel()

	if _, err := NewDeviceConn(nil, "usb:3-1.4"); err == nil {
		t.Fatal("NewDeviceConn accepted a nil client")
	}
	// An ADB serial is evidence, not an identifier. Duplicate OEM serials are
	// real, and a serial-addressed push could land a 200 MB APK on a device
	// three hours into somebody else's run.
	for _, bad := range []string{"", fakeadb.CloneSerial, "emulator-5554", "usb:", "3-1.4"} {
		if _, err := NewDeviceConn(adbwire.New("127.0.0.1:1"), bad); err == nil {
			t.Fatalf("NewDeviceConn accepted %q as a device address", bad)
		}
	}
}

// Two devices sharing one OEM serial is the trap this whole addressing scheme
// exists for. A devpath-addressed command must reach exactly one of them.
func TestCommandsReachOneCloneAndNotItsTwin(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	srv.Respond(fakeadb.CloneDevpathB, "shell,v2,raw:", shellReply(t, "B\n", "", 0))

	dev := deviceConn(t, srv.Addr(), fakeadb.CloneDevpathB)
	out, err := dev.Shell(testContext(t), "getprop ro.serialno")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if string(out.Stdout) != "B\n" || !out.Exited || out.ExitCode != 0 {
		t.Fatalf("out = %+v, want the reply scripted for clone B", out)
	}
	if n := len(srv.RequestsTo(fakeadb.CloneDevpathA)); n != 0 {
		t.Fatalf("clone A received %d request(s); a command landed on the wrong phone", n)
	}
	if dev.Devpath() != fakeadb.CloneDevpathB || dev.Endpoint() != srv.Addr() {
		t.Fatalf("Devpath = %q, Endpoint = %q", dev.Devpath(), dev.Endpoint())
	}
}

// ---------------------------------------------------------------------------
// The wire
// ---------------------------------------------------------------------------

func TestShellPassesTheDevicesAnswerThroughUnchanged(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SHELL01", Devpath: devpath}))
	srv.Respond(devpath, "shell,v2,raw:", shellReply(t, "FAILURES!!!\n", "warning\n", 9))

	out, err := deviceConn(t, srv.Addr(), devpath).Shell(testContext(t), "am instrument -w x/.R")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if !out.Exited || out.ExitCode != 9 {
		t.Fatalf("exited = %t, code = %d, want true/9", out.Exited, out.ExitCode)
	}
	if string(out.Stdout) != "FAILURES!!!\n" || string(out.Stderr) != "warning\n" {
		t.Fatalf("stdout = %q, stderr = %q", out.Stdout, out.Stderr)
	}
}

// A connection that died before the device sent an exit frame reports -1 and
// Exited=false, never a synthesised zero. Synthesising a zero exit for a
// truncated stream is how a bumped cable becomes a green test run.
func TestASeveredStreamIsNeverMistakableForASuccessfulCommand(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.2"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "RESET01", Devpath: devpath}))
	srv.Respond(devpath, "shell,v2,raw:", shellReply(t, "half the output\n", "", 0))
	// Sever partway through the reply, with a RST: the exact shape of #663.
	srv.ResetNext("shell", 8)

	out, err := deviceConn(t, srv.Addr(), devpath).Shell(testContext(t), "sleep 1")
	if err == nil {
		t.Fatalf("a severed stream reported success: %+v", out)
	}
	if out.Exited {
		t.Fatal("Exited is true for a stream that never carried an exit frame")
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 for a call that never got an answer", out.ExitCode)
	}
	if !retryable(err) {
		t.Fatalf("a severed socket was marked permanent: %v", err)
	}
	if errors.Is(err, lease.ErrFenced) {
		t.Fatalf("a socket error was reported as fencing: %v", err)
	}
}

// The caller's own context ending a call is the step's timeout, the job's
// max_runtime or the holder losing the lease. The runner reads the cause off
// the context, so this must reach it unmodified — and it must not be permanent,
// because a deadline INSIDE the transport is transport noise.
func TestAHangingDeviceEndsAtTheCallersDeadlineAndStaysRetryable(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.3"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "HANG01", Devpath: devpath}))
	srv.HangNext("shell")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	out, err := deviceConn(t, srv.Addr(), devpath).Shell(ctx, "sleep 3600")
	if err == nil {
		t.Fatal("a device that never answered reported success")
	}
	if !adbwire.IsCanceled(err) {
		t.Fatalf("err = %v, want the cancellation reported as one", err)
	}
	if !retryable(err) {
		t.Fatalf("a cancelled call was marked permanent: %v", err)
	}
	if out.ExitCode != -1 || out.Exited {
		t.Fatalf("out = %+v, want no answer at all", out)
	}
}

// "The ADB server has no transport at that position RIGHT NOW" is a USB
// re-enumeration, a device rebooting mid-job, a hub that flapped. It is the
// exact condition this farm exists to tolerate, so it is retried.
func TestAMissingTransportIsOneObservationOfAbsenceAndIsRetried(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "GONE01", Devpath: "usb:3-1.5"}))

	// A position the server currently knows nothing about.
	_, err := deviceConn(t, srv.Addr(), "usb:9-9.9").Shell(testContext(t), "true")
	if err == nil {
		t.Fatal("a missing transport reported success")
	}
	if !adbwire.IsNotFound(err) {
		t.Fatalf("err = %v (%T), want a not-found refusal", err, err)
	}
	if !retryable(err) {
		t.Fatalf("a device that may be back in four seconds was written off permanently: %v", err)
	}
}

// The ADB server's refusals are free text, and adbwire files everything it
// cannot name as a plain ProtocolError. So "device offline" and "no such user"
// arrive as the same Go type, and treating the whole class as permanent fails a
// step for a phone that is merely mid-reboot.
func TestFailRepliesAreReadBeforeTheyAreBelieved(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		reason        string
		wantRetryable bool
	}{
		// Every reset tier above 'none' reboots the phone. These are the
		// seconds a wait_for exists to sit through.
		{"device offline", true},
		{"device unauthorized", true},
		{"device still authorizing", true},
		{"device still connecting", true},
		{"connecting to daemon", true},
		{"protocol fault (couldn't read status)", true},
		{"connection closed by remote host", true},

		// A udev rule that is wrong stays wrong. Burning a step's whole budget
		// to rediscover that sends an operator to look at cables instead of at
		// the host.
		{"insufficient permissions for device", false},
		{"unknown host service", false},
		{"unknown command", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			const devpath = "usb:4-1.1"
			srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "FAIL01", Devpath: devpath}))
			srv.FailNext("shell", tc.reason)

			_, err := deviceConn(t, srv.Addr(), devpath).Shell(testContext(t), "true")
			if err == nil {
				t.Fatal("a FAIL reply reported success")
			}
			if got := retryable(err); got != tc.wantRetryable {
				t.Fatalf("retryable(%v) = %t, want %t", err, got, tc.wantRetryable)
			}
		})
	}
}

// classify's remaining arms, driven directly: they describe conditions the fake
// cannot produce through a devpath-addressed call, which is itself the point.
func TestClassifyMarksOnlyRealRefusalsPermanent(t *testing.T) {
	t.Parallel()

	if classify(nil) != nil {
		t.Fatal("classify invented an error out of success")
	}

	// A devpath is a position in the USB tree and cannot be ambiguous, so this
	// means the ADB server's view of the topology is broken. No number of
	// retries fixes that, and the step should fail with a message a human can
	// act on rather than after a silent half hour.
	ambiguous := &adbwire.AmbiguousTargetError{Target: fakeadb.CloneSerial, BySerial: true, Reason: "more than one device"}
	if retryable(classify(ambiguous)) {
		t.Fatal("an ambiguous target was retried; the topology will not fix itself")
	}
	if !errors.Is(classify(ambiguous), adbwire.ErrAmbiguousTarget) {
		t.Fatal("classify lost the ambiguity")
	}

	// Caught before anything touched the wire: a caller bug, not a device fault.
	usage := &adbwire.UsageError{Op: "sync_push", Detail: "remote path is not absolute", Value: "tmp/x"}
	if retryable(classify(usage)) {
		t.Fatal("a caller bug was retried against the device")
	}

	// Anything unrecognised is retried. That default IS the invariant.
	if !retryable(classify(errors.New("something nobody has classified"))) {
		t.Fatal("an unclassified failure was treated as fatal")
	}
}

// ---------------------------------------------------------------------------
// File transfer
// ---------------------------------------------------------------------------

func TestPushAndPullRoundTripThroughThePosition(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.1"
	const remote = "/data/local/tmp/device-farmer/run.sh"
	payload := []byte("#!/system/bin/sh\nexec /system/bin/logcat -d\n")

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNC01", Devpath: devpath})
	dev := deviceConn(t, srv.Addr(), devpath)
	ctx := testContext(t)

	if err := dev.Push(ctx, bytes.NewReader(payload), remote, 0o755); err != nil {
		t.Fatalf("Push: %v", err)
	}
	f, ok := srv.File(devpath, remote)
	if !ok || !bytes.Equal(f.Data, payload) {
		t.Fatalf("the device does not hold what was pushed: ok = %t", ok)
	}

	var got bytes.Buffer
	if err := dev.Pull(ctx, remote, &got); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("Pull returned %q", got.Bytes())
	}
}

// A transfer severed mid-stream is the wire, and the wire is retried inside the
// lease. A 200 MB APK that died at 180 MB is not a job failure.
func TestATransferSeveredMidStreamIsRetried(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.2"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNC02", Devpath: devpath})
	srv.ResetSyncAfter(fakeadb.SyncSend, 1)

	dev := deviceConn(t, srv.Addr(), devpath)
	big := bytes.Repeat([]byte("x"), 3*fakeadb.SyncDataMax)

	err := dev.Push(testContext(t), bytes.NewReader(big), "/data/local/tmp/big.apk", 0o644)
	if err == nil {
		t.Fatal("a severed transfer reported success")
	}
	if !retryable(err) {
		t.Fatalf("a severed transfer was marked permanent: %v", err)
	}
	if errors.Is(err, lease.ErrFenced) {
		t.Fatalf("a transfer failure was reported as fencing: %v", err)
	}
}

// A refusal the device will repeat is not worth a step's whole budget.
func TestATransferTheDeviceRefusesIsNotRetriedForever(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.3"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNC03", Devpath: devpath})
	srv.FailSyncNext(fakeadb.SyncSend, "Permission denied")

	err := deviceConn(t, srv.Addr(), devpath).Push(
		testContext(t), strings.NewReader("x"), "/system/xbin/nope", 0o755)
	if err == nil {
		t.Fatal("a refused transfer reported success")
	}
	if retryable(err) {
		t.Fatalf("a refusal that will refuse again was retried: %v", err)
	}
}

// transientRefusal reads the server's Reason field, not the whole error string,
// so a service string or a devpath that happens to contain one of these words
// cannot make a genuine refusal look transient.
func TestTransientRefusalReadsTheReasonAndNotTheServiceString(t *testing.T) {
	t.Parallel()

	sneaky := &adbwire.ProtocolError{
		Op:      "shell",
		Service: "shell,v2,raw:echo offline",
		Devpath: "usb:3-1.4",
		Reason:  "insufficient permissions for device",
	}
	if transientRefusal(sneaky) {
		t.Fatal("a genuine refusal was read as transient because the command mentioned 'offline'")
	}
	if retryable(classify(sneaky)) {
		t.Fatal("a genuine refusal was retried")
	}

	real := &adbwire.ProtocolError{Op: "shell", Service: "shell,v2,raw:true", Reason: "device offline"}
	if !transientRefusal(real) {
		t.Fatal("a phone mid-reboot was written off")
	}
	if transientRefusal(errors.New("device offline")) {
		t.Fatal("transientRefusal answered about an error that is not a FAIL reply")
	}
}
