package e2e

// REC-02: a rung whose blast radius exceeds what a live lease permits is
// refused, and the refusal is recorded.
//
// # What is actually at stake
//
// The recovery ladder repairs devices UNDERNEATH live leases. That is only
// safe because of one rule: before a rung runs, the ladder asks what the rung
// would disturb and whether every live lease in that scope carries a
// disruption_policy that permits it. When one does not, the rung is REFUSED —
// not downgraded, not deferred silently, and above all not performed anyway.
// A broken phone that still holds somebody's six-hour test run stays broken
// until the run ends. DeviceFarmer/STF issue #663 is what the other trade
// looks like.
//
// The unit tests in internal/recovery prove that checkBlastRadius returns the
// right sentence for the right row. What they cannot prove is the thing an
// operator is actually buying: that on a real farm, with the shipped binary
// sweeping a real database every fifteen seconds against real hardware, a
// device that is genuinely broken under a genuinely live no_disruption lease
// is left alone — and that the lease is still there afterwards, at the same
// fence, held by the same holder.
//
// # The four properties this scenario holds
//
//  1. The ladder ATTEMPTED and REFUSED. There is a farm.recovery_attempts row
//     with outcome='refused' and a refusal sentence naming the rung, the
//     policy it needed, and the lease that would not give it.
//  2. The refusal did NOT spend the rung. farm.device_runtime.ladder_tier
//     holds the lowest rung NOT YET SPENT, so a refused rung must leave it
//     exactly where it was: the ladder may not climb towards a port power
//     cycle on the strength of a rung that never ran.
//  3. THE LEASE DID NOT MOVE. Same id, same fence, still held, still the
//     device's current_lease_id — held across a window in which the ladder
//     refused the same rung again and again. This is the point of the whole
//     requirement, and it is asserted with Consistently because a negative
//     checked once passes on a farm where the wrong thing is about to happen.
//  4. The METRIC moved. DeviceFarmerRecoveryRefusedPolicy alerts on
//     farm_recovery_refusals_total, and farm_recovery_attempts_total is what
//     an operator groups by hub at 3am. A property proved only in SQL leaves
//     both of those unproven.
//
// # And the contrast, without which none of it means anything
//
// "Refused" and "the ladder never ran" are indistinguishable from the
// refusal side alone. So a SECOND device is broken in exactly the same way,
// at exactly the same rung, differing in one value: its job says
// allow_port_power_cycle. That rung is not refused — the ladder opens an
// attempt, performs it against the fake hardware, and spends the rung. The
// two devices differ in the disruption policy of their job and in nothing
// else, so the difference in outcome can only be the blast-radius check.
//
// Falsify: make checkBlastRadius in internal/recovery/ladder.go return
// ("", "", nil) before it reads farm.leases. Run against that, the permitted
// half is unchanged and the refused half fails at the wait for a refusal that
// never comes — with the row that replaced it quoted in the failure:
//
//	attempt 1 tier=5 outcome="failed" refusal: (none)
//	detail: {"rung":"device_reboot", …, "from_tier":5, "rung_spent":true}
//
// which is the ladder rebooting a handset in the middle of somebody's
// no_disruption run and then climbing a rung for having done it.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// The rung this scenario is about, and why it is this one rather than another.
//
// Two things had to be true of it at once.
//
// Its blast radius has to be a single DEVICE while its requires_policy is above
// no_disruption. That is what isolates the property under test: the refusal can
// only be about the POLICY, never about a neighbour that happens to share a hub
// or a power domain, so neither half of the contrast can come out right for an
// accidental reason.
//
// And the permitted half has to be a rung the fake hardware can actually
// answer, or its verdict would be a refusal too and the contrast would prove
// nothing. That rules out the cheaper device-radius rungs above no_disruption:
// tier 2 (transport_reset) uses the detach/attach verbs test/fakeadb does not
// implement, which the actuator correctly reads as a server refusing a verb it
// does not have, and tier 3 (usb_reset) needs a farmd-node agent this farm has
// none of. device_reboot is the first rung where both hold: adb reboot goes
// through a transport switch, which the fake serves.
const (
	refusedTier       = 5
	refusedTierName   = "device_reboot"
	refusedTierRadius = "device"
	refusedTierPolicy = "allow_port_power_cycle"

	// forbiddingPolicy is what the refused device's job carries. It is the
	// bottom of the ordering in policyRank, so it permits no rung above
	// no_disruption.
	forbiddingPolicy = "no_disruption"

	// obsTierLabel is the label farm_recovery_attempts_total carries for this
	// rung: internal/obs keeps a deliberately coarser tier vocabulary than
	// farm.recovery_tiers and folds device_reboot onto soft_reset. The metric
	// assertion has to name the folded value, not the table's.
	obsTierLabel = "soft_reset"

	// refusedPolicyOutcome is the obs outcome for a refusal that came from a
	// lease's policy rather than from a ganged power domain. Both halves of
	// the label pair matter to the alert: a rising refused_ganged rate means
	// buy per-port hubs, refused_policy means tenants are simply strict.
	refusedPolicyOutcome = "refused_policy"
)

func TestRecoveryRefusesARungALiveLeaseForbids(t *testing.T) {
	// api to file the jobs the way a tenant does, scheduler to place them on
	// devices, recovery to sweep. Deliberately NO jobrunner: this scenario
	// needs a lease that stays live while the ladder runs underneath it, and
	// the only honest way to get one is to let the work not finish. And
	// deliberately no reaper — nothing here may be able to end a lease.
	f := newFarm(t, farmOpts{Roles: []string{"api", "scheduler", "recovery"}})

	requireRungShape(t, f)

	// Two positions on two different hosts, each on a hub whose other devices
	// are all healthy. The separation is load-bearing: the ladder correlates
	// failures, and two broken devices on one four-port hub would reach the
	// hub-fault quorum and be answered with a single hub quarantine instead of
	// two per-device decisions — which is correct behaviour that would leave
	// this scenario with nothing to measure.
	victims := pickIsolatedPositions(t, f, 2)
	refused, permitted := victims[0], victims[1]
	t.Logf("refused half: %s at %s on host %s (hub %s)",
		refused.rackSlot, refused.devpath, refused.hostID, refused.hubPath)
	t.Logf("permitted half: %s at %s on host %s (hub %s)",
		permitted.rackSlot, permitted.devpath, permitted.hostID, permitted.hubPath)

	// -----------------------------------------------------------------
	// Two tenants take two phones. The jobs differ in one value.
	// -----------------------------------------------------------------

	refusedJob := submitHoldingJob(t, f, refused.deviceID, forbiddingPolicy)
	permittedJob := submitHoldingJob(t, f, permitted.deviceID, refusedTierPolicy)

	refusedLease := awaitLease(t, f, refusedJob)
	permittedLease := awaitLease(t, f, permittedJob)
	t.Logf("lease %s (fence %d) holds %s under %s; lease %s (fence %d) holds %s under %s",
		refusedLease.id, refusedLease.fence, refused.rackSlot, forbiddingPolicy,
		permittedLease.id, permittedLease.fence, permitted.rackSlot, refusedTierPolicy)

	// The lease inherits the job's policy at acquisition
	// (migrations/00009_reattach_auth.sql), and it is the LEASE row the ladder
	// reads. Checking it here means a later refusal cannot be attributed to a
	// policy nobody is holding.
	requireLeasePolicy(t, f, refusedLease.id, forbiddingPolicy)
	requireLeasePolicy(t, f, permittedLease.id, refusedTierPolicy)

	// -----------------------------------------------------------------
	// The same fault, on both phones, at the same rung.
	// -----------------------------------------------------------------

	// farm_recovery_refusals_total is a fleet-wide {tier,kind} counter with no
	// position on it, so its ABSOLUTE value proves nothing about one device:
	// any refusal anywhere on the farm lands on the same series. What can be
	// claimed honestly is the increase across a window whose only new fault is
	// the one this scenario just introduced, so the reading is taken now,
	// before anything is broken.
	refusalsBefore := refusalCount(t, f)

	breakUnderLease(t, f, refused)
	breakUnderLease(t, f, permitted)

	// -----------------------------------------------------------------
	// 1 and 2: the ladder refused, and the refusal did not spend the rung.
	// -----------------------------------------------------------------

	var refusal attemptRow
	f.Eventually(t, 90*time.Second,
		"the ladder to refuse the "+refusedTierName+" rung on "+refused.rackSlot,
		func() error {
			rows := readRecoveryAttempts(t, f, refused.deviceID)
			for _, a := range rows {
				if a.outcome == "refused" && strings.Contains(a.refusal, "disruption_policy") {
					refusal = a
					return nil
				}
			}
			return fmt.Errorf("no blast-radius refusal for %s yet:\n%s",
				refused.rackSlot, formatRecoveryAttempts(rows))
		})

	// The refusal has to be readable by a human who was not here when it
	// happened: which rung, what it needed, and WHOSE lease would not give it.
	// "tier 5 refused" with no subject is the gap the refusal column exists to
	// close.
	for _, want := range []string{
		refusedTierName, refusedTierRadius, refusedTierPolicy,
		forbiddingPolicy, refusedLease.id, refusedJob,
	} {
		if !strings.Contains(refusal.refusal, want) {
			t.Errorf("the refusal does not name %q, so an operator reading it cannot tell "+
				"what was not permitted:\n  %s", want, refusal.refusal)
		}
	}
	if refusal.tier != refusedTier {
		t.Errorf("the refusal was recorded at tier %d, want %d (%s)",
			refusal.tier, refusedTier, refusedTierName)
	}

	// from_tier is written by exactly one function — the ladder's begin, which
	// runs only AFTER the blast-radius check has cleared the rung. Its absence
	// is therefore the record that nothing was ever opened against this
	// device: the refusal came first, and it came before anything changed
	// state.
	if strings.Contains(refusal.detail, `"from_tier"`) {
		t.Errorf("the refusal row carries from_tier, which only the ladder's begin writes; "+
			"the rung was opened and then refused rather than refused before it opened:\n  %s",
			refusal.detail)
	}
	for _, want := range []string{
		`"blast_radius": "` + refusedTierRadius + `"`,
		`"requires_policy": "` + refusedTierPolicy + `"`,
	} {
		if !strings.Contains(refusal.detail, want) {
			t.Errorf("the refusal detail does not carry %s:\n  %s", want, refusal.detail)
		}
	}

	if tier := readLadderTier(t, f, refused.deviceID); tier != refusedTier {
		t.Fatalf("farm.device_runtime.ladder_tier for %s is %d, want %d. The column holds the "+
			"lowest rung NOT YET SPENT, so a refusal must leave it alone: the ladder has "+
			"climbed on the strength of a rung that never ran, and the next rung it reaches "+
			"is more disruptive than the one this lease already forbade.",
			refused.rackSlot, tier, refusedTier)
	}

	// -----------------------------------------------------------------
	// 3: THE LEASE DID NOT MOVE.
	// -----------------------------------------------------------------

	before := len(readRecoveryAttempts(t, f, refused.deviceID))
	// Longer than two of the ladder's fifteen-second cycles, so the window
	// contains sweeps rather than merely spanning time. What must be shown is
	// not that the lease survived an instant but that it survived a farm whose
	// recovery loop was actively deciding, over and over, that it may not
	// touch this device.
	//
	// Every witness of "the rung was not spent" is re-read inside the window
	// too, for the reason Consistently exists at all: the ladder gets another
	// go every fifteen seconds, so a single reading taken at the top would
	// pass on a farm that opens the rung twenty seconds later.
	f.Consistently(t, 35*time.Second,
		"the lease of job "+refusedJob+" exactly where the scheduler put it",
		func() error {
			now, err := readLease(t, f, refusedJob)
			if err != nil {
				return err
			}
			switch {
			case now.id != refusedLease.id:
				return fmt.Errorf("the job is on lease %s, not the %s it was placed on; "+
					"it was given a second device", now.id, refusedLease.id)
			case now.fence != refusedLease.fence:
				return fmt.Errorf("lease %s is at fence %d, was %d; the lease was handed over",
					now.id, now.fence, refusedLease.fence)
			case now.state != "held":
				return fmt.Errorf("lease %s is %q, want \"held\" (release_reason %q, ended by %q)",
					now.id, now.state, now.reason, now.endedBy)
			case now.reason != "":
				return fmt.Errorf("lease %s carries release_reason %q, which farm.lease_ended_by "+
					"classifies as %q", now.id, now.reason, now.endedBy)
			case now.holder != refusedLease.holder:
				return fmt.Errorf("lease %s is held by %q, was %q", now.id, now.holder, refusedLease.holder)
			case now.currentOnDevice != refusedLease.id:
				return fmt.Errorf("farm.devices.current_lease_id for %s is %q, not lease %s",
					refused.rackSlot, now.currentOnDevice, refusedLease.id)
			}
			if tier := readLadderTier(t, f, refused.deviceID); tier != refusedTier {
				return fmt.Errorf("ladder_tier for %s moved to %d; the ladder spent a rung "+
					"it had refused", refused.rackSlot, tier)
			}
			// begin writes ladder_tier and health='recovering' in ONE
			// statement, so an induced health value is a second, independent
			// witness that the rung was opened after all.
			if health := readHealth(t, f, refused.deviceID); health != "offline" {
				return fmt.Errorf("farm.device_runtime.health for %s is %q, want \"offline\"; "+
					"the ladder induced a state on a device whose rung it was not permitted "+
					"to run", refused.rackSlot, health)
			}
			return nil
		})

	// The window has to have been a busy one, or "the lease did not move"
	// degrades into "nothing happened". Every cycle in it re-derived the same
	// refusal, which is the behaviour a farm full of strict tenants actually
	// sees.
	attempts := readRecoveryAttempts(t, f, refused.deviceID)
	if len(attempts) <= before {
		t.Errorf("farm.recovery_attempts for %s went from %d to %d rows across a 35s window: "+
			"the ladder stopped considering this device, so the window proves nothing about "+
			"a lease surviving a ladder that was running",
			refused.rackSlot, before, len(attempts))
	}
	for _, a := range attempts {
		if a.outcome != "refused" {
			t.Errorf("attempt %d on %s ended %q; every attempt on a device whose lease forbids "+
				"this rung must be a refusal:\n%s", a.id, refused.rackSlot, a.outcome, a.String())
		}
	}

	// -----------------------------------------------------------------
	// 4: the metric moved, with the physical position attached.
	// -----------------------------------------------------------------

	scrape := f.Metrics(t, "recovery")

	// The alert's own series. tier is the tier TABLE's name here, because the
	// operator reading "which rung is my fleet refusing" wants the rung they
	// can edit in farm.recovery_tiers. It is asserted as a RISE rather than a
	// level: the series carries no position, so its absolute value is a
	// statement about the farm and only the increase is a statement about this
	// device's lease.
	after := refusalCount(t, f)
	t.Logf("farm_recovery_refusals_total{tier=%q,kind=%q}: %v -> %v",
		refusedTierName, refusedPolicyOutcome, refusalsBefore, after)
	if after <= refusalsBefore {
		t.Errorf("farm_recovery_refusals_total{tier=%q,kind=%q} is %v, was %v before this "+
			"scenario broke anything. DeviceFarmerRecoveryRefusedPolicy reads this series, so "+
			"a refusal that is only in the database is a refusal no operator is ever told "+
			"about.\n%s", refusedTierName, refusedPolicyOutcome, after, refusalsBefore,
			grepLines(scrape, "farm_recovery_refusals_total"))
	}

	// And the position-labelled counter the runbook groups by hub. Naming the
	// host, hub and rack slot is the difference between "something was refused"
	// and "walk to R1-U04-H2-P1" — and, unlike the counter above, it pins the
	// refusal to THIS device.
	v, ok := promValue(t, scrape, "farm_recovery_attempts_total", map[string]string{
		"tier": obsTierLabel, "outcome": refusedPolicyOutcome,
		"host": refused.hostID, "hub": refused.hubPath, "rack_slot": refused.rackSlot,
	})
	t.Logf("farm_recovery_attempts_total{tier=%q,outcome=%q,host=%q,hub=%q,rack_slot=%q}: %v (present=%v)",
		obsTierLabel, refusedPolicyOutcome, refused.hostID, refused.hubPath, refused.rackSlot, v, ok)
	if !ok || v < 1 {
		t.Errorf("farm_recovery_attempts_total{tier=%q,outcome=%q,host=%q,hub=%q,rack_slot=%q} "+
			"is %v (present=%v), want at least 1. The tier label is obs's coarser vocabulary, "+
			"not farm.recovery_tiers.name: obsTier in internal/recovery/ladder.go folds %s "+
			"onto it, so a wrong label here is likelier a change to that folding than a "+
			"counter that never moved. Every series of this metric in the scrape:\n%s",
			obsTierLabel, refusedPolicyOutcome, refused.hostID, refused.hubPath,
			refused.rackSlot, v, ok, refusedTierName,
			grepLines(scrape, "farm_recovery_attempts_total{"))
	}

	// -----------------------------------------------------------------
	// THE CONTRAST. Same fault, same rung, a job that permits it.
	// -----------------------------------------------------------------

	var performed attemptRow
	f.Eventually(t, 90*time.Second,
		"the ladder to open the "+refusedTierName+" rung on "+permitted.rackSlot,
		func() error {
			rows := readRecoveryAttempts(t, f, permitted.deviceID)
			for _, a := range rows {
				// from_tier is written only by begin, and begin runs only once
				// the blast-radius check has returned no refusal. This key is
				// therefore the record that the check PERMITTED this rung —
				// the exact statement the refused half is the negation of.
				//
				// finished matters as much: begin COMMITS the row before the
				// actuator runs, so a poll landing in that gap would hand the
				// outcome assertion below an empty string and blame the
				// actuator for a row that had simply not closed yet.
				if a.tier == refusedTier && a.finished && strings.Contains(a.detail, `"from_tier"`) {
					performed = a
					return nil
				}
			}
			return fmt.Errorf("no finished attempt at tier %d for %s yet:\n%s",
				refusedTier, permitted.rackSlot, formatRecoveryAttempts(rows))
		})

	for _, a := range readRecoveryAttempts(t, f, permitted.deviceID) {
		if strings.Contains(a.refusal, "disruption_policy") {
			t.Errorf("attempt %d on %s was refused over a disruption policy that permits this "+
				"rung; the blast-radius check is refusing something it should clear:\n%s",
				a.id, permitted.rackSlot, a.String())
		}
	}

	// The rung ran against the real fake hardware and the handset did not come
	// back — the ADB server refuses a transport switch to an offline device
	// exactly as a real one does. That is a verdict ABOUT THE HARDWARE, which
	// is the only kind that spends a rung.
	if performed.outcome != "failed" {
		t.Errorf("the permitted rung on %s ended %q, want \"failed\": the ladder was cleared to "+
			"act and the device is offline, so the actuator should have tried and found it "+
			"still broken\n%s", permitted.rackSlot, performed.outcome, performed.String())
	}
	if tier := readLadderTier(t, f, permitted.deviceID); tier <= refusedTier {
		t.Errorf("farm.device_runtime.ladder_tier for %s is %d; a rung that RAN must be spent, "+
			"so it should be above %d. Without this the contrast is empty: both halves would "+
			"leave the ladder where it was and 'refused' would mean nothing.",
			permitted.rackSlot, tier, refusedTier)
	}

	// The permitted half took a device somebody was holding and rebooted it,
	// which is what its policy asked for — and it still did not touch the
	// lease. Recovery acts FOR the holder; it never acts INSTEAD of it.
	if now, err := readLease(t, f, permittedJob); err != nil {
		t.Errorf("reading the lease of the permitted job: %v", err)
	} else if now.id != permittedLease.id || now.fence != permittedLease.fence || now.state != "held" {
		t.Errorf("lease %s (fence %d) of the permitted job is now %s (fence %d, state %q, "+
			"reason %q): performing a rung ended a lease",
			permittedLease.id, permittedLease.fence, now.id, now.fence, now.state, now.reason)
	}

	t.Logf("refused: %s stayed at rung %d under a %s lease; permitted: %s spent rung %d under a %s lease",
		refused.rackSlot, refusedTier, forbiddingPolicy,
		permitted.rackSlot, refusedTier, refusedTierPolicy)
}

// ---------------------------------------------------------------------------
// The fixture this scenario needs
// ---------------------------------------------------------------------------

// position is one physical place in the rack, and the device currently in it.
// Everything below addresses hardware through this: never a serial, because
// the seeded rack holds two handsets that share one.
type position struct {
	deviceID string
	hostID   string
	hubPath  string
	devpath  string
	rackSlot string
}

// requireRungShape fails loudly if farm.recovery_tiers no longer describes the
// rung this scenario reasons about.
//
// The tier table is deliberately operator-editable, so the row can change
// without a single Go file changing with it. Reading it here means such an edit
// renames the failure — "tier 5 is no longer device_reboot" — instead of
// quietly turning every assertion below into a test of something else.
func requireRungShape(t *testing.T, f *farm) {
	t.Helper()
	var name, radius, policy string
	var enabled bool
	if err := f.DB().QueryRow(t.Context(), `
SELECT name, blast_radius, requires_policy, enabled
  FROM farm.recovery_tiers WHERE tier = $1::int`, refusedTier).
		Scan(&name, &radius, &policy, &enabled); err != nil {
		t.Fatalf("reading farm.recovery_tiers row %d: %v", refusedTier, err)
	}
	if name != refusedTierName || radius != refusedTierRadius ||
		policy != refusedTierPolicy || !enabled {
		t.Fatalf("farm.recovery_tiers row %d is (%s, blast_radius=%s, requires_policy=%s, "+
			"enabled=%v); this scenario needs (%s, %s, %s, true) — a rung above no_disruption "+
			"whose radius is a single device, so a refusal can only be about the policy",
			refusedTier, name, radius, policy, enabled,
			refusedTierName, refusedTierRadius, refusedTierPolicy)
	}
}

// pickIsolatedPositions chooses n healthy devices, one per HOST, each on a hub
// whose every device is currently healthy.
//
// Every part of that is about the ladder's own correlation and blast radius
// rather than about tidiness.
//
//   - A hub on which a strict majority of devices go bad inside one window is
//     answered with a single hub-scoped quarantine, and a quarantined device is
//     not a recovery candidate at all. Two broken devices on one four-port hub,
//     or one broken device beside the pair the seeder plants as degraded, would
//     produce a farm in which the ladder correctly never considers this
//     scenario's devices and every assertion fails for a reason that has
//     nothing to do with blast radius.
//   - One per host, rather than merely one per hub, because the top of the
//     ladder has host-radius rungs (adb_restart, host_drain). Two victims on
//     one host would put each inside the other's eventual blast radius, and the
//     two halves of the contrast would stop being independent.
//
// It reads the fleet back out of the database rather than deriving positions
// from the seeder's arithmetic, for the same reason hardware_test.go does:
// agreement with the seeder's INTENT is not agreement with its rows.
func pickIsolatedPositions(t *testing.T, f *farm, n int) []position {
	t.Helper()
	const q = `
WITH hub AS (
  SELECT s.hub_id,
         count(*) FILTER (WHERE r.health <> 'healthy' OR r.adb_state <> 'device') AS suspect
    FROM farm.slots s
    JOIN farm.devices d        ON d.current_slot_id = s.id
    JOIN farm.device_runtime r ON r.device_id = d.id
   GROUP BY s.hub_id
)
SELECT DISTINCT ON (s.host_id)
       d.id::text, s.host_id, COALESCE(hb.usb_path, ''), s.adb_devpath, COALESCE(s.rack_slot, '')
  FROM farm.devices d
  JOIN farm.slots s          ON s.id = d.current_slot_id
  JOIN farm.hubs hb          ON hb.id = s.hub_id
  JOIN farm.device_runtime r ON r.device_id = d.id
  JOIN hub                   ON hub.hub_id = s.hub_id
 WHERE r.health = 'healthy'
   AND r.adb_state = 'device'
   AND d.admin_state = 'enabled'
   AND s.state = 'active'
   AND hub.suspect = 0
 ORDER BY s.host_id, s.hub_id, s.usb_path`

	rows, err := f.DB().Query(t.Context(), q)
	if err != nil {
		t.Fatalf("choosing devices on undisturbed hubs: %v", err)
	}
	defer rows.Close()

	var out []position
	for rows.Next() {
		var p position
		if err := rows.Scan(&p.deviceID, &p.hostID, &p.hubPath, &p.devpath, &p.rackSlot); err != nil {
			t.Fatalf("scanning the chosen devices: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("choosing devices on undisturbed hubs: %v", err)
	}
	if len(out) < n {
		t.Fatalf("this farm offers %d host(s) with a healthy device on a wholly healthy hub "+
			"and this scenario needs %d; the seed's %d host(s) and %d hub(s) in total no longer "+
			"leave one such hub per host clear of the faulty hub and the degraded pair the "+
			"seeder plants", len(out), n, len(f.Seed().Hosts), f.Seed().Hubs)
	}
	return out[:n]
}

// submitHoldingJob files a job pinned to one device, carrying one disruption
// policy, over the real HTTP API as the tenant credential.
//
// The step is a sleep far longer than this scenario, and this farm runs no
// jobrunner, so the work never finishes and the lease stays live for the whole
// run. That is deliberate and it is the only honest way to hold one: the
// harness offers no helper that ends a lease, and it offers none that keeps one
// alive either — a lease lasts until the job says so, a deadline elapses, or a
// human takes it back.
func submitHoldingJob(t *testing.T, f *farm, deviceID, policy string) string {
	t.Helper()
	res := f.post(t, "tenant", "/api/v1/jobs", map[string]any{
		"pool":              f.Seed().Pool,
		"queue":             f.Seed().Queue,
		"tenant":            f.Seed().Tenant,
		"pin_device":        deviceID,
		"disruption_policy": policy,
		"spec": map[string]any{
			"version": 1,
			"steps": []any{map[string]any{
				"id": "hold", "kind": "sleep", "timeout": "30m",
				"sleep": map[string]any{"duration": "29m"},
			}},
		},
	}).mustStatus(t, http.StatusCreated)

	id := res.str(t, "job", "id")
	t.Logf("submitted job %s pinned to device %s with disruption_policy=%s", id, deviceID, policy)
	return id
}

// breakUnderLease makes one leased device genuinely broken, in both worlds, at
// the rung this scenario is about.
//
// # Why the hardware AND the database
//
// The fake ADB server really stops answering for this position, so the
// permitted half's rung meets a handset that is actually offline and reports a
// verdict it actually earned. Without that the contrast would be a fiction:
// the ladder would be cleared to act and would then act on a working phone.
//
// # Why farm.device_runtime is written directly
//
// The health plane is not what this scenario is about, and it is not free to
// wait for: the watchdog debounces, damps flaps through a token bucket, and
// promotes and demotes on hysteresis deliberately slower than the ladder's
// cooldowns, and the ladder's own candidate query then wants the value settled
// past a debounce window. Driving all of that would spend minutes proving
// something internal/watchdog already proves, and would make the moment the
// ladder first sees the device depend on timing this scenario does not control.
//
// So the row is shaped to exactly what the ladder's candidate query selects —
// an unhealthy device whose transport is gone, settled well past the debounce,
// under no suppression, standing at the rung under test — and the ladder's own
// query is left to be the thing that picks it. Every timestamp is the server's
// now(); nothing here is a clock this test made up.
func breakUnderLease(t *testing.T, f *farm, p position) {
	t.Helper()

	if !f.ADB(t, p.hostID).SetState(p.devpath, fakeadb.StateOffline) {
		t.Fatalf("the fake hardware for host %s has no device at %s", p.hostID, p.devpath)
	}

	// health_since is put five minutes back so the device is SETTLED unhealthy
	// on the ladder's very next cycle: DefaultDebounce is 45 seconds and there
	// is no reason to spend it. suppress_until is cleared because a device
	// under an induced-reset window is deliberately invisible to the ladder.
	tag, err := f.DB().Exec(t.Context(), `
UPDATE farm.device_runtime
   SET health         = 'offline',
       health_since   = now() - interval '5 minutes',
       adb_state      = 'offline',
       suppress_until = NULL,
       ladder_tier    = $2::int,
       updated_at     = now()
 WHERE device_id = $1::uuid`, p.deviceID, refusedTier)
	if err != nil {
		t.Fatalf("breaking %s in farm.device_runtime: %v", p.rackSlot, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("breaking %s updated %d farm.device_runtime rows, want 1",
			p.rackSlot, tag.RowsAffected())
	}
	t.Logf("broke %s at %s: offline on the bus and standing at rung %d",
		p.rackSlot, p.devpath, refusedTier)
}

// ---------------------------------------------------------------------------
// Readers
// ---------------------------------------------------------------------------

// leaseSnapshot is a lease as this scenario has to compare it: the row
// smoke_test.go's readLeases already quotes, plus the two facts a "did it
// move" assertion needs and that reader does not carry — the fence, and the
// device's own pointer back at the lease. A lease row that still reads 'held'
// while farm.devices.current_lease_id has moved on is a lease that was taken
// away by something that forgot to close the row.
type leaseSnapshot struct {
	leaseRow
	fence           int64
	currentOnDevice string
}

// readLease returns the ONE lease of a job, and reports a second one as the
// error it is. Two lease rows for one job means the job was placed twice, which
// is the shape every failure this package guards against actually takes.
//
// The row itself comes from readLeases, which package doc.go names as the
// reader to reach for: it is where farm.lease_ended_by is applied, and a second
// copy of that query in this file would be a second place for the release-reason
// vocabulary to drift.
func readLease(t *testing.T, f *farm, jobID string) (leaseSnapshot, error) {
	t.Helper()
	rows := readLeases(t, f, jobID)
	switch len(rows) {
	case 1:
	case 0:
		return leaseSnapshot{}, fmt.Errorf("job %s holds no lease", jobID)
	default:
		return leaseSnapshot{}, fmt.Errorf("job %s has %d lease rows, want 1; it was handed a "+
			"device more than once:\n%s", jobID, len(rows), formatLeases(rows))
	}

	out := leaseSnapshot{leaseRow: rows[0]}
	if err := f.DB().QueryRow(t.Context(), `
SELECT l.fence, COALESCE(d.current_lease_id::text, '')
  FROM farm.leases l
  JOIN farm.devices d ON d.id = l.device_id
 WHERE l.id = $1::uuid`, rows[0].id).Scan(&out.fence, &out.currentOnDevice); err != nil {
		return leaseSnapshot{}, fmt.Errorf("reading the fence of lease %s: %w", rows[0].id, err)
	}
	return out, nil
}

// awaitLease waits for the scheduler to place a job and returns the lease it
// was placed on.
func awaitLease(t *testing.T, f *farm, jobID string) leaseSnapshot {
	t.Helper()
	var out leaseSnapshot
	f.Eventually(t, 2*time.Minute, "the scheduler to place job "+jobID+" on the device it pinned",
		func() error {
			l, err := readLease(t, f, jobID)
			if err != nil {
				return err
			}
			if l.state != "held" {
				return fmt.Errorf("lease %s is %q, want \"held\"", l.id, l.state)
			}
			out = l
			return nil
		})
	return out
}

// requireLeasePolicy asserts that the lease inherited the job's policy. The
// ladder reads farm.leases.disruption_policy and never farm.jobs, so this is
// the value under test; a lease that inherited the wrong one would make the
// refusal below true for the wrong reason.
func requireLeasePolicy(t *testing.T, f *farm, leaseID, want string) {
	t.Helper()
	var got string
	if err := f.DB().QueryRow(t.Context(),
		`SELECT disruption_policy FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&got); err != nil {
		t.Fatalf("reading the disruption policy of lease %s: %v", leaseID, err)
	}
	if got != want {
		t.Fatalf("lease %s carries disruption_policy %q, want %q; the lease did not inherit "+
			"the policy its job was filed with", leaseID, got, want)
	}
}

// attemptRow is one farm.recovery_attempts row, kept whole so a failure can
// quote it. detail is the raw jsonb text: this scenario asks it whether
// particular KEYS are present, and decoding into a map would lose the
// difference between "absent" and "present and empty".
type attemptRow struct {
	id       int64
	tier     int
	outcome  string
	refusal  string
	detail   string
	finished bool
}

func (a attemptRow) String() string {
	return fmt.Sprintf("  attempt %d tier=%d outcome=%q finished=%v\n    refusal: %s\n    detail:  %s",
		a.id, a.tier, a.outcome, a.finished, orNone(a.refusal), a.detail)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func readRecoveryAttempts(t *testing.T, f *farm, deviceID string) []attemptRow {
	t.Helper()
	rows, err := f.DB().Query(t.Context(), `
SELECT id, tier, COALESCE(outcome, ''), COALESCE(refusal, ''),
       detail::text, finished_at IS NOT NULL
  FROM farm.recovery_attempts
 WHERE device_id = $1::uuid
 ORDER BY id`, deviceID)
	if err != nil {
		t.Fatalf("reading farm.recovery_attempts: %v", err)
	}
	defer rows.Close()

	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.id, &a.tier, &a.outcome, &a.refusal, &a.detail, &a.finished); err != nil {
			t.Fatalf("scanning farm.recovery_attempts: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading farm.recovery_attempts: %v", err)
	}
	return out
}

func formatRecoveryAttempts(rows []attemptRow) string {
	if len(rows) == 0 {
		return "  (no attempts recorded for this device)"
	}
	var b strings.Builder
	for _, a := range rows {
		b.WriteString(a.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// refusalCount reads the counter DeviceFarmerRecoveryRefusedPolicy alerts on,
// for the rung this scenario is about. A series that is not there yet reads
// zero, which is what a counter that has never been incremented means.
func refusalCount(t *testing.T, f *farm) float64 {
	t.Helper()
	v, _ := promValue(t, f.Metrics(t, "recovery"), "farm_recovery_refusals_total",
		map[string]string{"tier": refusedTierName, "kind": refusedPolicyOutcome})
	return v
}

func readLadderTier(t *testing.T, f *farm, deviceID string) int {
	t.Helper()
	var tier int
	if err := f.DB().QueryRow(t.Context(),
		`SELECT ladder_tier FROM farm.device_runtime WHERE device_id = $1::uuid`,
		deviceID).Scan(&tier); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("device %s has no farm.device_runtime row", deviceID)
		}
		t.Fatalf("reading farm.device_runtime.ladder_tier: %v", err)
	}
	return tier
}

func readHealth(t *testing.T, f *farm, deviceID string) string {
	t.Helper()
	var health string
	if err := f.DB().QueryRow(t.Context(),
		`SELECT health FROM farm.device_runtime WHERE device_id = $1::uuid`,
		deviceID).Scan(&health); err != nil {
		t.Fatalf("reading farm.device_runtime.health: %v", err)
	}
	return health
}

// ---------------------------------------------------------------------------
// Reading a scrape
// ---------------------------------------------------------------------------

// promValue finds one series in an exposition text and returns its value.
// want is matched as a SUBSET of the series' labels, so a caller names the
// labels its assertion is about and ignores the rest.
//
// The parsing is done here rather than with prometheus/common's expfmt because
// this repository's dependency budget is pgx, prometheus/client_golang and
// goose, and expfmt would be a fourth direct dependency bought for four
// assertions. The format it has to read is one line per sample.
func promValue(t *testing.T, scrape, name string, want map[string]string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(scrape, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		block := ""
		switch {
		case strings.HasPrefix(rest, "{"):
			end, ok := endOfLabelBlock(rest)
			if !ok {
				t.Fatalf("the label block of a %s series is unterminated: %s", name, line)
			}
			block, rest = rest[1:end], rest[end+1:]
		case strings.HasPrefix(rest, " "):
		default:
			// A different metric whose name merely starts with this one, e.g.
			// a _bucket or _sum beside a histogram.
			continue
		}

		have, ok := parsePromLabels(block)
		if !ok {
			// Never fall through to "no such series": a block this cannot read
			// is a broken parser, and reporting it as a counter that never
			// moved would send the reader after the farm instead.
			t.Fatalf("the label block of a %s series could not be read: %s", name, line)
		}
		if !hasEvery(have, want) {
			continue
		}

		// The value is the FIRST field after the labels, not the last field of
		// the line: the exposition format permits a trailing millisecond
		// timestamp, and taking the last field would silently read that
		// timestamp as the counter — a number always large enough to satisfy
		// any "did it move" assertion.
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			t.Fatalf("a %s series carries no value: %s", name, line)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatalf("the %s series has an unreadable value %q: %v", name, fields[0], err)
		}
		return v, true
	}
	return 0, false
}

// endOfLabelBlock returns the index of the '}' that closes the block s opens
// with, skipping any brace inside a quoted label value.
func endOfLabelBlock(s string) (int, bool) {
	inValue := false
	for i := 1; i < len(s); i++ {
		switch {
		case inValue && s[i] == '\\':
			i++
		case s[i] == '"':
			inValue = !inValue
		case !inValue && s[i] == '}':
			return i, true
		}
	}
	return 0, false
}

// hasEvery reports whether have carries every label in want, with the same
// value. Labels outside want are ignored, so a caller names the ones its
// assertion is about.
func hasEvery(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// parsePromLabels reads a rendered label block, e.g. `a="b",c="d\"e"`, and
// reports whether the whole block parsed. It honours the backslash escapes the
// exposition format defines, because a label value containing a quote would
// otherwise end the value early and silently change which series a caller
// matched.
func parsePromLabels(block string) (map[string]string, bool) {
	out := make(map[string]string)
	for i := 0; i < len(block); {
		eq := strings.IndexByte(block[i:], '=')
		if eq < 0 {
			return out, false
		}
		key := strings.TrimSpace(block[i : i+eq])
		j := i + eq + 1
		if j >= len(block) || block[j] != '"' {
			return out, false
		}
		j++

		var val strings.Builder
		for j < len(block) && block[j] != '"' {
			if block[j] == '\\' && j+1 < len(block) {
				j++
				switch block[j] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(block[j])
				}
				j++
				continue
			}
			val.WriteByte(block[j])
			j++
		}
		if j >= len(block) {
			return out, false
		}
		out[key] = val.String()

		j++ // the closing quote
		if j < len(block) && block[j] == ',' {
			j++
		}
		i = j
	}
	return out, true
}

// grepLines quotes the part of a scrape a failed metric assertion is about.
// The whole exposition is thousands of lines and none of the other ones help.
func grepLines(scrape, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, prefix) {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return "  (no series with that name in the scrape at all)"
	}
	return b.String()
}
