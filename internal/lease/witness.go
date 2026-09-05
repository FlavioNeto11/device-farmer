package lease

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The witness loop: the second, slower way a lease says "I am still here".
//
// # What it is for
//
// The renewal loop answers "does the holder process exist?" against Postgres.
// The witness answers a different question — "is the WORK still running on the
// device?" — and it answers it from evidence gathered on the device itself: a
// marker file the job's own agent keeps touching (internal/runner writes it).
// farm.lease_reclaim ignores any lease whose witness_at is inside one grace
// period, so a job that can still prove it is working keeps its device even
// while its heartbeats are not landing.
//
// Without this, a job that loses its control-plane connection has only the
// grace band, and a control-plane outage longer than TTL+grace costs it the
// device. That is DeviceFarmer/STF #663 arriving by a different road: the work
// is fine, the device is fine, and the allocation is taken away anyway.
//
// # What it is not for
//
// It is not a health probe and must never become one. Presenting a witness
// says "our agent touched the marker", which is a fact about our own work, not
// about whether the device answers pings. Nothing here reads device health,
// and nothing here can end anything:
//
//   - A REFUSED witness (ok=false) is not a fencing event. The cap is spent,
//     the fence is stale, or the lease is no longer live — the loop records it
//     and keeps going. Only Renew may report fencing.
//   - A witness that ERRORS is a transport failure and is retried on the next
//     tick, inside the lease the job still holds.
//   - Evidence that has gone stale (the device was unreachable, so the agent
//     could not refresh the marker) means we present NOTHING. We do not
//     manufacture proof we do not have, and not presenting it ends nothing
//     either.
//
// So this file contains no call to cancel, Stop or Release, and there is no
// path from a witness outcome to one.
//
// # Why it runs even while renewals are landing
//
// A witness is presented on every tick that has fresh evidence, not only after
// the renewal path has started failing. Nobody can know in advance which tick
// is the last one before the outage, and a loop that waited for trouble would
// have to reach the database at the moment the database is the thing that
// broke. The cost of being early is one UPDATE per interval whose deadline is
// clamped by GREATEST() and whose counter the next renewal zeroes; the cost of
// being late is a device.

// Witness-loop defaults.
const (
	// DefaultWitnessInterval mirrors config.DefaultLeaseWitnessInterval. The
	// cadence must land at least twice inside a grace band — farm.lease_reclaim
	// only honours a witness younger than one grace period, so a witness that
	// lands once per band protects nothing the moment one round trip is lost.
	// internal/config validates that relationship against the operator's own
	// grace; this constant is only the fallback when a caller sets no cadence.
	DefaultWitnessInterval = 2 * time.Minute

	// DefaultWitnessTimeout bounds one witness round trip, for the same reason
	// the renewal path is bounded: a wedged connection must not consume the
	// whole interval and turn one hung socket into no evidence at all.
	DefaultWitnessTimeout = 10 * time.Second
)

// Evidence is the on-device proof a witness stands on.
//
// Implemented by the thing that actually writes the marker file on the device
// — *runner.Marker — so that presenting a witness is impossible without a
// component that has genuinely touched the device. WitnessedAt reports when
// that last succeeded; false means there is no proof to present right now, and
// the loop then presents none.
type Evidence interface {
	WitnessedAt() (time.Time, bool)
}

// EvidenceFunc adapts a plain function to Evidence.
type EvidenceFunc func() (time.Time, bool)

// WitnessedAt implements Evidence.
func (f EvidenceFunc) WitnessedAt() (time.Time, bool) { return f() }

// WitnessHooks are optional observation callbacks, so metrics can be wired
// from internal/obs without this package importing it.
//
// Every hook runs synchronously on the witness goroutine. Keep them fast, and
// do not call Holder.Stop or Holder.Release from one: nothing a witness
// observes is a reason to end a lease, and a hook that ended one would put the
// refusal path back on a road to allocation.
//
// WitnessLoop.Stop is equally off limits from a hook, for a duller reason: it
// waits for the goroutine the hook is running on, so calling it there wedges
// the loop rather than stopping it. Signal a supervisor and let it call Stop.
type WitnessHooks struct {
	// OnWitnessed fires after an accepted witness. consecutive is how many
	// witness-only extensions have been accepted since the last renewal landed.
	OnWitnessed func(l Lease, reclaimableAt time.Time, consecutive int)

	// OnRefused fires when the server declined the witness: the extension cap
	// is spent, the lease is no longer live, or the fence is stale. Worth a
	// counter; never worth aborting a job.
	OnRefused func(l Lease)

	// OnError fires when the witness round trip failed. A health signal about
	// the control plane, not about the job.
	OnError func(l Lease, err error)

	// OnSkipped fires when no witness was presented because the on-device
	// evidence was missing or older than MaxEvidenceAge. age is zero when
	// there was no evidence at all.
	OnSkipped func(l Lease, age time.Duration)
}

// WitnessConfig configures a WitnessLoop. The zero value is valid.
//
// The extension cap is deliberately NOT here: it lives in HolderConfig, is
// presented on every call by Holder.Witness, and is enforced server-side by
// farm.lease_witness. One cap, in one place, honoured on both sides.
type WitnessConfig struct {
	// Interval is the cadence. Operators set it through
	// FARM_LEASE_WITNESS_INTERVAL, which internal/config validates against the
	// grace band.
	Interval time.Duration

	// Timeout bounds one witness round trip.
	Timeout time.Duration

	// MaxEvidenceAge is how old the on-device marker may be and still be worth
	// presenting. The jobrunner, which starts every witness loop in a running
	// farm, sets it from the MARKER's cadence (config.MaxEvidenceAgeFor: a
	// few marker intervals, so a couple of lost writes are tolerated and a
	// device nobody has reached since the last tick is not). The fallback
	// here, three of this loop's own intervals, is only for a caller with no
	// marker cadence to derive from.
	//
	// This is an elapsed-time measurement between two readings of our own
	// clock, and it decides only whether we have something to say. It is not a
	// deadline: whether the lease is still valid is decided by Postgres against
	// its own now(), here as everywhere.
	MaxEvidenceAge time.Duration

	Logger *slog.Logger
	Hooks  WitnessHooks
}

func (c *WitnessConfig) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultWitnessInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultWitnessTimeout
	}
	if c.MaxEvidenceAge <= 0 {
		c.MaxEvidenceAge = 3 * c.Interval
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// WitnessStats is a snapshot of witness-loop health.
type WitnessStats struct {
	// Presented counts round trips actually issued; Accepted, Refused and
	// Errors partition them.
	Presented uint64
	Accepted  uint64
	Refused   uint64
	Errors    uint64

	// Skipped counts ticks where there was no fresh on-device evidence, so
	// nothing was presented.
	Skipped uint64

	// Consecutive is the number of accepted witness-only extensions since the
	// last renewal landed — the same quantity farm.leases.witness_extensions
	// holds, and the one the cap applies to.
	Consecutive int

	// CapSpent reports that Consecutive has reached the holder's cap, so this
	// loop is presenting nothing until a renewal resets it.
	CapSpent bool

	// LastWitnessAt is the reclaimable_at the server last returned.
	LastWitnessAt time.Time
}

// WitnessLoop presents a witness on a timer, beside the renewal loop and
// sharing nothing with it but the Holder.
type WitnessLoop struct {
	h   *Holder
	ev  Evidence
	cfg WitnessConfig

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	mu        sync.Mutex
	stats     WitnessStats
	capLogged bool
}

// StartWitness starts a witness loop for this holder and returns it running.
//
// ev is required: Holder.Witness may only be called by code that has actual
// device-side evidence, so a loop with nothing to gather evidence from is
// refused here rather than allowed to invent it. Call Stop when the job is
// done; the loop also exits on its own when the holder's context ends, so a
// fenced or released lease stops being witnessed without anyone remembering to
// say so.
func (h *Holder) StartWitness(ev Evidence, cfg WitnessConfig) (*WitnessLoop, error) {
	if h == nil {
		return nil, errors.New("lease: witness loop needs a holder")
	}
	if ev == nil {
		return nil, errors.New("lease: witness loop needs on-device evidence; a witness presented " +
			"without it would be a health probe wearing a holder's name")
	}
	cfg.applyDefaults()

	w := &WitnessLoop{
		h:    h,
		ev:   ev,
		cfg:  cfg,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// Stats returns a snapshot of the loop's health.
func (w *WitnessLoop) Stats() WitnessStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Stop halts the loop and blocks until its goroutine has exited. It is
// idempotent, safe on a nil loop, and does not touch the lease: stopping the
// witness stops producing evidence, and nothing else.
func (w *WitnessLoop) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

// run is the loop: one ticker, one question, and no branch that ends anything.
func (w *WitnessLoop) run() {
	defer close(w.done)

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	// The holder's renewal count is how this loop learns that a renewal landed.
	// It is polled rather than pushed because HolderHooks run on the renewal
	// goroutine and must stay fast; a counter comparison costs one mutex and
	// cannot wedge the path that keeps the lease alive.
	renewals := w.h.Stats().Renewals

	for {
		select {
		case <-w.stop:
			return
		case <-w.h.Done():
			// No lease, nothing to witness. Which lease ending this was —
			// fencing, release, shutdown — is not this loop's business.
			return
		case <-ticker.C:
		}

		// A second, non-blocking look at the same two doors. select picks at
		// random among the cases that are ready, so a tick that comes due in
		// the same instant the holder lets go of the lease has an even chance
		// of being served first. Presenting a witness after that point cannot
		// end anything — but on the Stop path (SIGTERM, node drain) the lease
		// deliberately stays live at the same fence, so a late witness would
		// push its reclaimable_at out and hold a device this process is in the
		// middle of walking away from.
		select {
		case <-w.stop:
			return
		case <-w.h.Done():
			return
		default:
		}

		if n := w.h.Stats().Renewals; n != renewals {
			renewals = n
			// The server zeroed farm.leases.witness_extensions on that renewal,
			// so the cap counts CONSECUTIVE witness-only extensions. Mirroring
			// the reset here is what stops a long job from spending its twelve
			// extensions in the first half hour and having no protection left
			// for the outage it eventually meets.
			w.resetExtensions()
		}
		w.presentOnce()
	}
}

// presentOnce issues at most one witness.
func (w *WitnessLoop) presentOnce() {
	l := w.h.Lease()

	age, fresh := w.evidenceAge()
	if !fresh {
		w.countSkipped()
		if hook := w.cfg.Hooks.OnSkipped; hook != nil {
			hook(l, age)
		}
		return
	}

	consecutive, allowed := w.reserve()
	if !allowed {
		// The cap is spent and the server would refuse anyway. Say so once,
		// then stay quiet: a line per tick would bury the renewal failures that
		// are the actual reason this loop is carrying the lease.
		if w.logCapOnce() {
			w.cfg.Logger.Warn("witness extension cap reached; no further witness until a renewal lands "+
				"(the lease is untouched and the job keeps running)",
				"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
				"fence", l.Fence, "consecutive_extensions", consecutive)
		}
		return
	}

	// The call context is deliberately detached from the holder's cancellation
	// and bounded by our own timeout, matching Holder.Witness's contract: a
	// witness already in flight when the work starts winding down is worth
	// finishing, and a wedged one must not eat the interval.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.h.Context()), w.cfg.Timeout)
	defer cancel()

	at, ok, err := w.h.Witness(ctx)
	switch {
	case err != nil:
		// TRANSIENT. The lease is untouched; the next tick tries again.
		w.countError()
		w.cfg.Logger.Warn("witness failed, will retry (lease NOT lost)",
			"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
			"fence", l.Fence, "evidence_age", age, "err", err)
		if hook := w.cfg.Hooks.OnError; hook != nil {
			hook(l, err)
		}

	case !ok:
		// REFUSED, which is a statement about the extension cap, the fence or
		// the lease's state — never about whether this job may continue. Only
		// Renew reports fencing, and this branch is the one place where getting
		// that wrong would rebuild #663 out of on-device evidence.
		w.countRefused()
		w.cfg.Logger.Info("witness refused; the job continues unchanged",
			"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
			"fence", l.Fence, "consecutive_extensions", consecutive)
		if hook := w.cfg.Hooks.OnRefused; hook != nil {
			hook(l)
		}

	default:
		accepted := w.countAccepted(at)
		// Info only while the witness is load-bearing — that is, while the
		// renewal loop is failing and this is the only thing still telling the
		// reaper the job is alive. In the steady state a witness lands on every
		// tick by design, and a fleet of sixty devices logging that at Info
		// would bury the renewal failures these lines exist to sit beside.
		failures := w.h.Stats().ConsecutiveFailures
		level := slog.LevelDebug
		if failures > 0 {
			level = slog.LevelInfo
		}
		w.cfg.Logger.Log(context.Background(), level,
			"witness accepted; the reaper will not consider this lease",
			"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
			"fence", l.Fence, "reclaimable_at", at,
			"consecutive_extensions", accepted, "evidence_age", age,
			"renewal_failures", failures)
		if hook := w.cfg.Hooks.OnWitnessed; hook != nil {
			hook(l, at, accepted)
		}
	}
}

// evidenceAge reports how long ago the device-side agent last refreshed the
// marker, and whether that is recent enough to present.
func (w *WitnessLoop) evidenceAge() (time.Duration, bool) {
	at, ok := w.ev.WitnessedAt()
	if !ok || at.IsZero() {
		return 0, false
	}
	age := time.Since(at)
	if age < 0 {
		// A clock that moved backwards under us. Evidence gathered "in the
		// future" is still evidence that our agent ran; treat it as fresh
		// rather than silently stopping.
		age = 0
	}
	return age, age <= w.cfg.MaxEvidenceAge
}

// reserve takes one extension from the cap, reporting the consecutive count
// and whether presenting is allowed at all.
func (w *WitnessLoop) reserve() (consecutive int, allowed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	limit := w.h.cfg.WitnessMaxExtensions
	if limit <= 0 {
		limit = DefaultWitnessMaxExtensions
	}
	if w.stats.Consecutive >= limit {
		w.stats.CapSpent = true
		return w.stats.Consecutive, false
	}
	w.stats.Presented++
	return w.stats.Consecutive, true
}

func (w *WitnessLoop) resetExtensions() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Consecutive = 0
	w.stats.CapSpent = false
	w.capLogged = false
}

func (w *WitnessLoop) countAccepted(at time.Time) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Accepted++
	w.stats.Consecutive++
	w.stats.LastWitnessAt = at
	return w.stats.Consecutive
}

// countRefused records a refusal. It deliberately does not touch Consecutive:
// nothing was extended, so nothing was spent, and the server's counter did not
// move either.
func (w *WitnessLoop) countRefused() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Refused++
}

func (w *WitnessLoop) countError() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Errors++
}

func (w *WitnessLoop) countSkipped() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Skipped++
}

// logCapOnce reports whether this is the first tick since the last reset on
// which the cap was found spent.
func (w *WitnessLoop) logCapOnce() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.capLogged {
		return false
	}
	w.capLogged = true
	return true
}
