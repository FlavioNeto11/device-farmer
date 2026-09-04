package recovery

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This file guards three fidelity properties of the recovery ladder, and one
// invariant that outranks all of them.
//
//	A  tier 0 ("observe") is reachable. It was not: farm.device_runtime.ladder_tier
//	   DEFAULTs to 0 and is only ever reset to 0, and next() asked for a rung
//	   strictly greater than it, so the cheapest rung could never be selected and
//	   every incident opened with an adb reconnect.
//	B  a power-domain acknowledgement can reach the host agent. It could not: the
//	   agent and its wire format have carried an `acknowledged` list all along,
//	   but recovery.HostRunner declared only PortPower, so the middle of the seam
//	   had no way to say "yes, I checked those three siblings, proceed".
//	C  closing a quarantine returns the device to service immediately, in the same
//	   transaction, rather than whenever the recovery loop next ticks — and it
//	   restores farm.devices.admin_state, which nothing anywhere used to do.
//
// The invariant: a lease ends when the job says so, when a user-written deadline
// elapses, or when a human takes it back. Nothing else. None of the three fixes
// may end, shorten or touch a lease, and TestRecoveryNeverTouchesALease is what
// says so about the code as it actually stands rather than as its comments hope.

// shippedTiers mirrors the rows migrations/00003_ops.sql seeds into
// farm.recovery_tiers, in the order Ladder.tiers reads them (ORDER BY tier).
// Only the fields next() and rungAfter() consult are populated.
func shippedTiers() []tier {
	return []tier{
		{Tier: 0, Name: "observe", BlastRadius: "device", RequiresPolicy: "no_disruption"},
		{Tier: 1, Name: "adb_reconnect", BlastRadius: "device", RequiresPolicy: "no_disruption"},
		{Tier: 2, Name: "transport_reset", BlastRadius: "device", RequiresPolicy: "allow_soft_reset"},
		{Tier: 3, Name: "usb_reset", BlastRadius: "device", RequiresPolicy: "allow_soft_reset"},
		{Tier: 4, Name: "port_power", BlastRadius: "power_domain", RequiresPolicy: "allow_port_power_cycle"},
		{Tier: 5, Name: "device_reboot", BlastRadius: "device", RequiresPolicy: "allow_port_power_cycle"},
		{Tier: 6, Name: "quarantine", BlastRadius: "device", RequiresPolicy: "no_disruption"},
		{Tier: 7, Name: "adb_restart", BlastRadius: "host", RequiresPolicy: "allow_port_power_cycle"},
		{Tier: 8, Name: "host_drain", BlastRadius: "host", RequiresPolicy: "no_disruption"},
	}
}

// ---------------------------------------------------------------------------
// Defect A: tier 0 is reachable, and the climb is still monotonic
// ---------------------------------------------------------------------------

// TestFreshDeviceStartsAtObserve is the whole of defect A in one assertion.
//
// 0 is both the column default for a device that has never had an incident and
// the value every reset writes, so if next() cannot return tier 0 for cur=0 then
// nothing in the farm can ever run the observe rung.
//
// Falsify: change next's comparison back to `t.Tier > cur`.
func TestFreshDeviceStartsAtObserve(t *testing.T) {
	got := next(shippedTiers(), 0)
	if got.Tier != 0 || got.Name != "observe" {
		t.Fatalf("a device at ladder_tier 0 must be offered the observe rung, got tier %d (%s). "+
			"ladder_tier DEFAULTs to 0 and is only ever reset to 0, so a rung this "+
			"predicate cannot reach is a rung the farm never runs", got.Tier, got.Name)
	}
}

// TestLadderClimbsEveryRungExactlyOnce walks a whole incident the way the loop
// does — pick a rung, spend it, write rungAfter into ladder_tier, repeat — and
// insists the walk visits every enabled rung, in order, without repeating one
// and without skipping one.
//
// This is the assertion that keeps defect A's fix honest in both directions. A
// `>=` in next() paired with a ladder_tier that still stored the rung just spent
// would make the ladder repeat every rung forever and never reach quarantine.
//
// Falsify either half: revert next() to `t.Tier > cur` (the walk then starts at
// tier 1), or make rungAfter return t.Tier (the walk then never leaves tier 0).
func TestLadderClimbsEveryRungExactlyOnce(t *testing.T) {
	tiers := shippedTiers()

	var walk []int
	cur := 0 // a fresh farm.device_runtime row
	for i := 0; i < len(tiers); i++ {
		rung := next(tiers, cur)
		walk = append(walk, rung.Tier)
		cur = rungAfter(rung)
	}

	want := []int{0, 1, 2, 3, 4, 5, 6, 7, 8}
	if !reflect.DeepEqual(walk, want) {
		t.Fatalf("the ladder walked %v; it must climb %v — every rung, in order, once", walk, want)
	}
}

// TestExhaustedLadderRepeatsItsTopRung: a device that has run out of ladder
// repeats host_drain under host_drain's own six-hourly cooldown. That is the
// ladder's way of saying "still broken, still needs a human" — and it must not
// silently fall back to a cheaper, more disruptive-per-hour rung.
//
// Falsify: return tiers[0] instead of tiers[len(tiers)-1] from next's fallback.
func TestExhaustedLadderRepeatsItsTopRung(t *testing.T) {
	tiers := shippedTiers()
	top := tiers[len(tiers)-1]

	cur := rungAfter(top)
	for i := 0; i < 3; i++ {
		rung := next(tiers, cur)
		if rung.Tier != top.Tier {
			t.Fatalf("an exhausted ladder offered tier %d (%s); it must repeat its top rung %d (%s)",
				rung.Tier, rung.Name, top.Tier, top.Name)
		}
		cur = rungAfter(rung)
	}
}

// TestNextSkipsRungsAnOperatorDisabled: farm.recovery_tiers.enabled is an
// operator knob and Ladder.tiers filters on it, so the tier numbers the ladder
// sees have gaps. rungAfter stores the first integer ABOVE the rung just spent
// rather than a position in the slice, precisely so the gap is resolved against
// whatever the table says on the cycle that reads it.
//
// Falsify: make rungAfter return the next tier number present in the slice at
// the time it was called, then disable a middle rung.
func TestNextSkipsRungsAnOperatorDisabled(t *testing.T) {
	// An operator has disabled usb_reset (3) and port_power (4) — the two rungs
	// that need a host agent this farm does not run.
	var tiers []tier
	for _, tr := range shippedTiers() {
		if tr.Tier == 3 || tr.Tier == 4 {
			continue
		}
		tiers = append(tiers, tr)
	}

	spent := next(tiers, 0)
	if spent.Tier != 0 {
		t.Fatalf("still expected to start at observe, got tier %d", spent.Tier)
	}
	for _, want := range []int{1, 2, 5, 6, 7, 8} {
		got := next(tiers, rungAfter(spent))
		if got.Tier != want {
			t.Fatalf("after spending tier %d the ladder offered tier %d; the next ENABLED rung is %d",
				spent.Tier, got.Tier, want)
		}
		spent = got
	}
}

// TestQuarantineCloseResetIsTheStartOfTheLadder ties defect A to defect C.
//
// Both reconcileQuarantines and the API's close handler write ladder_tier = 0.
// An operator who closes a quarantine has usually just fixed something, and the
// promise made in both places is that the next incident starts from the cheapest
// rung. That promise is only true if 0 selects tier 0.
func TestQuarantineCloseResetIsTheStartOfTheLadder(t *testing.T) {
	const afterClose = 0 // what both reset paths write

	if got := next(shippedTiers(), afterClose); got.Tier != 0 {
		t.Fatalf("after a quarantine close the ladder resumes at tier %d (%s); "+
			"both reset paths write ladder_tier = 0 and promise the cheapest rung",
			got.Tier, got.Name)
	}
}

// ---------------------------------------------------------------------------
// Defect B: the acknowledgement can reach the agent, and cannot be smuggled
// ---------------------------------------------------------------------------

// plainRunner is a HostRunner from before the seam was widened: it can cycle a
// port, and it has no way to be told about the port's neighbours.
type plainRunner struct {
	usbResets  []string
	portPowers []string
	err        error
}

func (p *plainRunner) USBReset(_ context.Context, _, devpath string) error {
	p.usbResets = append(p.usbResets, devpath)
	return p.err
}

func (p *plainRunner) PortPower(_ context.Context, _, devpath string) error {
	p.portPowers = append(p.portPowers, devpath)
	return p.err
}

// domainRunner also implements DomainPowerRunner, as *node.Agent does.
type domainRunner struct {
	plainRunner
	domainCalls  int
	gotDevpath   string
	gotAck       []string
	gotHostID    string
	domainReturn error
}

func (d *domainRunner) PortPowerWithDomain(_ context.Context, hostID, devpath string, ack []string) error {
	d.domainCalls++
	d.gotHostID = hostID
	d.gotDevpath = devpath
	d.gotAck = ack
	return d.domainReturn
}

// tier4 builds a tier-4 action. ADBEndpoint is deliberately empty: with no
// endpoint the actuator's post-action confirmation cannot dial anything, so the
// rung reports no_change and every test here stays hermetic.
func tier4(ack []string) Action {
	return Action{
		Tier: 4, TierName: "port_power",
		DeviceID: "11111111-1111-1111-1111-111111111111",
		SlotID:   7, Devpath: "usb:3-1.4", RackSlot: "r1u3s04",
		HostID: "h01", Acknowledged: ack,
	}
}

// TestAcknowledgementReachesAHostRunnerThatCanTakeIt is defect B: the capability
// existed on both ends of the wire and was unreachable from the middle.
//
// Falsify: drop the DomainPowerRunner branch from ADBActuator.portPower and call
// PortPower unconditionally.
func TestAcknowledgementReachesAHostRunnerThatCanTakeIt(t *testing.T) {
	runner := &domainRunner{}
	act := NewADBActuator(nil, runner)

	ack := []string{"usb:3-1.5", "usb:3-1.6"}
	res, err := act.Recover(context.Background(), tier4(ack))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if runner.domainCalls != 1 {
		t.Fatalf("PortPowerWithDomain called %d times, want 1: an acknowledgement the "+
			"control plane went to the trouble of checking must reach the agent",
			runner.domainCalls)
	}
	if len(runner.portPowers) != 0 {
		t.Fatalf("the plain PortPower was also called (%v); the acknowledgement would be lost "+
			"and the agent would refuse a cycle the ladder had already been cleared for",
			runner.portPowers)
	}
	if runner.gotHostID != "h01" || runner.gotDevpath != "usb:3-1.4" {
		t.Fatalf("agent addressed as host %q devpath %q, want h01 / usb:3-1.4 — a rung "+
			"addressed to the wrong position cuts power to the wrong rack",
			runner.gotHostID, runner.gotDevpath)
	}
	if !reflect.DeepEqual(runner.gotAck, ack) {
		t.Fatalf("agent acknowledged %v, want %v", runner.gotAck, ack)
	}

	// What was authorised has to be a record, not an inference: this is the list
	// that lands in farm.recovery_attempts.detail.
	if got, ok := res.Detail["acknowledged"].([]string); !ok || !reflect.DeepEqual(got, ack) {
		t.Fatalf("Detail[acknowledged] = %#v, want %v — a cycle that darkened neighbours "+
			"must record which ones", res.Detail["acknowledged"], ack)
	}
}

// TestNoAcknowledgementLeavesTheDefaultExactlyAsSafe: an empty list must take
// the old path, which authorises the target and nothing else.
//
// Falsify: make ADBActuator.portPower call PortPowerWithDomain with a nil slice
// whenever the runner supports it.
func TestNoAcknowledgementLeavesTheDefaultExactlyAsSafe(t *testing.T) {
	runner := &domainRunner{}
	act := NewADBActuator(nil, runner)

	if _, err := act.Recover(context.Background(), tier4(nil)); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if runner.domainCalls != 0 {
		t.Fatalf("PortPowerWithDomain was called with no acknowledgement to deliver; " +
			"the no-acknowledgement path must stay the single-device one")
	}
	if !reflect.DeepEqual(runner.portPowers, []string{"usb:3-1.4"}) {
		t.Fatalf("PortPower calls = %v, want exactly [usb:3-1.4]", runner.portPowers)
	}
}

// TestAcknowledgementIsNotSmuggledToARunnerThatCannotTakeIt.
//
// A runner that predates the seam cannot be handed a wider blast radius than its
// own signature admits, so the acknowledgement is DROPPED — the safe direction,
// because the agent will refuse a ganged cycle it was not told about. But a
// refusal nobody can explain is the failure mode this package exists to avoid,
// so the detail has to say the acknowledgement could not be delivered.
//
// Falsify: delete the acknowledgement_undeliverable branch, or make the detail
// silent about it.
func TestAcknowledgementIsNotSmuggledToARunnerThatCannotTakeIt(t *testing.T) {
	runner := &plainRunner{}
	act := NewADBActuator(nil, runner)

	res, err := act.Recover(context.Background(), tier4([]string{"usb:3-1.5"}))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !reflect.DeepEqual(runner.portPowers, []string{"usb:3-1.4"}) {
		t.Fatalf("PortPower calls = %v, want exactly [usb:3-1.4]", runner.portPowers)
	}
	note, _ := res.Detail["acknowledgement_undeliverable"].(string)
	if note == "" {
		t.Fatalf("Detail = %#v: an acknowledgement that could not be delivered must be "+
			"recorded, or the agent's refusal reads as an unexplained gap", res.Detail)
	}
	if got, ok := res.Detail["acknowledged"].([]string); !ok || len(got) != 1 {
		t.Fatalf("Detail[acknowledged] = %#v: record what was checked even when it could "+
			"not be sent", res.Detail["acknowledged"])
	}
}

// TestActuatorDoesNotAliasTheAcknowledgement: the ladder's slice outlives the
// call — it goes into the attempt detail — so the actuator must hand the agent a
// copy. An agent that mutated or retained an alias would change what the record
// says was authorised, after the fact.
//
// Falsify: pass act.Acknowledged straight through instead of copying it.
func TestActuatorDoesNotAliasTheAcknowledgement(t *testing.T) {
	runner := &domainRunner{}
	act := NewADBActuator(nil, runner)

	ack := []string{"usb:3-1.5", "usb:3-1.6"}
	if _, err := act.Recover(context.Background(), tier4(ack)); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	ack[0] = "usb:9-9.9" // the caller reuses its buffer
	if runner.gotAck[0] != "usb:3-1.5" {
		t.Fatalf("the agent's acknowledgement aliased the caller's slice: it now reads %v. "+
			"The record of which neighbours were authorised must not be rewritable "+
			"by whoever built it", runner.gotAck)
	}
}

// TestNoHostAgentIsStillARefusalNamingWhatIsMissing. Widening the seam must not
// blunt the refusal that made the seam's absence visible in the first place:
// reporting "no change" for a rung nobody attempted would let the ladder
// conclude the hardware is unrecoverable and quarantine a device whose actual
// problem is that nobody is listening on the host.
//
// Falsify: return OutcomeNoChange from hostLocal's nil-runner branch.
func TestNoHostAgentIsStillARefusalNamingWhatIsMissing(t *testing.T) {
	act := NewADBActuator(nil, nil)

	for _, tc := range []struct {
		name string
		a    Action
		want string
	}{
		{"tier 3", Action{Tier: 3, TierName: "usb_reset", HostID: "h01"}, "USBDEVFS_RESET"},
		{"tier 4 without an acknowledgement", tier4(nil), "VBUS power cycle"},
		{"tier 4 with an acknowledgement", tier4([]string{"usb:3-1.5"}), "VBUS power cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := act.Recover(context.Background(), tc.a)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if res.Outcome != OutcomeRefused {
				t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeRefused)
			}
			refusal, _ := res.Detail["refusal"].(string)
			if !strings.Contains(refusal, tc.want) || !strings.Contains(refusal, "farmd-node") {
				t.Fatalf("refusal = %q; it must name the operation (%q) and the agent that "+
					"could perform it", refusal, tc.want)
			}
		})
	}
}

// TestHostRungFailureIsFailedNotRecovered keeps the outcome vocabulary honest
// across the detail-merging rewrite of hostLocal: refused, failed and no_change
// each send an operator somewhere different, and a false "recovered" suppresses
// the page that should have followed.
//
// Falsify: return OutcomeRecovered (or OutcomeRefused) from hostLocal's error
// branch, or drop the extra-detail merge so `acknowledged` vanishes on failure.
func TestHostRungFailureIsFailedNotRecovered(t *testing.T) {
	boom := errors.New("uhubctl: exit status 1")
	runner := &domainRunner{domainReturn: boom}
	act := NewADBActuator(nil, runner)

	res, err := act.Recover(context.Background(), tier4([]string{"usb:3-1.5"}))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q: the rung ran and errored", res.Outcome, OutcomeFailed)
	}
	if got, _ := res.Detail["error"].(string); got != boom.Error() {
		t.Fatalf("Detail[error] = %q, want %q", got, boom.Error())
	}
	if _, ok := res.Detail["acknowledged"]; !ok {
		t.Fatalf("Detail = %#v: a failed cycle must still record which neighbours it was "+
			"willing to darken — that is the forensic half", res.Detail)
	}
}

// TestUnknownTierIsRefusedNotFaked. Tiers 0, 6 and 8 are database actions the
// ladder performs itself; anything else reaching the actuator is a tier table
// that gained a rung nobody taught it.
func TestUnknownTierIsRefusedNotFaked(t *testing.T) {
	act := NewADBActuator(nil, &domainRunner{})
	res, err := act.Recover(context.Background(), Action{Tier: 6, TierName: "quarantine"})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Outcome != OutcomeRefused {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeRefused)
	}
	if refusal, _ := res.Detail["refusal"].(string); !strings.Contains(refusal, "quarantine") {
		t.Fatalf("refusal = %q; it must name the tier it could not perform", refusal)
	}
}

// ---------------------------------------------------------------------------
// Defect C: the close finishes in the handler
// ---------------------------------------------------------------------------

// opsSource is internal/api/ops.go, the file that owns the quarantine-close
// handler. It is read rather than imported because the assertions below are
// about the SHAPE of the handler — what it writes, and inside which transaction
// — and there is no database in `go test`.
const opsSource = "../api/ops.go"

// TestQuarantineCloseReturnsDevicesToServiceInOneTransaction.
//
// Before this change the handler issued exactly one UPDATE — farm.quarantines
// SET closed_at, closed_by — then audited and returned. It never wrote
// farm.device_runtime.health, which is what farm.lease_acquire consults, and
// nothing anywhere restored farm.devices.admin_state. An operator closed a
// quarantine and the device stayed out of service, with no indication why,
// until (and only if) the recovery loop's reconcileQuarantines next ran.
//
// Falsify, one at a time: delete the device_runtime UPDATE from the release
// statement; delete the devices UPDATE; move the release after tx.Commit; or
// replace the transaction with two independent s.pool calls.
func TestQuarantineCloseReturnsDevicesToServiceInOneTransaction(t *testing.T) {
	body := funcBody(t, opsSource, "handleQuarantineClose")

	mustContain := []struct {
		frag string
		why  string
	}{
		{"s.pool.Begin(", "the close and the release must be one transaction, or an operator's " +
			"action can land half-done"},
		{"UPDATE farm.quarantines", "the close itself"},
		{"farm.device_runtime", "health is what farm.lease_acquire actually consults; closing " +
			"the quarantine row alone leaves the device out of service"},
		{"health = 'unknown'", "closing a quarantine is a human saying \"look again\", not an " +
			"observation: the watchdog decides whether the device is healthy"},
		{"ladder_tier = 0", "the next incident must start at the cheapest rung, not answer the " +
			"operator's repair with whatever rung the ladder had climbed to"},
		{"UPDATE farm.devices", "admin_state is the other half; nothing anywhere restored it"},
		{"admin_state = 'enabled'", "a device left at admin_state 'quarantined' never returns " +
			"to the allocation index"},
		{"tx.Commit(", "the transaction has to commit"},
	}
	for _, m := range mustContain {
		if !strings.Contains(body, m.frag) {
			t.Fatalf("handleQuarantineClose no longer contains %q: %s", m.frag, m.why)
		}
	}

	// Ordering: both writes must be inside the transaction, before the commit.
	commit := strings.Index(body, "tx.Commit(")
	begin := strings.Index(body, "s.pool.Begin(")
	for _, frag := range []string{"UPDATE farm.quarantines", "farm.device_runtime", "UPDATE farm.devices"} {
		at := strings.Index(body, frag)
		if at < begin || at > commit {
			t.Fatalf("%q is written outside the close transaction (begin=%d, stmt=%d, commit=%d): "+
				"a release that is not atomic with the close can leave a closed quarantine "+
				"and an out-of-service device", frag, begin, at, commit)
		}
	}

	// The release must be issued on the transaction, not on the pool.
	if strings.Count(body, "tx.QueryRow(") < 2 {
		t.Fatalf("handleQuarantineClose issues fewer than two statements on tx; the close and " +
			"the release must both be inside it")
	}

	// Only the guarded transitions. Closing a quarantine does not overrule a
	// 'disabled' or 'retired' device, and does not promote health to 'healthy'.
	for _, forbidden := range []string{"health = 'healthy'", "admin_state = 'retired'"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("handleQuarantineClose writes %q; closing a quarantine may only undo a "+
				"quarantine, never make a claim about the device or overrule another "+
				"operator's decision", forbidden)
		}
	}
}

// TestQuarantineCloseOnlyFreesDevicesNoOtherQuarantineCovers: a device inside
// both a hub quarantine and a device quarantine must stay out of service when
// only one of them is closed. Both this handler and reconcileQuarantines have to
// agree on that, and they agree by using the same shape of NOT EXISTS.
//
// Falsify: drop the NOT EXISTS clause from the release statement.
func TestQuarantineCloseOnlyFreesDevicesNoOtherQuarantineCovers(t *testing.T) {
	body := funcBody(t, opsSource, "handleQuarantineClose")

	release := body[strings.Index(body, "WITH covered AS"):]
	for _, frag := range []string{
		"NOT EXISTS",
		"FROM farm.quarantines q",
		"q.closed_at IS NULL",
		"q.device_id = c.device_id",
		"q.slot_id   = c.current_slot_id",
		"q.hub_id    = c.hub_id",
		"q.host_id   = c.host_id",
	} {
		if !strings.Contains(release, frag) {
			t.Fatalf("the release statement no longer tests %q: closing one quarantine would "+
				"free devices another still covers", frag)
		}
	}
}

// TestQuarantineCoverageIsDecidedByScope guards the trap that makes a
// quarantine predicate look right and behave catastrophically.
//
// farm.quarantines rows carry more subject columns than their scope uses:
// Ladder.quarantineDevice inserts scope='device' WITH host_id, and
// openHubQuarantine inserts scope='hub' WITH host_id, both so the row can be
// reported without a join. A predicate that reads host_id without first checking
// scope therefore reads ONE broken phone as a quarantine over its entire host.
//
// The consequences run in both directions and hide each other:
//
//   - in a NOT EXISTS ("is this device still covered?"), one stuck phone on a
//     host makes every quarantine close on that host a silent no-op, and makes
//     every device on that host invisible to the recovery ladder — no attempt,
//     no refusal, no record;
//   - in a coverage set ("which devices did this quarantine cover?"), closing
//     one phone's quarantine resets health, ladder_tier and admin_state for
//     every quarantined device on the host.
//
// Falsify: replace any scope-guarded arm with a bare column comparison, e.g.
// `q.host_id = s.host_id` in coveredByQuarantine.
func TestQuarantineCoverageIsDecidedByScope(t *testing.T) {
	for _, tc := range []struct {
		what     string
		sql      string
		arms     []string
		fallback string
	}{
		{
			what: "internal/recovery coveredByQuarantine",
			sql:  coveredByQuarantine,
			arms: []string{
				"q.scope = 'device' AND q.device_id = d.id",
				"q.scope = 'slot'   AND q.slot_id   = s.id",
				"q.scope = 'hub'    AND q.hub_id    = s.hub_id",
				"q.scope = 'host'   AND q.host_id   = s.host_id",
			},
			fallback: "q.scope NOT IN ('device','slot','hub','host')",
		},
		{
			what: "the quarantine-close release statement",
			sql:  funcBody(t, opsSource, "handleQuarantineClose"),
			arms: []string{
				// which devices the closed row covered
				"$1::text = 'device' AND d.id = $2::uuid",
				"$1::text = 'slot'   AND d.current_slot_id = $3::bigint",
				"$1::text = 'hub'    AND s.hub_id = $4::bigint",
				"$1::text = 'host'   AND s.host_id = $5::text",
				// which devices another open row still covers
				"q.scope = 'device' AND q.device_id = c.device_id",
				"q.scope = 'slot'   AND q.slot_id   = c.current_slot_id",
				"q.scope = 'hub'    AND q.hub_id    = c.hub_id",
				"q.scope = 'host'   AND q.host_id   = c.host_id",
			},
			fallback: "q.scope NOT IN ('device','slot','hub','host')",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			for _, arm := range tc.arms {
				if !strings.Contains(tc.sql, arm) {
					t.Errorf("%s no longer guards a subject column by scope: missing %q. "+
						"A scope='device' row carries host_id too, so an unguarded "+
						"host_id comparison reads one broken phone as a whole-host "+
						"quarantine", tc.what, arm)
				}
			}
			if !strings.Contains(tc.sql, tc.fallback) {
				t.Errorf("%s has no arm for a scope it cannot express (the table's CHECK "+
					"allows 'power_domain', which has no column here). Such a row must "+
					"over-cover, never be ignored", tc.what)
			}
		})
	}
}

// TestQuarantineClosesAreSerialised. Two quarantines can cover one device — an
// auto device quarantine inside a hub quarantine is ordinary. Under READ
// COMMITTED, two closes running at once each evaluate "is anything else still
// covering this device?" against a snapshot in which the other row is still
// open, so NEITHER releases it: both commit, both report zero, and the device is
// stranded out of service with no open quarantine left to explain it.
//
// Falsify: delete the pg_advisory_xact_lock, or move it after the UPDATE that
// closes the row.
func TestQuarantineClosesAreSerialised(t *testing.T) {
	body := funcBody(t, opsSource, "handleQuarantineClose")

	lock := strings.Index(body, "pg_advisory_xact_lock")
	if lock < 0 {
		t.Fatalf("handleQuarantineClose no longer serialises itself; two concurrent closes " +
			"of overlapping quarantines can each leave the device to the other")
	}
	if update := strings.Index(body, "UPDATE farm.quarantines"); lock > update {
		t.Fatalf("the lock is taken at %d, after the close at %d. A lock taken after the row "+
			"is read is a lock that has already lost the race", lock, update)
	}
	if !strings.Contains(body, "tx.Exec(") {
		t.Fatalf("the lock must be a transaction lock taken on the close's own transaction, " +
			"or it is released before the work it guards")
	}
}

// ---------------------------------------------------------------------------
// The invariant that outranks all three
// ---------------------------------------------------------------------------

// leaseCalls are the stored procedures that move a lease. A call is recognised
// by its opening parenthesis, so the prose in a doc comment or a metric Help
// string can name one — several deliberately do, to explain why they must not
// be reached — without being mistaken for one.
var leaseCalls = regexp.MustCompile(
	`(?i)\bfarm\.(lease_acquire|lease_renew|lease_release|lease_reclaim|lease_witness|` +
		`lease_mark_suspect|lease_expire_max_runtime|reaper_arm)\s*\(`)

// leaseWrites are the ways a lease can be ended, shortened or moved with plain
// SQL. They are matched only against literals that are whole SQL statements.
var leaseWrites = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bupdate\s+farm\.leases\b`),
	regexp.MustCompile(`(?i)\binsert\s+into\s+farm\.leases\b`),
	regexp.MustCompile(`(?i)\bdelete\s+from\s+farm\.leases\b`),
	// The denormalised pointer and the fence floor are the lease's own
	// bookkeeping; writing either from here would move a fence with no lease
	// transition behind it. expires_at is the deadline itself.
	regexp.MustCompile(`(?i)\bset\b[^;]*\bcurrent_lease_id\s*=`),
	regexp.MustCompile(`(?i)\bset\b[^;]*\bfence_floor\s*=`),
	regexp.MustCompile(`(?i)\bset\b[^;]*\bexpires_at\s*=`),
}

// sqlStatement recognises a literal that is a whole SQL statement rather than a
// sentence about one.
var sqlStatement = regexp.MustCompile(`(?is)^\s*(with|select|insert|update|delete)\b`)

// TestRecoveryNeverTouchesALease is the invariant, checked against the SQL this
// code actually sends rather than against the comments that promise it.
//
// A lease ends when the job says so, when a user-written deadline elapses, or
// when a human takes it back. Nothing else. Recovery acts ON BEHALF OF a holder
// that KEEPS its device: the clock keeps ticking and the fence never moves. Every
// one of the three fixes above had a plausible-looking shortcut that would have
// broken this — a quarantine close that "tidied up" the lease it found, an
// acknowledgement built by releasing the neighbours first, a ladder that reset a
// device by taking it back — and none of them is taken.
//
// Falsify: add `UPDATE farm.leases SET state='released' WHERE ...` to any string
// literal in internal/recovery or in the quarantine-close handler.
func TestRecoveryNeverTouchesALease(t *testing.T) {
	const whyNot = "A lease ends when the job says so, when a user-written deadline elapses, " +
		"or when a human takes it back. Nothing else, and recovery is none of those."

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	files = append(files, opsSource)

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for _, lit := range stringLiterals(t, f) {
			flat := strings.Join(strings.Fields(lit), " ")
			if leaseCalls.MatchString(flat) {
				t.Errorf("%s calls a lease procedure:\n\t%s\n%s", f, flat, whyNot)
			}
			if !sqlStatement.MatchString(flat) {
				continue
			}
			for _, re := range leaseWrites {
				if re.MatchString(flat) {
					t.Errorf("%s contains SQL matching %s:\n\t%s\n%s", f, re, flat, whyNot)
				}
			}
		}
	}
}

// TestRecoveryDoesNotImportTheLeasePackage. The package doc calls the import
// list the enforcement rather than a code-review convention; this is the check
// that makes that true. internal/lease is where lease_release and lease_reclaim
// live, and a recovery loop that can reach them is one refactor away from
// ending somebody's six-hour test run to fix a USB cable.
func TestRecoveryDoesNotImportTheLeasePackage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasSuffix(path, "/internal/lease") {
				t.Errorf("%s imports %s. The recovery package must not be able to name a "+
					"lease ending, in any function, ever.", f, path)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Source helpers
// ---------------------------------------------------------------------------

// funcBody returns the source text of one top-level function or method body.
func funcBody(t *testing.T, path, name string) string {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		lo := fset.Position(fn.Body.Pos()).Offset
		hi := fset.Position(fn.Body.End()).Offset
		return string(src[lo:hi])
	}
	t.Fatalf("%s: no function %s — the fidelity tests are pinned to it by name", path, name)
	return ""
}

// stringLiterals returns every string literal in a Go file, with raw and
// interpreted quoting removed.
func stringLiterals(t *testing.T, path string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, s)
		}
		return true
	})
	return out
}
