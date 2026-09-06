// Package janitor closes bookkeeping rows whose process is gone.
//
// # The gap it fills
//
// farm.job_steps and farm.job_attempts have exactly one writer, internal/runner,
// and that writer only writes while it is running. So every way a runner can
// stop without finishing leaves a row open forever:
//
//   - a jobrunner evicted mid-step leaves that step 'running' and its
//     farm.job_attempts row open,
//   - a process killed between its last recordStep and its closeAttempt leaves
//     every step terminal and the attempt open,
//   - internal/jobrunner only ever picks up jobs that still have a LIVE lease
//     (see its claimable query), so a job whose lease has ended is polled by
//     nothing at all and sits at 'running' until a human notices.
//
// The dashboard and `ctl job steps` then show a job forever executing a step
// that no process is executing. This loop is what ends that.
//
// # The one it used to make permanent
//
// An install step whose artifact push failed once left its farm.job_steps row
// at 'running' while the run carried on and the job was written 'succeeded'
// beside it — JOB-10, an API that says a job succeeded when its APK never
// landed. The passes above do not close that: sweepSteps moves the orphaned
// step to 'aborted', which tidies the contradiction without removing it, and
// sweepJobs is bounded by `x.state = 'running'`, so a job already at
// 'succeeded' is out of its scope by construction and no later cycle revisits
// it.
//
// Two changes close it. internal/runner now checks the step rows before it
// writes 'succeeded' and WITHHOLDS the verdict when they disagree, so the shape
// is no longer produced; and sweepUnevidencedSuccess below is the backstop for
// rows that reached it anyway — an older binary, a hand-edited row — recording
// such a job as failed with an error saying exactly what could not be
// evidenced. On a healthy farm it closes nothing, and its metric rising is a
// statement about the control plane rather than about tidying.
//
// # THE RULE THIS LOOP IS BUILT AROUND
//
// A step is an ORPHAN when its job has no live lease, or when its own attempt's
// lease is terminal — NOT merely because it has been running a long time.
//
// A six-hour shell_detached step is exactly what this system is for. Sweeping
// it because it "looks stale" would be DeviceFarmer/STF issue #663 wearing a
// different hat: work destroyed because a clock in the control plane decided,
// without evidence, that the holder must be dead. Duration is never evidence
// here. The only evidence accepted is the lease, and a lease in 'held' or
// 'suspect' is LIVE — suspect releases nothing and one heartbeat heals it.
//
// The liveness test is also what makes an eviction free. A replacement process
// re-attaches to the same lease at the same fence and internal/runner resumes
// the SAME attempt number from farm.jobs.checkpoint, rewriting that very step
// row. Closing the row while the lease is alive would delete the record of a
// step that is about to be resumed, and would abandon an attempt somebody is
// still executing.
//
// # What this loop may never do
//
// It must NEVER end a lease and must never write farm.leases at all. That is
// structural rather than promised: this package does not import internal/lease,
// so lease_release, lease_reclaim and every other ending are out of scope in
// every function here — the same barrier internal/watchdog and internal/recovery
// keep. Its heartbeat is one SQL statement for that reason. farm.leases appears
// in this file only inside SELECT and EXISTS, and janitor_test.go asserts that a
// full sweep leaves every lease row byte-identical, xmin included.
//
// Ending a lease is internal/lease's and internal/reaper's alone. This loop
// tidies the record of work that has already stopped; it never decides that
// work should stop.
//
// # What it can cause elsewhere
//
// "It writes no lease" is not the whole question; "can what it writes make
// somebody else end one?" is the rest of it. There is exactly one path, and it
// is worth naming rather than leaving for someone to rediscover.
//
// internal/jobrunner re-reads the job after attaching, and a job it finds
// terminal makes it hand the lease back with the reason the JOB carries. So a
// job this loop moves to 'failed' can be the thing a jobrunner reads. That
// path is safe, and not by luck: sweepJobs only moves a job that had NO live
// lease at the moment of its own write, and a jobrunner that has since
// allocated one has allocated it for a placement that has not started work —
// there is no run on it to destroy. What would be unsafe is moving a job whose
// work is still going, and the live-lease guard is what makes that impossible.
//
// Nothing else reads these rows and releases. Steps, attempts, bulk rows and
// recovery attempts are read by dashboards and by internal/recovery's budget
// check, none of which can end a lease.
//
// # Why a settle window is not a staleness rule
//
// Every sweep also requires the row to have been quiet for Config.Settle. That
// is hysteresis against reading a placement mid-construction, and it can only
// ever DELAY closing a row that is already an orphan by the rule above. It can
// never make a row an orphan. If Settle were set to zero the loop would still
// be correct, only racier; if the liveness test were removed, no value of
// Settle would save it.
//
// # Why the passes are separate statements
//
// Steps, attempts, jobs, bulk targets and recovery attempts are swept by
// independent statements outside a transaction, on purpose. Under READ
// COMMITTED each statement takes a FRESH snapshot, so a lease acquired between
// the scan and the write is visible to the write's own guard — which is the
// race that matters. A partial sweep is never worse than no sweep: aborted
// steps with a still-open attempt are picked up by the next cycle, and the job
// pass refuses to move a job while any of its steps or attempts is still open.
package janitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// Defaults. None of them can end a lease, so the worst a bad value here can do
// is sweep late or sweep in smaller batches.
const (
	// DefaultComponent is the name written to farm.component_heartbeat.
	//
	// It must NOT be added to FARM_REAPER_COMPONENTS. This loop is not on the
	// lease renewal path: nothing it does keeps a lease alive, so a janitor
	// outage is not a control-plane gap and must not refund lease time or arm
	// the reaper's quiesce window. The beat exists so operators can see whether
	// the sweeper is running, and for nothing else.
	DefaultComponent = "janitor"

	// DefaultInterval is the poll period. Orphans are produced by crashes, not
	// by traffic, so there is no benefit in polling fast — and every cycle is a
	// handful of index scans against tables the API also reads.
	DefaultInterval = 30 * time.Second

	// DefaultBatch bounds how many rows one pass may close. A control plane
	// that has been down for a day comes back to thousands of orphans; closing
	// them in bounded batches keeps one recovery cycle from monopolising the
	// pool the renewal path also borrows from.
	DefaultBatch = 200

	// DefaultSettle is the quiet window described in the package comment. Sixty
	// seconds comfortably exceeds the bookkeeping round trips internal/runner
	// and internal/api make on a context detached from cancellation, which is
	// the only way a row is legitimately in flux after its process has stopped.
	DefaultSettle = 60 * time.Second

	// DefaultCallTimeout bounds every individual database call. A wedged
	// statement must not wedge the loop.
	DefaultCallTimeout = 15 * time.Second

	// DefaultVerdictWindow is how far back sweepUnevidencedSuccess looks.
	//
	// It is the only bound in this file that can make a repair NOT happen, and
	// it exists because succeeded jobs are the one thing here that accumulates
	// without limit: an unbounded pass would re-read the whole history every
	// thirty seconds, grow slower every week, and eventually exceed
	// CallTimeout — at which point the backstop stops running at all, quietly,
	// on the busiest farms. Two days comfortably outlives a weekend outage of
	// the janitor itself, and an operator recovering from a longer one can
	// widen it for a cycle.
	DefaultVerdictWindow = 48 * time.Hour

	// DefaultVerdictInterval is how often that pass runs, as distinct from how
	// far back it looks.
	//
	// It is the only pass here that does not run every cycle, because it is the
	// only one whose cost is proportional to how much a farm has SUCCEEDED at
	// rather than to how much of it went wrong: deciding whether a job's rows
	// account for its spec means reading that job's rows, and on a healthy farm
	// every one of those reads finds nothing to do. Five minutes cuts that
	// standing cost by an order of magnitude, and what it delays is the
	// correction of a RECORD — no device is held while it waits, which is what
	// separates this pass from every other one here.
	DefaultVerdictInterval = 5 * time.Minute

	// DefaultLockKey is the pg_try_advisory_lock key that elects the single
	// active janitor. It spells "farmJant" in ASCII so an operator reading
	// pg_locks can tell whose lock it is.
	DefaultLockKey int64 = 0x6661726d4a616e74
)

// liveLease is the definition of a lease that still owns its device, repeated
// verbatim everywhere this package asks the question.
//
// 'suspect' is LIVE. It is an alert that a holder has gone quiet, it releases
// nothing, and a single heartbeat returns it to 'held'. Treating suspect as
// dead here would let a ten-minute network partition close the records of a
// six-hour run that is still going.
const liveLease = `l.state IN ('held','suspect')`

// stepLiveGuard is the whole of the orphan test for a step: the step is
// protected while a lease that could still be executing it is live.
//
// It is a named constant because janitor_test.go verifies it BY FALSIFICATION:
// the test removes exactly this text from stepScan and asserts that a
// long-running step under a live lease then DOES show up as sweepable. Without
// that, a test asserting "the live step was not touched" would keep passing if
// the guard were deleted, as long as some other clause happened to exclude the
// row — which is no test at all.
//
// stepClose asks the same question a second time at write time, with the
// attempt's lease passed as a parameter because there is no join to read it
// from there. The two must never diverge: the scan decides what is offered and
// the write decides what is closed, and only the write is authoritative.
const stepLiveGuard = `
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = s.job_id
                      AND ` + liveLease + `
                      AND (a.lease_id IS NULL OR l.id = a.lease_id))`

// Config is the janitor's wiring. Pool is required.
type Config struct {
	// Pool is used for the sweeps AND for the dedicated leadership connection,
	// so it must allow at least two connections: one held for the whole process
	// lifetime by the advisory lock, plus one to work with.
	Pool *pgxpool.Pool

	// Component is the farm.component_heartbeat key. Defaults to
	// DefaultComponent. See that constant before renaming it.
	Component string

	Interval    time.Duration
	Batch       int
	CallTimeout time.Duration

	// Settle is the quiet window every sweep requires on top of the orphan
	// test. It delays; it never qualifies. See the package comment.
	Settle time.Duration

	// VerdictWindow is how far back sweepUnevidencedSuccess looks for a job
	// whose success its own step rows contradict. Defaults to
	// DefaultVerdictWindow; read that constant before shortening it, because
	// unlike Settle this one can make a repair never happen.
	VerdictWindow time.Duration

	// VerdictInterval is how often that pass runs. Defaults to
	// DefaultVerdictInterval, which is many cycles: it is the one pass whose
	// cost a healthy farm pays in full.
	VerdictInterval time.Duration

	// RecoveryStale is how long an open farm.recovery_attempts row is allowed
	// to stay open before it is assumed dead. It defaults to
	// recovery.DefaultStaleAttempt deliberately: that is the same number
	// internal/recovery's own budget check uses to decide a device is still
	// busy climbing the ladder, and the two must not drift. A janitor that
	// swept sooner would unblock a rung the ladder still believes it owns.
	RecoveryStale time.Duration

	// LockKey is the advisory lock key for leader election. Every janitor
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
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.Settle <= 0 {
		c.Settle = DefaultSettle
	}
	if c.VerdictWindow <= 0 {
		c.VerdictWindow = DefaultVerdictWindow
	}
	if c.VerdictInterval <= 0 {
		c.VerdictInterval = DefaultVerdictInterval
	}
	// A window shorter than the settle it must outlive is a window that can
	// never contain anything: the pass would scan a range with no rows in it
	// and report a clean farm forever. Widening rather than clamping, because
	// the two are answers to different questions and the operator only set one.
	if c.VerdictWindow <= c.Settle {
		c.VerdictWindow = c.Settle + DefaultVerdictWindow
	}
	if c.RecoveryStale <= 0 {
		c.RecoveryStale = recovery.DefaultStaleAttempt
	}
	if c.LockKey == 0 {
		c.LockKey = DefaultLockKey
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Janitor is the sweeper loop. Construct it with New and run it with Run.
type Janitor struct {
	cfg Config
	log *slog.Logger

	// lead is the dedicated leadership connection and its lock state. It is
	// only ever touched from Run's goroutine.
	lead leadership

	// verdictDue is when sweepUnevidencedSuccess may next run. Like lead, it is
	// only ever touched from Run's goroutine — one cycle at a time, and a
	// standby that never becomes leader never reaches it.
	//
	// A monotonic instant and not a database one, deliberately: it schedules
	// THIS process's own work and is compared against nothing anybody else can
	// see. Every deadline that decides something about a row is still the
	// server's now(), computed in SQL. The zero value means "due now", so a
	// process that has just started, or has just taken leadership from one that
	// died, sweeps on its first cycle rather than after a first interval of
	// pretending it already had.
	verdictDue time.Time
}

// New validates cfg and returns a Janitor.
func New(cfg Config) (*Janitor, error) {
	if cfg.Pool == nil {
		return nil, errors.New("janitor: Config.Pool is required")
	}
	// A pool of one deadlocks this loop in a way that looks like a database
	// problem rather than a configuration one: leadership parks the only
	// connection on the advisory lock for the life of the process, every sweep
	// then waits out CallTimeout for a connection that cannot come back, and
	// all an operator sees is "could not scan" forever while the sweeper
	// silently closes nothing. Refuse it at construction instead.
	if conns := cfg.Pool.Config().MaxConns; conns < 2 {
		return nil, fmt.Errorf("janitor: Config.Pool allows %d connection(s); leader election "+
			"holds one for the life of the process, so the sweeps need at least one more", conns)
	}
	cfg.applyDefaults()
	return &Janitor{
		cfg:  cfg,
		log:  cfg.Logger.With("component", cfg.Component),
		lead: leadership{pool: cfg.Pool, key: cfg.LockKey, log: cfg.Logger},
	}, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation: for a daemon loop a SIGTERM is an orderly
// stop, not a failure. Nothing is swept on the way out — a row that is an
// orphan now will still be one when the replacement pod starts, and hurrying a
// sweep through a shutdown is how a sweeper closes a row belonging to work that
// is merely being handed over.
func (j *Janitor) Run(ctx context.Context) error {
	defer j.lead.release(ctx)
	// The gauge's contract is that it sums to one across replicas. A process
	// that has stopped sweeping but is still being scraped — Run returns before
	// the metrics listener does — would keep claiming leadership it has just
	// given up.
	defer leaderGauge.Set(0)

	j.log.Info("janitor loop starting",
		"interval", j.cfg.Interval, "batch", j.cfg.Batch,
		"settle", j.cfg.Settle, "recovery_stale", j.cfg.RecoveryStale,
		"verdict_window", j.cfg.VerdictWindow, "verdict_interval", j.cfg.VerdictInterval)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			j.log.Info("janitor loop stopping; nothing is swept on the way out")
			return nil
		case <-timer.C:
		}

		j.cycle(ctx)
		timer.Reset(jitter(j.cfg.Interval))
	}
}

// cycle runs one poll and returns how many rows it closed.
func (j *Janitor) cycle(ctx context.Context) int {
	// Run's select can pick the timer over a cancellation when both are ready,
	// so a cycle can be entered on a context that is already dead. Doing any of
	// it then costs a leadership handover and a warning for no work at all.
	if ctx.Err() != nil {
		return 0
	}
	cyclesTotal.Inc()

	// The beat is written whether or not we are the leader: it answers "is a
	// janitor process reachable?", which a standby ready to take over within
	// one poll answers truthfully.
	j.beat(ctx)

	leader, err := j.lead.ensure(ctx, j.cfg.CallTimeout)
	if err != nil {
		j.log.Warn("janitor leadership check failed; not sweeping this cycle", "err", err)
		leaderGauge.Set(0)
		return 0
	}
	if !leader {
		leaderGauge.Set(0)
		return 0
	}
	leaderGauge.Set(1)

	// Order matters exactly once: steps before attempts before jobs, because
	// the job pass refuses to move a job while any of its steps or attempts is
	// still open. Bulk targets come before bulk runs for the same reason. The
	// recovery sweep is independent of all of them.
	//
	// sweepUnevidencedSuccess comes after the step sweep for a smaller reason:
	// it reads step rows, and running it first would make it judge a 'running'
	// row the very next statement is about to close. Both orders reach the same
	// answer, this one reaches it in a single cycle.
	//
	// ctx is re-checked between passes rather than only at the top: a SIGTERM
	// arriving during the first pass would otherwise send five more statements
	// at a context that is already dead, each waiting out its own timeout on a
	// pool the shutdown is trying to drain.
	n := 0
	for _, pass := range []func(context.Context) int{
		j.sweepSteps,
		j.sweepAttempts,
		j.sweepJobs,
		j.sweepUnevidencedSuccessIfDue,
		j.sweepBulkTargets,
		j.sweepBulkRuns,
		j.sweepRecoveryAttempts,
	} {
		if ctx.Err() != nil {
			break
		}
		n += pass(ctx)
	}
	return n
}

// ---------------------------------------------------------------------------
// Orphaned job steps
// ---------------------------------------------------------------------------

// orphanStep is one row of the step scan, carrying enough of the lease to say
// in the closed row's error text WHY nothing can still be executing it.
type orphanStep struct {
	JobID     string
	Attempt   int
	StepIndex int32
	StepID    string
	Kind      string

	// AttemptLease is farm.job_attempts.lease_id for this step's attempt. Empty
	// means the attempt recorded no lease, or has no row at all.
	AttemptLease string
	LeaseState   string // '' when AttemptLease is empty, 'gone' when the row vanished
	LeaseReason  string
}

// stepScan finds steps that no process can still be executing.
//
// The orphan test is the WHERE clause and nothing else:
//
//	NOT EXISTS (a live lease for this job that is also this attempt's lease)
//
// Read it in its two halves. When the attempt recorded a lease, only THAT lease
// protects the step: a job placed again on another device holds a newer lease,
// and the old attempt's rows are dead history that the newer placement will
// never touch. When the attempt recorded no lease, any live lease on the job
// protects it — the conservative reading, chosen because "we do not know which
// placement this belongs to" must never resolve to "close it".
//
// The settle clause is a delay, never a qualification: see the package comment.
// It compares against the LATEST of the three instants at which something could
// still have been happening — the step's start, its attempt's start and its
// lease's ending — so a lease that ended one second ago buys the run's last
// bookkeeping writes a full window to land.
//
// The evidence clause in front of it is a guard against a row this loop has no
// business judging. "Aborted because the process running it is gone" is a claim
// about a process, and a step with no start of its own and no attempt row has
// no process to be gone: nothing ever placed it. No writer produces such a row
// today — internal/runner is the only one and it stamps started_at on the
// insert — but the schema allows it, and the obvious next feature does: writing
// the plan out as 'pending' rows when a job is submitted, so the UI can show
// the steps before a device is chosen. Those rows would arrive with no start,
// no attempt and, while the job waits in the queue, no lease, and every clause
// below would read them as orphans. Without this the sweeper would abort a
// whole job's plan before the job ever ran, on its first cycle, silently.
//
// It also repairs the settle window for those rows, which without it fall back
// to '-infinity' and are therefore ALWAYS past it — a fallback that turns the
// hysteresis inside out for exactly the rows the least is known about.
const stepScan = `
SELECT s.job_id::text, s.attempt, s.step_index, s.step_id, s.kind,
       COALESCE(a.lease_id::text, ''),
       CASE WHEN a.lease_id IS NULL THEN '' ELSE COALESCE(al.state, 'gone') END,
       COALESCE(al.release_reason, '')
  FROM farm.job_steps s
  LEFT JOIN farm.job_attempts a ON a.job_id = s.job_id AND a.attempt = s.attempt
  LEFT JOIN farm.leases al      ON al.id = a.lease_id
 WHERE s.state IN ('pending','running')
   AND (s.started_at IS NOT NULL OR a.started_at IS NOT NULL)` + stepLiveGuard + `
   AND COALESCE(GREATEST(s.started_at, a.started_at, al.released_at),
                '-infinity'::timestamptz) < now() - $1::interval
 ORDER BY s.job_id, s.attempt, s.step_index
 LIMIT $2`

// stepClose re-asserts the whole orphan test at write time.
//
// It is not a belt-and-braces repeat of the scan. This statement runs outside a
// transaction, so it takes its own snapshot: a lease acquired between the scan
// and this write IS visible here, and the step is left alone. That is the only
// window in which the scan's answer could have gone stale in the dangerous
// direction, and this closes it.
const stepClose = `
UPDATE farm.job_steps s
   SET state       = 'aborted',
       finished_at = now(),
       error       = CASE WHEN s.error IS NULL OR s.error = '' THEN $4::text
                          ELSE s.error || E'\n' || $4::text END,
       detail      = s.detail || $5::jsonb
 WHERE s.job_id = $1::uuid
   AND s.attempt = $2
   AND s.step_index = ANY($3::int[])
   AND s.state IN ('pending','running')
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = s.job_id
                      AND ` + liveLease + `
                      AND ($6::uuid IS NULL OR l.id = $6::uuid))`

func (j *Janitor) sweepSteps(ctx context.Context) int {
	rows, err := j.scanSteps(ctx)
	if err != nil {
		j.sweepFailed(ctx, "steps", "could not scan for orphaned job steps", "err", err)
		return 0
	}
	if len(rows) == 0 {
		return 0
	}

	// Grouped by (job, attempt) so one placement's abandoned steps are closed
	// by one statement with one explanation, rather than N statements with N
	// slightly different ones.
	type key struct {
		job     string
		attempt int
	}
	order := make([]key, 0, len(rows))
	group := make(map[key][]orphanStep, len(rows))
	for _, r := range rows {
		k := key{r.JobID, r.Attempt}
		if _, seen := group[k]; !seen {
			order = append(order, k)
		}
		group[k] = append(group[k], r)
	}

	closed := 0
	for _, k := range order {
		steps := group[k]
		idx := make([]int32, len(steps))
		names := make([]string, len(steps))
		for i, s := range steps {
			idx[i] = s.StepIndex
			// The kind travels into the log line because it is what an operator
			// reads first: an aborted 'install' and an aborted 'shell_detached'
			// leave the device in very different shapes.
			names[i] = s.StepID + "/" + s.Kind
		}
		head := steps[0]
		why := leaseVerdict(head.AttemptLease, head.LeaseState, head.LeaseReason)
		msg := fmt.Sprintf("aborted by the %s sweeper: no process can still be executing this "+
			"step — %s, so this attempt cannot be resumed. Its attempt is abandoned and the job "+
			"is placed again if its max_attempts budget allows.", j.cfg.Component, why)
		detail := jsonDetail(map[string]any{
			"swept_by":     j.cfg.Component,
			"orphan_cause": why,
			"lease_state":  head.LeaseState,
		})

		n, err := j.exec(ctx, stepClose, k.job, k.attempt, idx, msg, detail,
			nullUUID(head.AttemptLease))
		if err != nil {
			j.sweepFailed(ctx, "steps", "could not close orphaned job steps",
				"job", k.job, "attempt", k.attempt, "err", err)
			continue
		}
		if n == 0 {
			// The guard fired: a lease appeared, or another replica got here
			// first. Both are the design working, and neither is a problem.
			j.log.Debug("orphaned steps were no longer orphaned at write time",
				"job", k.job, "attempt", k.attempt)
			continue
		}
		closed += int(n)
		sweptTotal.WithLabelValues("job_step").Add(float64(n))
		j.log.Warn("closed job steps left behind by a process that is gone",
			"job", k.job, "attempt", k.attempt, "steps", names, "cause", why)
	}
	return closed
}

func (j *Janitor) scanSteps(ctx context.Context) ([]orphanStep, error) {
	cctx, cancel := j.db(ctx)
	defer cancel()

	rows, err := j.cfg.Pool.Query(cctx, stepScan, pgInterval(j.cfg.Settle), j.cfg.Batch)
	if err != nil {
		return nil, fmt.Errorf("janitor: scan orphaned steps: %w", err)
	}
	defer rows.Close()

	var out []orphanStep
	for rows.Next() {
		var s orphanStep
		if err := rows.Scan(&s.JobID, &s.Attempt, &s.StepIndex, &s.StepID, &s.Kind,
			&s.AttemptLease, &s.LeaseState, &s.LeaseReason); err != nil {
			return nil, fmt.Errorf("janitor: scan orphaned steps: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("janitor: scan orphaned steps: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Orphaned job attempts
// ---------------------------------------------------------------------------

// attemptScan finds attempts that are still open although nothing is running
// them. It is a separate pass from the step sweep rather than a consequence of
// it, because the two failure shapes are different: a process can die between
// its last recordStep and its closeAttempt, leaving every step terminal and the
// attempt open, and it can die between recordStepStart and recordStep, leaving
// a step open under an attempt that was later closed by somebody else.
//
// The orphan test is identical to the step scan's, which is the point: an
// attempt whose lease is live belongs to a placement that may still resume it,
// at the same attempt number, from farm.jobs.checkpoint.
const attemptScan = `
SELECT a.id, a.job_id::text, a.attempt,
       COALESCE(a.lease_id::text, ''),
       CASE WHEN a.lease_id IS NULL THEN '' ELSE COALESCE(al.state, 'gone') END,
       COALESCE(al.release_reason, '')
  FROM farm.job_attempts a
  LEFT JOIN farm.leases al ON al.id = a.lease_id
 WHERE a.finished_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = a.job_id
                      AND ` + liveLease + `
                      AND (a.lease_id IS NULL OR l.id = a.lease_id))
   AND COALESCE(GREATEST(a.started_at, al.released_at),
                '-infinity'::timestamptz) < now() - $1::interval
 ORDER BY a.started_at
 LIMIT $2`

// attemptClose closes one attempt as 'abandoned'.
//
// 'abandoned' and not 'failed': the vocabulary of farm.job_attempts.outcome
// distinguishes them and the difference decides what happens to the job. An
// attempt that was abandoned says nothing about the job or the device — the
// process running it disappeared — so it must not be counted as evidence when
// an operator asks whether four failures on four devices are a job problem.
const attemptClose = `
UPDATE farm.job_attempts a
   SET finished_at = now(),
       outcome     = 'abandoned',
       error       = CASE WHEN a.error IS NULL OR a.error = '' THEN $2::text
                          ELSE a.error || E'\n' || $2::text END
 WHERE a.id = $1
   AND a.finished_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = a.job_id
                      AND ` + liveLease + `
                      AND (a.lease_id IS NULL OR l.id = a.lease_id))`

func (j *Janitor) sweepAttempts(ctx context.Context) int {
	type openAttempt struct {
		ID          int64
		JobID       string
		Attempt     int
		LeaseID     string
		LeaseState  string
		LeaseReason string
	}

	cctx, cancel := j.db(ctx)
	rows, err := j.cfg.Pool.Query(cctx, attemptScan, pgInterval(j.cfg.Settle), j.cfg.Batch)
	if err != nil {
		cancel()
		j.sweepFailed(ctx, "attempts", "could not scan for orphaned job attempts", "err", err)
		return 0
	}
	var open []openAttempt
	for rows.Next() {
		var a openAttempt
		if err := rows.Scan(&a.ID, &a.JobID, &a.Attempt, &a.LeaseID,
			&a.LeaseState, &a.LeaseReason); err != nil {
			rows.Close()
			cancel()
			j.sweepFailed(ctx, "attempts", "could not read the orphaned attempt scan", "err", err)
			return 0
		}
		open = append(open, a)
	}
	rows.Close()
	scanErr := rows.Err()
	cancel()
	if scanErr != nil {
		j.sweepFailed(ctx, "attempts", "could not scan for orphaned job attempts", "err", scanErr)
		return 0
	}

	closed := 0
	for _, a := range open {
		why := leaseVerdict(a.LeaseID, a.LeaseState, a.LeaseReason)
		msg := fmt.Sprintf("abandoned by the %s sweeper: the process that opened this attempt "+
			"is gone — %s. Nothing about the job or the device is claimed by this outcome.",
			j.cfg.Component, why)

		n, err := j.exec(ctx, attemptClose, a.ID, msg)
		if err != nil {
			j.sweepFailed(ctx, "attempts", "could not close an orphaned job attempt",
				"attempt_row", a.ID, "job", a.JobID, "err", err)
			continue
		}
		if n == 0 {
			j.log.Debug("attempt was no longer orphaned at write time",
				"attempt_row", a.ID, "job", a.JobID)
			continue
		}
		closed++
		sweptTotal.WithLabelValues("job_attempt").Inc()
		j.log.Warn("abandoned a job attempt whose process is gone",
			"job", a.JobID, "attempt", a.Attempt, "attempt_row", a.ID, "cause", why)
		j.event(ctx, "job_attempt_abandoned", a.JobID, map[string]any{
			"attempt": a.Attempt, "attempt_row": a.ID, "cause": why,
		})
	}
	return closed
}

// ---------------------------------------------------------------------------
// Jobs left executing nothing
// ---------------------------------------------------------------------------

// jobSweep is the repair that makes an evicted jobrunner's work retryable
// instead of stuck.
//
// A job at 'running' with no live lease is polled by nothing: internal/jobrunner
// joins farm.leases on exactly the live states, so such a job is invisible to
// the one loop that would otherwise finish it. It gets the same disposition
// internal/runner and internal/jobrunner give a failed attempt — back on the
// queue while max_attempts allows, 'failed' when it does not — because that is
// what the user's own max_attempts means, and because an attempt this loop
// abandoned is not evidence of anything the job did wrong.
//
// The three NOT EXISTS clauses are ordering, not paranoia. They are what makes
// running the passes as separate statements safe: this one cannot move a job
// while any step or attempt of it is still open, so a half-applied sweep of the
// earlier passes leaves the job exactly where it was.
const jobSweep = `
UPDATE farm.jobs j
   SET state = CASE WHEN j.attempt < j.max_attempts THEN 'queued' ELSE 'failed' END,
       error = CASE WHEN j.attempt < j.max_attempts THEN $2::text ELSE $3::text END,
       finished_at = CASE WHEN j.attempt < j.max_attempts THEN j.finished_at ELSE now() END
 WHERE j.id IN (
         SELECT x.id
           FROM farm.jobs x
          WHERE x.state = 'running'
            AND COALESCE(x.started_at, x.created_at) < now() - $1::interval
            AND NOT EXISTS (SELECT 1 FROM farm.leases l
                             WHERE l.job_id = x.id AND ` + liveLease + `)
            AND NOT EXISTS (SELECT 1 FROM farm.job_steps s
                             WHERE s.job_id = x.id AND s.state IN ('pending','running'))
            AND NOT EXISTS (SELECT 1 FROM farm.job_attempts a
                             WHERE a.job_id = x.id AND a.finished_at IS NULL)
          ORDER BY COALESCE(x.started_at, x.created_at)
          LIMIT $4)
   AND j.state = 'running'
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = j.id AND ` + liveLease + `)
RETURNING j.id::text, j.state, j.attempt, j.max_attempts`

func (j *Janitor) sweepJobs(ctx context.Context) int {
	requeued := fmt.Sprintf("the placement running this job disappeared and its attempt was "+
		"abandoned by the %s sweeper; the job is queued for another device.", j.cfg.Component)
	failed := fmt.Sprintf("the placement running this job disappeared and its attempt was "+
		"abandoned by the %s sweeper; max_attempts is exhausted, so there is no placement "+
		"left to try.", j.cfg.Component)

	cctx, cancel := j.db(ctx)
	defer cancel()

	rows, err := j.cfg.Pool.Query(cctx, jobSweep,
		pgInterval(j.cfg.Settle), requeued, failed, j.cfg.Batch)
	if err != nil {
		j.sweepFailed(ctx, "jobs", "could not sweep jobs whose placement disappeared", "err", err)
		return 0
	}

	type moved struct {
		id          string
		state       string
		attempt     int
		maxAttempts int
	}
	var out []moved
	for rows.Next() {
		var m moved
		if err := rows.Scan(&m.id, &m.state, &m.attempt, &m.maxAttempts); err != nil {
			rows.Close()
			j.sweepFailed(ctx, "jobs", "could not read the job sweep result", "err", err)
			return 0
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		j.sweepFailed(ctx, "jobs", "could not sweep jobs whose placement disappeared", "err", err)
		return 0
	}

	for _, m := range out {
		kind, event := "job_requeued", "job_orphan_requeued"
		if m.state == "failed" {
			kind, event = "job_failed", "job_orphan_failed"
		}
		sweptTotal.WithLabelValues(kind).Inc()
		j.log.Warn("job was executing nothing; moved it out of 'running'",
			"job", m.id, "state", m.state,
			"attempt", m.attempt, "max_attempts", m.maxAttempts)
		j.event(ctx, event, m.id, map[string]any{
			"state": m.state, "attempt": m.attempt, "max_attempts": m.maxAttempts,
		})
	}
	return len(out)
}

// ---------------------------------------------------------------------------
// Jobs reported succeeded that their own rows contradict
// ---------------------------------------------------------------------------

// stepBeliesSuccess is the whole of the contradiction test for one step row of
// a job that claims to have succeeded.
//
// Read it as three claims a succeeded job cannot make about its own steps:
//
//   - 'pending' or 'running' — a step still recorded as executing. This is the
//     shape JOB-10 was reported as: an install whose push failed left its row
//     at 'running', the runner carried on, and the job was written 'succeeded'
//     beside it;
//   - 'aborted' — a step nothing finished. That includes the rows THIS loop
//     writes: sweepSteps turns the 'running' row above into 'aborted' one cycle
//     earlier, which is why that pass alone never closed this. It made the
//     contradiction tidier, not smaller;
//   - 'failed' without detail.tolerated. A failed step under a succeeded job is
//     legitimate when and only when the spec said continue_on_error, and
//     internal/runner stamps exactly that case as tolerated.
//
// What is deliberately NOT here: 'ok', and 'skipped'. A resumed attempt writes
// 'skipped' for every step an earlier run of the same attempt completed, so a
// perfectly successful job can be mostly skipped rows.
//
// internal/runner spells this predicate character for character in its own
// attemptStepsAgree, which asks the same question before a success is written.
// The two must never diverge: a row it accepts and this one rejects is a job
// reported succeeded and reversed here on the next cycle, each side logging
// that the other must be at fault.
const stepBeliesSuccess = `(s.state IN ('pending','running','aborted')
        OR (s.state = 'failed' AND COALESCE(s.detail->>'tolerated', '') <> 'true'))`

// specSteps is how many steps the job's own spec has, or 0 for a spec whose
// steps are missing or not an array — jsonb_array_length would raise on those,
// and a job whose spec cannot be counted is one this pass has no opinion about.
const specSteps = `CASE WHEN jsonb_typeof(%s.spec->'steps') = 'array'
                        THEN jsonb_array_length(%s.spec->'steps') ELSE 0 END`

// successClock is when a succeeded job last moved, and it is a COALESCE rather
// than plain finished_at because that column is legitimately NULL for a while:
// internal/runner writes the 'succeeded' state without a finish time and
// internal/jobrunner's stampFinished fills it in afterwards, so a pass keyed on
// finished_at alone would skip every job in that window and skip forever any
// job whose supervisor never got to stamp it.
//
// It is spelled to match the expression jobs_recent_success is built on
// (migration 00018) so the window below is a range scan. Change one and the
// other stops being used, silently and only under load.
const successClock = `COALESCE(x.finished_at, x.started_at, x.created_at)`

// successScan finds jobs reported 'succeeded' whose farm.job_steps rows do not
// support it.
//
// # Why this pass has to exist at all
//
// Every other pass in this file is bounded by a row that is still OPEN. The job
// sweep above is bounded by `x.state = 'running'`, so a job that reached
// 'succeeded' is out of its scope by construction and no cycle ever looks at it
// again: sweepSteps moves an orphaned 'running' step to 'aborted' and the pair
// "job succeeded, step aborted" then stands forever. That is the permanent
// half of JOB-10 and this is the pass that ends it.
//
// # Why it should stay at zero
//
// internal/runner now checks the rows before it writes 'succeeded' and
// withholds the verdict when they disagree — see its attemptStepsAgree and
// refuseUnevidencedSuccess — so a farm running this binary should never produce
// one of these. farm_janitor_swept_total{kind="job_unevidenced"} rising means
// something wrote a success past that guard: an older binary, a hand-edited
// row, or a second writer nobody has accounted for. It is a signal about the
// control plane, not routine tidying.
//
// # Why 'failed' and not something gentler
//
// The two honest options are "say the success cannot be evidenced" and "leave
// the lie". There is no third: a job cannot be un-finished by this loop —
// requeueing a job whose lease is long gone and whose attempt budget may be
// spent would re-run side effects to repair a record — and 'succeeded' is the
// one state a caller acts on. The error text says exactly what is known and
// nothing more, and the rows it is drawn from stay where they are for the
// operator to read.
//
// The live-lease and settle guards are the same ones every pass here carries. A
// job whose lease is still live is a job something may still be writing, and
// this loop never overrules a live holder.
//
// # The evidence clause, and what it deliberately does not judge
//
// A job with no farm.job_attempts row for its current attempt is skipped
// outright, for the reason stepScan skips a step with no start and no attempt:
// "this success is not evidenced" is a claim about a RUNNER, and a job no
// runner ever opened an attempt on has no runner to have failed. internal/runner
// writes that row before it does anything at all — before the work, before the
// device, before any reason to refuse the job — so a success it wrote always
// has one, and a success without one was written by something else entirely.
//
// The something else is real and shipped: internal/demo's simulated worker
// writes farm.jobs.state directly from a model of its own and creates neither
// attempt nor step rows, while its jobs carry full specs. Without this clause
// the demo box's own dashboard would show every completed job as failed within
// a minute of finishing, under an ERROR line accusing an absent binary. This
// loop tidies the record of work that has already stopped; work it has no
// record of at all is somebody else's to explain.
//
// # Why the window has an upper edge as well as a lower one
//
// Every other scan here is anchored to a partial index over rows that are still
// open, and stays small because a healthy farm has few of those. Succeeded jobs
// are the opposite: they accumulate forever, so an unbounded pass would re-read
// the whole history every cycle, take longer every week, and eventually blow
// CallTimeout — at which point the backstop stops running altogether, silently,
// on exactly the busy farm that needed it. Config.VerdictWindow bounds it to
// recently finished jobs, and jobs_recent_success (migration 00018) is the
// partial index that makes that bound a range scan rather than a filter.
//
// What no index can bound is the lateral below: deciding whether one job's rows
// account for its spec means reading that job's rows, so this pass costs one
// small index range scan per succeeded job in the window — and the HEALTHY case
// is the expensive one, because when nothing is wrong nothing is filtered out
// early. That is why it runs on Config.VerdictInterval rather than on every
// cycle like its neighbours.
//
// There is no ORDER BY, and that is the same argument from the other side: with
// one, LIMIT could only be applied after every candidate had been examined and
// sorted, so a farm with a backlog would pay the whole scan to close a batch.
// Progress does not need the ordering — a job this pass corrects leaves the
// candidate set, so successive cycles walk through a backlog whatever order
// they see it in.
var successScan = `
SELECT q.id, q.attempt, q.want, q.have, q.live, q.bad, q.live_step, q.bad_step
  FROM (
    SELECT x.id::text AS id, x.attempt AS attempt,
           ` + fmt.Sprintf(specSteps, "x", "x") + ` AS want,
           COALESCE(c.have, 0) AS have,
           COALESCE(c.live, 0) AS live,
           COALESCE(c.bad, 0) AS bad,
           COALESCE(c.live_step, '') AS live_step,
           COALESCE(c.bad_step, '') AS bad_step
      FROM farm.jobs x
      LEFT JOIN LATERAL (
           SELECT count(*) AS have,
                  count(*) FILTER (WHERE s.state IN ('pending','running')) AS live,
                  count(*) FILTER (WHERE ` + stepBeliesSuccess + `) AS bad,
                  (array_agg(s.step_id ORDER BY s.step_index)
                     FILTER (WHERE s.state IN ('pending','running')))[1] AS live_step,
                  (array_agg(s.step_id ORDER BY s.step_index)
                     FILTER (WHERE ` + stepBeliesSuccess + `))[1] AS bad_step
             FROM farm.job_steps s
            WHERE s.job_id = x.id AND s.attempt = x.attempt
              AND s.step_index >= 0
              AND s.step_index < ` + fmt.Sprintf(specSteps, "x", "x") + `) c ON true
     WHERE x.state = 'succeeded'
       AND ` + successClock + ` < now() - $1::interval
       AND ` + successClock + ` > now() - $2::interval
       AND EXISTS (SELECT 1 FROM farm.job_attempts a
                    WHERE a.job_id = x.id AND a.attempt = x.attempt)
       AND NOT EXISTS (SELECT 1 FROM farm.leases l
                        WHERE l.job_id = x.id AND ` + liveLease + `)
  ) q
 WHERE q.bad > 0 OR q.have < q.want
 LIMIT $3`

// successClose re-asserts the whole contradiction test at write time, for the
// reason stepClose does: this statement takes its own snapshot, so a lease
// acquired or a row repaired between the scan and the write is visible here and
// the job is left alone.
var successClose = `
UPDATE farm.jobs j
   SET state = 'failed',
       error = CASE WHEN j.error IS NULL OR j.error = '' THEN $2::text
                    ELSE j.error || E'\n' || $2::text END,
       finished_at = COALESCE(j.finished_at, now())
 WHERE j.id = $1::uuid
   AND j.state = 'succeeded'
   AND EXISTS (SELECT 1 FROM farm.job_attempts a
                WHERE a.job_id = j.id AND a.attempt = j.attempt)
   AND NOT EXISTS (SELECT 1 FROM farm.leases l
                    WHERE l.job_id = j.id AND ` + liveLease + `)
   AND ( EXISTS (SELECT 1 FROM farm.job_steps s
                  WHERE s.job_id = j.id AND s.attempt = j.attempt
                    AND s.step_index >= 0
                    AND s.step_index < ` + fmt.Sprintf(specSteps, "j", "j") + `
                    AND ` + stepBeliesSuccess + `)
      OR (SELECT count(*) FROM farm.job_steps s
           WHERE s.job_id = j.id AND s.attempt = j.attempt
             AND s.step_index >= 0
             AND s.step_index < ` + fmt.Sprintf(specSteps, "j", "j") + `)
         < ` + fmt.Sprintf(specSteps, "j", "j") + ` )`

// unevidenced is one job whose success its own step rows contradict.
type unevidenced struct {
	JobID   string
	Attempt int
	Want    int // steps in the job's spec
	Have    int // in-range step rows for the job's current attempt
	Live    int // rows still saying 'pending' or 'running'
	Bad     int // rows that contradict success, the live ones included

	// LiveStep and BadStep are the lowest-indexed row of each kind. They are
	// separate because Bad is a superset of Live: reporting the first
	// CONTRADICTING step as the one still executing would send an operator to
	// an aborted step and tell them to look for a process running it.
	LiveStep string
	BadStep  string
}

// why renders the sentence the job's error column and the audit event carry. It
// is built in Go rather than in SQL so an operator reading either one gets the
// counts, not a category.
func (u unevidenced) why(component string) string {
	var what string
	switch {
	case u.Live > 0:
		what = fmt.Sprintf("%d of its step row(s) still say 'pending' or 'running' (%s)",
			u.Live, u.LiveStep)
	case u.Bad > 0:
		what = fmt.Sprintf("%d of its step row(s) record work that did not finish (%s)",
			u.Bad, u.BadStep)
	default:
		what = fmt.Sprintf("only %d of the spec's %d step(s) left any record at all", u.Have, u.Want)
	}
	return fmt.Sprintf("this job was reported succeeded, but %s for attempt %d. "+
		"The %s sweeper cannot evidence that success, and a status that can be wrong is "+
		"worse than one that is missing, so the job is recorded as failed. Its step rows "+
		"are unchanged: read them for what actually happened.",
		what, u.Attempt, component)
}

// sweepUnevidencedSuccessIfDue is the pass's place in the cycle. It is separate
// from the sweep so a test can run the sweep directly, and so the clock that
// spaces it out is one line rather than a condition threaded through the work.
//
// The interval is applied even when a cycle finds a full batch, which is not an
// oversight: a backlog large enough to fill a batch is a backlog nobody is
// waiting on — no device is held by any of it — and draining it at full speed
// would put the standing cost of this pass back where the interval took it from.
func (j *Janitor) sweepUnevidencedSuccessIfDue(ctx context.Context) int {
	now := time.Now()
	if now.Before(j.verdictDue) {
		return 0
	}
	j.verdictDue = now.Add(j.cfg.VerdictInterval)
	return j.sweepUnevidencedSuccess(ctx)
}

func (j *Janitor) sweepUnevidencedSuccess(ctx context.Context) int {
	rows, err := j.scanUnevidenced(ctx)
	if err != nil {
		j.sweepFailed(ctx, "verdicts", "could not scan for jobs whose success their step rows contradict",
			"err", err)
		return 0
	}

	closed := 0
	for _, u := range rows {
		why := u.why(j.cfg.Component)

		n, err := j.exec(ctx, successClose, u.JobID, why)
		if err != nil {
			j.sweepFailed(ctx, "verdicts", "could not correct a job whose success is unevidenced",
				"job", u.JobID, "err", err)
			continue
		}
		if n == 0 {
			// A lease appeared, the rows were repaired, or another replica got
			// here first. All three are the design working.
			j.log.Debug("job's success was no longer unevidenced at write time", "job", u.JobID)
			continue
		}
		closed++
		sweptTotal.WithLabelValues("job_unevidenced").Inc()
		// Deliberately does not name a culprit. internal/runner refuses to
		// write a success its rows do not support, so something else wrote
		// this one — but "something else" is as much as this loop knows, and
		// an ERROR that guesses is an ERROR that misdirects.
		j.log.Error("a job reported 'succeeded' is contradicted by its own step rows; "+
			"recorded it as failed. Nothing in this binary writes such a success, so "+
			"look at what wrote this one",
			"job", u.JobID, "attempt", u.Attempt, "spec_steps", u.Want,
			"step_rows", u.Have, "live_rows", u.Live, "contradicting_rows", u.Bad,
			"live_step", u.LiveStep, "contradicting_step", u.BadStep)
		j.event(ctx, "job_success_unevidenced", u.JobID, map[string]any{
			"attempt": u.Attempt, "spec_steps": u.Want, "step_rows": u.Have,
			"live_rows": u.Live, "contradicting_rows": u.Bad,
			"live_step": u.LiveStep, "contradicting_step": u.BadStep, "reason": why,
		})
	}
	return closed
}

func (j *Janitor) scanUnevidenced(ctx context.Context) ([]unevidenced, error) {
	cctx, cancel := j.db(ctx)
	defer cancel()

	rows, err := j.cfg.Pool.Query(cctx, successScan,
		pgInterval(j.cfg.Settle), pgInterval(j.cfg.VerdictWindow), j.cfg.Batch)
	if err != nil {
		return nil, fmt.Errorf("janitor: scan unevidenced successes: %w", err)
	}
	defer rows.Close()

	var out []unevidenced
	for rows.Next() {
		var u unevidenced
		if err := rows.Scan(&u.JobID, &u.Attempt, &u.Want, &u.Have,
			&u.Live, &u.Bad, &u.LiveStep, &u.BadStep); err != nil {
			return nil, fmt.Errorf("janitor: scan unevidenced successes: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("janitor: scan unevidenced successes: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Bulk execution
// ---------------------------------------------------------------------------

// bulkMotionGuard is the whole of the liveness test for a PENDING bulk target:
// the target is protected while anything about its run is still moving.
//
// It is a named constant for the reason stepLiveGuard is. janitor_test.go
// removes exactly this text and asserts that the pending target of a run whose
// executor is demonstrably alive then DOES get closed — so the test that says
// "the live run kept its queue" is testing this clause and not some other one
// that happens to exclude the row.
//
// It cannot tell a finish written by internal/api from one written by the pass
// above it, so a run whose interrupted targets are closed in one cycle has its
// queue closed a window later rather than in the same statement. That is the
// safe direction and it converges; reading our own writes as evidence of life
// costs one poll, and the alternative is a guard that trusts a heuristic about
// who wrote a row.
const bulkMotionGuard = `
                   AND NOT EXISTS (SELECT 1 FROM farm.bulk_targets y
                                    WHERE y.run_id = x.run_id
                                      AND y.state = 'running'
                                      AND (y.started_at IS NULL
                                           OR y.started_at > now() - r.timeout - $1::interval))
                   AND NOT EXISTS (SELECT 1 FROM farm.bulk_targets y
                                    WHERE y.run_id = x.run_id
                                      AND y.finished_at > now() - r.timeout - $1::interval)`

// bulkTargetSweep closes targets a dead bulk run left behind.
//
// Bulk runs execute on goroutines inside the API process (internal/api), and a
// SIGKILLed pod leaves their rows exactly where they were. Three shapes are
// closed. Each is a statement about what CAN still be executing, never about
// how long something has taken:
//
//   - the run itself is already terminal, so by definition nothing is executing
//     any target of it;
//   - the target has been 'running' for longer than the run's OWN timeout — the
//     number the operator wrote down, and the same one that bounds the shell
//     context in internal/api — plus the settle window. A command whose context
//     expired that long ago is not still running;
//   - the target is still 'pending' under a run that has stopped moving: no
//     target of that run is running inside its own timeout, and none has
//     finished inside one either.
//
// That third shape needs its argument spelled out, because "pending for a long
// time" on its own would be a staleness rule and this file does not have those.
// internal/api gives every run its OWN semaphores — a channel per hub and a
// global of 64 created inside the run's goroutine, shared with no other run —
// so a live executor that holds a pending target while none of its own targets
// is running is an executor with a free slot, and it takes that slot in
// microseconds. The one way it can be busy without a 'running' row is a target
// whose mark-running UPDATE failed, and internal/api bounds that command by the
// run's timeout and then writes a finish. A run with no running target and no
// finish for longer than its own timeout plus the settle window therefore has
// no goroutines left.
//
// Without that third shape the sweep is half a repair, and half in the shape
// that matters: a killed pod almost always leaves targets queued behind the
// concurrency cap, bulkRunClose refuses to finish a run while any target is
// outstanding, and the run — and the operator's page — reads 'running' forever.
// That is the exact lie this loop exists to remove.
//
// The two closing words are internal/api's own, and the distinction is its own
// too: 'error' for a command that was interrupted, 'skipped' for one that never
// started, which is what its finishBulkRun writes when the control plane stops
// cleanly. A target this closes wrongly — the mark-failure case above — is
// corrected rather than lost: internal/api's result write is not conditioned on
// state, so a command that does come back overwrites this row with the truth.
const bulkTargetSweep = `
UPDATE farm.bulk_targets t
   SET state       = CASE WHEN t.state = 'running' THEN 'error' ELSE 'skipped' END,
       finished_at = now(),
       error       = CASE WHEN t.error IS NULL OR t.error = '' THEN $2::text
                          ELSE t.error || E'\n' || $2::text END
 WHERE (t.run_id, t.device_id) IN (
         SELECT x.run_id, x.device_id
           FROM farm.bulk_targets x
           JOIN farm.bulk_runs r ON r.id = x.run_id
          WHERE x.state IN ('pending','running')
            AND ( (r.state <> 'running'
                   AND COALESCE(r.finished_at, r.created_at) < now() - $1::interval)

               OR (r.state = 'running' AND x.state = 'running'
                   AND x.started_at IS NOT NULL
                   AND x.started_at < now() - r.timeout - $1::interval)

               OR (r.state = 'running' AND x.state = 'pending'
                   AND r.created_at < now() - r.timeout - $1::interval` + bulkMotionGuard + `) )
          ORDER BY x.run_id, x.device_id
          LIMIT $3)
   AND t.state IN ('pending','running')`

// bulkRunClose finishes a run whose targets have all stopped.
//
// Without it the sweep is half a repair: the operator's run page shows zero
// pending, zero running and a state of 'running' forever, which is the same lie
// the loop exists to remove. 'cancelled' is the honest word and it is the word
// internal/api already writes for this situation — its own finishBulkRun uses it
// when the control plane stops before the run is done, which is exactly what
// happened here.
//
// The guard is that nothing is outstanding and that the last thing to finish
// did so more than a settle window ago, so a run whose finisher is simply a few
// milliseconds behind is never taken away from it.
const bulkRunClose = `
UPDATE farm.bulk_runs r
   SET state = 'cancelled', finished_at = now()
 WHERE r.id IN (
         SELECT x.id FROM farm.bulk_runs x
          WHERE x.state = 'running'
            AND x.created_at < now() - $1::interval
            AND NOT EXISTS (SELECT 1 FROM farm.bulk_targets t
                             WHERE t.run_id = x.id AND t.state IN ('pending','running'))
            AND NOT EXISTS (SELECT 1 FROM farm.bulk_targets t
                             WHERE t.run_id = x.id AND t.finished_at > now() - $1::interval)
          ORDER BY x.created_at
          LIMIT $2)
   AND r.state = 'running'
RETURNING r.id::text`

func (j *Janitor) sweepBulkTargets(ctx context.Context) int {
	msg := fmt.Sprintf("closed by the %s sweeper: the control-plane process executing this "+
		"bulk run is gone, so no result will ever be recorded for this target.", j.cfg.Component)

	n, err := j.exec(ctx, bulkTargetSweep, pgInterval(j.cfg.Settle), msg, j.cfg.Batch)
	if err != nil {
		j.sweepFailed(ctx, "bulk", "could not close bulk targets left behind by a dead run", "err", err)
		return 0
	}
	if n > 0 {
		sweptTotal.WithLabelValues("bulk_target").Add(float64(n))
		j.log.Warn("closed bulk targets whose executing process is gone", "targets", n)
	}
	return int(n)
}

func (j *Janitor) sweepBulkRuns(ctx context.Context) int {
	cctx, cancel := j.db(ctx)
	defer cancel()

	rows, err := j.cfg.Pool.Query(cctx, bulkRunClose, pgInterval(j.cfg.Settle), j.cfg.Batch)
	if err != nil {
		j.sweepFailed(ctx, "bulk", "could not close bulk runs whose process is gone", "err", err)
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			j.sweepFailed(ctx, "bulk", "could not read the bulk run sweep result", "err", err)
			return 0
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		j.sweepFailed(ctx, "bulk", "could not close bulk runs whose process is gone", "err", err)
		return 0
	}

	for _, id := range ids {
		sweptTotal.WithLabelValues("bulk_run").Inc()
		j.log.Warn("closed a bulk run that nothing was executing", "run", id)
	}
	return len(ids)
}

// ---------------------------------------------------------------------------
// Recovery attempts
// ---------------------------------------------------------------------------

// recoverySweep closes recovery attempts that never recorded a finish.
//
// This one is not cosmetic. farm.recovery_attempts has a partial index on
// exactly this predicate and internal/recovery's budget check reads it as "an
// attempt on this device is still open", refusing every new rung while it
// stands. A ladder process killed mid-action therefore blocks recovery on that
// phone — so the row is closed as 'aborted', which is what abandoning it
// actually was, rather than as any outcome that claims to know what the action
// did to the device.
//
// The threshold is Config.RecoveryStale, which defaults to the ladder's own
// recovery.DefaultStaleAttempt: closing sooner would take a rung away from a
// ladder that still believes it owns it.
const recoverySweep = `
UPDATE farm.recovery_attempts a
   SET finished_at = now(),
       outcome     = 'aborted',
       detail      = a.detail || $2::jsonb
 WHERE a.id IN (SELECT x.id FROM farm.recovery_attempts x
                 WHERE x.finished_at IS NULL
                   AND x.started_at < now() - $1::interval
                 ORDER BY x.started_at
                 LIMIT $3)
   AND a.finished_at IS NULL`

func (j *Janitor) sweepRecoveryAttempts(ctx context.Context) int {
	detail := jsonDetail(map[string]any{
		"swept_by": j.cfg.Component,
		"reason": fmt.Sprintf("no finish was recorded within %s, so the process running this "+
			"recovery is gone; the attempt is closed as aborted rather than left blocking the "+
			"ladder on this device", j.cfg.RecoveryStale),
	})

	n, err := j.exec(ctx, recoverySweep, pgInterval(j.cfg.RecoveryStale), detail, j.cfg.Batch)
	if err != nil {
		j.sweepFailed(ctx, "recovery", "could not close stale recovery attempts", "err", err)
		return 0
	}
	if n > 0 {
		sweptTotal.WithLabelValues("recovery_attempt").Add(float64(n))
		j.log.Warn("closed recovery attempts that never recorded a finish",
			"attempts", n, "stale_after", j.cfg.RecoveryStale)
	}
	return int(n)
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// beat records that this janitor is alive.
//
// The statement is issued directly rather than through internal/lease, for the
// same reason internal/node and internal/recovery issue it directly: that
// package's Store is the door to acquiring, renewing and RELEASING leases, and
// a sweeper must not have that door in scope. One SQL statement is a smaller
// thing to own than an API that can end a lease.
func (j *Janitor) beat(ctx context.Context) {
	cctx, cancel := j.db(ctx)
	defer cancel()

	if _, err := j.cfg.Pool.Exec(cctx, `SELECT farm.component_beat($1::text)`, j.cfg.Component); err != nil {
		if ctx.Err() == nil {
			beatFailures.Inc()
			// Not a gap: this component is deliberately absent from
			// FARM_REAPER_COMPONENTS, so its silence moves no lease clock. It
			// only makes the sweeper invisible to operators.
			j.log.Warn("component heartbeat failed", "err", err)
		}
	}
}

// event records what was swept, so an operator asking why a job restarted finds
// an answer instead of a gap.
//
// A failure to write it is logged and swallowed: the repair has already
// happened, and re-running it because the audit line did not land would close
// nothing new while risking a second pass over rows somebody has since taken.
func (j *Janitor) event(ctx context.Context, kind, jobID string, detail map[string]any) {
	cctx, cancel := j.db(ctx)
	defer cancel()

	if _, err := j.cfg.Pool.Exec(cctx, `
INSERT INTO farm.events (kind, job_id, actor, detail)
VALUES ($1::text, $2::uuid, $3::text, $4::jsonb)`,
		kind, jobID, j.cfg.Component, jsonDetail(detail)); err != nil {
		auditFailures.Inc()
		j.log.Warn("could not record a sweep event; the rows are closed, the trail is not",
			"kind", kind, "job", jobID, "err", err)
	}
}

// sweepFailed records a sweep that could not run. It is deliberately silent
// when the reason is this process shutting down.
//
// Every database call in a cycle fails at once when ctx is cancelled, so a
// SIGTERM arriving mid-cycle would otherwise produce a burst of warnings and
// six increments of a counter whose whole purpose is to say that something is
// wrong with the database. An alert that fires on every rolling deploy is an
// alert nobody reads on the day a runner really is dying mid-write.
//
// A failed sweep closes nothing, which is the safe direction: the rows are
// still orphans on the next cycle, and on the next process.
func (j *Janitor) sweepFailed(ctx context.Context, sweep, msg string, args ...any) {
	if ctx.Err() != nil {
		return
	}
	sweepErrors.WithLabelValues(sweep).Inc()
	j.log.Warn(msg, args...)
}

// exec runs one bounded statement and returns the rows it affected.
func (j *Janitor) exec(ctx context.Context, q string, args ...any) (int64, error) {
	cctx, cancel := j.db(ctx)
	defer cancel()

	tag, err := j.cfg.Pool.Exec(cctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("janitor: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (j *Janitor) db(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, j.cfg.CallTimeout)
}

// leaseVerdict renders, for the closed row's own error text, why nothing can
// still be executing the work. An operator reading the row at 3am gets the
// reason there rather than having to join two tables to reconstruct it.
func leaseVerdict(leaseID, state, reason string) string {
	switch {
	case leaseID == "":
		return "this attempt recorded no lease and its job holds no live lease"
	case state == "gone":
		return fmt.Sprintf("its lease %s no longer exists", leaseID)
	case reason == "":
		return fmt.Sprintf("its lease %s is %s", leaseID, state)
	default:
		return fmt.Sprintf("its lease %s is %s (%s)", leaseID, state, reason)
	}
}

// nullUUID turns an empty id into SQL NULL, which the write guards read as
// "this attempt named no lease, so ANY live lease on the job protects it".
func nullUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func jsonDetail(m map[string]any) string {
	if m == nil {
		return `{}`
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"detail_marshal_error": ` + strconv.Quote(err.Error()) + `}`
	}
	return string(b)
}

// pgInterval renders a duration as a Postgres interval literal in exact
// microseconds. It is a DURATION and never an instant: nothing here tells
// Postgres what time this pod thinks it is, so every deadline in every sweep is
// evaluated against the server's own now().
func pgInterval(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(int64(d/time.Microsecond), 10) + " microseconds"
}

// jitter spreads timers so N janitor replicas — and the other loops sharing this
// database — do not wake in lockstep after a restart.
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
// The janitor needs it because its passes are separately-committed statements
// rather than one transaction. Two sweepers running at once would not corrupt
// anything — every write re-asserts its own guard and the second one simply
// finds zero rows — but they would double the scan load on farm.job_steps and
// farm.job_attempts, which the API reads on every job page, and they would emit
// two audit events for one repair.
type leadership struct {
	pool *pgxpool.Pool
	key  int64
	log  *slog.Logger

	conn *pgxpool.Conn
	held bool
}

// ensure returns whether this process currently holds the janitor lock, taking
// it if it is free.
func (l *leadership) ensure(ctx context.Context, timeout time.Duration) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if l.conn != nil {
		// The lock lives in the session. If the session is gone, so is the
		// lock, whatever this process last believed.
		if err := l.conn.Ping(cctx); err != nil {
			// Standing down is right either way, but a cancelled ctx is this
			// process shutting down rather than a database that lost us.
			if ctx.Err() == nil {
				l.log.Warn("janitor leadership connection died; standing down", "err", err)
			}
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
			return false, fmt.Errorf("janitor: acquire leadership connection: %w", err)
		}
		l.conn = c
	}

	var ok bool
	if err := l.conn.QueryRow(cctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&ok); err != nil {
		l.drop()
		return false, fmt.Errorf("janitor: try advisory lock: %w", err)
	}
	if !ok {
		// Another replica leads. Give the connection back rather than holding
		// one hostage per standby for the life of the deployment.
		l.drop()
		return false, nil
	}
	l.held = true
	l.log.Info("janitor acquired leadership", "lock_key", l.key)
	return true, nil
}

// release unlocks and returns the connection so a rolling deploy hands
// leadership over in milliseconds.
func (l *leadership) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	if l.held {
		// ctx is typically already cancelled by the signal that started the
		// shutdown, and an unlock on a cancelled context does not run.
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
		Namespace: "farm", Subsystem: "janitor", Name: "cycles_total",
		Help: "Janitor poll cycles, including cycles run by a standby that swept nothing.",
	})

	// A healthy farm sweeps nothing. Every increment here is a process that
	// died without finishing its bookkeeping, so a rising rate is the signal
	// that something upstream is being killed — not that the janitor is busy.
	sweptTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "janitor", Name: "swept_total",
		Help: "Rows closed because the process that owned them is gone. Flat at zero on a " +
			"healthy farm; a rising rate means processes are dying mid-write.",
	}, []string{"kind"})

	sweepErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "janitor", Name: "sweep_errors_total",
		Help: "Sweeps that failed. A failed sweep closes nothing, which is the safe direction.",
	}, []string{"sweep"})

	auditFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "janitor", Name: "audit_failures_total",
		Help: "Sweep events that were not written to farm.events. The rows are closed; the trail is not.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "janitor", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. This component must NOT be listed in " +
			"FARM_REAPER_COMPONENTS: a sweeper's downtime may not move lease clocks.",
	})

	leaderGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "janitor", Name: "leader",
		Help: "1 when this replica holds the janitor advisory lock. Sum across replicas must be 1.",
	})
)

// Collectors returns this package's metrics for registration by the binary. The
// known labels are pre-created so an alert on them is armed from the first
// scrape rather than from the first casualty.
func Collectors() []prometheus.Collector {
	for _, kind := range []string{
		"job_step", "job_attempt", "job_requeued", "job_failed", "job_unevidenced",
		"bulk_target", "bulk_run", "recovery_attempt",
	} {
		sweptTotal.WithLabelValues(kind)
	}
	for _, s := range []string{"steps", "attempts", "jobs", "verdicts", "bulk", "recovery"} {
		sweepErrors.WithLabelValues(s)
	}
	return []prometheus.Collector{
		cyclesTotal, sweptTotal, sweepErrors, auditFailures, beatFailures, leaderGauge,
	}
}
