// Package reaper runs THE ONLY AUTOMATIC RELEASE PATH IN THE SYSTEM.
//
// # Read this before changing anything in this file
//
// Three functions end a lease without a human: farm.lease_reclaim,
// farm.lease_expire_max_runtime, and farm.lease_release called by the job
// itself. This loop drives the first two. Nothing else in device-farmer may
// take a device away from the job that holds it.
//
// Each of them fires on exactly one fact:
//
//	lease_reclaim            the HOLDER stopped heartbeating for ttl+grace,
//	                         produced no witness, is not protected, and no
//	                         control-plane gap overlaps its silence.
//	lease_expire_max_runtime the job outran farm.jobs.max_runtime — a number
//	                         the user wrote down themselves.
//
// Notice what is absent from both: any fact about the device. No adb_state, no
// probe result, no socket error, no ping. That absence is the design.
//
// # Do not add a health check to this loop
//
// It will look reasonable. It always does: "we are about to reclaim a lease, so
// let us first check whether the device is even reachable / let us reclaim
// early because the device is clearly gone / let us skip the reclaim because
// the device looks fine". Every one of those is DeviceFarmer/STF issue #663
// rebuilt on top of a schema built to prevent it — a device handed to another
// job mid-run because a transport failed, destroying multi-hour work.
//
// The database refuses to help you do it. farm.lease_reclaim carries
// `SET role = farm_reaper`, and SELECT on farm.device_runtime is REVOKED from
// that role: reclamation is structurally blind to health, not merely
// discouraged from consulting it. The equivalent rule for this file is simpler
// still: farm.device_runtime is named nowhere in it except in this paragraph,
// and no query it issues may ever name it.
//
// A device that is broken is the recovery ladder's problem (internal/recovery).
// A device that is broken AND leased stays with its holder while recovery acts
// on the holder's behalf: the lease clock keeps ticking and the fence never
// moves.
//
// # Arming before sweeping
//
// On every gain of leadership this loop calls farm.reaper_arm BEFORE its first
// sweep. That call does two things that must both happen while the reaper is
// still idle: it refunds any control-plane outage to every live lease, and it
// sets a quiesce window so a control plane that has just come back does not
// mass-reclaim at the instant of restoration. Sweeping first and arming second
// would reclaim exactly the leases the refund exists to save.
package reaper

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

// Defaults.
const (
	// DefaultComponent is the farm.component_heartbeat key and must appear in
	// farm.reaper_arm's component list.
	DefaultComponent = "reaper"

	DefaultInterval    = 10 * time.Second
	DefaultBatch       = 100
	DefaultGapFloor    = 60 * time.Second
	DefaultCallTimeout = 15 * time.Second

	// DefaultCensusEvery is how often the lease gauges are recomputed. It is
	// deliberately coarser than the sweep: the gauges are for dashboards, and a
	// GROUP BY over farm.leases every ten seconds buys nothing.
	DefaultCensusEvery = 30 * time.Second

	// DefaultLockKey spells "farmReap" in ASCII.
	DefaultLockKey int64 = 0x6661726d52656170
)

// Config is the reaper's wiring. Pool and Store are required.
type Config struct {
	// Pool serves the census queries, the audit inserts, and the dedicated
	// leadership connection, so it must allow at least two connections.
	Pool *pgxpool.Pool

	// Store binds the farm.lease_* functions. Every ending in this loop goes
	// through them; this package writes farm.leases through no other path.
	Store *lease.Store

	Component string
	Interval  time.Duration

	// Batch bounds each sweep. The SQL functions use SKIP LOCKED, so a bounded
	// batch is a fairness and latency knob, not a correctness one.
	Batch int

	// GapFloor is the shortest control-plane silence that counts as an outage
	// worth refunding. Below it, ordinary scheduling jitter would be recorded
	// as downtime and would hand back lease budget nobody lost.
	GapFloor time.Duration

	// Components is the set farm.reaper_arm computes the gap across. It must
	// name EVERY component on the renewal path. Keying the gap on the reaper's
	// own heartbeat alone is how a healthy reaper next to a dead API reclaims
	// the entire farm (BLOCKER 8 in migrations/00002_lease.sql).
	//
	// It must NOT name the watchdog or the recovery ladder. Those beat too, and
	// their heartbeats are for operators to see a stalled loop — but a health
	// plane outage moving lease deadlines is the very fusion of clocks this
	// system exists to prevent.
	Components []string

	// Rearm is passed as p_rearm to farm.lease_reclaim: how long a slot stays
	// unschedulable after a reclaim so the previous holder's sockets are
	// certainly severed. It MUST exceed the node proxy's self-fence timeout.
	Rearm time.Duration

	CallTimeout time.Duration
	CensusEvery time.Duration
	LockKey     int64
	Logger      *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.GapFloor <= 0 {
		c.GapFloor = DefaultGapFloor
	}
	if len(c.Components) == 0 {
		c.Components = lease.ReaperComponents
	}
	if c.Rearm <= 0 {
		c.Rearm = lease.DefaultRearm
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.CensusEvery <= 0 {
		c.CensusEvery = DefaultCensusEvery
	}
	if c.LockKey == 0 {
		c.LockKey = DefaultLockKey
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Reaper is the suspect sweep and the automatic release path.
type Reaper struct {
	cfg Config
	log *slog.Logger

	lead       leadership
	nextCensus time.Time
}

// New validates cfg and returns a Reaper.
func New(cfg Config) (*Reaper, error) {
	if cfg.Pool == nil {
		return nil, errors.New("reaper: Config.Pool is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("reaper: Config.Store is required")
	}
	cfg.applyDefaults()
	return &Reaper{
		cfg:  cfg,
		log:  cfg.Logger.With("component", cfg.Component),
		lead: leadership{pool: cfg.Pool, key: cfg.LockKey, log: cfg.Logger},
	}, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation. A SIGTERM stops the sweep and releases
// nothing: leases that would have been reclaimed simply are not, which is the
// safe direction to fail in. The replacement process arms again on startup, and
// its arm refunds the gap this shutdown created.
func (r *Reaper) Run(ctx context.Context) error {
	defer r.lead.release(ctx)

	r.log.Info("reaper loop starting",
		"interval", r.cfg.Interval, "batch", r.cfg.Batch,
		"gap_floor", r.cfg.GapFloor, "components", r.cfg.Components, "rearm", r.cfg.Rearm)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("reaper loop stopping; no lease is released on the way out")
			return nil
		case <-timer.C:
		}

		r.cycle(ctx)
		timer.Reset(jitter(r.cfg.Interval))
	}
}

func (r *Reaper) cycle(ctx context.Context) {
	cyclesTotal.Inc()
	r.beat(ctx)

	// The census runs from every replica, leader or not. It is read-only, and a
	// standby that can answer /metrics with the truth is strictly better than
	// one that reports nothing.
	if time.Now().After(r.nextCensus) {
		r.census(ctx)
		r.nextCensus = time.Now().Add(r.cfg.CensusEvery)
	}

	gained, leader, err := r.lead.ensure(ctx, r.cfg.CallTimeout)
	if err != nil {
		r.log.Warn("reaper leadership check failed; not sweeping this cycle", "err", err)
		leaderGauge.Set(0)
		return
	}
	if !leader {
		leaderGauge.Set(0)
		return
	}
	leaderGauge.Set(1)

	if gained {
		// ARM BEFORE SWEEPING. Not a nicety and not reorderable: until this
		// returns, every live lease is still carrying the deadline it had
		// before our outage, and the quiesce gate that stops a restored control
		// plane from mass-reclaiming is not yet set.
		if err := r.arm(ctx); err != nil {
			r.log.Error("reaper could not arm; refusing to sweep", "err", err)
			// Stand down rather than sweep unarmed. A reaper that sweeps
			// without a refund charges tenants for our downtime, and the
			// leases most at risk are the ones that were silent the longest —
			// the long jobs.
			r.lead.release(ctx)
			leaderGauge.Set(0)
			return
		}
	}

	r.sweepSuspect(ctx)
	r.sweepMaxRuntime(ctx)
	r.sweepReclaim(ctx)
}

// arm calls farm.reaper_arm and records the refund.
func (r *Reaper) arm(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	gap, err := r.cfg.Store.ReaperArm(cctx, r.cfg.Components, r.cfg.GapFloor)
	if err != nil {
		armFailures.Inc()
		return fmt.Errorf("reaper: arm: %w", err)
	}
	armsTotal.Inc()

	if gap <= 0 {
		r.log.Info("reaper armed; no control-plane gap to refund", "components", r.cfg.Components)
		return nil
	}

	// Label the histogram with the component that was actually oldest, which is
	// the one farm.reaper_arm recorded. Guessing "reaper" here would hide the
	// case the gap accounting exists for: a dead API next to a healthy reaper.
	component := r.lastGapComponent(ctx)
	obs.ControlPlaneGap(component, gap)
	r.log.Warn("control-plane gap refunded to every live lease",
		"gap", gap, "oldest_component", string(component))
	return nil
}

// lastGapComponent reads back the row farm.reaper_arm just inserted. On any
// doubt it returns an invalid Component, which obs folds to "unknown" — an
// honest label beats a guessed one.
func (r *Reaper) lastGapComponent(ctx context.Context) obs.Component {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	var name string
	err := r.cfg.Pool.QueryRow(cctx,
		`SELECT component FROM farm.control_plane_gap ORDER BY ended_at DESC LIMIT 1`).Scan(&name)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			r.log.Debug("could not read back the control-plane gap component", "err", err)
		}
		return obs.Component("")
	}
	return obs.Component(name)
}

// sweepSuspect moves overdue leases from held to suspect.
//
// ENTERING SUSPECT DOES NOTHING. No reset, no reboot, no reallocation, no
// release. The device stays unschedulable and stays with its holder, and a
// heartbeat anywhere in the grace band self-heals it back to held at the same
// fence with zero work lost. Every row here is an alert.
func (r *Reaper) sweepSuspect(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	rows, err := r.cfg.Store.MarkSuspect(cctx, r.cfg.Batch)
	if err != nil {
		if ctx.Err() == nil {
			sweepErrors.WithLabelValues("suspect").Inc()
			r.log.Warn("suspect sweep failed", "err", err)
		}
		return
	}
	for _, l := range rows {
		if l.Protected {
			// A protected lease is never auto-reclaimed. It will sit here until
			// a human resolves it, which is correct and is also why nobody
			// notices without a log line and the farm_lease_suspect{protected}
			// gauge behind it.
			suspectTotal.WithLabelValues("protected").Inc()
			r.log.Warn("PROTECTED lease went suspect; it will be held indefinitely until a human acts",
				"lease", l.LeaseID, "job", l.JobID, "device", l.DeviceID)
			continue
		}
		suspectTotal.WithLabelValues("unprotected").Inc()
		r.log.Info("lease went suspect; nothing has been released and a heartbeat still heals it",
			"lease", l.LeaseID, "job", l.JobID, "device", l.DeviceID)
	}
}

// sweepMaxRuntime ends leases whose job outran farm.jobs.max_runtime.
func (r *Reaper) sweepMaxRuntime(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	rows, err := r.cfg.Store.ExpireMaxRuntime(cctx, r.cfg.Batch)
	if err != nil {
		if ctx.Err() == nil {
			sweepErrors.WithLabelValues("max_runtime").Inc()
			r.log.Warn("max-runtime sweep failed", "err", err)
		}
		return
	}
	for _, l := range rows {
		// Counted once per ROW, never once per attempt: the SQL is
		// LIMIT-bounded and uses SKIP LOCKED, so an attempt that lost a race
		// ended nothing.
		endedTotal.WithLabelValues("max_runtime").Inc()
		obs.LeaseReaped(obs.ReasonMaxRuntime)
		r.log.Info("lease ended at the user's own max_runtime",
			"lease", l.LeaseID, "job", l.JobID, "device", l.DeviceID)
	}
}

// sweepReclaim is the automatic release path.
//
// Every row it returns is work the control plane destroyed. In a healthy farm
// this is flat at zero, which is why it is logged at WARN and alerted on.
func (r *Reaper) sweepReclaim(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	rows, err := r.cfg.Store.Reclaim(cctx, r.cfg.Batch, r.cfg.Rearm)
	if err != nil {
		if ctx.Err() == nil {
			sweepErrors.WithLabelValues("reclaim").Inc()
			r.log.Warn("reclaim sweep failed", "err", err)
		}
		return
	}
	if len(rows) == 0 {
		// Either nothing was reclaimable or the quiesce gate is still closed.
		// Both are server-side decisions inside farm.lease_reclaim; there is
		// deliberately no Go-side condition that could second-guess them.
		return
	}

	for _, l := range rows {
		endedTotal.WithLabelValues("holder_expired").Inc()
		obs.LeaseReaped(obs.ReasonHolderExpired)
		r.log.Warn("RECLAIMED a lease: its holder went silent for ttl+grace across every watched component",
			"lease", l.LeaseID, "job", l.JobID, "device", l.DeviceID,
			"old_fence", l.OldFence, "new_floor", l.NewFloor)
	}
	r.auditReclaims(ctx, rows)
}

// auditReclaims writes one farm.events row per reclaimed lease.
//
// farm.lease_reclaim deliberately writes no events — it runs as farm_reaper
// inside its own transaction and its job is to be minimal. The forensic record
// belongs here, where the device, slot, job and both fences are already in hand
// and GET /api/v1/events can find them.
func (r *Reaper) auditReclaims(ctx context.Context, rows []lease.ReclaimedLease) {
	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
VALUES ('lease_reclaimed', $1::uuid, $2::bigint, $3::uuid, $4::uuid, $5::text,
        jsonb_build_object('old_fence', $6::bigint, 'new_floor', $7::bigint,
                           'release_reason', 'holder_expired'))`

	// One statement per row inside a single batch: one round trip, and every
	// parameter keeps its own type. slot_id is a *int64 because a lease need
	// not be bound to a slot, and a NULL there is a fact worth recording
	// rather than a zero worth inventing.
	batch := &pgx.Batch{}
	for _, l := range rows {
		batch.Queue(q, l.DeviceID, l.SlotID, l.LeaseID, l.JobID,
			r.cfg.Component, l.OldFence, l.NewFloor)
	}

	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	if err := r.cfg.Pool.SendBatch(cctx, batch).Close(); err != nil && ctx.Err() == nil {
		// The leases are already closed; only the audit trail suffered. Say so
		// loudly rather than retrying, because a retry would double-write.
		auditFailures.Inc()
		r.log.Error("could not write reclaim audit events", "count", len(rows), "err", err)
	}
}

// census publishes the lease gauges.
func (r *Reaper) census(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	if held, err := r.heldCensus(cctx); err != nil {
		r.log.Debug("held-lease census failed", "err", err)
	} else {
		obs.SetLeaseHeld(held)
	}
	if suspect, err := r.suspectCensus(cctx); err != nil {
		r.log.Debug("suspect-lease census failed", "err", err)
	} else {
		obs.SetLeaseSuspect(suspect)
	}
	if rearm, err := r.rearmCensus(cctx); err != nil {
		r.log.Debug("slot rearm census failed", "err", err)
	} else {
		obs.SetSlotRearmPending(rearm)
	}
}

func (r *Reaper) heldCensus(ctx context.Context) ([]obs.LeaseCount, error) {
	const q = `
SELECT j.pool_id, l.tenant_id, count(*)
  FROM farm.leases l JOIN farm.jobs j ON j.id = l.job_id
 WHERE l.state = 'held'
 GROUP BY 1, 2`

	rows, err := r.cfg.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []obs.LeaseCount
	for rows.Next() {
		var c obs.LeaseCount
		if err := rows.Scan(&c.Pool, &c.Tenant, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Reaper) suspectCensus(ctx context.Context) ([]obs.SuspectCount, error) {
	const q = `
SELECT j.pool_id, l.tenant_id, l.protected, count(*)
  FROM farm.leases l JOIN farm.jobs j ON j.id = l.job_id
 WHERE l.state = 'suspect'
 GROUP BY 1, 2, 3`

	rows, err := r.cfg.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []obs.SuspectCount
	for rows.Next() {
		var c obs.SuspectCount
		if err := rows.Scan(&c.Pool, &c.Tenant, &c.Protected, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Reaper) rearmCensus(ctx context.Context) ([]obs.SlotRearmCount, error) {
	const q = `
SELECT s.host_id, hb.usb_path, count(*)
  FROM farm.slots s JOIN farm.hubs hb ON hb.id = s.hub_id
 WHERE s.rearm_at > now()
 GROUP BY 1, 2`

	rows, err := r.cfg.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []obs.SlotRearmCount
	for rows.Next() {
		var c obs.SlotRearmCount
		if err := rows.Scan(&c.Host, &c.Hub, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Reaper) beat(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	if err := r.cfg.Store.ComponentBeat(cctx, r.cfg.Component); err != nil && ctx.Err() == nil {
		beatFailures.Inc()
		r.log.Warn("component heartbeat failed", "err", err)
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(int64(d)/4+1))
}

// ---------------------------------------------------------------------------
// Leader election
// ---------------------------------------------------------------------------

// leadership is the same session-advisory-lock pattern as
// internal/scheduler.leadership, and the full rationale for the dedicated
// connection is documented there: a session lock taken through a pool is a lock
// whose lifetime is unrelated to the work it guards.
//
// The reaper needs it for a reason the scheduler does not have. farm.reaper_arm
// inserts a farm.control_plane_gap row and ADDS the gap to every live lease's
// deadlines. Two reapers arming at once would refund the same outage twice, and
// two reapers restarting in a loop would keep extending every lease in the farm
// until nothing was ever reclaimable. One arm per outage requires one reaper.
type leadership struct {
	pool *pgxpool.Pool
	key  int64
	log  *slog.Logger

	conn *pgxpool.Conn
	held bool
}

// ensure reports whether this process holds the reaper lock, and whether it
// gained it on THIS call — the signal that farm.reaper_arm must run before any
// sweep.
func (l *leadership) ensure(ctx context.Context, timeout time.Duration) (gained, leader bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if l.conn != nil {
		if perr := l.conn.Ping(cctx); perr != nil {
			// The session is gone, so the lock is gone with it. Standing down
			// here is what makes the next successful ensure report gained=true
			// and re-arm: a reaper that lost the database was blind, and a
			// blind reaper must refund before it acts.
			l.log.Warn("reaper leadership connection died; standing down", "err", perr)
			l.drop()
			return false, false, nil
		}
		if l.held {
			return false, true, nil
		}
	}

	if l.conn == nil {
		c, aerr := l.pool.Acquire(cctx)
		if aerr != nil {
			return false, false, fmt.Errorf("reaper: acquire leadership connection: %w", aerr)
		}
		l.conn = c
	}

	var ok bool
	if qerr := l.conn.QueryRow(cctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&ok); qerr != nil {
		l.drop()
		return false, false, fmt.Errorf("reaper: try advisory lock: %w", qerr)
	}
	if !ok {
		l.drop()
		return false, false, nil
	}
	l.held = true
	l.log.Info("reaper acquired leadership", "lock_key", l.key)
	return true, true, nil
}

func (l *leadership) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	if l.held {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := l.conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, l.key); err != nil {
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
		Namespace: "farm", Subsystem: "reaper", Name: "cycles_total",
		Help: "Reaper cycles, including cycles run by a standby that swept nothing.",
	})

	armsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "arms_total",
		Help: "farm.reaper_arm calls. One per gain of leadership, always before the first sweep.",
	})

	armFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "arm_failures_total",
		Help: "Failed farm.reaper_arm calls. The reaper stands down rather than sweep unarmed.",
	})

	// suspectTotal is an ALERTING signal only. Nothing was released.
	suspectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "suspect_marks_total",
		Help: "Leases moved held -> suspect. Suspect releases nothing and a heartbeat heals it; " +
			"protected=true rows are held indefinitely for a human.",
	}, []string{"protection"})

	// endedTotal duplicates obs.LeaseReaped on purpose: obs carries the whole
	// fleet's seven reasons, this one is scoped to what THIS loop did, so a
	// reaper that has started ending leases is visible without correlating.
	endedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "leases_ended_total",
		Help: "Leases ended by this loop. holder_expired is work destroyed by the control plane " +
			"and should be flat at zero.",
	}, []string{"reason"})

	sweepErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "sweep_errors_total",
		Help: "Sweeps that failed. A failed sweep releases nothing, which is the safe direction.",
	}, []string{"sweep"})

	auditFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "audit_failures_total",
		Help: "Reclaims that were not written to farm.events. The leases are closed; the trail is not.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. A sustained rate becomes a refunded control-plane gap.",
	})

	leaderGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "reaper", Name: "leader",
		Help: "1 when this replica holds the reaper advisory lock. Sum across replicas must be 1.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
//
// The two series that mean "work was destroyed" are created at zero here, so
// `increase(farm_reaper_leases_ended_total{reason="holder_expired"}[15m]) > 0`
// is armed from the first scrape rather than from the first casualty.
func Collectors() []prometheus.Collector {
	endedTotal.WithLabelValues("holder_expired")
	endedTotal.WithLabelValues("max_runtime")
	suspectTotal.WithLabelValues("protected")
	suspectTotal.WithLabelValues("unprotected")
	for _, s := range []string{"suspect", "max_runtime", "reclaim"} {
		sweepErrors.WithLabelValues(s)
	}
	return []prometheus.Collector{
		cyclesTotal, armsTotal, armFailures, suspectTotal, endedTotal,
		sweepErrors, auditFailures, beatFailures, leaderGauge,
	}
}
