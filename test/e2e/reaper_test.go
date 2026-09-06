package e2e

// The reaper, and the three ways it must restrain itself.
//
// The reaper is THE ONLY AUTOMATIC RELEASE PATH IN THE SYSTEM. A lease
// otherwise ends when the job says so, when a deadline the user wrote down
// elapses, or when a human takes it back — so every ending this loop does not
// make is somebody's multi-hour run that survived. This file is therefore not
// about what the reaper does. It is about what it refuses to do, proved
// against the shipped `farmd reaper` process:
//
//  1. an overdue UNPROTECTED lease IS reclaimed. That is the positive control,
//     and it is not decoration. "The lease survived" and "nothing here was
//     reclaimable" are the same sentence read from outside, so a scenario
//     without a casualty in the same sweep proves neither of the two below;
//  2. an overdue PROTECTED lease is HELD rather than reclaimed — by a reaper
//     that is enabled, out of quiesce, armed, and demonstrably sweeping,
//     because it took the lease planted beside it. It is held indefinitely for
//     a human, and the gauge the page reads rises;
//  3. a watched component that has NEVER written a heartbeat makes the arm
//     REFUSE, and a lease that satisfies every other predicate of
//     farm.lease_reclaim is left alone until that component beats.
//
// # The fixture is SQL, and it has to be
//
// config.MinLeaseTTL is ten minutes and config.MinLeaseGrace five, so waiting
// for a lease to become reclaimable by the clock costs a quarter of an hour of
// test — and a quarter-hour test is a test nobody runs. The guard trigger
// farm.trg_leases_guard is BEFORE UPDATE only (migrations/00002_lease.sql:150):
// it forbids MOVING a deadline backwards and says nothing about a row that
// ARRIVES with one already in the past. So the leases below are inserted two
// hours silent, exactly as test/assertions_v12.sql:74 builds its own.
//
// That is the only shortcut taken. The fixture is a row; everything that acts
// on it afterwards is the real process, in its own database, deciding for
// itself — and the assertions read the database for the endings and the
// process's own /metrics for the gauges, because a property proved only in SQL
// leaves the alerting rule that reads it unproven.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// reaperHold is how long a "this lease did NOT end" claim is held open. At the
// one-second cycle these scenarios run, it is fifteen sweeps that could have
// taken the lease and did not — long enough to mean something, short enough
// that the whole file stays inside a couple of minutes.
const reaperHold = 15 * time.Second

// TestReaperReclaimsTheOverdueAndHoldsTheProtected plants two leases that are
// identical in every respect the reaper is allowed to see — same silence, same
// deadlines, same absent witness — and different in the one it must obey.
//
// LEASE-10: a protected lease is never auto-reclaimed; it is held and a human
// is paged. migrations/00012_reaper_arm_unbeaten.sql:202 is the whole
// mechanism, one line inside the candidate set: `AND l.protected = false --
// hold and page instead`.
//
// Falsify: delete that line. The unprotected lease is still reclaimed, so the
// first subtest passes; the second fails on its first check, because the
// protected lease is gone in the same sweep.
func TestReaperReclaimsTheOverdueAndHoldsTheProtected(t *testing.T) {
	// Parallel with the refusal scenario below. They are two farms with two
	// databases, which is what makes it safe: the reaper is a farm-wide sweep,
	// and a scenario sharing a database with another would have its fixtures
	// swept by somebody else's reaper.
	t.Parallel()

	f := newFarm(t, farmOpts{
		// The api is not scenery. It is on the lease renewal path, so a farm
		// without it would watch a one-name component list, and `ctl reaper`
		// is what an operator actually reads when they want to know whether
		// the farm is about to take a device back.
		Roles: []string{"api", "reaper"},
		Env: map[string]string{
			// A one-second cycle. Every sweep this scenario waits for is a
			// full cycle away at the shipped ten seconds, and it waits for
			// several.
			config.EnvReaperInterval: "1s",
		},
	})
	ctx := t.Context()

	// An alerting rule reads a TIME SERIES. `farm_lease_suspect{protected="true"} > 0`
	// over a series that does not exist yields an empty vector — which is not
	// zero, it is no data, and no data fires nothing. obs.zeroFill creates the
	// series at zero for exactly this reason, and it is asserted HERE, before
	// the fixture, because a gauge that springs into existence at the moment
	// it first goes bad is a gauge whose alert was armed by the incident it
	// was supposed to warn about.
	if sum, samples := reaperMetric(t, f.Metrics(t, "reaper"), "farm_lease_suspect", `protected="true"`); samples == 0 || sum != 0 {
		t.Fatalf(`farm_lease_suspect{protected="true"} sums to %v across %d sample(s) on a farm `+
			`with no leases at all; want one pre-seeded series at 0. `+
			`DeviceFarmerProtectedLeaseSuspect reads this series, and a rule over an absent `+
			`series returns no data and never fires.`, sum, samples)
	}

	// The reaper arms before its first sweep, always, and a successful arm
	// sets quiesce_until to now() plus the longest live TTL — fifteen minutes
	// on a farm with no leases yet. Wait for it rather than race it: a fixture
	// that opened the gate BEFORE the arm would have the arm shut it again for
	// a quarter of an hour, and the scenario would read that as the guard
	// under test.
	f.Eventually(t, 90*time.Second, "the reaper to arm for the first time", func() error {
		var quiesceIn float64
		var refusal string
		if err := f.DB().QueryRow(ctx, `
SELECT extract(epoch FROM quiesce_until - now())::float8, COALESCE(last_refusal, '')
  FROM farm.reaper_state WHERE singleton`).Scan(&quiesceIn, &refusal); err != nil {
			return err
		}
		if refusal != "" {
			// Ordinary at startup and it clears itself: the reaper beats on
			// its first cycle and the api on its own schedule, so whichever
			// arm runs first can meet a component that has not beaten yet.
			return fmt.Errorf("the arm is refusing: %s", refusal)
		}
		if quiesceIn <= 0 {
			return fmt.Errorf("quiesce_until is still %.0fs in the past, so no arm has stamped it", -quiesceIn)
		}
		return nil
	})

	// Both planted 'held' with a deadline already past, so the reaper's own
	// suspect sweep does the held -> suspect promotion. That is the path a
	// real silent holder takes, and it is what produces the WARN line and the
	// suspect_marks_total{protection="protected"} count asserted below.
	planted := reaperStage(t, f,
		reaperFixtureSpec{state: "held", protected: false},
		reaperFixtureSpec{state: "held", protected: true},
	)
	doomed, spared := planted[0], planted[1]

	t.Run("an overdue unprotected lease is reclaimed", func(t *testing.T) {
		f.Eventually(t, 60*time.Second, "the reaper to reclaim the overdue unprotected lease", func() error {
			e, err := reaperEnding(ctx, f, doomed.leaseID)
			if err != nil {
				return err
			}
			if !e.released {
				// Every gate is server-side, so a sweep that did not happen
				// has to be explained by the state of one of them. Quoting
				// them all is the difference between a fix and a re-run.
				return fmt.Errorf("lease %s is %s\n%s", doomed.leaseID, e, reaperGates(t, f))
			}
			return nil
		})

		e, err := reaperEnding(ctx, f, doomed.leaseID)
		if err != nil {
			t.Fatalf("reading lease %s back: %v", doomed.leaseID, err)
		}
		// Reported independently rather than as a switch: an ending that went
		// through some OTHER path differs in all three at once, and the
		// attribution is the most diagnostic of them — "released /
		// operator_revoked / operator" says who ended it, where "not expired"
		// says only that somebody did.
		if e.state != "expired" {
			t.Errorf("lease %s is %q, want \"expired\": %s", doomed.leaseID, e.state, e)
		}
		if e.reason != "holder_expired" {
			t.Errorf("lease %s ended with release_reason %q, want \"holder_expired\". The reaper "+
				"has exactly one reason to give, so anything else here is some other clock "+
				"ending a lease the reaper was about to end.", doomed.leaseID, e.reason)
		}
		if e.endedBy != "reaper" {
			t.Errorf("farm.lease_ended_by(%q) is %q, want \"reaper\"", e.reason, e.endedBy)
		}

		// A reclaim that did not fence is a reclaim that handed the device to
		// the next job while the previous holder could still write to it —
		// which is the harm the whole ending exists to avoid, arriving through
		// the ending itself.
		var floor int64
		if err := f.DB().QueryRow(ctx,
			`SELECT fence_floor FROM farm.devices WHERE id = $1::uuid`, doomed.deviceID).Scan(&floor); err != nil {
			t.Fatalf("reading the fence floor of device %s: %v", doomed.deviceID, err)
		}
		if floor <= doomed.fence {
			t.Errorf("device %s has fence_floor %d after its lease was reclaimed at fence %d; "+
				"the previous holder is not fenced out and can still write to the handset",
				doomed.deviceID, floor, doomed.fence)
		}
	})

	t.Run("an overdue protected lease is held, not reclaimed", func(t *testing.T) {
		// Read out the gates before opening the window. The lease planted
		// beside this one is already gone, so the reaper is provably sweeping;
		// this is the belt to that braces, and it turns a silent pass on a
		// disarmed reaper into a failure that says which gate was shut.
		var enabled bool
		var quiesceIn float64
		var refusal string
		if err := f.DB().QueryRow(ctx, `
SELECT enabled, extract(epoch FROM quiesce_until - now())::float8, COALESCE(last_refusal, '')
  FROM farm.reaper_state WHERE singleton`).Scan(&enabled, &quiesceIn, &refusal); err != nil {
			t.Fatalf("reading farm.reaper_state: %v", err)
		}
		if !enabled || quiesceIn > 0 || refusal != "" {
			t.Fatalf("the reaper is in no position to reclaim anything, so holding this lease "+
				"proves nothing:\n%s", reaperGates(t, f))
		}
		if sum, _ := reaperMetric(t, f.Metrics(t, "reaper"), "farm_reaper_unbeaten_components"); sum != 0 {
			t.Fatalf("farm_reaper_unbeaten_components is %v; this farm's protection would then be "+
				"the refusal to arm rather than farm.leases.protected, which is a different "+
				"property and is tested separately", sum)
		}

		// The window first, and the gauge after it. Both are required, but this
		// is the one that fails within a quarter of a second of the guard being
		// deleted, and it fails saying which lease went and what that means —
		// where the gauge below can only report, ninety seconds later, that a
		// series never rose.
		//
		// It may open immediately: the sibling lease is already reclaimed, and
		// farm.lease_mark_suspect promoted both of them in the same batch, one
		// sweep earlier in the same cycle.
		f.Consistently(t, reaperHold, "the protected lease to stay with its holder", func() error {
			e, err := reaperEnding(ctx, f, spared.leaseID)
			if err != nil {
				return errNotAboutTheLease(err)
			}
			if e.released {
				return fmt.Errorf("protected lease %s was taken back automatically: %s.\n"+
					"migrations/00012_reaper_arm_unbeaten.sql:202 (`AND l.protected = false`) is "+
					"what stops this, and a human's work has just been destroyed by the control "+
					"plane", spared.leaseID, e)
			}
			if e.state != "suspect" {
				return fmt.Errorf("protected lease %s is %q, want \"suspect\": it should have been "+
					"marked for alerting and left alone", spared.leaseID, e.state)
			}
			return nil
		})

		// The census is deliberately coarser than the sweep
		// (reaper.DefaultCensusEvery is 30 seconds), so the gauge lands a
		// cycle or two after the lease does — which is why it is waited for
		// rather than read once.
		f.Eventually(t, 90*time.Second, `farm_lease_suspect{protected="true"} to carry the held lease`, func() error {
			sum, samples := reaperMetric(t, f.Metrics(t, "reaper"), "farm_lease_suspect", `protected="true"`)
			if sum < 1 {
				return fmt.Errorf("it sums to %v across %d sample(s); the page for a lease waiting "+
					"on a human would not fire", sum, samples)
			}
			return nil
		})

		// Held is only half of it. "Hold and page" means the holding is
		// VISIBLE, and these two series are what makes it so: the reaper
		// counts a protected suspect apart from an ordinary one (it logs a
		// WARN beside this counter), and it ended exactly one lease in this
		// whole farm — the other one.
		scrape := f.Metrics(t, "reaper")
		if sum, _ := reaperMetric(t, scrape, "farm_reaper_suspect_marks_total", `protection="protected"`); sum < 1 {
			t.Errorf(`farm_reaper_suspect_marks_total{protection="protected"} is %v, want at least 1: `+
				`a protected lease went suspect and will now wait indefinitely for a human, and `+
				`nothing said so`, sum)
		}
		if sum, _ := reaperMetric(t, scrape, "farm_reaper_leases_ended_total", `reason="holder_expired"`); sum != 1 {
			t.Errorf(`farm_reaper_leases_ended_total{reason="holder_expired"} is %v, want exactly 1. `+
				`Two leases were planted and only the unprotected one may be counted here.`, sum)
		}
	})

	t.Run("the operator can see the protected lease waiting for them", func(t *testing.T) {
		stdout, _, code := f.Ctl(t, "reaper")
		if code != 0 {
			t.Fatalf("ctl reaper exited %d, want 0", code)
		}
		fields := reaperCtlFields(stdout)

		// The four lines together are the whole finding: the reaper is armed
		// and could sweep, one lease is suspect, and the number of leases the
		// next sweep would take is zero. An operator reading this knows the
		// lease is not going to be taken and that it is theirs to resolve.
		if v := fields["armed"]; !strings.HasPrefix(v, "YES") {
			t.Errorf("ctl reaper reports armed %q, want it to start with YES; the hold below would "+
				"then be the quiesce window or the kill switch rather than the protection\n%s", v, stdout)
		}
		if v := fields["live leases"]; v != "1, of which 1 protected" {
			t.Errorf("ctl reaper reports live leases %q, want \"1, of which 1 protected\"\n%s", v, stdout)
		}
		if v := fields["suspect leases"]; v != "1" {
			t.Errorf("ctl reaper reports suspect leases %q, want \"1\"\n%s", v, stdout)
		}
		if v := fields["reclaimable now"]; !strings.HasPrefix(v, "0 ") {
			t.Errorf("ctl reaper reports reclaimable now %q, want it to start with 0: the protected "+
				"lease is overdue in every other respect and must not be counted as something "+
				"the next sweep would take\n%s", v, stdout)
		}

		// And the listing an operator reaches for first names the situation in
		// words, on stderr so a listing piped into jq stays parseable.
		_, stderr, code := f.Ctl(t, "leases")
		if code != 0 {
			t.Fatalf("ctl leases exited %d, want 0", code)
		}
		if !strings.Contains(stderr, "protected lease(s) are suspect") ||
			!strings.Contains(stderr, "will NOT take these back") {
			t.Errorf("ctl leases did not warn that a protected lease is suspect and will not be "+
				"taken back; nobody is paged by a fact nothing says out loud.\nstderr:\n%s", stderr)
		}
	})
}

// TestReaperRefusesToArmWhileAWatchedComponentHasNeverBeaten is LEASE-05's
// remaining hole, end to end.
//
// farm.reaper_arm computes the control-plane gap it refunds across every
// component on the renewal path. A watched name with NO farm.component_heartbeat
// row used to drop silently out of that minimum: the gap read small, nothing
// was refunded, and TTL+grace then ran against leases whose holder had never
// once been given the chance to renew. migrations/00012_reaper_arm_unbeaten.sql
// makes such a name refuse the arm instead — nothing is reclaimed, in the whole
// farm, until it beats.
//
// The lease planted here satisfies every predicate in farm.lease_reclaim's
// candidate set. The refusal is the only thing between it and the sweep, which
// is why the scenario ends by lifting the refusal and watching the same lease
// go: without that, "the lease survived" would only mean "the fixture was never
// reclaimable".
//
// Falsify: change `IF v_unbeaten IS NOT NULL THEN` in
// migrations/00012_reaper_arm_unbeaten.sql:82 to `IF false THEN`. The arm stops
// refusing, the wait for the recorded refusal times out, and the lease is
// reclaimed inside the hold window.
func TestReaperRefusesToArmWhileAWatchedComponentHasNeverBeaten(t *testing.T) {
	t.Parallel()

	// A name in FARM_REAPER_COMPONENTS that nothing in this farm writes. It is
	// not a contrived misconfiguration: it is a component scaled to zero, a
	// role renamed in one manifest and not the other, or a farm deployed with
	// the shipped default list before the last role was rolled out.
	const ghost = "ghost_component"

	f := newFarm(t, farmOpts{
		Roles: []string{"api", "reaper"},
		Env: map[string]string{
			config.EnvReaperInterval: "1s",
			// The harness narrows this list to the roles a scenario actually
			// starts, precisely so that a role somebody left out cannot disarm
			// every other scenario's reaper (see reaperComponents in
			// harness_test.go). This scenario wants exactly that failure, so
			// it writes the list itself — and it must still name every
			// renewal-path component these two processes run, or internal/config
			// refuses to start them at all.
			config.EnvReaperComponent: "reaper,api," + ghost,
		},
	})
	ctx := t.Context()

	// Planted already suspect, unlike the scenario above. The Go loop returns
	// before any sweep while the arm refuses, so a lease planted 'held' here
	// would never be promoted, and "it was not reclaimed" would prove only
	// that the promotion had not happened. Suspect and past reclaimable_at is
	// the state farm.lease_reclaim would take this instant.
	stranded := reaperStage(t, f, reaperFixtureSpec{state: "suspect", protected: false})[0]

	t.Run("the arm refuses and names the component that has never beaten", func(t *testing.T) {
		f.Eventually(t, 45*time.Second, "the reaper to record its refusal to arm", func() error {
			var refusal string
			var recorded bool
			if err := f.DB().QueryRow(ctx, `
SELECT COALESCE(last_refusal, ''), last_refusal_at IS NOT NULL
  FROM farm.reaper_state WHERE singleton`).Scan(&refusal, &recorded); err != nil {
				return err
			}
			if !recorded {
				return fmt.Errorf("farm.reaper_state.last_refusal_at is still unset")
			}
			if !strings.Contains(refusal, ghost) {
				return fmt.Errorf("the standing refusal does not name %s: %s", ghost, refusal)
			}
			return nil
		})

		f.Eventually(t, 30*time.Second, "the reaper's own metrics to publish the refusal", func() error {
			scrape := f.Metrics(t, "reaper")
			if sum, samples := reaperMetric(t, scrape, "farm_reaper_unbeaten_components"); sum != 1 || samples != 1 {
				return fmt.Errorf("farm_reaper_unbeaten_components is %v across %d sample(s), want 1: "+
					"this is the gauge that says a live, leading reaper is reclaiming nothing on "+
					"purpose", sum, samples)
			}
			if sum, _ := reaperMetric(t, scrape, "farm_reaper_arm_refusals_total"); sum < 1 {
				return fmt.Errorf("farm_reaper_arm_refusals_total is %v, want at least 1", sum)
			}
			return nil
		})

		stdout, _, code := f.Ctl(t, "reaper")
		if code != 0 {
			t.Fatalf("ctl reaper exited %d, want 0", code)
		}
		fields := reaperCtlFields(stdout)
		if v := fields["armed"]; !strings.Contains(v, "REFUSED TO ARM") {
			t.Errorf("ctl reaper reports armed %q, want it to say REFUSED TO ARM\n%s", v, stdout)
		}
		if v := fields["never beaten"]; !strings.Contains(v, ghost) {
			t.Errorf("ctl reaper reports never beaten %q, want it to name %s\n%s", v, ghost, stdout)
		}
		// The number that makes the refusal matter rather than merely being
		// recorded: computed with farm.lease_reclaim's own predicates, this
		// farm HAS a lease the next sweep would take, and the reaper is not
		// taking it.
		if v := fields["reclaimable now"]; !strings.HasPrefix(v, "1 ") {
			t.Errorf("ctl reaper reports reclaimable now %q, want it to start with 1: the planted "+
				"lease must be reclaimable in every respect except the refusal, or nothing below "+
				"is being tested\n%s", v, stdout)
		}
	})

	t.Run("nothing is reclaimed while the refusal stands", func(t *testing.T) {
		f.Consistently(t, reaperHold, "the stranded lease to stay with its holder", func() error {
			e, err := reaperEnding(ctx, f, stranded.leaseID)
			if err != nil {
				return errNotAboutTheLease(err)
			}
			if e.released {
				return fmt.Errorf("lease %s was reclaimed under a refused arm: %s.\n"+
					"Its holder was never given a chance to renew across a component the gap "+
					"accounting could not see, which is the BLOCKER 8 mass-reclaim", stranded.leaseID, e)
			}
			return nil
		})
	})

	t.Run("the refusal lifts by itself once the component beats, and the lease then goes", func(t *testing.T) {
		// One heartbeat row, written the way the missing component would have
		// written it. Nothing else about the farm changes.
		if _, err := f.DB().Exec(ctx, `SELECT farm.component_beat($1)`, ghost); err != nil {
			t.Fatalf("beating for %s: %v", ghost, err)
		}

		f.Eventually(t, 60*time.Second, "the reaper to arm by itself now that every watched component has beaten", func() error {
			var refusal string
			if err := f.DB().QueryRow(ctx, `
SELECT COALESCE(last_refusal, '') FROM farm.reaper_state WHERE singleton`).Scan(&refusal); err != nil {
				return err
			}
			if refusal != "" {
				return fmt.Errorf("the refusal still stands: %s", refusal)
			}
			scrape := f.Metrics(t, "reaper")
			if sum, _ := reaperMetric(t, scrape, "farm_reaper_unbeaten_components"); sum != 0 {
				return fmt.Errorf("farm_reaper_unbeaten_components is still %v", sum)
			}
			if sum, _ := reaperMetric(t, scrape, "farm_reaper_arms_total"); sum < 1 {
				return fmt.Errorf("farm_reaper_arms_total is %v, so no arm has succeeded", sum)
			}
			return nil
		})

		// A successful arm sets a fresh quiesce window — the longest live TTL,
		// so every holder gets a full TTL to renew before the first sweep.
		// That is the reaper's OTHER restraint and it is not the one on trial
		// here, so it is wound back rather than waited out.
		reaperOpenQuiesce(t, f)

		f.Eventually(t, 60*time.Second, "the lease the refusal had been holding to be reclaimed", func() error {
			e, err := reaperEnding(ctx, f, stranded.leaseID)
			if err != nil {
				return err
			}
			if !e.released {
				return fmt.Errorf("lease %s is %s\n%s", stranded.leaseID, e, reaperGates(t, f))
			}
			if e.endedBy != "reaper" {
				return fmt.Errorf("lease %s ended, but %s", stranded.leaseID, e)
			}
			return nil
		})
	})
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

// reaperFixtureSpec is one lease to plant.
type reaperFixtureSpec struct {
	// state is the farm.leases.state to INSERT. 'held' with a past expires_at
	// makes the reaper's own suspect sweep do the held -> suspect promotion,
	// which is the path a real silent holder takes; 'suspect' plants a lease
	// that farm.lease_reclaim would take this instant, for a scenario whose
	// reaper never reaches its suspect sweep at all.
	state string

	// protected is farm.leases.protected: the flag that turns an ending into a
	// page.
	protected bool
}

// reaperFixture is one planted lease, as the assertions need to name it.
type reaperFixture struct {
	leaseID  string
	jobID    string
	deviceID string
	fence    int64
}

// reaperStage plants leases whose deadlines are ALREADY in the past, and opens
// the reaper's gates over them.
//
// It runs in ONE transaction because the reaper is sweeping every second
// beside it: a scenario that committed the gates and the leases separately
// would let the reaper observe half a fixture — an open gate over leases that
// are not there yet, or leases with the gate still shut — and either reads as
// the farm misbehaving.
//
// Three gates are touched here and one is deliberately not:
//
//   - quiesce_until is wound into the past. Every successful farm.reaper_arm
//     sets it to now() plus the longest live TTL, so that a control plane which
//     has just come back does not mass-reclaim at the instant of restoration.
//     That restraint is real and is not what any of these scenarios is about.
//   - enabled is asserted true, because the kill switch gates the same
//     function and a farm that ships with it off would pass every negative
//     assertion in this file for the wrong reason.
//   - farm.control_plane_gap is emptied. This is the one that is easy to miss:
//     farm.lease_reclaim refuses to reclaim ANY lease whose silence overlaps a
//     recorded outage in the last six hours, and these leases have been silent
//     for two. A single gap row left by a startup arm would hold every one of
//     them, and the scenario would read that as the guard under test.
//   - last_refusal is NOT touched, in either direction. It is the subject of
//     the third property, and a fixture that cleared it would erase the very
//     thing that scenario is watching.
//
// Every timestamp comes from the server's now(). A fixture built from the test
// machine's clock would drift against the clock every predicate in
// farm.lease_reclaim is evaluated on.
func reaperStage(t *testing.T, f *farm, specs ...reaperFixtureSpec) []reaperFixture {
	t.Helper()
	ctx := t.Context()

	tx, err := f.DB().Begin(ctx)
	if err != nil {
		t.Fatalf("opening the fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One device per lease, read out of the database rather than derived from
	// the seeder's arithmetic: farm.leases has a partial unique index on
	// device_id for live leases, so two fixtures on one handset would fail the
	// second insert rather than plant a second lease.
	type position struct {
		deviceID string
		slotID   int64
	}
	rows, err := tx.Query(ctx, `
SELECT d.id::text, d.current_slot_id
  FROM farm.devices d
 WHERE d.current_lease_id IS NULL AND d.current_slot_id IS NOT NULL
 ORDER BY d.farm_uid
 LIMIT $1`, len(specs))
	if err != nil {
		t.Fatalf("choosing devices for the fixture: %v", err)
	}
	var free []position
	for rows.Next() {
		var p position
		if err := rows.Scan(&p.deviceID, &p.slotID); err != nil {
			rows.Close()
			t.Fatalf("scanning a fixture device: %v", err)
		}
		free = append(free, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("choosing devices for the fixture: %v", err)
	}
	if len(free) != len(specs) {
		t.Fatalf("this farm has %d unleased seeded device(s) and the fixture needs %d",
			len(free), len(specs))
	}

	out := make([]reaperFixture, 0, len(specs))
	for i, spec := range specs {
		// A job per lease. farm.leases references one, the reaper's census
		// joins through it for the pool label, and `ctl reaper` counts live
		// leases from it — a fixture that shared one job across two leases
		// would be a shape the scheduler cannot produce.
		var jobID string
		if err := tx.QueryRow(ctx, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, protected, started_at)
VALUES ($1, $2, $3, 'running', $4, now() - interval '2 hours')
RETURNING id::text`,
			f.Seed().Tenant, f.Seed().Queue, f.Seed().Pool, spec.protected).Scan(&jobID); err != nil {
			t.Fatalf("creating the fixture job: %v", err)
		}

		// Two hours silent, an hour past expires_at and half an hour past
		// reclaimable_at, with no witness. This is test/assertions_v12.sql:74
		// and it is legal for the same reason: the guard trigger is BEFORE
		// UPDATE, so a row may ARRIVE overdue even though nothing may be
		// MOVED overdue.
		l := reaperFixture{jobID: jobID, deviceID: free[i].deviceID}
		if err := tx.QueryRow(ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, state, protected, ttl, grace,
                         acquired_at, heartbeat_at, expires_at, reclaimable_at)
VALUES ($1::uuid, $2::bigint, $3::uuid, $4, $5,
        'e2e-dead-holder', gen_random_uuid(), $6, $7,
        interval '15 minutes', interval '30 minutes',
        now() - interval '2 hours', now() - interval '2 hours',
        now() - interval '1 hour',  now() - interval '30 minutes')
RETURNING id::text, fence`,
			free[i].deviceID, free[i].slotID, jobID, f.Seed().Tenant, f.Seed().Queue,
			spec.state, spec.protected).Scan(&l.leaseID, &l.fence); err != nil {
			t.Fatalf("planting the %s fixture lease: %v", spec.state, err)
		}
		out = append(out, l)
		t.Logf("planted a %s lease %s (fence %d, protected %t) on device %s, silent for two hours",
			spec.state, l.leaseID, l.fence, spec.protected, l.deviceID)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM farm.control_plane_gap`); err != nil {
		t.Fatalf("clearing farm.control_plane_gap for the fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE farm.reaper_state SET enabled = true, quiesce_until = now() - interval '1 hour'
 WHERE singleton`); err != nil {
		t.Fatalf("opening the reaper's gates for the fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the fixture: %v", err)
	}
	return out
}

// reaperOpenQuiesce winds the quiesce window back into the past, so that the
// next sweep is not waiting out a window a fresh arm has just set. See
// reaperStage for why that window exists and why no scenario here waits it out.
func reaperOpenQuiesce(t *testing.T, f *farm) {
	t.Helper()
	if _, err := f.DB().Exec(t.Context(), `
UPDATE farm.reaper_state SET enabled = true, quiesce_until = now() - interval '1 hour'
 WHERE singleton`); err != nil {
		t.Fatalf("winding the quiesce window back: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Readers
// ---------------------------------------------------------------------------

// reaperLeaseState is how one lease stands right now, and — once it has ended —
// what farm.lease_ended_by classifies the ending as. That classification is the
// assertion to reach for: farm.leases.release_reason has no value meaning "a
// socket failed", so the way a broken farm fails here is an ending attributed
// to the wrong party, not an ending with an impossible reason.
type reaperLeaseState struct {
	state    string
	reason   string // empty while the lease is live
	endedBy  string // job | deadline | operator | reaper | unaccounted
	released bool
}

func (e reaperLeaseState) String() string {
	if !e.released {
		return fmt.Sprintf("state=%q and still live", e.state)
	}
	return fmt.Sprintf("state=%q, released with reason %q, which farm.lease_ended_by attributes to %q",
		e.state, e.reason, e.endedBy)
}

// errNotAboutTheLease labels a failure to READ a lease as what it is.
//
// Consistently ends the test on the first non-nil answer, because that is what
// a negative claim requires — but it cannot tell "this lease was taken back"
// from "this query did not complete", and the two would otherwise arrive
// wearing the same sentence. A window that ended because the pool was busy
// must not read as the control plane destroying somebody's work.
func errNotAboutTheLease(err error) error {
	return fmt.Errorf("the lease could not be read, which is a fact about this harness's database "+
		"connection rather than about the lease: %w", err)
}

// reaperEnding reads one lease. It returns an error rather than failing the
// test, so a wait loop can use it.
func reaperEnding(ctx context.Context, f *farm, leaseID string) (reaperLeaseState, error) {
	var e reaperLeaseState
	err := f.DB().QueryRow(ctx, `
SELECT state, COALESCE(release_reason, ''), farm.lease_ended_by(release_reason),
       released_at IS NOT NULL
  FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&e.state, &e.reason, &e.endedBy, &e.released)
	return e, err
}

// reaperGates quotes every gate farm.lease_reclaim consults, for the failure
// message of a sweep that did not happen.
//
// There are four of them, they are all server-side, and internal/reaper has no
// Go-side condition that could second-guess any of them — which is deliberate,
// and is why a scenario that fails with "the lease is still suspect" and none
// of them has told its reader nothing at all.
func reaperGates(t *testing.T, f *farm) string {
	t.Helper()
	var (
		enabled   bool
		quiesceIn float64
		refusal   string
		gaps      int
	)
	if err := f.DB().QueryRow(t.Context(), `
SELECT s.enabled,
       extract(epoch FROM s.quiesce_until - now())::float8,
       COALESCE(s.last_refusal, ''),
       (SELECT count(*)::int FROM farm.control_plane_gap
         WHERE ended_at > now() - interval '6 hours')
  FROM farm.reaper_state s WHERE s.singleton`).Scan(&enabled, &quiesceIn, &refusal, &gaps); err != nil {
		return fmt.Sprintf("  (the reaper's gates could not be read: %v)", err)
	}
	var b strings.Builder
	b.WriteString("the gates farm.lease_reclaim consults:\n")
	fmt.Fprintf(&b, "  enabled:       %t\n", enabled)
	if quiesceIn > 0 {
		fmt.Fprintf(&b, "  quiesce:       shut for another %.0fs\n", quiesceIn)
	} else {
		fmt.Fprintf(&b, "  quiesce:       open (ended %.0fs ago)\n", -quiesceIn)
	}
	if refusal == "" {
		b.WriteString("  arm refusal:   none standing\n")
	} else {
		fmt.Fprintf(&b, "  arm refusal:   %s\n", refusal)
	}
	fmt.Fprintf(&b, "  gap rows:      %d inside the six-hour window, each of which shields every "+
		"lease silent across it\n", gaps)
	return b.String()
}

// reaperMetric sums every sample of one metric family whose series line
// contains all of filters, and reports how many samples matched.
//
// The count is returned because zero samples and a sample at zero are
// different claims, and the difference is the whole point of obs.zeroFill: a
// rule over an absent series returns no data and never fires, while the same
// rule over a series at zero is armed from the first scrape. An assertion that
// could not tell them apart would pass on the farm the pre-seeding exists to
// protect.
func reaperMetric(t *testing.T, scrape, name string, filters ...string) (sum float64, samples int) {
	t.Helper()
	for _, line := range strings.Split(scrape, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// name{labels} value — the value is last, and labels may contain
		// spaces, so the split is from the right.
		cut := strings.LastIndexByte(line, ' ')
		if cut < 0 {
			continue
		}
		series, raw := line[:cut], line[cut+1:]
		if !strings.HasPrefix(series, name) {
			continue
		}
		// Otherwise "farm_reaper_arms_total" would also match
		// "farm_reaper_arms_total_seconds", were one ever added.
		if rest := series[len(name):]; rest != "" && !strings.HasPrefix(rest, "{") {
			continue
		}
		matched := true
		for _, want := range filters {
			if !strings.Contains(series, want) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("the %s listener served a sample this harness cannot read: %q", name, line)
		}
		sum += v
		samples++
	}
	return sum, samples
}

// reaperCtlFields parses `ctl reaper`'s aligned block into key/value pairs.
//
// The block is rendered with the keys padded to the widest one, so the padding
// changes whenever a line is added or removed — which makes a scenario that
// asserts on the rendered text fail for reasons that have nothing to do with
// the farm. Splitting on the first colon and trimming is stable against that,
// and the values keep their own colons.
func reaperCtlFields(stdout string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}
