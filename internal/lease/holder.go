package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Holder-side defaults.
//
// The renewal interval is a small fraction of the TTL on purpose. With a 15
// minute TTL and a 60 second interval, roughly fourteen consecutive renewals
// must fail before the lease even becomes suspect — and suspect still releases
// nothing. A database that is down for ten minutes costs a running job exactly
// nothing.
const (
	// DefaultRenewInterval is how often the holder asks Postgres "am I still
	// alive?". It is a local timer and nothing more; it never computes a
	// deadline.
	DefaultRenewInterval = 60 * time.Second

	// DefaultTTL documents the server-side default (jobs.ttl, minimum 10
	// minutes, default 15). The database owns the real value; this constant
	// exists so callers can sanity-check their interval against it.
	DefaultTTL = 15 * time.Minute

	// DefaultRenewTimeout bounds a single renewal attempt. A renewal that hangs
	// must not eat the whole interval, or one wedged connection turns into
	// silence at the server.
	DefaultRenewTimeout = 10 * time.Second

	// DefaultRetryBase and DefaultRetryMax bound the jittered backoff after a
	// TRANSIENT failure. The cap stays well under the renewal interval so a
	// database blip is retried more aggressively than the steady state, never
	// less.
	DefaultRetryBase = 1 * time.Second
	DefaultRetryMax  = 15 * time.Second

	// DefaultReleaseTimeout bounds the release round trip when the caller
	// supplied no deadline of its own. A release that never completes leaves
	// the device parked until the reaper takes it TTL+grace later, so this
	// should be generous enough to ride out a brief database hiccup and short
	// enough that shutdown still finishes.
	DefaultReleaseTimeout = 15 * time.Second
)

// Cancellation causes, readable via context.Cause(h.Context()).
var (
	// ErrHolderStopped means the renewal loop was stopped without releasing the
	// lease — the correct response to SIGTERM. Local work unwinds; the lease,
	// the device and the fence survive, and the replacement pod re-attaches to
	// them by job id.
	ErrHolderStopped = errors.New("lease: holder stopped")

	// ErrHolderReleased means the lease was deliberately released by this
	// holder.
	ErrHolderReleased = errors.New("lease: holder released the lease")
)

// HolderHooks are optional observation callbacks.
//
// They exist so that metrics and alerting can be wired up from internal/obs
// without this package importing it — the dependency graph around leases stays
// deliberately thin.
//
// Every hook runs synchronously on the renewal goroutine. Keep them fast, and
// never call Stop, Release or Witness from inside one: those join the loop and
// would deadlock. Spawn a goroutine if a hook needs to act on the holder.
type HolderHooks struct {
	// OnRenewed fires after every successful renewal. res.WasSuspect true means
	// a lease the sweeper had flagged just self-healed with no work lost —
	// worth a counter, never worth an alarm.
	OnRenewed func(l Lease, res RenewResult)

	// OnTransientError fires when a renewal failed for any reason other than
	// fencing. attempt is the count of consecutive failures and retryIn is the
	// jittered delay before the next try. The lease is untouched; this is a
	// health signal about the control plane, not about the job.
	OnTransientError func(l Lease, attempt int, err error, retryIn time.Duration)

	// OnFenced fires exactly once, immediately before the holder's context is
	// cancelled. By the time it returns, callers derived from Context() are
	// being torn down.
	OnFenced func(l Lease)
}

// leaseOps is the part of *Store that a Holder actually calls.
//
// It exists for exactly one reason. The ErrFenced-versus-transient branch in
// run() is the single most consequential decision in this project — getting it
// backwards is DeviceFarmer/STF #663 — and a branch that can only be exercised
// against a live Postgres is a branch that never gets exercised. The interface
// is unexported and NewHolder still takes a concrete *Store, so no consumer of
// this package sees any difference; but a test inside this package can drive
// the loop with a stub that returns zero-rows fencing, a transport failure and
// a cancelled context in turn, and assert that only the first one ends the job.
type leaseOps interface {
	Renew(ctx context.Context, leaseID string, fence int64, holderInstance string) (RenewResult, error)
	Release(ctx context.Context, leaseID string, fence int64, reason ReleaseReason, rearm time.Duration) (bool, error)
	Witness(ctx context.Context, leaseID string, fence int64, maxExtensions int) (time.Time, bool, error)
}

var _ leaseOps = (*Store)(nil)

// HolderConfig configures a Holder. The zero value is valid and yields the
// defaults above.
type HolderConfig struct {
	Interval             time.Duration
	RenewTimeout         time.Duration
	RetryBase            time.Duration
	RetryMax             time.Duration
	ReleaseTimeout       time.Duration
	WitnessMaxExtensions int
	Logger               *slog.Logger
	Hooks                HolderHooks
}

func (c *HolderConfig) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultRenewInterval
	}
	if c.RenewTimeout <= 0 {
		c.RenewTimeout = DefaultRenewTimeout
	}
	if c.RetryBase <= 0 {
		c.RetryBase = DefaultRetryBase
	}
	if c.RetryMax <= 0 {
		c.RetryMax = DefaultRetryMax
	}
	if c.RetryMax < c.RetryBase {
		c.RetryMax = c.RetryBase
	}
	if c.ReleaseTimeout <= 0 {
		c.ReleaseTimeout = DefaultReleaseTimeout
	}
	if c.WitnessMaxExtensions <= 0 {
		c.WitnessMaxExtensions = DefaultWitnessMaxExtensions
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Holder owns exactly one lease and keeps it alive.
//
// # Path separation is the mechanism, not a happy accident
//
// The renewal loop below talks to Postgres over a connection borrowed from the
// Store's pool. It never opens an ADB socket, never reads device health, never
// waits on device output, and shares no connection, no goroutine and no failure
// mode with the ADB data path. That is not tidiness — it is THE reason a
// six-hour job survives a ten-minute ADB partition:
//
//   - The renewal is answering "does the holder exist?", which is a question
//     about this process. Asking the device would make the answer depend on the
//     device, and that fusion is DeviceFarmer/STF #663.
//   - Because the two paths share nothing, an ADB partition produces exactly
//     one effect: adbwire returns a typed error to its caller and increments a
//     counter. The renewal loop does not notice, the heartbeat keeps landing,
//     the lease never becomes suspect, and the job resumes on the same device
//     at the same fence when the transport comes back.
//   - The dependency is one-way and CI-enforced: internal/adbwire may not
//     import internal/lease and contains no lease vocabulary. There is no
//     function in this package that a socket error could be passed to.
//
// # The two failure branches, which must never be swapped
//
//	ErrFenced          Terminal. The context is cancelled with ErrFenced as its
//	                   cause, so every ADB socket, every subprocess and every
//	                   derived worker unwinds at once. We do not own the device
//	                   any more; something else may already be driving it.
//	any other error    Transient. Retry with jittered backoff and DO NOT cancel.
//	                   A database blip is not a fencing event. Getting this
//	                   backwards recreates #663 with the database as the new
//	                   trigger instead of the socket.
type Holder struct {
	store leaseOps
	cfg   HolderConfig

	ctx    context.Context
	cancel context.CancelCauseFunc

	loopDone chan struct{}
	stopOnce sync.Once

	mu             sync.RWMutex
	lease          Lease
	lastRenewAt    time.Time
	renewals       uint64
	consecFailures int

	// fenced records that renew reported zero rows, independently of which
	// caller won the race to cancel the context. Without it, a Release or Stop
	// that lands microseconds before the loop notices the fencing wins
	// stopOnce, leaves the cause as ErrHolderReleased, and Fenced() then
	// answers "no" to the one question a supervisor most needs answered
	// truthfully: did I lose the device?
	fenced bool
}

// HolderStats is a snapshot of renewal-loop health, for dashboards and for the
// job supervisor's own logging.
type HolderStats struct {
	Renewals            uint64
	ConsecutiveFailures int
	LastRenewAt         time.Time
	ExpiresAt           time.Time
	ReclaimableAt       time.Time
}

// NewHolder takes ownership of an acquired lease and starts renewing it
// immediately.
//
// The returned Holder's Context is derived from parent, so cancelling parent
// stops renewal and unwinds the job — but note that this leaves the lease alive
// at the server until it is released or reclaimed, which is exactly what you
// want for a pod eviction and exactly what you do not want for a finished job.
// Call Release when the work is done; call Stop when the process is going away
// but the job is not.
func NewHolder(parent context.Context, store *Store, l Lease, cfg HolderConfig) *Holder {
	cfg.applyDefaults()

	ctx, cancel := context.WithCancelCause(parent)
	h := &Holder{
		store:    store,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		loopDone: make(chan struct{}),
		lease:    l,
	}
	go h.run()
	return h
}

// Context returns the context every unit of the job's work must derive from:
// ADB sessions, subprocesses, uploads, log tailers, all of it.
//
// It is cancelled when, and only when, this holder no longer holds the lease —
// on fencing, on release, on Stop, or when the parent context ends.
// context.Cause reports which: ErrFenced, ErrHolderReleased, ErrHolderStopped,
// or the parent's own cause.
//
// Deriving work from this context is what makes "abort the job and close every
// ADB socket" a single cancellation rather than a cleanup checklist someone has
// to remember to run.
func (h *Holder) Context() context.Context { return h.ctx }

// Done is a shorthand for Context().Done().
func (h *Holder) Done() <-chan struct{} { return h.ctx.Done() }

// Err returns the cause of the holder's termination, or nil while it is still
// holding the lease. Compare with errors.Is(err, ErrFenced) to distinguish
// losing the device from an orderly shutdown.
func (h *Holder) Err() error {
	if h.ctx.Err() == nil {
		return nil
	}
	return context.Cause(h.ctx)
}

// Fenced reports whether this holder lost its lease to fencing, as opposed to
// stopping for any orderly reason.
//
// It consults the loop's own record first and the cancellation cause second,
// because a concurrent Stop or Release can win the race to set the cause. The
// answer must not depend on that race: "we were fenced" and "we shut down
// tidily" call for opposite responses from the supervisor.
func (h *Holder) Fenced() bool {
	h.mu.RLock()
	fenced := h.fenced
	h.mu.RUnlock()
	return fenced || errors.Is(context.Cause(h.ctx), ErrFenced)
}

// Lease returns a snapshot of the lease, with deadlines as of the most recent
// successful renewal.
//
// Those deadlines are a cached copy of server state. Do not compare them
// against the local clock to decide whether the lease is still valid: that
// question is answered by Renew, against Postgres' now(), and by nothing else.
func (h *Holder) Lease() Lease {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lease
}

// Stats returns a snapshot of renewal-loop health.
func (h *Holder) Stats() HolderStats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return HolderStats{
		Renewals:            h.renewals,
		ConsecutiveFailures: h.consecFailures,
		LastRenewAt:         h.lastRenewAt,
		ExpiresAt:           h.lease.ExpiresAt,
		ReclaimableAt:       h.lease.ReclaimableAt,
	}
}

// Witness records on-device proof that this holder is alive, extending
// reclaimable_at by one grace period.
//
// Call it from the code that has actual device-side evidence — the holder's own
// agent touching a marker file — not from a health probe. ok is false when the
// witness was refused (extension cap exhausted, stale fence, lease no longer
// live); that is not fencing and never aborts the job.
//
// ctx is the caller's, deliberately not the holder's, so a witness can still be
// attempted while work is winding down.
func (h *Holder) Witness(ctx context.Context) (time.Time, bool, error) {
	l := h.Lease()
	at, ok, err := h.store.Witness(ctx, l.ID, l.Fence, h.cfg.WitnessMaxExtensions)
	if err != nil || !ok {
		return time.Time{}, false, err
	}

	h.mu.Lock()
	// Monotonic, mirroring the server's GREATEST(): a witness may only push a
	// deadline outward.
	if at.After(h.lease.ReclaimableAt) {
		h.lease.ReclaimableAt = at
	}
	h.mu.Unlock()
	return at, true, nil
}

// Stop halts renewal and cancels the holder's context WITHOUT releasing the
// lease. It blocks until the renewal loop has exited, and is idempotent.
//
// This is the correct response to SIGTERM, to a node drain, and to any other
// eviction. The lease, the device and the fence stay exactly as they are; the
// replacement pod calls Acquire with the same job id, re-attaches at the same
// fence, and resumes from the job's checkpoint. Releasing on shutdown instead
// would hand a half-finished job's device to somebody else.
func (h *Holder) Stop() { h.stop(ErrHolderStopped) }

// Release ends the lease deliberately and stops renewal. It is idempotent, and
// returns whether this call was the one that actually released.
//
// The holder's context is cancelled BEFORE the release is issued. That ordering
// matters: release bumps the device's fence_floor and starts the slot's rearm
// window, after which the device can be handed to another job within seconds.
// Local work must be severed first, not concurrently.
//
// reason must be one of the seven ReleaseReason constants. A connectivity
// reason is refused by the database with *CheckViolationError; if a transport
// failure is what ended the run, the honest reason is ReasonFailed, and the
// honest response to a transport failure that has not ended the run is to keep
// holding the device.
func (h *Holder) Release(ctx context.Context, reason ReleaseReason, rearm time.Duration) (bool, error) {
	l := h.Lease()
	h.stop(ErrHolderReleased)

	// The release must survive the cancellation it just caused. Callers
	// naturally pass a context descended from Context() — that is the context
	// their whole job runs under — and cancelling first would then abort this
	// very call, leaving the device parked until the reaper takes it TTL+grace
	// later. So we keep the caller's values and any deadline they set
	// deliberately, and drop only the cancellation.
	rctx := context.WithoutCancel(ctx)
	var cancel context.CancelFunc
	// An inherited deadline is honoured only while it still has budget. The
	// most ordinary way to reach this line is a job being torn down BECAUSE its
	// own deadline just elapsed, and re-imposing a deadline already in the past
	// would kill the release before it reached the wire — parking the device
	// for TTL+grace, which is precisely what the WithoutCancel above exists to
	// prevent. A spent deadline is not an instruction to skip the release.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) > 0 {
		rctx, cancel = context.WithDeadline(rctx, deadline)
	} else {
		rctx, cancel = context.WithTimeout(rctx, h.cfg.ReleaseTimeout)
	}
	defer cancel()

	released, err := h.store.Release(rctx, l.ID, l.Fence, reason, rearm)
	if err != nil {
		return false, err
	}
	if !released {
		// The UPDATE matched no live row at (id, fence): somebody else ended
		// this lease first — the reaper, an operator, or a re-attached
		// replacement that fenced us. Not an error and not lost work, but the
		// supervisor must not read the ordinary "lease released" line and
		// conclude it completed the handover itself.
		h.cfg.Logger.Warn("lease release matched nothing; lease already ended or fence stale",
			"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
			"fence", l.Fence, "reason", string(reason), "fenced", h.Fenced())
		return false, nil
	}
	h.cfg.Logger.Info("lease released",
		"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
		"fence", l.Fence, "reason", string(reason))
	return true, nil
}

// stop cancels with the given cause and waits for the renewal goroutine, so
// that no renewal can be in flight after it returns.
func (h *Holder) stop(cause error) {
	h.stopOnce.Do(func() { h.cancel(cause) })
	<-h.loopDone
}

// run is the renewal loop: one timer, one question, two outcomes.
func (h *Holder) run() {
	defer close(h.loopDone)

	// A timer rather than a ticker: after a transient failure the next attempt
	// is scheduled by backoff, not by the steady cadence. Go timers are
	// monotonic, so an NTP step cannot make us skip a renewal — and even if the
	// process were frozen straight through the TTL, the server-side grace band
	// and the control-plane gap refund are what cover it. The local clock only
	// decides WHEN to ask; Postgres decides what the answer means.
	timer := time.NewTimer(h.cfg.Interval)
	defer timer.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-timer.C:
		}

		res, err := h.renewOnce()

		switch {
		case err == nil:
			l := h.onRenewed(res)
			if hook := h.cfg.Hooks.OnRenewed; hook != nil {
				hook(l, res)
			}
			if res.WasSuspect {
				// Self-healed at the same fence, no work lost. Loud enough to
				// notice, quiet enough not to page.
				h.cfg.Logger.Warn("lease self-healed from suspect",
					"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
					"fence", l.Fence, "expires_at", res.ExpiresAt)
			}
			timer.Reset(h.cfg.Interval)

		case errors.Is(err, ErrFenced):
			// TERMINAL. Somebody else owns this device now, or is about to.
			// Cancelling the context is what closes every ADB socket derived
			// from it; there is nothing to retry and nothing safe to write.
			//
			// Record the fact before cancelling, so Fenced() is truthful even
			// when a concurrent Stop or Release wins stopOnce and sets a
			// different cause.
			l := h.markFenced()
			h.cfg.Logger.Error("lease FENCED, aborting job",
				"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
				"fence", l.Fence, "err", err)
			if hook := h.cfg.Hooks.OnFenced; hook != nil {
				hook(l)
			}
			h.stopOnce.Do(func() { h.cancel(fmt.Errorf("lease %s: %w", l.ID, ErrFenced)) })
			return

		case h.ctx.Err() != nil:
			// We are shutting down and the attempt died with us. Not a failure,
			// and emphatically not a fencing event.
			return

		default:
			// TRANSIENT. The lease is untouched: no deadline moved, no state
			// changed, the device is still ours. Retry, and do not cancel — a
			// database blip that killed running jobs would be #663 rebuilt on
			// the control plane instead of the socket.
			attempt, l := h.onTransientError()
			retryIn := h.backoff(attempt)
			h.cfg.Logger.Warn("lease renewal failed, retrying (lease NOT lost)",
				"lease_id", l.ID, "device_id", l.DeviceID, "job_id", l.JobID,
				"attempt", attempt, "retry_in", retryIn,
				"expires_at", l.ExpiresAt, "err", err)
			if hook := h.cfg.Hooks.OnTransientError; hook != nil {
				hook(l, attempt, err, retryIn)
			}
			timer.Reset(retryIn)
		}
	}
}

// renewOnce issues one renewal, bounded so a hung connection cannot consume the
// whole interval.
func (h *Holder) renewOnce() (RenewResult, error) {
	l := h.Lease()

	ctx, cancel := context.WithTimeout(h.ctx, h.cfg.RenewTimeout)
	defer cancel()

	// (id, fence, holder_instance): all three are matched server-side. A
	// process holding a stale fence is refused here rather than at the device,
	// where it would already have done damage.
	return h.store.Renew(ctx, l.ID, l.Fence, l.HolderInstance)
}

// onRenewed folds the server's deadlines into the cached lease and returns the
// updated snapshot.
func (h *Holder) onRenewed(res RenewResult) Lease {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.lease.ExpiresAt = res.ExpiresAt
	h.lease.ReclaimableAt = res.ReclaimableAt
	h.lastRenewAt = time.Now()
	h.renewals++
	h.consecFailures = 0
	return h.lease
}

// markFenced records the terminal fencing verdict and returns the lease
// snapshot to log it against.
func (h *Holder) markFenced() Lease {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.fenced = true
	return h.lease
}

func (h *Holder) onTransientError() (attempt int, l Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.consecFailures++
	return h.consecFailures, h.lease
}

// backoff returns a jittered delay for the given consecutive-failure count:
// exponential from RetryBase, capped at RetryMax, then uniformly jittered over
// the top half of the window.
//
// The jitter is not decoration. Every holder in the farm renews on the same
// cadence, so a database failover would otherwise synchronise thousands of
// retries into a thundering herd against the instance that is trying to come
// back — turning a short blip into the long outage that actually threatens
// leases.
func (h *Holder) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := h.cfg.RetryBase
	for i := 1; i < attempt && d < h.cfg.RetryMax; i++ {
		d *= 2
	}
	if d > h.cfg.RetryMax || d <= 0 {
		d = h.cfg.RetryMax
	}

	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}
