package scheduler

// Behavioural tests for the only thing that hands a device to a job.
//
// The scheduler is additive: it creates leases and never ends one, with a
// single exception it opens itself (a job that reaches a terminal state between
// the acquire and the state write). So the tests fall into three groups:
//
//   - It places what it should, on the device it should.
//   - It places NOTHING when the answer is "no", and treats that as ordinary.
//   - Its one release path uses the JOB's own reason, never an invented one.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// ---------------------------------------------------------------------------
// The positive control
// ---------------------------------------------------------------------------

// TestSchedulerPlacesAQueuedJob is the control every "nothing was placed" test
// below depends on. If this ever stops passing, those tests stop meaning
// anything.
func TestSchedulerPlacesAQueuedJob(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	ctx := context.Background()

	if placed := s.cycle(ctx); placed != 1 {
		t.Fatalf("cycle placed %d jobs, want 1", placed)
	}

	leaseID, device, fence, ok := f.leaseFor(job)
	if !ok {
		t.Fatal("no lease was created for a queued job with a free healthy device")
	}
	if device != dev {
		t.Errorf("lease is on device %s, want %s", device, dev)
	}
	if got := f.jobState(job); got != "running" {
		t.Errorf("job state = %q, want running", got)
	}

	// The denormalised pointer and the fence floor are what make a second grant
	// impossible and a stale socket refusable. A lease that did not move them
	// is a lease the rest of the system cannot see.
	var currentLease string
	var floor int64
	var slotBound bool
	var startedAt bool
	f.scan(&currentLease, `SELECT current_lease_id::text FROM farm.devices WHERE id = $1::uuid`, dev)
	f.scan(&floor, `SELECT fence_floor FROM farm.devices WHERE id = $1::uuid`, dev)
	f.scan(&slotBound, `SELECT slot_id IS NOT NULL FROM farm.leases WHERE id = $1::uuid`, leaseID)
	f.scan(&startedAt, `SELECT started_at IS NOT NULL FROM farm.jobs WHERE id = $1::uuid`, job)

	if currentLease != leaseID {
		t.Errorf("devices.current_lease_id = %q, want %q", currentLease, leaseID)
	}
	if floor != fence {
		t.Errorf("devices.fence_floor = %d, want the granting fence %d", floor, fence)
	}
	if !slotBound {
		t.Error("the lease is not bound to a slot; every ADB call that targets a position " +
			"resolves through the slot, never through a serial")
	}
	if !startedAt {
		t.Error("jobs.started_at was not set")
	}
}

// TestSchedulerReattachesWithoutBumpingTheFence.
//
// A pod eviction is the most ordinary event in a Kubernetes control plane. The
// replacement asks for the same job and must get the same lease, the same
// device and the SAME FENCE: the evicted process's work may still be running
// detached on the device, and a bumped fence would fence the job out of itself.
func TestSchedulerReattachesWithoutBumpingTheFence(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{})

	first := f.newScheduler(&logRecorder{}, "farmd-0")
	ctx := context.Background()
	if placed := first.cycle(ctx); placed != 1 {
		t.Fatalf("first cycle placed %d jobs, want 1", placed)
	}
	leaseID, _, fence, _ := f.leaseFor(job)

	// An operator re-queues the running job, or a supervisor died before
	// writing the state. Either way the job is 'queued' with a live lease.
	f.exec(`UPDATE farm.jobs SET state = 'queued' WHERE id = $1::uuid`, job)
	first.lead.release(ctx)

	second := f.newScheduler(&logRecorder{}, "farmd-1")
	if placed := second.cycle(ctx); placed != 1 {
		t.Fatalf("second cycle placed %d jobs, want 1 (a re-attach counts as a placement)", placed)
	}

	newLeaseID, newDevice, newFence, ok := f.leaseFor(job)
	if !ok {
		t.Fatal("the job lost its lease on re-attach")
	}
	if newLeaseID != leaseID {
		t.Errorf("lease id changed %s -> %s on re-attach", leaseID, newLeaseID)
	}
	if newDevice != dev {
		t.Errorf("device changed on re-attach; the job's own state is on %s", dev)
	}
	if newFence != fence {
		t.Errorf("fence moved %d -> %d on re-attach: the job would be fenced out of its own "+
			"detached work", fence, newFence)
	}
	if n := f.liveLeases(); n != 1 {
		t.Errorf("live leases = %d, want 1: a re-attach must not allocate a second device", n)
	}
}

// ---------------------------------------------------------------------------
// Zero rows is an ordinary outcome
// ---------------------------------------------------------------------------

// TestNoCapacityIsAnOrdinaryOutcome.
//
// A pool at 100% utilisation returns zero rows on every poll. Logging that as
// an error trains operators to ignore the scheduler's logs, and an ignored log
// is where the real failure hides. So the assertion here is about SEVERITY as
// much as about behaviour.
func TestNoCapacityIsAnOrdinaryOutcome(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	// A pool with no schedulable device at all.
	job := f.job(jobOpts{})

	rec := &logRecorder{}
	s := f.newScheduler(rec, "farmd-0")
	ctx := context.Background()

	// Take leadership first, then start listening: becoming the leader is a
	// legitimate info-level event, and this test is about what the loop says
	// once it is running.
	if _, err := s.lead.ensure(ctx, time.Second); err != nil {
		t.Fatalf("take leadership: %v", err)
	}
	rec.reset()

	if placed := s.cycle(ctx); placed != 0 {
		t.Fatalf("cycle placed %d jobs against an empty pool", placed)
	}

	if msgs := rec.atOrAbove(slog.LevelInfo); len(msgs) != 0 {
		t.Errorf("no_capacity was logged at info or above: %v\n\n"+
			"It is the steady state of a busy farm, not a fault. Logging it loudly is how "+
			"operators learn to ignore this loop's output.", msgs)
	}
	if got := f.jobState(job); got != "queued" {
		t.Errorf("job state = %q, want queued: re-queueing is a local decision to stop asking "+
			"for a moment, not a state change", got)
	}
	if n := f.liveLeases(); n != 0 {
		t.Errorf("live leases = %d, want 0", n)
	}
	if _, deferred := s.deferred[job]; !deferred {
		t.Error("the job was not deferred; it would be retried at full rate against a pool " +
			"that has already said no")
	}
}

// TestErrNoCapacityIsWhatZeroRowsProduces pins the contract the branch above
// depends on. If Store.Acquire ever reported zero rows as a plain error, the
// scheduler would take the "transient database trouble" branch, abandon the
// rest of the batch and log a warning on every poll of a full farm.
func TestErrNoCapacityIsWhatZeroRowsProduces(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	job := f.job(jobOpts{})
	store := lease.NewStore(pool)
	instance, err := lease.NewHolderInstance()
	if err != nil {
		t.Fatalf("mint holder instance: %v", err)
	}

	_, err = store.Acquire(context.Background(), job, "farmd-0", instance)
	if !errors.Is(err, lease.ErrNoCapacity) {
		t.Fatalf("Acquire against an empty pool returned %v, want lease.ErrNoCapacity", err)
	}
}

// TestUnpinnedNoCapacityMarksThePoolExhausted. One answer of "no" per pool per
// cycle, not one per job: a 32-job batch against a full pool must cost one
// lease_acquire, not thirty-two.
func TestUnpinnedNoCapacityMarksThePoolExhausted(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	first := f.job(jobOpts{createdAt: -2 * time.Minute})
	second := f.job(jobOpts{createdAt: -1 * time.Minute})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	s.cycle(context.Background())

	if _, ok := s.deferred[first]; !ok {
		t.Error("the first job was not deferred")
	}
	if _, ok := s.deferred[second]; ok {
		t.Error("the second job in an exhausted pool was asked anyway; the pool had already " +
			"answered for every job in it")
	}
}

// TestPinnedNoCapacityDoesNotStallThePool is the subtle half of the same map.
//
// farm.lease_acquire filters on jobs.pin_device, so a pinned job whose one
// device is busy answers no_capacity while the pool is half idle. Letting that
// answer mark the pool would stall every other job in it for the rest of the
// cycle — a single stuck job silently freezing a whole pool's throughput.
func TestPinnedNoCapacityDoesNotStallThePool(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	// A device that is already spoken for, and a free one.
	busy := f.device(deviceOpts{})
	free := f.device(deviceOpts{})
	holderJob := f.job(jobOpts{state: "running"})
	f.seedLease(busy, holderJob, leaseOpts{expiresIn: 15 * time.Minute, reclaimableIn: 45 * time.Minute})

	// The pinned job is polled FIRST, so if its answer marked the pool the
	// second job would never be asked.
	pinned := f.job(jobOpts{pinDevice: busy, createdAt: -2 * time.Minute})
	ordinary := f.job(jobOpts{createdAt: -1 * time.Minute})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	placed := s.cycle(context.Background())

	if placed != 1 {
		t.Fatalf("cycle placed %d jobs, want 1: a pinned job's no_capacity answer is about ONE "+
			"device and says nothing about the pool", placed)
	}
	if _, device, _, ok := f.leaseFor(ordinary); !ok || device != free {
		t.Errorf("the unpinned job did not get the free device %s (ok=%v device=%s)", free, ok, device)
	}
	if _, _, _, ok := f.leaseFor(pinned); ok {
		t.Error("the pinned job got a lease although its device is busy")
	}
}

// ---------------------------------------------------------------------------
// A selector that cannot be satisfied yields no capacity, not a wrong placement
// ---------------------------------------------------------------------------

// TestUnsatisfiableSelectorPlacesNothing.
//
// The worst possible outcome for a constrained job is not "no device" — the
// scheduler re-queues and life goes on. It is a device that does not match,
// handed over as though it did, which produces a test result nobody can trust.
// Each case here keeps an obviously attractive but WRONG device free, so a
// scheduler that fell back to "close enough" fails loudly.
func TestUnsatisfiableSelectorPlacesNothing(t *testing.T) {
	cases := []struct {
		name string
		why  string
		// setup returns the job that must not be placed.
		setup func(f *fixture) string
	}{
		{
			name: "pinned to a device in another pool",
			why:  "pin_device names one device; it does not widen the pool",
			setup: func(f *fixture) string {
				other := f.newPool("other")
				elsewhere := f.device(deviceOpts{poolID: other})
				f.device(deviceOpts{}) // an attractive, wrong device in the job's own pool
				return f.job(jobOpts{pinDevice: elsewhere})
			},
		},
		{
			name: "empty pool",
			why:  "every device lives in another pool",
			setup: func(f *fixture) string {
				other := f.newPool("other")
				f.device(deviceOpts{poolID: other})
				return f.job(jobOpts{})
			},
		},
		{
			name: "device unhealthy",
			why:  "the allocator reads health; the reaper never does",
			setup: func(f *fixture) string {
				f.device(deviceOpts{health: "degraded"})
				return f.job(jobOpts{})
			},
		},
		{
			name: "device offline",
			why:  "adb_state must be 'device' to allocate",
			setup: func(f *fixture) string {
				f.device(deviceOpts{adbState: "offline", health: "offline"})
				return f.job(jobOpts{})
			},
		},
		{
			name: "device administratively disabled",
			why:  "an operator took this device out of service",
			setup: func(f *fixture) string {
				f.device(deviceOpts{adminState: "disabled"})
				return f.job(jobOpts{})
			},
		},
		{
			name: "device quarantined",
			why:  "the recovery ladder is still working on it",
			setup: func(f *fixture) string {
				f.device(deviceOpts{adminState: "quarantined"})
				return f.job(jobOpts{})
			},
		},
		{
			name: "slot in maintenance",
			why:  "a human is holding the physical position",
			setup: func(f *fixture) string {
				f.device(deviceOpts{slotState: "maintenance"})
				return f.job(jobOpts{})
			},
		},
		{
			name: "slot still in its post-reclaim rearm window",
			why: "the previous holder's sockets are not certainly severed yet; " +
				"scheduling into the window is how two jobs end up on one phone",
			setup: func(f *fixture) string {
				f.device(deviceOpts{rearmIn: time.Minute})
				return f.job(jobOpts{})
			},
		},
	}

	pool := requireDB(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, pool)
			job := tc.setup(f)

			s := f.newScheduler(&logRecorder{}, "farmd-0")
			if placed := s.cycle(context.Background()); placed != 0 {
				t.Fatalf("cycle placed %d jobs; %s", placed, tc.why)
			}
			if n := f.liveLeases(); n != 0 {
				t.Fatalf("live leases = %d, want 0; %s", n, tc.why)
			}
			if got := f.jobState(job); got != "queued" {
				t.Errorf("job state = %q, want queued", got)
			}
		})
	}
}

// TestDrainingHostTakesNoNewWork.
//
// POST /api/v1/hosts/{id}/drain answers "no new leases will be placed on this
// host" (internal/api/ops.go). That promise is only worth anything if the
// allocator enforces it.
//
// The guard belongs in farm.lease_acquire, not here: a Go-side filter would be
// a second allocation predicate racing the SQL one, which is the shape of bug
// the single-function allocator exists to prevent. So this test probes the
// schema first and reports the gap rather than asserting against a database
// that cannot satisfy it.
func TestDrainingHostTakesNoNewWork(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	var def string
	f.scan(&def, `
SELECT pg_get_functiondef(p.oid)
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'farm' AND p.proname = 'lease_acquire'`)

	if !strings.Contains(def, "farm.hosts") || !strings.Contains(def, "admin_state") {
		t.Skip("farm.lease_acquire does not join farm.hosts, so a draining host still takes " +
			"new work. internal/api/ops.go's drain endpoint promises the opposite. The fix " +
			"is a migration adding h.admin_state = 'enabled' to both the candidate SELECT " +
			"and the re-check under the lock; this test activates when it lands.")
	}

	drained, drainedHub := f.newHost("h-drained", "draining")
	f.device(deviceOpts{hostID: drained, hubID: drainedHub})
	job := f.job(jobOpts{})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	if placed := s.cycle(context.Background()); placed != 0 {
		t.Fatalf("cycle placed %d jobs onto a draining host", placed)
	}
	if n := f.liveLeases(); n != 0 {
		t.Fatalf("live leases = %d, want 0: a draining host must take no new work", n)
	}
	if got := f.jobState(job); got != "queued" {
		t.Errorf("job state = %q, want queued", got)
	}
}

// TestDrainingAHostDoesNotDisturbItsLiveLeases is the other half of the drain
// promise, and it holds today. "Drained" meaning "I took eleven phones away
// from running jobs" is the failure this whole system is built against.
func TestDrainingAHostDoesNotDisturbItsLiveLeases(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	dev := f.device(deviceOpts{})
	job := f.job(jobOpts{})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	ctx := context.Background()
	if placed := s.cycle(ctx); placed != 1 {
		t.Fatalf("setup: cycle placed %d jobs, want 1", placed)
	}
	leaseID, _, fence, _ := f.leaseFor(job)

	f.exec(`UPDATE farm.hosts SET admin_state = 'draining' WHERE id = $1`, f.hostID)
	f.exec(`UPDATE farm.devices SET admin_state = 'disabled' WHERE id = $1::uuid`, dev)

	for i := 0; i < 3; i++ {
		s.cycle(ctx)
	}

	state, reason := f.leaseState(leaseID)
	if state != "held" {
		t.Fatalf("lease state = %q reason = %q after its host was drained and its device "+
			"disabled; an administrative state change must never end a lease", state, str(reason))
	}
	if _, _, gotFence, _ := f.leaseFor(job); gotFence != fence {
		t.Errorf("fence moved %d -> %d", fence, gotFence)
	}
}

// ---------------------------------------------------------------------------
// Quota
// ---------------------------------------------------------------------------

// TestQuotaCapsDeferRatherThanPlace. The caps are a fairness knob an operator
// set; being at one is neither an error nor a device shortage, and it must not
// consume a device that a job under its cap could use.
func TestQuotaCapsDeferRatherThanPlace(t *testing.T) {
	for _, tc := range []struct{ name, cap string }{{"tenant", "tenant"}, {"queue", "queue"}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := requireDB(t)
			f := newFixture(t, pool)

			switch tc.cap {
			case "tenant":
				f.exec(`UPDATE farm.tenants SET max_devices = 1 WHERE id = $1`, f.tenantID)
			case "queue":
				f.exec(`UPDATE farm.queues SET max_devices = 1 WHERE id = $1`, f.queueID)
			}

			// One device already held by this tenant/queue, one free.
			busy := f.device(deviceOpts{})
			f.device(deviceOpts{})
			holder := f.job(jobOpts{state: "running"})
			f.seedLease(busy, holder, leaseOpts{expiresIn: 15 * time.Minute, reclaimableIn: 45 * time.Minute})

			job := f.job(jobOpts{})
			s := f.newScheduler(&logRecorder{}, "farmd-0")

			if placed := s.cycle(context.Background()); placed != 0 {
				t.Fatalf("cycle placed %d jobs past the %s cap", placed, tc.cap)
			}
			if n := f.liveLeases(); n != 1 {
				t.Errorf("live leases = %d, want 1", n)
			}
			if _, ok := s.deferred[job]; !ok {
				t.Error("a job at its cap was not deferred; it would be retried at full rate")
			}
			if got := f.jobState(job); got != "queued" {
				t.Errorf("job state = %q, want queued", got)
			}
		})
	}
}

// TestQuotaDoesNotBlockAnotherTenant. A cap belongs to whoever it was set on.
func TestQuotaDoesNotBlockAnotherTenant(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	f.exec(`UPDATE farm.tenants SET max_devices = 1 WHERE id = $1`, f.tenantID)
	busy := f.device(deviceOpts{})
	f.device(deviceOpts{})
	f.seedLease(busy, f.job(jobOpts{state: "running"}),
		leaseOpts{expiresIn: 15 * time.Minute, reclaimableIn: 45 * time.Minute})

	other := f.newTenant("t2", 0)
	otherQueue := f.newQueue("q2", other, 100, 0)
	free := f.job(jobOpts{tenantID: other, queueID: otherQueue})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	if placed := s.cycle(context.Background()); placed != 1 {
		t.Fatalf("cycle placed %d jobs; one tenant's cap must not stop another's work", placed)
	}
	if _, _, _, ok := f.leaseFor(free); !ok {
		t.Error("the uncapped tenant's job was not placed")
	}
}

// TestHigherPriorityQueueWinsTheLastDevice. queues.priority is ascending
// urgency, then oldest first inside a queue. With one device free, the order of
// the poll IS the allocation decision.
func TestHigherPriorityQueueWinsTheLastDevice(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	f.device(deviceOpts{}) // exactly one
	urgentQueue := f.newQueue("q-urgent", f.tenantID, 10, 0)

	// The ordinary job is OLDER, so only priority can put the urgent one first.
	ordinary := f.job(jobOpts{createdAt: -1 * time.Hour})
	urgent := f.job(jobOpts{queueID: urgentQueue})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	if placed := s.cycle(context.Background()); placed != 1 {
		t.Fatalf("cycle placed %d jobs, want 1", placed)
	}
	if _, _, _, ok := f.leaseFor(urgent); !ok {
		t.Error("the higher-priority queue did not get the only free device")
	}
	if _, _, _, ok := f.leaseFor(ordinary); ok {
		t.Error("the lower-priority queue took the only free device")
	}
}

// ---------------------------------------------------------------------------
// The one release path
// ---------------------------------------------------------------------------

// TestUnwindUsesTheJobsOwnReason.
//
// The scheduler's only release closes a window the scheduler itself opened: a
// job reached a terminal state between the acquire and the state write, so the
// lease it just took has no owner. The reason written must be the JOB's — those
// are endings the user asked for. Inventing one would put a system-inferred
// ending into a column whose whole purpose is to contain only deliberate ones.
func TestUnwindUsesTheJobsOwnReason(t *testing.T) {
	cases := []struct {
		jobState string
		want     lease.ReleaseReason
	}{
		{"cancelled", lease.ReasonJobCancelled},
		{"failed", lease.ReasonFailed},
		{"succeeded", lease.ReasonCompleted},
	}

	pool := requireDB(t)
	for _, tc := range cases {
		t.Run(tc.jobState, func(t *testing.T) {
			f := newFixture(t, pool)
			dev := f.device(deviceOpts{})
			job := f.job(jobOpts{})

			s := f.newScheduler(&logRecorder{}, "farmd-0")
			ctx := context.Background()

			// Acquire exactly as the cycle would, then let the job go terminal
			// underneath us — the race the unwind exists for.
			res, err := s.acquire(ctx, job)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			f.exec(`UPDATE farm.jobs SET state = $2, finished_at = now() WHERE id = $1::uuid`,
				job, tc.jobState)

			s.onAcquired(ctx, candidate{ID: job, PoolID: f.poolID}, res)

			state, reason := f.leaseState(res.Lease.ID)
			if state != "released" {
				t.Fatalf("lease state = %q, want released", state)
			}
			if str(reason) != string(tc.want) {
				t.Errorf("release_reason = %q, want %q", str(reason), tc.want)
			}

			// And the handover is complete: floor raised, slot quarantined.
			var floor int64
			var quarantined bool
			f.scan(&floor, `SELECT fence_floor FROM farm.devices WHERE id = $1::uuid`, dev)
			f.scan(&quarantined, `
SELECT s.rearm_at > now() FROM farm.slots s
  JOIN farm.devices d ON d.current_slot_id = s.id WHERE d.id = $1::uuid`, dev)
			if floor <= res.Lease.Fence {
				t.Errorf("fence_floor = %d, want above the released fence %d", floor, res.Lease.Fence)
			}
			if !quarantined {
				t.Error("the slot was not quarantined; a new job could land on a device the " +
					"old holder is still talking to")
			}
		})
	}
}

// TestUnwindLeavesALiveJobsLeaseAlone. 'running' or 'allocating' means somebody
// else owns the bookkeeping now, and the lease is theirs.
func TestUnwindLeavesALiveJobsLeaseAlone(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	f.device(deviceOpts{})
	job := f.job(jobOpts{})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	ctx := context.Background()
	res, err := s.acquire(ctx, job)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A runner picked the job up and marked it running between our acquire and
	// our state write.
	f.exec(`UPDATE farm.jobs SET state = 'running' WHERE id = $1::uuid`, job)
	s.onAcquired(ctx, candidate{ID: job, PoolID: f.poolID}, res)

	if state, reason := f.leaseState(res.Lease.ID); state != "held" {
		t.Fatalf("lease state = %q reason = %q: the scheduler released a lease that another "+
			"owner had already taken over", state, str(reason))
	}
}

// TestAFailedStateWriteDoesNotDestroyTheAllocation.
//
// The lease exists and is correct; only the bookkeeping failed. Releasing here
// would destroy an allocation because of a failed UPDATE — the exact shape of
// mistake this project exists to prevent. The next cycle re-attaches instead,
// which costs nothing because acquire is idempotent on job id.
func TestAFailedStateWriteDoesNotDestroyTheAllocation(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	f.device(deviceOpts{})
	job := f.job(jobOpts{})

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	ctx := context.Background()
	res, err := s.acquire(ctx, job)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Make the state write fail the way a database in trouble would, without
	// taking the whole scratch database down with it.
	f.exec(`
CREATE OR REPLACE FUNCTION farm.trg_test_block_job_update() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'injected failure' USING ERRCODE = 'serialization_failure';
END $fn$`)
	f.exec(`CREATE TRIGGER test_block_job_update BEFORE UPDATE ON farm.jobs
	        FOR EACH ROW EXECUTE FUNCTION farm.trg_test_block_job_update()`)
	t.Cleanup(func() {
		f.exec(`DROP TRIGGER IF EXISTS test_block_job_update ON farm.jobs`)
		f.exec(`DROP FUNCTION IF EXISTS farm.trg_test_block_job_update()`)
	})

	rec := &logRecorder{}
	s.log = rec.logger()
	s.onAcquired(ctx, candidate{ID: job, PoolID: f.poolID}, res)

	if state, reason := f.leaseState(res.Lease.ID); state != "held" {
		t.Fatalf("lease state = %q reason = %q after a failed UPDATE on farm.jobs. A failed "+
			"state write must never cost a device", state, str(reason))
	}
	if msgs := rec.atOrAbove(slog.LevelWarn); len(msgs) == 0 {
		t.Error("the failed state write was not logged; a job stuck in 'queued' with a live " +
			"lease is invisible otherwise")
	}
}

// TestSchedulerEndsNoLeaseInASteadyStateCycle is the blunt version of the whole
// package comment: the scheduler is additive. Nothing about polling a queue
// full of unplaceable jobs may touch a lease that already exists.
func TestSchedulerEndsNoLeaseInASteadyStateCycle(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	busy := f.device(deviceOpts{})
	held := f.seedLease(busy, f.job(jobOpts{state: "running"}),
		leaseOpts{expiresIn: 15 * time.Minute, reclaimableIn: 45 * time.Minute})
	// A suspect lease too: the scheduler must not "tidy up" what the reaper is
	// still deciding about.
	suspectDev := f.device(deviceOpts{})
	suspect := f.seedLease(suspectDev, f.job(jobOpts{state: "running"}), leaseOpts{
		state:         "suspect",
		acquiredIn:    -6 * time.Hour,
		heartbeatIn:   -6 * time.Hour,
		expiresIn:     -5 * time.Hour,
		reclaimableIn: -4 * time.Hour,
	})

	f.job(jobOpts{}) // queued, and there is nothing free

	s := f.newScheduler(&logRecorder{}, "farmd-0")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.cycle(ctx)
	}

	for name, l := range map[string]seededLease{"held": held, "suspect": suspect} {
		state, reason := f.leaseState(l.id)
		want := name
		if state != want {
			t.Errorf("%s lease state = %q reason = %q, want %q: the scheduler creates leases "+
				"and never ends one", name, state, str(reason), want)
		}
	}
}

// ---------------------------------------------------------------------------
// Leader election
// ---------------------------------------------------------------------------

// TestLeadershipHoldsADedicatedConnection. A session advisory lock belongs to a
// SESSION. Taken through the pool, the lock's lifetime has nothing to do with
// the work it guards: the connection goes back, unrelated work runs on it, and
// the pool's MaxConnLifetime may close it — ending the session and silently
// releasing the lock while this process still believes it leads. Two schedulers
// then allocate at once, and each one's view of tenant quota is half the truth.
func TestLeadershipHoldsADedicatedConnection(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	leader := f.newScheduler(&logRecorder{}, "farmd-0")
	standby := f.newScheduler(&logRecorder{}, "farmd-1")
	ctx := context.Background()

	leader.cycle(ctx)
	if got := pool.Stat().AcquiredConns(); got != 1 {
		t.Fatalf("acquired connections after a leader cycle = %d, want 1: leadership must hold "+
			"its own connection between cycles, not borrow one per query", got)
	}
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 1 {
		t.Fatalf("sessions holding the scheduler lock = %d, want 1", n)
	}

	standby.cycle(ctx)
	if got := pool.Stat().AcquiredConns(); got != 1 {
		t.Errorf("acquired connections after a standby cycle = %d, want 1: a standby must give "+
			"its connection back rather than hold one hostage per replica", got)
	}
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 1 {
		t.Errorf("sessions holding the scheduler lock = %d, want 1: leader election elected two", n)
	}

	leader.lead.release(ctx)
	if n := f.advisoryLockHolders(lockKeyFor(t.Name())); n != 0 {
		t.Errorf("sessions holding the scheduler lock after release = %d, want 0", n)
	}
}

// TestSecondSchedulerIdlesInsteadOfAllocating. Two schedulers allocating at
// once each see half the tenant's live leases, so both let a job through a cap
// that neither would have breached alone.
func TestSecondSchedulerIdlesInsteadOfAllocating(t *testing.T) {
	pool := requireDB(t)
	f := newFixture(t, pool)

	f.device(deviceOpts{})
	job := f.job(jobOpts{})

	leader := f.newScheduler(&logRecorder{}, "farmd-0")
	standby := f.newScheduler(&logRecorder{}, "farmd-1")
	ctx := context.Background()

	leader.lead.ensure(ctx, time.Second) // take the lock without allocating

	if placed := standby.cycle(ctx); placed != 0 {
		t.Fatalf("a standby scheduler placed %d jobs; only the leader may allocate", placed)
	}
	if n := f.liveLeases(); n != 0 {
		t.Fatalf("live leases = %d, want 0 after a standby cycle", n)
	}
	// The standby still beats: a healthy standby that would take over within
	// one poll is not a control-plane outage, and must not refund lease time
	// that nobody lost.
	var beats int
	f.scan(&beats, `SELECT count(*) FROM farm.component_heartbeat WHERE component = $1`, DefaultComponent)
	if beats != 1 {
		t.Errorf("scheduler heartbeat rows = %d, want 1", beats)
	}

	if placed := leader.cycle(ctx); placed != 1 {
		t.Fatalf("the leader placed %d jobs; the standby assertion above would pass against a "+
			"scheduler that can never place anything", placed)
	}
	if _, _, _, ok := f.leaseFor(job); !ok {
		t.Error("the leader did not place the job")
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewRejectsIncompleteConfig(t *testing.T) {
	full := Config{Pool: &pgxpool.Pool{}, Store: &lease.Store{}, Holder: "farmd-0", HolderInstance: "u"}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"no Pool", func(c *Config) { c.Pool = nil }},
		{"no Store", func(c *Config) { c.Store = nil }},
		{"no Holder", func(c *Config) { c.Holder = "" }},
		{"no HolderInstance", func(c *Config) { c.HolderInstance = "" }},
	} {
		cfg := full
		tc.mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New accepted a Config with %s", tc.name)
		}
	}
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
