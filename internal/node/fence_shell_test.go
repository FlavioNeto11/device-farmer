package node

// The real path for a connection that carries no fence and needs a shell.
//
// fence_test.go proves the fence reaches the device. This file proves the other
// half of admission — the service whitelist — over the same real wire: a real
// fenceproxy.Server on a real mTLS listener, a real fakeadb behind it, and a real
// adbwire.Client dialling it with the maintenance certificate and the maintenance
// preamble, which is exactly how internal/watchdog, internal/recovery and
// internal/api's ADB client are configured by cmd/farmd.
//
// It exists because a table-level assertion would have missed the defect that
// prompted it. internal/fenceproxy's DefaultPolicy admitted no shell to the
// maintenance class while internal/watchdog's battery probe opened one on every
// attached device, so on every farm with the proxy on the probe was refused at
// the socket and the battery columns stopped being written. Every test in
// internal/fenceproxy passed throughout: none of them knew what the watchdog
// dials, and the ones in internal/watchdog dialled a fake with no proxy in front
// of it. The two halves only meet on a wire, so this is on a wire.
//
// The cases are the answers a whitelist can give, and each is asserted against
// the DEVICE rather than against the decision:
//
//   - a shell the owning package published arrives at the ADB server and its
//     output comes back;
//   - an operator's own shell does not arrive at all, and the caller is told why
//     in the proxy's own words;
//   - a command built from a published TEMPLATE arrives, and the near miss of it
//     — the same template with its two uid regions pulled apart — does not.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/enroll"
	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
	"github.com/flaviopadilha/device-farmer/internal/watchdog"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// publishedShells is the wiring cmd/farmd installs, narrowed to the packages
// this file drives. Every string comes from the package that dials it, never
// from one typed here: a copy in this file would pass while the real caller was
// refused, which is the exact failure being tested for.
func publishedShells(pol fenceproxy.Policy) (fenceproxy.Policy, error) {
	return pol.AllowingDeviceServices(fenceproxy.ClassMaintenance,
		append(watchdog.FenceDeviceServices(), enroll.FenceDeviceServices()...),
		enroll.FenceDevicePatterns())
}

// shellV2Reply frames stdout and an exit status the way a device's shell v2
// stream does, so adbwire parses a real answer rather than loose bytes.
func shellV2Reply(t *testing.T, stdout string, exit byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout)); err != nil {
		t.Fatalf("framing stdout: %v", err)
	}
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{exit}); err != nil {
		t.Fatalf("framing the exit packet: %v", err)
	}
	return b.String()
}

// maintenanceClient is the ADB client cmd/farmd hands the watchdog, the recovery
// ladder and the API server: this host's advertised endpoint, the fence client
// certificate, and an announcement of the maintenance class.
func maintenanceClient(t *testing.T, pki *testPKI, addr string) *adbwire.Client {
	t.Helper()
	return adbwire.New(addr,
		adbwire.WithTLS(pki.client(pki.maintenance)),
		adbwire.WithAdmissionPreamble(adbwire.AdmissionClass(adbwire.AdmissionClassMaintenance)),
		adbwire.WithCallTimeout(10*time.Second),
		adbwire.WithMaxOutput(32<<10))
}

// TestAPublishedShellReachesTheDeviceThroughTheProxy is the assertion the defect
// would have failed: the watchdog's battery probe, as the watchdog builds it,
// through a real proxy, to a real ADB server, with its output coming back.
func TestAPublishedShellReachesTheDeviceThroughTheProxy(t *testing.T) {
	pki := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))

	const dump = "level: 77\nscale: 100\ntemperature: 281\n"
	probe := adbwire.ShellService(watchdog.BatteryCommand)
	adb.Respond(devA, probe, shellV2Reply(t, dump, 0))

	a, _ := startFence(t, pki, adb.Addr(), staticFloors{devA: 100}, func(c *Config) {
		c.Fence.Policy = publishedShells
	})
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := maintenanceClient(t, pki, a.FenceAddr()).Shell(ctx, devA, watchdog.BatteryCommand)
	if err != nil {
		t.Fatalf("the watchdog's battery probe did not complete through the proxy: %v.\n"+
			"This is the defect: the probe is refused at the ADB socket, the watchdog records "+
			"the device as having said nothing, and farm.device_runtime.battery_pct silently "+
			"stops being written on every fenced farm.", err)
	}
	if res == nil || !strings.Contains(string(res.Stdout), "level: 77") {
		t.Fatalf("the probe was admitted but its output did not come back: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}

	// And it reached the ADB server, not something the proxy answered itself.
	var sawProbe bool
	for _, r := range adb.RequestsTo(devA) {
		if r.Service == probe {
			sawProbe = true
		}
	}
	if !sawProbe {
		t.Errorf("the adb server never saw %q; the output must have come from somewhere other "+
			"than the device", probe)
	}
}

// TestAnOperatorsOwnShellIsRefusedBeforeItReachesTheDevice is the other answer.
//
// POST /api/v1/devices/{id}/exec builds its service string from whatever an
// operator typed, which no exact list enumerates and no pattern bounds without
// handing a stolen maintenance credential a shell on every handset. So it is
// refused, and the two things that matter about the refusal are asserted here:
// the command never reaches the ADB server, and the caller is told WHY in the
// proxy's own words — a protocol refusal carrying text, not a severed socket.
// internal/api turns that into a 501 with a message about the policy before it
// dials at all; see refuseExecBehindTheFence. This is what it is protecting the
// operator from reading as a broken phone.
func TestAnOperatorsOwnShellIsRefusedBeforeItReachesTheDevice(t *testing.T) {
	pki := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))

	const operatorCommand = "ls /sdcard"
	exec := adbwire.ShellService(operatorCommand)
	// Scripted so that a pass cannot come from the fake having nothing to say.
	adb.Respond(devA, exec, shellV2Reply(t, "ringtones\n", 0))

	a, _ := startFence(t, pki, adb.Addr(), staticFloors{devA: 100}, func(c *Config) {
		c.Fence.Policy = publishedShells
	})
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := maintenanceClient(t, pki, a.FenceAddr()).Shell(ctx, devA, operatorCommand)
	if err == nil {
		t.Fatalf("an arbitrary operator command was admitted through the proxy and answered %+v. "+
			"The maintenance certificate is shared by the watchdog and the recovery ladder, so "+
			"this is a shell on every handset in the rack for whoever holds it.", res)
	}

	// A protocol refusal, not a transport failure. The difference is the whole
	// reason internal/api can refuse this route with a policy message: the proxy
	// speaks the client's own protocol and says what was wrong.
	if adbwire.IsTransport(err) {
		t.Errorf("the refusal arrived as a transport failure (%v); a severed socket is "+
			"indistinguishable from a wedged handset and sends an operator to the wrong rack", err)
	}
	if !strings.Contains(err.Error(), "whitelist") {
		t.Errorf("the refusal does not name the whitelist, so nobody reading it at 3am learns "+
			"that this is policy rather than hardware: %v", err)
	}

	for _, r := range adb.RequestsTo(devA) {
		if r.Service == exec {
			t.Fatalf("%q reached the adb server; the whitelist is checked per host-protocol "+
				"frame precisely so a transport switch cannot carry a device service past it", exec)
		}
	}
}

// TestATemplatedBrandWriteIsAdmittedAndItsNearMissIsNot puts the one pattern in
// this tree whose regions are correlated through the whole real path.
//
// The brand write is what POST /api/v1/slots/{id}/rebrand sends, with the API
// server's maintenance certificate, so this is that route's wire. The two cases
// differ by one uid:
//
//   - the command internal/enroll builds — guard on a uid, install the same uid
//     — reaches the device;
//   - the same template with the guard uid and the installed uid pulled apart
//     does not. Nothing in the regexp refuses it: RE2 has no backreferences, so
//     substitution alone leaves two independent regions. What refuses it is the
//     correlation the pattern declares by naming them, and if that ever stops
//     working the symptom is not a crash — it is a credential that can rebrand a
//     phone whose uid it merely read, over the device-side guard that exists to
//     stop two devices' histories being fused into one row.
func TestATemplatedBrandWriteIsAdmittedAndItsNearMissIsNot(t *testing.T) {
	pki := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))

	write := enroll.FenceDevicePatternExamples()[0]
	nearMiss := enroll.FenceDeviceNearMisses()[0]
	// Both scripted, so a refusal cannot come from the fake having nothing to say.
	adb.Respond(devA, write, shellV2Reply(t, "", 0))
	adb.Respond(devA, nearMiss, shellV2Reply(t, "", 0))

	a, _ := startFence(t, pki, adb.Addr(), staticFloors{devA: 100}, func(c *Config) {
		c.Fence.Policy = publishedShells
	})
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cli := maintenanceClient(t, pki, a.FenceAddr())

	st, err := cli.OpenService(ctx, devA, write)
	if err != nil {
		t.Fatalf("the brand write internal/enroll builds was refused through the proxy: %v.\n"+
			"POST /api/v1/slots/{id}/rebrand is dead on every fenced farm while this is true.", err)
	}
	_ = st.Close()

	if st, err := cli.OpenService(ctx, devA, nearMiss); err == nil {
		_ = st.Close()
		t.Fatal("a guard on one farm uid followed by an install of a different one reached the " +
			"device. No code path builds that command; admitting it lets whoever holds this " +
			"certificate rebrand a phone over the guard that keeps two devices' histories apart.")
	}

	for _, r := range adb.RequestsTo(devA) {
		if r.Service == nearMiss {
			t.Fatalf("the near miss reached the adb server: %q", nearMiss)
		}
	}
}
