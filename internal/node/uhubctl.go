//go:build linux

// Per-port USB power control — recovery tier 4.
//
// Tier 4 is the last rung that can fix a device without a human walking to the
// rack, and it is the most dangerous one this agent can perform. Cutting VBUS
// is a physical act on shared hardware: on a hub without per-port switching it
// takes down every phone on that hub, several of which are probably running
// somebody's multi-hour job. Everything in this file exists to make sure that
// what actually happens is what the caller asked for.

package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// minKernelMajor / minKernelMinor is the floor below which a port power
	// cycle is a lie.
	//
	// Before Linux 6.0 the USB core re-powers a port that was switched off
	// behind its back: uhubctl sends the CLEAR_FEATURE(PORT_POWER) request,
	// the hub obeys, and the kernel turns the port straight back on. uhubctl
	// reports success, the port never really went dark, and the device never
	// saw a power cycle. Reporting that as a completed rung would teach the
	// recovery ladder that tier 4 was tried and did not help — so the ladder
	// escalates to quarantining a device whose only real problem is that the
	// one rung that would have fixed it was never actually performed.
	//
	// Refusing with the reason is the honest answer, and it names something an
	// operator can act on: upgrade the host kernel.
	minKernelMajor = 6
	minKernelMinor = 0

	// offVerifyWindow is how long the agent waits for a device to disappear
	// from sysfs after power is cut. A real VBUS cut disconnects the device in
	// milliseconds; a second is generous.
	offVerifyWindow = 2 * time.Second

	// powerOnAttempts is how hard the agent tries to give a port its power
	// back. A port left dark is a device removed from the farm until a human
	// notices, so this is the one step that retries.
	powerOnAttempts = 3

	// restoreTimeout bounds the detached power-on that runs when the normal
	// path did not finish. It is deliberately independent of the caller's
	// deadline: the caller giving up does not make a dark port acceptable. It
	// covers powerOnAttempts invocations of uhubctl and the second of spacing
	// between them, with room to spare.
	restoreTimeout = 15 * time.Second
)

// uhubctlCandidates are the usual install locations, searched when uhubctl is
// not on PATH. A host agent normally runs from a unit file with a minimal
// PATH, so "it works in my shell" is not evidence.
var uhubctlCandidates = []string{
	"/usr/sbin/uhubctl", "/usr/bin/uhubctl",
	"/usr/local/sbin/uhubctl", "/usr/local/bin/uhubctl",
	"/sbin/uhubctl", "/bin/uhubctl",
}

// hubStatusRE matches uhubctl's per-hub header, e.g.
//
//	Current status for hub 3-1 [05e3:0608 GenesysLogic USB2.1 Hub, USB 2.10, 4 ports, ppps]
//
// The bracketed tail is the hub descriptor uhubctl read off the hardware, and
// its last field is the hub's power switching mode: "ppps" for per-port
// switching, "ganged" for one switch shared by every port.
var hubStatusRE = regexp.MustCompile(`^(?:Current|New) status for hub (\S+)(?:\s+\[(.*)\])?`)

// hubStatus is one hub as uhubctl sees it, joined with what sysfs says is
// plugged into it.
type hubStatus struct {
	location string
	info     string
	// perPort is true only when the hub told uhubctl it has per-port power
	// switching. Anything else — "ganged", or a descriptor we could not read —
	// is treated as ganged, because assuming per-port switching on a hub that
	// does not have it is how one recovery action becomes seven broken jobs.
	perPort bool
	// children maps port number to the USB position enumerated on it.
	children map[int]string
}

// portPower performs recovery tier 4.
func (p linuxPlatform) portPower(ctx context.Context, devpath string, acknowledged []string, o opsConfig) error {
	usbPath, err := usbPathOf(devpath)
	if err != nil {
		return err
	}
	loc, port, err := hubLocation(usbPath)
	if err != nil {
		return err
	}
	log := o.log.With("devpath", devpath, "usb_path", usbPath, "hub", loc, "port", port)

	if err := p.checkKernelFloor(); err != nil {
		return err
	}

	bin, err := uhubctlPath(o.uhubctl)
	if err != nil {
		return err
	}

	hubs, err := readHubs(ctx, bin, loc)
	if err != nil {
		return err
	}
	for _, h := range hubs {
		// The descriptor is logged because "which hub is this" is the first
		// question an operator asks when a rack starts refusing tier 4, and
		// the model plus the switching mode answers it without a site visit.
		log.Info("hub reported by uhubctl", "hub", h.location,
			"per_port_power", h.perPort, "descriptor", h.info,
			"occupied_ports", len(h.children))
	}
	if err := checkBlastRadius(hubs, loc, port, usbPath, acknowledged); err != nil {
		return err
	}

	// Whether the device is enumerated right now decides what can be verified
	// afterwards. A device that already fell off the bus — the ordinary reason
	// to reach tier 4 — cannot be watched disappearing, so its absence during
	// the off step proves nothing and is not treated as evidence either way.
	wasPresent := present(usbPath)
	log.Info("cutting VBUS", "uhubctl", bin, "device_enumerated", wasPresent,
		"off_settle", o.offSettle)

	// The guard is armed BEFORE the port is switched off, not after. uhubctl
	// can fail partway — it switches the USB3 hub and then errors on the USB2
	// companion — and a non-zero exit is therefore not evidence that power is
	// still on. Arming first means the only way out of this function is
	// through a power-on attempt; when power was never cut, that attempt is a
	// harmless no-op.
	restored := false
	defer func() {
		if restored {
			return
		}
		// Restoring power runs on a context DETACHED from the caller's. A
		// deadline that expired, or an operator who pressed Ctrl-C, is not a
		// reason to leave a rack of phones dark: whatever ends this call, the
		// port gets its power back.
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
		defer cancel()
		if err := powerOn(restore, bin, loc, port); err != nil {
			log.Error("THE PORT IS PROBABLY LEFT WITHOUT POWER: neither the power-on step "+
				"nor the restore succeeded; a human must run "+
				"`uhubctl -l <hub> -p <port> -a on` on this host",
				"hub", loc, "port", port, "err", err)
		}
	}()

	if _, err := runUhubctl(ctx, bin, loc, port, "off"); err != nil {
		return err
	}

	if wasPresent {
		if err := waitAbsent(ctx, usbPath, offVerifyWindow); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("node: the VBUS cycle for %s was cancelled while the port "+
					"was dark; power is being restored regardless: %w", devpath, ctx.Err())
			}
			return fmt.Errorf("node: uhubctl reported the port off but the device at %s is "+
				"still enumerated %s later, so VBUS was not actually cut; this host's hub or "+
				"kernel is undoing the power-off and tier 4 cannot work here: %w",
				devpath, offVerifyWindow, err)
		}
		log.Info("device left the bus; port is dark")
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("node: the VBUS cycle for %s was cancelled while the port was "+
			"dark; power is being restored regardless: %w", devpath, ctx.Err())
	case <-time.After(o.offSettle):
	}

	if err := powerOn(ctx, bin, loc, port); err != nil {
		return err
	}
	restored = true

	back, err := waitPresent(ctx, usbPath, o.returnTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("node: power was restored to %s port %d, but the wait for the "+
				"device at %s to come back was cut short; the port has its power and the "+
				"device may still be enumerating: %w", loc, port, devpath, ctx.Err())
		}
		return fmt.Errorf("node: power was restored to %s port %d but no device enumerated "+
			"at %s: %w", loc, port, devpath, err)
	}
	log.Info("device re-enumerated after the VBUS cycle",
		"busnum", back.bus, "devnum", back.dev)
	return nil
}

// checkKernelFloor refuses the whole rung on a kernel that will silently undo
// it. See minKernelMajor.
func (p linuxPlatform) checkKernelFloor() error {
	rel, err := p.kernelRelease()
	if err != nil {
		return fmt.Errorf("node: %w: the kernel release could not be read, so this agent "+
			"cannot tell whether a port power-off would survive; a cycle that is silently "+
			"undone reports success and repairs nothing: %w", ErrNotSupported, err)
	}
	major, minor, err := kernelVersion(rel)
	if err != nil {
		return fmt.Errorf("node: %w: %w", ErrNotSupported, err)
	}
	if major < minKernelMajor || (major == minKernelMajor && minor < minKernelMinor) {
		return fmt.Errorf("node: %w: this host runs kernel %s, and before %d.%d the USB core "+
			"re-powers a port that was switched off, so the cycle would report success "+
			"without the device ever losing power; upgrade the host kernel to make "+
			"recovery tier 4 real here",
			ErrNotSupported, rel, minKernelMajor, minKernelMinor)
	}
	return nil
}

// uhubctlPath finds the binary, preferring an explicitly configured path.
func uhubctlPath(configured string) (string, error) {
	if configured != "" {
		info, err := os.Stat(configured)
		if err != nil {
			return "", fmt.Errorf("node: %w: the configured uhubctl at %s is not usable: %w",
				ErrNotSupported, configured, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("node: %w: the configured uhubctl at %s is not an "+
				"executable file", ErrNotSupported, configured)
		}
		return configured, nil
	}
	if p, err := exec.LookPath("uhubctl"); err == nil {
		return p, nil
	}
	for _, c := range uhubctlCandidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("node: %w: uhubctl is not installed on this host, and VBUS "+
		"control is the only way to perform recovery tier 4; install it or accept that "+
		"this host's ladder stops at tier 3", ErrNotSupported)
}

// hubLocation splits a USB position into the hub that feeds it and the port
// number on that hub, in uhubctl's own address form.
//
//	3-1.4.2  ->  hub "3-1.4", port 2
//	3-1      ->  hub "3",     port 1   (a root hub, which uhubctl names by bus)
func hubLocation(usbPath string) (string, int, error) {
	bus, chain, ok := strings.Cut(usbPath, "-")
	if !ok {
		return "", 0, fmt.Errorf("node: %w: %q has no bus separator and is not a USB position",
			ErrRefused, usbPath)
	}
	parts := strings.Split(chain, ".")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || port < 1 {
		return "", 0, fmt.Errorf("node: %w: %q does not end in a port number",
			ErrRefused, usbPath)
	}
	if len(parts) == 1 {
		return bus, port, nil
	}
	return bus + "-" + strings.Join(parts[:len(parts)-1], "."), port, nil
}

// readHubs asks uhubctl what it sees at a location and joins each hub it
// reports with the devices sysfs says are plugged into it.
//
// The location is passed WITHOUT --exact on purpose. A USB3 receptacle appears
// to the kernel as two hubs — a SuperSpeed one and a USB2 companion — and one
// physical port is one port on each. uhubctl's default duality handling
// switches both, which is what actually cuts power to the socket; --exact
// would switch one half and could leave a USB2 phone powered. The cost is that
// the true blast radius spans both hubs, which is exactly why this function
// returns every hub uhubctl reported rather than only the one asked for.
func readHubs(ctx context.Context, bin, loc string) ([]hubStatus, error) {
	out, err := runUhubctl(ctx, bin, loc, 0, "")
	if err != nil {
		return nil, err
	}

	var hubs []hubStatus
	for _, line := range strings.Split(out, "\n") {
		m := hubStatusRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		children, cerr := hubChildren(m[1])
		if cerr != nil {
			return nil, cerr
		}
		hubs = append(hubs, hubStatus{
			location: m[1],
			info:     m[2],
			// A descriptor we could not parse is treated as ganged: see
			// hubStatus.perPort.
			perPort:  strings.Contains(m[2], "ppps"),
			children: children,
		})
	}

	if len(hubs) == 0 {
		return nil, fmt.Errorf("node: %w: uhubctl reports no power-switchable hub at "+
			"location %s on this host, so there is no VBUS to cycle; the ladder stops at "+
			"tier 3 for devices behind this hub", ErrNotSupported, loc)
	}
	// Acting on a hub we did not ask for is exactly the failure mode this
	// whole file guards against, so the requested location must be among what
	// uhubctl actually found.
	for _, h := range hubs {
		if h.location == loc {
			return hubs, nil
		}
	}
	return nil, fmt.Errorf("node: %w: uhubctl answered for %s but not for the requested "+
		"hub %s; refusing to switch a hub that is not the one addressed",
		ErrRefused, hubs[0].location, loc)
}

// hubChildren lists what is currently plugged into a hub, by port.
//
// sysfs is the authority here rather than uhubctl's "connect" flags: it is the
// same namespace the rest of the farm addresses devices in, so a port's
// occupant can be compared directly against the devpaths the caller
// acknowledged.
func hubChildren(loc string) (map[int]string, error) {
	// A root hub is named by its bus, so its children are "3-1"; every other
	// hub's children extend its own path, "3-1.4" -> "3-1.4.2".
	sep := "."
	if !strings.Contains(loc, "-") {
		sep = "-"
	}
	prefix := loc + sep

	entries, err := os.ReadDir(sysfsUSBDevices)
	if err != nil {
		return nil, fmt.Errorf("node: read %s: %w", sysfsUSBDevices, err)
	}

	out := make(map[int]string)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		tail := name[len(prefix):]
		// A further dot means a grandchild (a device behind a nested hub), and
		// a colon means an interface, not a device. Neither is a direct
		// occupant of a port on this hub.
		if strings.ContainsAny(tail, ".:") {
			continue
		}
		port, err := strconv.Atoi(tail)
		if err != nil {
			continue
		}
		if !present(name) {
			continue
		}
		out[port] = name
	}
	return out, nil
}

// checkBlastRadius refuses a cycle that would disturb a device nobody named.
//
// The caller has checked lease policy for the target and for anything it
// listed in acknowledged. The agent is the last line: it is the only party
// that can see what is plugged into the hub at this instant, and on a ganged
// hub the difference between "cycle this port" and "cycle these seven ports"
// is invisible from the control plane. Being a blind executor here turns one
// operator's tier-4 decision into six destroyed jobs.
func checkBlastRadius(hubs []hubStatus, loc string, port int, target string, acknowledged []string) error {
	allowed := map[string]struct{}{target: {}}
	for _, a := range acknowledged {
		p, err := usbPathOf(a)
		if err != nil {
			return fmt.Errorf("node: %w: the acknowledged device list contains %q, which is "+
				"not a USB position, so this agent cannot tell what was authorised",
				ErrRefused, a)
		}
		allowed[p] = struct{}{}
	}

	disturbed := make(map[string]string) // usb path -> the hub that would switch it
	// note records a port occupant and everything enumerated behind it. An
	// occupant may itself be a hub, and acknowledging a hub is NOT
	// acknowledging the phones plugged into it: each of those positions is a
	// separate device that may be holding a separate lease, so each has to be
	// named in acknowledged on its own. See usbDescendants.
	note := func(child, hub string) error {
		behind, err := usbDescendants(child)
		if err != nil {
			return err
		}
		for _, p := range behind {
			if _, ok := allowed[p]; !ok {
				disturbed[p] = hub
			}
		}
		return nil
	}

	for _, h := range hubs {
		if h.perPort {
			// Only this port's occupant loses power. On a companion hub that
			// is the same physical receptacle, so anything found there is
			// still accounted for rather than assumed away.
			if child, ok := h.children[port]; ok {
				if err := note(child, h.location); err != nil {
					return err
				}
			}
			continue
		}
		// One switch for the whole hub: every occupant goes dark together.
		for _, child := range h.children {
			if err := note(child, h.location); err != nil {
				return err
			}
		}
	}
	if len(disturbed) == 0 {
		return nil
	}

	names := make([]string, 0, len(disturbed))
	for child, hub := range disturbed {
		names = append(names, fmt.Sprintf("%s (on hub %s)", devpathOf(child), hub))
	}
	// Sorted so the same refusal reads identically every time it is recorded
	// in farm.recovery_attempts.
	sort.Strings(names)

	// ErrGangedDomain rather than ErrRefused: this is the one refusal whose
	// remedy is hardware, and the control plane counts it on its own so that
	// a rising rate reads as "buy per-port hubs" instead of "leases said no".
	return fmt.Errorf("node: %w: cycling port %d of hub %s shares one power domain with %d "+
		"device(s) nobody authorised — %s. Every one of them may be holding a live lease. "+
		"Either check lease policy for them and pass them as acknowledged, or move the "+
		"devices to a hub with per-port power switching",
		ErrGangedDomain, port, loc, len(disturbed), strings.Join(names, ", "))
}

// powerOn restores power to a port, retrying, because leaving it dark removes
// the device from the farm until somebody walks to the rack.
//
// It is the only step in this file that retries. Every other failure here can
// be reported and left alone; this one leaves hardware in a state nobody asked
// for. The caller decides which context to spend: the normal path uses the
// caller's, and the deferred safety net in portPower uses a detached one.
func powerOn(ctx context.Context, bin, loc string, port int) error {
	var last error
	for attempt := 1; attempt <= powerOnAttempts; attempt++ {
		_, err := runUhubctl(ctx, bin, loc, port, "on")
		if err == nil {
			return nil
		}
		last = err
		if attempt < powerOnAttempts {
			select {
			case <-ctx.Done():
				// Out of budget: say so rather than burning the remaining
				// attempts on a context that will reject every one of them.
				return fmt.Errorf("node: power-on for hub %s port %d was cut short after "+
					"%d attempt(s); THE PORT MAY BE DARK: %w", loc, port, attempt, last)
			case <-time.After(time.Second):
			}
		}
	}
	return fmt.Errorf("node: could not restore power to hub %s port %d after %d attempts; "+
		"THE PORT IS PROBABLY DARK and the device behind it is unreachable until a human "+
		"runs `uhubctl -l %s -p %d -a on`: %w",
		loc, port, powerOnAttempts, loc, port, last)
}

// maxUhubctlOutput caps what one invocation may hand back. uhubctl prints a
// few lines per hub and exits; a binary at that path that streams instead is a
// broken install, and this process is the one holding the USB bus.
const maxUhubctlOutput = 1 << 20

// boundedBuffer collects a child process's output up to a limit and then keeps
// accepting and discarding.
//
// Write always reports the full length so the child never sees a short write
// or an EPIPE: killing uhubctl partway through switching a port is a worse
// outcome than losing the tail of its chatter.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// runUhubctl invokes the binary. An empty action asks for status only, in
// which case the port is not named.
func runUhubctl(ctx context.Context, bin, loc string, port int, action string) (string, error) {
	args := []string{"-l", loc}
	if action != "" {
		args = append(args, "-p", strconv.Itoa(port), "-a", action)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	stdout := &boundedBuffer{limit: maxUhubctlOutput}
	stderr := &boundedBuffer{limit: maxUhubctlOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		what := "status"
		if action != "" {
			what = fmt.Sprintf("-a %s on port %d", action, port)
		}
		return stdout.String(), fmt.Errorf("node: uhubctl %s for hub %s failed: %w (%s)",
			what, loc, err, detail)
	}
	return stdout.String(), nil
}
