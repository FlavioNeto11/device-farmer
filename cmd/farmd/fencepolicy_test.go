package main

// What this binary dials, and whether the fence proxy it ships admits it.
//
// This file exists because of a defect that was invisible from inside every
// package involved. internal/fenceproxy's DefaultPolicy held the maintenance
// class to an exact-match whitelist and said, in a comment, that
// "internal/watchdog uses only host:track-devices-l". internal/watchdog's
// battery probe had been opening "shell,v2,raw:dumpsys battery" since it was
// written. Neither package could see the other — the proxy may not import a
// package that imports a database driver, and the watchdog has no idea a proxy
// exists — so on every farm with the proxy switched on the battery-health loop
// was refused at the ADB socket on every device, and the only symptom was a
// column that stopped being written. POST /api/v1/devices/{id}/exec was dead for
// the same reason, and POST /api/v1/slots/{id}/rebrand with it.
//
// cmd/farmd is the one place that imports every side of this, which makes it the
// only place the question is answerable. So the question is asked here, and it is
// asked about the policy this binary actually serves — fencePolicy applied to
// fenceproxy.DefaultPolicy(), exactly as internal/node builds it.
//
// # The shape, and why it is this shape
//
// The table below is a CLASSIFICATION, not a list of things that must be
// admitted. Two service strings in this tree are refused on purpose and must
// stay refused, and a test that only asserted "everything is admitted" would
// have to leave them out — at which point the table is incomplete again and the
// next one to be forgotten is a fourth caller nobody wrote down. So every entry
// carries a verdict and, when the verdict is a refusal, the reason it is
// deliberate.
//
// Every string is DERIVED from the package that puts it on the wire
// (watchdog.FenceDeviceServices, enroll.FenceDevicePatternExamples,
// recovery.DeviceRebootService, adbwire.ControlReconnect, scrcpy.Spawn.Service).
// A string retyped here would be a third copy, and a copy is what this whole
// mechanism exists to stop.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/enroll"
	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
	"github.com/flaviopadilha/device-farmer/internal/recovery"
	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
	"github.com/flaviopadilha/device-farmer/internal/watchdog"
)

// The one position every case addresses, and a fence at the floor, so that
// nothing below is measuring a devpath mismatch or a stale view by accident.
const (
	fenceTestDevpath = "usb:3-1.4"
	fenceTestFence   = int64(41207)
)

// dialed is one service string this binary can put on the wire, and the verdict
// the shipped policy must return for it.
type dialed struct {
	service string
	// by names the production call site, so a failure sends the reader to the
	// code rather than to this table.
	by string
	// admit is the verdict required. False means the refusal is DELIBERATE and
	// why must say why.
	admit bool
	why   string
}

// classDials is one credential class's whole story.
type classDials struct {
	// fenceOnly marks a class that consults no service whitelist at all: its
	// bound is the fence and the teardown that comes with it. Only ClassLease is
	// one, because the job runner executes arbitrary step kinds and enumerating
	// their service strings is not possible.
	fenceOnly bool
	why       string
	dials     []dialed
}

// hostTargetServices builds the position-addressed form of a verb under both
// prefixes adbwire can be configured with. Which one a deployment uses is
// adbwire.WithTargetPrefix, so a policy that admitted only one of them would
// refuse half the farms.
func hostTargetServices(verb string) []string {
	var out []string
	for _, p := range []adbwire.TargetPrefix{adbwire.PrefixUSB, adbwire.PrefixTargetName} {
		out = append(out, string(p)+":"+fenceTestDevpath+":"+verb)
	}
	return out
}

// controlSpawnServices is what internal/scrcpy builds for a live screen, asked of
// the builder rather than spelled out.
func controlSpawnServices(t *testing.T) []string {
	t.Helper()
	s := scrcpy.Spawn{
		JarID:   "0f1e2d3c4b5a",
		Version: "3.1",
		SCID:    0x1a2b3c4d,
		Args: []scrcpy.Arg{
			{Key: "video_codec", Value: "h264"},
			{Key: "max_size", Value: "1024"},
		},
	}
	spawn, err := s.Service()
	if err != nil {
		t.Fatalf("building the scrcpy spawn command: %v", err)
	}
	socket, err := s.Socket()
	if err != nil {
		t.Fatalf("building the scrcpy socket service: %v", err)
	}
	return []string{spawn, socket}
}

// dialsByClass is the classification. Read it as the answer to "what does this
// binary open, with which credential, and is it allowed to".
func dialsByClass(t *testing.T) map[fenceproxy.Class]classDials {
	t.Helper()

	maintenance := classDials{
		why: "the class every role in this binary announces when FARM_FENCE_CLIENT_CERT is " +
			"set: the watchdog, the recovery ladder and the API server's ADB client share one " +
			"certificate",
	}
	add := func(by string, admit bool, why string, services ...string) {
		for _, s := range services {
			maintenance.dials = append(maintenance.dials,
				dialed{service: s, by: by, admit: admit, why: why})
		}
	}

	// internal/watchdog: the reconciler holds one long-lived stream per host, and
	// the battery poller opens a transport plus a shell per device per minute.
	// The shell is the one that was refused before this file existed.
	add("internal/watchdog reconciler (TrackDevices)", true, "", "host:track-devices-l")
	add("internal/watchdog battery probe", true, "",
		append([]string{"host:transport:" + fenceTestDevpath}, watchdog.FenceDeviceServices()...)...)

	// internal/recovery: tiers 1 and 2 are position-addressed verbs, tier 5 is a
	// device service, and the confirming read after each is a state query.
	for _, cmd := range []adbwire.ControlCmd{
		adbwire.ControlReconnect, adbwire.ControlReconnectOffline,
		adbwire.ControlDetach, adbwire.ControlAttach,
	} {
		add("internal/recovery tier 1/2", true, "", hostTargetServices(string(cmd))...)
	}
	add("internal/recovery confirming read", true, "", hostTargetServices("get-state")...)
	add("internal/recovery tier 5", true, "", recovery.DeviceRebootService)

	// internal/recovery tier 7. Refused on purpose, and the only reason this
	// table has a verdict column.
	add("internal/recovery tier 7 (restartServer)", false,
		"host:kill stops the ADB server for every device on the host, including the ones under "+
			"other people's leases, so it is on no class's list in DefaultPolicy. Tier 7 is "+
			"refused behind the proxy; the ladder records the refusal as a rung disposition and "+
			"climbs on. Admitting it would make one stolen maintenance credential a "+
			"farm-wide outage.",
		"host:kill")

	// internal/enroll's commands, reached with THIS certificate by
	// POST /api/v1/slots/{id}/rebrand, which builds its Brander over the API
	// server's ADB client.
	add("internal/api rebrand -> internal/enroll", true, "",
		append(enroll.FenceDeviceServices(), enroll.FenceDevicePatternExamples()...)...)
	add("nobody — a near miss of internal/enroll's brand write", false,
		"a guard on one farm uid followed by an install of a different one. It is built from the "+
			"same template and no code path builds it: it says 'check that the phone holds A, "+
			"then write B', which walks through the device-side guard that stops one phone being "+
			"rebranded over another's identity and fuses two device rows' failure scores, "+
			"quarantines and lease records. RE2 has no backreferences, so the regexp alone does "+
			"not refuse it; the pattern names its repeated regions and ServiceRules requires them "+
			"to agree.",
		enroll.FenceDeviceNearMisses()...)

	// POST /api/v1/devices/{id}/exec. Refused on purpose, and refused up front by
	// internal/api so the operator reads a policy statement instead of a 502.
	for _, cmd := range []string{"ls /sdcard", "logcat -d", "id"} {
		add("internal/api POST /devices/{id}/exec", false,
			"an operator's command is arbitrary. No exact list enumerates it, and the only "+
				"pattern that would admit what operators actually run is one bounded by a safe "+
				"ALPHABET — which still admits `rm -rf /sdcard` on every handset in the rack and "+
				"so hands a stolen maintenance credential the root shell this class exists to "+
				"prevent. internal/api refuses the route on a fenced farm; see "+
				"refuseExecBehindTheFence.",
			adbwire.ShellService(cmd))
	}

	enrol := classDials{
		why: "the class whose reason for existing is that a brand-new handset must be asked " +
			"what it is before it can be adopted. Nothing in this tree issues an enroll-class " +
			"certificate yet, so this list bounds the class ahead of the credential",
	}
	// What internal/enroll dials: one listing per cycle, then a transport and a
	// shell per device it has not identified.
	for _, s := range append([]string{
		"host:devices-l",
		"host:transport:" + fenceTestDevpath,
	}, append(enroll.FenceDeviceServices(), enroll.FenceDevicePatternExamples()...)...) {
		enrol.dials = append(enrol.dials,
			dialed{service: s, by: "internal/enroll listing, probe and brand", admit: true})
	}
	for _, s := range enroll.FenceDeviceNearMisses() {
		enrol.dials = append(enrol.dials, dialed{
			service: s, by: "nobody — a near miss of internal/enroll's brand write", admit: false,
			why: "the same template with the guard uid and the installed uid pulled apart. " +
				"See the maintenance entry: this class holds the same patterns and must refuse " +
				"the same thing.",
		})
	}

	control := classDials{
		why: "the live screen. Nothing in this binary dials it yet — there is no role holding " +
			"FARM_FENCE_CONTROL_CERT — but internal/scrcpy builds the commands and its own " +
			"admission_test.go pushes every one of them through Admit",
	}
	for _, s := range append([]string{
		"host:version", "host:features",
		"host:transport:" + fenceTestDevpath,
		"sync:",
	}, controlSpawnServices(t)...) {
		control.dials = append(control.dials,
			dialed{service: s, by: "internal/scrcpy", admit: true})
	}
	control.dials = append(control.dials,
		dialed{
			service: "host:kill", by: "nobody", admit: false,
			why: "listed so the narrowest class on the listener is asserted to be narrow: a " +
				"live screen is a human on a network holding somebody's phone",
		},
		dialed{
			service: recovery.DeviceRebootService, by: "nobody", admit: false,
			why: "maintenance may reboot a phone and control may not; the human driving a " +
				"screen is not repairing hardware",
		})

	return map[fenceproxy.Class]classDials{
		fenceproxy.ClassMaintenance: maintenance,
		fenceproxy.ClassEnroll:      enrol,
		fenceproxy.ClassControl:     control,
		fenceproxy.ClassLease: {
			fenceOnly: true,
			why: "the job runner executes arbitrary step kinds, so its service strings cannot " +
				"be enumerated. It is bounded by the fence and by the teardown that comes with " +
				"one, which is a stronger bound than a whitelist and not a weaker one",
			dials: []dialed{
				{service: "host:version", by: "internal/jobrunner", admit: true},
				{service: "host:transport:" + fenceTestDevpath, by: "internal/jobrunner", admit: true},
				{service: adbwire.ShellService("am instrument -w com.example.test/androidx.test.runner.AndroidJUnitRunner"),
					by: "internal/jobrunner step", admit: true},
				{service: "sync:", by: "internal/jobrunner artifact pull", admit: true},
				{
					service: "host:kill", by: "nobody", admit: false,
					why: "Policy.LeaseHost is two entries, and host:kill is not one of them: a " +
						"holder of one device may not stop the server serving everybody else's",
				},
			},
		},
	}
}

// shippedPolicy is the policy internal/node serves: the shipped default with
// this binary's own literals published into it.
func shippedPolicy(t *testing.T) fenceproxy.Policy {
	t.Helper()
	pol, err := fencePolicy(fenceproxy.DefaultPolicy())
	if err != nil {
		t.Fatalf("fencePolicy refused to build: %v", err)
	}
	return pol
}

// fenceRequest builds the admission input for one class and one service, fenced
// and on-target where the class requires it, so that no case is measuring
// anything but the whitelist and the verdict under test.
func fenceRequest(class fenceproxy.Class, service string) fenceproxy.Request {
	req := fenceproxy.Request{
		Identity: fenceproxy.Identity{
			Subject:  "farmd-test",
			Class:    class,
			NotAfter: fenceTestNow().Add(24 * time.Hour),
		},
		Service: service,
		Bound:   fenceTestDevpath,
	}
	if class.CarriesFence() {
		req.Claim = fenceproxy.Claim{
			Class:    class,
			Devpath:  fenceTestDevpath,
			Fence:    fenceTestFence,
			HasFence: true,
		}
	}
	return req
}

func fenceTestNow() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }

// fenceTestView knows the floor and was read just now, so neither staleness nor
// an unknown position can be what a case is measuring.
func fenceTestView() fenceproxy.View {
	return fenceproxy.View{Known: true, Floor: fenceTestFence, ObservedAt: fenceTestNow()}
}

// TestEveryServiceThisBinaryDialsIsClassified is the guard the whole file is for.
//
// It walks fenceproxy.Classes() rather than a list written here — that is the
// point, because the list written by hand is what drifted — and requires every
// class to have an entry above. A fifth class, or a fifth caller, cannot be added
// without somebody answering what it dials and whether the proxy admits it.
func TestEveryServiceThisBinaryDialsIsClassified(t *testing.T) {
	t.Parallel()

	pol := shippedPolicy(t)
	table := dialsByClass(t)

	for _, class := range fenceproxy.Classes() {
		entry, ok := table[class]
		if !ok {
			t.Errorf("credential class %q has no entry in dialsByClass. Every class this proxy "+
				"knows is reachable by somebody's certificate, and a class nobody has written "+
				"down what it dials is a class whose whitelist is maintained by accident.", class)
			continue
		}
		if len(entry.dials) == 0 {
			t.Errorf("class %q has an entry with no service strings in it; it is asserting nothing", class)
		}
		if entry.why == "" {
			t.Errorf("class %q has no note saying what it is for", class)
		}

		for _, d := range entry.dials {
			if !d.admit && d.why == "" {
				t.Errorf("class %q: %q is marked refused with no reason. A deliberate refusal "+
					"must say why, or the next reader will read it as the bug it looks like.",
					class, d.service)
			}
			dec := fenceproxy.Admit(fenceRequest(class, d.service), fenceTestView(), fenceTestNow(), pol)
			switch {
			case d.admit && !dec.Admitted():
				t.Errorf("class %q: %q is REFUSED (%s: %s) and %s puts it on the wire. "+
					"Whatever that caller does is dead on every farm with the fence proxy on, and "+
					"it will fail looking like broken hardware.",
					class, d.service, dec.Outcome, dec.Reason, d.by)
			case !d.admit && dec.Admitted():
				t.Errorf("class %q: %q is now ADMITTED and it is meant to be refused. %s",
					class, d.service, d.why)
			}
		}
	}

	// The lease class's bound is the fence, and it must not quietly acquire a
	// whitelist: the job runner's step kinds are not enumerable, so a whitelist
	// would refuse whichever kinds nobody listed.
	if _, ok := pol.Rules[fenceproxy.ClassLease]; ok {
		t.Error("ClassLease acquired a service whitelist in the policy this binary serves; " +
			"every step kind the job runner can execute would have to be enumerated first")
	}
	for class, entry := range table {
		_, hasRules := pol.Rules[class]
		if entry.fenceOnly == hasRules {
			t.Errorf("class %q is marked fenceOnly=%t and the shipped policy %s a whitelist for "+
				"it; the table and the policy disagree about how this class is bounded",
				class, entry.fenceOnly, map[bool]string{true: "has", false: "has no"}[hasRules])
		}
	}
}

// TestNoAdmittedDeviceServiceCanCarryASecondCommand is the assertion every
// pattern on a whitelist has to earn, applied to the policy this binary serves
// rather than to one pattern in isolation.
//
// fenceproxy.ServiceRules says why: a shell service string is an arbitrary
// command line, so `;`, `&&`, a newline and `$( )` all extend a command that
// started out looking fine. Each case takes a device service the policy ADMITS
// and appends one of them.
//
// The lease class is excluded, and that is not a gap. It consults no whitelist at
// all — a lease-class connection may open any device service on the devpath it
// holds a fence for — so there is nothing here to bypass; the bound is the fence
// and the teardown that arrives with a fencing fact.
func TestNoAdmittedDeviceServiceCanCarryASecondCommand(t *testing.T) {
	t.Parallel()

	pol := shippedPolicy(t)
	tails := []string{
		"; rm -rf /sdcard",
		" && id",
		" || id",
		" | sh",
		"\nid",
		"$(id)",
		"`id`",
		" > /sdcard/out",
		" &",
		" #",
	}

	checked := 0
	for class, entry := range dialsByClass(t) {
		if entry.fenceOnly {
			continue
		}
		for _, d := range entry.dials {
			if !d.admit || fenceproxy.ParseService(d.service).Kind != fenceproxy.KindDevice {
				continue
			}
			if !strings.HasPrefix(d.service, "shell") {
				// "sync:" and "reboot:" are bare protocol words with no command
				// line in them; there is nothing to append to.
				continue
			}
			checked++
			for _, tail := range tails {
				dec := fenceproxy.Admit(fenceRequest(class, d.service+tail),
					fenceTestView(), fenceTestNow(), pol)
				if dec.Admitted() {
					t.Errorf("class %q admits %q extended with %q. Whatever rule admits that "+
						"command has acquired a region that accepts a second one, which is the "+
						"prefix hole this whitelist is exact-match to avoid. Source: %s.",
						class, d.service, tail, d.by)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no admitted shell service was found to extend; this test is asserting nothing, " +
			"which means the table stopped describing the shells this binary opens")
	}
}

// TestThePublishedLiteralsAreTheCommandsTheOwningPackagesBuild closes the last
// way the old defect could come back: a literal published to the policy that is
// no longer the string its package sends.
//
// Both halves are read from the owning package here, so this cannot be satisfied
// by a copy. What it actually proves is that the published value is a DEVICE
// service carrying the package's own command text — a publication of
// "host:version" or of an empty string would satisfy a test that only checked
// admission.
func TestThePublishedLiteralsAreTheCommandsTheOwningPackagesBuild(t *testing.T) {
	t.Parallel()

	if got, want := watchdog.FenceDeviceServices(),
		[]string{adbwire.ShellService(watchdog.BatteryCommand)}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("watchdog.FenceDeviceServices() = %q, want %q — the published service string is "+
			"no longer the shell the battery probe runs", got, want)
	}

	for _, s := range append(enroll.FenceDeviceServices(), enroll.FenceDevicePatternExamples()...) {
		if k := fenceproxy.ParseService(s).Kind; k != fenceproxy.KindDevice {
			t.Errorf("enroll publishes %q, which parses as a %s service; only a device service "+
				"can be admitted through ServiceRules.Device or DevicePatterns", s, k)
		}
		if !strings.HasPrefix(s, adbwire.ShellService("")) {
			t.Errorf("enroll publishes %q, which is not a shell-v2 service string; the Brander "+
				"and the Probe both go through adbwire.Shell", s)
		}
	}

	// One example per pattern, in the same order, or the guard test above would
	// be pushing a string through a pattern that was never meant to match it.
	if got, want := len(enroll.FenceDevicePatternExamples()), len(enroll.FenceDevicePatterns()); got != want {
		t.Fatalf("enroll publishes %d patterns and %d examples; they are documented to be in the "+
			"same order, so an unequal count means one shape is unproved", want, got)
	}
	for i, re := range enroll.FenceDevicePatterns() {
		example := enroll.FenceDevicePatternExamples()[i]
		if loc := re.FindStringIndex(example); loc == nil || loc[0] != 0 || loc[1] != len(example) {
			t.Errorf("enroll pattern %d does not match its own example end to end.\npattern: %s\nexample: %s",
				i, re, example)
		}
		if strings.Contains(re.String(), ".*") || strings.Contains(re.String(), ".+") {
			t.Errorf("enroll pattern %d contains an unbounded wildcard: %s. A pattern over a shell "+
				"command line may only leave regions whose alphabet cannot hold a metacharacter.",
				i, re)
		}
	}
	// And the uid region is hex digits. Belt and braces: the extension test above
	// proves nothing can be appended, this proves nothing can be substituted.
	uidRegion := regexp.MustCompile(`\[0-9a-f\]\{32\}`)
	for i, re := range enroll.FenceDevicePatterns() {
		if !uidRegion.MatchString(re.String()) {
			t.Errorf("enroll pattern %d has no [0-9a-f]{32} region: %s. Either the uid shape "+
				"changed or the pattern grew a different variable region.", i, re)
		}
	}
}
