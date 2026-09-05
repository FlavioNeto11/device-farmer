package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// POST /api/v1/quarantines — an operator opens a quarantine at the scope the
// evidence supports.
//
// The recovery ladder opens quarantines on its own evidence, and only at the
// three scopes its evidence can reach: a device it has exhausted the ladder
// on, a hub whose devices failed together, a host it has climbed to. The
// other two scopes the schema has always permitted — 'slot' and
// 'power_domain' — had no writer anywhere. An operator who knew one port was
// chewing cables, or that a ganged power switch browned out under load, had
// to choose between quarantining one device (which the next enrolment on that
// port would walk straight past) and quarantining the whole hub (six healthy
// phones for one bad socket). This route is where a human states what they
// actually know.
//
// The row is opened with the same effect the ladder's own quarantines have:
// every covered device leaves allocation now, in the same transaction, and
// stays out until POST /api/v1/quarantines/{id}/close returns it. It never
// touches farm.leases. A covered device three hours into somebody's job keeps
// its lease, its fence and its deadline; the quarantine stops the NEXT
// allocation, and the reply says how many live leases are inside it so the
// operator knows what they are waiting on.

// quarantineOpenRequest names the scope and exactly the subject that scope
// identifies. device_id accepts a uuid or a farm_uid, as /api/v1/devices/{id}
// does; the other subjects are the ids the topology listing reports.
type quarantineOpenRequest struct {
	Scope         string `json:"scope"`
	DeviceID      string `json:"device_id,omitempty"`
	SlotID        *int64 `json:"slot_id,omitempty"`
	HubID         *int64 `json:"hub_id,omitempty"`
	HostID        string `json:"host_id,omitempty"`
	PowerDomainID *int64 `json:"power_domain_id,omitempty"`
	Reason        string `json:"reason"`
}

// quarantineSubject is the request resolved against the schema: the column
// the scope names is always set, and host_id is filled for every scope, as
// the ladder does, so the row can be reported without a join. Every predicate
// that reads the table is driven by scope, so the extra column changes what a
// listing shows and nothing about what the row covers.
type quarantineSubject struct {
	deviceID      *string
	slotID        *int64
	hubID         *int64
	hostID        *string
	powerDomainID *int64
}

// subjectFieldFor is the one request field each scope is identified by. A
// request naming any other subject field is refused rather than having the
// stray field ignored: an operator who sends scope=hub with a device_id has
// made a mistake, and which of the two they meant is not for this handler to
// guess.
var subjectFieldFor = map[string]string{
	"device":       "device_id",
	"slot":         "slot_id",
	"hub":          "hub_id",
	"host":         "host_id",
	"power_domain": "power_domain_id",
}

// openQuarantineMatch is the "same subject, same scope, still open" test used
// both to refuse a duplicate and to name the row that already exists. The
// partial unique indexes enforce the same rule; this is what makes losing the
// race a 409 with an id in it rather than a bare constraint error.
const openQuarantineMatch = `
q.closed_at IS NULL AND q.scope = $1::text
AND CASE $1::text
      WHEN 'device'       THEN q.device_id       = $2::uuid
      WHEN 'slot'         THEN q.slot_id         = $3::bigint
      WHEN 'hub'          THEN q.hub_id          = $4::bigint
      WHEN 'host'         THEN q.host_id         = $5::text
      WHEN 'power_domain' THEN q.power_domain_id = $6::bigint
    END`

// quarantineSweep takes every device the new row covers out of allocation.
//
// The covered set is decided by SCOPE, in the same five arms
// handleQuarantineClose uses to decide what a close releases; the two have to
// agree or a close could free a device an open never froze, or the reverse.
//
// Both halves of "out of allocation" are written, because farm.lease_acquire
// consults both: health, which the watchdog owns, and admin_state, which the
// hot allocation index is built on. health moves only from a state that is
// not already somebody's decision — 'quarantined' is this same mechanism,
// 'retired' is permanent, and 'parked' is a deliberate hold whose reason must
// not be overwritten by a sweep (migration 00008 refuses the write anyway).
// admin_state moves only from 'enabled' for the same reason: 'disabled',
// 'parked' and 'retired' are other people's decisions.
//
// The live-lease count is a READ. Nothing here writes farm.leases, and the
// number is reported so the operator knows how many runs are still inside
// the blast radius and will end on their own terms.
const quarantineSweep = `
WITH covered AS (
  SELECT d.id AS device_id
    FROM farm.devices d
    LEFT JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE ($1::text = 'device' AND d.id = $2::uuid)
      OR ($1::text = 'slot'   AND d.current_slot_id = $3::bigint)
      OR ($1::text = 'hub'    AND s.hub_id = $4::bigint)
      OR ($1::text = 'host'   AND s.host_id = $5::text)
      OR ($1::text = 'power_domain' AND s.power_domain_id = $6::bigint)
), health AS (
  UPDATE farm.device_runtime r
     SET health = 'quarantined', health_since = now(), updated_at = now()
   WHERE r.device_id IN (SELECT device_id FROM covered)
     AND r.health NOT IN ('quarantined','retired','parked')
  RETURNING 1
), admin AS (
  UPDATE farm.devices d
     SET admin_state = 'quarantined', updated_at = now()
   WHERE d.id IN (SELECT device_id FROM covered)
     AND d.admin_state = 'enabled'
  RETURNING 1
), live AS (
  SELECT count(*) AS n FROM farm.leases l
   WHERE l.device_id IN (SELECT device_id FROM covered)
     AND l.state IN ('held','suspect')
)
SELECT (SELECT count(*) FROM covered),
       (SELECT count(*) FROM health),
       (SELECT count(*) FROM admin),
       (SELECT n FROM live)`

// handleQuarantineOpen serves POST /api/v1/quarantines.
func (s *Server) handleQuarantineOpen(w http.ResponseWriter, r *http.Request) {
	var req quarantineOpenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	scope := strings.TrimSpace(req.Scope)
	want, known := subjectFieldFor[scope]
	if !known {
		badRequest(w, "scope must be one of device, slot, power_domain, hub or host",
			map[string]any{"scope": req.Scope})
		return
	}
	var given []string
	if strings.TrimSpace(req.DeviceID) != "" {
		given = append(given, "device_id")
	}
	if req.SlotID != nil {
		given = append(given, "slot_id")
	}
	if req.HubID != nil {
		given = append(given, "hub_id")
	}
	if strings.TrimSpace(req.HostID) != "" {
		given = append(given, "host_id")
	}
	if req.PowerDomainID != nil {
		given = append(given, "power_domain_id")
	}
	if len(given) != 1 || given[0] != want {
		badRequest(w, fmt.Sprintf("scope %q is identified by %s, and by nothing else", scope, want),
			map[string]any{"scope": scope, "expected": want, "given": given})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required: a quarantine with no reason is indistinguishable from a fault", nil)
		return
	}
	who := actor(r.Context())

	subj, err := s.resolveQuarantineSubject(r, scope, &req)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("no such %s", strings.ReplaceAll(scope, "_", " ")),
				map[string]any{"scope": scope, want: subjectValue(&req, want)})
			return
		}
		s.fail(w, r, "open quarantine: resolve subject", err)
		return
	}
	args := []any{scope, subj.deviceID, subj.slotID, subj.hubID, subj.hostID, subj.powerDomainID}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.fail(w, r, "open quarantine: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	var id int64
	err = tx.QueryRow(r.Context(), `
INSERT INTO farm.quarantines
  (scope, device_id, slot_id, hub_id, host_id, power_domain_id, reason, auto)
SELECT $1::text, $2::uuid, $3::bigint, $4::bigint, $5::text, $6::bigint, $7::text, false
 WHERE NOT EXISTS (SELECT 1 FROM farm.quarantines q WHERE `+openQuarantineMatch+`)
RETURNING id`, append(args, reason)...).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		lostRace := errors.As(err, &pgErr) && pgErr.Code == "23505"
		if !errors.Is(err, pgx.ErrNoRows) && !lostRace {
			s.fail(w, r, "open quarantine", err)
			return
		}
		// Already open, either before this request or by a request that won
		// the race between the EXISTS test and the insert. Either way the
		// answer is the row that exists, not an error about the one that does
		// not. Rolled back first so the read is not answered from inside a
		// transaction that will never commit.
		_ = tx.Rollback(r.Context())
		var existing int64
		readErr := s.pool.QueryRow(r.Context(),
			`SELECT q.id FROM farm.quarantines q WHERE `+openQuarantineMatch, args...).Scan(&existing)
		if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
			s.fail(w, r, "open quarantine: read the open row", readErr)
			return
		}
		detail := map[string]any{"scope": scope}
		if readErr == nil {
			detail["quarantine_id"] = existing
		}
		writeError(w, http.StatusConflict, CodeConflict,
			"an open quarantine already covers this "+strings.ReplaceAll(scope, "_", " ")+
				"; close it before opening another, or leave it — it is doing the same job",
			detail)
		return
	}

	var covered, frozen, disabled, live int64
	if err := tx.QueryRow(r.Context(), quarantineSweep, args...).
		Scan(&covered, &frozen, &disabled, &live); err != nil {
		s.fail(w, r, "open quarantine: take the devices out of allocation", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, r, "open quarantine: commit", err)
		return
	}

	detail := map[string]any{
		"quarantine_id":    id,
		"scope":            scope,
		"device_id":        subj.deviceID,
		"slot_id":          subj.slotID,
		"hub_id":           subj.hubID,
		"host_id":          subj.hostID,
		"power_domain_id":  subj.powerDomainID,
		"devices_covered":  covered,
		"devices_frozen":   frozen,
		"devices_disabled": disabled,
		"live_leases":      live,
	}
	// The devices are already out of allocation; who took them out, and why,
	// must be recorded whether or not the operator's connection survives.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, "quarantine.open", fmt.Sprintf("quarantine:%d", id), reason, detail)
	s.recordEvent(bookCtx, eventRow{
		Kind: "quarantine_opened", DeviceID: subj.deviceID, SlotID: subj.slotID, Actor: who, Detail: detail,
	})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues("quarantine.open", "ok").Inc()
	s.log.InfoContext(r.Context(), "quarantine opened by operator",
		"quarantine_id", id, "scope", scope, "actor", who, "reason", reason,
		"devices_covered", covered, "live_leases", live)

	note := fmt.Sprintf("%d device(s) are covered and will not be allocated until this "+
		"quarantine is closed.", covered)
	if live > 0 {
		note += fmt.Sprintf(" %d of them hold a live lease: those runs are untouched and end on "+
			"their own terms — nothing here releases a lease.", live)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"quarantine_id":   id,
		"scope":           scope,
		"device_id":       subj.deviceID,
		"slot_id":         subj.slotID,
		"hub_id":          subj.hubID,
		"host_id":         subj.hostID,
		"power_domain_id": subj.powerDomainID,
		"reason":          reason,
		// Counts rather than a bare "opened": zero covered devices on a
		// power-domain quarantine means the domain has no slots wired to it,
		// which is worth knowing before walking to the rack.
		"devices_covered":  covered,
		"devices_frozen":   frozen,
		"devices_disabled": disabled,
		"live_leases":      live,
		"note":             note,
	})
}

// resolveQuarantineSubject checks that the subject exists and reads the
// columns the row will carry. pgx.ErrNoRows means the subject does not exist.
func (s *Server) resolveQuarantineSubject(r *http.Request, scope string, req *quarantineOpenRequest) (quarantineSubject, error) {
	ctx := r.Context()
	var subj quarantineSubject
	var err error
	switch scope {
	case "device":
		// id::text rather than a uuid cast on the parameter, so a farm_uid —
		// or a typo — is a miss rather than SQLSTATE 22P02.
		err = s.pool.QueryRow(ctx, `
SELECT d.id::text, d.current_slot_id, d.host_id
  FROM farm.devices d
 WHERE d.id::text = $1 OR d.farm_uid = $1`, strings.TrimSpace(req.DeviceID)).
			Scan(&subj.deviceID, &subj.slotID, &subj.hostID)
	case "slot":
		err = s.pool.QueryRow(ctx,
			`SELECT s.id, s.host_id FROM farm.slots s WHERE s.id = $1`, *req.SlotID).
			Scan(&subj.slotID, &subj.hostID)
	case "hub":
		err = s.pool.QueryRow(ctx,
			`SELECT h.id, h.host_id FROM farm.hubs h WHERE h.id = $1`, *req.HubID).
			Scan(&subj.hubID, &subj.hostID)
	case "host":
		err = s.pool.QueryRow(ctx,
			`SELECT h.id FROM farm.hosts h WHERE h.id = $1`, strings.TrimSpace(req.HostID)).
			Scan(&subj.hostID)
	case "power_domain":
		err = s.pool.QueryRow(ctx,
			`SELECT pd.id, pd.host_id FROM farm.power_domains pd WHERE pd.id = $1`, *req.PowerDomainID).
			Scan(&subj.powerDomainID, &subj.hostID)
	}
	return subj, err
}

// subjectValue is the request's value for one subject field, for the 404 body.
func subjectValue(req *quarantineOpenRequest, field string) any {
	switch field {
	case "device_id":
		return strings.TrimSpace(req.DeviceID)
	case "slot_id":
		return req.SlotID
	case "hub_id":
		return req.HubID
	case "host_id":
		return strings.TrimSpace(req.HostID)
	case "power_domain_id":
		return req.PowerDomainID
	}
	return nil
}
