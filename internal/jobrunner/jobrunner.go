// Package jobrunner is the loop that turns a placed job into a running one.
//
// The scheduler allocates: it calls farm.lease_acquire and moves a job to
// 'running'. internal/runner executes: given a lease, a device and a spec, it
// runs the steps. Nothing joined the two, so a placed job sat with a device
// held and no work happening. This package is that join, and it owns exactly
// four things: claiming a placed job, attaching to its lease, handing the
// placement to the runner, and putting the lease and the job row back
// afterwards.
//
// # Re-attaching is the mechanism, not an optimisation
//
// This loop calls lease.Store.Acquire with the JOB's id and THIS process's
// holder instance. farm.lease_acquire is idempotent on job_id, so for a job
// the scheduler has already placed that call does not allocate anything: it
// hands back the same lease, on the same device, at the SAME fence, with
// Reattached set. The fence is deliberately not bumped, because the job's own
// work may still be running detached on the device and bumping would fence out
// its own process.
//
// That is what makes a pod eviction cost nothing. Node drain, preemption, spot
// reclaim, cluster upgrade, OOM kill — all ordinary Kubernetes events. The
// replacement replica claims the same job, re-attaches to the same lease, and
// the runner resumes from farm.jobs.checkpoint. The alternative, releasing on
// shutdown, would hand a half-finished six-hour job's device to somebody else.
//
// # What may end a lease here, and what may not
//
// Exactly one thing releases a lease in this package: an [runner.Outcome] that
// names a ReleaseReason. The runner decides that reason from what the JOB did.
// This loop adds no reason of its own from anything it observes about a
// socket, a device or a host, and it cannot: farm.leases.release_reason has no
// connectivity value, so there is no word for it to write.
//
// Two endings are the whole reason for the shape of finish():
//
//   - Outcome.Fenced. We no longer own the lease; somebody else may already be
//     driving the device. Releasing now would take the device from ITS new
//     holder. So nothing is released, nothing is written to the job, and the
//     line is logged loudly.
//   - An empty ReleaseReason — an abandoned attempt, a SIGTERM mid-run. The
//     lease MUST survive: it is what the replacement process re-attaches to.
//
// Fencing is asked about at every release site, and not once at the end,
// because the database cannot answer it for us. A re-attach hands the
// replacement the SAME fence — that is precisely what makes a pod eviction free
// — so a fenced process still presents a fence that MATCHES, and
// farm.lease_release would accept its release and hand the new holder's device
// away in the middle of a run. Every path below that ends a lease or writes a
// verdict therefore asks [lease.Holder.Fenced] first. It is the only thing in
// the system that can tell two placements at one fence apart.
//
// # One runner per job, enforced twice
//
// Two processes driving one phone would interleave shell commands into one
// device and corrupt a run, so the claim is guarded in two independent places:
//
//  1. A session-scoped Postgres advisory lock per job, held on one dedicated
//     connection for the whole run (see claimLocks). It is released the instant
//     the process dies, which is what lets a replacement take over promptly,
//     and it is the guard that actually holds across replicas.
//  2. The poll's own predicate, which skips a job whose current attempt is
//     still open while its lease is being heartbeated. That covers the one case
//     the advisory lock cannot: a claim session that dropped while its job kept
//     running.
//
// The SELECT ... FOR UPDATE SKIP LOCKED in the poll is a third, weaker thing:
// it keeps N replicas polling the same queue from all picking the same rows in
// the same instant. And behind all three sits the database itself — if two
// replicas ever did attach to one job, the second Acquire takes over renewal
// and the first is fenced at its next renewal, which the runner turns into an
// abandoned attempt without another byte written to the device.
package jobrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
	"github.com/flaviopadilha/device-farmer/internal/runner"
)

// Defaults. None of them can end a lease, so the worst a bad value here can do
// is start work slowly.
const (
	// DefaultComponent is the name written to farm.component_heartbeat.
	//
	// It must also appear in FARM_REAPER_COMPONENTS. The reaper's gap
	// computation takes the OLDEST beat across every component on the renewal
	// path, and this loop is on that path: it holds the leases whose deadlines
	// the reaper enforces. A jobrunner outage that is invisible to gap
	// accounting is BLOCKER 8 in migrations/00002_lease.sql — the reaper sees
	// fresh beats from everybody else, records no outage, and reclaims leases
	// whose holders were never given a chance to renew.
	DefaultComponent = "jobrunner"

	// DefaultInterval is the poll period while there is work to start.
	DefaultInterval = 2 * time.Second

	// DefaultIdleInterval is the ceiling the poll backs off to when a cycle
	// starts nothing. A farm whose every device is busy should not ask four
	// times a second whether that is still true.
	DefaultIdleInterval = 15 * time.Second

	// DefaultBatch bounds one poll's candidate list.
	DefaultBatch = 8

	// DefaultConcurrency is how many jobs one replica runs at once. Each one
	// costs a goroutine, a Holder on its own timer and one ADB connection per
	// call in flight.
	DefaultConcurrency = 4

	// DefaultCallTimeout bounds every individual bookkeeping round trip. A
	// wedged statement must not wedge the loop: a loop that stops beating is
	// what the reaper's gap refund exists to notice.
	DefaultCallTimeout = 10 * time.Second

	// DefaultTakeover is how stale a lease's heartbeat must be before this loop
	// will take over a job whose attempt is still open — that is, one that some
	// other process was running and may still be.
	//
	// It is three renewal intervals, so a single missed renewal never invites a
	// takeover, and it is far below the minimum lease TTL of ten minutes, so a
	// replacement resumes long before the lease is even marked suspect.
	// applyDefaults raises it if the configured renewal interval is slower.
	DefaultTakeover = 3 * lease.DefaultRenewInterval

	// DefaultJobBackoff and DefaultJobBackoffMax bound how often one job is
	// retried after this loop failed to START it. They never bound anything
	// inside a run.
	DefaultJobBackoff    = 5 * time.Second
	DefaultJobBackoffMax = 2 * time.Minute
)

// Executor is the part of *runner.Runner this loop uses.
//
// It is an interface for the same reason lease.Holder's store is: the branches
// below that decide whether a lease is released, re-queued or left strictly
// alone are the consequential ones, and a branch that can only be exercised
// with a phone, a hub and an adb server is a branch that never gets exercised.
type Executor interface {
	Run(ctx context.Context, h runner.Holder, p runner.Placement, dev runner.Conn) (runner.Outcome, error)
}

var _ Executor = (*runner.Runner)(nil)

// Dialer builds the device connection for one placement. It is a field so a
// test can drive the whole loop against a fake device.
//
// admission is the placement's fence, and it is here so that the fence reaches
// the wire: on a host whose ADB server sits behind the fence proxy, every
// connection announces it in an admission preamble, and the proxy refuses a
// fence below the device's floor at the socket rather than leaving the refusal
// to the database alone. A Dialer that does not present it will find its
// connections refused on such a host.
type Dialer func(endpoint, devpath string, admission int64) (runner.Conn, error)

// admissionClass is the credential class a job's connections announce to the
// fence proxy: the one class that carries a fence and is bound to one device
// (docs/design/fence-proxy.md, section 7). adbwire cannot name it — its
// vocabulary barrier is what keeps a socket failure unable to reach a lease —
// so the word lives here, beside the fence it accompanies.
const admissionClass = "lease"

// leaseHolder is the part of *lease.Holder that ENDING a lease uses.
//
// It exists for the same reason lease.leaseOps does, and for the same single
// decision: the refusal in releaseLease to release a lease this process was
// fenced out of is the one line standing between a stale process and somebody
// else's running job, and a line that can only be exercised against a live
// Postgres and a real renewal loop is a line that never gets exercised.
//
// Notice what is absent. There is no Acquire, no Renew, no Witness and no
// Stop: nothing reachable through this interface can start a lease, extend one
// or take one over. It can ask whether we still hold the device, and it can
// give one back — which is exactly the pair of facts every release site below
// needs and nothing more.
type leaseHolder interface {
	// Fenced reports whether this process lost the lease. It is the only thing
	// in the system that can tell two placements at one fence apart, because a
	// re-attach hands the replacement the SAME fence.
	Fenced() bool

	// Release ends the lease with a reason the JOB produced.
	Release(ctx context.Context, reason lease.ReleaseReason, rearm time.Duration) (bool, error)
}

var _ leaseHolder = (*lease.Holder)(nil)

// Config is the loop's wiring. Pool, Store, Runner, Holder and HolderInstance
// are required.
type Config struct {
	// Pool is used for job bookkeeping and for the dedicated claim connection,
	// so it must allow at least Concurrency+3 connections: one held for the
	// process lifetime by the claim session, one per running job's renewal, and
	// room for the poll and the runner's own bookkeeping.
	Pool *pgxpool.Pool

	// Store is the binding to the farm.lease_* functions. This loop never
	// writes farm.leases directly; the row locks and partial unique indexes
	// that make a double grant impossible live inside those functions.
	Store *lease.Store

	// Runner executes one placement. Normally *runner.Runner.
	Runner Executor

	// Component is the farm.component_heartbeat key. See DefaultComponent.
	Component string

	// Holder is this process's identity, normally the pod name. AUDIT ONLY: it
	// confers no ownership, which is why a job survives the pod that started it.
	Holder string

	// HolderInstance is a UUID minted once per process with
	// lease.NewHolderInstance. farm.lease_renew matches on it, so re-minting it
	// mid-run would fence this process out of its own leases.
	HolderInstance string

	// HolderConfig configures the renewal loop of every lease this process
	// holds. Its Interval is the clock the Takeover default is derived from.
	HolderConfig lease.HolderConfig

	// Dial builds the device connection. Defaults to one adbwire client per
	// placement, announcing the placement's fence, plus NewDeviceConn.
	Dial Dialer

	// ADBOptions are passed to every adbwire client this loop constructs —
	// adbwire.WithTLS is how a deployment points every job at the fence proxy.
	// Ignored when Dial is supplied.
	ADBOptions []adbwire.Option

	Concurrency int
	Batch       int

	Interval     time.Duration
	IdleInterval time.Duration
	CallTimeout  time.Duration
	Takeover     time.Duration

	JobBackoff    time.Duration
	JobBackoffMax time.Duration

	// SlotRearm is passed to farm.lease_release. It MUST exceed the node
	// proxy's self-fence timeout so the previous holder's sockets are certainly
	// severed before the slot is scheduled again; config.Config.Validate
	// asserts that relationship at startup.
	SlotRearm time.Duration

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.IdleInterval < c.Interval {
		c.IdleInterval = max(DefaultIdleInterval, c.Interval)
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.JobBackoff <= 0 {
		c.JobBackoff = DefaultJobBackoff
	}
	if c.JobBackoffMax < c.JobBackoff {
		c.JobBackoffMax = max(DefaultJobBackoffMax, c.JobBackoff)
	}
	if c.SlotRearm <= 0 {
		c.SlotRearm = lease.DefaultRearm
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	// The takeover window is expressed in renewal intervals rather than in
	// seconds, because what it is really asking is "has this lease missed
	// several renewals?". Deriving it from the interval keeps that question
	// meaningful when an operator slows renewal down.
	interval := c.HolderConfig.Interval
	if interval <= 0 {
		interval = lease.DefaultRenewInterval
	}
	if c.Takeover <= 0 {
		c.Takeover = DefaultTakeover
	}
	if c.Takeover < 3*interval {
		c.Takeover = 3 * interval
	}
}

// JobRunner is the loop. Construct it with New and run it with Run.
type JobRunner struct {
	cfg Config
	log *slog.Logger

	claims *claimLocks

	// mu guards the two maps that decide whether a candidate may be started.
	// Both are local scheduling memory: neither is authoritative about
	// anything, and losing them on restart costs at most one redundant poll.
	mu       sync.Mutex
	busy     map[string]struct{}
	deferred map[string]*deferral

	wg sync.WaitGroup
}

type deferral struct {
	until time.Time
	tries int
}

// New validates cfg and returns a JobRunner.
func New(cfg Config) (*JobRunner, error) {
	if cfg.Pool == nil {
		return nil, errors.New("jobrunner: Config.Pool is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("jobrunner: Config.Store is required")
	}
	if cfg.Runner == nil {
		return nil, errors.New("jobrunner: Config.Runner is required")
	}
	if cfg.Holder == "" {
		return nil, errors.New("jobrunner: Config.Holder is required (the pod name; audit only)")
	}
	if cfg.HolderInstance == "" {
		return nil, errors.New("jobrunner: Config.HolderInstance is required (mint one per process with lease.NewHolderInstance)")
	}
	cfg.applyDefaults()

	jr := &JobRunner{
		cfg:      cfg,
		log:      cfg.Logger.With("component", cfg.Component),
		busy:     make(map[string]struct{}),
		deferred: make(map[string]*deferral),
	}
	jr.claims = &claimLocks{
		pool:    cfg.Pool,
		log:     jr.log,
		timeout: cfg.CallTimeout,
		held:    make(map[string]int64),
	}
	if jr.cfg.Dial == nil {
		jr.cfg.Dial = jr.dial
	}

	// One connection is held for the life of the process by the claim session
	// and never returned, so a pool sized for the poll alone deadlocks the
	// moment the farm is busy: the claim session cannot be built, take() times
	// out, and this replica quietly stops starting work. Warned rather than
	// refused, because Store may be bound to a different pool with its own
	// budget for the renewals — a fact only the caller knows.
	if want := int32(cfg.Concurrency) + 3; cfg.Pool.Config() != nil && cfg.Pool.Config().MaxConns < want {
		jr.log.Warn("the connection pool may be too small for this concurrency; "+
			"one connection is held permanently by the claim session and one per running job renews its lease",
			"max_conns", cfg.Pool.Config().MaxConns, "want_at_least", want, "concurrency", cfg.Concurrency)
	}
	return jr, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation and it releases NOTHING on the way out. A
// SIGTERM is an orderly stop, not a verdict on any job: every in-flight run
// sees the cancellation, abandons its attempt with its checkpoint intact, and
// leaves its lease exactly as it is for the replacement process to re-attach
// to. Run blocks until those runs have finished unwinding, because a process
// that exits mid-unwind leaves attempt rows open forever.
func (jr *JobRunner) Run(ctx context.Context) error {
	jr.log.Info("jobrunner loop starting",
		"interval", jr.cfg.Interval, "idle_interval", jr.cfg.IdleInterval,
		"concurrency", jr.cfg.Concurrency, "batch", jr.cfg.Batch,
		"takeover", jr.cfg.Takeover, "holder", jr.cfg.Holder)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			jr.log.Info("jobrunner loop stopping; in-flight jobs are unwinding and their leases are intentionally untouched",
				"in_flight", jr.inFlight())
			jr.wg.Wait()
			jr.claims.close()
			return nil
		case <-timer.C:
		}

		started := jr.cycle(ctx)

		// Poll fast whenever there is anything to do: a cycle that started work
		// may have more waiting, and a cycle that started none because this
		// replica is already full will have room the moment one of its jobs
		// ends. Only a genuinely idle replica backs off, because only then is
		// the next answer certain to be the same one.
		wait := jr.cfg.Interval
		if started == 0 && jr.inFlight() == 0 {
			wait = jr.cfg.IdleInterval
		}
		timer.Reset(jitter(wait))
	}
}

// cycle polls once and returns how many jobs it started.
func (jr *JobRunner) cycle(ctx context.Context) int {
	cyclesTotal.Inc()

	// Deferred so that the cycles which return early — at capacity, or after a
	// failed poll — still bound the map. Those are exactly the cycles a busy or
	// struggling replica runs most, and they were the ones leaving lapsed
	// entries to accumulate for as long as the process lived.
	defer jr.pruneDeferrals(time.Now())

	// The heartbeat comes first and is unconditional. It answers "was a
	// jobrunner process reachable?", which is the only question the reaper's
	// gap accounting asks of this loop, and it must be answered even by a cycle
	// that goes on to do nothing at all.
	jr.beat(ctx)

	free := jr.cfg.Concurrency - jr.inFlight()
	if free <= 0 {
		atCapacity.Inc()
		return 0
	}

	rows, err := jr.claimable(ctx, min(free, jr.cfg.Batch))
	if err != nil {
		if ctx.Err() == nil {
			jr.log.Warn("could not poll for placed jobs", "err", err)
			pollErrors.Inc()
		}
		return 0
	}

	started := 0
	for _, c := range rows {
		if ctx.Err() != nil || started >= free {
			break
		}
		if !jr.reserve(c.JobID) {
			continue
		}
		locked, err := jr.claims.take(ctx, c.JobID)
		if err != nil {
			jr.unreserve(c.JobID)
			jr.log.Warn("could not take the claim lock; not starting anything this cycle", "job", c.JobID, "err", err)
			return started
		}
		if !locked {
			// Another replica is running it. Ordinary, and the reason the lock
			// exists; not worth a log line above debug.
			jr.unreserve(c.JobID)
			contendedTotal.Inc()
			jr.log.Debug("job is claimed by another jobrunner", "job", c.JobID)
			continue
		}

		claimedTotal.Inc()
		jr.wg.Add(1)
		go func(c claim) {
			defer jr.wg.Done()
			jr.runJob(ctx, c)
		}(c)
		started++
	}
	return started
}

// claim is one row of the poll: a job the scheduler has placed and nobody is
// running. Only JobID is load-bearing — the lease is re-read authoritatively by
// Acquire — the rest is for the log line that says what we thought we saw.
type claim struct {
	JobID    string
	LeaseID  string
	Fence    int64
	DeviceID string
}

// claimable returns placed jobs that need a runner.
//
// The predicate, clause by clause:
//
//	j.state = 'running'          the scheduler has placed it,
//	live lease                   and the placement still exists. A job with no
//	                             live lease is the scheduler's problem, not
//	                             ours; claiming one would allocate a device to
//	                             a job that may already be finished.
//	no open attempt              nobody has started this attempt, so it is free
//	  OR stale heartbeat         — or somebody did and has stopped renewing,
//	                             which after three renewal intervals means the
//	                             process holding it is gone and its work should
//	                             be resumed rather than abandoned.
//
// FOR UPDATE OF j SKIP LOCKED keeps N replicas polling one queue from all
// picking the same rows in the same instant. It is not the claim — the row lock
// dies with this statement's transaction — the advisory lock in claimLocks is.
func (jr *JobRunner) claimable(ctx context.Context, limit int) ([]claim, error) {
	const q = `
SELECT j.id::text, l.id::text, l.fence, l.device_id::text
  FROM farm.jobs j
  JOIN farm.leases l ON l.job_id = j.id AND l.state IN ('held','suspect')
 WHERE j.state = 'running'
   AND (NOT EXISTS (SELECT 1 FROM farm.job_attempts a
                     WHERE a.job_id = j.id
                       AND a.attempt = j.attempt
                       AND a.finished_at IS NULL)
        OR l.heartbeat_at < now() - $1::interval)
 ORDER BY j.started_at, j.created_at
 LIMIT $2
 FOR UPDATE OF j SKIP LOCKED`

	cctx, cancel := jr.db(ctx)
	defer cancel()

	// An explicit transaction, because FOR UPDATE outside one is released the
	// instant the statement ends and SKIP LOCKED would then skip nothing.
	tx, err := jr.cfg.Pool.Begin(cctx)
	if err != nil {
		return nil, fmt.Errorf("jobrunner: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(cctx) }()

	rows, err := tx.Query(cctx, q, intervalArg(jr.cfg.Takeover), limit)
	if err != nil {
		return nil, fmt.Errorf("jobrunner: poll placed jobs: %w", err)
	}
	var out []claim
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.JobID, &c.LeaseID, &c.Fence, &c.DeviceID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("jobrunner: poll placed jobs scan: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobrunner: poll placed jobs: %w", err)
	}
	if err := tx.Commit(cctx); err != nil {
		return nil, fmt.Errorf("jobrunner: commit claim: %w", err)
	}
	placedGauge.Set(float64(len(out)))
	return out, nil
}

// runJob is one placement from attach to release. It runs on its own goroutine
// with its own Holder and its own device connection.
func (jr *JobRunner) runJob(ctx context.Context, c claim) {
	log := jr.log.With("job", c.JobID)

	// Registered first so it unwinds last, after every other defer below has
	// run. A panic anywhere in this goroutine — a step executor, a Dialer a
	// deployment supplied, this loop's own bookkeeping — must not reach the
	// runtime, because every OTHER job on this replica renews its lease from
	// this same process. Letting the process die would stop all of those
	// heartbeats at once and the reaper would take their devices ttl+grace
	// later: one job's bug destroying unrelated multi-hour work, which is the
	// precise harm this system is built to make impossible.
	defer jr.recoverRun(log)

	defer jr.finishClaim(c.JobID)

	runningGauge.Inc()
	defer runningGauge.Dec()

	// What the poll saw is already tens of milliseconds old, and this is the
	// last moment before the process takes ownership of a device.
	state, hasLease, err := jr.preflight(ctx, c.JobID)
	switch {
	case err != nil:
		log.Warn("could not re-read the job before attaching; leaving it for the next cycle", "err", err)
		jr.defer_(c.JobID)
		return
	case state == "running":
		// The ordinary path.
	case hasLease && (state == "succeeded" || state == "failed" || state == "cancelled"):
		// Terminal, but a device is still held for it. Attaching is how that
		// device gets handed back in seconds instead of at ttl+grace, and it
		// cannot allocate anything: a live lease exists, so acquire re-attaches.
	default:
		// Terminal with nothing held, 'queued', 'allocating', or gone
		// altogether. Nothing here is ours to touch, and attaching would do
		// real harm in two different ways — see preflight.
		log.Info("job is no longer a placed, running job; leaving it alone",
			"state", state, "has_lease", hasLease)
		return
	}

	// Re-attach. For a job the scheduler placed this returns the same lease,
	// the same device and the same fence, now heartbeated by us.
	res, err := jr.acquire(ctx, c.JobID)
	switch {
	case errors.Is(err, lease.ErrNoCapacity):
		// The lease we saw in the poll is gone and nothing else matched, so the
		// job is 'running' with no device. Nothing here is anybody's to
		// release; put it back on the queue and let the scheduler place it.
		log.Warn("placed job has no live lease; returning it to the queue")
		jr.requeueOrphan(ctx, log, c.JobID)
		return
	case errors.Is(err, lease.ErrJobNotFound):
		log.Warn("job disappeared between the poll and the attach")
		return
	case err != nil:
		// Says nothing about capacity and nothing about any existing lease.
		log.Warn("could not attach to the job's lease", "err", err)
		jr.defer_(c.JobID)
		return
	}

	l := res.Lease
	log = log.With("lease", l.ID, "device", l.DeviceID, "fence", l.Fence)
	switch {
	case res.Reattached && l.Fence == c.Fence:
		// The ordinary path, and the one the whole package exists for. The
		// device may still carry this job's own prior state — including a
		// detached command that has been running the whole time — so the runner
		// resumes from the checkpoint rather than starting over.
		reattachedTotal.Inc()
		log.Info("re-attached to the placed lease at the same fence", "reattached", true)
	case res.Reattached:
		// Still a re-attach, but not to the lease the poll saw: that placement
		// ended and the job was placed again while we were asking. Worth a line,
		// because the device under this job is not the one we were looking at.
		reattachedTotal.Inc()
		log.Info("re-attached to a lease the poll did not see; the placement changed underneath us",
			"polled_lease", c.LeaseID, "polled_fence", c.Fence, "polled_device", c.DeviceID)
	default:
		// No live lease existed by the time we asked, so acquire allocated one.
		// The job is genuinely running and now has a device it has never
		// touched; the runner will find no checkpoint of its own for it.
		log.Info("the placed lease had ended; this job was allocated a new device",
			"polled_lease", c.LeaseID, "polled_fence", c.Fence, "polled_device", c.DeviceID)
	}

	hcfg := jr.cfg.HolderConfig
	hcfg.Logger = log
	// The holder's parent is the loop's context, so a SIGTERM stops renewal
	// without releasing anything: the lease, the device and the fence survive
	// the process, which is exactly what a pod eviction must cost.
	h := lease.NewHolder(ctx, jr.cfg.Store, l, hcfg)
	defer h.Stop()

	job, err := jr.load(ctx, c.JobID, l.DeviceID)
	if err != nil {
		log.Warn("could not read the job and its device; leaving the lease alone", "err", err)
		jr.defer_(c.JobID)
		return
	}

	switch job.State {
	case "running":
	case "succeeded", "failed", "cancelled":
		// The job reached a terminal state between the poll and now. The lease
		// we are holding has no owner, so it goes back with the reason the JOB
		// carries — never with one invented here.
		jr.unwind(ctx, log, h, job.State)
		return
	default:
		// 'queued' or 'allocating': a human re-queued it, or the scheduler is
		// mid-placement. Either way the bookkeeping is somebody else's now and
		// the lease is theirs to re-attach to.
		log.Info("job left 'running' before work started; leaving its lease in place", "state", job.State)
		return
	}

	if job.Devpath == "" {
		jr.unaddressable(ctx, log, h, job, l,
			"device has no adb devpath recorded; work is addressed by USB position, never by serial")
		return
	}
	if job.Endpoint == "" {
		jr.unaddressable(ctx, log, h, job, l, "device's host has no adb endpoint recorded")
		return
	}

	dev, err := jr.cfg.Dial(job.Endpoint, job.Devpath, l.Fence)
	if err != nil {
		// The default Dialer touches no wire — it validates the devpath and
		// binds a client that opens a socket per call — so its only failure is
		// permanent. Dial is an exported field, though, and a deployment may
		// supply one that does reach the host. A wire failure is never a
		// verdict on a placement, so it is retried in the next cycle inside the
		// lease the job still holds; only a refusal that will refuse again ends
		// this placement.
		if adbwire.IsTransport(err) || adbwire.IsCanceled(err) {
			log.Warn("could not reach the host to build the device connection; "+
				"retrying inside the lease, which is left exactly as it is", "err", err)
			jr.defer_(c.JobID)
			return
		}
		// The devpath the database recorded is not one this binary will send.
		// Retrying cannot fix that and a serial is not an acceptable
		// substitute, so this placement ends like a device with no position.
		jr.unaddressable(ctx, log, h, job, l,
			fmt.Sprintf("no device connection for devpath %q: %v", job.Devpath, err))
		return
	}

	p := runner.Placement{
		JobID:    c.JobID,
		LeaseID:  l.ID,
		Fence:    l.Fence,
		DeviceID: l.DeviceID,
		SlotID:   l.SlotID,
		Devpath:  job.Devpath,
		Endpoint: job.Endpoint,
	}

	log.Info("running job", "devpath", p.Devpath, "endpoint", p.Endpoint,
		"attempt", job.Attempt, "max_attempts", job.MaxAttempts)

	out, err := jr.cfg.Runner.Run(ctx, h, p, dev)
	if err != nil {
		// The runner could not do its own bookkeeping — the database blinked,
		// the job vanished. That is not a verdict on the job and not a fact
		// about the device, so nothing is released and nothing is written. The
		// next cycle re-attaches to this same lease at this same fence.
		runnerErrors.Inc()
		log.Error("the runner could not record this placement; lease and device left exactly as they are", "err", err)
		jr.defer_(c.JobID)
		return
	}

	outcomesTotal.WithLabelValues(string(out.State)).Inc()

	// The holder is asked again here, and not only the outcome, because the two
	// answer different questions. out.Fenced is what the runner saw while it was
	// working; h.Fenced() is true right now, including for a lease lost in the
	// microseconds between the runner's last write and this line.
	//
	// Nothing else can tell the difference. A re-attach hands the replacement
	// the SAME fence — that is what makes a pod eviction free — so the fence
	// guard in writeJob matches the new holder's placement just as happily as
	// our own, and farm.lease_release matching on (id, fence) would accept a
	// release from this process and hand the new holder's device away
	// mid-run. The holder is the only thing in the system that knows.
	if out.Fenced || h.Fenced() {
		// Terminal for this attempt and for nothing else. We do not own the
		// lease any more: releasing it would take the device from whoever does,
		// which is the exact harm this whole system is built to make
		// impossible. The checkpoint is already written and the next placement
		// resumes from it.
		fencedTotal.Inc()
		log.Error("FENCED mid-run; releasing nothing and touching nothing",
			"state", string(out.State), "outcome_fenced", out.Fenced, "error", out.Error)
		return
	}

	// The job row is written BEFORE the lease is released, and that order is
	// not arbitrary. Releasing first and then failing to write would leave a
	// job marked 'running' with no lease at all, which no loop in this system
	// polls for and no operator would think to look for. Writing first and then
	// failing to release parks a device until the reaper reclaims it TTL+grace
	// later, which is a path that already exists and already heals.
	jr.finalize(ctx, log, p, job, out)

	switch {
	case out.ReleaseReason == "":
		// An abandoned attempt: SIGTERM, node drain, preemption. The lease MUST
		// survive — it is what the replacement process re-attaches to.
		log.Warn("attempt ended without a verdict; lease deliberately left held",
			"state", string(out.State), "error", out.Error)
	default:
		jr.releaseLease(ctx, log, h, out.ReleaseReason)
		log.Info("job finished", "state", string(out.State), "attempt", out.Attempt,
			"steps", out.Steps, "skipped", out.Skipped, "error", out.Error)
	}
}

// jobRow is what this loop needs to know about a job and the device it was
// placed on. The SPEC is deliberately absent: the runner loads and validates it
// from the same row, and two independent readings of one document are two
// chances for this loop and the runner to disagree about what the job is.
type jobRow struct {
	State       string
	Attempt     int
	MaxAttempts int

	// Devpath is farm.slots.adb_devpath via farm.v_fleet — the USB position.
	// Empty means the device is not currently in a slot, and an empty devpath
	// is the end of the road: there is no fallback to the serial, because a
	// serial-addressed command can land on a healthy clone mid-run.
	Devpath string

	// Endpoint is the host's adb server address.
	Endpoint string
}

// preflight re-reads the two facts the poll asserted, in the last instant
// before this loop takes ownership of a device.
//
// It exists because farm.lease_acquire does not look at farm.jobs.state. The
// function is idempotent on job_id and its second phase ALLOCATES, so calling
// it for a job that finished a moment ago hands a healthy phone to a dead job,
// holds it until load() reads the state back, and then quarantines the slot for
// the rearm window on the way out. Under a burst of cancellations that is real
// capacity, taken from live work.
//
// It also keeps this loop off a placement the scheduler is still making. Phase
// one of acquire rewrites holder and holder_instance and bumps holder_epoch —
// that is how a replacement pod takes a lease over — so attaching to a job the
// scheduler has moved back to 'queued' or 'allocating' fences the scheduler out
// of the very lease it is in the middle of committing.
//
// A missing job yields an empty state rather than an error: "the job is gone"
// is an answer, and the caller treats every state that is not a live placement
// the same way.
func (jr *JobRunner) preflight(ctx context.Context, jobID string) (state string, hasLease bool, err error) {
	const q = `
SELECT j.state,
       EXISTS (SELECT 1 FROM farm.leases l
                WHERE l.job_id = j.id AND l.state IN ('held','suspect'))
  FROM farm.jobs j
 WHERE j.id = $1::uuid`

	cctx, cancel := jr.db(ctx)
	defer cancel()

	err = jr.cfg.Pool.QueryRow(cctx, q, jobID).Scan(&state, &hasLease)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("jobrunner: preflight job %s: %w", jobID, err)
	}
	return state, hasLease, nil
}

func (jr *JobRunner) load(ctx context.Context, jobID, deviceID string) (jobRow, error) {
	// LEFT JOIN so that a device with no fleet row still yields the job's
	// state: "the job was cancelled" is a far more useful answer than "the
	// device is missing", and the caller checks the state first.
	const q = `
SELECT j.state, j.attempt, j.max_attempts,
       COALESCE(f.adb_devpath, ''), COALESCE(f.adb_endpoint, '')
  FROM farm.jobs j
  LEFT JOIN farm.v_fleet f ON f.device_id = $2::uuid
 WHERE j.id = $1::uuid`

	cctx, cancel := jr.db(ctx)
	defer cancel()

	var row jobRow
	err := jr.cfg.Pool.QueryRow(cctx, q, jobID, deviceID).Scan(
		&row.State, &row.Attempt, &row.MaxAttempts, &row.Devpath, &row.Endpoint)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return jobRow{}, fmt.Errorf("jobrunner: job %s does not exist", jobID)
	case err != nil:
		return jobRow{}, fmt.Errorf("jobrunner: load job %s: %w", jobID, err)
	}
	return row, nil
}

// finalize writes the job's state, and is a SAFETY NET rather than the primary
// path.
//
// internal/runner writes its own verdict before it returns, guarded by the
// fence. This write is guarded on the job still being 'running', so the
// runner's verdict always wins and zero rows here is the ordinary case. What it
// catches is the run whose final write did not land — a database blip in the
// last second of a six-hour job — which would otherwise leave a finished job
// marked 'running' forever.
func (jr *JobRunner) finalize(ctx context.Context, log *slog.Logger,
	p runner.Placement, job jobRow, out runner.Outcome) {

	if out.Fenced {
		// The lease is not ours any more, whatever State says. The runner
		// reports StateAbandoned alongside Fenced today and the switch below
		// would fall through to its default, but that is the runner's coupling
		// and not a promise this loop should rest a job's verdict on. The guard
		// below cannot substitute for this one: a re-attach preserves the fence,
		// so a newer placement's lease is not "at some other fence" and the
		// NOT EXISTS lets our stale verdict through.
		return
	}

	var (
		state   string
		errText = out.Error
		finish  bool
	)
	switch out.State {
	case runner.StateSucceeded:
		state, errText, finish = "succeeded", "", true
	case runner.StateCancelled:
		state, finish = "cancelled", true
	case runner.StateFailed:
		attempt := out.Attempt
		if attempt <= 0 {
			attempt = job.Attempt
		}
		// A job with attempts left goes back on the queue rather than to
		// 'failed', so the scheduler places it on a different device. The same
		// failure four times on one device is a device problem; four failures
		// on four devices is a job problem, and farm.job_attempts is the table
		// that tells them apart. When the runner's own write did not land we
		// cannot know whether the failure was permanent, and re-queueing is the
		// forgiving guess: it is bounded by max_attempts, while a wrong
		// 'failed' is not recoverable at all.
		if attempt < job.MaxAttempts {
			state = "queued"
		} else {
			state, finish = "failed", true
		}
	default:
		// Abandoned. The job belongs to whoever picks it up next; a process
		// that has just been told it lost the device does not get the last word.
		return
	}
	jr.writeJob(ctx, log, p.JobID, p.Fence, state, errText, 0, finish)
}

// writeJob is the one place this package writes farm.jobs.
//
// attempt is a FLOOR, not an increment: GREATEST leaves the counter alone
// unless this loop is recording a placement the runner never got far enough to
// claim an attempt for. internal/runner owns the increment (it claims one per
// attempt and keys farm.job_attempts on the result), and adding a second writer
// that increments unconditionally would spend a job's budget twice for one
// placement.
//
// The lease guard says: do not write a verdict if this job has a LIVE lease at
// some other fence. That is a newer placement, and its holder's opinion is
// newer than ours.
//
// Note what that guard does NOT cover, because it is easy to over-trust: a
// re-attach keeps the fence, so a replacement process holds a lease at OUR
// fence and this predicate passes for it too. Only the Holder knows the
// difference, which is why every caller here consults h.Fenced() before it
// calls this at all.
//
// When the write finds nothing to do because somebody else already recorded the
// ending, one narrow repair still runs: see stampFinished.
func (jr *JobRunner) writeJob(ctx context.Context, log *slog.Logger,
	jobID string, fence int64, state, errText string, attempt int, finish bool) {

	const q = `
UPDATE farm.jobs j
   SET state = $2,
       error = $3,
       attempt = GREATEST(j.attempt, $4),
       finished_at = CASE WHEN $5 THEN now() ELSE NULL END
 WHERE j.id = $1::uuid
   AND j.state = 'running'
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = j.id
                      AND l.state IN ('held','suspect')
                      AND l.fence <> $6)`

	// Bookkeeping must outlive the cancellation that ended the work: a job left
	// 'running' after the process that ran it is gone is a job nothing recovers.
	cctx, cancel := jr.db(context.WithoutCancel(ctx))
	defer cancel()

	tag, err := jr.cfg.Pool.Exec(cctx, q, jobID, state, nullString(errText), attempt, finish, fence)
	switch {
	case err != nil:
		writeErrors.Inc()
		log.Error("could not write the job's state", "state", state, "err", err)
	case tag.RowsAffected() == 0:
		// Overwhelmingly this means the runner already wrote its own verdict,
		// which is the design working. Debug, not warn: a warning that fires on
		// every successful job is a warning operators stop reading.
		log.Debug("job state not written; the runner's own verdict already stands or a newer placement owns it",
			"state", state, "fence", fence)
		if finish {
			jr.stampFinished(ctx, log, jobID)
		}
	default:
		log.Info("job state written by the supervisor rather than the runner",
			"state", state, "finished", finish)
	}
}

// stampFinished fills in the finish time of a job that reached a terminal state
// without one.
//
// It replaces NULL and nothing else, so it cannot overwrite anybody's verdict,
// and it runs only when the verdict was somebody else's. The number matters:
// finished_at minus started_at is how long a device was busy, and a fleet that
// cannot answer that cannot tell a slow job from a wedged one.
func (jr *JobRunner) stampFinished(ctx context.Context, log *slog.Logger, jobID string) {
	const q = `
UPDATE farm.jobs
   SET finished_at = now()
 WHERE id = $1::uuid
   AND state IN ('succeeded','failed','cancelled')
   AND finished_at IS NULL`

	cctx, cancel := jr.db(context.WithoutCancel(ctx))
	defer cancel()

	tag, err := jr.cfg.Pool.Exec(cctx, q, jobID)
	if err != nil {
		writeErrors.Inc()
		log.Warn("could not stamp the job's finish time", "err", err)
		return
	}
	if tag.RowsAffected() == 1 {
		log.Info("stamped the finish time of a terminal job that had none")
	}
}

// requeueOrphan returns a job to the queue when its placement no longer exists.
// Nothing is released because there is nothing to release.
func (jr *JobRunner) requeueOrphan(ctx context.Context, log *slog.Logger, jobID string) {
	// Fence 0 never exists — farm.fence_seq starts at 1 — so the guard in
	// writeJob reads as "write only if this job has NO live lease at all",
	// which is precisely the condition we believe we are repairing.
	jr.writeJob(ctx, log, jobID, 0, "queued", "placement lost before work started", 0, false)
	orphansTotal.Inc()
}

// unaddressable handles a lease on a device this loop cannot address.
//
// The device is real and healthy enough to have been allocated, but the fleet
// view has no USB position for it, or its host has no adb endpoint recorded.
// There is no fallback: work is addressed by position and never by serial,
// because duplicate OEM serials are real and a serial-addressed command can
// land on a device three hours into somebody else's run.
//
// So the lease goes back with the job's own honest reason, and the job returns
// to the queue with its attempt counter advanced. The advance is what stops the
// loop: without it the scheduler would place the job onto the same broken
// position forever, and farm.job_attempts would show nothing, because no
// attempt was ever opened. The event row below is the evidence in its place.
func (jr *JobRunner) unaddressable(ctx context.Context, log *slog.Logger,
	h leaseHolder, job jobRow, l lease.Lease, reason string) {

	if h.Fenced() {
		// We lost the lease while working this out, so both halves below are
		// somebody else's business now: the device belongs to whoever holds it,
		// and the job's verdict belongs to whoever is running it. Advancing the
		// attempt counter here would spend a budget on their placement.
		fencedTotal.Inc()
		log.Error("cannot address the leased device, and this process has since been fenced; "+
			"writing nothing and releasing nothing", "reason", reason)
		return
	}

	unaddressableTotal.Inc()
	log.Error("cannot address the leased device; releasing it so the job can be placed elsewhere",
		"reason", reason, "devpath", job.Devpath, "endpoint", job.Endpoint,
		"attempt", job.Attempt, "max_attempts", job.MaxAttempts)

	jr.event(ctx, log, l, "job_placement_unaddressable", map[string]any{
		"reason":       reason,
		"devpath":      job.Devpath,
		"endpoint":     job.Endpoint,
		"attempt":      job.Attempt + 1,
		"max_attempts": job.MaxAttempts,
	})

	state, finish := "queued", false
	if job.Attempt+1 >= job.MaxAttempts {
		state, finish = "failed", true
	}
	jr.writeJob(ctx, log, l.JobID, l.Fence, state, reason, job.Attempt+1, finish)
	jr.releaseLease(ctx, log, h, lease.ReasonFailed)
}

// unwind releases the lease of a job that reached a terminal state elsewhere.
// It mirrors the scheduler's own unwind: the reason comes from the job, never
// from anything observed about the device.
func (jr *JobRunner) unwind(ctx context.Context, log *slog.Logger, h leaseHolder, state string) {
	var reason lease.ReleaseReason
	switch state {
	case "cancelled":
		reason = lease.ReasonJobCancelled
	case "failed":
		reason = lease.ReasonFailed
	case "succeeded":
		reason = lease.ReasonCompleted
	default:
		return
	}
	log.Info("job reached a terminal state before work started; returning its device", "state", state)
	jr.releaseLease(ctx, log, h, reason)
}

// releaseLease ends a lease deliberately, with a reason the JOB produced.
//
// There is no branch here that turns a transport failure into a reason, and
// there could not be: farm.leases.release_reason has no connectivity value, so
// such a release raises check_violation instead of quietly destroying hours of
// work. That refusal is logged as the caller's bug it would be.
func (jr *JobRunner) releaseLease(ctx context.Context, log *slog.Logger,
	h leaseHolder, reason lease.ReleaseReason) {

	// This check is load-bearing, and it is the one thing standing between a
	// fenced process and somebody else's running job.
	//
	// It is tempting to assume the database catches this: every mutating lease
	// function matches on (id, fence). It does not. farm.lease_acquire
	// re-attaches at the SAME fence — deliberately, so a pod eviction costs
	// nothing — so a process that was fenced by its own replacement still holds
	// a fence that MATCHES. Its release would be accepted, the device would be
	// handed back mid-run, and the replacement's work would be destroyed by a
	// bookkeeping call from a process that had already lost. That is
	// DeviceFarmer/STF #663 with the control plane as the trigger.
	//
	// The holder knows because it is the thing renewal talks to. Nothing else
	// in the system can tell these two placements apart.
	if h.Fenced() {
		fencedTotal.Inc()
		log.Error("refusing to release a lease this process was fenced out of; it belongs to another holder now",
			"reason", string(reason))
		return
	}

	released, err := h.Release(context.WithoutCancel(ctx), reason, jr.cfg.SlotRearm)
	if err != nil {
		var cv *lease.CheckViolationError
		if errors.As(err, &cv) {
			log.Error("the database refused this release reason; this is a bug in the caller, not a transient fault",
				"reason", string(reason), "err", err)
		} else {
			log.Error("could not release the lease; the reaper will reclaim it after ttl+grace",
				"reason", string(reason), "err", err)
		}
		releaseErrors.Inc()
		return
	}
	if released {
		obs.LeaseReaped(obs.ReleaseReason(reason))
		releasesTotal.WithLabelValues(string(reason)).Inc()
	}
}

func (jr *JobRunner) acquire(ctx context.Context, jobID string) (lease.AcquireResult, error) {
	cctx, cancel := jr.db(ctx)
	defer cancel()
	return jr.cfg.Store.Acquire(cctx, jobID, jr.cfg.Holder, jr.cfg.HolderInstance)
}

// beat records that this loop is alive. A missed beat is not fatal: it is how
// the reaper learns we were gone, and being gone refunds tenant lease budget
// rather than costing it.
func (jr *JobRunner) beat(ctx context.Context) {
	cctx, cancel := jr.db(ctx)
	defer cancel()
	if err := jr.cfg.Store.ComponentBeat(cctx, jr.cfg.Component); err != nil && ctx.Err() == nil {
		jr.log.Warn("component heartbeat failed", "err", err)
		beatFailures.Inc()
	}
}

func (jr *JobRunner) event(ctx context.Context, log *slog.Logger, l lease.Lease,
	kind string, detail map[string]any) {

	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
VALUES ($1, $2::uuid, $3, $4::uuid, $5::uuid, 'jobrunner', $6::jsonb)`

	cctx, cancel := jr.db(context.WithoutCancel(ctx))
	defer cancel()

	if _, err := jr.cfg.Pool.Exec(cctx, q, kind, l.DeviceID, l.SlotID,
		nullString(l.ID), l.JobID, jsonOrEmpty(detail)); err != nil {
		log.Warn("could not write audit event", "event", kind, "err", err)
	}
}

// db bounds one bookkeeping round trip.
func (jr *JobRunner) db(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, jr.cfg.CallTimeout)
}

// recoverRun contains a panic to the job that raised it.
//
// Nothing is released and nothing is written to farm.jobs, which is the same
// answer this loop gives to every other ending it cannot explain: the lease
// survives, the checkpoint survives, and the next cycle re-attaches at the same
// fence. The one thing that would be worse than the panic is the crash — the
// process holds the renewal timer for every job on this replica, so dying here
// would silence all of them and cost devices that were doing nothing wrong.
//
// This is not a way to keep running through corruption. It is loud on purpose,
// and farm_jobrunner_panics_total is meant to be alerted on at any rate above
// zero.
func (jr *JobRunner) recoverRun(log *slog.Logger) {
	r := recover()
	if r == nil {
		return
	}
	panicsTotal.Inc()
	log.Error("PANIC while running a job; the lease, the device and the checkpoint were left untouched "+
		"and the next cycle re-attaches to them",
		"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
}

// dial is the default Dialer: one adbwire client per placement, announcing that
// placement's fence on every connection it opens.
//
// Per placement rather than the per-endpoint cache this used to keep. The
// preamble is a property of the placement — its fence — and a client is a
// dialer, not a connection: building one costs a struct and no socket. A cache
// keyed by (endpoint, fence) would have bought nothing and cost an eviction
// policy tied to the lease's lifetime, which is exactly the kind of bookkeeping
// that drifts from the truth it mirrors.
func (jr *JobRunner) dial(endpoint, devpath string, admission int64) (runner.Conn, error) {
	opts := make([]adbwire.Option, 0, len(jr.cfg.ADBOptions)+2)
	opts = append(opts, adbwire.WithLogger(jr.log))
	opts = append(opts, jr.cfg.ADBOptions...)
	opts = append(opts, adbwire.WithAdmissionPreamble(func() (string, string, int64, bool) {
		return admissionClass, devpath, admission, true
	}))
	return NewDeviceConn(adbwire.New(endpoint, opts...), devpath)
}

// ---------------------------------------------------------------------------
// Local scheduling memory. None of it is authoritative about anything.
// ---------------------------------------------------------------------------

// reserve marks a job as this replica's, unless it is already running here or
// is serving a local backoff.
func (jr *JobRunner) reserve(jobID string) bool {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	if _, ok := jr.busy[jobID]; ok {
		return false
	}
	if d, ok := jr.deferred[jobID]; ok && time.Now().Before(d.until) {
		return false
	}
	jr.busy[jobID] = struct{}{}
	return true
}

func (jr *JobRunner) unreserve(jobID string) {
	jr.mu.Lock()
	delete(jr.busy, jobID)
	jr.mu.Unlock()
}

// finishClaim gives up both halves of the claim. The advisory lock goes back
// first: it is the one another replica is waiting on.
func (jr *JobRunner) finishClaim(jobID string) {
	jr.claims.give(jobID)
	jr.unreserve(jobID)
}

func (jr *JobRunner) inFlight() int {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	return len(jr.busy)
}

// defer_ pushes a job's next START attempt out with exponential backoff. It
// bounds how often this loop retries a job it could not begin; it bounds
// nothing inside a run.
func (jr *JobRunner) defer_(jobID string) {
	jr.mu.Lock()
	defer jr.mu.Unlock()

	d, ok := jr.deferred[jobID]
	if !ok {
		d = &deferral{}
		jr.deferred[jobID] = d
	}
	d.tries++
	wait := jr.cfg.JobBackoff
	for i := 1; i < d.tries && wait < jr.cfg.JobBackoffMax; i++ {
		wait *= 2
	}
	if wait > jr.cfg.JobBackoffMax {
		wait = jr.cfg.JobBackoffMax
	}
	d.until = time.Now().Add(jitter(wait))
}

// pruneDeferrals bounds the map, keeping a lapsed entry for one further window
// so a job polled again immediately does not reset its backoff to the floor.
func (jr *JobRunner) pruneDeferrals(now time.Time) {
	jr.mu.Lock()
	defer jr.mu.Unlock()

	cutoff := now.Add(-jr.cfg.JobBackoffMax)
	for id, d := range jr.deferred {
		if d.until.Before(cutoff) {
			delete(jr.deferred, id)
		}
	}
	deferredGauge.Set(float64(len(jr.deferred)))
}

// ---------------------------------------------------------------------------
// The claim lock.
// ---------------------------------------------------------------------------

// claimLocks is the cross-replica claim: one dedicated Postgres session holding
// one advisory lock per job this process is running.
//
// A session advisory lock belongs to a SESSION, and a pooled connection is not
// a session you own past the end of one query — so the connection is acquired
// once and never handed back while the process lives. That is also the
// mechanism's best property: if this process dies, the session ends, every
// lock is released at once, and a replacement replica can pick the jobs up
// immediately rather than waiting out a timeout somebody had to guess.
//
// If the session itself is lost while jobs are still running (a Postgres
// restart), the locks vanish with it. The next take() reconnects and re-locks
// everything still held. Should another replica have slipped in first, no work
// is lost anyway: its Acquire takes over renewal, this process's holder is
// fenced at its next renewal, and the runner abandons the attempt with its
// checkpoint intact.
//
// "The session ends" is load-bearing and is NOT what handing a pooled
// connection back does; see discard.
type claimLocks struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// timeout bounds every round trip on the claim session. Without it a wedged
	// connection would hold the mutex and stall the poll goroutine — and a poll
	// goroutine that never returns is a loop that stops beating, which is the
	// one failure this package must never produce silently.
	timeout time.Duration

	mu   sync.Mutex
	conn *pgxpool.Conn
	held map[string]int64
}

// take tries to claim jobID. A false return with a nil error means another
// process holds it.
func (c *claimLocks) take(ctx context.Context, jobID string) (bool, error) {
	key := claimKey(jobID)

	c.mu.Lock()
	defer c.mu.Unlock()

	// pg_try_advisory_lock is RE-ENTRANT within a session: a second successful
	// call on the same key stacks a second reference that needs a second
	// unlock. give() issues exactly one, so taking a lock we already hold would
	// strand it for the life of the process — and a stranded claim key is a job
	// no replica in the farm can ever start again. The caller's own reserve()
	// already prevents this; being certain of it here costs one map lookup and
	// removes the dependency.
	if _, ok := c.held[jobID]; ok {
		return true, nil
	}

	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if err := c.ensure(cctx); err != nil {
		return false, err
	}
	var ok bool
	if err := c.conn.QueryRow(cctx, `SELECT pg_try_advisory_lock($1::bigint)`, key).Scan(&ok); err != nil {
		// The answer was lost, so the lock may or may not have been taken. End
		// the session rather than guess: that releases it either way, and the
		// next call builds a fresh one.
		c.discard()
		return false, fmt.Errorf("jobrunner: claim job %s: %w", jobID, err)
	}
	if ok {
		c.held[jobID] = key
	}
	return ok, nil
}

// give releases one job's claim. It is called from the job's own goroutine when
// the run ends, however it ended.
func (c *claimLocks) give(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key, ok := c.held[jobID]
	if !ok {
		return
	}
	delete(c.held, jobID)
	if c.conn == nil {
		return
	}
	// Not derived from the run's context: the release must happen precisely
	// when the run is being torn down, which is when that context is dead.
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	if _, err := c.conn.Exec(ctx, `SELECT pg_advisory_unlock($1::bigint)`, key); err != nil {
		// A stuck claim key keeps every replica off a job that is free, so the
		// session goes rather than the lock staying. Ending it releases this
		// key and every other one on it; the entries still in c.held are
		// re-taken by the next ensure().
		c.log.Warn("could not release the claim lock; ending the claim session so it cannot stay held",
			"job", jobID, "err", err)
		c.discard()
	}
}

// ensure makes sure there is a live session, re-locking anything still held if
// it had to build a new one. The caller holds c.mu.
func (c *claimLocks) ensure(ctx context.Context) error {
	if c.conn != nil && !c.conn.Conn().IsClosed() {
		return nil
	}
	c.discard()

	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("jobrunner: acquire the claim connection: %w", err)
	}
	c.conn = conn

	for jobID, key := range c.held {
		var ok bool
		if err := c.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, key).Scan(&ok); err != nil {
			c.discard()
			return fmt.Errorf("jobrunner: re-take the claim lock for job %s: %w", jobID, err)
		}
		if !ok {
			// Somebody still holds this key. Either another replica claimed a
			// job we are still running, or the backend of the session we just
			// ended has not finished dying yet. Nothing to do from here in
			// either case: the database settles the first at the next renewal,
			// where this process's holder is fenced and the runner abandons the
			// attempt with its checkpoint intact rather than writing a wrong
			// verdict; and the second resolves itself when that backend exits.
			c.log.Error("the claim lock for a running job could not be re-taken; another process may have claimed it",
				"job", jobID)
			delete(c.held, jobID)
		}
	}
	return nil
}

// discard ends the claim session, which is the ONLY thing that reliably
// releases the advisory locks on it.
//
// pgxpool.Conn.Release() hands a healthy connection back to the POOL, and a
// session-scoped advisory lock survives that untouched. Releasing a connection
// that still held a claim key would leave that key locked by an idle pooled
// connection: no replica in the farm could ever claim that job again, its
// device would sit doing nothing, and the job would end only when the reaper
// reclaimed its lease ttl+grace later — the multi-hour loss this package
// exists to prevent, arriving through the bookkeeping instead of the socket.
//
// So the connection is hijacked out of the pool (which then opens a
// replacement, so no capacity is lost) and closed. The close runs on its own
// goroutine because c.mu is held here and the poll goroutine waits on it: a
// socket wedged against a dead database must not be able to stall the loop's
// heartbeat, which is the one silence the reaper's gap accounting reads.
func (c *claimLocks) discard() {
	if c.conn == nil {
		return
	}
	raw := c.conn.Hijack()
	c.conn = nil
	timeout := c.timeout
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = raw.Close(ctx)
	}()
}

// close gives back every lock this loop took and ends the session.
//
// Unlocking explicitly is what makes a rolling deploy hand its jobs over in
// milliseconds instead of waiting for the departing pod's TCP session to be
// reaped. It names the keys rather than calling pg_advisory_unlock_all(),
// which would be shorter and wrong: this connection came from a pool shared
// with the scheduler's and the reaper's leadership keys, which live in the
// same one-argument advisory space, and clearing everything on it would hand
// leadership away as a side effect of a jobrunner shutting down.
//
// The session is then discarded rather than returned, so that a key the unlock
// did not reach cannot outlive this process on a pooled connection.
func (c *claimLocks) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		c.held = make(map[string]int64)
		return
	}
	if len(c.held) > 0 {
		keys := make([]int64, 0, len(c.held))
		for _, key := range c.held {
			keys = append(keys, key)
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		if _, err := c.conn.Exec(ctx,
			`SELECT pg_advisory_unlock(k) FROM unnest($1::bigint[]) AS k`, keys); err != nil {
			c.log.Warn("could not release the claim locks on shutdown; ending the session instead", "err", err)
		}
		cancel()
	}
	c.held = make(map[string]int64)
	c.discard()
}

// claimKey maps a job id onto the advisory lock space.
//
// The one-argument (bigint) advisory space is shared with the scheduler's and
// the reaper's leadership keys, so the prefix is not decoration: it makes a
// collision with either of those two fixed constants a 2^-63 event rather than
// something a future hash choice could stumble into.
func claimKey(jobID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("device-farmer/jobrunner:"))
	_, _ = h.Write([]byte(jobID))
	return int64(h.Sum64())
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// jitter spreads timers so N replicas sharing one database do not wake in
// lockstep after a restart.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := d / 5
	if spread <= 0 {
		return d
	}
	return d - spread/2 + time.Duration(rand.Int64N(int64(spread)+1))
}

// intervalArg renders a duration as a Postgres interval literal. It is sent as
// text and cast server-side so the value crosses the wire in exact
// microseconds. It is a DURATION: nothing here tells Postgres what time this
// process thinks it is.
func intervalArg(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%d microseconds", int64(d/time.Microsecond))
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func jsonOrEmpty(v map[string]any) string {
	if len(v) == 0 {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"detail_marshal_error":%q}`, err.Error())
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Metrics.
// ---------------------------------------------------------------------------

var (
	cyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_cycles_total",
		Help: "Poll cycles run by this jobrunner.",
	})

	claimedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_claimed_total",
		Help: "Placed jobs this replica claimed and started.",
	})

	contendedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_contended_total",
		Help: "Candidate jobs skipped because another replica already holds the claim lock.",
	})

	reattachedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_reattached_total",
		Help: "Attaches that re-attached to an existing lease at the same fence, rather than allocating.",
	})

	outcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_jobrunner_outcomes_total",
		Help: "Attempts by ending: succeeded, failed, cancelled, abandoned.",
	}, []string{"state"})

	releasesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_jobrunner_releases_total",
		Help: "Leases released by this loop, by the reason the job produced.",
	}, []string{"reason"})

	fencedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_fenced_total",
		Help: "Runs that lost their lease mid-flight. Nothing was released and nothing was written to the device.",
	})

	orphansTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_orphans_total",
		Help: "Jobs found in state 'running' with no live lease and returned to the queue.",
	})

	unaddressableTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_unaddressable_total",
		Help: "Placements refused because the leased device had no USB position or no adb endpoint. Never resolved by serial.",
	})

	panicsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_panics_total",
		Help: "Runs that panicked and were contained. Nothing was released; alert on any rate above zero.",
	})

	runnerErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_runner_errors_total",
		Help: "Runs that ended in a bookkeeping error rather than a verdict. The lease was left untouched.",
	})

	releaseErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_release_errors_total",
		Help: "Failed lease releases. Each one parks a device until the reaper reclaims it.",
	})

	writeErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_job_write_errors_total",
		Help: "Failed writes to farm.jobs.",
	})

	pollErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_poll_errors_total",
		Help: "Failed queue polls.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_beat_failures_total",
		Help: "Failed farm.component_beat calls. A sustained rate becomes a refunded control-plane gap.",
	})

	atCapacity = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_at_capacity_total",
		Help: "Cycles that claimed nothing because this replica was already running its maximum.",
	})

	runningGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "farm_jobrunner_running",
		Help: "Jobs this replica is running right now.",
	})

	placedGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "farm_jobrunner_claimable",
		Help: "Placed jobs the last poll found waiting for a runner.",
	})

	deferredGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "farm_jobrunner_deferred",
		Help: "Jobs this replica is backing off from starting.",
	})
)

// Collectors returns this package's metrics for registration alongside the
// other loops'.
func Collectors() []prometheus.Collector {
	// A CounterVec with no children exports NOTHING — not a zero, nothing at
	// all — and an alerting rule written over a series that does not exist
	// yields no result and therefore never fires. DeviceFarmerJobsFailing is
	// written over outcomesTotal{state="failed"}, so on a farm where no job has
	// ended yet the rule is silently unarmed, which is the failure mode the
	// alerting-blind runbook is about. The four states are the whole domain of
	// runner.State's terminal values; touching each one puts the series on the
	// first scrape.
	for _, st := range []runner.State{
		runner.StateSucceeded, runner.StateFailed,
		runner.StateCancelled, runner.StateAbandoned,
	} {
		outcomesTotal.WithLabelValues(string(st))
	}

	return []prometheus.Collector{
		cyclesTotal, claimedTotal, contendedTotal, reattachedTotal,
		outcomesTotal, releasesTotal, fencedTotal, orphansTotal,
		unaddressableTotal, panicsTotal, runnerErrors, releaseErrors, writeErrors,
		pollErrors, beatFailures, atCapacity,
		runningGauge, placedGauge, deferredGauge,
	}
}
