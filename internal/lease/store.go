// Package lease is the Go binding over the SQL lease functions defined in
// migrations/00002_lease.sql, plus the holder-side renewal loop in holder.go.
//
// # The invariant
//
// A lease is ended by the job, by a deadline the user wrote down, or by a
// human. Nothing else. Not a socket error, not a probe timeout, not a device
// going offline, not a pod dying.
//
// DeviceFarmer/STF issue #663 — open and unanswered since 2023 — releases a
// device mid-run after a ~90-minute ECONNRESET, destroying multi-hour work.
// That bug is a single fused decision: a transport failure was allowed to mean
// "the holder is gone". Everything below exists to keep those two facts apart.
//
// # How the invariant shows up as code here
//
//   - No function in this package accepts an io.Reader, a net.Conn, an ADB
//     handle, or any other transport value, and this package imports nothing
//     from internal/adbwire. A socket error has no parameter to travel through
//     and therefore no path into lease state. The dependency is one-way and
//     CI-enforced: adbwire must not import lease either.
//
//   - There is no connectivity release reason. ReleaseReason has exactly the
//     seven constants the schema's CHECK permits, and Release surfaces the
//     database's refusal of anything else as *CheckViolationError rather than
//     hiding it behind a client-side test.
//
//   - No client-side timestamps are ever sent. Not one query below binds a
//     time.Time parameter; every deadline is computed by Postgres against its
//     own now(). Pod clock skew, an NTP step, a VM live-migration or a
//     container paused by a busy node therefore cannot expire a lease early or
//     extend one late. Durations (rearm windows, gap floors) are relative and
//     are sent as intervals, which carry no notion of an absolute instant.
//
//   - Renew separates "zero rows" from "an error". Zero rows means FENCED and
//     is terminal. An error means the database was momentarily unreachable and
//     is transient. Collapsing those two — in either direction — recreates
//     #663, either by killing jobs on a database blip or by letting a fenced
//     process keep writing to a device that now belongs to someone else.
//
// # The three clocks, which are never collapsed
//
//  1. Lease liveness — leases.heartbeat_at / expires_at. Answers only "does the
//     entity holding this lease still exist?". Driven by Store.Renew.
//  2. Job liveness — device-side progress. An alerting concern that can NEVER
//     release a device. It is not represented in this package at all.
//  3. Device health — device_runtime.adb_state. Drives the watchdog and touches
//     leases exactly never. Postgres enforces this with a role, not a
//     convention: farm.lease_reclaim runs as farm_reaper, which has had SELECT
//     on farm.device_runtime revoked.
package lease

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors. Callers branch on these with errors.Is; each one is wrapped
// with identifying context before it is returned.
var (
	// ErrFenced means farm.lease_renew returned zero rows.
	//
	// THIS IS TERMINAL AND UNRECOVERABLE for the holder that saw it. The lease
	// was released, reclaimed, or re-attached by another instance, and the
	// device's fence_floor has moved above our fence. There is no retry, no
	// backoff and no grace: abort the job, close every ADB socket, write
	// nothing further to the device. Anything still in flight now belongs to
	// somebody else's six-hour run.
	//
	// ErrFenced is returned ONLY for zero rows. A dial failure, a timeout, a
	// cancelled context, a failover, a full connection pool — none of those are
	// fencing events, and none of them may be reported as this error.
	ErrFenced = errors.New("lease: fenced")

	// ErrNoCapacity means farm.lease_acquire returned zero rows: no healthy,
	// enabled, unleased device in the pool matched the job's selector, or the
	// insert lost a race. It is an ordinary scheduling outcome, not a failure.
	// Re-queue and try again.
	ErrNoCapacity = errors.New("lease: no capacity")

	// ErrJobNotFound means farm.lease_acquire was handed a job id that does not
	// exist (SQLSTATE P0002).
	ErrJobNotFound = errors.New("lease: job not found")
)

// SQLSTATEs we interpret. Spelled out rather than pulled from a dependency so
// that go.mod stays at three third-party modules.
const (
	sqlStateCheckViolation = "23514" // CHECK constraint, incl. release_reason
	sqlStateNoDataFound    = "P0002" // PL/pgSQL RAISE ... ERRCODE = no_data_found
)

// Defaults mirroring the SQL function defaults, applied when a caller passes a
// non-positive value rather than silently sending 0.
const (
	// DefaultRearm quarantines a slot after a release so the previous holder's
	// sockets are certainly severed before anything new is scheduled onto it.
	// It MUST exceed the node proxy's self-fence timeout.
	DefaultRearm = 35 * time.Second

	// DefaultGapFloor is the shortest control-plane silence that counts as an
	// outage worth refunding to tenants.
	DefaultGapFloor = 60 * time.Second

	// DefaultSuspectBatch and DefaultReclaimBatch match the SQL defaults. A
	// limit of zero would make the sweeper silently inert (LIMIT 0), which is
	// the kind of quiet failure that only shows up as a mass reclaim later.
	DefaultSuspectBatch = 500
	DefaultReclaimBatch = 100

	// DefaultWitnessMaxExtensions matches farm.lease_witness's own default: at
	// most twelve CONSECUTIVE witness-only extensions, so a wedged agent cannot
	// hold a device indefinitely on device-side evidence alone. A successful
	// renewal resets the counter.
	DefaultWitnessMaxExtensions = 12
)

// ReaperComponents is the default component set for Store.ReaperArm.
//
// The gap is computed across EVERY component on the renewal path, not just the
// reaper's own heartbeat. If farm-api were down while the reaper and Postgres
// stayed healthy, a reaper-only gap check would see a fresh heartbeat, record
// no outage, and after TTL+grace reclaim every unprotected lease in the farm —
// the exact mass-reclaim this design exists to prevent.
var ReaperComponents = []string{"reaper", "api", "scheduler"}

// CheckViolationError reports that Postgres refused a write because it violated
// a CHECK constraint (SQLSTATE 23514).
//
// The case this type exists for: passing a connectivity-flavoured release
// reason such as "device_offline". The schema has no such value, so the write
// is rejected at the database rather than quietly destroying hours of work.
// Callers must treat this as a programming error in the caller, not as a
// transient condition to retry.
type CheckViolationError struct {
	Op         string        // "release", "renew", ...
	Reason     ReleaseReason // the offending reason, when the op had one
	Constraint string        // constraint name, empty for trigger-raised violations
	Message    string        // Postgres' own message
	Detail     string

	pg *pgconn.PgError
}

func (e *CheckViolationError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("lease: %s rejected by database check (reason %q, constraint %q): %s",
			e.Op, e.Reason, e.Constraint, e.Message)
	}
	return fmt.Sprintf("lease: %s rejected by database check (constraint %q): %s",
		e.Op, e.Constraint, e.Message)
}

// Unwrap exposes the underlying *pgconn.PgError for callers that want the raw
// SQLSTATE.
func (e *CheckViolationError) Unwrap() error { return e.pg }

// Store is the binding layer over the farm.lease_* SQL functions.
//
// It holds no lease state of its own and no per-lease connection. Every method
// is one round trip that borrows a pooled connection and returns it; the whole
// decision lives in the function it calls, because that is where the row locks,
// the partial unique indexes and the role firewall are. Reimplementing any of
// that logic in Go would move it outside the transaction that makes it correct.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool. The pool must be sized so that the renewal path is
// never starved by bulk work: a holder that cannot borrow a connection for
// TTL+grace loses its device, which is why holder.go bounds every attempt with
// its own timeout and retries well inside the TTL.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Acquire allocates a device for a job, or re-attaches to the one it already
// holds.
//
// Acquire is IDEMPOTENT ON JOB ID and that is load-bearing. When a pod is
// evicted — node drain, preemption, spot reclaim, cluster upgrade, OOM kill —
// its replacement calls Acquire with the same job id and receives the same
// lease, the same device and the SAME FENCE back, with AcquireResult.Reattached
// set. The fence is not bumped because the job's own work may still be running
// detached on the device; bumping would fence out its own process.
//
// Returns ErrNoCapacity (zero rows) when nothing schedulable matched — an
// ordinary outcome; re-queue. Returns ErrJobNotFound when the job id is unknown.
func (s *Store) Acquire(ctx context.Context, jobID, holder, holderInstance string) (AcquireResult, error) {
	const q = `
SELECT a.lease_id::text, a.device_id::text, a.slot_id, a.fence,
       a.expires_at, a.reclaimable_at, a.reattached
  FROM farm.lease_acquire($1::uuid, $2::text, $3::uuid) AS a`

	var out AcquireResult
	err := s.pool.QueryRow(ctx, q, jobID, holder, holderInstance).Scan(
		&out.Lease.ID,
		&out.Lease.DeviceID,
		&out.Lease.SlotID,
		&out.Lease.Fence,
		&out.Lease.ExpiresAt,
		&out.Lease.ReclaimableAt,
		&out.Reattached,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return AcquireResult{}, fmt.Errorf("lease: acquire job %s: %w", jobID, ErrNoCapacity)
	case isSQLState(err, sqlStateNoDataFound):
		return AcquireResult{}, fmt.Errorf("lease: acquire job %s: %w", jobID, ErrJobNotFound)
	case err != nil:
		return AcquireResult{}, fmt.Errorf("lease: acquire job %s: %w", jobID, err)
	}

	// The identity half of the lease is what we asked for; the SQL returns only
	// the allocation half.
	out.Lease.JobID = jobID
	out.Lease.Holder = holder
	out.Lease.HolderInstance = holderInstance
	return out, nil
}

// Renew is the only thing that keeps a lease alive, and the zero-rows branch
// below is the single most important line of Go in this project.
//
// It contacts no device, takes no device lock, and reads no health. It answers
// exactly one question — "does the holder still exist?" — over a database
// connection that has nothing to do with the ADB data path.
//
// TWO OUTCOMES, NEVER CONFLATED:
//
//	ErrFenced  (zero rows)  Terminal. We no longer own this lease. Abort the
//	                        job, close every ADB socket, write nothing. Retrying
//	                        cannot help and would be an attempt to operate a
//	                        device that belongs to another job.
//	any other error         Transient. Postgres was unreachable for a moment.
//	                        The lease is untouched and still ours; the server
//	                        has not moved a single deadline. Retry with backoff
//	                        and DO NOT abort. Treating this as fencing is
//	                        precisely #663 with a different trigger.
func (s *Store) Renew(ctx context.Context, leaseID string, fence int64, holderInstance string) (RenewResult, error) {
	const q = `
SELECT r.expires_at, r.reclaimable_at, r.was_suspect
  FROM farm.lease_renew($1::uuid, $2::bigint, $3::uuid) AS r`

	var out RenewResult
	err := s.pool.QueryRow(ctx, q, leaseID, fence, holderInstance).
		Scan(&out.ExpiresAt, &out.ReclaimableAt, &out.WasSuspect)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Zero rows: the UPDATE matched nothing, so one of the four
			// predicates failed — wrong fence, wrong holder_instance, terminal
			// state, or the lease is gone. All of them mean the same thing to
			// us, and none of them are recoverable.
			return RenewResult{}, fmt.Errorf("lease: renew %s at fence %d: %w", leaseID, fence, ErrFenced)
		}
		// Deliberately NOT ErrFenced. Everything that is not zero rows is the
		// database being briefly unavailable, and a database blip must cost
		// nobody a device.
		return RenewResult{}, fmt.Errorf("lease: renew %s: %w", leaseID, err)
	}
	return out, nil
}

// Witness records on-device proof that the holder is alive and pushes
// reclaimable_at out by one grace period.
//
// This is evidence about the HOLDER, gathered on the device — a marker file
// touched by the holder's own agent — not evidence about device health. It
// buys a job that has lost its control-plane connection more room before the
// reaper considers it, capped at maxExtensions consecutive extensions so a
// wedged agent cannot hold a device forever. A successful Renew resets that
// counter to zero.
//
// ok is false when the witness was refused: the extension cap is exhausted, the
// lease is no longer live, or the fence is at or below the device's fence_floor
// (a stale witness from a process that has already been fenced). A refused
// witness is NOT a fencing event and must never abort a job on its own; only
// Renew can report fencing.
func (s *Store) Witness(ctx context.Context, leaseID string, fence int64, maxExtensions int) (reclaimableAt time.Time, ok bool, err error) {
	const q = `SELECT farm.lease_witness($1::uuid, $2::bigint, $3::int)`

	if maxExtensions <= 0 {
		maxExtensions = DefaultWitnessMaxExtensions
	}
	// Nullable: a scalar SQL function whose body matched no row yields NULL.
	var t *time.Time
	if err := s.pool.QueryRow(ctx, q, leaseID, fence, maxExtensions).Scan(&t); err != nil {
		return time.Time{}, false, fmt.Errorf("lease: witness %s: %w", leaseID, err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// Release ends a lease deliberately. This is the normal end of a job.
//
// It also bumps the device's fence_floor and quarantines the slot for rearm, so
// any socket still carrying the old fence is refused at the host proxy before
// the device can be handed to the next job. rearm must exceed the node proxy's
// self-fence timeout; a non-positive value is replaced by DefaultRearm rather
// than sent as zero.
//
// Returns false when nothing was released: the lease was already terminal or
// the fence did not match. That is not an error — it is the idempotent case,
// and a holder that has been fenced will land here.
//
// A reason outside the seven the schema permits raises check_violation and is
// returned as *CheckViolationError. That is deliberate: it is how the schema
// refuses "released because the transport dropped", and it is tested.
func (s *Store) Release(ctx context.Context, leaseID string, fence int64, reason ReleaseReason, rearm time.Duration) (bool, error) {
	const q = `SELECT farm.lease_release($1::uuid, $2::bigint, $3::text, $4::interval)`

	if rearm <= 0 {
		rearm = DefaultRearm
	}
	var released bool
	err := s.pool.QueryRow(ctx, q, leaseID, fence, string(reason), intervalArg(rearm)).Scan(&released)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlStateCheckViolation {
			return false, &CheckViolationError{
				Op:         "release",
				Reason:     reason,
				Constraint: pgErr.ConstraintName,
				Message:    pgErr.Message,
				Detail:     pgErr.Detail,
				pg:         pgErr,
			}
		}
		return false, fmt.Errorf("lease: release %s: %w", leaseID, err)
	}
	return released, nil
}

// ReaperArm performs cold-start quiescence and the control-plane gap refund,
// and must be called by the reaper before its first sweep after any restart.
//
// It returns the gap that was refunded (zero when there was none). Every live
// lease has its deadlines pushed out by exactly the outage, because our
// downtime is refunded to tenants and never charged to them as lease budget,
// and the reaper is then held quiet for the longest TTL it could have missed so
// that a restored control plane does not mass-revoke at the moment of recovery.
//
// components should normally be ReaperComponents: keying the gap on the
// reaper's own heartbeat alone is how a healthy reaper next to a dead API
// reclaims the entire farm.
func (s *Store) ReaperArm(ctx context.Context, components []string, gapFloor time.Duration) (time.Duration, error) {
	// EXTRACT yields numeric seconds; scale to microseconds server-side so no
	// float ever touches a deadline.
	const q = `
SELECT (EXTRACT(EPOCH FROM farm.reaper_arm($1::text[], $2::interval)) * 1000000)::bigint`

	if len(components) == 0 {
		components = ReaperComponents
	}
	if gapFloor <= 0 {
		gapFloor = DefaultGapFloor
	}
	var us int64
	if err := s.pool.QueryRow(ctx, q, components, intervalArg(gapFloor)).Scan(&us); err != nil {
		return 0, fmt.Errorf("lease: reaper arm: %w", err)
	}
	return time.Duration(us) * time.Microsecond, nil
}

// MarkSuspect moves leases whose heartbeat is overdue from held to suspect and
// returns them.
//
// Entering suspect DOES NOTHING. No reset, no reboot, no reallocation, no
// release. The device stays unschedulable and stays with its holder, and any
// heartbeat inside the grace band self-heals suspect back to held at the same
// fence with zero work lost. Every returned row is an alert; rows with
// Protected set will never be reclaimed at all and should page a human instead.
func (s *Store) MarkSuspect(ctx context.Context, limit int) ([]SuspectLease, error) {
	const q = `
SELECT m.lease_id::text, m.device_id::text, m.job_id::text, m.protected
  FROM farm.lease_mark_suspect($1::int) AS m`

	if limit <= 0 {
		limit = DefaultSuspectBatch
	}
	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("lease: mark suspect: %w", err)
	}
	defer rows.Close()

	var out []SuspectLease
	for rows.Next() {
		var l SuspectLease
		if err := rows.Scan(&l.LeaseID, &l.DeviceID, &l.JobID, &l.Protected); err != nil {
			return nil, fmt.Errorf("lease: mark suspect scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lease: mark suspect: %w", err)
	}
	return out, nil
}

// Reclaim is THE ONLY AUTOMATIC RELEASE PATH IN THE SYSTEM, and it fires on one
// fact alone: the holder stopped heartbeating for TTL + grace, produced no
// witness, is not protected, and no control-plane gap overlaps its silence.
//
// Note what it does not consider: device health. It cannot. The SQL function
// executes SET LOCAL ROLE farm_reaper for the duration of its transaction, and
// that role has had SELECT on farm.device_runtime revoked, so "reclaim it
// because the device looks offline" is not discouraged, it is unexecutable.
// Never add a health check in Go around this call; doing so would rebuild #663
// on top of a schema built to prevent it.
//
// Each returned row carries the new fence floor. The caller's next duty is
// cleanup, never reallocation: the slot's own rearm window governs when the
// device becomes schedulable again.
func (s *Store) Reclaim(ctx context.Context, limit int, rearm time.Duration) ([]ReclaimedLease, error) {
	const q = `
SELECT r.lease_id::text, r.device_id::text, r.slot_id, r.job_id::text,
       r.old_fence, r.new_floor
  FROM farm.lease_reclaim($1::int, $2::interval) AS r`

	if limit <= 0 {
		limit = DefaultReclaimBatch
	}
	if rearm <= 0 {
		rearm = DefaultRearm
	}
	rows, err := s.pool.Query(ctx, q, limit, intervalArg(rearm))
	if err != nil {
		return nil, fmt.Errorf("lease: reclaim: %w", err)
	}
	defer rows.Close()

	var out []ReclaimedLease
	for rows.Next() {
		var l ReclaimedLease
		if err := rows.Scan(&l.LeaseID, &l.DeviceID, &l.SlotID, &l.JobID, &l.OldFence, &l.NewFloor); err != nil {
			return nil, fmt.Errorf("lease: reclaim scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lease: reclaim: %w", err)
	}
	return out, nil
}

// ExpireMaxRuntime ends leases whose job has outrun jobs.max_runtime — the only
// other automatic ending, and it fires on a number the user wrote down
// themselves rather than on anything the system inferred.
func (s *Store) ExpireMaxRuntime(ctx context.Context, limit int) ([]ExpiredLease, error) {
	const q = `
SELECT e.lease_id::text, e.device_id::text, e.job_id::text
  FROM farm.lease_expire_max_runtime($1::int) AS e`

	if limit <= 0 {
		limit = DefaultReclaimBatch
	}
	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("lease: expire max runtime: %w", err)
	}
	defer rows.Close()

	var out []ExpiredLease
	for rows.Next() {
		var l ExpiredLease
		if err := rows.Scan(&l.LeaseID, &l.DeviceID, &l.JobID); err != nil {
			return nil, fmt.Errorf("lease: expire max runtime scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lease: expire max runtime: %w", err)
	}
	return out, nil
}

// ComponentBeat records that a component on the renewal path is alive.
//
// Every such component must call this on a timer. The reaper's gap computation
// takes the OLDEST beat across the whole set, so a component that stops beating
// while the others carry on still buys every live lease a refund instead of a
// mass reclaim.
func (s *Store) ComponentBeat(ctx context.Context, component string) error {
	const q = `SELECT farm.component_beat($1::text)`

	if _, err := s.pool.Exec(ctx, q, component); err != nil {
		return fmt.Errorf("lease: component beat %s: %w", component, err)
	}
	return nil
}

// isSQLState reports whether err is a Postgres error with the given SQLSTATE.
func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// intervalArg renders a duration as a Postgres interval literal.
//
// Sent as text and cast server-side, so the value crosses the wire in exact
// microseconds rather than through interval's float8 multiplication operator.
// It is a DURATION, not an instant: nothing here tells Postgres what time this
// pod thinks it is.
func intervalArg(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(int64(d/time.Microsecond), 10) + " microseconds"
}
