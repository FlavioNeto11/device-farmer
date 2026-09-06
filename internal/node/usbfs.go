// Opening a usbfs device node, and the one question about it that is not
// Linux's: what a failed open MEANS to the recovery ladder.
//
// The ioctl and everything around it lives in hostops.go behind
// //go:build linux, because usbfs exists on exactly one platform. This does
// not, for two reasons. The answer it produces belongs to the contract in
// api.go — the table that says which status codes mean the port was touched —
// and not to the kernel. And it is the half that can be proved anywhere: there
// is no /dev/bus/usb on a build machine, on a laptop or in CI, so a
// classification written inside the Linux file is a branch no test on this
// project has ever been able to reach. Both branches below were wrong for that
// reason.

package node

import (
	"errors"
	"fmt"
	"os"
)

// devBusUSB holds the usbfs nodes the ioctl is issued against, laid out as
// /dev/bus/usb/<busnum>/<devnum> with both numbers zero-padded to three digits
// by the kernel's own device naming.
//
// A variable rather than a constant so a test can point the classification at a
// tree it built, the same way sysfsUSBDevices exists for the blast-radius
// check: whether a missing node means "this device is gone" or "this host has
// no usbfs at all" is decided by what is here, and that decision has to be
// provable on a machine with no USB bus. Nothing writes it outside tests.
var devBusUSB = "/dev/bus/usb"

// usbfsOpen opens a usbfs device node for the ioctl in resetNode.
//
// Write-only because usbfs requires write access for the commands that change
// device state; a read-only descriptor gets EACCES from the ioctl rather than
// from the open, which is a confusing place to discover a permissions problem.
func usbfsOpen(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, usbfsOpenError(path, err)
	}
	return f, nil
}

// usbfsOpenError classifies a failed open of a usbfs node. Two of the three
// answers are refusals, and saying so is the whole point of this function.
//
// A PERMISSION DENIAL is a refusal. No descriptor was obtained, so no ioctl was
// issued and the port is bit-for-bit as it was — which is the definition api.go
// gives a refusal, and the distinction is not bookkeeping. An error carrying no
// sentinel reaches [recovery.ClassifyHostFault] as DispositionFailed, the
// ladder does not hand a failed rung back, and its next cycle answers "tier 3
// ran and did not help" by cutting VBUS at tier 4 and then quarantining — on a
// handset whose only problem was a file mode. Filed as ErrRefused it is a 409
// with reason [ReasonPolicy], the rung stays available for the moment the udev
// rule is fixed, and the sentence below is what farm.recovery_attempts.detail
// hands whoever has to fix it.
//
// ErrNotSupported would be the wrong word for that one even though it, too,
// reads as a refusal: api.go renders it to operators as "this build of the
// agent cannot perform the rung at all", and an ownership or udev problem is a
// fact about this host, not about this binary. The same rung on the same build
// works the moment the device nodes land in the right group.
//
// NO USBFS AT ALL is exactly the case ErrNotSupported does describe, and it is
// how an agent says "I am the software half only". A container given /sys but
// not /dev/bus/usb discovers topology and enrols devices perfectly well and can
// never issue this ioctl; without this branch every tier-3 call on such a host
// is an ENOENT carrying no sentinel — a rung recorded as attempted-and-failed
// against hardware nobody touched, which is the same escalation as above by a
// different road. The root is consulted only after an open has already failed
// for want of the node, so a host whose usbfs is there never pays for it.
//
// Every other open failure is deliberately left as a failed rung. A node that
// is missing while /dev/bus/usb is there means the device sysfs named a moment
// ago re-enumerated or dropped off, which IS a statement about the hardware,
// and the ladder should be free to act on it.
func usbfsOpenError(path string, err error) error {
	switch {
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("node: %w: cannot open %s for writing: %w — the agent needs "+
			"write access to usbfs, which is normally a udev rule putting the device "+
			"nodes in the farm's group; nothing was sent to the device, so this is a "+
			"refusal and not a rung that failed", ErrRefused, path, err)

	case errors.Is(err, os.ErrNotExist) && !isDir(devBusUSB):
		return fmt.Errorf("node: %w: this host has no usbfs at %s, so there is no device "+
			"node to issue USBDEVFS_RESET against and tier 3 cannot be performed here at "+
			"all; on bare metal that means usbcore is not loaded, and in a container it "+
			"means /dev/bus/usb was not mounted in — discovery and enrollment need only "+
			"/sys and are unaffected", ErrNotSupported, devBusUSB)
	}
	return fmt.Errorf("node: open %s: %w", path, err)
}

// isDir reports whether path is a directory this process can stat. Every other
// answer — missing, a plain file, a permission error on the way there — is
// false, because the only caller is asking "is there a usbfs on this host", and
// anything it cannot confirm must not be read as one.
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
