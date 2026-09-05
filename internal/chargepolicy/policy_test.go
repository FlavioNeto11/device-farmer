package chargepolicy

// The decision table, as pure functions. decide takes one unit's state and
// returns a plan; nothing here touches a database or an agent, so every row
// of the policy can be read off this file. The SQL-backed and agent-backed
// halves are in integration_test.go.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/node"
)

const (
	testActor = "chargepolicy"
	testHost  = "rack1-host-a"
)

var band = thresholds{actor: testActor, min: 40, max: 80}

// dev is an idle, enabled, agent-reachable device on a per-port domain with a
// battery inside the band. Every row below starts from it and changes one
// thing.
func dev(n string, tweak ...func(*device)) device {
	d := device{
		ID: "dev-" + n, HostID: testHost, Devpath: "usb:3-1." + n, SlotID: 1,
		DomainID: 1, DomainKind: "per_port", DomainControl: "uhubctl", HasAgent: true,
		Battery: 60, AdminState: "enabled",
	}
	for _, f := range tweak {
		f(&d)
	}
	return d
}

func battery(pct int) func(*device) { return func(d *device) { d.Battery = pct } }
func leased(d *device)              { d.Leased = true }
func ours(d *device) {
	d.Parked, d.ParkAuto, d.ParkedBy, d.AdminState = true, true, testActor, "parked"
}
func humanParked(d *device) {
	d.Parked, d.ParkAuto, d.ParkedBy, d.AdminState = true, false, "alice", "parked"
}

// perPort wraps one device as its own unit with a client available.
func perPort(d device, tweak ...func(*unit)) unit {
	u := unit{Key: "device/" + d.ID, HostID: d.HostID, Devices: []device{d}, HasClient: true}
	for _, f := range tweak {
		f(&u)
	}
	return u
}

// ganged wraps several devices as one domain unit.
func ganged(ds ...device) unit {
	u := unit{Key: "domain/" + testHost + "/9", HostID: testHost, Ganged: true, HasClient: true}
	for _, d := range ds {
		d.DomainID, d.DomainKind = 9, "ganged"
		u.Devices = append(u.Devices, d)
		u.Siblings = append(u.Siblings, d.Devpath)
	}
	return u
}

func held(u *unit) {
	u.Held = &node.ChargeGate{HostID: u.HostID, Devpath: u.Devices[0].Devpath,
		Power: node.ChargePowerOff, Held: true, Reason: reasonPrefix + "holding"}
}

func verdictName(v verdict) string {
	return [...]string{"none", "gate", "regate", "renew", "peek", "release", "skip"}[v]
}

func wantVerdict(t *testing.T, got plan, want verdict) {
	t.Helper()
	if got.verdict != want {
		t.Fatalf("verdict = %s (%q), want %s", verdictName(got.verdict), got.reason, verdictName(want))
	}
}

// TestAnIdleDeviceOverTheCeilingIsParkedAndGated is the row the whole loop
// exists for. The park lists the device, the gate is anchored at its port,
// and the reason carries the reading so the agent's log says why.
//
// Falsify: change `maxPct > th.max` in decide to `>=`, then run with 80%.
func TestAnIdleDeviceOverTheCeilingIsParkedAndGated(t *testing.T) {
	pl := decide(perPort(dev("1", battery(95))), band)
	wantVerdict(t, pl, verdictGate)
	if len(pl.park) != 1 || pl.park[0] != "dev-1" {
		t.Fatalf("park = %v, want [dev-1]", pl.park)
	}
	if pl.anchor != "usb:3-1.1" {
		t.Fatalf("anchor = %q, want the device's own port", pl.anchor)
	}
	if !strings.Contains(pl.reason, "95%") || !strings.Contains(pl.reason, "80%") {
		t.Fatalf("reason %q does not carry the reading and the ceiling", pl.reason)
	}
	// Exactly at the ceiling is inside the band.
	wantVerdict(t, decide(perPort(dev("1", battery(80))), band), verdictNone)
	wantVerdict(t, decide(perPort(dev("1", battery(60))), band), verdictNone)
}

// TestALeasedDeviceIsInvisibleWhateverItsCharge. The rule the package is
// built around: a lease is never touched, so a leased phone at 100% is
// counted and left alone, and a gate this loop holds on a device that has
// since been leased comes off without a park being closed under it.
//
// Falsify: disable the `case leased:` arm in decide (`case leased && false:`).
func TestALeasedDeviceIsInvisibleWhateverItsCharge(t *testing.T) {
	pl := decide(perPort(dev("1", battery(100), leased)), band)
	wantVerdict(t, pl, verdictSkip)
	if pl.reason != "leased" {
		t.Fatalf("skip reason = %q, want leased", pl.reason)
	}
	if len(pl.park) != 0 {
		t.Fatalf("a leased device was scheduled for a park: %v", pl.park)
	}

	// A human unparked it and the scheduler took it while our gate was still
	// on the port: release the gate, close nothing.
	pl = decide(perPort(dev("1", battery(100), leased), held), band)
	wantVerdict(t, pl, verdictRelease)
	if pl.anchor != "usb:3-1.1" || len(pl.unpark) != 0 {
		t.Fatalf("release = %+v, want the gate off and no unpark", pl)
	}
}

// TestAHumansParkIsNeverReversed. Automation reverses only its own decision:
// a device somebody else parked is skipped, and if this loop still holds a
// gate there the gate comes off but the park stays theirs.
//
// Falsify: in decide, put `d.Parked && d.ParkAuto` devices under `ours`
// regardless of ParkedBy.
func TestAHumansParkIsNeverReversed(t *testing.T) {
	pl := decide(perPort(dev("1", battery(95), humanParked)), band)
	wantVerdict(t, pl, verdictSkip)
	if pl.reason != "foreign_park" {
		t.Fatalf("skip reason = %q, want foreign_park", pl.reason)
	}

	pl = decide(perPort(dev("1", battery(95), humanParked), held), band)
	wantVerdict(t, pl, verdictRelease)
	if len(pl.unpark) != 0 {
		t.Fatalf("a human's park was scheduled to be closed: %v", pl.unpark)
	}

	// Another automated actor's park is just as foreign.
	other := dev("1", battery(95), ours)
	other.ParkedBy = "some-other-loop"
	if pl := decide(perPort(other), band); pl.verdict != verdictSkip || pl.reason != "foreign_park" {
		t.Fatalf("another loop's park: %+v, want skip foreign_park", pl)
	}
}

// TestAHeldGateIsRenewedEveryCycle. While the agent reports the gate and the
// park is this loop's, every cycle re-asserts — that renewal is the proof of
// life the agent's dead-man's switch waits for.
//
// Falsify: return verdictNone instead of verdictRenew from the Held branch.
func TestAHeldGateIsRenewedEveryCycle(t *testing.T) {
	pl := decide(perPort(dev("1", battery(95), ours), held), band)
	wantVerdict(t, pl, verdictRenew)
	if pl.anchor != "usb:3-1.1" {
		t.Fatalf("renewal anchored at %q, want the held port", pl.anchor)
	}
	// Frozen at the value that earned the hold, still renewed: a dark port
	// cannot be read, and the peek is what refreshes it.
	wantVerdict(t, decide(perPort(dev("1", battery(81), ours), held), band), verdictRenew)
}

// TestAReadingAtTheFloorEndsTheHold. Whatever produced the fresh reading, at
// or below MIN the gate comes off and the park is closed — the release is not
// made to wait for the next peek.
//
// Falsify: delete the floor check at the top of decide's "hold in progress"
// section; the held device at 35% is then renewed.
func TestAReadingAtTheFloorEndsTheHold(t *testing.T) {
	for _, pct := range []int{35, 40} {
		pl := decide(perPort(dev("1", battery(pct), ours), held), band)
		wantVerdict(t, pl, verdictRelease)
		if len(pl.unpark) != 1 || pl.unpark[0] != "dev-1" {
			t.Fatalf("at %d%%: unpark = %v, want [dev-1]", pct, pl.unpark)
		}
		if pl.anchor != "usb:3-1.1" {
			t.Fatalf("at %d%%: release anchored at %q, want the held port", pct, pl.anchor)
		}
	}
	// One above the floor is still a hold.
	wantVerdict(t, decide(perPort(dev("1", battery(41), ours), held), band), verdictRenew)
}

// TestADroppedGateIsReassertedUnderItsPark. The ledger says parked, the agent
// says nothing is held — it restarted, or the switch fired. The gate is put
// back rather than the park closed: the reading that earned it still stands.
//
// Falsify: make the fall-through of the "hold in progress" section return
// verdictRelease.
func TestADroppedGateIsReassertedUnderItsPark(t *testing.T) {
	d := dev("1", battery(95), ours)
	d.ChargeGate = node.ChargePowerUnknown
	pl := decide(perPort(d), band)
	wantVerdict(t, pl, verdictRegate)
	if pl.reopen {
		t.Fatal("a plain re-assertion reopened the ledger; only a settled peek does that")
	}
	if len(pl.unpark) != 0 {
		t.Fatalf("a re-assertion closed parks: %v", pl.unpark)
	}
}

// TestAPeekReadsTheBatteryAndDecides. After PeekEvery of darkness the gate is
// released with the park kept; the settled reading either ends the hold or
// re-asserts the gate with the ledger reopened so the dark clock restarts.
//
// Falsify: return verdictRenew from the HoldDue branch.
func TestAPeekReadsTheBatteryAndDecides(t *testing.T) {
	due := dev("1", battery(95), ours)
	due.HoldDue = true
	pl := decide(perPort(due, held), band)
	wantVerdict(t, pl, verdictPeek)
	if pl.anchor != "usb:3-1.1" {
		t.Fatalf("peek released %q, want the held port", pl.anchor)
	}

	// Peeking, not yet settled: nothing, whatever the column says.
	peeking := dev("1", battery(95), ours)
	peeking.ChargeGate = node.ChargePowerOn
	pl = decide(perPort(peeking, func(u *unit) { u.Peeking = true }), band)
	wantVerdict(t, pl, verdictNone)

	settled := func(u *unit) { u.Peeking, u.PeekSettled = true, true }

	// Settled above the floor: gate again, and reopen the ledger.
	pl = decide(perPort(peeking, settled), band)
	wantVerdict(t, pl, verdictRegate)
	if !pl.reopen || len(pl.unpark) != 1 {
		t.Fatalf("a settled peek above the floor did not reopen the ledger: %+v", pl)
	}

	// Settled at the floor: the hold ends.
	low := dev("1", battery(38), ours)
	low.ChargeGate = node.ChargePowerOn
	pl = decide(perPort(low, settled), band)
	wantVerdict(t, pl, verdictRelease)
	if len(pl.unpark) != 1 {
		t.Fatalf("a settled peek at the floor did not close the park: %+v", pl)
	}

	// Settled with no reading at all: held again, dark is the safe direction.
	blind := dev("1", battery(-1), ours)
	blind.ChargeGate = node.ChargePowerOn
	pl = decide(perPort(blind, settled), band)
	wantVerdict(t, pl, verdictRegate)
	if !strings.Contains(pl.reason, "no battery") {
		t.Fatalf("reason %q does not say the peek read nothing", pl.reason)
	}
}

// TestAGateWithNoParkUnderItIsReleased. A human closed this loop's park while
// the port was still dark. The park was theirs to close; the gate follows.
//
// Falsify: delete the `if u.Held != nil` block at the top of decide's
// section 4.
func TestAGateWithNoParkUnderItIsReleased(t *testing.T) {
	pl := decide(perPort(dev("1", battery(95)), held), band)
	wantVerdict(t, pl, verdictRelease)
	if pl.reason != "park_closed" || len(pl.unpark) != 0 {
		t.Fatalf("release = %+v, want park_closed with nothing to unpark", pl)
	}
}

// TestAHumanUnparkEarnsACooldown. The reading that earned the park is still
// on the row, so without the cooldown the device would be parked again two
// minutes after a human freed it.
//
// Falsify: disable the `if cooldown` check in decide (`if cooldown && false`).
func TestAHumanUnparkEarnsACooldown(t *testing.T) {
	d := dev("1", battery(95))
	d.HumanUnparked = true
	pl := decide(perPort(d), band)
	wantVerdict(t, pl, verdictSkip)
	if pl.reason != "human_unparked" {
		t.Fatalf("skip reason = %q, want human_unparked", pl.reason)
	}
}

// TestNothingIsHeldWhereNoGateCanBePlaced. Each "no way to hold a port here"
// row counts the device under a reason an operator can act on, and closes
// any park of this loop's that has lost its gate.
//
// Falsify: disable the `case !u.HasClient` arm (`case false:`); the first row
// then gates.
func TestNothingIsHeldWhereNoGateCanBePlaced(t *testing.T) {
	rows := []struct {
		name   string
		unit   unit
		reason string
	}{
		{"no client", perPort(dev("1", battery(95)), func(u *unit) { u.HasClient = false }), "no_client"},
		{"no slot", perPort(dev("1", battery(95), func(d *device) { d.Devpath = "" })), "no_slot"},
		{"no agent", perPort(dev("1", battery(95), func(d *device) { d.HasAgent = false })), "no_agent"},
		{"unswitchable domain", perPort(dev("1", battery(95), func(d *device) { d.DomainKind = "none" })), "unswitchable"},
		{"no domain control", perPort(dev("1", battery(95), func(d *device) { d.DomainControl = "none" })), "unswitchable"},
		{"foreign gate", perPort(dev("1", battery(95)), func(u *unit) { u.ForeignGate = true }), "foreign_gate"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			pl := decide(r.unit, band)
			wantVerdict(t, pl, verdictSkip)
			if pl.reason != r.reason {
				t.Fatalf("skip reason = %q, want %s", pl.reason, r.reason)
			}
			// Inside the band the same shape is silent: nothing to count.
			u := r.unit
			u.Devices[0].Battery = 60
			wantVerdict(t, decide(u, band), verdictNone)
		})
	}

	// A park of ours on a device whose host lost its agent is closed: a
	// park with no gate under it is an unschedulable phone that is charging.
	pl := decide(perPort(dev("1", battery(95), ours, func(d *device) { d.HasAgent = false })), band)
	wantVerdict(t, pl, verdictRelease)
	if len(pl.unpark) != 1 {
		t.Fatalf("an orphaned park was left open: %+v", pl)
	}
}

// TestAGangedDomainIsDecidedAsOne. One switch darkens every port, so every
// device must be idle and unclaimed, every one is parked before the gate
// goes on, every sibling position is acknowledged, and the anchor is the
// hottest device. One leased neighbour stops the whole domain.
//
// Falsify: in decide's verdictGate return, send `ack: nil` instead of the
// siblings; the acknowledgement check below then fails.
func TestAGangedDomainIsDecidedAsOne(t *testing.T) {
	u := ganged(dev("1", battery(85)), dev("2", battery(95)), dev("3", battery(70)))
	pl := decide(u, band)
	wantVerdict(t, pl, verdictGate)
	if len(pl.park) != 3 {
		t.Fatalf("park = %v, want all three devices", pl.park)
	}
	if pl.anchor != "usb:3-1.2" {
		t.Fatalf("anchor = %q, want the hottest device's port", pl.anchor)
	}
	if len(pl.ack) != 3 {
		t.Fatalf("acknowledged = %v, want every position in the domain", pl.ack)
	}

	// One leased neighbour: hands off the whole domain.
	u = ganged(dev("1", battery(95)), dev("2", battery(60), leased))
	pl = decide(u, band)
	wantVerdict(t, pl, verdictSkip)
	if pl.reason != "leased" {
		t.Fatalf("skip reason = %q, want leased", pl.reason)
	}

	// One neighbour with no reading: the domain cannot be judged, so it is
	// not held — a device nobody has observed may be at 5%.
	u = ganged(dev("1", battery(95)), dev("2", battery(-1)))
	pl = decide(u, band)
	wantVerdict(t, pl, verdictSkip)
	if pl.reason != "unknown_battery" {
		t.Fatalf("skip reason = %q, want unknown_battery", pl.reason)
	}

	// Held: released when the LOWEST device reaches the floor, renewed until.
	u = ganged(dev("1", battery(90), ours), dev("2", battery(41), ours))
	held(&u)
	wantVerdict(t, decide(u, band), verdictRenew)
	u = ganged(dev("1", battery(90), ours), dev("2", battery(40), ours))
	held(&u)
	pl = decide(u, band)
	wantVerdict(t, pl, verdictRelease)
	if len(pl.unpark) != 2 {
		t.Fatalf("unpark = %v, want both devices", pl.unpark)
	}
}

// TestGroupKeysGangedDevicesByDomain. Per-port devices are units of their
// own; ganged ones on one domain share a unit and carry the domain's full
// position list as siblings, including positions with no enrolled device.
//
// Falsify: key every device by its own id in group.
func TestGroupKeysGangedDevicesByDomain(t *testing.T) {
	g1 := dev("1")
	g1.DomainID, g1.DomainKind = 9, "ganged"
	g2 := dev("2")
	g2.DomainID, g2.DomainKind = 9, "ganged"
	p := dev("3")

	units := group([]device{g1, p, g2}, map[int64][]string{9: {"usb:3-1.1", "usb:3-1.2", "usb:3-1.4"}})
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2 (one domain, one device): %+v", len(units), units)
	}
	var dom, single *unit
	for i := range units {
		if units[i].Ganged {
			dom = &units[i]
		} else {
			single = &units[i]
		}
	}
	if dom == nil || single == nil {
		t.Fatalf("units = %+v, want one ganged and one per-port", units)
	}
	if len(dom.Devices) != 2 || len(dom.Siblings) != 3 {
		t.Fatalf("domain unit has %d devices and %d siblings, want 2 and 3", len(dom.Devices), len(dom.Siblings))
	}
	if len(single.Devices) != 1 || single.Devices[0].ID != "dev-3" {
		t.Fatalf("per-port unit = %+v, want dev-3 alone", single)
	}
}

// TestAttachGatesTellsOursFromTheirs. A gate whose reason does not carry this
// loop's prefix is somebody's deliberate hold: it marks the unit foreign and
// is never adopted as Held.
//
// Falsify: drop the reason-prefix check in attachGates.
func TestAttachGatesTellsOursFromTheirs(t *testing.T) {
	p := &Policy{cfg: Config{Actor: testActor}}
	u := perPort(dev("1"))
	p.attachGates(&u, []node.ChargeGate{
		{Devpath: "usb:3-1.1", Held: true, Reason: "operator: bench test"},
		{Devpath: "usb:3-1.9", Held: true, Reason: reasonPrefix + "elsewhere"},
	})
	if u.Held != nil || !u.ForeignGate {
		t.Fatalf("unit = %+v, want ForeignGate and no Held", u)
	}

	u = perPort(dev("1"))
	p.attachGates(&u, []node.ChargeGate{
		{Devpath: "usb:3-1.1", Held: true, Reason: reasonPrefix + "holding at 95%"},
		{Devpath: "usb:3-1.1", Held: false, Reason: reasonPrefix + "released"},
	})
	if u.Held == nil || u.ForeignGate {
		t.Fatalf("unit = %+v, want Held and no foreign gate", u)
	}
}

// TestNewRefusesWhatTheAgentWouldRefuse. The hold is two intervals; above
// half the agent's cap the second interval has nowhere to go, and an
// inverted band would park everything on sight.
//
// Falsify: delete the Interval check in New.
func TestNewRefusesWhatTheAgentWouldRefuse(t *testing.T) {
	pool := offlinePool(t, 2)
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no pool", Config{}, "Pool is required"},
		{"inverted band", Config{Pool: pool, MinPct: 80, MaxPct: 40}, "0 < min < max"},
		{"max above 100", Config{Pool: pool, MinPct: 40, MaxPct: 101}, "0 < min < max"},
		{"interval above half the cap", Config{Pool: pool, Interval: node.MaxChargeGateHold/2 + time.Second}, "half"},
		{"pool too small", Config{Pool: offlinePool(t, 1)}, "at least"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}

	p, err := New(Config{Pool: pool, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if p.hold != 2*DefaultInterval {
		t.Fatalf("hold = %s, want two intervals (%s)", p.hold, 2*DefaultInterval)
	}
	if p.hold >= node.MaxChargeGateHold {
		t.Fatalf("hold %s is not below the agent's cap %s", p.hold, node.MaxChargeGateHold)
	}
}

// TestConfigMirrorsTheAgentsHoldCap. internal/config validates the interval
// against a copy of node.MaxChargeGateHold because it must stay a leaf; this
// is the assertion the copy's comment promises.
//
// Falsify: change either constant.
func TestConfigMirrorsTheAgentsHoldCap(t *testing.T) {
	if config.MaxChargeGateHold != node.MaxChargeGateHold {
		t.Fatalf("config.MaxChargeGateHold = %s, node.MaxChargeGateHold = %s; the config "+
			"validation would accept an interval the agent refuses",
			config.MaxChargeGateHold, node.MaxChargeGateHold)
	}
	if 2*config.DefaultChargeInterval > config.MaxChargeGateHold {
		t.Fatalf("the default interval %s does not leave room for a two-interval hold under %s",
			config.DefaultChargeInterval, config.MaxChargeGateHold)
	}
}

// TestPgIntervalIsADuration. Every deadline is measured against the server's
// now(); the literal handed to Postgres must be a length of time, never an
// instant, and never negative.
func TestPgIntervalIsADuration(t *testing.T) {
	if got := pgInterval(90 * time.Second); got != "90000000 microseconds" {
		t.Fatalf("pgInterval(90s) = %q", got)
	}
	if got := pgInterval(-time.Second); got != "0 microseconds" {
		t.Fatalf("pgInterval(-1s) = %q, want zero", got)
	}
}

// offlinePool is a pool that never connects: pgxpool creates connections on
// first use, so New's validation runs without a database.
func offlinePool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	pc.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
