// Package runner executes one job placement: it takes a device somebody else
// has already leased and runs that job's spec against it, step by step.
//
// # What this package is not allowed to do
//
// It never renews a lease and it never ends one. Renewal lives on a separate
// path (internal/lease.Holder) that shares no goroutine, no connection and no
// failure mode with the ADB data path; ending a lease belongs to
// internal/lease and internal/reaper alone. The runner's entire relationship
// with the lease is one-directional and read-only: it derives every unit of
// work from Holder.Context() and stops when that context is cancelled. The
// [Holder] interface below is deliberately two methods wide — there is no
// Release for a socket error to reach.
//
// # A transport failure is not a job failure
//
// This is the whole reason the package is shaped the way it is. A step whose
// ADB call fails is RETRIED, with jittered backoff, inside the lease the job
// still holds. There is no retry budget: the bound is the step's own timeout,
// which the user wrote down. A device that is unreachable for nine minutes of
// a ten-minute step is the normal condition this farm exists to tolerate —
// DeviceFarmer/STF issue #663 is what happens when that condition is allowed
// to mean "the holder is gone".
//
// Only three things end a step badly:
//
//   - the step's own timeout ([ErrStepTimeout]),
//   - a non-retryable error — a protocol refusal, a bad spec, a content hash
//     that does not match ([ErrNotRetryable]),
//   - the job's max_runtime ([ErrMaxRuntime]), the one user-supplied clock the
//     schema lets end a lease automatically.
//
// Losing the lease ([lease.ErrFenced]) ends the ATTEMPT, immediately and
// without writing another byte to the device — but it is not a job failure
// either. The job keeps its checkpoint and is placed again.
//
// # Long work runs detached
//
// The shell_detached executor starts a command under nohup setsid with its
// output and its eventual exit code redirected to files on the device, and
// returns at once. wait_for then polls those files. No ADB socket is the
// source of truth for anything, so a six-hour job survives a ten-minute
// partition and a pod eviction in the middle of it: the replacement process
// re-attaches to the same lease at the same fence, sees the handle's marker
// files already on the device, and carries on waiting.
//
// # Fencing discipline on the bookkeeping
//
// Two writes are guarded by the fence in SQL rather than by hope: the
// checkpoint (a stale holder must not clobber a newer attempt's progress) and
// the job's terminal state (a stale holder must not mark 'failed' a job that
// somebody else is now running successfully). Both guards are WHERE clauses;
// zero rows updated is an ordinary, logged outcome, not an error.
//
// # 'succeeded' is a claim about rows, not about a loop
//
// A job is reported succeeded only when farm.job_steps says the whole spec ran.
// Two halves make that true. Every ending that reaches the step loop and stops
// inside it writes the steps it never reached (see [Runner.recordSkipped]), so
// a step list does not trail off at the point of the failure; and the verdict
// itself is checked against the rows before it is written (see
// [Runner.attemptStepsAgree]). When the rows disagree the verdict is WITHHELD
// rather than downgraded — nothing is written, the job keeps the state it had,
// and the reason lands in the log, on the job's error, and in farm.events.
// [Runner.refuseUnevidencedSuccess] explains why withholding is the only move
// that keeps this property and "a bookkeeping failure is never a verdict on the
// job" at the same time.
//
// Note the scope of the first half. A placement refused BEFORE the loop starts
// — a spec that does not parse, an exhausted attempt budget, a max_runtime that
// had already elapsed, a work directory the device would not create — writes no
// step rows at all, and this package does not pretend otherwise. Those endings
// are never a success, so they cannot produce the status JOB-10 is about, but a
// four-step job that failed to prepare its device does still read as a job with
// no steps.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// Defaults. Every one of them can be overridden per Runner, and the
// per-step timeout can be overridden per step.
const (
	// DefaultStepTimeout bounds one step, retries included. It is the only
	// thing that stops a step retrying a dead device forever, so it is
	// generous: a step that has not finished in half an hour is a step whose
	// author should have said how long it may take.
	DefaultStepTimeout = 30 * time.Minute

	// DefaultRetryBase and DefaultRetryMax bound the jittered backoff between
	// transport retries INSIDE a step. The cap is small because the device
	// coming back is the expected outcome and we want to notice it quickly.
	DefaultRetryBase = 1 * time.Second
	DefaultRetryMax  = 30 * time.Second

	// DefaultPollInterval is how often wait_for asks the device whether a
	// detached command has finished.
	DefaultPollInterval = 5 * time.Second

	// DefaultMaxOutput bounds what one step stores in farm.job_steps.output.
	// A chatty test can emit hundreds of megabytes; the row records the first
	// slice of it and says how much was dropped, because a job table nobody
	// can query is worse than a truncated log.
	DefaultMaxOutput = 64 << 10

	// DefaultCallTimeout bounds one bookkeeping round trip to Postgres.
	DefaultCallTimeout = 10 * time.Second

	// DefaultWorkRoot is where per-job scratch — detached logs, exit codes,
	// pushed APKs — lives on the device. /data/local/tmp is the one path the
	// shell user can reliably write and execute from on a stock device.
	DefaultWorkRoot = "/data/local/tmp/device-farmer"

	// agreementTries bounds the one read this package retries. See
	// [Runner.attemptStepsAgree] for why that read is the exception.
	agreementTries = 3
)

// Endings the runner distinguishes. They are also, exactly, the vocabulary of
// farm.job_attempts.outcome.
type State string

const (
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"

	// StateAbandoned means the attempt stopped without a verdict: the lease
	// was fenced, or the process is being shut down. The job is untouched and
	// keeps its checkpoint. Nothing here is a failure of the job.
	StateAbandoned State = "abandoned"
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrMaxRuntime is the cause attached to the run context when the job's
	// max_runtime interval elapses. It is the ONLY clock in this package that
	// can end a run without a step of its own asking for it, and it exists
	// because the user wrote it down in farm.jobs.max_runtime.
	ErrMaxRuntime = errors.New("runner: job max_runtime elapsed")

	// ErrStepTimeout is the cause attached to a step's context when the step's
	// own timeout elapses. Retries stop here; the step ends badly.
	ErrStepTimeout = errors.New("runner: step timeout elapsed")

	// ErrNotRetryable marks an error that retrying cannot fix: a protocol
	// refusal from the device, a spec that names something that does not
	// exist, content whose hash does not match. Everything that is NOT marked
	// with it is treated as transport noise and retried, because that default
	// is the invariant this farm is built on.
	//
	// A Conn implementation should wrap its protocol-level refusals with
	// [NotRetryable]; its socket failures must be left unwrapped.
	ErrNotRetryable = errors.New("runner: not retryable")
)

// NotRetryable marks err as one that no amount of retrying will fix.
//
// Conn adapters use it to distinguish "the device refused" from "the wire
// broke": the second is retried inside the lease, the first is not. Wrapping a
// transport error with this would recreate #663 one step at a time.
func NotRetryable(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrNotRetryable, err)
}

func notRetryablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotRetryable, fmt.Sprintf(format, args...))
}

// Holder is the part of *lease.Holder the runner is allowed to see.
//
// Notice what is missing: no Renew, no Release, no Witness. The runner cannot
// end a lease because it is never handed anything that could. It observes
// exactly two facts — the context every unit of work derives from, and whether
// the lease was lost — and touches the lease in no other way.
type Holder interface {
	// Context is cancelled when, and only when, this holder no longer holds
	// the lease. Every ADB call the runner makes descends from it.
	Context() context.Context

	// Fenced distinguishes losing the device from an orderly shutdown. They
	// call for the same immediate stop and for opposite bookkeeping.
	Fenced() bool
}

var _ Holder = (*lease.Holder)(nil)

// Placement is one job on one device under one lease: everything the runner
// needs to address the work and to record who did it.
type Placement struct {
	// JobID is farm.jobs.id and the ownership key of the lease.
	JobID string

	// LeaseID and Fence identify the lease. The runner presents neither to the
	// device; it records them in farm.job_attempts and uses Fence to guard its
	// own writes against a stale incarnation of itself.
	LeaseID string
	Fence   int64

	// DeviceID is farm.devices.id — the branded identity, never an ADB serial.
	DeviceID string

	// SlotID is the physical USB position, for audit rows.
	SlotID *int64

	// Devpath is farm.slots.adb_devpath ("usb:1-1.4"). Work is addressed by
	// position, never by serial: duplicate OEM serials are real and a
	// serial-addressed command can land on somebody else's six-hour run.
	Devpath string

	// Endpoint is the host's adb server address, recorded for audit. The
	// runner does not dial it — the caller supplies an already-bound Conn.
	Endpoint string
}

func (p Placement) validate() error {
	switch {
	case p.JobID == "":
		return errors.New("runner: placement has no job id")
	case p.DeviceID == "":
		return errors.New("runner: placement has no device id")
	case p.Devpath == "":
		return errors.New("runner: placement has no devpath; work must be addressed by position, never by serial")
	case p.Fence <= 0:
		return errors.New("runner: placement has no fence")
	}
	return nil
}

// Outcome is what one placement produced.
type Outcome struct {
	State   State
	Attempt int

	// Steps and Skipped count what this attempt actually did. A resume that
	// skipped four steps and ran one reports 1 and 4.
	Steps   int
	Skipped int

	// Error is the human-readable reason the attempt did not succeed, as
	// written to farm.job_attempts.error — or, when VerdictWithheld is set, the
	// reason the job's state was not written at all.
	Error string

	// VerdictWithheld means the runner DELIBERATELY did not write the job's
	// terminal state, and no supervisor may write one on its behalf.
	//
	// It is set on exactly one path: an attempt that ran every step it was
	// given, whose farm.job_steps rows do not support reporting the JOB as
	// succeeded. A safety net that wrote 'succeeded' here because the runner's
	// write "did not land" would undo the whole point of the refusal — see
	// [Runner.refuseUnevidencedSuccess] — so a caller that mirrors verdicts
	// must check this before it writes one.
	VerdictWithheld bool

	// Fenced is true when the lease was lost mid-run. The device is not ours
	// any more; do not release it, do not touch it.
	Fenced bool

	// ReleaseReason is what the SUPERVISOR should release the lease with — the
	// runner itself never releases anything. An empty value means "do not
	// release": the attempt was abandoned, the job is unfinished, and the lease
	// must survive for the replacement process to re-attach to.
	ReleaseReason lease.ReleaseReason
}

// Config configures a Runner. Pool is required; everything else has a default.
type Config struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// Artifacts is the content-addressed store. Required only by specs that
	// push, install or pull.
	Artifacts Artifacts

	WorkRoot     string
	MaxOutput    int
	StepTimeout  time.Duration
	RetryBase    time.Duration
	RetryMax     time.Duration
	PollInterval time.Duration
	CallTimeout  time.Duration
}

func (c *Config) applyDefaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.WorkRoot == "" {
		c.WorkRoot = DefaultWorkRoot
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}
	if c.StepTimeout <= 0 {
		c.StepTimeout = DefaultStepTimeout
	}
	if c.RetryBase <= 0 {
		c.RetryBase = DefaultRetryBase
	}
	// Two passes, because a cap below the floor is not a value to honour: the
	// first fills in an unset or nonsensical cap, and the second covers an
	// operator who raised RetryBase past the default cap. A RetryMax under
	// RetryBase would make backoff return a delay shorter than the floor the
	// same operator asked for.
	if c.RetryMax < c.RetryBase {
		c.RetryMax = DefaultRetryMax
	}
	if c.RetryMax < c.RetryBase {
		c.RetryMax = c.RetryBase
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
}

// Runner executes placements. It is stateless between runs and safe for
// concurrent use: everything about one placement lives in the call.
type Runner struct {
	cfg Config
	log *slog.Logger
}

// New returns a Runner. pool must be non-nil: every step this package runs is
// recorded, and a runner that cannot record is a runner nobody can debug at
// 3am.
func New(cfg Config) (*Runner, error) {
	if cfg.Pool == nil {
		return nil, errors.New("runner: no database pool")
	}
	cfg.applyDefaults()
	return &Runner{cfg: cfg, log: cfg.Logger.With("component", "runner")}, nil
}

// jobRow is farm.jobs as the runner reads it.
type jobRow struct {
	State       string
	Attempt     int
	MaxAttempts int
	Resumable   bool
	ResetTier   string
	ProfileID   string
	Spec        []byte
	Checkpoint  []byte

	// HasMaxRuntime and Remaining come from Postgres in one shot:
	// max_runtime - (now() - started_at), computed against the SERVER's clock.
	// No client timestamp is sent and none is compared; the runner only ever
	// learns "you have this much left from now", which is a duration and
	// carries no notion of an absolute instant.
	HasMaxRuntime bool
	Remaining     time.Duration
}

// Run executes the job named by p on dev and returns what happened.
//
// A non-nil error means the runner could not do its own bookkeeping — the
// database was unreachable, the job does not exist. It does NOT mean the job
// failed, and the returned Outcome.ReleaseReason will be empty, so a caller
// that releases only on a named reason cannot turn an infrastructure blip into
// a lost device.
func (r *Runner) Run(ctx context.Context, h Holder, p Placement, dev Conn) (Outcome, error) {
	if h == nil {
		return Outcome{}, errors.New("runner: nil holder; the runner must not run work it cannot have cancelled")
	}
	if dev == nil {
		return Outcome{}, errors.New("runner: nil device connection")
	}
	if err := p.validate(); err != nil {
		return Outcome{}, err
	}

	log := r.log.With(
		"job_id", p.JobID, "device_id", p.DeviceID, "devpath", p.Devpath,
		"lease_id", p.LeaseID, "fence", p.Fence)

	// Every unit of work descends from the holder's context, so losing the
	// lease closes every ADB socket in one cancellation rather than in a
	// cleanup checklist someone has to remember to run. The caller's own ctx
	// is folded in as a second cancellation source (SIGTERM, supervisor
	// shutdown) without becoming the parent: the holder's context is the one
	// that carries the fencing cause.
	work, cancelWork := context.WithCancelCause(h.Context())
	defer cancelWork(nil)
	stopWatch := context.AfterFunc(ctx, func() { cancelWork(context.Cause(ctx)) })
	defer stopWatch()

	// Bookkeeping must survive the cancellation that ends the work: an attempt
	// row left open forever is how a fleet loses the ability to tell a job
	// problem from a device problem.
	book := context.WithoutCancel(work)

	job, err := r.loadJob(book, p.JobID)
	if err != nil {
		return Outcome{}, err
	}

	switch job.State {
	case "cancelled":
		// A human ended it. Nothing to run, and the lease should go back with
		// the reason the human gave it.
		log.Info("job was cancelled before this placement ran")
		return Outcome{State: StateCancelled, Attempt: job.Attempt,
			Error:         "job was cancelled before this placement ran",
			ReleaseReason: lease.ReasonJobCancelled}, nil
	case "succeeded", "failed":
		return Outcome{}, fmt.Errorf("runner: job %s is already %s; refusing to run it again", p.JobID, job.State)
	}

	kinds, err := r.loadKinds(book)
	if err != nil {
		return Outcome{}, err
	}

	// Parse and validate before anything is written and long before the device
	// is touched. jobspec.Parse checks the document against the vocabulary as
	// this binary knows it; the check that follows re-asks the DATABASE, which
	// is the authority, so a runner whose migrations are behind refuses a job
	// instead of half-executing it.
	spec, specErr := jobspec.Parse(job.Spec)
	if specErr == nil {
		specErr = checkKindsAgainstSchema(spec, kinds)
	}

	ckpt := parseCheckpoint(job.Checkpoint)
	resuming := specErr == nil && job.Resumable && ckpt.belongsTo(p)

	attempt := 0
	if resuming {
		// Re-attaching to our own interrupted attempt: same lease, same fence,
		// same device, same attempt number. Burning a fresh attempt here would
		// spend the job's budget on a pod eviction, which is the most ordinary
		// event in a Kubernetes control plane.
		attempt = ckpt.Attempt
		log.Info("re-attaching to an interrupted attempt",
			"attempt", attempt, "completed_steps", len(ckpt.Completed))
	}
	overBudget := false
	if attempt <= 0 {
		attempt, err = r.claimAttempt(book, p.JobID)
		if err != nil {
			return Outcome{}, err
		}
		// A resume does not spend budget; a fresh placement does, and the
		// budget is the user's own max_attempts.
		overBudget = attempt > job.MaxAttempts
		ckpt = newCheckpoint(attempt, p, spec)
	}

	// The row goes down before the work does, and before any reason to refuse
	// the work: a placement that was refused is still a placement, and the
	// table whose whole purpose is telling a job problem from a device problem
	// must see it.
	attemptID, err := r.openAttempt(book, p, attempt)
	if err != nil {
		return Outcome{}, err
	}
	r.event(book, log, p, "job_attempt_started", map[string]any{
		"attempt": attempt, "resumed": resuming, "endpoint": p.Endpoint})

	out := Outcome{Attempt: attempt}
	if overBudget {
		out.State = StateFailed
		out.Error = fmt.Sprintf("attempt %d exceeds the job's max_attempts of %d", attempt, job.MaxAttempts)
		out.ReleaseReason = lease.ReasonFailed
		log.Error("attempt budget exhausted", "attempt", attempt, "max_attempts", job.MaxAttempts)
		r.closeAttempt(book, log, attemptID, out.State, out.Error)
		r.writeJobState(book, log, p, "failed", out.Error, true)
		r.event(book, log, p, "job_attempt_finished", map[string]any{
			"attempt": attempt, "outcome": string(out.State), "error": out.Error})
		return out, nil
	}
	if specErr != nil {
		// A spec that does not parse will not parse next time either: this is
		// permanent, and re-queueing it would only burn devices.
		out.State, out.Error, out.ReleaseReason = StateFailed, specErr.Error(), lease.ReasonFailed
		log.Error("job spec is not runnable", "err", specErr)
		r.closeAttempt(book, log, attemptID, out.State, out.Error)
		r.writeJobState(book, log, p, "failed", out.Error, true)
		r.event(book, log, p, "job_attempt_finished", map[string]any{
			"attempt": attempt, "outcome": string(out.State), "error": out.Error})
		return out, nil
	}

	// max_runtime is the user's own clock and the only one that may end this
	// run on its own. Postgres computed what is left of it; we turn that into
	// a relative deadline and never look at a wall-clock instant.
	runCtx := work
	if job.HasMaxRuntime {
		if job.Remaining <= 0 {
			out.State = StateFailed
			out.Error = "job max_runtime had already elapsed when this placement started"
			out.ReleaseReason = lease.ReasonMaxRuntime
			r.closeAttempt(book, log, attemptID, out.State, out.Error)
			r.writeJobState(book, log, p, "failed", out.Error, true)
			return out, nil
		}
		var cancelRuntime context.CancelFunc
		runCtx, cancelRuntime = context.WithTimeoutCause(work, job.Remaining, ErrMaxRuntime)
		defer cancelRuntime()
		log.Info("job runtime budget", "remaining", job.Remaining.Round(time.Second))
	}

	res := r.execute(runCtx, book, log, h, p, job, spec, kinds, ckpt, resuming, dev, &out)

	r.closeAttempt(book, log, attemptID, out.State, out.Error)
	r.event(book, log, p, "job_attempt_finished", map[string]any{
		"attempt": attempt, "outcome": string(out.State),
		"steps": out.Steps, "skipped": out.Skipped, "error": out.Error})

	switch out.State {
	case StateSucceeded:
		// ctx, not work: the retries inside stop when the PROCESS is shutting
		// down, and losing the lease is not that. A fenced holder never reaches
		// this branch — execute would have classified it as abandoned — so the
		// only thing work adds here is a way to cut the read short for a
		// cancellation that has nothing to say about the database.
		if why := r.attemptStepsAgree(book, ctx, log, p, attempt, len(spec.Steps)); why != "" {
			// The lease still goes back as 'completed', deliberately. The
			// release reason describes what this PLACEMENT did with the device
			// — it ran every step it was given and is done with it — and is not
			// a verdict on the job; the job's verdict is farm.jobs.state, which
			// is precisely what has just been withheld. Releasing as 'failed'
			// would put a claim about the work into the lease history to
			// describe a gap in the bookkeeping.
			out.VerdictWithheld, out.Error = true, why
			r.refuseUnevidencedSuccess(book, log, p, attempt, why)
			break
		}
		r.writeJobState(book, log, p, "succeeded", "", false)
	case StateFailed:
		// A job with attempts left goes back on the queue rather than to
		// 'failed': the scheduler will place it on another device, where a
		// device-specific fault will not follow it. A permanent failure — a bad
		// spec, an elapsed max_runtime — never gets that treatment.
		if !res.permanent && attempt < job.MaxAttempts {
			r.writeJobState(book, log, p, "queued", out.Error, false)
			log.Warn("attempt failed; job re-queued for another placement",
				"attempt", attempt, "max_attempts", job.MaxAttempts, "error", out.Error)
		} else {
			r.writeJobState(book, log, p, "failed", out.Error, true)
		}
	default:
		// Abandoned or cancelled: the job's state belongs to whoever picks it
		// up next. Writing it here would be this process, which has just been
		// told it no longer owns the device, having the last word.
		log.Warn("attempt ended without a verdict; job state left alone",
			"outcome", string(out.State), "error", out.Error)
	}
	return out, nil
}

// execResult is what execute learned that Outcome has no field for.
type execResult struct {
	// permanent means re-running this job on another device would fail the
	// same way.
	permanent bool
}

// execute runs the steps. It fills out and returns only the facts that decide
// what happens to the JOB, because the attempt's own record is written as it
// goes rather than at the end.
func (r *Runner) execute(
	ctx, book context.Context, log *slog.Logger, h Holder, p Placement,
	job jobRow, spec jobspec.Spec, kinds map[jobspec.Kind]kindInfo, ckpt Checkpoint,
	resuming bool, dev Conn, out *Outcome,
) execResult {

	plan, err := planResume(spec, ckpt, kinds, resuming)
	if err != nil {
		// The only way to get here is a resume that would repeat a side
		// effect. Refusing is the entire point — and it is marked permanent,
		// because re-queueing the job would run the whole spec again on a
		// fresh device and repeat the very step we just refused to repeat.
		out.State, out.Error, out.ReleaseReason = StateFailed, err.Error(), lease.ReasonFailed
		log.Error("refusing to resume", "err", err)
		if plan.refusedIndex >= 0 && plan.refusedIndex < len(spec.Steps) {
			st := spec.Steps[plan.refusedIndex]
			r.recordStep(book, log, p, out.Attempt, plan.refusedIndex, st, "failed",
				nil, nil, err.Error(), map[string]any{"resume_refused": true})
			// The refusal ends the attempt exactly as a failed step does, and
			// permanently: the job will not be placed again. Everything after
			// the refused step is work nobody will ever do, so it is recorded
			// as such. Without this the step list of a refused resume stops at
			// the step that was in flight, and a reader who does not have the
			// spec in front of them cannot tell that from a job that was only
			// ever that long.
			r.recordSkipped(book, log, p, out.Attempt, spec.Steps, plan.refusedIndex, endedByRefusal)
		}
		return execResult{permanent: true}
	}

	env := &env{
		dev:       dev,
		artifacts: r.cfg.Artifacts,
		pool:      r.cfg.Pool,
		log:       log,
		place:     p,
		attempt:   out.Attempt,
		spec:      spec,
		resetTier: job.ResetTier,
		profileID: job.ProfileID,
		workDir:   deviceWorkDir(r.cfg.WorkRoot, p.JobID),
		maxOutput: r.cfg.MaxOutput,
		poll:      r.cfg.PollInterval,
		callTO:    r.cfg.CallTimeout,
	}

	// One preparation round trip: create the scratch directory and find out
	// whether this device has setsid. Both answers are needed before any
	// detached step can be honest about what it started.
	if err := r.prepare(ctx, log, env, r.stepTimeout(spec, jobspec.Step{})); err != nil {
		if isAbort(ctx) {
			return r.classifyAbort(ctx, h, log, err, out, "preparing the device work directory")
		}
		// The device is reachable and refused, or it stayed unreachable for a
		// whole step's worth of retries. Either way this placement cannot run —
		// but another device very well might, so it is not permanent, and the
		// job goes back on the queue rather than to 'failed'.
		out.State = StateFailed
		out.Error = fmt.Sprintf("preparing the device work directory %s: %v", env.workDir, err)
		out.ReleaseReason = lease.ReasonFailed
		log.Error("could not prepare the device; look at this device's health before its next placement",
			"work_dir", env.workDir, "err", err)
		return execResult{}
	}

	for i, st := range spec.Steps {
		if plan.skip[i] {
			out.Skipped++
			r.recordStep(book, log, p, out.Attempt, i, st, "skipped", nil, nil, "",
				map[string]any{"reason": "completed in an earlier attempt of this placement"})
			continue
		}

		// The checkpoint goes down BEFORE a step that cannot be repeated, so
		// that a crash between this write and the step's completion is
		// recoverable as "we do not know" rather than silently repeated.
		if !plan.idempotent(i) {
			ckpt.markInFlight(i, st)
			r.saveCheckpoint(book, log, p, ckpt)
		}

		r.recordStepStart(book, log, p, out.Attempt, i, st)

		stepCtx, cancelStep := context.WithTimeoutCause(ctx, r.stepTimeout(spec, st), ErrStepTimeout)
		result, retries, err := r.runStep(stepCtx, env, st, plan.reattach[i])
		cancelStep()

		detail := map[string]any{"transport_retries": retries}
		if plan.reattach[i] {
			detail["reattached"] = true
		}
		if result != nil {
			for k, v := range result.Detail {
				detail[k] = v
			}
		}

		switch {
		case err != nil && isAbort(ctx):
			// The lease, the supervisor or max_runtime ended us mid-step. The
			// step did not fail: it was stopped.
			state := "aborted"
			if errors.Is(context.Cause(ctx), ErrMaxRuntime) {
				state = "failed"
			}
			r.recordStep(book, log, p, out.Attempt, i, st, state,
				exitOf(result), outputOf(result), err.Error(), detail)

			res := r.classifyAbort(ctx, h, log, err, out, "step "+st.ID)
			// The tail is recorded only once classifyAbort has said which of
			// the three aborts this was, because two of them are not endings.
			// A fenced or shut-down attempt keeps its checkpoint and is resumed
			// at the SAME attempt number, rewriting these very rows — so
			// 'skipped' there would tell an operator that work about to happen
			// never will. An elapsed max_runtime is the ending: the user's own
			// clock has been spent, the job goes to 'failed', and without these
			// rows the step list stops dead at the step that was interrupted.
			if out.State != StateAbandoned {
				r.recordSkipped(book, log, p, out.Attempt, spec.Steps, i, endedByStop)
			}
			return res

		case err != nil:
			// Step timeout, or something no retry could fix. This is the step
			// ending badly, which is a job failure — not a lease failure.
			out.Steps++
			if st.ContinueOnError {
				detail["tolerated"] = true
			}
			r.recordStep(book, log, p, out.Attempt, i, st, "failed",
				exitOf(result), outputOf(result), err.Error(), detail)
			if st.ContinueOnError {
				log.Warn("step failed but the spec tolerates it", "step", st.ID, "err", err)
				ckpt.markDone(i, st)
				r.saveCheckpoint(book, log, p, ckpt)
				continue
			}
			out.State = StateFailed
			out.Error = fmt.Sprintf("step %d (%s/%s): %v", i, st.ID, st.Kind(), err)
			out.ReleaseReason = lease.ReasonFailed
			log.Error("step failed", "step", st.ID, "kind", string(st.Kind()), "err", err)
			r.recordSkipped(book, log, p, out.Attempt, spec.Steps, i, endedByFailure)
			// Deliberately not permanent, even for a non-retryable error. The
			// same failure on four different devices is what tells an operator
			// this is a job problem, and farm.job_attempts is the table that
			// shows it. Declaring the verdict here instead would replace that
			// evidence with one process's opinion.
			return execResult{}

		case result.Failure != "":
			// The step ran to completion and the device said no. Retrying that
			// on the same device would produce the same answer.
			out.Steps++
			if st.ContinueOnError {
				detail["tolerated"] = true
			}
			r.recordStep(book, log, p, out.Attempt, i, st, "failed",
				exitOf(result), outputOf(result), result.Failure, detail)
			if st.ContinueOnError {
				log.Warn("step reported failure but the spec tolerates it",
					"step", st.ID, "failure", result.Failure)
				ckpt.markDone(i, st)
				r.saveCheckpoint(book, log, p, ckpt)
				continue
			}
			out.State = StateFailed
			out.Error = fmt.Sprintf("step %d (%s/%s): %s", i, st.ID, st.Kind(), result.Failure)
			out.ReleaseReason = lease.ReasonFailed
			log.Error("step reported failure", "step", st.ID, "kind", string(st.Kind()),
				"failure", result.Failure)
			r.recordSkipped(book, log, p, out.Attempt, spec.Steps, i, endedByFailure)
			return execResult{}

		default:
			out.Steps++
			r.recordStep(book, log, p, out.Attempt, i, st, "ok",
				exitOf(result), outputOf(result), "", detail)
			ckpt.markDone(i, st)
			r.saveCheckpoint(book, log, p, ckpt)
			log.Info("step ok", "step", st.ID, "kind", string(st.Kind()),
				"retries", retries)
		}
	}

	// Nothing left to run, and the context outlived every step.
	if err := abortErr(ctx); err != nil {
		return r.classifyAbort(ctx, h, log, err, out, "after the last step")
	}
	out.State = StateSucceeded
	out.ReleaseReason = lease.ReasonCompleted
	return execResult{}
}

// classifyAbort turns a cancelled run context into the right ending. The three
// causes call for three different things and must never be collapsed.
func (r *Runner) classifyAbort(ctx context.Context, h Holder, log *slog.Logger,
	err error, out *Outcome, where string) execResult {

	cause := context.Cause(ctx)
	switch {
	case h.Fenced() || errors.Is(cause, lease.ErrFenced):
		// TERMINAL for this attempt and for nothing else. We do not own the
		// device any more; something else may already be driving it. Write
		// nothing further to it, release nothing, and leave the job alone: the
		// checkpoint is already on disk and the next placement resumes from it.
		out.State, out.Fenced = StateAbandoned, true
		out.Error = fmt.Sprintf("lease fenced during %s", where)
		out.ReleaseReason = ""
		log.Error("FENCED mid-run; abandoning the attempt without touching the device",
			"where", where)
		return execResult{}

	case errors.Is(cause, ErrMaxRuntime):
		// The one user-supplied clock that is allowed to end a lease.
		out.State = StateFailed
		out.Error = fmt.Sprintf("job max_runtime elapsed during %s", where)
		out.ReleaseReason = lease.ReasonMaxRuntime
		log.Warn("max_runtime elapsed", "where", where)
		return execResult{permanent: true}

	default:
		// SIGTERM, node drain, preemption. The lease is untouched and MUST
		// stay untouched: the replacement process re-acquires by job id, gets
		// the same lease at the same fence, and resumes from the checkpoint.
		out.State = StateAbandoned
		out.Error = fmt.Sprintf("run stopped during %s: %v", where, cause)
		out.ReleaseReason = ""
		log.Warn("run stopped; lease and device left exactly as they are",
			"where", where, "cause", cause, "err", err)
		return execResult{}
	}
}

// prepare creates the job's scratch directory and probes for setsid. It is
// retried like any other device call: a device that is briefly unreachable at
// the start of a job is not a job failure either.
//
// budget is the spec's own step budget, so the one clock that can give up on
// an unreachable device here is still a number the job's author wrote down
// rather than a constant compiled into this binary.
func (r *Runner) prepare(ctx context.Context, log *slog.Logger, e *env, budget time.Duration) error {
	pctx, cancel := context.WithTimeoutCause(ctx, budget, ErrStepTimeout)
	defer cancel()

	var out ShellOutput
	_, err := r.retry(pctx, log, "prepare", func(c context.Context) error {
		var err error
		out, err = e.dev.Shell(c, prepareCommand(e.workDir))
		if err != nil {
			return err
		}
		// A stream that ended before the device reported a status is a wire
		// failure wearing a success's clothes: ExitCode is zero because
		// nothing set it. Reading it as "the directory exists" would let a
		// bumped cable produce a work directory that is not there, and every
		// later step would then fail somewhere far away from the cause.
		if !out.Exited {
			return errors.New("shell stream ended without an exit status while preparing the device")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return notRetryablef(
			"could not create the device work directory %s (exit status %d: %s); "+
				"check that the shell user can write under %s on this device, or point Config.WorkRoot somewhere it can",
			e.workDir, out.ExitCode, firstLine(string(out.Stderr)+string(out.Stdout)), r.cfg.WorkRoot)
	}
	e.detach = detachPrefix(string(out.Stdout))
	log.Info("device prepared", "work_dir", e.workDir, "detach", e.detach)
	return nil
}

// runStep executes one step, retrying transport failures for as long as the
// step's deadline allows.
//
// The retry loop has no attempt cap on purpose. The bound is the step's own
// timeout, which is a number the user wrote down; a hard-coded "three tries"
// would end a job because a USB hub took four seconds too long to come back,
// which is the failure this whole system exists to prevent.
func (r *Runner) runStep(ctx context.Context, e *env, st jobspec.Step, reattach bool) (*Result, int, error) {
	exec, ok := executorFor(st.Kind())
	if !ok {
		// Both jobspec and farm.step_kinds have already refused an unknown
		// kind, so reaching this would be a bug in the runner, not in the spec.
		return nil, 0, notRetryablef("no executor for step kind %q", st.Kind())
	}
	var last *Result
	retries, err := r.retry(ctx, e.log.With("step", st.ID, "kind", string(st.Kind())),
		st.ID, func(c context.Context) error {
			// The re-attach probe is inside the retry loop, not before it: the
			// question "is this run already going on the device?" is exactly the
			// question a flaky link must not be allowed to answer with "no".
			if reattach {
				res, err := reattachDetached(c, e, st)
				if err != nil {
					return err
				}
				if res != nil {
					last = res
					return nil
				}
				// The device carries no trace of the earlier start, so the
				// previous process died before launching anything. Run it.
				reattach = false
			}
			var err error
			last, err = exec(c, e, st)
			return err
		})
	return last, retries, err
}

// retry runs fn until it succeeds, until the context ends, or until fn returns
// something retrying cannot fix.
func (r *Runner) retry(ctx context.Context, log *slog.Logger, what string,
	fn func(context.Context) error) (int, error) {

	retries := 0
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return retries, nil
		}
		if aerr := abortErr(ctx); aerr != nil {
			return retries, aerr
		}
		if !isRetryable(err) {
			return retries, err
		}

		delay := r.backoff(attempt)
		// Deliberately loud: this line, repeated, is what an operator reads
		// when a hub is flapping — and its absence from the lease log is the
		// proof that the flapping cost the job nothing.
		log.Warn("transport failure inside a step; retrying INSIDE the lease (job NOT failed)",
			"what", what, "attempt", attempt, "retry_in", delay, "err", err)
		retries++

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return retries, abortErr(ctx)
		case <-timer.C:
		}
	}
}

// backoff is exponential from RetryBase to RetryMax, jittered over the top
// half of the window. The jitter matters at fleet scale: an adb server restart
// hits every runner on the host at once, and unjittered retries would keep
// hitting it in lockstep exactly while it is trying to come back.
func (r *Runner) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := r.cfg.RetryBase
	for i := 1; i < attempt && d < r.cfg.RetryMax; i++ {
		d *= 2
	}
	if d > r.cfg.RetryMax || d <= 0 {
		d = r.cfg.RetryMax
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// stepTimeout is the step's own budget, retries included. jobspec.Validate
// guarantees a positive effective timeout, so the fallback below is only
// reached by a spec that arrived some other way.
func (r *Runner) stepTimeout(spec jobspec.Spec, st jobspec.Step) time.Duration {
	if d := spec.StepTimeout(st); d > 0 {
		return d
	}
	return r.cfg.StepTimeout
}

// checkKindsAgainstSchema re-asks the database whether it knows every kind in
// the spec. farm.step_kinds is the closed vocabulary, and the foreign key on
// farm.job_steps.kind would refuse an unknown one anyway — but it would refuse
// it AFTER the step had run on the device, which is the wrong end of the job
// to discover it.
func checkKindsAgainstSchema(spec jobspec.Spec, kinds map[jobspec.Kind]kindInfo) error {
	for i, st := range spec.Steps {
		if _, ok := kinds[st.Kind()]; !ok {
			return fmt.Errorf("runner: step %d (%s) has kind %q, which is not in farm.step_kinds",
				i, st.ID, st.Kind())
		}
	}
	return nil
}

// isRetryable decides whether an error is about the wire or about the work.
//
// The default is RETRY. That is not laziness: an unclassified failure from a
// device connection is far more likely to be a cable, a hub or an adb server
// than a deliberate refusal, and treating the unknown as fatal is precisely
// how a transport error ends up ending a job. Errors that genuinely cannot be
// retried say so, by wrapping [ErrNotRetryable] or by implementing
// Retryable() bool.
//
// Note what is NOT special-cased here: context.Canceled and
// context.DeadlineExceeded. Every caller checks its own context first —
// [Runner.retry] via [abortErr], execWaitFor via wait.Err() — so a context
// error that reaches this function did not come from a context the runner
// owns. It came from a deadline INSIDE the connection: a dial timeout, a read
// deadline, an http client the transport is built on. That is a transport
// failure wearing a context error's clothes, and treating it as fatal would
// end a six-hour job because one socket took too long to open, which is
// exactly DeviceFarmer/STF #663. The bound on retrying it is the step's own
// timeout, which the user wrote down.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// The runner's own endings. Every caller reaches these through abortErr
	// first, so seeing one here means an executor passed one back up while its
	// caller's context was still live — a reset sub-step's deadline, say.
	// Retrying any of them is wrong in a different way each time: the two
	// deadlines are budgets that have already been spent, and ErrFenced means
	// the device belongs to somebody else now and must not be written to at
	// all.
	if errors.Is(err, ErrNotRetryable) ||
		errors.Is(err, ErrStepTimeout) ||
		errors.Is(err, ErrMaxRuntime) ||
		errors.Is(err, lease.ErrFenced) {
		return false
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) {
		return rt.Retryable()
	}
	return true
}

// abortErr reports the cause when the run context has ended, and nil while it
// is still live.
func abortErr(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return context.Cause(ctx)
}

// isAbort distinguishes "the run was ended" from "this step ended badly". A
// step timeout is a step's own affair and leaves the run context live.
func isAbort(ctx context.Context) bool { return ctx.Err() != nil }

func exitOf(res *Result) *int {
	if res == nil {
		return nil
	}
	return res.ExitCode
}

func outputOf(res *Result) *string {
	if res == nil || res.Output == "" {
		return nil
	}
	s := res.Output
	return &s
}

// ---------------------------------------------------------------------------
// Bookkeeping. Every timestamp below is now(), computed by Postgres.
// ---------------------------------------------------------------------------

func (r *Runner) db(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.cfg.CallTimeout)
}

func (r *Runner) loadJob(ctx context.Context, jobID string) (jobRow, error) {
	const q = `
SELECT j.state, j.attempt, j.max_attempts, j.resumable, j.reset_tier,
       COALESCE(j.profile_id, ''), j.spec, j.checkpoint,
       (j.max_runtime IS NOT NULL) AS has_max_runtime,
       COALESCE(EXTRACT(EPOCH FROM (j.max_runtime - (now() - COALESCE(j.started_at, now())))), 0)::float8
  FROM farm.jobs j
 WHERE j.id = $1::uuid`

	cctx, cancel := r.db(ctx)
	defer cancel()

	var row jobRow
	var seconds float64
	err := r.cfg.Pool.QueryRow(cctx, q, jobID).Scan(
		&row.State, &row.Attempt, &row.MaxAttempts, &row.Resumable, &row.ResetTier,
		&row.ProfileID, &row.Spec, &row.Checkpoint, &row.HasMaxRuntime, &seconds)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return jobRow{}, fmt.Errorf("runner: job %s does not exist", jobID)
	case err != nil:
		return jobRow{}, fmt.Errorf("runner: load job %s: %w", jobID, err)
	}
	row.Remaining = time.Duration(seconds * float64(time.Second))
	return row, nil
}

// kindInfo is one row of farm.step_kinds. The vocabulary is closed and it
// lives in the database, so the runner asks rather than assumes: a spec
// written today still means the same thing when it resumes tomorrow.
type kindInfo struct {
	Idempotent    bool
	NeedsArtifact bool
}

func (r *Runner) loadKinds(ctx context.Context) (map[jobspec.Kind]kindInfo, error) {
	cctx, cancel := r.db(ctx)
	defer cancel()

	rows, err := r.cfg.Pool.Query(cctx, `SELECT kind, idempotent, needs_artifact FROM farm.step_kinds`)
	if err != nil {
		return nil, fmt.Errorf("runner: load step kinds: %w", err)
	}
	defer rows.Close()

	out := make(map[jobspec.Kind]kindInfo, 16)
	for rows.Next() {
		var k string
		var info kindInfo
		if err := rows.Scan(&k, &info.Idempotent, &info.NeedsArtifact); err != nil {
			return nil, fmt.Errorf("runner: scan step kind: %w", err)
		}
		out[jobspec.Kind(k)] = info
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runner: read step kinds: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("runner: farm.step_kinds is empty; migrations have not been applied")
	}
	return out, nil
}

// claimAttempt takes the next attempt number for this job. It is a single
// UPDATE ... RETURNING so two schedulers racing the same job cannot mint the
// same number.
func (r *Runner) claimAttempt(ctx context.Context, jobID string) (int, error) {
	cctx, cancel := r.db(ctx)
	defer cancel()

	var attempt int
	err := r.cfg.Pool.QueryRow(cctx,
		`UPDATE farm.jobs SET attempt = attempt + 1 WHERE id = $1::uuid RETURNING attempt`,
		jobID).Scan(&attempt)
	if err != nil {
		return 0, fmt.Errorf("runner: claim attempt for job %s: %w", jobID, err)
	}
	return attempt, nil
}

func (r *Runner) openAttempt(ctx context.Context, p Placement, attempt int) (int64, error) {
	const q = `
INSERT INTO farm.job_attempts (job_id, attempt, device_id, lease_id, fence)
VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5)
ON CONFLICT (job_id, attempt) DO UPDATE
   SET device_id = EXCLUDED.device_id,
       lease_id  = EXCLUDED.lease_id,
       fence     = EXCLUDED.fence,
       finished_at = NULL,
       outcome     = NULL,
       error       = NULL
RETURNING id`

	cctx, cancel := r.db(ctx)
	defer cancel()

	var id int64
	err := r.cfg.Pool.QueryRow(cctx, q, p.JobID, attempt, p.DeviceID,
		nullString(p.LeaseID), p.Fence).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("runner: open attempt %d of job %s: %w", attempt, p.JobID, err)
	}
	return id, nil
}

// closeAttempt writes the verdict. A failure to record it is logged rather
// than returned: by the time it runs, the work is over and the caller's next
// move — releasing or not releasing the lease — must not depend on a database
// blip.
func (r *Runner) closeAttempt(ctx context.Context, log *slog.Logger, id int64, state State, errText string) {
	cctx, cancel := r.db(ctx)
	defer cancel()

	_, err := r.cfg.Pool.Exec(cctx,
		`UPDATE farm.job_attempts SET finished_at = now(), outcome = $2, error = $3 WHERE id = $1`,
		id, string(state), nullString(errText))
	if err != nil {
		log.Error("could not close the job attempt row", "attempt_row", id, "err", err)
	}
}

func (r *Runner) recordStepStart(ctx context.Context, log *slog.Logger, p Placement,
	attempt, index int, st jobspec.Step) {

	const q = `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state, started_at, detail)
VALUES ($1::uuid, $2, $3, $4, $5, 'running', now(), $6::jsonb)
ON CONFLICT (job_id, attempt, step_index) DO UPDATE
   SET step_id = EXCLUDED.step_id,
       kind    = EXCLUDED.kind,
       state   = 'running',
       started_at  = now(),
       finished_at = NULL,
       exit_code   = NULL,
       output      = NULL,
       error       = NULL,
       detail      = EXCLUDED.detail`

	cctx, cancel := r.db(ctx)
	defer cancel()

	if _, err := r.cfg.Pool.Exec(cctx, q, p.JobID, attempt, index, st.ID, string(st.Kind()),
		jsonOrEmpty(stepDetail(st))); err != nil {
		log.Error("could not record step start", "step", st.ID, "index", index, "err", err)
	}
}

// recordStep writes a step's final row. output is already sanitised and
// bounded by the caller.
func (r *Runner) recordStep(ctx context.Context, log *slog.Logger, p Placement,
	attempt, index int, st jobspec.Step, state string, exit *int, output *string,
	errText string, detail map[string]any) {

	const q = `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state,
                            started_at, finished_at, exit_code, output, error, detail)
VALUES ($1::uuid, $2, $3, $4, $5, $6, now(), now(), $7, $8, $9, $10::jsonb)
ON CONFLICT (job_id, attempt, step_index) DO UPDATE
   SET step_id = EXCLUDED.step_id,
       kind    = EXCLUDED.kind,
       state   = EXCLUDED.state,
       started_at  = COALESCE(farm.job_steps.started_at, now()),
       finished_at = now(),
       exit_code   = EXCLUDED.exit_code,
       output      = EXCLUDED.output,
       error       = EXCLUDED.error,
       detail      = farm.job_steps.detail || EXCLUDED.detail`

	cctx, cancel := r.db(ctx)
	defer cancel()

	if detail == nil {
		detail = map[string]any{}
	}
	for k, v := range stepDetail(st) {
		if _, ok := detail[k]; !ok {
			detail[k] = v
		}
	}

	if _, err := r.cfg.Pool.Exec(cctx, q, p.JobID, attempt, index, st.ID, string(st.Kind()),
		state, exit, output, nullString(errText), jsonOrEmpty(detail)); err != nil {
		log.Error("could not record step result", "step", st.ID, "index", index,
			"state", state, "err", err)
	}
}

// skippedRow is one step that was never reached, ready to be written.
type skippedRow struct {
	Index  int
	ID     string
	Kind   string
	Detail map[string]any
}

// stepEnding is how the step that ended the attempt ended, in the words an
// operator reads on every step after it.
//
// It is a parameter rather than one fixed sentence because the three endings
// are not the same event and this package may not blur them. "failed" is a
// verdict on the work; a run cut short by the job's own max_runtime did not
// fail at the step it was standing on, and a resume this package refused was
// never run at all. Writing "failed" over all three would put a verdict in the
// ledger for something that was merely stopped — the small version of the
// mistake the whole package is shaped against.
type stepEnding string

const (
	// endedByFailure: the step ran and ended badly — a timeout, a refusal from
	// the device, a non-zero verdict the spec does not tolerate.
	endedByFailure stepEnding = "failed and ended the attempt"

	// endedByStop: the step was still running when the run was ended out from
	// under it and the attempt cannot be resumed — in practice the job's own
	// max_runtime, the one user-supplied clock that ends a run.
	endedByStop stepEnding = "was stopped mid-run and ended the attempt"

	// endedByRefusal: a resume that would have repeated a side effect. The step
	// was not run a second time and nothing after it was reached.
	endedByRefusal stepEnding = "could not be resumed safely and ended the attempt"
)

// skippedAfter lists the steps after the one at ended, each carrying the reason
// it was not run. It is pure so the reason can be checked without a database:
// detail.reason is the sentence an operator reads, and detail.failed_step is
// what a dashboard links.
//
// failed_step and failed_step_index keep those names for all three endings. The
// key is a link target in a ledger that already holds rows written under the
// old, failure-only spelling, and one fact reachable under two key names
// depending on when the row was written is worse for the reader than a key
// whose name is a shade broader than its contents. detail.reason is where the
// ending is stated precisely.
func skippedAfter(steps []jobspec.Step, ended int, how stepEnding) []skippedRow {
	if ended < 0 || ended+1 >= len(steps) {
		return nil
	}
	cause := steps[ended]
	reason := fmt.Sprintf("not run: step %d (%s/%s) %s",
		ended, cause.ID, cause.Kind(), how)
	out := make([]skippedRow, 0, len(steps)-ended-1)
	for i := ended + 1; i < len(steps); i++ {
		st := steps[i]
		d := stepDetail(st)
		d["reason"] = reason
		d["failed_step"] = cause.ID
		d["failed_step_index"] = ended
		out = append(out, skippedRow{Index: i, ID: st.ID, Kind: string(st.Kind()), Detail: d})
	}
	return out
}

// recordSkipped writes a 'skipped' row for every step after the one that
// ended the attempt, so the job's step list is complete.
//
// Without these rows a job that failed at step 2 of 4 shows two rows, and a
// reader has to know the spec to learn that two more were never reached: the
// dashboard's count and `ctl job steps` both looked like a job with two steps
// that ran one of them. Each row names the step whose ending is its reason,
// because 'skipped' on its own is also what a resumed attempt writes for a
// step that already completed, and the reason is what tells the two apart.
//
// Every branch that ends an attempt for good calls this — a failure, a refused
// resume, and a run cut short by max_runtime alike — because a complete step
// list is what [Runner.attemptStepsAgree] checks before this package will
// report a job as succeeded. An attempt that is merely ABANDONED does not call
// it: the same attempt number resumes from the checkpoint, so its remaining
// steps are work somebody is about to do, and writing 'skipped' over them
// would state the opposite of what is about to happen.
//
// They are 'skipped' and never 'pending'. pending and running are the two
// states the janitor sweeps and the job_steps_live index covers, and a row
// for work nobody will do must not look like work somebody has yet to do.
// started_at stays NULL — the step never started — so the API reports no
// duration for it rather than a zero one.
//
// One statement for the whole tail: a failure at the first of forty steps
// must not cost thirty-nine round trips to record. A row that already exists
// at one of these indexes is overwritten, not merged: it can only be left
// from an earlier run of this same attempt, and what it says about that run
// is no longer what happened to this one.
func (r *Runner) recordSkipped(ctx context.Context, log *slog.Logger, p Placement,
	attempt int, steps []jobspec.Step, ended int, how stepEnding) {

	rows := skippedAfter(steps, ended, how)
	if len(rows) == 0 {
		return
	}

	const q = `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state, finished_at, detail)
SELECT $1::uuid, $2, s.step_index, s.step_id, s.kind, 'skipped', now(), s.detail::jsonb
  FROM unnest($3::int[], $4::text[], $5::text[], $6::text[]) AS s(step_index, step_id, kind, detail)
ON CONFLICT (job_id, attempt, step_index) DO UPDATE
   SET step_id     = EXCLUDED.step_id,
       kind        = EXCLUDED.kind,
       state       = 'skipped',
       started_at  = NULL,
       finished_at = now(),
       exit_code   = NULL,
       output      = NULL,
       error       = NULL,
       detail      = EXCLUDED.detail`

	indexes := make([]int32, len(rows))
	ids := make([]string, len(rows))
	kinds := make([]string, len(rows))
	details := make([]string, len(rows))
	for i, row := range rows {
		indexes[i], ids[i], kinds[i], details[i] = int32(row.Index), row.ID, row.Kind, jsonOrEmpty(row.Detail)
	}

	cctx, cancel := r.db(ctx)
	defer cancel()

	if _, err := r.cfg.Pool.Exec(cctx, q, p.JobID, attempt, indexes, ids, kinds, details); err != nil {
		log.Error("could not record the steps that were not run", "after", steps[ended].ID,
			"count", len(rows), "err", err)
		return
	}
	log.Info("steps after the ending recorded as skipped", "after", steps[ended].ID,
		"how", string(how), "count", len(rows))
}

// attemptStepsAgree asks farm.job_steps whether this attempt's own rows support
// reporting the job as succeeded. It returns "" when they do, and otherwise the
// sentence an operator needs to understand why the job is not finished.
//
// It asks the DATABASE rather than out.Steps and out.Skipped on purpose. Those
// two counters are incremented by the same loop that decides the verdict, so
// comparing them against len(spec.Steps) would only prove the loop agrees with
// itself — including on the run where every recordStep failed on the wire and
// the job's step list is empty. The rows are the thing the API shows and the
// thing JOB-10 is about, so the rows are what gets asked.
//
// Two disagreements are recognised, and the first is the one that produced
// JOB-10: an install whose push failed left step 1 at 'running' — inserted by
// recordStepStart, never overwritten, because recordStep's failure is logged
// and swallowed — the loop carried on, and the job was written 'succeeded'
// beside a step row that still said it was executing.
//
// want is len(spec.Steps): rows outside that range are not counted as evidence,
// because a row at an index the current spec does not have is a leftover from a
// run this verdict is not about.
//
// stepBeliesSuccess below is spelled character for character as
// internal/janitor's constant of the same name, and the two must never diverge.
// They are the same question asked from two sides — this one before a success
// is written, that one over successes already written — so a row this function
// accepted and that one rejects is a job the runner reports succeeded and the
// next sweep silently reverses, with each of them logging that the other must
// be at fault. The duplication is deliberate: internal/janitor imports nothing
// that could end a lease, and it is not going to start importing this package
// to share a string.
// ctx is the bookkeeping context, which by design does not cancel: the read has
// to happen even though the work is over. stop is the process's own context and
// gates only the RETRIES — see the loop below.
func (r *Runner) attemptStepsAgree(ctx, stop context.Context, log *slog.Logger, p Placement,
	attempt, want int) string {

	const stepBeliesSuccess = `(s.state IN ('pending','running','aborted')
        OR (s.state = 'failed' AND COALESCE(s.detail->>'tolerated', '') <> 'true'))`

	// The whole query is confined to the spec's own index range, in the WHERE
	// rather than in each FILTER, so no count can read a row the others cannot.
	// A row outside it is a leftover from a run against a different step list;
	// counting it as evidence would let it stand in for a step that is missing,
	// and counting it as a contradiction would withhold this job's verdict on
	// every attempt for the rest of its life, with nothing an operator could do
	// about it.
	const q = `
SELECT count(*) FILTER (WHERE s.state IN ('pending','running')),
       COALESCE((array_agg(s.step_id ORDER BY s.step_index)
                   FILTER (WHERE s.state IN ('pending','running')))[1], ''),
       count(*) FILTER (WHERE ` + stepBeliesSuccess + `),
       COALESCE((array_agg(s.step_id ORDER BY s.step_index)
                   FILTER (WHERE ` + stepBeliesSuccess + `))[1], ''),
       count(*)
  FROM farm.job_steps s
 WHERE s.job_id = $1::uuid AND s.attempt = $2
   AND s.step_index >= 0 AND s.step_index < $3::int`

	var live, belies, have int
	var liveStep, beliesStep string

	// Retried, alone among this package's bookkeeping calls, and for a reason
	// that is about consequence rather than importance.
	//
	// Every other write here logs its failure and carries on because the job's
	// verdict must not depend on a database blip. This one is a READ whose
	// failure withholds the verdict — and a withheld verdict costs the job a
	// whole re-execution on another device, side effects included, since a
	// fresh placement does not resume from a checkpoint that belongs to the old
	// lease. Spending three round trips to avoid re-running a six-hour job
	// because one query lost a socket is not a close call.
	//
	// It is bounded and small: the alternative to succeeding here is not
	// failure, it is the re-run, so there is nothing to gain from trying
	// harder. If the database is genuinely gone, writeJobState is about to fail
	// too and the job is re-queued either way.
	//
	// The retries — and only the retries — stop when stop is cancelled. The
	// FIRST read always runs on the bookkeeping context, because a SIGTERM
	// arriving in the middle of it must not turn a healthy database into a
	// withheld verdict. What the guard prevents is the other shape: a pod being
	// drained against a database that is already gone, spending three call
	// timeouts here on top of the ones closeAttempt and writeJobState are about
	// to spend, and being SIGKILLed for it partway through.
	var err error
	for try := 1; ; try++ {
		func() {
			cctx, cancel := r.db(ctx)
			defer cancel()
			err = r.cfg.Pool.QueryRow(cctx, q, p.JobID, attempt, want).
				Scan(&live, &liveStep, &belies, &beliesStep, &have)
		}()
		if err == nil || try >= agreementTries || stop.Err() != nil {
			break
		}
		log.Warn("the step-row read a 'succeeded' verdict rests on failed; retrying rather "+
			"than costing the job a whole re-run over one blip", "try", try, "err", err)
		select {
		case <-stop.Done():
		case <-time.After(r.cfg.RetryBase):
		}
	}
	if err != nil {
		// Not knowing is not agreement. The database is the only witness to
		// what this attempt recorded, and a verdict of 'succeeded' written
		// without reading it would be the exact claim this function exists to
		// stop — so an unreadable ledger withholds the verdict too.
		log.Error("could not read the step rows that a 'succeeded' verdict rests on", "err", err)
		return fmt.Sprintf("the step rows of attempt %d could not be read (%v), "+
			"so nothing here can say the whole spec ran", attempt, err)
	}

	switch {
	case live > 0:
		return fmt.Sprintf("%d step row(s) of attempt %d still say 'pending' or 'running' (%s); "+
			"a job has not succeeded while a step of it is still recorded as executing",
			live, attempt, liveStep)
	case belies > 0:
		// A step this attempt ran and did not finish cannot be here: every
		// branch that produces one returns before the loop can reach the end.
		// So this is a row from an earlier run of the same attempt number that
		// the current run should have overwritten and could not — which is
		// exactly as much of an unevidenced success as a missing row is.
		return fmt.Sprintf("%d step row(s) of attempt %d record work that did not finish (%s); "+
			"this attempt ran to the end, so those rows are from an earlier run of it that "+
			"was never overwritten", belies, attempt, beliesStep)
	case have < want:
		return fmt.Sprintf("attempt %d recorded a verdict for %d of the spec's %d step(s); "+
			"%d step(s) left no record of what happened to them",
			attempt, have, want, want-have)
	}
	return ""
}

// refuseUnevidencedSuccess is what happens INSTEAD of writing 'succeeded' when
// the step rows do not support it.
//
// # The tension this arbitrates
//
// [Runner.recordStep] and [Runner.recordStepStart] log their failures and carry
// on, deliberately, and TestABookkeepingFailureIsNeverAVerdictOnTheJob pins
// that: a database blip must never become a verdict on a job. But that same
// tolerance is what lets one failed write leave step i at 'running' while the
// loop runs to the end — and JOB-10 requires that a job's reported state agree
// with its step rows. Both properties are real and they pull opposite ways.
//
// They are reconciled by REFUSING rather than by downgrading. Nothing is
// written here at all: farm.jobs keeps the state it already had, which is
// 'running'. That is the absence of a verdict, not a new one, so the blip has
// still decided nothing about the job — and 'succeeded' has still not been
// claimed without evidence. Downgrading to 'failed' would have satisfied
// JOB-10 by breaking the other property, saying a job failed when every step of
// it ran; leaving 'succeeded' would have kept the other property by breaking
// JOB-10. Withholding is the only move that keeps both, and it is available
// only because a job at 'running' is already a state this system knows how to
// resolve.
//
// # What the operator sees
//
//   - this ERROR line, naming the job, the attempt and which rows disagree;
//   - a job_success_not_asserted event on the job in farm.events carrying the
//     same sentence, so "why is this job not finished?" is answerable from the
//     API rather than from one pod's logs;
//   - farm.job_attempts still records this attempt as 'succeeded', because it
//     did succeed: every step this process ran, ran. The attempt's own truth
//     was never what was in doubt, and the row is written before this point;
//   - the reason on farm.jobs.error, without touching farm.jobs.state, so the
//     job's own page answers "why is this still running?" while it waits;
//   - then the job goes where a 'running' job with no live lease goes.
//     internal/janitor's job sweep puts it back on the queue while its own
//     max_attempts allows. Running the spec again on another device is the
//     same disposition this package already gives a failed attempt, and it is
//     the only one that can produce the evidence that is missing.
//
// # What happens when the budget is spent
//
// Say it plainly, because the withholding above does not make it go away: when
// the withheld attempt was the job's LAST, the janitor's job sweep has nowhere
// left to place it and records it 'failed'. A job cannot stay 'running'
// forever, and the schema has no state for "this cannot be evidenced".
//
// That is not the property TestABookkeepingFailureIsNeverAVerdictOnTheJob
// protects arriving by a side door. That test is about a process declaring a
// verdict it has no standing to declare, in the moment, on a job somebody else
// may still be running — and none of that happens here: nothing is written, the
// lease goes back, and every remaining attempt in the user's own budget is
// spent on the job before anything terminal is said about it. What is left at
// the end is a job whose success genuinely cannot be evidenced after every
// attempt it was allowed, which is a fact about the job and not an opinion
// about a database blip. The job_success_not_asserted events on the way there
// are what tell an operator which of the two they are looking at.
//
// Outcome.VerdictWithheld carries the refusal out to the supervisor, whose
// finalize is a safety net for a verdict that did not land. This one did not
// land because it was not sent.
func (r *Runner) refuseUnevidencedSuccess(ctx context.Context, log *slog.Logger,
	p Placement, attempt int, why string) {

	log.Error("REFUSING to report this job as succeeded: its step rows do not support it. "+
		"The job's state is left alone rather than downgraded — a bookkeeping failure "+
		"must not become a verdict either way — and the janitor will place it again "+
		"while its max_attempts allows",
		"attempt", attempt, "disagreement", why)

	r.noteJobError(ctx, log, p, fmt.Sprintf(
		"attempt %d ran every step it was given, but its farm.job_steps rows do not support "+
			"reporting this job as succeeded: %s. The verdict was withheld rather than "+
			"guessed, so this job is still running and will be placed again while its "+
			"max_attempts allows.", attempt, why))

	r.event(ctx, log, p, "job_success_not_asserted", map[string]any{
		"attempt": attempt, "reason": why,
	})
}

// noteJobError records why a job is not finished, WITHOUT touching its state.
//
// It carries the same fence guard writeJobState does and for the same reason: a
// stale holder must not scribble on a job a newer placement is running. What it
// deliberately does not carry is a state, so nothing it writes is a verdict —
// the whole point of the path that calls it is that no verdict is being made.
//
// The text is transient by design. The janitor overwrites farm.jobs.error when
// it re-queues the job, which is correct: by then the reason the job is moving
// is the sweeper's, not this one's. The durable trail is the
// job_success_not_asserted event, which is per-attempt and is never overwritten.
func (r *Runner) noteJobError(ctx context.Context, log *slog.Logger, p Placement, text string) {
	const q = `
UPDATE farm.jobs j
   SET error = $2
 WHERE j.id = $1::uuid
   AND j.state NOT IN ('succeeded','failed','cancelled')
   AND EXISTS (SELECT 1 FROM farm.leases l
                WHERE l.job_id = j.id AND l.fence = $3
                  AND l.state IN ('held','suspect'))`

	cctx, cancel := r.db(ctx)
	defer cancel()

	if _, err := r.cfg.Pool.Exec(cctx, q, p.JobID, text, p.Fence); err != nil {
		log.Warn("could not record why the job's verdict was withheld; the log line above "+
			"and the audit event are the trail", "err", err)
	}
}

// writeJobState moves farm.jobs.state, fenced.
//
// The EXISTS guard is the point: a process that lost its lease an hour ago and
// only now got its statement through must not mark 'failed' a job that another
// device is quietly succeeding at. Zero rows updated means exactly that
// happened, and it is logged rather than raised, because at this point in the
// run there is nothing left to abort.
func (r *Runner) writeJobState(ctx context.Context, log *slog.Logger, p Placement,
	state, errText string, finish bool) {

	const q = `
UPDATE farm.jobs j
   SET state = $2,
       error = $3,
       finished_at = CASE WHEN $4 THEN now() ELSE NULL END
 WHERE j.id = $1::uuid
   AND j.state NOT IN ('succeeded','failed','cancelled')
   AND EXISTS (SELECT 1 FROM farm.leases l
                WHERE l.job_id = j.id AND l.fence = $5
                  AND l.state IN ('held','suspect'))`

	cctx, cancel := r.db(ctx)
	defer cancel()

	tag, err := r.cfg.Pool.Exec(cctx, q, p.JobID, state, nullString(errText), finish, p.Fence)
	switch {
	case err != nil:
		log.Error("could not write the job's state", "state", state, "err", err)
	case tag.RowsAffected() == 0:
		log.Warn("job state not written, and nothing needs doing about it: "+
			"either the job is already terminal or a newer holder owns it at a higher fence, "+
			"and this process's verdict is the stale one",
			"state", state, "fence", p.Fence)
	}
}

func (r *Runner) event(ctx context.Context, log *slog.Logger, p Placement, kind string, detail map[string]any) {
	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
VALUES ($1, $2::uuid, $3, $4::uuid, $5::uuid, 'runner', $6::jsonb)`

	cctx, cancel := r.db(ctx)
	defer cancel()

	if _, err := r.cfg.Pool.Exec(cctx, q, kind, p.DeviceID, p.SlotID,
		nullString(p.LeaseID), p.JobID, jsonOrEmpty(detail)); err != nil {
		log.Warn("could not write audit event", "event", kind, "err", err)
	}
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// jsonOrEmpty renders detail as a jsonb literal. It is passed as text and cast
// in SQL: []byte would be sent as bytea, which does not cast to jsonb.
func jsonOrEmpty(v map[string]any) string {
	if len(v) == 0 {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		// A detail map the runner built itself failing to marshal is a bug,
		// but it must not cost the row that carries the actual result.
		return fmt.Sprintf(`{"detail_marshal_error":%q}`, err.Error())
	}
	return string(b)
}

// maxMessageLine bounds one line of device output quoted into an error
// message. farm.jobs.error, farm.job_attempts.error and farm.job_steps.error
// are what an operator reads; a single line of a minified stack trace is not
// worth a megabyte in three of them.
const maxMessageLine = 2000

// firstLine renders the first line of device output for a human-facing
// message.
//
// It sanitises, and that is not decoration. Its callers quote raw device bytes
// into strings that end up in text columns, and Postgres refuses U+0000
// outright — a crashed process emits them by the handful, and one of them
// would lose the whole row that explains why the job failed.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > maxMessageLine {
		s = s[:maxMessageLine] + "…"
	}
	return sanitiseText(s)
}
