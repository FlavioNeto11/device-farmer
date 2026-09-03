package lease

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// ReleaseReason is the vocabulary of DELIBERATE lease endings.
//
// It mirrors, exactly and exhaustively, the CHECK constraint on
// farm.leases.release_reason in migrations/00001_core.sql:
//
//	CHECK (release_reason IN ('completed','failed','job_cancelled','max_runtime',
//	                          'operator_revoked','holder_expired','device_retired'))
//
// Read the list again and notice what is missing: there is no value that
// describes connectivity. Not 'device_offline', not 'transport_error', not
// 'unreachable', not 'probe_failed'. That absence is the whole design. A socket
// error cannot end a lease because there is no word for it in the domain — in
// the database, and now in the Go type system too.
//
// ReleaseReason is a defined type rather than a bare string so that the seven
// constants below are the only reasons a caller can name without going out of
// their way. A conversion such as ReleaseReason("device_offline") still
// compiles — Go cannot forbid that — and Store.Release deliberately does NOT
// pre-empt it client-side. The database is the enforcement point: such a
// release raises check_violation (SQLSTATE 23514), surfaced here as
// *CheckViolationError. Validating in Go instead would replace a loud,
// tested, server-side refusal with a quiet client-side one.
type ReleaseReason string

const (
	// ReasonCompleted — the job finished its work. The overwhelmingly common
	// ending, and the only one most jobs will ever write.
	ReasonCompleted ReleaseReason = "completed"

	// ReasonFailed — the job itself failed. Note the subject: the JOB failed,
	// not the transport, not the device. A test that crashed is a failure; a
	// dropped ADB socket during a test is not, and must not be laundered into
	// this reason.
	ReasonFailed ReleaseReason = "failed"

	// ReasonJobCancelled — a human, or a control-plane action on a human's
	// behalf, cancelled the job.
	ReasonJobCancelled ReleaseReason = "job_cancelled"

	// ReasonMaxRuntime — the deadline the user wrote down in jobs.max_runtime
	// elapsed. Normally written by farm.lease_expire_max_runtime, not by a
	// holder.
	ReasonMaxRuntime ReleaseReason = "max_runtime"

	// ReasonOperatorRevoked — a human took the device back.
	ReasonOperatorRevoked ReleaseReason = "operator_revoked"

	// ReasonHolderExpired — the reaper reclaimed a lease whose holder stopped
	// heartbeating for TTL + grace and produced no witness. Written by
	// farm.lease_reclaim; a live holder has no business writing it.
	ReasonHolderExpired ReleaseReason = "holder_expired"

	// ReasonDeviceRetired — the physical device left the farm.
	ReasonDeviceRetired ReleaseReason = "device_retired"
)

// Valid reports whether r is one of the seven reasons the schema permits.
//
// Provided for callers that build a reason from user input (an operator UI, an
// API request body) and want to reject it before it reaches Postgres. It is
// intentionally not called by Store.Release; see the ReleaseReason doc.
func (r ReleaseReason) Valid() bool {
	switch r {
	case ReasonCompleted, ReasonFailed, ReasonJobCancelled, ReasonMaxRuntime,
		ReasonOperatorRevoked, ReasonHolderExpired, ReasonDeviceRetired:
		return true
	default:
		return false
	}
}

func (r ReleaseReason) String() string { return string(r) }

// Lease is the holder-side identity of one live lease.
//
// ID alone is never sufficient to mutate a lease: every mutating SQL function
// matches on (id, fence), and renew additionally matches holder_instance. A
// process that was paused for an hour and woke up after its lease was
// reclaimed therefore cannot touch the lease that replaced it — it presents a
// fence below the device's fence_floor and is refused at the database and
// again at the host proxy.
//
// Deadlines here are a CACHED COPY of server state, refreshed by each renewal.
// They are useful for reporting and for dashboards. They are not authoritative
// and must never be compared against the local clock to decide whether the
// lease is still valid: that decision belongs to Postgres, which owns now().
type Lease struct {
	// ID is farm.leases.id, an RFC 4122 UUID in canonical text form.
	ID string

	// DeviceID is farm.devices.id (UUID text). The device's stable farm-branded
	// identity, never its ADB serial — serials are observations and are not
	// unique.
	DeviceID string

	// SlotID is the physical USB position, nil when the lease is not bound to a
	// slot. Every ADB call that targets a position must resolve through the
	// slot (slots.adb_devpath), never through a serial.
	SlotID *int64

	// JobID is the ownership key. Not the holder pod: a pod eviction is the
	// most ordinary event in a Kubernetes control plane and must never cost a
	// device, so re-acquiring with the same JobID re-attaches to this same
	// lease at this same Fence.
	JobID string

	// Fence is monotonic and unique per lease. It is presented on every
	// renew, witness and release, and is compared against devices.fence_floor
	// at the host proxy so a stale socket is refused at the resource rather
	// than merely in the database.
	Fence int64

	// Holder is a pod name. AUDIT ONLY — it confers no ownership.
	Holder string

	// HolderInstance identifies this particular process incarnation. Renew
	// matches on it, so a resurrected pod that skipped acquire cannot renew a
	// lease that has since been re-attached by its replacement.
	HolderInstance string

	// ExpiresAt is when the lease becomes suspect if no heartbeat arrives.
	// Entering suspect does nothing but alert: no reset, no reboot, no
	// reallocation, and a later heartbeat self-heals it at the same fence.
	ExpiresAt time.Time

	// ReclaimableAt is ExpiresAt plus grace — the earliest instant the reaper
	// may reclaim, and only then if the lease is unprotected, unwitnessed, and
	// no control-plane gap overlaps the silence.
	ReclaimableAt time.Time
}

// AcquireResult is one row of farm.lease_acquire.
type AcquireResult struct {
	Lease Lease

	// Reattached is true when acquire found a live lease for this job and
	// handed it back instead of allocating a new device — same lease, same
	// device, SAME FENCE. The fence is deliberately not bumped: the job's own
	// work may still be running detached on the device, and bumping would fence
	// out its own process.
	//
	// A holder that sees Reattached must assume the device is dirty with its
	// own prior state and resume from jobs.checkpoint rather than starting over.
	Reattached bool
}

// RenewResult is one row of farm.lease_renew.
//
// Its very existence is the good case: a RenewResult means the lease is still
// ours. The bad case is not a field on this struct, it is ErrFenced.
type RenewResult struct {
	// ExpiresAt and ReclaimableAt are computed by Postgres against now() and
	// are monotonic — GREATEST() in the SQL plus a guard trigger prevent a
	// renewal from ever pulling a deadline backwards, which is what protects a
	// control-plane-gap refund from being silently erased by a routine
	// heartbeat.
	ExpiresAt     time.Time
	ReclaimableAt time.Time

	// WasSuspect reports that this renewal self-healed a lease the sweeper had
	// already marked suspect. It is an ALERTING signal and nothing more: the
	// lease never left its holder, the device was never reallocated, and no
	// work was lost. The SQL computes it from a pre-image CTE because RETURNING
	// yields the post-update row, where state is always 'held'.
	WasSuspect bool
}

// SuspectLease is one row of farm.lease_mark_suspect.
//
// Every row here is an alert, never an action. Nothing has been released and
// nothing may be released on the strength of this row.
type SuspectLease struct {
	LeaseID   string
	DeviceID  string
	JobID     string
	Protected bool // true => the reaper will never take it; page a human instead
}

// ReclaimedLease is one row of farm.lease_reclaim: a lease that was actually
// taken back because its holder went silent for TTL + grace across no
// control-plane gap.
type ReclaimedLease struct {
	LeaseID  string
	DeviceID string
	SlotID   *int64
	JobID    string

	// OldFence is the fence the departed holder still believes it owns.
	OldFence int64

	// NewFloor is the device's new fence_floor. Any socket still carrying
	// OldFence is now refused at the host proxy, which is what makes the
	// handover safe rather than merely hopeful.
	NewFloor int64
}

// ExpiredLease is one row of farm.lease_expire_max_runtime: a lease ended by
// the one user-supplied clock that is allowed to end a lease automatically.
type ExpiredLease struct {
	LeaseID  string
	DeviceID string
	JobID    string
}

// NewHolderInstance mints a random RFC 4122 v4 UUID for use as a lease's
// holder_instance.
//
// Call it once per process incarnation, at startup, and never again: it is the
// value farm.lease_renew matches on, so re-minting it mid-run fences the
// process out of its own lease.
func NewHolderInstance() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lease: generate holder instance: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}
