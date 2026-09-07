package fenceproxy

// ClassControl is the only class bounded by BOTH a service whitelist and a
// fence, and these tests exist because that combination did not used to be
// expressible.
//
// Admit's two branches were alternatives: the whitelist branch returned before
// the fence was ever consulted, so the only class that carried a fence was also
// the only class that consulted no whitelist. A live screen wanted the opposite
// of both — the narrowest service list on the listener, and the fence teardown
// that stops bytes the instant a lease ends — so the axes were separated.
//
// What these tests defend is that separating them did not open a hole. The two
// that matter most are the fail-closed case (a class with neither bound must be
// refused, not admitted) and the pattern cases: this is the first entry in any
// shipped whitelist that is a REGEXP over a shell command line, and
// ServiceRules' own doc explains at length why a prefix rule over one is
// bypassable.

import (
	"strings"
	"testing"
	"time"
)

func controlIdentity() Identity {
	return Identity{Subject: "api-1", Class: ClassControl, NotAfter: t0.Add(24 * time.Hour)}
}

// A spawn command of the exact shape internal/scrcpy builds.
const controlSpawn = "shell,v2,raw:CLASSPATH=/data/local/tmp/scrcpy-server-0f1e2d3c4b5a.jar " +
	"app_process / com.genymobile.scrcpy.Server 4.1 " +
	"video_codec=h264 max_size=1024 audio=false control=true raw_stream=true scid=1a2b3c4d"

func controlReq(service string, fence int64) Request {
	return Request{
		Identity: controlIdentity(),
		Claim:    Claim{Class: ClassControl, Devpath: devA, Fence: fence, HasFence: true},
		Service:  service,
		Bound:    devA,
	}
}

// fresh is a view that knows the floor and was read just now, so neither
// staleness nor an unknown floor can be what a case below is measuring.
func fresh(floor int64) View {
	return View{Known: true, Floor: floor, ObservedAt: t0}
}

func TestControlWhitelist(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()

	allowed := []string{
		"host:version",
		"host:features",
		"host-serial:" + devA + ":get-state",
		"host:transport:" + devA,
		"sync:",
		controlSpawn,
		"localabstract:scrcpy_1a2b3c4d",
	}
	for _, svc := range allowed {
		if d := Admit(controlReq(svc, 41207), fresh(41207), t0, pol); !d.Admitted() {
			t.Errorf("control %q: outcome = %s (%s), want admit", svc, d.Outcome, d.Reason)
		}
	}

	// Everything a screen does not need. reboot: is on the list deliberately:
	// maintenance may reboot a phone and control may not, because the human
	// driving a screen is not repairing hardware.
	refused := []string{
		"shell,v2,raw:id",
		"shell:getprop ro.build.id",
		"reboot:",
		"host:kill",
		"host:devices-l",
		"host-serial:" + devA + ":get-serialno",
		"host-serial:" + devA + ":reconnect",
		// The verb check that lease class does NOT make. This is the whole
		// reason ClassControl exists rather than reusing lease: a lease-class
		// connection is refused nothing here, because refuseOffTarget compares
		// only the devpath for a host-target service.
		"host-serial:" + devA + ":forward:tcp:9000;tcp:9000",
	}
	for _, svc := range refused {
		if d := Admit(controlReq(svc, 41207), fresh(41207), t0, pol); d.Admitted() {
			t.Errorf("control %q was admitted; a live screen needs three services and this is not one", svc)
		}
	}
}

// TestControlSpawnPatternCannotBeExtended is the assertion the pattern exists
// to earn. A whole-string regexp over a shell command line is only safe if its
// alphabet excludes every character that chains, substitutes or redirects — so
// each case below takes the command that IS admitted and appends one.
func TestControlSpawnPatternCannotBeExtended(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()

	if d := Admit(controlReq(controlSpawn, 41207), fresh(41207), t0, pol); !d.Admitted() {
		t.Fatalf("the unmodified spawn was refused (%s: %s); every case below would then pass "+
			"for the wrong reason", d.Outcome, d.Reason)
	}

	for _, tail := range []string{
		"; rm -rf /sdcard",
		" && id",
		" || id",
		" | sh",
		"\nid",
		"$(id)",
		"`id`",
		" > /sdcard/out",
		" &",
		"; ",
		" #",
	} {
		if d := Admit(controlReq(controlSpawn+tail, 41207), fresh(41207), t0, pol); d.Admitted() {
			t.Errorf("the spawn command extended with %q was admitted; the pattern has acquired a "+
				"character class that lets a second command ride along", tail)
		}
	}

	// And the shape itself may not be loosened: a jar that is not content-named,
	// a class that is not scrcpy's, an abstract socket that is not one of its.
	for _, svc := range []string{
		"shell,v2,raw:CLASSPATH=/data/local/tmp/evil.jar app_process / com.genymobile.scrcpy.Server 4.1",
		"shell,v2,raw:CLASSPATH=/data/local/tmp/scrcpy-server-0f1e2d3c4b5a.jar app_process / com.example.Other 4.1",
		"localabstract:anything",
		"localabstract:scrcpy_",
		"localabstract:scrcpy_zzzzzzzz",
	} {
		if d := Admit(controlReq(svc, 41207), fresh(41207), t0, pol); d.Admitted() {
			t.Errorf("control %q was admitted; the pattern is meant to name scrcpy and nothing else", svc)
		}
	}
}

// TestControlIsFencedToo is the half a whitelist cannot provide. The service
// being on the list says what may be opened; the fence says whether this
// connection may still touch this device at all.
func TestControlIsFencedToo(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()

	// Below the floor: dead, terminal, do not retry.
	d := Admit(controlReq("sync:", 41206), fresh(41207), t0, pol)
	if d.Outcome != OutcomeRefuseFenced {
		t.Errorf("a stale fence on a whitelisted service: outcome = %s (%s), want %s — "+
			"the whitelist must not be able to admit past the fence",
			d.Outcome, d.Reason, OutcomeRefuseFenced)
	}
	if !d.Terminal {
		t.Error("a fenced control connection is not Terminal; being fenced is a one-way door")
	}

	// A control connection that presents no fence is malformed, not unfenced.
	// The class carries one by definition, so omitting it is not a way to opt out.
	noFence := Request{Identity: controlIdentity(), Service: "sync:", Bound: devA}
	if d := Admit(noFence, fresh(41207), t0, pol); d.Outcome != OutcomeRefuseMalformed {
		t.Errorf("control with no fence: outcome = %s (%s), want %s",
			d.Outcome, d.Reason, OutcomeRefuseMalformed)
	}

	// And it is bound to its own device, like every fence-carrying class.
	offTarget := controlReq("host:transport:"+devB, 41207)
	if d := Admit(offTarget, fresh(41207), t0, pol); d.Admitted() {
		t.Error("control reached a devpath it holds no fence for")
	}
}

// TestAClassBoundedByNothingIsRefused pins the fail-closed direction of
// separating the two axes.
//
// Before the split, a class absent from Policy.Rules was refused by the
// whitelist branch. After it, "absent from Rules" means "no whitelist bound",
// and a class that also carries no fence would be bounded by nothing at all —
// which is a root shell on every phone in the rack, reached by adding a class
// to Class.Valid and forgetting DefaultPolicy.
func TestAClassBoundedByNothingIsRefused(t *testing.T) {
	t.Parallel()

	// ClassEnroll with its rules removed stands in for that mistake: a class
	// this proxy has heard of, carrying no fence, with nobody having published
	// its whitelist.
	pol := DefaultPolicy()
	delete(pol.Rules, ClassEnroll)

	req := Request{
		Identity: Identity{Subject: "enroller", Class: ClassEnroll, NotAfter: t0.Add(time.Hour)},
		Service:  "host:version",
	}
	d := Admit(req, fresh(1), t0, pol)
	if d.Admitted() {
		t.Fatal("a class bounded by neither a whitelist nor a fence was admitted")
	}
	if !strings.Contains(d.Reason, "Policy.Rules") {
		t.Errorf("reason = %q; it should name the omission (Policy.Rules) rather than the "+
			"connection, because the connection is not what is wrong", d.Reason)
	}
}

// TestEveryValidClassIsBounded is the guard that makes the test above
// unnecessary to remember. It walks the classes rather than a list written
// here, so a fifth class cannot be added without answering the question.
func TestEveryValidClassIsBounded(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()
	for _, c := range []Class{ClassLease, ClassMaintenance, ClassEnroll, ClassControl} {
		if !c.Valid() {
			t.Errorf("%s is in this list and not in Class.Valid", c)
			continue
		}
		_, hasRules := pol.Rules[c]
		if !hasRules && !c.CarriesFence() {
			t.Errorf("class %q is bounded by neither Policy.Rules nor a fence. Admit refuses it, "+
				"so this is not a hole — but it is a class that can open nothing, which means "+
				"either DefaultPolicy or CarriesFence was not finished.", c)
		}
	}

	// ClassLease deliberately has no whitelist: the job runner executes
	// arbitrary step kinds, and enumerating their service strings is not
	// possible. If it ever acquires one, every kind must be enumerated first.
	if _, ok := pol.Rules[ClassLease]; ok {
		t.Error("ClassLease acquired a service whitelist; the job runner runs arbitrary steps, " +
			"so every step kind's service string has to be enumerated before this is safe")
	}
}
