//go:build linux

// The Linux half of farmd-node. Everything in this file talks to sysfs, to
// /dev/bus/usb, or to the kernel directly, so it exists only on the platform
// where those mean something. On every other GOOS the agent keeps the
// unsupportedPlatform value from agent.go and refuses tiers 3 and 4 with a
// reason instead of silently succeeding.

package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Wiring the real platform in from init keeps agent.go free of build tags: the
// portable file declares the seam and its honest refusal, and this file
// replaces it wherever the operations actually exist.
func init() { platform = linuxPlatform{} }

type linuxPlatform struct{}

// sysfsUSBDevices holds one directory per USB device, named by its position:
// "3-1.4.2" is port 2 of the hub on port 4 of the hub on port 1 of bus 3. That
// name is exactly farm.slots.usb_path, which is why a devpath can be mapped to
// hardware without guessing anything.
//
// A variable rather than a constant so a test can point the blast-radius check
// at a directory it built: what checkBlastRadius refuses, and with which
// sentinel, is decided by what it finds here, and that decision has to be
// provable on a build machine with no USB bus. Nothing writes it outside tests.
var sysfsUSBDevices = "/sys/bus/usb/devices"

const (
	// devBusUSB holds the usbfs nodes the ioctl is issued against, laid out as
	// /dev/bus/usb/<busnum>/<devnum> with both numbers zero-padded to three
	// digits by the kernel's own device naming.
	devBusUSB = "/dev/bus/usb"

	// resetReturnWindow caps how long tier 3 waits for the device to come
	// back. A USBDEVFS_RESET re-enumerates in place and is done in a second or
	// two; a phone still missing after ten is not going to be repaired by this
	// rung, and holding the ladder's action budget open pretending otherwise
	// only delays the rung that would have worked.
	resetReturnWindow = 10 * time.Second

	// procOSRelease is uname -r without calling uname(2).
	//
	// syscall.Utsname.Release is [65]int8 on amd64 and [65]uint8 on arm64, so
	// a uname-based reader needs per-architecture conversion code to produce
	// the same string. The proc file is identical text on every Linux
	// architecture and needs none.
	procOSRelease = "/proc/sys/kernel/osrelease"
)

// usbPathRE mirrors the CHECK constraint on farm.slots.usb_path exactly. It is
// also the only sanitisation these paths get before they are joined onto
// /sys and /dev, and it admits neither a slash nor a dot-dot.
var usbPathRE = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)

// usbPathOf turns adbwire's devpath form ("usb:3-1.4.2") into the sysfs name
// ("3-1.4.2").
//
// The "usb:" prefix is how adb spells a position-addressed target, and the
// remainder is the kernel's own name for the device. Anything else — an
// emulator, a network device, a serial that wandered in where a devpath was
// expected — is refused rather than coerced, because the next thing that
// happens to this value is a power cycle.
func usbPathOf(devpath string) (string, error) {
	p := strings.TrimSpace(devpath)
	p = strings.TrimPrefix(p, "usb:")
	if !usbPathRE.MatchString(p) {
		return "", fmt.Errorf("node: %w: %q is not a USB position; this agent addresses "+
			"hardware only by devpath, because duplicate OEM serials are real and a "+
			"serial names no port", ErrRefused, devpath)
	}
	return p, nil
}

// devpathOf is the inverse: the sysfs name as the rest of the farm spells it.
func devpathOf(usbPath string) string { return "usb:" + usbPath }

// usbNode is one device's addresses: where the kernel filed it, and which
// usbfs node speaks to it.
type usbNode struct {
	usbPath string
	bus     int
	dev     int
}

// path is the usbfs node. The kernel names these with three-digit zero
// padding, which is why this is not a plain Sprintf of two integers.
func (n usbNode) path() string {
	return fmt.Sprintf("%s/%03d/%03d", devBusUSB, n.bus, n.dev)
}

// readUSBNode maps a USB position to its usbfs node by READING busnum and
// devnum out of sysfs.
//
// It never derives them. A devnum is assigned by the kernel when the device
// enumerates and changes every time the device re-enumerates; the bus number
// depends on controller probe order. Guessing either would eventually send
// USBDEVFS_RESET to a different phone — one that is very likely holding a
// live lease and running somebody's job.
func readUSBNode(usbPath string) (usbNode, error) {
	dir := filepath.Join(sysfsUSBDevices, usbPath)
	if _, err := os.Stat(dir); err != nil {
		return usbNode{}, fmt.Errorf("node: no USB device at position %s: %w", usbPath, err)
	}
	bus, err := readIntFile(filepath.Join(dir, "busnum"))
	if err != nil {
		return usbNode{}, err
	}
	dev, err := readIntFile(filepath.Join(dir, "devnum"))
	if err != nil {
		return usbNode{}, err
	}
	// The kernel's own limits: bus numbers start at 1, and a USB address is
	// seven bits. A value outside those means we are reading something that is
	// not a USB device directory, and building a /dev path from it would be
	// addressing a node at random.
	if bus < 1 || bus > 999 || dev < 1 || dev > 127 {
		return usbNode{}, fmt.Errorf(
			"node: sysfs reports busnum=%d devnum=%d for %s, which is not a valid USB address",
			bus, dev, usbPath)
	}
	return usbNode{usbPath: usbPath, bus: bus, dev: dev}, nil
}

func readIntFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("node: read %s: %w", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("node: %s contains %q, which is not a number: %w",
			path, strings.TrimSpace(string(b)), err)
	}
	return n, nil
}

// present reports whether a device is currently enumerated at this position.
func present(usbPath string) bool {
	_, err := os.Stat(filepath.Join(sysfsUSBDevices, usbPath, "devnum"))
	return err == nil
}

// usbDescendants returns the position itself plus every device currently
// enumerated BEHIND it.
//
// A port's occupant is not always one phone. Plug a hub into port 3 of
// "3-1.4" and the kernel files that hub as "3-1.4.3" and the phones behind it
// as "3-1.4.3.1", "3-1.4.3.2" and so on. Cutting power to port 3 takes every
// one of them down, so every one of them has to be counted when the blast
// radius is worked out — counting only the hub would let a caller authorise a
// single position and darken six jobs. See checkBlastRadius.
func usbDescendants(usbPath string) ([]string, error) {
	entries, err := os.ReadDir(sysfsUSBDevices)
	if err != nil {
		return nil, fmt.Errorf("node: read %s: %w", sysfsUSBDevices, err)
	}
	out := []string{usbPath}
	prefix := usbPath + "."
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// "3-1.4.3.1:1.0" is an interface of a descendant, not a device.
		if strings.Contains(name, ":") || !present(name) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// waitPresent polls sysfs until the position is occupied, or the deadline
// passes. It returns the node it found, so the caller can log the devnum the
// device came back with — a changed devnum is proof of a real re-enumeration
// rather than a device that never actually left.
func waitPresent(ctx context.Context, usbPath string, within time.Duration) (usbNode, error) {
	deadline := time.Now().Add(within)
	for {
		if n, err := readUSBNode(usbPath); err == nil {
			return n, nil
		}
		if ctx.Err() != nil {
			return usbNode{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return usbNode{}, fmt.Errorf(
				"node: no device enumerated at %s within %s", usbPath, within)
		}
		select {
		case <-ctx.Done():
			return usbNode{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitAbsent is the mirror: it waits for a position to empty.
func waitAbsent(ctx context.Context, usbPath string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if !present(usbPath) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node: the device at %s was still enumerated %s later",
				usbPath, within)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// kernelRelease reads uname -r from proc. See procOSRelease for why not uname.
func (linuxPlatform) kernelRelease() (string, error) {
	b, err := os.ReadFile(procOSRelease)
	if err != nil {
		return "", fmt.Errorf("node: read %s: %w", procOSRelease, err)
	}
	rel := strings.TrimSpace(string(b))
	if rel == "" {
		return "", fmt.Errorf("node: %s is empty", procOSRelease)
	}
	return rel, nil
}

// kernelVersion parses the leading "major.minor" of a release string such as
// "6.8.0-45-generic" or "5.15.0". Distribution suffixes are ignored; only the
// two numbers the USB port-power behaviour actually turns on are read.
func kernelVersion(release string) (major, minor int, err error) {
	s := strings.TrimSpace(release)
	// Cut at the first character that can start a suffix, so "6.1-rc4" and
	// "6.1.0-generic" both reduce to their numeric head.
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("node: cannot read a major.minor version out of kernel release %q", release)
	}
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("node: kernel release %q has a non-numeric major version: %w", release, err)
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("node: kernel release %q has a non-numeric minor version: %w", release, err)
	}
	return major, minor, nil
}

// ---------------------------------------------------------------------------
// USBDEVFS_RESET — recovery tier 3
// ---------------------------------------------------------------------------

// usbdevfsReset returns the USBDEVFS_RESET ioctl request number for the
// architecture this binary is running on.
//
// The kernel defines it in include/uapi/linux/usbdevice_fs.h as
//
//	#define USBDEVFS_RESET   _IO('U', 20)
//
// and _IO(type, nr) expands to _IOC(_IOC_NONE, type, nr, 0), where _IOC packs
// four fields into one 32-bit word:
//
//	(dir << DIRSHIFT) | (size << SIZESHIFT) | (type << TYPESHIFT) | (nr << NRSHIFT)
//
// The generic layout in include/uapi/asm-generic/ioctl.h — used by x86, arm,
// arm64, riscv, s390 and loongarch — is NR:8 bits at 0, TYPE:8 at 8, SIZE:14
// at 16, DIR:2 at 30, with _IOC_NONE = 0. So
//
//	_IO('U', 20) = (0 << 30) | (0 << 16) | (0x55 << 8) | 20 = 0x5514
//
// PowerPC, MIPS, SPARC and PA-RISC kept the older BSD-derived layout, where
// SIZE is 13 bits, DIR is 3 bits at 29, and _IOC_NONE is 1 rather than 0. The
// same macro expands to (1 << 29) | 0x5514 = 0x20005514 there. A hard-coded
// 0x5514 would be an unrecognised request on those hosts and the reset would
// fail with EINVAL — a rung that is refused honestly is fine, but a rung that
// fails for a reason nobody can read is not. Hence the derivation rather than
// the constant.
func usbdevfsReset() uintptr {
	const (
		typ = 'U'
		nr  = 20
	)
	switch runtime.GOARCH {
	case "mips", "mipsle", "mips64", "mips64le",
		"ppc", "ppc64", "ppc64le", "sparc64":
		const dirNone = 1 // _IOC_NONE on the BSD-derived layout
		return uintptr(dirNone<<29 | typ<<8 | nr)
	default:
		const dirNone = 0 // _IOC_NONE on the asm-generic layout
		return uintptr(dirNone<<30 | typ<<8 | nr)
	}
}

// usbReset performs recovery tier 3: re-enumerate one device in place.
//
// This is the gentlest rung that touches the USB stack. It does not cut power,
// it does not disturb any neighbour on the hub, and it does not end anything:
// the lease, the fence and the job on the phone are all untouched, and the
// only thing that changes is that the host and the device renegotiate their
// link. That is why it sits below the power rung on the ladder.
func (linuxPlatform) usbReset(ctx context.Context, devpath string, o opsConfig) error {
	usbPath, err := usbPathOf(devpath)
	if err != nil {
		return err
	}
	node, err := readUSBNode(usbPath)
	if err != nil {
		return err
	}

	log := o.log.With("devpath", devpath, "usb_path", usbPath,
		"dev_node", node.path(), "busnum", node.bus, "devnum", node.dev)
	log.Info("issuing USBDEVFS_RESET")

	// The ioctl is uninterruptible: the kernel holds the device lock and
	// re-enumerates, which takes anywhere from tens of milliseconds to a few
	// seconds, and no context can stop it once it has started. Running it on
	// its own goroutine lets the caller's deadline expire without blocking,
	// and the goroutine still owns the file for the whole call, so nothing
	// closes the descriptor out from under an in-flight syscall.
	done := make(chan error, 1)
	go func() { done <- resetNode(node) }()

	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return fmt.Errorf("node: USBDEVFS_RESET on %s did not return before the deadline; "+
			"the ioctl cannot be interrupted and is still running in the kernel: %w",
			node.path(), ctx.Err())
	}

	// A reset that returns 0 means the kernel accepted it, not that the device
	// came back. Confirm the position is occupied again before reporting
	// anything: the actuator will then poll ADB for the transport, and the two
	// checks together are the difference between "the syscall worked" and "the
	// phone is back".
	wait := o.returnTimeout
	if wait > resetReturnWindow {
		wait = resetReturnWindow
	}
	back, err := waitPresent(ctx, usbPath, wait)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("node: USBDEVFS_RESET on %s was performed, but the wait for "+
				"the device to re-enumerate was cut short; the reset itself completed: %w",
				devpath, ctx.Err())
		}
		return fmt.Errorf("node: USBDEVFS_RESET on %s completed but the device did not "+
			"re-enumerate: %w", devpath, err)
	}
	log.Info("device re-enumerated after USBDEVFS_RESET",
		"devnum_before", node.dev, "devnum_after", back.dev)
	return nil
}

// resetNode opens the usbfs node and issues the ioctl.
//
// The node is opened write-only because usbfs requires write access for the
// commands that change device state; a read-only descriptor gets EACCES from
// the ioctl rather than from the open, which is a confusing place to discover
// a permissions problem.
func resetNode(node usbNode) error {
	f, err := os.OpenFile(node.path(), os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("node: cannot open %s for writing: %w — the agent needs "+
				"write access to usbfs, which is normally a udev rule putting the "+
				"device nodes in the farm's group", node.path(), err)
		}
		return fmt.Errorf("node: open %s: %w", node.path(), err)
	}
	defer f.Close()

	raw, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("node: raw descriptor for %s: %w", node.path(), err)
	}

	var errno syscall.Errno
	// Control keeps the descriptor valid for the duration of the call, which
	// is what makes issuing a raw syscall against a *os.File safe here.
	if cerr := raw.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, usbdevfsReset(), 0)
	}); cerr != nil {
		return fmt.Errorf("node: ioctl on %s: %w", node.path(), cerr)
	}
	if errno != 0 {
		switch errno {
		case syscall.ENODEV:
			return fmt.Errorf("node: USBDEVFS_RESET on %s: the device disconnected during "+
				"the reset instead of re-enumerating (%w)", node.path(), errno)
		case syscall.EACCES, syscall.EPERM:
			return fmt.Errorf("node: USBDEVFS_RESET on %s: %w — the descriptor is open but "+
				"the kernel refused the command, which usually means another process holds "+
				"the interface", node.path(), errno)
		default:
			return fmt.Errorf("node: USBDEVFS_RESET on %s: %w", node.path(), errno)
		}
	}
	return nil
}
