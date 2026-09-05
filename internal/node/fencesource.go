package node

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
)

// hostFloors is the fenceproxy.FenceSource for one host, read through the
// pool the agent already owns.
//
// It is the one statement per host per poll interval that section 4 of the
// design document specifies — never per connection, never per packet — and it
// is the proxy's ONLY channel to the control plane. It reads. It has no
// second method, and the test in internal/fenceproxy that counts the
// interface's methods is what keeps it that way: there is nothing here through
// which a refusal could become an allocation decision.
//
// The snapshot carries no timestamp on purpose. The instant that matters is
// when the read completed on THIS machine, and fenceproxy.Cache stamps that
// with its own clock; a Postgres now() compared against a local clock is the
// skew hazard the rest of this system avoids by never sending a client
// timestamp to the database.
type hostFloors struct {
	pool    *pgxpool.Pool
	hostID  string
	timeout time.Duration
}

// Floors reads every position on the host and the floor of the device sitting
// in it. A host with no positions yields an empty snapshot, not an error: that
// is a fact about the join, and the cache treats it as knowing nothing new
// rather than as knowing that every floor vanished.
func (h hostFloors) Floors(ctx context.Context) (fenceproxy.Snapshot, error) {
	const q = `
SELECT s.adb_devpath, d.fence_floor
  FROM farm.devices d
  JOIN farm.slots   s ON s.id = d.current_slot_id
 WHERE d.host_id = $1
   AND s.adb_devpath IS NOT NULL`

	cctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	rows, err := h.pool.Query(cctx, q, h.hostID)
	if err != nil {
		return fenceproxy.Snapshot{}, fmt.Errorf("node: read fence floors for host %s: %w", h.hostID, err)
	}
	defer rows.Close()

	floors := map[string]int64{}
	for rows.Next() {
		var devpath string
		var floor int64
		if err := rows.Scan(&devpath, &floor); err != nil {
			return fenceproxy.Snapshot{}, fmt.Errorf("node: read fence floors for host %s: %w", h.hostID, err)
		}
		floors[devpath] = floor
	}
	if err := rows.Err(); err != nil {
		return fenceproxy.Snapshot{}, fmt.Errorf("node: read fence floors for host %s: %w", h.hostID, err)
	}
	return fenceproxy.Snapshot{Floors: floors}, nil
}
