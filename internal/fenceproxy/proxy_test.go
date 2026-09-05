package fenceproxy

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// t0 is a fixed instant. Nothing here sleeps to make time pass: the cache takes
// its clock as a parameter and Admit takes now as an argument, which is the
// whole reason the freshness rules are testable at all.
var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

const devA = "usb:3-1.4"
const devB = "usb:3-1.5"

func leaseIdentity() Identity {
	return Identity{Subject: "jobrunner-7", Class: ClassLease, NotAfter: t0.Add(24 * time.Hour)}
}

func maintenanceIdentity() Identity {
	return Identity{Subject: "recovery", Class: ClassMaintenance, NotAfter: t0.Add(24 * time.Hour)}
}

func leaseReq(service string, fence int64) Request {
	return Request{
		Identity: leaseIdentity(),
		Claim:    Claim{Class: ClassLease, Devpath: devA, Fence: fence, HasFence: true},
		Service:  service,
	}
}

func freshView(floor int64) View {
	return View{Floor: floor, Known: true, ObservedAt: t0.Add(-time.Second)}
}

func staleView(floor int64) View {
	return View{Floor: floor, Known: true, ObservedAt: t0.Add(-5 * time.Minute)}
}

// ---------------------------------------------------------------------------
// Service string parsing
// ---------------------------------------------------------------------------

func TestParseService(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw     string
		kind    ServiceKind
		devpath string
		verb    string
	}{
		{"host:version", KindHost, "", ""},
		{"host:devices-l", KindHost, "", ""},
		{"host:track-devices-l", KindHost, "", ""},
		{"host:kill", KindHost, "", ""},

		// A devpath contains a colon, so splitting on the last one would break
		// the moment a verb contained one too. The parser walks the devpath's
		// own character class instead.
		{"host-serial:usb:3-1.4:get-state", KindHostTarget, devA, "get-state"},
		{"host-serial:usb:3-1.4.2:reconnect-offline", KindHostTarget, "usb:3-1.4.2", "reconnect-offline"},
		{"host-usb:usb:3-1.4:features", KindHostTarget, devA, "features"},
		{"host-serial:usb:3-1.4:forward:tcp:1;tcp:2", KindHostTarget, devA, "forward:tcp:1;tcp:2"},

		{"host:transport:usb:3-1.4", KindTransport, devA, ""},
		// "give me any device" carries no devpath and is therefore refused by
		// the target check rather than by a special case in the parser.
		{"host:transport-any", KindTransport, "", ""},
		{"host:transport-usb", KindTransport, "", ""},

		{"shell,v2,raw:ls /sdcard", KindDevice, "", ""},
		{"sync:", KindDevice, "", ""},
		{"reboot:", KindDevice, "", ""},

		{"", KindInvalid, "", ""},
		{"host-serial:not-a-devpath:get-state", KindInvalid, "", ""},
		{"host-serial:usb:3-1.4", KindInvalid, "", ""},
		{"host-serial:usb:3-1.4:", KindInvalid, "", ""},
		{"host:transport:tcp:5555", KindInvalid, "", ""},
		{"host:transport:", KindInvalid, "", ""},
		{"shell:\x00evil", KindInvalid, "", ""},
	}

	for _, c := range cases {
		got := ParseService(c.raw)
		if got.Kind != c.kind || got.Devpath != c.devpath || got.Verb != c.verb {
			t.Errorf("ParseService(%q) = {%v %q %q}, want {%v %q %q}",
				c.raw, got.Kind, got.Devpath, got.Verb, c.kind, c.devpath, c.verb)
		}
	}
}

// ---------------------------------------------------------------------------
// The preamble
// ---------------------------------------------------------------------------

func TestParsePreamble(t *testing.T) {
	t.Parallel()

	got, err := ParsePreamble("fence:v1 class=lease devpath=usb:3-1.4 fence=41207")
	if err != nil {
		t.Fatalf("ParsePreamble: %v", err)
	}
	want := Claim{Class: ClassLease, Devpath: devA, Fence: 41207, HasFence: true}
	if got != want {
		t.Errorf("ParsePreamble = %+v, want %+v", got, want)
	}

	// fence=0 is a real value below every floor the sequence issues, and must
	// not be confused with "no fence sent".
	zero, err := ParsePreamble("fence:v1 devpath=usb:3-1.4 fence=0")
	if err != nil || !zero.HasFence || zero.Fence != 0 {
		t.Errorf("ParsePreamble(fence=0) = %+v, %v; want a present zero fence", zero, err)
	}

	bad := []string{
		"",
		"hello",
		"fence:v2 devpath=usb:3-1.4 fence=1",
		"fence:v1 devpath=usb:3-1.4 fence=abc",
		"fence:v1 devpath=../../etc fence=1",
		"fence:v1 devpath=usb:3-1.4 fence=1 bypass=yes",
		"fence:v1 devpathusb:3-1.4",
	}
	for _, b := range bad {
		if _, err := ParsePreamble(b); err == nil {
			t.Errorf("ParsePreamble(%q) succeeded; a preamble this proxy cannot fully "+
				"understand must be refused, not partially honoured", b)
		}
	}
}

// ---------------------------------------------------------------------------
// The admission matrix
// ---------------------------------------------------------------------------

// TestAdmitFenceMatrix walks section 5.2 of the design document row by row.
func TestAdmitFenceMatrix(t *testing.T) {
	t.Parallel()

	const svc = "host:transport:usb:3-1.4"
	pol := DefaultPolicy()

	cases := []struct {
		name    string
		fence   int64
		view    View
		outcome Outcome
	}{
		{"fresh view, fence above the floor", 100, freshView(90), OutcomeAdmit},
		{"fresh view, fence at the floor", 100, freshView(100), OutcomeAdmit},
		{"fresh view, fence below the floor", 90, freshView(100), OutcomeRefuseFenced},
		{"stale view, fence below the floor", 90, staleView(100), OutcomeRefuseFenced},
		{"stale view, fence at the floor", 100, staleView(100), OutcomeRefuseUnknown},
		{"never observed", 100, View{}, OutcomeRefuseUnknown},
		{"observation exactly at the budget", 100,
			View{Floor: 100, Known: true, ObservedAt: t0.Add(-DefaultMaxStaleness)}, OutcomeAdmit},
		{"observation one tick past the budget", 100,
			View{Floor: 100, Known: true, ObservedAt: t0.Add(-DefaultMaxStaleness - time.Millisecond)},
			OutcomeRefuseUnknown},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Admit(leaseReq(svc, c.fence), c.view, t0, pol)
			if got.Outcome != c.outcome {
				t.Fatalf("outcome = %s, want %s (reason: %s)", got.Outcome, c.outcome, got.Reason)
			}
			if got.Terminal != (c.outcome == OutcomeRefuseFenced) {
				t.Errorf("Terminal = %v for %s; only a fencing fact is terminal", got.Terminal, c.outcome)
			}
		})
	}
}

// TestBlindnessIsNeverReportedAsAFence is the assertion the whole failure policy
// rests on. A proxy that could not read Postgres must say so in a way no client
// can mistake for a fence, or a database blip aborts six-hour jobs.
func TestBlindnessIsNeverReportedAsAFence(t *testing.T) {
	t.Parallel()

	blind := []View{
		{},                            // never read anything
		staleView(100),                // read once, long ago, nothing disproving
		{Known: false, Floor: 999999}, // a floor we cannot vouch for is not knowledge
	}
	for i, v := range blind {
		d := Admit(leaseReq("host:transport:usb:3-1.4", 100), v, t0, DefaultPolicy())
		if d.Outcome != OutcomeRefuseUnknown {
			t.Fatalf("view %d: outcome = %s, want %s", i, d.Outcome, OutcomeRefuseUnknown)
		}
		if d.Terminal {
			t.Errorf("view %d: blindness was reported as terminal; that is STF #663 by another route", i)
		}
		if !d.Retryable {
			t.Errorf("view %d: blindness was not marked retryable, so a client cannot tell it apart "+
				"from a fence", i)
		}
		if !strings.Contains(d.Reason, "not a fence") && !strings.Contains(d.Reason, "retry") {
			t.Errorf("view %d: reason %q does not tell the client to retry", i, d.Reason)
		}
	}
}

// TestStaleKnowledgeStillProvesFencing is the other half: fence_floor is
// monotonically non-decreasing, so an old observation showing the claim below
// the floor can only have become more true. Enforcement does not evaporate when
// the database does.
func TestStaleKnowledgeStillProvesFencing(t *testing.T) {
	t.Parallel()

	for _, age := range []time.Duration{time.Second, time.Minute, 6 * time.Hour, 30 * 24 * time.Hour} {
		v := View{Floor: 500, Known: true, ObservedAt: t0.Add(-age)}
		d := Admit(leaseReq("host:transport:usb:3-1.4", 499), v, t0, DefaultPolicy())
		if d.Outcome != OutcomeRefuseFenced || !d.Terminal {
			t.Errorf("age %s: outcome = %s terminal=%v; a fence proved once stays proved",
				age, d.Outcome, d.Terminal)
		}
	}
}

func TestAdmitRefusesOffTargetAndUnbound(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()
	v := freshView(1)

	// Another device entirely.
	if d := Admit(leaseReq("host:transport:"+devB, 100), v, t0, pol); d.Outcome != OutcomeRefuseTarget {
		t.Errorf("addressing %s while claiming %s: outcome = %s, want %s", devB, devA, d.Outcome, OutcomeRefuseTarget)
	}
	if d := Admit(leaseReq("host-serial:"+devB+":reconnect", 100), v, t0, pol); d.Outcome != OutcomeRefuseTarget {
		t.Errorf("host-serial to another device: outcome = %s, want %s", d.Outcome, OutcomeRefuseTarget)
	}
	// "any device" is exactly what a fenced client must not be able to ask for.
	if d := Admit(leaseReq("host:transport-any", 100), v, t0, pol); d.Outcome != OutcomeRefuseTarget {
		t.Errorf("host:transport-any: outcome = %s, want %s", d.Outcome, OutcomeRefuseTarget)
	}
	// A device service before any transport switch.
	if d := Admit(leaseReq("shell,v2,raw:id", 100), v, t0, pol); d.Outcome != OutcomeRefuseTarget {
		t.Errorf("unbound device service: outcome = %s, want %s", d.Outcome, OutcomeRefuseTarget)
	}
	// The same service once the connection is bound to the claimed devpath.
	bound := leaseReq("shell,v2,raw:id", 100)
	bound.Bound = devA
	if d := Admit(bound, v, t0, pol); !d.Admitted() {
		t.Errorf("bound device service: outcome = %s (%s), want admit", d.Outcome, d.Reason)
	}
	// host:kill stops the ADB server for every device on the host.
	if d := Admit(leaseReq("host:kill", 100), v, t0, pol); d.Outcome != OutcomeRefuseService {
		t.Errorf("host:kill from a lease client: outcome = %s, want %s", d.Outcome, OutcomeRefuseService)
	}
}

func TestAdmitMaintenanceWhitelist(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()
	req := func(svc string) Request {
		return Request{Identity: maintenanceIdentity(), Service: svc}
	}

	// Everything the recovery ladder and the watchdog actually call.
	allowed := []string{
		"host:version",
		"host:track-devices-l",
		"host-serial:" + devA + ":reconnect",
		"host-serial:" + devA + ":detach",
		"host-serial:" + devA + ":attach",
		"host-serial:" + devA + ":get-state",
		"host:transport:" + devA,
	}
	for _, svc := range allowed {
		// A maintenance connection carries no fence, so an unknown view must
		// not stop it: the device it is repairing may hold no lease at all.
		if d := Admit(req(svc), View{}, t0, pol); !d.Admitted() {
			t.Errorf("maintenance %q: outcome = %s (%s), want admit", svc, d.Outcome, d.Reason)
		}
	}

	// reboot: is on the list; an arbitrary shell is not, and that is the whole
	// difference between a bounded bypass and a root shell on every phone.
	if d := Admit(req("reboot:"), View{}, t0, pol); !d.Admitted() {
		t.Errorf("maintenance reboot: outcome = %s, want admit", d.Outcome)
	}
	refused := []string{
		"shell,v2,raw:id",
		"shell:getprop ro.build.id",
		"shell:getprop ro.build.id; rm -rf /sdcard",
		"sync:",
		"host:kill",
		"host-serial:" + devA + ":forward:tcp:1;tcp:2",
	}
	for _, svc := range refused {
		if d := Admit(req(svc), View{}, t0, pol); d.Admitted() {
			t.Errorf("maintenance %q was admitted; the whitelist is exact-match precisely so "+
				"that a shell cannot ride in on a prefix", svc)
		}
	}
}

// TestWhitelistIsExactNotPrefix is the assertion behind section 7.2 of the
// design document, and it needs a policy that actually contains a shell to mean
// anything — the shipped default deliberately contains none, so a prefix bug
// would hide there.
//
// The enrol class is the only one that may open a shell at all, and its
// whitelist is the literal commands internal/enroll runs. A shell service string
// is an arbitrary command line, so any prefix rule over it is bypassable with
// ';', '&&' or a newline.
func TestWhitelistIsExactNotPrefix(t *testing.T) {
	t.Parallel()

	// The shape of internal/enroll's probe, which is a package-level value built
	// from constants and is therefore a fixed literal at runtime.
	const probe = "shell,v2,raw:getprop ro.build.id"
	// The shape of brandWriteCmd: templated on a uid, admitted by a pattern
	// whose only variable region is that uid.
	const uid = "8b1f0c2e-4d55-4a19-9f0a-0c1d2e3f4a5b"
	brand := regexp.MustCompile(`^shell,v2,raw:printf '%s' '[0-9a-f-]{36}' > /data/local/tmp/farm\.uid$`)
	brandCmd := "shell,v2,raw:printf '%s' '" + uid + "' > /data/local/tmp/farm.uid"

	// A pattern somebody wrote without anchors, which is the mistake this rule
	// is expected to survive: the whole-string span check in ServiceRules.allows
	// is what stops it becoming the prefix hole in another costume.
	loose := regexp.MustCompile(`shell,v2,raw:getprop ro\.serialno`)
	const looseCmd = "shell,v2,raw:getprop ro.serialno"

	pol := DefaultPolicy()
	pol.Rules[ClassEnroll] = ServiceRules{
		Transport:      true,
		Device:         []string{probe},
		DevicePatterns: []*regexp.Regexp{brand, loose},
	}
	id := Identity{Subject: "enroller", Class: ClassEnroll, NotAfter: t0.Add(time.Hour)}
	admit := func(svc string) Decision {
		return Admit(Request{Identity: id, Service: svc}, View{}, t0, pol)
	}

	for _, svc := range []string{probe, brandCmd, looseCmd} {
		if d := admit(svc); !d.Admitted() {
			t.Fatalf("the enroller's own command %q was refused: %s (%s)", svc, d.Outcome, d.Reason)
		}
	}

	// Every one of these shares a prefix with something on the list.
	for _, svc := range []string{
		probe + "; rm -rf /sdcard",
		probe + " && cat /data/data/com.bank/secrets",
		probe + "\nid",
		probe + "$(reboot)",
		brandCmd + "; su",
		looseCmd + "; su",
		"shell,v2,raw:id && " + looseCmd,
		"shell,v2,raw:getprop",              // a prefix of the probe, not the probe
		"shell,v2,raw:getprop ro.build.idX", // one character longer
	} {
		if d := admit(svc); d.Admitted() {
			t.Errorf("%q was admitted; the whitelist must match the whole service string, "+
				"or a shell rides in on a prefix", svc)
		}
	}
}

func TestAdmitRefusesLapsedCertificateBeforeConsultingTheFence(t *testing.T) {
	t.Parallel()

	req := leaseReq("host:transport:"+devA, 100)
	req.Identity.NotAfter = t0.Add(-time.Minute)

	// The view says this client is also fenced. It must still be told about the
	// certificate, because that is the thing it can act on.
	d := Admit(req, freshView(999), t0, DefaultPolicy())
	if d.Outcome != OutcomeRefuseCertLapsed {
		t.Fatalf("outcome = %s, want %s", d.Outcome, OutcomeRefuseCertLapsed)
	}
	if d.Terminal || !d.Retryable {
		t.Errorf("a lapsed certificate is a renewable condition, not a fence: terminal=%v retryable=%v",
			d.Terminal, d.Retryable)
	}
	if !strings.Contains(d.Reason, "lease is untouched") {
		t.Errorf("reason %q does not say the lease survives; a client that reads this as a fence "+
			"throws away its work", d.Reason)
	}

	// A certificate that lapses one nanosecond from now is still valid.
	req.Identity.NotAfter = t0.Add(time.Nanosecond)
	if d := Admit(req, freshView(1), t0, DefaultPolicy()); d.Admitted() != true {
		t.Errorf("a certificate valid for one more nanosecond was refused: %s", d.Outcome)
	}
}

func TestAdmitRefusesUnknownClass(t *testing.T) {
	t.Parallel()

	req := leaseReq("host:version", 1)
	req.Identity.Class = Class("superuser")
	if d := Admit(req, freshView(1), t0, DefaultPolicy()); d.Outcome != OutcomeRefuseIdentity {
		t.Errorf("outcome = %s, want %s; an unrecognised class must satisfy nothing", d.Outcome, OutcomeRefuseIdentity)
	}
	req.Identity.Class = ""
	if d := Admit(req, freshView(1), t0, DefaultPolicy()); d.Outcome != OutcomeRefuseIdentity {
		t.Errorf("empty class: outcome = %s, want %s", d.Outcome, OutcomeRefuseIdentity)
	}
}

// TestExactlyOneOutcomeIsTerminal reaches every outcome in Outcomes and checks
// the two flags against it. Ranging over the exported set means an outcome added
// later cannot quietly escape this.
func TestExactlyOneOutcomeIsTerminal(t *testing.T) {
	t.Parallel()

	pol := DefaultPolicy()
	badClass := leaseReq("host:version", 1)
	badClass.Identity.Class = Class("nope")
	lapsed := leaseReq("host:version", 1)
	lapsed.Identity.NotAfter = t0.Add(-time.Hour)
	noFence := leaseReq("host:transport:"+devA, 1)
	noFence.Claim.HasFence = false

	produced := map[Outcome]Decision{}
	for _, tc := range []struct {
		req  Request
		view View
	}{
		{leaseReq("host:version", 100), freshView(1)},
		{badClass, freshView(1)},
		{lapsed, freshView(1)},
		{leaseReq("host-serial:bogus:x", 1), freshView(1)},
		{leaseReq("host:kill", 1), freshView(1)},
		{leaseReq("host:transport:"+devB, 1), freshView(1)},
		{leaseReq("host:transport:"+devA, 1), freshView(9000)},
		{leaseReq("host:transport:"+devA, 1), View{}},
		{noFence, freshView(1)},
	} {
		d := Admit(tc.req, tc.view, t0, pol)
		produced[d.Outcome] = d
	}

	for _, o := range Outcomes {
		d, ok := produced[o]
		if !ok {
			t.Errorf("outcome %s was never produced by this test; it is either unreachable "+
				"or this test has stopped covering it", o)
			continue
		}
		wantTerminal := o == OutcomeRefuseFenced
		if d.Terminal != wantTerminal {
			t.Errorf("%s: Terminal = %v, want %v", o, d.Terminal, wantTerminal)
		}
		if d.Terminal && d.Retryable {
			t.Errorf("%s: terminal and retryable at once; a client cannot act on that", o)
		}
	}
}

// ---------------------------------------------------------------------------
// The cache
// ---------------------------------------------------------------------------

func TestCacheViewAndFreshness(t *testing.T) {
	t.Parallel()

	now := t0
	c := NewCache(func() time.Time { return now })

	if v := c.View(devA); v.Known {
		t.Fatal("an empty cache reported knowledge")
	}
	c.Apply(Snapshot{Floors: map[string]int64{devA: 10}})
	v := c.View(devA)
	if !v.Known || v.Floor != 10 || !v.ObservedAt.Equal(t0) {
		t.Fatalf("View = %+v, want floor 10 observed at t0", v)
	}

	now = t0.Add(7 * time.Second)
	if age := c.View(devA).Age(now); age != 7*time.Second {
		t.Errorf("Age = %s, want 7s", age)
	}

	// A position that drops out of the query's join keeps its floor. That is a
	// fact about a join, never about a fence.
	c.Apply(Snapshot{Floors: map[string]int64{devB: 3}})
	if v := c.View(devA); !v.Known || v.Floor != 10 {
		t.Errorf("View(%s) after a snapshot omitting it = %+v; knowledge must not be erased", devA, v)
	}

	// The floor never moves backwards, whatever a source hands over.
	c.Apply(Snapshot{Floors: map[string]int64{devA: 4}})
	if v := c.View(devA); v.Floor != 10 {
		t.Errorf("floor moved backwards to %d; fence_floor is monotone and so is this cache", v.Floor)
	}
}

func TestCacheWatchFiresOnlyOnAFencingFact(t *testing.T) {
	t.Parallel()

	c := NewCache(func() time.Time { return t0 })
	c.Apply(Snapshot{Floors: map[string]int64{devA: 10}})

	fired, cancel := c.Watch(devA, 10)
	defer cancel()

	// A snapshot at the same floor is not a fact about this claim.
	c.Apply(Snapshot{Floors: map[string]int64{devA: 10}})
	// Nor is another device moving.
	c.Apply(Snapshot{Floors: map[string]int64{devB: 99}})
	// Nor is the position disappearing.
	c.Apply(Snapshot{Floors: map[string]int64{}})
	select {
	case <-fired:
		t.Fatal("the watcher fired without a floor above the claimed fence")
	default:
	}

	c.Apply(Snapshot{Floors: map[string]int64{devA: 11}})
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("the watcher did not fire on a floor above the claimed fence")
	}
}

func TestCacheWatchFiresImmediatelyOnAnAlreadyStaleClaim(t *testing.T) {
	t.Parallel()

	c := NewCache(func() time.Time { return t0 })
	c.Apply(Snapshot{Floors: map[string]int64{devA: 50}})

	fired, cancel := c.Watch(devA, 49)
	defer cancel()
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("a claim already below the floor did not fire; a caller would have to race the first poll")
	}
}

// errSource fails every read after the first n successes.
type errSource struct {
	mu     sync.Mutex
	floors map[string]int64
	ok     int
	calls  int
}

func (s *errSource) Floors(context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls > s.ok {
		return Snapshot{}, errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	}
	out := map[string]int64{}
	for k, v := range s.floors {
		out[k] = v
	}
	return Snapshot{Floors: out}, nil
}

// TestPollErrorsChangeNothing is the structural form of "blindness is not a
// fencing fact". A source that has stopped answering must not move a floor, must
// not erase one, and above all must not fire a watcher — because firing one
// tears down somebody's live transfer.
func TestPollErrorsChangeNothing(t *testing.T) {
	t.Parallel()

	now := t0
	c := NewCache(func() time.Time { return now })
	src := &errSource{floors: map[string]int64{devA: 10}, ok: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired, stop := c.Watch(devA, 10)
	defer stop()

	done := make(chan struct{})
	go func() { defer close(done); c.Poll(ctx, src, time.Millisecond, discardLogger()) }()

	waitUntil(t, func() bool {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.calls > 20
	}, "the poller to fail repeatedly")

	cancel()
	<-done

	select {
	case <-fired:
		t.Fatal("a live connection was torn down because the proxy could not reach Postgres; " +
			"that is STF #663 arriving through the front door")
	default:
	}
	if v := c.View(devA); !v.Known || v.Floor != 10 {
		t.Fatalf("View = %+v after twenty failed polls; the last good knowledge must stand", v)
	}

	// The only consequence blindness is allowed to have: the view ages, and new
	// connections start being refused — with an outcome that is not a fence.
	now = t0.Add(DefaultMaxStaleness + time.Second)
	d := Admit(leaseReq("host:transport:"+devA, 10), c.View(devA), now, DefaultPolicy())
	if d.Outcome != OutcomeRefuseUnknown || d.Terminal {
		t.Fatalf("after ageing out: outcome = %s terminal = %v, want %s and not terminal",
			d.Outcome, d.Terminal, OutcomeRefuseUnknown)
	}
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// recordConn counts what actually reached the far end.
type recordConn struct {
	net.Conn
	mu     sync.Mutex
	n      int
	closed bool
}

func (c *recordConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.n += len(p)
	return len(p), nil
}

func (c *recordConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *recordConn) bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestGateStopsBeforeTheNextWrite asserts the package's central guarantee
// exactly rather than probabilistically: no write BEGINS after a fencing fact.
func TestGateStopsBeforeTheNextWrite(t *testing.T) {
	t.Parallel()

	rec := &recordConn{}
	g := &gate{w: rec}

	if _, err := g.Write(make([]byte, 1000)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	g.Shut()

	for i := 0; i < 5; i++ {
		if _, err := g.Write(make([]byte, 1000)); !errors.Is(err, ErrShut) {
			t.Fatalf("write %d after Shut returned %v, want ErrShut", i, err)
		}
	}
	if got := rec.bytes(); got != 1000 {
		t.Fatalf("%d bytes reached the device; only the 1000 written before the fact may have", got)
	}
	if !rec.closed {
		t.Error("Shut did not close the device-side socket")
	}

	g.Shut() // idempotent
}

// ---------------------------------------------------------------------------
// The invariant
// ---------------------------------------------------------------------------

// TestFenceSourceIsReadShaped guards the import graph from the other side.
//
// The proxy's only channel to the control plane has one method and it reads. A
// second method is precisely the change that would let a refusal DO something,
// so the count is asserted rather than left to review.
func TestFenceSourceIsReadShaped(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf((*FenceSource)(nil)).Elem()
	if n := typ.NumMethod(); n != 1 {
		t.Fatalf("FenceSource has %d methods, want exactly 1; a second method is how a "+
			"refusal acquires the ability to change allocation state", n)
	}
	m := typ.Method(0)
	if m.Name != "Floors" {
		t.Fatalf("FenceSource's method is %q, want Floors", m.Name)
	}
	// One return value plus an error, no side channel back to the caller.
	if got := m.Type.NumOut(); got != 2 {
		t.Fatalf("Floors returns %d values, want 2 (a snapshot and an error)", got)
	}
}

// TestPackageCannotEndALease enforces what the package doc declares.
//
// It walks the syntax tree rather than the file text, so these comments may name
// what is barred while the code may not. Identifiers reached through a selector
// are skipped: this package cannot rename another package's API, and the import
// ban below is what keeps the packages that could end a lease out of reach in
// the first place.
func TestPackageCannotEndALease(t *testing.T) {
	t.Parallel()

	forbiddenImports := []string{
		"device-farmer/internal/lease",
		"device-farmer/internal/reaper",
		"device-farmer/internal/scheduler",
		"database/sql",
		"jackc/pgx",
		"net/http",
	}
	// The verbs that end a lease in this system. "expire" is on the list, which
	// is why certificate expiry is called "lapsed" everywhere in this package.
	forbiddenNames := []string{"release", "reclaim", "revoke", "expire", "deallocate"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		text := string(src)
		for _, imp := range forbiddenImports {
			if strings.Contains(text, "\""+imp) || strings.Contains(text, imp+"\"") {
				t.Errorf("%s imports %q; a refusal must not be able to reach an allocation "+
					"decision, even transitively", name, imp)
			}
		}

		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++

		skip := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				skip[sel.Sel] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || skip[id] {
				return true
			}
			lower := strings.ToLower(id.Name)
			for _, bad := range forbiddenNames {
				if strings.Contains(lower, bad) {
					t.Errorf("%s:%d declares or uses identifier %q, which names an act that ends "+
						"a lease; this package refuses traffic and nothing else",
						name, fset.Position(id.Pos()).Line, id.Name)
				}
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("the scan read no production files; it is asserting nothing")
	}
}

// recordingSource hands out floors and remembers that it was only ever read.
type recordingSource struct {
	mu     sync.Mutex
	floors map[string]int64
	calls  int
}

func (s *recordingSource) Floors(context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	out := map[string]int64{}
	for k, v := range s.floors {
		out[k] = v
	}
	return Snapshot{Floors: out}, nil
}

func (s *recordingSource) snapshotOfState() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	for k, v := range s.floors {
		out[k] = v
	}
	return out
}

// TestRefusalChangesNoFenceState is the invariant, asserted behaviourally.
//
// Refusing traffic and ending a lease are different acts. A refusal here — and a
// mid-transfer teardown, which is the most tempting moment to "help" — must
// leave the control plane's state exactly as it found it. A proxy that reported
// its refusals back so somebody could clean up would fail this.
func TestRefusalChangesNoFenceState(t *testing.T) {
	t.Parallel()

	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	src := &recordingSource{floors: map[string]int64{devA: 100}}
	before := src.snapshotOfState()

	cache := NewCache(func() time.Time { return t0 })
	snap, err := src.Floors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cache.Apply(snap)

	proxy := startProxy(t, cache, DefaultPolicy(), leaseIdentity(), adb.Addr(), nil)

	// A fenced claim: fence 99 against a floor of 100.
	_, reason := dialAndAsk(t, proxy, preamble(devA, 99), "host:transport:"+devA)
	if !strings.Contains(reason, "below the floor") {
		t.Fatalf("refusal reason = %q, want it to name the floor", reason)
	}

	if got := src.snapshotOfState(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the fence source's state changed from %v to %v; a refusal must not write anything",
			before, got)
	}
	if src.calls != 1 {
		t.Fatalf("the source was called %d times; the refusal path consulted it beyond the one "+
			"read this test performed", src.calls)
	}
}

// ---------------------------------------------------------------------------
// End to end, against test/fakeadb
// ---------------------------------------------------------------------------

func TestAdmittedConnectionReachesTheDevice(t *testing.T) {
	t.Parallel()

	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	adb.Respond(devA, "shell", "uid=0(root)\n")

	cache := NewCache(func() time.Time { return t0 })
	cache.Apply(Snapshot{Floors: map[string]int64{devA: 100}})

	proxy := startProxy(t, cache, DefaultPolicy(), leaseIdentity(), adb.Addr(), nil)
	front := sidecar(t, proxy, preamble(devA, 100))

	// A real adbwire client, unmodified, driven through the proxy.
	cli := adbwire.New(front)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cli.Version(ctx); err != nil {
		t.Fatalf("host:version through the proxy: %v", err)
	}
	tr, err := cli.Transport(ctx, devA)
	if err != nil {
		t.Fatalf("transport through the proxy: %v", err)
	}
	st, err := tr.Service(ctx, adbwire.ShellService("id"))
	if err != nil {
		t.Fatalf("shell through the proxy: %v", err)
	}
	defer st.Close()
	if _, err := io.ReadAll(st); err != nil {
		t.Fatalf("reading the shell stream: %v", err)
	}

	if n := len(adb.RequestsTo(devA)); n == 0 {
		t.Fatal("the fake ADB server saw no request for the device; nothing was actually proxied")
	}
}

func TestRefusedConnectionGetsAReadableFail(t *testing.T) {
	t.Parallel()

	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	cache := NewCache(func() time.Time { return t0 })
	cache.Apply(Snapshot{Floors: map[string]int64{devA: 100}})
	proxy := startProxy(t, cache, DefaultPolicy(), leaseIdentity(), adb.Addr(), nil)

	status, reason := dialAndAsk(t, proxy, preamble(devA, 99), "host:transport:"+devA)
	if status != "FAIL" {
		t.Fatalf("status = %q, want FAIL; a refusal must be spoken in the client's own protocol, "+
			"not delivered as a connection reset", status)
	}
	if !strings.Contains(reason, "do not retry") {
		t.Errorf("reason = %q, want it to say the claim is dead", reason)
	}

	// And nothing reached the device.
	if n := len(adb.RequestsTo(devA)); n != 0 {
		t.Fatalf("the fake ADB server saw %d requests for a device the client was fenced out of", n)
	}
}

// TestTransportSwitchDoesNotBypassTheWhitelist is the attack the framing-mode
// loop exists to stop: get admitted on host:transport, then send anything.
func TestTransportSwitchDoesNotBypassTheWhitelist(t *testing.T) {
	t.Parallel()

	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	cache := NewCache(func() time.Time { return t0 })
	proxy := startProxy(t, cache, DefaultPolicy(), maintenanceIdentity(), adb.Addr(), nil)

	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := writeFrame(c, "fence:v1 class=maintenance"); err != nil {
		t.Fatal(err)
	}
	// Frame 1: allowed for maintenance.
	if err := writeFrame(c, "host:transport:"+devA); err != nil {
		t.Fatal(err)
	}
	if st := readStatus(t, c); st != "OKAY" {
		t.Fatalf("transport switch status = %q, want OKAY", st)
	}
	// Frame 2: a shell, which maintenance may not open.
	if err := writeFrame(c, "shell,v2,raw:rm -rf /sdcard"); err != nil {
		t.Fatal(err)
	}
	st := readStatus(t, c)
	if st != "FAIL" {
		t.Fatalf("shell after a transport switch: status = %q, want FAIL; the whitelist must be "+
			"checked on every host frame, not only the first", st)
	}
}

// TestFenceGoesStaleMidTransfer is the case the design document's section 6 is
// about: a push is in flight when the floor moves.
func TestFenceGoesStaleMidTransfer(t *testing.T) {
	t.Parallel()

	const remote = "/data/local/tmp/big.bin"
	sync := fakeadb.StartSync(t, fakeadb.Device{Serial: "SER1", Devpath: devA})

	cache := NewCache(func() time.Time { return t0 })
	cache.Apply(Snapshot{Floors: map[string]int64{devA: 100}})

	// Throttle the device side so the fact lands mid-transfer deterministically
	// rather than by racing loopback.
	slow := func(ctx context.Context) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", sync.Addr())
		if err != nil {
			return nil, err
		}
		return &slowConn{Conn: c, per: 2 * time.Millisecond}, nil
	}
	proxy := startProxy(t, cache, DefaultPolicy(), leaseIdentity(), "", slow)
	front := sidecar(t, proxy, preamble(devA, 100))

	cli := adbwire.New(front)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		errc <- cli.Push(ctx, devA, pr, remote, 0o644)
		_ = pr.Close()
	}()
	go func() {
		defer pw.Close()
		buf := make([]byte, 64<<10)
		for i := 0; i < 256; i++ { // at most 16 MiB
			if _, err := pw.Write(buf); err != nil {
				return
			}
		}
	}()

	waitUntil(t, func() bool { return sync.Stats().BytesIn >= 256<<10 }, "the push to start crossing")
	crossed := sync.Stats().BytesIn

	// THE FACT: the floor moves above the fence this connection presented.
	cache.Apply(Snapshot{Floors: map[string]int64{devA: 101}})

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the push succeeded after the fence went stale")
		}
		t.Logf("push failed as intended: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the push never returned after the fence went stale; the teardown did not happen")
	}

	// Nothing landed: DONE never arrived, so the daemon never completed the file.
	if _, ok := sync.File(devA, remote); ok {
		t.Error("a completed file exists on the device after a torn SEND")
	}
	// And the transfer stopped rather than draining. The bound is generous
	// because the teardown is asynchronous; what matters is that it stopped
	// well short of the 16 MiB the source was willing to supply.
	if got := sync.Stats().BytesIn; got >= 16<<20 {
		t.Errorf("%d bytes reached the device after the fence went stale (transfer had reached %d "+
			"when the fact landed); forwarding did not stop", got, crossed)
	}
}

// TestBlindnessDoesNotTearDownALiveTransfer is the mirror image, and it is the
// assertion that stops this proxy from being STF #663 with a certificate.
func TestBlindnessDoesNotTearDownALiveTransfer(t *testing.T) {
	t.Parallel()

	const remote = "/data/local/tmp/quiet.bin"
	sync := fakeadb.StartSync(t, fakeadb.Device{Serial: "SER1", Devpath: devA})

	now := t0
	cache := NewCache(func() time.Time { return now })
	cache.Apply(Snapshot{Floors: map[string]int64{devA: 100}})

	proxy := startProxy(t, cache, DefaultPolicy(), leaseIdentity(), sync.Addr(), nil)
	front := sidecar(t, proxy, preamble(devA, 100))

	cli := adbwire.New(front)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := cli.Sync(ctx, devA)
	if err != nil {
		t.Fatalf("opening a sync session through the proxy: %v", err)
	}
	defer conn.Close()

	// Blindness, for far longer than any staleness budget. The poller would be
	// failing; the cache is simply never updated again, which is exactly what a
	// failed poll leaves behind.
	now = t0.Add(6 * time.Hour)

	// The live session must still work. Nothing in this package may sever it.
	if err := conn.Push(ctx, strings.NewReader("still ours"), remote, 0o644); err != nil {
		t.Fatalf("a live transfer failed after six hours of blindness: %v\n"+
			"Severing live work because the proxy lost its database is the exact defect "+
			"DeviceFarmer/STF #663 reports.", err)
	}
	if _, ok := sync.File(devA, remote); !ok {
		t.Error("the file did not land, so the transfer did not really complete")
	}

	// A NEW connection, however, is refused — and told to retry, not to abort.
	status, reason := dialAndAsk(t, proxy, preamble(devA, 100), "host:transport:"+devA)
	if status != "FAIL" {
		t.Fatalf("a new connection was admitted on a six-hour-old view: status %q", status)
	}
	if !strings.Contains(reason, "not a fence") {
		t.Errorf("reason = %q; a client must be able to tell this apart from a fence", reason)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func preamble(devpath string, fence int64) string {
	return fmt.Sprintf("%s class=lease devpath=%s fence=%d", PreambleV1, devpath, fence)
}

// discardLogger keeps the WARN lines these tests deliberately provoke out of the
// test output. The lines themselves are asserted where they matter; here they
// would only bury the failures.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// startProxy runs a Server on a loopback port and returns its address.
//
// Identity is injected rather than read from a certificate: TLS termination is
// orthogonal to everything these tests assert, and Server.Identify is the seam
// that makes that separation possible.
func startProxy(t *testing.T, cache *Cache, pol Policy, id Identity, upstreamAddr string,
	dial func(context.Context) (net.Conn, error)) string {
	t.Helper()

	if dial == nil {
		addr := upstreamAddr
		dial = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &Server{
		Cache: cache, Policy: pol, DialUpstream: dial,
		Identify: func(net.Conn) (Identity, error) { return id, nil },
		Log:      discardLogger(),
		Now:      func() time.Time { return t0 },
	}
	if cache != nil && cache.now != nil {
		srv.Now = cache.now
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String()
}

// sidecar is a plain-TCP front end that speaks the preamble on a client's
// behalf and then gets out of the way.
//
// It exists because adbwire.WithDialer takes a concrete *net.Dialer, so an
// unmodified adbwire.Client cannot be handed a connection that has already done
// a handshake and sent a preamble. This is a working model of the dial seam
// adbwire needs; see section 11 of the design document.
func sidecar(t *testing.T, proxyAddr, pre string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sidecar listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", proxyAddr)
				if err != nil {
					return
				}
				defer up.Close()
				if err := writeFrame(up, pre); err != nil {
					return
				}
				go func() {
					_, _ = io.Copy(up, c)
					if tc, ok := up.(*net.TCPConn); ok {
						_ = tc.CloseWrite()
					}
				}()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

// dialAndAsk sends a preamble and one service request, and returns the four
// status bytes and the FAIL reason if there is one.
func dialAndAsk(t *testing.T, proxyAddr, pre, service string) (status, reason string) {
	t.Helper()

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if err := writeFrame(c, pre); err != nil {
		t.Fatalf("writing the preamble: %v", err)
	}
	if err := writeFrame(c, service); err != nil {
		t.Fatalf("writing the service: %v", err)
	}
	status = readStatus(t, c)
	if status == "FAIL" {
		r, err := readFrame(c)
		if err != nil {
			t.Fatalf("reading the FAIL reason: %v", err)
		}
		reason = r
	}
	return status, reason
}

func readStatus(t *testing.T, r io.Reader) string {
	t.Helper()
	var st [4]byte
	if _, err := io.ReadFull(r, st[:]); err != nil {
		t.Fatalf("reading the status word: %v", err)
	}
	return string(st[:])
}

// slowConn paces writes so a test can land an event mid-transfer without racing
// loopback.
type slowConn struct {
	net.Conn
	per time.Duration
}

func (c *slowConn) Write(p []byte) (int, error) {
	time.Sleep(c.per)
	return c.Conn.Write(p)
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
