package recovery

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The statements in this file are the ones farm.recovery_attempts is written
// and read through by every process that opens an attempt: the ladder, and the
// operator route that performs a slot power cycle by hand. They are exported
// so that the second cannot drift from the first — a row the API closes must
// be indistinguishable, to the UI and to the ladder's own budget query, from
// a row the loop closed.

// Execer is the slice of pgx a single statement needs. *pgxpool.Pool,
// *pgxpool.Conn and pgx.Tx all satisfy it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Querier is the slice of pgx a single row-returning query needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LockAttempts takes the transaction-scoped advisory lock under which attempts
// on one position are serialised across every process in the farm.
//
// The key is the device id when the position holds a device — the SAME key the
// ladder takes in begin, so an operator's cycle and the ladder's rung on the
// same phone contend for one lock rather than each opening a row the other
// cannot see — and the slot otherwise, so two operators cycling an empty
// position do not cut its VBUS twice. A transaction lock rather than a session
// lock, so COMMIT or a dying process releases it.
//
// lockClass is the ladder's [Config.LockClass]; a process that does not run
// the ladder passes [DefaultLockClass].
func LockAttempts(ctx context.Context, tx Execer, lockClass int32, deviceID *string, slotID int64) error {
	key := fmt.Sprintf("slot:%d", slotID)
	if deviceID != nil && *deviceID != "" {
		key = *deviceID
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, hashtext($2::text)::int)`,
		lockClass, key); err != nil {
		return fmt.Errorf("recovery: attempt lock for %s: %w", key, err)
	}
	return nil
}

// FinishAttempt closes an OPEN farm.recovery_attempts row with what happened:
// the outcome, and the detail merged over what the row was opened with.
//
// It reports closed=false, and no error, when no open row matched: the janitor
// may have marked a long-running attempt aborted in the meantime, and
// overwriting that would turn "the process running this was presumed dead"
// into a result nobody observed. The caller decides what to tell whoever is
// waiting; it must not claim the row carries an outcome it does not.
//
// farm.recovery_attempts.refusal is the column an operator reads to learn why
// a rung did not run. It is taken from detail[DetailRefusal] and COALESCEd
// under any refusal recorded earlier — a blast-radius check writes one before
// the action runs — rather than overwriting it.
func FinishAttempt(ctx context.Context, db Execer, id int64, out Outcome, detail map[string]any) (closed bool, err error) {
	var refusal any
	if r, ok := detail[DetailRefusal].(string); ok && r != "" {
		refusal = r
	}
	tag, err := db.Exec(ctx, `
UPDATE farm.recovery_attempts
   SET finished_at = now(), outcome = $2::text, detail = detail || $3::jsonb,
       refusal = COALESCE(refusal, $4::text)
 WHERE id = $1::bigint AND finished_at IS NULL`, id, string(out), jsonDetail(detail), refusal)
	if err != nil {
		return false, fmt.Errorf("recovery: finish attempt %d as %s: %w", id, out, err)
	}
	return tag.RowsAffected() == 1, nil
}

// PowerDomainSiblings lists the other positions in a power domain, as
// devpaths, for a power-domain acknowledgement.
//
// It selects every slot with this power_domain_id and drops only the target,
// which is the SAME set a blast-radius check over the domain covers. That
// correspondence is the whole warrant for the acknowledgement: a slot this
// query returned but the check did not cover would be a position going dark on
// nobody's authority, and a slot the check covered but this query missed would
// be one the agent then refuses over, after the rung has already been spent.
//
// It deliberately does not filter on farm.slots.state or on whether the slot
// holds a device. The agent compares against what is PHYSICALLY plugged in
// right now, and a slot the control plane calls inactive can still have a
// phone in it; a blast-radius check does not filter on state either, so an
// inactive slot holding a live lease is checked like any other. Filtering here
// would produce a list that looks narrower and is not.
func PowerDomainSiblings(ctx context.Context, db Querier, powerDomainID, slotID int64) ([]string, error) {
	// adb_devpath is GENERATED ALWAYS AS ('usb:' || usb_path) over a NOT NULL
	// column, so every row has one and there is nothing to filter out.
	rows, err := db.Query(ctx, `
SELECT s.adb_devpath
  FROM farm.slots s
 WHERE s.power_domain_id = $1::bigint
   AND s.id <> $2::bigint
 ORDER BY s.adb_devpath`, powerDomainID, slotID)
	if err != nil {
		return nil, fmt.Errorf("recovery: power domain siblings: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var devpath string
		if err := rows.Scan(&devpath); err != nil {
			return nil, fmt.Errorf("recovery: power domain siblings scan: %w", err)
		}
		out = append(out, devpath)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recovery: power domain siblings: %w", err)
	}
	return out, nil
}
