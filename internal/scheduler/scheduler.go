// Package scheduler is the allocation loop: it turns queued jobs into leases.
//
// It is one of the four control loops (scheduler, reaper, watchdog, recovery)
// and the only one that calls farm.lease_acquire. Everything it does is
// additive — it creates leases, it never ends one. The single exception is the
// narrow window it opens itself: if a job reaches a terminal state between the
// acquire and the state write, the scheduler releases the lease it just took
// with that job's own reason ('job_cancelled', 'failed', 'completed'). Those
// are endings the user asked for, not endings the system inferred.
//
// # Why zero rows is not an error
//
// farm.lease_acquire returns zero rows when no healthy, enabled, unleased
// device in the pool matched. That is the steady state of a busy farm, not a
// fault: a pool at 100% utilisation returns zero rows on every poll. Logging it
// as an error trains operators to ignore the scheduler's logs, and an ignored
// log is where the real failure hides. So no_capacity is counted, the job is
// deferred with backoff, and nothing is logged above debug.
//
// # Why the leader election needs its own connection
//
// See ensureLeadership. In short: a session advisory lock belongs to a Postgres
// SESSION, and a pooled connection is not a session you own past the end of one
// query. Taking the lock through the pool would give a lock whose lifetime is
// unrelated to the work it is supposed to guard.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// Defaults. Each is safe on its own and none of them can end a lease, so the
// worst a bad value here can do is schedule slowly.
const (
	// DefaultComponent is the name written to farm.component_heartbeat. It must
	// stay in farm.reaper_arm's component list: the scheduler is on the renewal
	// path (it mints the leases whose deadlines the reaper enforces), and a
	// scheduler outage that is invisible to gap accounting is BLOCKER 8 in
	// migrations/00002_lease.sql.
	DefaultComponent = "scheduler"

	// DefaultInterval is the poll period while there is work to place.
	DefaultInterval = 2 * time.Second

	// DefaultIdleInterval is the ceiling the poll period backs off to when a
	// cycle places nothing. A farm at full utilisation should not generate a
	// query storm to be told, four times a second, that it is still full.
	DefaultIdleInterval = 20 * time.Second

	// DefaultBatch bounds one cycle's candidate list. A LIMIT here is what
	// stops one enormous queue from starving every other queue's first row of
	// attention for a whole cycle.
	DefaultBatch = 32

	// DefaultJobBackoff/DefaultJobBackoffMax bound the per-job retry delay after
	// no_capacity. The job stays 'queued' in the database throughout — "re-queue"
	// is a local decision to stop asking for a moment, not a state change.
	DefaultJobBackoff    = 3 * time.Second
	DefaultJobBackoffMax = 2 * time.Minute

	// DefaultCallTimeout bounds every individual database call. The scheduler
	// must never block forever on a statement: a wedged scheduler stops
	// beating, and a component that stops beating is what farm.reaper_arm's gap
	// refund exists to notice.
	DefaultCallTimeout = 10 * time.Second

	// DefaultLockKey is the pg_try_advisory_lock key that elects the single
	// active scheduler. It spells "farmSchl" in ASCII so an operator reading
	// pg_locks can tell whose lock it is.
	DefaultLockKey int64 = 0x6661726d5363686c
)

// Config is the scheduler's wiring. Pool, Store and Holder are required.
type Config struct {
	// Pool is used for job bookkeeping AND for the dedicated leadership
	// connection, so it must allow at least two connections: one held for the
	// whole process lifetime by the advisory lock, plus one to work with.
	Pool *pgxpool.Pool

	// Store is the binding to the farm.lease_* functions. The scheduler never
	// writes farm.leases directly; every allocation goes through
	// farm.lease_acquire, because the partial unique index and the row locks
	// that make a double grant impossible live inside that function.
	Store *lease.Store

	// Component is the farm.component_heartbeat key. Defaults to
	// DefaultComponent; override only in lockstep with FARM_REAPER_COMPONENTS.
	Component string

	// Holder is this process's identity, normally the pod name. It is AUDIT
	// ONLY and confers no ownership: ownership is keyed on job_id, which is why
	// the job's runner can later re-attach to the very lease allocated here.
	Holder string

	// HolderInstance is a UUID minted once per process (lease.NewHolderInstance).
	// The runner that picks the job up calls acquire with its own instance and
	// takes over the lease at the same fence; until then this scheduler is the
	// nominal holder.
	HolderInstance string

	Interval     time.Duration
	IdleInterval time.Duration
	Batch        int

	JobBackoff    time.Duration
	JobBackoffMax time.Duration

	CallTimeout time.Duration

	// SlotRearm is passed to farm.lease_release on the terminal-job path. It
	// MUST exceed the node proxy's self-fence timeout; config.Config.Validate
	// asserts that relationship at startup.
	SlotRearm time.Duration

	// LockKey is the advisory lock key for leader election. Every scheduler
	// replica must use the same value or leader election elects everyone.
	LockKey int64

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.IdleInterval < c.Interval {
		c.IdleInterval = max(DefaultIdleInterval, c.Interval)
	}
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.JobBackoff <= 0 {
		c.JobBackoff = DefaultJobBackoff
	}
	if c.JobBackoffMax < c.JobBackoff {
		c.JobBackoffMax = max(DefaultJobBackoffMax, c.JobBackoff)
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.SlotRearm <= 0 {
		c.SlotRearm = lease.DefaultRearm
	}
	if c.LockKey == 0 {
		c.LockKey = DefaultLockKey
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Scheduler is the allocation loop. Construct it with New and run it with Run.
type Scheduler struct {
	cfg Config
	log *slog.Logger

	// lead is the dedicated leadership connection and its lock state. It is
	// only ever touched from Run's goroutine.
	lead leadership

	// deferred remembers when each job may next be attempted. It exists purely
	// so a full pool does not cost one lease_acquire per job per poll; the
	// authoritative queue is farm.jobs.state, which this map never modifies.
	deferred map[string]*deferral
}

type deferral struct {
	until time.Time
	tries int
}

// New validates cfg and returns a Scheduler.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Pool == nil {
		return nil, errors.New("scheduler: Config.Pool is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("scheduler: Config.Store is required")
	}
	if cfg.Holder == "" {
		return nil, errors.New("scheduler: Config.Holder is required (the pod name; audit only)")
	}
	if cfg.HolderInstance == "" {
		return nil, errors.New("scheduler: Config.HolderInstance is required (mint one per process with lease.NewHolderInstance)")
	}
	cfg.applyDefaults()
	return &Scheduler{
		cfg:      cfg,
		log:      cfg.Logger.With("component", cfg.Component),
		lead:     leadership{pool: cfg.Pool, key: cfg.LockKey, log: cfg.Logger},
		deferred: make(map[string]*deferral),
	}, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation: for a daemon loop a SIGTERM is an orderly
// stop, not a failure, and — per cmd/farmd/main.go — it does not release a
// single lease. Every live lease keeps its device; the runner re-attaches, or
// the reaper's grace band eventually applies. A non-nil error means the loop
// could not continue at all.
func (s *Scheduler) Run(ctx context.Context) error {
	defer s.lead.release(ctx)

	s.log.Info("scheduler loop starting",
		"interval", s.cfg.Interval, "idle_interval", s.cfg.IdleInterval,
		"batch", s.cfg.Batch, "holder", s.cfg.Holder)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler loop stopping; live leases are intentionally untouched")
			return nil
		case <-timer.C:
		}

		placed := s.cycle(ctx)

		// Placing something means there may be more to place: poll fast. An
		// empty cycle means the farm is full or the queue is empty, and either
		// way the next answer will not be different in 2s.
		wait := s.cfg.Interval
		if placed == 0 {
			wait = s.cfg.IdleInterval
		}
		timer.Reset(jitter(wait))
	}
}

// cycle runs one poll and returns how many jobs it placed.
func (s *Scheduler) cycle(ctx context.Context) int {
	cyclesTotal.Inc()

	// The heartbeat is written whether or not we are the leader. It answers
	// "was a scheduler process reachable?", which is the question
	// farm.reaper_arm's gap refund asks; a healthy standby that would take over
	// within one poll is not a control-plane outage and must not refund lease
	// time that was never lost.
	s.beat(ctx)

	leader, err := s.lead.ensure(ctx, s.cfg.CallTimeout)
	if err != nil {
		s.log.Warn("scheduler leadership check failed; not scheduling this cycle", "err", err)
		leaderGauge.Set(0)
		return 0
	}
	if !leader {
		leaderGauge.Set(0)
		return 0
	}
	leaderGauge.Set(1)

	jobs, err := s.candidates(ctx)
	if err != nil {
		s.log.Warn("scheduler could not read the queue", "err", err)
		return 0
	}

	now := time.Now()
	placed := 0
	// A pool that just answered "no capacity" will answer the same for every
	// other job in that pool this cycle. Remembering it turns a 32-job batch
	// against an exhausted pool into one call instead of thirty-two.
	//
	// Only UNPINNED jobs may write to this map. farm.lease_acquire filters on
	// jobs.pin_device, so a pinned job whose one device is busy answers
	// no_capacity while the pool is half idle; letting that answer mark the
	// pool would stall every other job in it for the rest of the cycle.
	exhausted := make(map[string]struct{})

	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		if d, ok := s.deferred[j.ID]; ok && now.Before(d.until) {
			continue
		}
		if _, dead := exhausted[j.PoolID]; dead {
			continue
		}
		if !j.withinQuota() {
			// Not an error and not a device shortage: the tenant or queue is at
			// the cap an operator set. Defer briefly so the job is retried as
			// soon as one of its siblings finishes.
			placementsTotal.WithLabelValues("quota").Inc()
			s.defer_(j.ID, now)
			continue
		}

		res, err := s.acquire(ctx, j.ID)
		switch {
		case errors.Is(err, lease.ErrNoCapacity):
			// The ordinary outcome. Not logged above debug on purpose: see the
			// package comment.
			placementsTotal.WithLabelValues("no_capacity").Inc()
			if !j.Pinned {
				exhausted[j.PoolID] = struct{}{}
			}
			s.defer_(j.ID, now)
			s.log.Debug("no capacity", "job", j.ID, "pool", j.PoolID, "pinned", j.Pinned)
			continue
		case errors.Is(err, lease.ErrJobNotFound):
			// The job was deleted between the SELECT and the acquire.
			placementsTotal.WithLabelValues("job_gone").Inc()
			delete(s.deferred, j.ID)
			continue
		case err != nil:
			// Transient database trouble. It says nothing about capacity and
			// nothing about any existing lease, so stop this cycle rather than
			// hammering a struggling database with the rest of the batch.
			placementsTotal.WithLabelValues("error").Inc()
			s.log.Warn("acquire failed", "job", j.ID, "err", err)
			return placed
		}

		delete(s.deferred, j.ID)
		s.onAcquired(ctx, j, res)
		placed++
	}

	s.pruneDeferrals(now)
	deferredGauge.Set(float64(len(s.deferred)))
	return placed
}

// onAcquired moves the job to running, or unwinds if the job is no longer
// schedulable.
func (s *Scheduler) onAcquired(ctx context.Context, j candidate, res lease.AcquireResult) {
	log := s.log.With("job", j.ID, "lease", res.Lease.ID, "device", res.Lease.DeviceID,
		"fence", res.Lease.Fence)

	// Acquire runs BEFORE the state write, and that order is deliberate.
	// farm.lease_acquire is idempotent on job_id, so a crash between the two
	// costs nothing: the job is still 'queued', the next cycle re-acquires, and
	// Phase 1 hands back the same lease, the same device and the same fence.
	// The reverse order would leave a job marked 'running' with no lease at all.
	moved, err := s.markRunning(ctx, j.ID)
	if err != nil {
		// The lease exists and is correct; only the bookkeeping failed. Leave
		// it alone — releasing here would destroy an allocation because of a
		// failed UPDATE, which is the shape of mistake this project exists to
		// prevent.
		placementsTotal.WithLabelValues("state_write_failed").Inc()
		log.Warn("acquired a lease but could not mark the job running; the next cycle re-attaches", "err", err)
		return
	}
	if moved {
		placementsTotal.WithLabelValues("placed").Inc()
		if res.Reattached {
			// The job had a live lease already — a supervisor that died before
			// writing the state, or an operator re-queueing a running job. The
			// device may still carry that run's state, so the runner must
			// resume from jobs.checkpoint rather than start over.
			log.Info("re-attached an existing lease for a queued job", "reattached", true)
		} else {
			log.Info("placed job", "slot", slotOf(res.Lease))
		}
		return
	}

	// Zero rows: the job left 'queued'/'allocating' underneath us. The only
	// legitimate cause is a terminal transition (a human cancelled it), and the
	// lease we just took has no owner. Unwind it with the reason the JOB
	// carries — never with an invented one.
	s.unwind(ctx, j.ID, res.Lease, log)
}

// unwind releases a lease that was granted to a job which is no longer
// schedulable. It is the scheduler's only release path and it closes a window
// the scheduler itself opened.
func (s *Scheduler) unwind(ctx context.Context, jobID string, l lease.Lease, log *slog.Logger) {
	state, err := s.jobState(ctx, jobID)
	if err != nil {
		placementsTotal.WithLabelValues("state_write_failed").Inc()
		log.Warn("job left the queue and its state could not be re-read; leaving the lease alone", "err", err)
		return
	}

	var reason lease.ReleaseReason
	switch state {
	case "cancelled":
		reason = lease.ReasonJobCancelled
	case "failed":
		reason = lease.ReasonFailed
	case "succeeded":
		reason = lease.ReasonCompleted
	default:
		// 'running' or 'allocating': somebody else owns the bookkeeping now.
		// The lease is theirs and must not be touched.
		placementsTotal.WithLabelValues("raced").Inc()
		log.Info("job left the queue for a live state; leaving its lease in place", "state", state)
		return
	}

	// Bounded like every other database call in this loop. An unbounded release
	// is the one statement that could wedge the cycle indefinitely against a
	// struggling database, and a wedged scheduler stops beating — which is
	// precisely the control-plane gap farm.reaper_arm has to refund.
	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()

	ok, err := s.cfg.Store.Release(cctx, l.ID, l.Fence, reason, s.cfg.SlotRearm)
	if err != nil {
		placementsTotal.WithLabelValues("unwind_failed").Inc()
		log.Warn("could not release the lease of a terminal job", "state", state, "err", err)
		return
	}
	if ok {
		placementsTotal.WithLabelValues("unwound").Inc()
		obs.LeaseReaped(obs.ReleaseReason(reason))
		log.Info("released the lease of a job that finished while being placed",
			"state", state, "reason", string(reason))
	}
}

// acquire wraps Store.Acquire with the per-call timeout.
func (s *Scheduler) acquire(ctx context.Context, jobID string) (lease.AcquireResult, error) {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	return s.cfg.Store.Acquire(cctx, jobID, s.cfg.Holder, s.cfg.HolderInstance)
}

// beat records that this scheduler is alive.
func (s *Scheduler) beat(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	if err := s.cfg.Store.ComponentBeat(cctx, s.cfg.Component); err != nil && ctx.Err() == nil {
		// A missed beat is not fatal — it is how the reaper learns we were
		// gone, and being gone refunds tenant lease budget rather than costing
		// it. Log it so a persistent failure is visible before it becomes a gap.
		s.log.Warn("component heartbeat failed", "err", err)
		beatFailures.Inc()
	}
}

// candidate is one row of the queue poll.
type candidate struct {
	ID       string
	PoolID   string
	TenantID string
	QueueID  string

	// Pinned is farm.jobs.pin_device IS NOT NULL. A pinned job's no_capacity
	// answer is about ONE device, so it says nothing about the pool — see the
	// exhausted map in cycle.
	Pinned bool

	tenantCap  int64 // farm.tenants.max_devices, 0 = unlimited
	queueCap   int64 // farm.queues.max_devices, 0 = unlimited
	tenantHeld int64
	queueHeld  int64
}

// withinQuota reports whether placing this job would stay inside the operator's
// per-tenant and per-queue device caps.
//
// The caps are enforced here because nothing else enforces them:
// farm.lease_acquire deliberately knows only about pools and health. The counts
// are read at the top of the cycle, so a batch can overshoot a cap by the
// number of jobs it places for one tenant in one pass. That is acceptable —
// the cap is a fairness knob, not a safety property, and the safety property
// (one lease per device) is held by a partial unique index inside the database.
func (c candidate) withinQuota() bool {
	if c.tenantCap > 0 && c.tenantHeld >= c.tenantCap {
		return false
	}
	if c.queueCap > 0 && c.queueHeld >= c.queueCap {
		return false
	}
	return true
}

func (s *Scheduler) candidates(ctx context.Context) ([]candidate, error) {
	// queues.priority is ascending urgency (the default of 100 leaves room on
	// both sides), then oldest first inside a queue.
	//
	// The live-lease counts are aggregated ONCE in a CTE rather than as
	// correlated subqueries per candidate row: the predicate matches the
	// partial indexes on farm.leases exactly, so it reads the live leases and
	// not the history, which grows without bound.
	const q = `
WITH live AS (
  SELECT l.tenant_id, l.queue_id FROM farm.leases l WHERE l.state IN ('held','suspect')
), by_tenant AS (
  SELECT tenant_id, count(*) AS n FROM live GROUP BY tenant_id
), by_queue AS (
  SELECT queue_id, count(*) AS n FROM live GROUP BY queue_id
)
SELECT j.id::text, j.pool_id, j.tenant_id, j.queue_id,
       j.pin_device IS NOT NULL,
       t.max_devices, q.max_devices,
       COALESCE(bt.n, 0), COALESCE(bq.n, 0)
  FROM farm.jobs j
  JOIN farm.queues  q ON q.id = j.queue_id
  JOIN farm.tenants t ON t.id = j.tenant_id
  LEFT JOIN by_tenant bt ON bt.tenant_id = j.tenant_id
  LEFT JOIN by_queue  bq ON bq.queue_id  = j.queue_id
 WHERE j.state = 'queued'
 ORDER BY q.priority, j.created_at
 LIMIT $1`

	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()

	rows, err := s.cfg.Pool.Query(cctx, q, s.cfg.Batch)
	if err != nil {
		return nil, fmt.Errorf("scheduler: poll queue: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ID, &c.PoolID, &c.TenantID, &c.QueueID, &c.Pinned,
			&c.tenantCap, &c.queueCap, &c.tenantHeld, &c.queueHeld); err != nil {
			return nil, fmt.Errorf("scheduler: poll queue scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduler: poll queue: %w", err)
	}
	queuedGauge.Set(float64(len(out)))
	return out, nil
}

// markRunning moves a placed job to 'running'. It reports whether the row moved.
func (s *Scheduler) markRunning(ctx context.Context, jobID string) (bool, error) {
	// started_at is COALESCEd so a re-attached job keeps the instant its work
	// actually began; farm.jobs.max_runtime is measured from the LEASE's
	// acquired_at, but dashboards and humans read this column.
	const q = `
UPDATE farm.jobs
   SET state = 'running', started_at = COALESCE(started_at, now())
 WHERE id = $1::uuid AND state IN ('queued','allocating')`

	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()

	tag, err := s.cfg.Pool.Exec(cctx, q, jobID)
	if err != nil {
		return false, fmt.Errorf("scheduler: mark job %s running: %w", jobID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Scheduler) jobState(ctx context.Context, jobID string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()

	var state string
	err := s.cfg.Pool.QueryRow(cctx, `SELECT state FROM farm.jobs WHERE id = $1::uuid`, jobID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("scheduler: job %s vanished", jobID)
	}
	if err != nil {
		return "", fmt.Errorf("scheduler: read job %s state: %w", jobID, err)
	}
	return state, nil
}

// defer_ pushes a job's next attempt out with exponential backoff.
func (s *Scheduler) defer_(jobID string, now time.Time) {
	d, ok := s.deferred[jobID]
	if !ok {
		d = &deferral{}
		s.deferred[jobID] = d
	}
	d.tries++
	wait := s.cfg.JobBackoff
	for i := 1; i < d.tries && wait < s.cfg.JobBackoffMax; i++ {
		wait *= 2
	}
	if wait > s.cfg.JobBackoffMax {
		wait = s.cfg.JobBackoffMax
	}
	d.until = now.Add(jitter(wait))
}

// pruneDeferrals bounds the map. An entry is kept while it is still in force
// and for one further backoff window after it lapses, so a job that is polled
// again immediately keeps its attempt count and does not reset its backoff to
// the floor on every cycle.
func (s *Scheduler) pruneDeferrals(now time.Time) {
	cutoff := now.Add(-s.cfg.JobBackoffMax)
	for id, d := range s.deferred {
		if d.until.Before(cutoff) {
			delete(s.deferred, id)
		}
	}
}

func slotOf(l lease.Lease) any {
	if l.SlotID == nil {
		return "none"
	}
	return *l.SlotID
}

// jitter spreads timers so N scheduler replicas — and N reaper and watchdog
// loops sharing the same database — do not wake in lockstep after a restart.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(int64(d)/4+1))
}

// ---------------------------------------------------------------------------
// Leader election
// ---------------------------------------------------------------------------

// leadership holds a Postgres SESSION advisory lock on a connection that is
// acquired from the pool and then kept for as long as leadership lasts.
//
// WHY A POOLED CONNECTION IS WRONG HERE, in full, because this is the kind of
// bug that only shows up as two schedulers allocating at once:
//
//   - pg_try_advisory_lock takes a SESSION-scoped lock. It is held by the
//     backend that executed it and released when that backend's session ends —
//     not when the statement, or the transaction, or the Go function returns.
//
//   - pgxpool hands out a connection per call and takes it back immediately.
//     Two consecutive pool.Exec calls may run on two different backends. Take
//     the lock through the pool and the next statement — the one the lock is
//     supposed to protect — very likely runs on a backend that holds nothing.
//
//   - Worse, the connection that does hold the lock goes back into the pool
//     where it is handed to unrelated work, and where the pool's
//     MaxConnLifetime / MaxConnIdleTime reaper may close it. Closing it ends
//     the session and silently releases the lock, while this process still
//     believes it is the leader. Two schedulers then run allocation
//     simultaneously, and each one's view of tenant quota is half of the truth.
//
// So the lock is taken on a connection this struct owns outright, every
// leadership check is a liveness probe on that same connection, and losing the
// connection is treated as losing leadership immediately. A checked-out
// connection is exempt from the pool's idle and lifetime reaping, which is
// exactly the property being relied on.
//
// The lock is also never taken with the blocking pg_advisory_lock: a standby
// blocked inside the database would still be beating, still be a candidate, and
// would hold a pool connection open forever waiting for a leader that may never
// let go.
type leadership struct {
	pool *pgxpool.Pool
	key  int64
	log  *slog.Logger

	conn *pgxpool.Conn
	held bool
}

// ensure returns whether this process currently holds the scheduler lock,
// taking it if it is free.
func (l *leadership) ensure(ctx context.Context, timeout time.Duration) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if l.conn != nil {
		// The lock lives in the session. If the session is gone, so is the
		// lock, whatever this process last believed.
		if err := l.conn.Ping(cctx); err != nil {
			l.log.Warn("scheduler leadership connection died; standing down", "err", err)
			l.drop()
			return false, nil
		}
		if l.held {
			return true, nil
		}
	}

	if l.conn == nil {
		c, err := l.pool.Acquire(cctx)
		if err != nil {
			return false, fmt.Errorf("scheduler: acquire leadership connection: %w", err)
		}
		l.conn = c
	}

	var ok bool
	if err := l.conn.QueryRow(cctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&ok); err != nil {
		l.drop()
		return false, fmt.Errorf("scheduler: try advisory lock: %w", err)
	}
	if !ok {
		// Another replica leads. Give the connection back rather than holding
		// one hostage per standby for the life of the deployment.
		l.drop()
		return false, nil
	}
	l.held = true
	l.log.Info("scheduler acquired leadership", "lock_key", l.key)
	return true, nil
}

// release unlocks and returns the connection. It runs on shutdown so a rolling
// deploy hands leadership over in milliseconds instead of waiting for the old
// pod's TCP session to be reaped.
func (l *leadership) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	if l.held {
		// ctx is typically already cancelled by the signal that started the
		// shutdown, and an unlock on a cancelled context does not run. Detach
		// from that cancellation deliberately: this is the one call whose whole
		// purpose is to happen after the stop.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := l.conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, l.key); err != nil {
			// Not fatal: closing the connection ends the session, which
			// releases the lock anyway. It is just slower.
			l.log.Debug("advisory unlock failed; the session close will release it", "err", err)
		}
	}
	l.drop()
}

func (l *leadership) drop() {
	if l.conn != nil {
		l.conn.Release()
		l.conn = nil
	}
	l.held = false
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	cyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "scheduler", Name: "cycles_total",
		Help: "Scheduler poll cycles, including cycles run by a standby that placed nothing.",
	})

	// outcome="no_capacity" is the ordinary steady state of a full farm and
	// must never be alerted on; outcome="error" is the one worth a rule.
	placementsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "scheduler", Name: "placements_total",
		Help: "Job placement attempts by outcome. no_capacity is normal at full utilisation.",
	}, []string{"outcome"})

	queuedGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "scheduler", Name: "queue_candidates",
		Help: "Jobs in state 'queued' seen by the last poll, bounded by the batch size.",
	})

	deferredGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "scheduler", Name: "deferred_jobs",
		Help: "Queued jobs currently inside a local retry backoff after no_capacity or a quota cap.",
	})

	leaderGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "scheduler", Name: "leader",
		Help: "1 when this replica holds the scheduler advisory lock. Sum across replicas must be 1.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "scheduler", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. A sustained rate becomes a control-plane gap.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
// The known outcome labels are pre-created so an alert on them is armed from
// the first scrape rather than from the first occurrence.
func Collectors() []prometheus.Collector {
	for _, outcome := range []string{
		"placed", "no_capacity", "quota", "job_gone", "error",
		"state_write_failed", "raced", "unwound", "unwind_failed",
	} {
		placementsTotal.WithLabelValues(outcome)
	}
	return []prometheus.Collector{
		cyclesTotal, placementsTotal, queuedGauge, deferredGauge, leaderGauge, beatFailures,
	}
}
