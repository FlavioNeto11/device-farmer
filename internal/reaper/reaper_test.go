package reaper

// Behavioural tests for the only automatic release path in the system.
//
// Every test here is written against one sentence:
//
//	A lease ends when the job says so, when a deadline the user wrote down
//	elapses, or when a human takes it back. NOTHING ELSE.
//
// So the tests come in two shapes. The ones that prove a lease DOES end name
// the permitted cause (the holder went silent across no control-plane gap; the
// user's own max_runtime elapsed). The ones that prove a lease does NOT end
// hand the loop every excuse a well-meaning patch might take — an offline
// device, a disabled device, a drained host, an outage, a protected lease — and
// require it to keep its hands off.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// ---------------------------------------------------------------------------
// 1. Arming happens before any sweep
// ---------------------------------------------------------------------------

// TestReaperArmsBeforeItSweeps is the ordering test, and it is the reason the
// gained-leadership branch exists at all.
//
// The lease below is long overdue and the quiesce gate is wide open, so a sweep
// would take it instantly. What saves it is farm.reaper_arm running first: the
// refund pushes its deadline back out by the length of the outage, and the
// recorded gap makes it unreclaimable for as long as that gap overlaps its
// silence. Sweep first and this device is handed to another job because OUR
// control plane was down.
func TestReaperArmsBeforeItSweeps(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{state: "running"})
	l := f.seedLease(dev, job, leaseOpts{
		state:         "suspect",
		acquiredIn:    -2 * time.Hour,
		heartbeatIn:   -2 * time.Hour,
		expiresIn:     -1 * time.Hour,
		reclaimableIn: -30 * time.Minute,
	})

	// Gate open: nothing server-side is protecting this lease yet.
	f.exec(`UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 hour', enabled = true`)

	// A ten-minute API outage while the reaper and Postgres stayed healthy.
	// This is BLOCKER 8's exact shape: a gap keyed on the reaper's own
	// heartbeat would see nothing wrong and reclaim the whole farm.
	f.beat("api", 10*time.Minute)

	var before time.Time
	f.scan(&before, `SELECT reclaimable_at FROM farm.leases WHERE id = $1::uuid`, l.id)

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)

	if state, reason := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("the cycle that gained leadership reclaimed a lease (state=%q reason=%q): "+
			"farm.reaper_arm must run BEFORE the first sweep, or the tenant is charged for "+
			"the control plane's own downtime", state, str(reason))
	}

	var gaps int
	f.scan(&gaps, `SELECT count(*) FROM farm.control_plane_gap`)
	if gaps != 1 {
		t.Fatalf("control_plane_gap rows = %d, want 1: the outage was not recorded", gaps)
	}

	var component string
	var gapSeconds float64
	f.scan(&component, `SELECT component FROM farm.control_plane_gap ORDER BY ended_at DESC LIMIT 1`)
	f.scan(&gapSeconds, `SELECT EXTRACT(EPOCH FROM (ended_at - started_at))
	                       FROM farm.control_plane_gap ORDER BY ended_at DESC LIMIT 1`)
	if component != "api" {
		t.Errorf("gap component = %q, want %q: the gap must name the component that was "+
			"actually oldest, not the reaper", component, "api")
	}
	if gapSeconds < 9*60 {
		t.Errorf("recorded gap = %.0fs, want about 600s", gapSeconds)
	}

	// The refund is the point: every live lease gets the outage handed back.
	var after time.Time
	f.scan(&after, `SELECT reclaimable_at FROM farm.leases WHERE id = $1::uuid`, l.id)
	if got := after.Sub(before); got < 9*time.Minute {
		t.Errorf("reclaimable_at moved by %s, want about 10m refunded", got)
	}

	// And the quiesce gate is armed, so a control plane that has just come back
	// does not mass-reclaim at the instant of restoration.
	var quiesceAhead bool
	f.scan(&quiesceAhead, `SELECT quiesce_until > now() FROM farm.reaper_state`)
	if !quiesceAhead {
		t.Error("quiesce_until is not in the future: the restoration gate was not armed")
	}
}

// TestReaperArmsOncePerGainOfLeadership guards the other half: arming is not
// idempotent. farm.reaper_arm ADDS the gap to every live lease, so arming on
// every cycle would extend every lease in the farm forever and nothing would
// ever be reclaimable again.
func TestReaperArmsOncePerGainOfLeadership(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{state: "running"})
	f.seedLease(dev, job, leaseOpts{expiresIn: 15 * time.Minute, reclaimableIn: 45 * time.Minute})

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)
	var firstArm time.Time
	f.scan(&firstArm, `SELECT armed_at FROM farm.reaper_state`)

	for i := 0; i < 3; i++ {
		r.cycle(ctx)
	}
	var lastArm time.Time
	f.scan(&lastArm, `SELECT armed_at FROM farm.reaper_state`)

	if !lastArm.Equal(firstArm) {
		t.Errorf("armed_at moved from %s to %s across steady-state cycles: "+
			"re-arming without a change of leadership refunds an outage that did not happen",
			firstArm, lastArm)
	}
}

// TestReaperStandsDownWhenItCannotArm covers the failure branch. A reaper that
// cannot arm is a reaper that cannot know whether the silence it is looking at
// is the holder's or its own, so it must sweep nothing and give leadership up.
func TestReaperStandsDownWhenItCannotArm(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{state: "running"})
	l := f.seedLease(dev, job, leaseOpts{
		state:         "suspect",
		acquiredIn:    -2 * time.Hour,
		heartbeatIn:   -2 * time.Hour,
		expiresIn:     -1 * time.Hour,
		reclaimableIn: -30 * time.Minute,
	})
	f.exec(`UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 hour'`)

	// Break the arm by renaming the function out from under the store. Any
	// failure would do; this one is reversible and leaves the rest of the
	// schema untouched.
	f.exec(`ALTER FUNCTION farm.reaper_arm(text[], interval) RENAME TO reaper_arm_unavailable`)
	t.Cleanup(func() {
		f.exec(`ALTER FUNCTION farm.reaper_arm_unavailable(text[], interval) RENAME TO reaper_arm`)
	})

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)

	if state, reason := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("a reaper that could not arm swept anyway and ended a lease "+
			"(state=%q reason=%q)", state, str(reason))
	}
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 0 {
		t.Errorf("advisory lock still held by %d session(s) after a failed arm: "+
			"the reaper must stand down so another replica can try", n)
	}
	if msgs := rec.atOrAbove(slog.LevelError); len(msgs) == 0 {
		t.Error("a failed arm was not logged at error level; it is the one failure that " +
			"silently disables the release path")
	}
}

// ---------------------------------------------------------------------------
// 2. The quiesce gate and the enabled switch
// ---------------------------------------------------------------------------

// TestReaperRespectsTheQuiesceGate and TestReaperRespectsReaperStateEnabled
// share a shape: prove the reaper reclaims nothing while the gate is shut, then
// open it and prove it reclaims — so "nothing was reclaimed" can never pass
// because the setup was inert.
func TestReaperRespectsTheQuiesceGate(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	l, r, ctx := reclaimableLeaseAndArmedReaper(t, f)
	defer r.lead.release(ctx)

	f.openReclaimGate()
	f.exec(`UPDATE farm.reaper_state SET quiesce_until = now() + interval '1 hour'`)

	r.cycle(ctx)
	if state, _ := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("lease state = %q while the quiesce window was open; the gate was ignored", state)
	}

	f.openReclaimGate()
	r.cycle(ctx)
	if state, reason := f.leaseState(l.id); state != "expired" || str(reason) != "holder_expired" {
		t.Fatalf("lease state = %q reason = %q after the gate closed, want expired/holder_expired "+
			"(without this the test above proves nothing)", state, str(reason))
	}
}

func TestReaperRespectsReaperStateEnabled(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	l, r, ctx := reclaimableLeaseAndArmedReaper(t, f)
	defer r.lead.release(ctx)

	f.openReclaimGate()
	f.exec(`UPDATE farm.reaper_state SET enabled = false`)

	r.cycle(ctx)
	if state, _ := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("lease state = %q with farm.reaper_state.enabled = false; the operator's "+
			"off switch was ignored", state)
	}

	f.exec(`UPDATE farm.reaper_state SET enabled = true`)
	r.cycle(ctx)
	if state, _ := f.leaseState(l.id); state != "expired" {
		t.Fatalf("lease state = %q after re-enabling, want expired", state)
	}
}

// TestReaperNeverReclaimsAcrossAControlPlaneGap is the refund's second half. A
// recorded outage that ended after the holder's last heartbeat means the
// silence may be ours, not theirs, and an ambiguous silence is not evidence.
func TestReaperNeverReclaimsAcrossAControlPlaneGap(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	l, r, ctx := reclaimableLeaseAndArmedReaper(t, f)
	defer r.lead.release(ctx)

	f.openReclaimGate()
	// An outage that ended after this holder went quiet.
	f.exec(`INSERT INTO farm.control_plane_gap (component, started_at, ended_at)
	        VALUES ('api', now() - interval '20 minutes', now() - interval '5 minutes')`)

	r.cycle(ctx)
	if state, _ := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("lease state = %q, want suspect: a silence that overlaps our own outage "+
			"is not evidence that the holder is gone", state)
	}
}

// ---------------------------------------------------------------------------
// 3. Protection
// ---------------------------------------------------------------------------

// TestReaperNeverReclaimsAProtectedLease runs a protected and an unprotected
// lease through the same sweep. The unprotected one going away is what proves
// the sweep actually ran.
func TestReaperNeverReclaimsAProtectedLease(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	overdue := leaseOpts{
		state:         "suspect",
		acquiredIn:    -6 * time.Hour,
		heartbeatIn:   -6 * time.Hour,
		expiresIn:     -5 * time.Hour,
		reclaimableIn: -4 * time.Hour,
	}

	protectedOpts := overdue
	protectedOpts.protected = true
	protectedLease := f.seedLease(f.device(deviceOpts{}), f.job(jobOpts{state: "running"}), protectedOpts)
	ordinary := f.seedLease(f.device(deviceOpts{}), f.job(jobOpts{state: "running"}), overdue)

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx) // gains leadership and arms; the arm closes the gate
	f.openReclaimGate()

	// Sweep repeatedly: "protected" means never, not "not yet".
	for i := 0; i < 3; i++ {
		r.cycle(ctx)
	}

	if state, reason := f.leaseState(protectedLease.id); state != "suspect" {
		t.Fatalf("PROTECTED lease was auto-reclaimed (state=%q reason=%q). It must be held "+
			"indefinitely and a human paged instead", state, str(reason))
	}
	if state, reason := f.leaseState(ordinary.id); state != "expired" || str(reason) != "holder_expired" {
		t.Fatalf("the unprotected sibling was not reclaimed (state=%q reason=%q); the sweep "+
			"never ran, so the assertion above is vacuous", state, str(reason))
	}
}

// TestSuspectReleasesNothingAndPagesForProtected covers the suspect sweep,
// whose whole job is to alert. Entering suspect must move no device.
func TestSuspectReleasesNothingAndPagesForProtected(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	held := leaseOpts{
		state:         "held",
		acquiredIn:    -1 * time.Hour,
		heartbeatIn:   -1 * time.Hour,
		expiresIn:     -1 * time.Minute,
		reclaimableIn: 29 * time.Minute,
	}
	protectedOpts := held
	protectedOpts.protected = true

	protectedLease := f.seedLease(f.device(deviceOpts{}), f.job(jobOpts{state: "running"}), protectedOpts)
	ordinary := f.seedLease(f.device(deviceOpts{}), f.job(jobOpts{state: "running"}), held)

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)

	for _, l := range []seededLease{protectedLease, ordinary} {
		if state, _ := f.leaseState(l.id); state != "suspect" {
			t.Fatalf("lease %s state = %q, want suspect", l.id, state)
		}
		// The device is still spoken for. Suspect is an alert, not a handover.
		var stillHeld bool
		f.scan(&stillHeld,
			`SELECT current_lease_id = $2::uuid FROM farm.devices WHERE id = $1::uuid`, l.device, l.id)
		if !stillHeld {
			t.Errorf("device %s lost its lease pointer on entering suspect; suspect must "+
				"release nothing", l.device)
		}
	}

	if !containsMessage(rec.atOrAbove(slog.LevelWarn), "PROTECTED") {
		t.Error("a protected lease went suspect without a warning; nobody will notice a lease " +
			"that is now stuck until a human acts")
	}
}

// TestAHeartbeatHealsASuspectLeaseAtTheSameFence is the other half of "suspect
// does nothing": the holder comes back and loses nothing at all.
func TestAHeartbeatHealsASuspectLeaseAtTheSameFence(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{state: "running"})
	l := f.seedLease(dev, job, leaseOpts{
		state:         "held",
		acquiredIn:    -1 * time.Hour,
		heartbeatIn:   -1 * time.Hour,
		expiresIn:     -1 * time.Minute,
		reclaimableIn: 29 * time.Minute,
	})

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)
	if state, _ := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("lease state = %q, want suspect", state)
	}

	store := lease.NewStore(pool)
	res, err := store.Renew(ctx, l.id, l.fence, l.holderInstance)
	if err != nil {
		t.Fatalf("renew a suspect lease: %v (a heartbeat in the grace band must heal it)", err)
	}
	if !res.WasSuspect {
		t.Error("RenewResult.WasSuspect = false; the self-heal was invisible to alerting")
	}

	var state string
	var fence int64
	f.scan(&state, `SELECT state FROM farm.leases WHERE id = $1::uuid`, l.id)
	f.scan(&fence, `SELECT fence FROM farm.leases WHERE id = $1::uuid`, l.id)
	if state != "held" {
		t.Errorf("lease state = %q after a heartbeat, want held", state)
	}
	if fence != l.fence {
		t.Errorf("fence moved %d -> %d on self-heal; the holder would have been fenced out "+
			"of its own run", l.fence, fence)
	}
}

// ---------------------------------------------------------------------------
// 4. Device health is not an input
// ---------------------------------------------------------------------------

// TestDeviceHealthCannotEndALiveLease is STF #663, written as a test.
//
// The device is offline, unauthorised, unreachable and its host is drained. The
// holder is heartbeating normally. Nothing may happen to this lease.
func TestDeviceHealthCannotEndALiveLease(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	drainedHost, drainedHub := f.newHost("h-drained", "draining")
	dev := f.device(deviceOpts{
		hostID:     drainedHost,
		hubID:      drainedHub,
		adbState:   "offline",
		health:     "offline",
		adminState: "quarantined",
		slotState:  "maintenance",
	})
	job := f.job(jobOpts{state: "running"})
	l := f.seedLease(dev, job, leaseOpts{
		acquiredIn:    -3 * time.Hour,
		heartbeatIn:   -1 * time.Minute, // the HOLDER is fine
		expiresIn:     14 * time.Minute,
		reclaimableIn: 44 * time.Minute,
	})

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)
	f.openReclaimGate()
	for i := 0; i < 3; i++ {
		r.cycle(ctx)
	}

	if state, reason := f.leaseState(l.id); state != "held" {
		t.Fatalf("a live lease on an offline, quarantined device on a drained host was ended "+
			"(state=%q reason=%q). This is DeviceFarmer/STF #663: a transport fact was allowed "+
			"to mean the holder is gone", state, str(reason))
	}
}

// TestDeviceHealthCannotSaveALeaseFromReclaim is the same firewall from the
// other side. A perfectly healthy device does not buy a silent holder more
// time, because health is not evidence about the holder either way.
func TestDeviceHealthCannotSaveALeaseFromReclaim(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	overdue := leaseOpts{
		state:         "suspect",
		acquiredIn:    -6 * time.Hour,
		heartbeatIn:   -6 * time.Hour,
		expiresIn:     -5 * time.Hour,
		reclaimableIn: -4 * time.Hour,
	}
	healthy := f.seedLease(f.device(deviceOpts{}), f.job(jobOpts{state: "running"}), overdue)
	broken := f.seedLease(
		f.device(deviceOpts{adbState: "offline", health: "offline"}),
		f.job(jobOpts{state: "running"}), overdue)

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)
	f.openReclaimGate()
	r.cycle(ctx)

	for name, l := range map[string]seededLease{"healthy device": healthy, "offline device": broken} {
		state, reason := f.leaseState(l.id)
		if state != "expired" || str(reason) != "holder_expired" {
			t.Errorf("%s: lease state = %q reason = %q, want expired/holder_expired. "+
				"Reclamation must turn on the holder's silence alone", name, state, str(reason))
		}
	}
}

// TestReclaimRunsAsARoleThatCannotReadHealth checks the mechanism rather than
// the behaviour: the Go loop's guarantee that health cannot influence
// reclamation rests entirely on farm.lease_reclaim carrying `SET role =
// farm_reaper`, and on that role having no SELECT on farm.device_runtime.
// Either half quietly disappearing would leave every test above still passing
// while the door stood open for the next patch.
func TestReclaimRunsAsARoleThatCannotReadHealth(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	var settings []string
	f.scan(&settings, `
SELECT COALESCE(p.proconfig, ARRAY[]::text[])
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'farm' AND p.proname = 'lease_reclaim'`)

	var found bool
	for _, s := range settings {
		if strings.EqualFold(strings.ReplaceAll(s, " ", ""), "role=farm_reaper") {
			found = true
		}
	}
	if !found {
		t.Fatalf("farm.lease_reclaim proconfig = %v, want role=farm_reaper. Without it "+
			"reclamation runs with the caller's privileges and can read device health", settings)
	}

	var canReadHealth bool
	f.scan(&canReadHealth,
		`SELECT has_table_privilege('farm_reaper', 'farm.device_runtime', 'SELECT')`)
	if canReadHealth {
		t.Fatal("farm_reaper can SELECT farm.device_runtime: the firewall that makes " +
			"reclamation structurally blind to health has been opened")
	}
}

// ---------------------------------------------------------------------------
// 5. Leader election
// ---------------------------------------------------------------------------

// TestLeadershipHoldsADedicatedConnection is the property a pooled connection
// would break. A session advisory lock lives in a Postgres SESSION; if the
// connection went back to the pool between cycles the lock's lifetime would
// have nothing to do with the work it guards, and the pool's idle reaper could
// close it while this process still believed it led.
func TestLeadershipHoldsADedicatedConnection(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	rec := &logRecorder{}
	leader := f.newReaper(rec)
	standby := f.newReaper(rec)
	ctx := context.Background()
	defer leader.lead.release(ctx)
	defer standby.lead.release(ctx)

	leader.cycle(ctx)

	// Checked out and STILL checked out now the cycle has returned.
	if got := pool.Stat().AcquiredConns(); got != 1 {
		t.Fatalf("acquired connections after a leader cycle = %d, want 1: leadership must "+
			"hold its own connection between cycles, not borrow one per query", got)
	}
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 1 {
		t.Fatalf("sessions holding the reaper lock = %d, want 1", n)
	}

	standby.cycle(ctx)
	if got := pool.Stat().AcquiredConns(); got != 1 {
		t.Errorf("acquired connections after a standby cycle = %d, want 1: a standby must "+
			"give its connection back rather than hold one hostage per replica", got)
	}
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 1 {
		t.Errorf("sessions holding the reaper lock = %d, want 1: leader election elected two", n)
	}

	// Releasing hands leadership over in milliseconds instead of waiting for a
	// TCP session to be reaped.
	leader.lead.release(ctx)
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 0 {
		t.Errorf("sessions holding the reaper lock after release = %d, want 0", n)
	}
}

// TestSecondReaperIdlesInsteadOfDuplicatingReclaims. Two reapers arming at once
// would refund the same outage twice; two sweeping at once would double the
// audit trail. The standby must do nothing but census and heartbeat.
func TestSecondReaperIdlesInsteadOfDuplicatingReclaims(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	l, leader, ctx := reclaimableLeaseAndArmedReaper(t, f)
	defer leader.lead.release(ctx)

	rec := &logRecorder{}
	standby := f.newReaper(rec)
	defer standby.lead.release(ctx)

	f.openReclaimGate()

	var armBefore time.Time
	f.scan(&armBefore, `SELECT armed_at FROM farm.reaper_state`)

	standby.cycle(ctx)

	if state, _ := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("a standby reaper reclaimed a lease (state=%q); only the leader may sweep", state)
	}
	var armAfter time.Time
	f.scan(&armAfter, `SELECT armed_at FROM farm.reaper_state`)
	if !armAfter.Equal(armBefore) {
		t.Error("a standby reaper armed: two reapers arming refund the same outage twice")
	}
	// The standby still beats, because a healthy standby that would take over
	// within one cycle is not a control-plane outage.
	var beats int
	f.scan(&beats, `SELECT count(*) FROM farm.component_heartbeat WHERE component = $1`, DefaultComponent)
	if beats != 1 {
		t.Errorf("reaper heartbeat rows = %d, want 1: a standby must still report itself alive", beats)
	}

	// And the leader does the work.
	leader.cycle(ctx)
	if state, _ := f.leaseState(l.id); state != "expired" {
		t.Fatalf("the leader did not reclaim either (state=%q); the standby assertion above "+
			"would pass against a reaper that can never reclaim at all", state)
	}
}

// TestReaperReArmsWhenLeadershipMoves. A replica taking over was blind for
// however long the handover took, and a blind reaper must refund before it acts.
func TestReaperReArmsWhenLeadershipMoves(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{state: "running"})
	f.seedLease(dev, job, leaseOpts{expiresIn: 15 * time.Minute, reclaimableIn: 45 * time.Minute})

	rec := &logRecorder{}
	first := f.newReaper(rec)
	second := f.newReaper(rec)
	ctx := context.Background()
	defer first.lead.release(ctx)
	defer second.lead.release(ctx)

	first.cycle(ctx)
	var firstArm time.Time
	f.scan(&firstArm, `SELECT armed_at FROM farm.reaper_state`)

	// While the first still leads, the second must not arm.
	second.cycle(ctx)
	var duringStandby time.Time
	f.scan(&duringStandby, `SELECT armed_at FROM farm.reaper_state`)
	if !duringStandby.Equal(firstArm) {
		t.Fatal("a standby armed while another replica held the lock")
	}

	first.lead.release(ctx)
	second.cycle(ctx)

	var secondArm time.Time
	f.scan(&secondArm, `SELECT armed_at FROM farm.reaper_state`)
	if !secondArm.After(firstArm) {
		t.Errorf("armed_at = %s, want later than %s: a replica that gains leadership must "+
			"arm before its first sweep", secondArm, firstArm)
	}
}

// ---------------------------------------------------------------------------
// 6. The permitted endings
// ---------------------------------------------------------------------------

// TestReaperEndsALeaseAtTheUsersOwnMaxRuntime. The holder is heartbeating
// perfectly and the device is healthy; what ends this lease is a number the
// user wrote down. The sibling without a max_runtime is the control.
func TestReaperEndsALeaseAtTheUsersOwnMaxRuntime(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	live := leaseOpts{
		acquiredIn:    -2 * time.Hour,
		heartbeatIn:   -10 * time.Second,
		expiresIn:     14 * time.Minute,
		reclaimableIn: 44 * time.Minute,
	}
	capped := f.seedLease(
		f.device(deviceOpts{}),
		f.job(jobOpts{state: "running", maxRuntime: dur(time.Minute)}),
		live)
	uncapped := f.seedLease(
		f.device(deviceOpts{}),
		f.job(jobOpts{state: "running"}),
		live)

	rec := &logRecorder{}
	r := f.newReaper(rec)
	ctx := context.Background()
	defer r.lead.release(ctx)

	r.cycle(ctx)

	if state, reason := f.leaseState(capped.id); state != "expired" || str(reason) != "max_runtime" {
		t.Errorf("capped lease state = %q reason = %q, want expired/max_runtime", state, str(reason))
	}
	if state, _ := f.leaseState(uncapped.id); state != "held" {
		t.Errorf("uncapped lease state = %q, want held: a job with no max_runtime has no "+
			"deadline to outrun", state)
	}
}

// TestReclaimWritesAnAuditEvent. farm.lease_reclaim writes no events by design,
// so the forensic record is this loop's responsibility. A reclaim nobody can
// explain afterwards is indistinguishable from data loss.
func TestReclaimWritesAnAuditEvent(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	l, r, ctx := reclaimableLeaseAndArmedReaper(t, f)
	defer r.lead.release(ctx)

	f.openReclaimGate()
	r.cycle(ctx)

	if state, _ := f.leaseState(l.id); state != "expired" {
		t.Fatalf("lease state = %q, want expired", state)
	}

	var (
		actor    string
		deviceID string
		jobID    string
		slotID   *int64
		reason   string
		oldFence int64
		newFloor int64
	)
	if err := pool.QueryRow(ctx, `
SELECT actor, device_id::text, job_id::text, slot_id,
       detail->>'release_reason', (detail->>'old_fence')::bigint, (detail->>'new_floor')::bigint
  FROM farm.events
 WHERE kind = 'lease_reclaimed' AND lease_id = $1::uuid`, l.id).
		Scan(&actor, &deviceID, &jobID, &slotID, &reason, &oldFence, &newFloor); err != nil {
		t.Fatalf("read the reclaim audit event: %v", err)
	}

	if actor != DefaultComponent {
		t.Errorf("event actor = %q, want %q", actor, DefaultComponent)
	}
	if deviceID != l.device || jobID != l.job {
		t.Errorf("event names device %s job %s, want device %s job %s", deviceID, jobID, l.device, l.job)
	}
	if slotID == nil {
		t.Error("event slot_id is NULL for a slot-bound lease; the physical position is what " +
			"a human needs to find the phone")
	}
	if reason != "holder_expired" {
		t.Errorf("event release_reason = %q, want holder_expired", reason)
	}
	if oldFence != l.fence {
		t.Errorf("event old_fence = %d, want %d", oldFence, l.fence)
	}
	if newFloor <= oldFence {
		t.Errorf("event new_floor = %d, not above old_fence = %d: the departed holder's "+
			"sockets would still be accepted at the proxy", newFloor, oldFence)
	}
}

// TestReclaimQuarantinesTheSlotAndRaisesTheFenceFloor. Ending the lease is only
// half of a safe handover; the other half is making the previous holder's fence
// stale and keeping the slot unschedulable until its sockets are certainly gone.
func TestReclaimQuarantinesTheSlotAndRaisesTheFenceFloor(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	l, r, ctx := reclaimableLeaseAndArmedReaper(t, f)
	defer r.lead.release(ctx)

	f.openReclaimGate()
	r.cycle(ctx)

	var floor int64
	f.scan(&floor, `SELECT fence_floor FROM farm.devices WHERE id = $1::uuid`, l.device)
	if floor <= l.fence {
		t.Errorf("device fence_floor = %d, want above the reclaimed fence %d", floor, l.fence)
	}

	var quarantined bool
	f.scan(&quarantined, `
SELECT s.rearm_at > now()
  FROM farm.slots s JOIN farm.devices d ON d.current_slot_id = s.id
 WHERE d.id = $1::uuid`, l.device)
	if !quarantined {
		t.Error("the slot is immediately schedulable again after a reclaim; the rearm window " +
			"is what stops a new job landing on a device the old holder is still talking to")
	}

	var current *string
	f.scan(&current, `SELECT current_lease_id::text FROM farm.devices WHERE id = $1::uuid`, l.device)
	if current != nil {
		t.Errorf("device still points at lease %s after it was reclaimed", *current)
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted a Config with no Pool")
	}
	// A zero pool is never dialled; New must reject the missing Store before
	// anything touches it.
	if _, err := New(Config{Pool: &pgxpool.Pool{}}); err == nil {
		t.Error("New accepted a Config with no Store")
	}
}

// ---------------------------------------------------------------------------
// Shared setup
// ---------------------------------------------------------------------------

// reclaimableLeaseAndArmedReaper produces the standard starting position for
// the gate tests: one unprotected lease whose holder has been silent for hours,
// and a reaper that already holds leadership and has already armed. The caller
// opens the gate itself, so no test reclaims by accident.
func reclaimableLeaseAndArmedReaper(t *testing.T, f *fixture) (seededLease, *Reaper, context.Context) {
	t.Helper()

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{state: "running"})
	l := f.seedLease(dev, job, leaseOpts{
		state:         "suspect",
		acquiredIn:    -6 * time.Hour,
		heartbeatIn:   -6 * time.Hour,
		expiresIn:     -5 * time.Hour,
		reclaimableIn: -4 * time.Hour,
	})

	r := f.newReaper(&logRecorder{})
	ctx := context.Background()
	r.cycle(ctx) // gain leadership and arm

	if state, _ := f.leaseState(l.id); state != "suspect" {
		t.Fatalf("the arming cycle already reclaimed the lease (state=%q)", state)
	}
	return l, r, ctx
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func containsMessage(msgs []string, substr string) bool {
	for _, m := range msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}
