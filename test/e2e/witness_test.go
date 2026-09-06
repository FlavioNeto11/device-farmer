package e2e

// LEASE-09, end to end: a job that is demonstrably alive on the device but
// blind to the control plane keeps its device.
//
// # What this scenario shows that the unit suites cannot
//
// internal/lease proves farm.lease_witness pushes a deadline; internal/runner
// proves the marker only reports a write that landed; internal/jobrunner proves
// the loop is started for every placement. What none of them can show is the
// thing the requirement is actually about: a REAL lease, granted by the real
// scheduler to a real job, sitting past its reclaimable_at with a real reaper
// sweeping over it, and surviving — because a marker on a phone is being
// rewritten and a witness is landing.
//
// So this scenario reaches the state STF #663 destroys work in, and then asks
// the farm to destroy it. It does not.
//
// # The outage this models, and why the alternatives are wrong
//
// The holder cannot RENEW. Everything else is intact: the process is alive, the
// database is up, the device answers, and the marker is being rewritten. That
// combination is deliberate, and it is the only one that tests the witness:
//
//   - Taking Postgres away takes the witness away too. Both paths are UPDATEs
//     on farm.leases issued from one process over one pool; a database outage
//     silences the evidence along with the renewals and proves nothing about
//     which of them was carrying the lease.
//   - Killing the jobrunner takes the marker, the witness and the work with it.
//   - Changing the lease's holder_instance or fence is not an outage, it is a
//     HANDOVER: farm.lease_renew returns zero rows, which means fenced, and a
//     fenced holder is supposed to abort. Reading that as an outage would be
//     exactly the conflation this system exists to prevent.
//
// Because the two paths share the process, the pool and the socket, the only
// place they can be told apart is the STATEMENT each issues — so that is where
// the fault goes: a trigger that refuses any UPDATE which advances
// heartbeat_seq, which farm.lease_renew does and farm.lease_witness does not.
// The holder sees an ordinary error, which internal/lease classifies as
// TRANSIENT (only zero rows is fencing), retries on its backoff, and never
// stops holding. Server-side the farm is left in exactly the state a long
// control-plane outage leaves: heartbeat_at frozen at the grant, witness_at
// advancing on its own cadence. The trigger counts its refusals in a sequence
// — nextval survives the exception that follows it — so the scenario can assert
// that the outage was real and how many renewals it cost.
//
// One other writer can refresh a lease without renewing it, and it is watched
// rather than blocked: the re-attach in farm.lease_acquire leaves heartbeat_seq
// alone, bumps holder_epoch, and pushes both deadlines while zeroing
// witness_extensions. It happens once, when the jobrunner picks the placement
// up, and every assertion below reads holder_epoch beside heartbeat_seq so that
// a second one cannot be mistaken for the witness's work.
//
// # Why the clock is moved by hand
//
// A lease cannot become reclaimable inside a short scenario, and that is not a
// gap in the harness: farm.jobs CHECKs ttl >= 10 minutes and grace >= 5
// minutes, and the leases_guard trigger refuses to let either deadline move
// backwards. The earliest instant any real lease may be reclaimed is therefore
// fifteen minutes after it was granted. Waiting is not an option and faking the
// ending is forbidden, so the scenario does what internal/lease's own suite
// does (backdateLease in store_test.go): it disables leases_guard for the
// length of one transaction and moves the lease's clocks — acquired_at,
// heartbeat_at, expires_at, reclaimable_at — into the past, which is elapsed
// time and nothing else.
//
// witness_at is the one clock it does NOT move, because that one is not
// hypothetical: the device really is being touched, right now, by a marker loop
// that has never stopped. That is the whole experiment. Everything else about
// this lease says "silent for the better part of an hour"; the evidence says
// "working"; and the reaper reads the evidence.
//
// # What it costs, and why that is the floor
//
// Two and a half minutes, measured, and almost all of it is other people's
// clocks:
//
//   - ~35s before the work starts. The scheduler and the jobrunner back their
//     polls off to 20s and 15s on an idle farm, and neither ceiling is settable
//     by environment — a queued job on a quiet farm waits for both.
//   - 90s of witness. The cadence is floored at 30s
//     (config.MinLeaseWitnessInterval: the marker cadence follows it at a
//     quarter, and lower would have every leased device answering a shell round
//     trip every few hundred milliseconds), and three ticks are needed — one to
//     show a witness landing at all, one to show the counter ADVANCING and the
//     deadline being pushed out of the past, one dark tick to show the loop
//     refusing to present evidence it no longer has.
//   - the tail, in which the job finishes and gives its own lease back.
//
// Nothing here can be made shorter without lowering that floor, and the job's
// sleep is sized to outlast the last of it with about twenty seconds to spare:
// a job that ended mid-phase would release the lease under an assertion that
// the lease is still held.
//
// # Falsification, both halves, run
//
//   - Break the product. With jobrunner.startWitness returning nil before it
//     starts the loop, no placement gets a witness: witness_at stays NULL and
//     the first wait fails after 90s with the whole row quoted.
//   - Break the premise. Leave the farm alone and pass withWitness=true to the
//     backdate below, which ages witness_at along with the deadlines — the same
//     outage, with the device dark for all of it instead of being touched
//     throughout. The reaper takes the lease within one sweep: the wait for
//     'suspect' fails reporting state=expired, release_reason='holder_expired'.
//     That is the run this scenario is the counterfactual of, and the single
//     input that differs between the two is whether witness_at was current.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/runner"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// The cadences this scenario runs at. Three of them are taken from the
// constants the binary validates against rather than written down again, so a
// farm whose floors move takes this scenario with it.
var (
	// witnessCadence is the floor on FARM_LEASE_WITNESS_INTERVAL. Everything
	// below is timed off it: the marker is rewritten four times per tick and
	// evidence older than three marker intervals is not presented, which is
	// what makes the dark phase land on the very next tick.
	witnessCadence = config.MinLeaseWitnessInterval

	// witnessMarkerCadence and witnessEvidenceWindow are derived by the same
	// rule internal/jobrunner derives them by; they are read here only to
	// justify the waits.
	witnessMarkerCadence  = config.MarkerIntervalFor(witnessCadence)
	witnessEvidenceWindow = config.MaxEvidenceAgeFor(witnessMarkerCadence)

	// witnessRenewCadence is shorter than the shipped 90s on purpose. The
	// outage below refuses renewals, and a holder whose first attempt is due
	// two thirds of the way through the run would spend most of the scenario
	// not having tried. At 20s the holder has been refused several times
	// before the first witness even lands, and farm.lease_renew's own rule —
	// three attempts inside one TTL — is satisfied with room to spare.
	witnessRenewCadence = 20 * time.Second

	// witnessReaperCadence: the window in which this lease is past due and
	// nothing but the witness covers it is bounded by one witness tick, so the
	// reaper has to sweep several times inside it for "it took nothing" to be
	// a statement about the reaper rather than about its timer.
	witnessReaperCadence = 3 * time.Second

	// witnessHold is the sleep step. It has to outlast every phase below, and
	// the last of them is bounded by the fourth witness tick.
	witnessHold = 115 * time.Second

	// witnessSaveWindow is the negative assertion at the centre of this file.
	// It has to close before the next witness tick pushes reclaimable_at back
	// into the future — that push is asserted separately, immediately
	// afterwards — so it is a little under half a witness cadence.
	witnessSaveWindow = 12 * time.Second
)

// The series this scenario reads off the roles' own /metrics.
const (
	witnessAcceptedSeries = `farm_jobrunner_witness_total{outcome="accepted"}`
	witnessRefusedSeries  = `farm_jobrunner_witness_total{outcome="refused"}`
	witnessErrorSeries    = `farm_jobrunner_witness_total{outcome="error"}`
	witnessSkippedSeries  = `farm_jobrunner_witness_total{outcome="skipped"}`
	witnesslessSeries     = `farm_jobrunner_witnessless_total`
	markerOKSeries        = `farm_jobrunner_marker_refreshes_total{outcome="ok"}`
	markerFailedSeries    = `farm_jobrunner_marker_refreshes_total{outcome="failed"}`
	reaperExpiredSeries   = `farm_reaper_leases_ended_total{reason="holder_expired"}`

	// The renewal path's own meters, on the two registers internal/jobrunner
	// writes each outcome to: its own, which counts every ATTEMPT so a failure
	// rate has a denominator, and the fleet-wide one internal/obs owns, which
	// is what DeviceFarmerLeaseFenced is written over.
	//
	// Both matter to this scenario for the same reason the witness series do.
	// It refuses renewals deliberately, for its whole length, and until the
	// hooks were wired the only external record of that was a WARN line: an
	// operator watching /metrics through this outage saw a farm identical to
	// one where nothing was wrong. These are what changed.
	renewalOKSeries         = `farm_jobrunner_renewals_total{outcome="ok"}`
	renewalSelfHealedSeries = `farm_jobrunner_renewals_total{outcome="self_healed"}`
	renewalTransientSeries  = `farm_jobrunner_renewals_total{outcome="transient"}`
	renewalFencedSeries     = `farm_jobrunner_renewals_total{outcome="fenced"}`
	leaseRenewTransient     = `farm_lease_renew_failures_total{kind="transient"}`
	leaseRenewFenced        = `farm_lease_renew_failures_total{kind="fenced"}`
)

func TestWitnessSavesALeaseTheHolderCannotRenew(t *testing.T) {
	f := newFarm(t, farmOpts{
		// The reaper is here because it is the defendant: it is the only loop
		// in the farm that can take a lease from a job that has not asked to
		// give it back, and a scenario that proved a lease survived a farm
		// with no reaper in it would prove nothing at all. It starts LAST so
		// that the three components it watches have written a
		// farm.component_heartbeat row before it arms — an arm that refuses
		// reclaims nothing, and "nothing was reclaimed" would then be a
		// statement about the arm rather than about the witness.
		Roles: []string{"api", "scheduler", "jobrunner", "reaper"},
		Env: map[string]string{
			config.EnvLeaseWitnessInterval: witnessCadence.String(),
			config.EnvLeaseRenewInterval:   witnessRenewCadence.String(),
			config.EnvReaperInterval:       witnessReaperCadence.String(),
		},
	})
	db := f.DB()
	ctx := t.Context()

	// -----------------------------------------------------------------
	// The outage, installed before the job is filed.
	// -----------------------------------------------------------------
	//
	// Before, and not after the placement, so there is no instant in this
	// job's life at which its renewals could have landed: farm.leases
	// .heartbeat_seq stays at the 0 the grant wrote, and heartbeat_at stays at
	// the grant's own now(). A holder that had renewed once would have pushed
	// its deadlines and zeroed witness_extensions, and every number below
	// would be measuring a mixture of the two paths.
	installWitnessRenewalOutage(t, f)

	jobID := f.SubmitJob(t, map[string]any{
		"version": 1,
		"steps": []any{
			// The sleep is the outage's container: everything this scenario
			// does to the farm happens while this step is on the device.
			map[string]any{
				"id": "hold", "kind": "sleep", "timeout": "5m",
				"sleep": map[string]any{"duration": witnessHold.String()},
			},
			// The last word, asked of the device after the outage: the fake
			// answers every shell with the devpath that ran it, so this step's
			// stored output is evidence that the work was still on the SAME
			// physical position at the end. A job that had lost its device and
			// been re-placed would answer with another one.
			map[string]any{
				"id": "probe", "kind": "shell", "timeout": "60s",
				"shell": map[string]any{"command": "getprop ro.build.version.sdk"},
			},
		},
	})

	var leaseID, deviceID string
	var fence int64
	f.Eventually(t, 2*time.Minute, "the scheduler to place the job on a device", func() error {
		return db.QueryRow(ctx,
			`SELECT id::text, device_id::text, fence FROM farm.leases WHERE job_id = $1::uuid`,
			jobID).Scan(&leaseID, &deviceID, &fence)
	})
	host, devpath := f.DevicePosition(t, deviceID)
	t.Logf("job %s placed on %s at %s (lease %s, fence %d)", jobID, host, devpath, leaseID, fence)

	// -----------------------------------------------------------------
	// 1. The witness WRITES.
	// -----------------------------------------------------------------

	f.Eventually(t, 2*witnessCadence+30*time.Second, "the first witness to land on the lease", func() error {
		v, err := readWitnessLease(ctx, f, jobID)
		if err != nil {
			return err
		}
		if v.witnessAt == nil {
			return fmt.Errorf("farm.leases.witness_at is still NULL: %s", v.describe())
		}
		if v.extensions < 1 {
			return fmt.Errorf("witness_at is set but witness_extensions is %d: %s", v.extensions, v.describe())
		}
		return nil
	})

	granted, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the lease after its first witness: %v", err)
	}
	t.Logf("first witness: %s", granted.describe())

	// The preconditions the rest of this file rests on. Each of them is a way
	// the scenario could pass while proving nothing, so each is checked rather
	// than assumed.
	switch {
	case granted.protected:
		// A protected lease is held for a human and never auto-reclaimed, so
		// the reaper would decline it whatever the witness said.
		t.Fatalf("this job's lease is protected, so nothing below would be about the witness: %s",
			granted.describe())
	case granted.heartbeats != 0:
		t.Fatalf("a renewal landed despite the outage (heartbeat_seq=%d); the fault was not installed "+
			"before the grant and this lease's deadlines are not the ones the grant wrote: %s",
			granted.heartbeats, granted.describe())
	case granted.gapRows != 0:
		// farm.lease_reclaim skips any lease whose silence overlaps a recorded
		// control-plane gap. One here would exempt this lease for a reason
		// that has nothing to do with the witness.
		t.Fatalf("%d control-plane gap(s) already shield this lease; the save below would be the "+
			"gap refund's doing, not the witness's: %s", granted.gapRows, granted.describe())
	}

	// The holder has been TRYING. This is the difference between "blind" and
	// "not yet due", and there are now two independent registers of it: the
	// sequence the trigger takes a number from on the server, and the holder's
	// own meter on the client, which internal/jobrunner wires from
	// lease.HolderHooks.
	//
	// The METER is read first and the sequence second, so the comparison below
	// is sound in one direction: the trigger takes its number before it raises,
	// and the holder records the error only after receiving it, so every
	// increment the meter has already published is backed by a refusal the
	// sequence had already issued. Reading them the other way round would let
	// a retry land in between and make the meter look like it over-counted.
	seen := readWitnessCounter(t, f, "jobrunner", renewalTransientSeries)
	refused := witnessBlockedRenewals(t, f)
	if refused < 1 {
		t.Fatalf("the renewal outage has refused %d renewals, with this holder renewing every %s and "+
			"its last accepted beat %s ago; either the holder has not tried yet or the fault is not "+
			"on the renewal path: %s",
			refused, witnessRenewCadence, granted.heartbeatAge.Round(time.Second), granted.describe())
	}
	t.Logf("the renewal path has refused %d attempt(s) so far; heartbeat_seq is still %d at holder_epoch %d",
		refused, granted.heartbeats, granted.holderEpoch)

	// THE ASSERTION THIS UNIT EXISTS FOR. Everything above was already true
	// before the hooks were wired; what was missing was any way for an
	// operator to know it. A holder whose renewals are failing is the exact
	// condition the witness loop exists to survive, and it reached no meter,
	// so a rule written over one returned no data and never fired — which
	// reads identically to a farm on which nothing has gone wrong.
	switch {
	case seen < 1:
		t.Fatalf("%s is %v while the outage has refused %d renewal(s): the holder's renewal "+
			"outcomes reach no metric, so this outage is invisible to everything except the "+
			"jobrunner's own log", renewalTransientSeries, seen, refused)
	case seen > float64(refused):
		// Read meter-then-sequence, so the sequence can only be ahead. A meter
		// ahead of it means something other than this outage is failing the
		// renewal path, and every count below would be measuring a mixture.
		t.Errorf("the holder has recorded %v transient renewal failures but the outage has only "+
			"refused %d; something other than the trigger is failing renewals, so this scenario is "+
			"no longer about one fault", seen, refused)
	}

	// And the classification, which is the one thing here that must never be
	// wrong in either direction. internal/lease reports fencing for ZERO ROWS
	// and for nothing else; this outage raises an exception, so every refusal
	// above is transient by construction. A fenced count here would mean the
	// holder had been told to abort a job that is running perfectly well —
	// DeviceFarmer/STF #663 with the control plane as the trigger — and it
	// would also have paged a human through DeviceFarmerLeaseFenced.
	for _, series := range []string{renewalFencedSeries, leaseRenewFenced} {
		if n := readWitnessCounter(t, f, "jobrunner", series); n != 0 {
			t.Fatalf("%s is %v: a renewal that failed with an ordinary error was recorded as proof "+
				"the lease is gone. The lease is not gone — %s", series, n, granted.describe())
		}
	}
	// The same refusals on the fleet-wide register, which is the one
	// DeviceFarmerLeaseFenced reads and which the jobrunner published nothing
	// to until these hooks existed.
	if n := readWitnessCounter(t, f, "jobrunner", leaseRenewTransient); n < 1 {
		t.Errorf("%s is %v: this replica's holders are failing every renewal and the fleet-wide "+
			"counter has not heard about any of it", leaseRenewTransient, n)
	}
	// Nothing landed, which the server has already said (heartbeat_seq is 0)
	// and the client must agree with. A meter that reported successes here
	// would be counting something other than this lease's renewals.
	for _, series := range []string{renewalOKSeries, renewalSelfHealedSeries} {
		if n := readWitnessCounter(t, f, "jobrunner", series); n != 0 {
			t.Errorf("%s is %v with heartbeat_seq still %d: a renewal was recorded as landing on a "+
				"farm where the renewal path is closed", series, n, granted.heartbeats)
		}
	}

	// -----------------------------------------------------------------
	// 2. Elapsed time, applied by hand. See the file comment.
	// -----------------------------------------------------------------

	// The quiesce gate first. farm.reaper_arm sets quiesce_until to now() plus
	// the longest TTL it could have missed, so a reaper that has just started
	// reclaims nothing for the next fifteen minutes — correct for a control
	// plane coming back, and fatal to a scenario that wants to know what the
	// reaper does with this lease. No operator route opens it (enable ARMS,
	// which sets it again), so it is opened here, and the reaper's own answer
	// to "are you armed" is read back through the API before anything is
	// asserted about what it did not do.
	if _, err := db.Exec(ctx, `UPDATE farm.reaper_state SET quiesce_until = now() WHERE singleton`); err != nil {
		t.Fatalf("opening the reaper's quiesce gate: %v", err)
	}
	state := f.get(t, "operator", "/api/v1/reaper").mustStatus(t, http.StatusOK)
	if armed, _ := state.value(t, "armed").(bool); !armed {
		t.Fatalf("the reaper reports it is not armed, so a lease it does not take proves nothing:\n%s",
			state.text())
	}

	// ttl + grace is the whole life of a lease nobody renews; the extra five
	// minutes put reclaimable_at unambiguously in the past rather than on the
	// boundary. Nothing is invented: this is the state the farm would be in on
	// its own, fifty minutes from now, if this holder never renewed again.
	age := granted.ttl + granted.grace + 5*time.Minute
	backdateForWitness(t, f, leaseID, age, false)

	aged, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the lease after moving its clocks into the past: %v", err)
	}
	if !aged.reclaimableAt.Before(aged.now) {
		t.Fatalf("the backdate left reclaimable_at in the future, so the reaper was never entitled "+
			"to this lease and nothing below is about the witness: %s", aged.describe())
	}
	// The gap check is re-run HERE and not only on the granted row, because
	// moving heartbeat_at fifty minutes into the past is exactly what pulls a
	// gap recorded during start-up into scope: farm.lease_reclaim's clause is
	// `g.ended_at > l.heartbeat_at`, and a gap that ended before the grant
	// ends after a heartbeat that has just been moved behind it.
	if aged.gapRows != 0 {
		t.Fatalf("%d control-plane gap(s) shield this lease now that its heartbeat is %s old; the "+
			"save below would be the gap refund's doing, not the witness's: %s",
			aged.gapRows, aged.heartbeatAge.Round(time.Second), aged.describe())
	}
	t.Logf("moved lease %s %s into the past, leaving witness_at where it is: %s", leaseID, age, aged.describe())

	// The reaper notices, which is itself worth asserting: 'suspect' is the
	// state that says the reaper has this lease in its sights. It releases
	// nothing — a heartbeat would heal it at the same fence — and it is the
	// precondition of farm.lease_reclaim's candidate set.
	f.Eventually(t, 45*time.Second, "the reaper to mark the silent lease suspect", func() error {
		v, err := readWitnessLease(ctx, f, jobID)
		if err != nil {
			return err
		}
		if v.state != "suspect" {
			return fmt.Errorf("lease state is %q: %s", v.state, v.describe())
		}
		return nil
	})

	// -----------------------------------------------------------------
	// 3. THE SAVE. The assertion this whole file exists for.
	// -----------------------------------------------------------------
	//
	// For the length of this window the lease is a member of every clause of
	// farm.lease_reclaim's candidate set except one: it is suspect, it is past
	// its reclaimable_at, it is unprotected, no control-plane gap shields it,
	// and the reaper's gate is open and being swept every few seconds. The one
	// clause it fails is the witness clause — witness_at is younger than one
	// grace period — and the condition below asserts BOTH halves at every
	// poll, because a window in which the lease was never a candidate at all
	// would hold just as well and mean nothing.
	//
	// If this fails with candidate_but_for_witness=false and reclaimable_at in
	// the future, the next witness tick beat the window; that is a scheduling
	// fact about the test, not a farm bug, and the push it represents is
	// asserted immediately below.
	f.Consistently(t, witnessSaveWindow,
		"the reaper to leave a past-due lease its witness still covers", func() error {
			v, err := readWitnessLease(ctx, f, jobID)
			if err != nil {
				return err
			}
			switch {
			case v.rows != 1:
				return fmt.Errorf("job %s has %d lease rows; it was placed again, so something took "+
					"its device away:\n%s", jobID, v.rows, formatLeases(readLeases(t, f, jobID)))
			case v.released:
				return fmt.Errorf("the lease ended: %s", v.describe())
			case v.holderEpoch != granted.holderEpoch:
				// A re-attach refreshes heartbeat_at and both deadlines and
				// zeroes witness_extensions without touching heartbeat_seq, so
				// it would look like a lease the witness was carrying.
				return fmt.Errorf("holder_epoch moved %d -> %d: somebody re-attached to this lease and "+
					"refreshed it, so what happens here is not the witness's doing: %s",
					granted.holderEpoch, v.holderEpoch, v.describe())
			case !v.reaperEnabled:
				return fmt.Errorf("the reaper was switched off under us: %s", v.describe())
			case !v.gateOpen:
				return fmt.Errorf("the reaper's gate closed under us — a quiesce window or a refused "+
					"arm — so this window is no longer evidence about what it declined to do: %s",
					v.describe())
			case !v.candidateButForWitness:
				return fmt.Errorf("this lease is no longer a farm.lease_reclaim candidate on grounds "+
					"other than the witness, so the window proves nothing: %s", v.describe())
			case !v.witnessExempt:
				return fmt.Errorf("the witness clause no longer exempts this lease, and it has not "+
					"been reclaimed either — the reaper is not sweeping: %s", v.describe())
			}
			return nil
		})

	// The reaper's own count of the thing that must never happen. It is
	// pre-seeded at zero in reaper.Collectors precisely so it can be read
	// before the first casualty.
	if n := readWitnessCounter(t, f, "reaper", reaperExpiredSeries); n != 0 {
		t.Fatalf("%s is %v: the reaper ended a lease by expiry during this scenario", reaperExpiredSeries, n)
	}

	// And the same claim in the API's words rather than the test's: the
	// operator endpoint counts reclaim candidates with the predicates
	// farm.lease_reclaim uses, and it says none.
	state = f.get(t, "operator", "/api/v1/reaper").mustStatus(t, http.StatusOK)
	if suspect, _ := state.value(t, "suspect_leases").(float64); suspect != 1 {
		t.Errorf("GET /api/v1/reaper reports %v suspect leases, want the 1 this scenario made:\n%s",
			suspect, state.text())
	}
	if n, _ := state.value(t, "reclaimable_now").(float64); n != 0 {
		t.Errorf("GET /api/v1/reaper reports reclaimable_now=%v; the sweep would take this lease, "+
			"and the witness is the only thing that should be stopping it:\n%s", n, state.text())
	}

	// -----------------------------------------------------------------
	// 4. THE PUSH, which is only visible now.
	// -----------------------------------------------------------------
	//
	// farm.lease_witness sets reclaimable_at = GREATEST(reclaimable_at, now()
	// + grace), so on a lease whose deadlines are still ahead of it the push
	// is a no-op and cannot be told from the grant's own arithmetic. On this
	// one — reclaimable_at five minutes in the past — the GREATEST finally
	// binds, and the next tick moves the deadline from behind the reaper to a
	// full grace band ahead of it. That is the mechanism, and this is the only
	// state in which asserting it means anything.
	//
	// The baseline is read HERE, immediately before the wait, and not carried
	// over from the backdate: the checks between the two took the better part
	// of twenty seconds, and a baseline read before a witness tick that has
	// already landed would let this wait succeed on its first poll without a
	// push ever being observed. Reading it now also re-establishes the
	// precondition — the deadline is still behind the reaper — which is what
	// makes the move that follows a push out of the past rather than a nudge
	// inside the future.
	past, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the lease before waiting for the push: %v", err)
	}
	if !past.reclaimableAt.Before(past.now) {
		t.Fatalf("a witness pushed reclaimable_at before this phase started, so the push cannot be "+
			"observed: %s", past.describe())
	}
	before := past.reclaimableAt
	f.Eventually(t, 2*witnessCadence, "the next witness to push reclaimable_at out of the past", func() error {
		v, err := readWitnessLease(ctx, f, jobID)
		if err != nil {
			return err
		}
		if v.released {
			// The job's sleep step outlasts every phase by design; if it did
			// not, this phase would be waiting on a placement that has ended.
			return fmt.Errorf("the job gave its lease back before this phase ran, so the scenario's "+
				"phases have outgrown its %s sleep step: %s", witnessHold, v.describe())
		}
		if !v.reclaimableAt.After(before) {
			return fmt.Errorf("reclaimable_at has not moved from %s: %s", before.Format(time.RFC3339), v.describe())
		}
		if v.extensions < 2 {
			return fmt.Errorf("reclaimable_at moved but witness_extensions is %d: %s", v.extensions, v.describe())
		}
		return nil
	})

	pushed, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the lease after the push: %v", err)
	}
	t.Logf("push: %s", pushed.describe())
	switch {
	case !pushed.reclaimableAt.After(pushed.now):
		t.Errorf("reclaimable_at %s is still in the past at %s; the witness moved it, but not past "+
			"now(), so the reaper is still entitled to this lease: %s",
			pushed.reclaimableAt.Format(time.RFC3339), pushed.now.Format(time.RFC3339), pushed.describe())
	case pushed.witnessAt == nil:
		t.Errorf("reclaimable_at moved with witness_at NULL; something other than the witness pushed "+
			"this deadline: %s", pushed.describe())
	case pushed.reclaimableAt.Before(pushed.witnessAt.Add(pushed.grace)):
		// The postcondition the reaper depends on: after an accepted witness
		// the lease is covered for a full grace band from the moment the
		// evidence was presented.
		t.Errorf("reclaimable_at is %s but the witness at %s buys a full grace band (%s), so it "+
			"should be at or past %s: %s", pushed.reclaimableAt.Format(time.RFC3339),
			pushed.witnessAt.Format(time.RFC3339), pushed.grace,
			pushed.witnessAt.Add(pushed.grace).Format(time.RFC3339), pushed.describe())
	}
	// Independent of the three above, and the one fact that would make all of
	// them somebody else's work: the two other writers that move a deadline
	// are farm.lease_renew (heartbeat_seq) and the re-attach in
	// farm.lease_acquire (holder_epoch).
	if pushed.heartbeats != 0 || pushed.holderEpoch != granted.holderEpoch {
		t.Errorf("heartbeat_seq is %d and holder_epoch moved %d -> %d: a renewal or a re-attach "+
			"refreshed this lease, so the push above may be its doing and not the witness's: %s",
			pushed.heartbeats, granted.holderEpoch, pushed.holderEpoch, pushed.describe())
	}

	accepted := readWitnessCounter(t, f, "jobrunner", witnessAcceptedSeries)
	if accepted < 2 {
		t.Errorf("%s is %v after two ticks, want at least 2", witnessAcceptedSeries, accepted)
	}

	// -----------------------------------------------------------------
	// 5. THE NEGATIVE CONTROL: the loop judges its evidence.
	// -----------------------------------------------------------------
	//
	// The device is left perfectly healthy — it is on the bus, it answers
	// every other command, and the watchdog would find nothing wrong with it.
	// Only the marker's own writes fail. That separation is the point: the
	// witness is not a health probe, so what must stop it is the loss of OUR
	// evidence and nothing else.
	//
	// Note what the outcome is, and is not. A dead marker is not REFUSED:
	// refusal is the server's verdict — a spent extension cap, a stale fence,
	// a lease that is no longer live — and it is reached with evidence in
	// hand. Evidence that has gone stale produces SKIPPED, which is the loop
	// declining to present proof it does not have. A dead marker that produced
	// an accepted witness would be the loop manufacturing proof; a dead marker
	// that produced a refusal would mean it had presented some.
	// Both counters are baselined BEFORE the fault. A skip that had already
	// happened — a witness tick that fired before the marker's first write
	// landed, which a slow first ADB round trip produces — would otherwise
	// satisfy the wait below on its first poll, and the whole dark period
	// would be asserted over two reads taken milliseconds apart.
	skippedBefore := readWitnessCounter(t, f, "jobrunner", witnessSkippedSeries)

	adb := f.ADB(t, host)
	adb.Inject(fakeadb.Fault{
		Devpath: devpath,
		Match:   runner.MarkerPath,
		Kind:    fakeadb.FaultFail,
		Message: "e2e: the marker path is unwritable on this device",
	})
	t.Logf("the on-device marker at %s is now unwritable on %s; every other command still answers",
		runner.MarkerPath, devpath)

	dark, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the lease as the evidence goes dark: %v", err)
	}
	// One tick to notice, plus the evidence window it takes for the last
	// successful write to age out, plus slack for the round trip.
	f.Eventually(t, witnessCadence+witnessEvidenceWindow+30*time.Second,
		"the witness to skip a tick rather than present stale evidence", func() error {
			v, err := readWitnessLease(ctx, f, jobID)
			if err != nil {
				return err
			}
			if v.released {
				return fmt.Errorf("the job gave its lease back before the marker's evidence went "+
					"stale, so the placement — and its witness loop — ended before this phase could "+
					"observe anything; the scenario's phases have outgrown its %s sleep step: %s",
					witnessHold, v.describe())
			}
			n, err := witnessCounter(t, f, "jobrunner", witnessSkippedSeries)
			if err != nil {
				return err
			}
			if n <= skippedBefore {
				return fmt.Errorf("%s is %v, unmoved from the %v it stood at when the marker died",
					witnessSkippedSeries, n, skippedBefore)
			}
			return nil
		})

	// Nothing was written while it was dark. This is the assertion that
	// separates a loop that judges from a loop that runs.
	silent, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the lease after the skipped tick: %v", err)
	}
	// Separate ifs and not a switch: farm.lease_witness moves witness_at and
	// witness_extensions in the SAME update, so a loop that presented stale
	// evidence trips both, and a reader who is told only about the first
	// cannot tell an accepted witness from one that errored after writing.
	if silent.witnessAt == nil || dark.witnessAt == nil {
		t.Errorf("witness_at went NULL during the dark period, which nothing in the farm does: %s",
			silent.describe())
	} else if silent.witnessAt.After(*dark.witnessAt) {
		t.Errorf("witness_at advanced from %s to %s while the device could not take a marker write: "+
			"the loop presented evidence it did not have: %s", dark.witnessAt.Format(time.RFC3339),
			silent.witnessAt.Format(time.RFC3339), silent.describe())
	}
	if silent.extensions != dark.extensions {
		t.Errorf("witness_extensions moved %d -> %d with no marker on the device: %s",
			dark.extensions, silent.extensions, silent.describe())
	}
	if now := readWitnessCounter(t, f, "jobrunner", witnessAcceptedSeries); now != accepted {
		t.Errorf("%s moved %v -> %v across the dark period; a witness was accepted for a device "+
			"that took no marker write", witnessAcceptedSeries, accepted, now)
	}
	if n := readWitnessCounter(t, f, "jobrunner", witnessRefusedSeries); n != 0 {
		t.Errorf("%s is %v: a stale marker made the loop PRESENT evidence and be turned down, "+
			"rather than present none", witnessRefusedSeries, n)
	}

	// -----------------------------------------------------------------
	// 6. The counterfactual, in the API's own words.
	// -----------------------------------------------------------------
	//
	// The save above is only worth what its counterfactual is worth: would the
	// sweep really have taken this lease? The endpoint that answers that is
	// GET /api/v1/reaper's reclaimable_now, and here it is asked about the
	// same lease, in the same farm, with the evidence gone.
	//
	// The reaper is DISABLED first, through the operator's own route, because
	// the answer must not be acted on: this job is still running on that
	// device, and re-arming a sweep over a lease whose evidence has just been
	// taken away would destroy the very work this file is about. It stays
	// disabled for the rest of the run for the same reason.
	f.post(t, "operator", "/api/v1/reaper/disable", map[string]any{
		"reason": "e2e: reading what the sweep would take, without letting it take it",
	}).mustStatus(t, http.StatusOK)

	// Thirty-five more minutes of the same outage, this time with the device
	// dark for all of it: witness_at moves with the rest, because there is no
	// marker being written any more and evidence that stood still while the
	// world moved would be a lie in the other direction.
	backdateForWitness(t, f, leaseID, silent.grace+5*time.Minute, true)

	// reclaimable_now is a FARM-WIDE census, and this farm has exactly one live
	// lease (asserted throughout as rows == 1), so 1 here is a statement about
	// this one. A larger number would mean the farm grew a lease this scenario
	// does not know about, which is worth failing on for its own sake.
	state = f.get(t, "operator", "/api/v1/reaper").mustStatus(t, http.StatusOK)
	if n, _ := state.value(t, "reclaimable_now").(float64); n != 1 {
		gone, rerr := readWitnessLease(ctx, f, jobID)
		t.Errorf("with the witness gone, GET /api/v1/reaper reports reclaimable_now=%v, want the 1 "+
			"live lease this farm has: the sweep would NOT have taken this lease, so the window "+
			"above did not measure the witness.\nlease: %s (read error: %v)\n%s",
			n, gone.describe(), rerr, state.text())
	} else {
		t.Logf("counterfactual: with witness_at %s old, the same lease is 1 of the sweep's candidates; "+
			"while the witness was landing it was 0 of them", silent.grace+5*time.Minute)
	}

	// The device gets its marker path back, so the placement's tail — the last
	// step, and the marker delete that runs before the device is released —
	// happens on hardware that is not being sabotaged.
	adb.ClearFaults()

	// -----------------------------------------------------------------
	// 7. The job ends, and ends the lease itself.
	// -----------------------------------------------------------------

	var jobState, jobError string
	f.Eventually(t, 3*time.Minute, "the job to reach a terminal state", func() error {
		if err := db.QueryRow(ctx,
			`SELECT state, COALESCE(error,'') FROM farm.jobs WHERE id = $1::uuid`,
			jobID).Scan(&jobState, &jobError); err != nil {
			return err
		}
		switch jobState {
		case "succeeded":
			return nil
		case "failed", "cancelled":
			t.Fatalf("job %s ended %q: %s\nThe outage was on the renewal path only; nothing in it "+
				"should have reached the work.", jobID, jobState, jobError)
		}
		return fmt.Errorf("job is %q", jobState)
	})

	steps := readSteps(t, f, jobID)
	if len(steps) != 2 {
		t.Fatalf("farm.job_steps has %d rows for job %s, want 2:\n%s", len(steps), jobID, formatSteps(steps))
	}
	for _, s := range steps {
		if s.state != "ok" {
			t.Errorf("step %s (%s) is %q: %s\nall steps:\n%s", s.stepID, s.kind, s.state, s.errText, formatSteps(steps))
		}
	}
	// The trailing " ran " matters: devpaths are prefixes of one another
	// ("usb:3-1.1" of "usb:3-1.11"), so a bare Contains would accept a
	// re-placement onto a sibling port on a wider hub as the same device.
	if probe := steps[1]; !strings.Contains(probe.output, devpath+" ran ") {
		t.Errorf("the last step ran somewhere other than %s, so this job did not finish on the device "+
			"the witness was covering.\noutput: %q", devpath, probe.output)
	}

	// The job row is written BEFORE the lease is released — deliberately, so
	// that a crash between the two leaves a finished job holding a device
	// rather than a running job holding none — so "the job succeeded" is not
	// yet "the lease is back".
	f.Eventually(t, 60*time.Second, "the job to give its lease back", func() error {
		v, err := readWitnessLease(ctx, f, jobID)
		if err != nil {
			return err
		}
		if !v.released {
			return fmt.Errorf("the lease is still open: %s", v.describe())
		}
		return nil
	})

	// THE INVARIANT. farm.leases.release_reason has no connectivity value and
	// no control-plane value — the CHECK in migrations/00001_core.sql permits
	// seven reasons and none of them is an outage — so a farm that failed this
	// scenario would not do it by writing a bad reason. It would do it with a
	// SECOND lease row: the first one reclaimed while its holder was blind,
	// the job placed again on another device. That is what is counted here.
	leases := readLeases(t, f, jobID)
	if len(leases) != 1 {
		t.Fatalf("job %s has %d lease rows, want exactly 1. Its device was taken away and given "+
			"back during a control-plane outage it survived on device-side evidence:\n%s",
			jobID, len(leases), formatLeases(leases))
	}
	l := leases[0]
	switch {
	case l.id != leaseID:
		t.Errorf("the job ended on lease %s but was placed on %s; it changed devices mid-run", l.id, leaseID)
	case l.state != "released":
		t.Errorf("lease %s is %q, want \"released\"", l.id, l.state)
	case l.reason != "completed":
		t.Errorf("lease %s ended with release_reason %q, want \"completed\": the job succeeded, so "+
			"anything else here is some other clock ending a lease the job still owned", l.id, l.reason)
	case l.endedBy != "job":
		t.Errorf("farm.lease_ended_by(%q) is %q, want \"job\"", l.reason, l.endedBy)
	}

	// -----------------------------------------------------------------
	// 8. The account, closed on both roles' meters.
	// -----------------------------------------------------------------

	final, err := readWitnessLease(ctx, f, jobID)
	if err != nil {
		t.Fatalf("reading the ended lease: %v", err)
	}
	if final.heartbeats != 0 {
		t.Errorf("heartbeat_seq is %d on the ended lease: the renewal outage did not last the whole "+
			"run, so parts of this scenario were measuring an ordinary lease: %s",
			final.heartbeats, final.describe())
	}
	if final.holderEpoch != granted.holderEpoch {
		t.Errorf("holder_epoch moved %d -> %d: this lease was re-attached to during the run, which "+
			"refreshes its deadlines and zeroes witness_extensions without renewing anything: %s",
			granted.holderEpoch, final.holderEpoch, final.describe())
	}
	if final.extensions < 2 {
		t.Errorf("witness_extensions ended at %d, want at least the 2 accepted ticks. Only "+
			"farm.lease_renew and the re-attach in farm.lease_acquire zero it, and the two checks "+
			"above say neither happened, so nothing else could have reset it: %s",
			final.extensions, final.describe())
	}
	if n := witnessBlockedRenewals(t, f); n <= refused {
		t.Errorf("the renewal outage refused %d attempts by the first witness and %d by the end; "+
			"the holder stopped trying, so the later phases were not an outage", refused, n)
	} else {
		t.Logf("the holder was refused %d renewals across the run and never stopped holding", n)
	}

	// The same account on the holder's own meter. It has to have moved since
	// the first witness for the same reason the sequence does — a holder that
	// stopped trying would make the later phases a quiet lease rather than an
	// outage — and the three other outcomes have to be untouched, because a
	// run in which one renewal landed, or in which one was read as fencing,
	// is not the run every assertion above was written about.
	if now := readWitnessCounter(t, f, "jobrunner", renewalTransientSeries); now <= seen {
		t.Errorf("%s stood at %v by the first witness and %v at the end; the holder's meter stopped "+
			"moving while the outage did not", renewalTransientSeries, seen, now)
	} else {
		t.Logf("the holder recorded %v refused renewals of its own, and one lease that never ended", now)
	}
	for _, series := range []string{
		renewalOKSeries, renewalSelfHealedSeries, renewalFencedSeries, leaseRenewFenced,
	} {
		if n := readWitnessCounter(t, f, "jobrunner", series); n != 0 {
			t.Errorf("%s is %v at the end of a run in which heartbeat_seq never left 0 and the job "+
				"released its own lease with reason %q", series, n, l.reason)
		}
	}

	// The marker's counts are folded in when the placement ends, which is why
	// they are read here and not earlier: they are the record of the dark
	// period, from the other side.
	//
	// And they are WAITED for rather than read. The summary that folds them in
	// runs on a goroutine of its own (internal/jobrunner/witness.go's stop),
	// which the release deliberately does not wait behind — a device must not
	// stay parked on a finished job while a witness round trip finishes — so
	// the lease can be back before the counters have moved. Both series are
	// pre-seeded at zero, so reading them a moment too early reads a real 0
	// and blames the farm for a race in this file.
	f.Eventually(t, 30*time.Second, "the ended placement's marker counts to be folded in", func() error {
		ok, err := witnessCounter(t, f, "jobrunner", markerOKSeries)
		if err != nil {
			return err
		}
		failed, err := witnessCounter(t, f, "jobrunner", markerFailedSeries)
		if err != nil {
			return err
		}
		if ok < 1 {
			return fmt.Errorf("%s is %v: no marker write has been recorded as landing, so no witness "+
				"could have been honest", markerOKSeries, ok)
		}
		if failed < 1 {
			return fmt.Errorf("%s is %v: no marker write has been recorded as failing, so the dark "+
				"period never happened and the skipped tick had some other cause", markerFailedSeries, failed)
		}
		return nil
	})
	if n := readWitnessCounter(t, f, "jobrunner", witnesslessSeries); n != 0 {
		t.Errorf("%s is %v: this placement ran without a witness at all, so the assertions above "+
			"were about some other lease", witnesslessSeries, n)
	}
	if n := readWitnessCounter(t, f, "jobrunner", witnessErrorSeries); n != 0 {
		t.Errorf("%s is %v: a witness round trip failed. It ends nothing, but it means the outage "+
			"reached the witness path as well as the renewal path, and this scenario needs them apart",
			witnessErrorSeries, n)
	}
	if n := readWitnessCounter(t, f, "reaper", reaperExpiredSeries); n != 0 {
		t.Errorf("%s is %v: the reaper ended a lease by expiry in a farm where the only silent "+
			"holder was proving itself on the device", reaperExpiredSeries, n)
	}
}

// ---------------------------------------------------------------------------
// The fault: a renewal path this holder cannot reach.
// ---------------------------------------------------------------------------

// witnessBlockedSeq counts renewals the outage refused. It is a SEQUENCE and
// not a table because nextval is not transactional: the trigger below raises
// immediately after taking a number, and everything else it could have written
// would be rolled back with the statement it aborted.
const witnessBlockedSeq = "farm.e2e_witness_blocked_renewals"

// installWitnessRenewalOutage makes farm.lease_renew fail for every lease in
// this farm, and only farm.lease_renew.
//
// The predicate is heartbeat_seq, which farm.lease_renew is the only writer of
// in the shipped schema: farm.lease_witness does not touch it, and neither does
// farm.lease_release, farm.lease_mark_suspect or farm.reaper_arm's gap refund —
// so the job can still witness, still be marked suspect, and still give its own
// lease back at the end, which is exactly the shape of a holder that is blind on
// one path only.
//
// The re-attach in migrations/00009_reattach_auth.sql is NOT caught by this, and
// deliberately is not: it leaves heartbeat_seq alone and bumps holder_epoch
// instead. It is not a renewal — it is a replacement holder taking the lease
// over — but it does refresh heartbeat_at, both deadlines and
// witness_extensions, so a scenario that read heartbeat_seq alone could mistake
// one for a witness's work. That is why every assertion below reads holder_epoch
// beside it: this placement re-attaches once, before the first witness, and
// nothing may re-attach again after that.
//
// The failure is an ordinary error rather than zero rows, and that distinction
// is the whole reason this is safe to do to a running job: internal/lease
// reports fencing ONLY for zero rows, so the holder classifies this as
// transient, retries on its backoff, and never lets go of the device.
//
// Nothing drops it again. The scratch database goes at teardown, and a fault
// that outlives the scenario cannot reach another one.
func installWitnessRenewalOutage(t *testing.T, f *farm) {
	t.Helper()
	ctx := t.Context()

	// pgx sends one statement per Exec on the extended protocol, so these
	// cannot be a single script.
	for _, stmt := range []string{
		`CREATE SEQUENCE ` + witnessBlockedSeq,

		`CREATE FUNCTION farm.e2e_witness_block_renew() RETURNS trigger
		 LANGUAGE plpgsql AS $fn$
		 BEGIN
		   PERFORM nextval('` + witnessBlockedSeq + `');
		   RAISE EXCEPTION 'e2e: the renewal path is unreachable for this holder';
		   RETURN NEW;
		 END $fn$`,

		`CREATE TRIGGER e2e_witness_renewal_outage
		   BEFORE UPDATE ON farm.leases
		   FOR EACH ROW
		   WHEN (NEW.heartbeat_seq <> OLD.heartbeat_seq)
		   EXECUTE FUNCTION farm.e2e_witness_block_renew()`,
	} {
		if _, err := f.DB().Exec(ctx, stmt); err != nil {
			t.Fatalf("installing the renewal outage: %v\nstatement: %s", err, strings.TrimSpace(stmt))
		}
	}
	t.Log("the renewal path is now closed to every holder in this farm; the witness path is untouched")
}

// witnessBlockedRenewals is how many renewals the outage has refused.
func witnessBlockedRenewals(t *testing.T, f *farm) int64 {
	t.Helper()
	var n int64
	// is_called distinguishes a sequence that has issued its first value from
	// one that has issued none; without it an untouched sequence reads as 1.
	if err := f.DB().QueryRow(t.Context(),
		`SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM `+witnessBlockedSeq).Scan(&n); err != nil {
		t.Fatalf("counting the renewals the outage refused: %v", err)
	}
	return n
}

// backdateForWitness moves a lease's clocks into the past to stand in for
// elapsed time, optionally including its witness.
//
// It is the same technique, for the same reason, as backdateLease in
// internal/lease/store_test.go: the leases_guard trigger refuses to let a
// deadline move backwards — that refusal is what stops an ordinary heartbeat
// erasing a control-plane-gap refund — so the guard is switched off for the
// length of one transaction. The ACCESS EXCLUSIVE lock ALTER TABLE takes is
// held until commit, so no role can observe farm.leases without its guard, and
// the roles that are mid-statement simply wait the few milliseconds out.
//
// acquired_at and heartbeat_at move with the deadlines. A lease whose
// reclaimable_at is in the past but whose heartbeat is fresh is a state the
// server never produces, and asserting anything about the reaper's treatment of
// an impossible row would be worthless.
//
// withWitness is the difference between the two things this scenario needs to
// simulate: an outage during which the device kept being touched (false — the
// evidence is real and current, and must not be aged), and one during which it
// went dark (true).
func backdateForWitness(t *testing.T, f *farm, leaseID string, by time.Duration, withWitness bool) {
	t.Helper()
	ctx := t.Context()

	tx, err := f.DB().Begin(ctx)
	if err != nil {
		t.Fatalf("begin, to move lease %s %s into the past: %v", leaseID, by, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ALTER TABLE takes ACCESS EXCLUSIVE, and four role processes are reading
	// and writing farm.leases the whole time — the reaper every few seconds,
	// the holder and the witness on their own cadences. A bounded wait keeps
	// this from turning into a stall that ends in the go-test deadline with no
	// explanation, and keeps the queue behind the lock short enough not to
	// time out a witness round trip, which the scenario counts as an error.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatalf("setting a lock timeout for the backdate: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE farm.leases DISABLE TRIGGER leases_guard`); err != nil {
		t.Fatalf("disabling the leases_guard trigger: %v\nALTER TABLE ... DISABLE TRIGGER needs "+
			"ownership of farm.leases and an ACCESS EXCLUSIVE lock on it, and migration 00002 must "+
			"still name the trigger leases_guard", err)
	}
	tag, err := tx.Exec(ctx, `
UPDATE farm.leases
   SET acquired_at    = acquired_at    - make_interval(secs => $2::bigint),
       heartbeat_at   = heartbeat_at   - make_interval(secs => $2::bigint),
       expires_at     = expires_at     - make_interval(secs => $2::bigint),
       reclaimable_at = reclaimable_at - make_interval(secs => $2::bigint),
       witness_at     = CASE WHEN $3::boolean
                             THEN witness_at - make_interval(secs => $2::bigint)
                             ELSE witness_at END
 WHERE id = $1::uuid`, leaseID, int64(by.Seconds()), withWitness)
	if err != nil {
		t.Fatalf("moving lease %s %s into the past: %v", leaseID, by, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("moving lease %s into the past matched %d rows, want 1", leaseID, tag.RowsAffected())
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE farm.leases ENABLE TRIGGER leases_guard`); err != nil {
		t.Fatalf("re-enabling the leases_guard trigger: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the backdate of lease %s: %v", leaseID, err)
	}
}

// ---------------------------------------------------------------------------
// Reading the lease the way the reaper reads it
// ---------------------------------------------------------------------------

// witnessLease is one job's lease, plus farm.lease_reclaim's own candidate
// predicates evaluated against the same now() as the rest of the row.
//
// The predicates are split in two on purpose. "This lease was not reclaimed"
// is worth nothing on its own — a lease with a deadline in the future is not
// reclaimed either — so the interesting claim is the conjunction:
// candidateButForWitness says every OTHER clause of the sweep's candidate set
// holds, and witnessExempt says the witness clause is what is standing in the
// way.
type witnessLease struct {
	rows      int
	id        string
	state     string
	reason    string
	released  bool
	protected bool

	fence       int64
	heartbeats  int64
	holderEpoch int64
	extensions  int

	witnessAt     *time.Time
	reclaimableAt time.Time
	expiresAt     time.Time
	now           time.Time

	ttl   time.Duration
	grace time.Duration

	heartbeatAge time.Duration

	witnessExempt          bool
	candidateButForWitness bool
	gapRows                int

	reaperEnabled bool
	gateOpen      bool
}

// witnessLeaseQuery answers everything about one lease against a single now().
// Splitting it would let the deadline and the predicate that reads it come from
// different transaction timestamps, which is the one thing a scenario about
// clocks must not allow.
const witnessLeaseQuery = `
SELECT (SELECT count(*)::int FROM farm.leases WHERE job_id = $1::uuid),
       l.id::text, l.state, COALESCE(l.release_reason,''),
       (l.released_at IS NOT NULL), l.protected,
       l.fence, l.heartbeat_seq, l.holder_epoch, l.witness_extensions,
       l.witness_at, l.reclaimable_at, l.expires_at, now(),
       extract(epoch FROM l.ttl)::float8,
       extract(epoch FROM l.grace)::float8,
       extract(epoch FROM now() - l.heartbeat_at)::float8,
       (l.witness_at IS NOT NULL AND l.witness_at >= now() - l.grace),
       (    l.state = 'suspect'
        AND l.reclaimable_at < now()
        AND l.protected = false
        AND NOT EXISTS (SELECT 1 FROM farm.control_plane_gap g
                         WHERE g.ended_at > l.heartbeat_at
                           AND g.ended_at > now() - interval '6 hours')),
       (SELECT count(*)::int FROM farm.control_plane_gap g
         WHERE g.ended_at > l.heartbeat_at AND g.ended_at > now() - interval '6 hours'),
       r.enabled,
       -- farm.lease_reclaim's own gate, all three terms of it
       -- (migrations/00012_reaper_arm_unbeaten.sql): a refusal standing
       -- against the last arm stops the sweep as surely as the switch does.
       (r.enabled AND r.quiesce_until <= now() AND r.last_refusal_at IS NULL)
  FROM farm.leases l, farm.reaper_state r
 WHERE l.job_id = $1::uuid AND r.singleton
 ORDER BY l.acquired_at
 LIMIT 1`

// readWitnessLease returns the job's oldest lease row. It reports a missing row
// as an error rather than failing the test, because every caller is inside a
// wait that has something better to say about it.
func readWitnessLease(ctx context.Context, f *farm, jobID string) (witnessLease, error) {
	var v witnessLease
	var ttlSecs, graceSecs, beatSecs float64
	err := f.DB().QueryRow(ctx, witnessLeaseQuery, jobID).Scan(
		&v.rows, &v.id, &v.state, &v.reason, &v.released, &v.protected,
		&v.fence, &v.heartbeats, &v.holderEpoch, &v.extensions,
		&v.witnessAt, &v.reclaimableAt, &v.expiresAt, &v.now,
		&ttlSecs, &graceSecs, &beatSecs,
		&v.witnessExempt, &v.candidateButForWitness, &v.gapRows,
		&v.reaperEnabled, &v.gateOpen)
	if err != nil {
		return witnessLease{}, fmt.Errorf("reading the lease of job %s: %w", jobID, err)
	}
	v.ttl = time.Duration(ttlSecs * float64(time.Second))
	v.grace = time.Duration(graceSecs * float64(time.Second))
	v.heartbeatAge = time.Duration(beatSecs * float64(time.Second))
	return v, nil
}

// describe quotes the whole row. Every assertion in this file fails with it,
// because "the lease was reclaimed" without the six numbers that decided it
// sends the reader back to reproduce the run.
func (v witnessLease) describe() string {
	witness := "NULL"
	if v.witnessAt != nil {
		witness = fmt.Sprintf("%s (%s old)", v.witnessAt.Format(time.RFC3339),
			v.now.Sub(*v.witnessAt).Round(time.Second))
	}
	return fmt.Sprintf(
		"lease %s rows=%d state=%s reason=%q released=%t protected=%t fence=%d "+
			"heartbeat_seq=%d holder_epoch=%d (last beat %s ago) witness_at=%s witness_extensions=%d "+
			"reclaimable_at=%s (%s from now) expires_at=%s ttl=%s grace=%s "+
			"witness_exempt=%t candidate_but_for_witness=%t shielding_gaps=%d "+
			"reaper_enabled=%t gate_open=%t",
		v.id, v.rows, v.state, v.reason, v.released, v.protected, v.fence,
		v.heartbeats, v.holderEpoch, v.heartbeatAge.Round(time.Second), witness, v.extensions,
		v.reclaimableAt.Format(time.RFC3339), v.reclaimableAt.Sub(v.now).Round(time.Second),
		v.expiresAt.Format(time.RFC3339), v.ttl, v.grace,
		v.witnessExempt, v.candidateButForWitness, v.gapRows, v.reaperEnabled, v.gateOpen)
}

// ---------------------------------------------------------------------------
// Counters
// ---------------------------------------------------------------------------

// readWitnessCounter scrapes one role's own /metrics and returns one series,
// failing the test if it cannot.
//
// A missing series is a failure and not a zero. Every series this file reads is
// pre-seeded at zero by its package's Collectors — that is what makes
// "increase(...) > 0" an alert an operator can arm before the first casualty —
// so a series that is absent means it was renamed, and answering 0 would let
// this scenario pass on a farm whose witness counters had stopped existing.
func readWitnessCounter(t *testing.T, f *farm, role, series string) float64 {
	t.Helper()
	v, err := witnessCounter(t, f, role, series)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return v
}

// witnessCounter is the same read, reporting failure as an error.
//
// It exists because two of the waits in this file poll a counter, and f.Metrics
// fails the test on any scrape error — correct for a one-shot assertion, wrong
// inside a loop whose whole job is to keep asking. One refused connection while
// a role is busy would end the scenario instead of being retried on the next
// poll. It scrapes the address the harness recorded, so it is the same listener
// f.Metrics reads; only the failure handling differs.
func witnessCounter(t *testing.T, f *farm, role, series string) (float64, error) {
	t.Helper()
	addr, ok := f.metricsAddr[role]
	if !ok {
		t.Fatalf("this farm has no %q role; it was built with %v", role, f.opts.Roles)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return 0, fmt.Errorf("building the %s metrics request: %w", role, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("scraping the %s role at %s: %w", role, addr, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, fmt.Errorf("reading the %s role's metrics: %w", role, err)
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s/metrics = %d, want 200", addr, res.StatusCode)
	}

	metrics := string(body)
	for _, line := range strings.Split(metrics, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(line, series)
		if !ok || !strings.HasPrefix(rest, " ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return 0, fmt.Errorf("the %s role exposes %s as %q, which is not a number",
				role, series, strings.TrimSpace(rest))
		}
		return v, nil
	}
	return 0, fmt.Errorf("the %s role's /metrics has no series %s; it is pre-seeded at zero by that "+
		"package's Collectors, so its absence means it was renamed:\n%s",
		role, series, firstLines(metrics, 60))
}
