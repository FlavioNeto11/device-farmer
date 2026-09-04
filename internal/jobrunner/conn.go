package jobrunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/runner"
)

// DeviceConn is runner.Conn over one ADB server and one physical position.
//
// # It is deliberately a translation and nothing else
//
// There is no retry here, no timeout of its own, no reconnect, no health
// bookkeeping, no counter of consecutive failures. Every one of those would be
// a policy decision, and the only component that can make those decisions
// correctly is the runner: it is the one that knows the step's own timeout,
// which the job's author wrote down. A five-second timeout invented here would
// silently overrule a thirty-minute budget a user chose deliberately, and a
// retry loop here would hide from the runner exactly the transport noise it is
// built to absorb.
//
// The value of that thinness is what the type CANNOT do. A DeviceConn holds no
// lease, no database handle and no allocation state; a socket error inside it
// has one destination, its immediate caller, and there is no method on it that
// could turn one into a released device.
//
// # Position addressing
//
// The devpath is fixed at construction and validated there. Every call the
// adapter makes carries it, so no call can be retargeted by anything the
// device reports about itself. An ADB serial is evidence, not an identifier:
// duplicate OEM serials are real, and a serial-addressed push could land a
// 200 MB APK on a device three hours into somebody else's run.
type DeviceConn struct {
	cli     *adbwire.Client
	devpath string
}

var _ runner.Conn = (*DeviceConn)(nil)

// NewDeviceConn binds cli to devpath.
//
// The devpath is validated here rather than on first use so that a device with
// no recorded USB position is refused before a lease is committed to it, and
// with an error naming the position rather than the wire.
func NewDeviceConn(cli *adbwire.Client, devpath string) (*DeviceConn, error) {
	if cli == nil {
		return nil, errors.New("jobrunner: no adb client for the device connection")
	}
	if err := adbwire.ValidateDevpath(devpath); err != nil {
		return nil, fmt.Errorf("jobrunner: device connection: %w", err)
	}
	return &DeviceConn{cli: cli, devpath: devpath}, nil
}

// Devpath is the physical position every call on this connection targets.
func (d *DeviceConn) Devpath() string { return d.devpath }

// Endpoint is the ADB server this connection speaks to, for audit rows.
func (d *DeviceConn) Endpoint() string { return d.cli.Endpoint() }

// Shell runs one command to completion.
//
// The result is passed through exactly as the wire reported it, including the
// case where the stream ended before the device sent an exit frame. That case
// arrives here as Exited=false with an ExitCode of -1, and it is NOT turned
// into an error: the runner inspects Exited at every call site and decides
// what to do with it. Synthesising a zero exit for a truncated stream is how a
// bumped cable becomes a green test run.
func (d *DeviceConn) Shell(ctx context.Context, command string) (runner.ShellOutput, error) {
	res, err := d.cli.Shell(ctx, d.devpath, command)

	// -1 rather than 0 for the no-result case, matching adbwire's own
	// convention: a connection that failed before the command ran must not be
	// mistakable for a command that succeeded.
	out := runner.ShellOutput{ExitCode: -1}
	if res != nil {
		// Partial output on a failed call is still worth returning: it is
		// often the last thing the device said before the socket went away.
		out = runner.ShellOutput{
			Stdout:    res.Stdout,
			Stderr:    res.Stderr,
			ExitCode:  res.ExitCode,
			Exited:    res.Exited,
			Truncated: res.Truncated,
		}
	}
	return out, classify(err)
}

// Push writes r to remote on the device.
func (d *DeviceConn) Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error {
	return classify(d.cli.Push(ctx, d.devpath, r, remote, mode))
}

// Pull copies remote off the device into w.
func (d *DeviceConn) Pull(ctx context.Context, remote string, w io.Writer) error {
	return classify(d.cli.Pull(ctx, d.devpath, remote, w))
}

// classify is the whole judgement this file makes, and it makes it in one
// direction: an error is retryable unless there is positive evidence that
// retrying is pointless.
//
// That default is not laziness, it is the invariant. A transport failure must
// be retried inside the lease the job still holds, so anything unrecognised is
// left unwrapped and the runner retries it within the step's own budget. Only
// a refusal that will still be a refusal next time is marked with
// runner.NotRetryable, because retrying one of those burns the step's timeout
// to arrive at the same answer — and a FAIL is not by itself such a refusal,
// which is what transientRefusal is for.
//
// The subtle case is [adbwire.ErrNotFound]. It is a FAIL response, so it looks
// like a refusal, but it means "the ADB server has no transport at that
// position RIGHT NOW" — a USB re-enumeration, a device rebooting mid-job, a
// hub that flapped. That is the exact condition this farm exists to tolerate,
// so it is retryable. Marking it permanent would fail a step for a device that
// came back four seconds later, which is DeviceFarmer/STF #663 rebuilt one
// step at a time.
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case adbwire.IsTransport(err):
		// The socket. Retried inside the lease, always.
		return err

	case adbwire.IsCanceled(err):
		// Our own context ended the call: the step's timeout, the job's
		// max_runtime, or the holder losing the lease. The runner reads the
		// cause off the context and must see this unmodified.
		return err

	case adbwire.IsNotFound(err):
		// One observation of absence, not a state. See above.
		return err

	case errors.Is(err, adbwire.ErrAmbiguousTarget):
		// A devpath is a position in the USB tree and cannot be ambiguous, so
		// this means the ADB server's view of the topology is broken. No
		// number of retries fixes that, and the step should fail with a
		// message a human can act on rather than after a silent half hour.
		return runner.NotRetryable(err)

	case adbwire.IsProtocol(err):
		// A well-formed refusal in the server's own words — but read the words
		// before believing them. See transientRefusal.
		if transientRefusal(err) {
			return err
		}
		return runner.NotRetryable(err)

	default:
		var ue *adbwire.UsageError
		if errors.As(err, &ue) {
			// Caught before anything touched the wire: a malformed devpath, a
			// request too large to frame. A caller bug, not a device fault.
			return runner.NotRetryable(err)
		}
		return err
	}
}

// transientRefusals are the ADB server's own words for "that transport is not
// usable at this instant", as opposed to "what you asked for is wrong".
//
// The distinction is invisible in the type system, because adbwire's
// failToError only lifts two reasons out of the FAIL text — ambiguous and not
// found — and files everything else as a plain [adbwire.ProtocolError]. So
// "device offline" and "no such user" arrive as the same Go type, and treating
// the whole class as permanent is a mistake with teeth:
//
//   - Every reset tier above 'none' reboots the phone. From the reboot until
//     the transport re-enumerates, the server answers "device offline" — and
//     right after that, while the RSA handshake completes, "device
//     unauthorized" or "device still authorizing". Those are the seconds a
//     wait_for exists to sit through.
//   - A USB hub that renegotiates, a phone that drops to fastboot and back, a
//     `adb wait-for-device` race after an install: all of them speak in this
//     vocabulary, all of them clear on their own.
//
// Marked permanent, any one of them fails the step the instant it is seen
// ([runner.isRetryable] stops at ErrNotRetryable), the runner reports the job
// failed, and the supervisor releases the device — a phone that was healthy
// four seconds later. That is DeviceFarmer/STF #663 rebuilt one step at a
// time, which is the failure this whole package is arranged to make
// impossible.
//
// Retrying costs nothing that was not already budgeted: the bound is the
// step's own timeout, a number the job's author wrote down. Absent from this
// list, deliberately, is "insufficient permissions" — a udev rule that is
// wrong stays wrong, and burning a step's whole budget to rediscover that
// sends an operator to look at cables instead of at the host.
var transientRefusals = []string{
	"offline",
	"unauthorized",
	"still authorizing",
	"still connecting",
	"connecting to",
	"protocol fault",
	"closed",
}

// transientRefusal reports whether a FAIL describes a transport that is
// momentarily unusable rather than a request that is wrong.
//
// It matches the server's Reason field rather than err.Error(), so a service
// string or a devpath that happens to contain one of these words cannot make a
// genuine refusal look transient.
func transientRefusal(err error) bool {
	var pe *adbwire.ProtocolError
	if !errors.As(err, &pe) {
		return false
	}
	reason := strings.ToLower(pe.Reason)
	for _, s := range transientRefusals {
		if strings.Contains(reason, s) {
			return true
		}
	}
	return false
}
