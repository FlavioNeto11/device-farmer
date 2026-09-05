package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// POST /api/v1/devices/{id}/park and /unpark — the operator surface for the
// deliberate hold that migration 00008 introduced.
//
// farm.device_park and farm.device_unpark have been the only supported way in
// and out of admin_state 'parked' since 00008, and until this file the only
// way to reach them was psql. That is the wrong constraint for the situation
// parking exists for — a shelf being rewired at 18:00, a handset going to the
// lab for an afternoon — because a person with a database shell and a
// deadline reaches for UPDATE farm.devices instead, and the guards in 00008
// then refuse them with a HINT naming a function they cannot call from where
// they are.
//
// # What this handler does not do
//
// It does not write farm.audit_log or farm.events itself. Both functions do,
// under the caller's name, in the same transaction as the state change; a
// second row from here would make every park look like two. What this layer
// supplies is the actor from the bearer token, the p_auto=false that marks
// the decision as a human's — automation may only ever reverse its own park,
// and a human's park keeps the last word — and the status codes a client can
// act on where the functions RAISE.
//
// It does not touch farm.leases, and neither can the functions: they run as
// farm_parker, which has no privilege on that table at all. A parked device
// with a live lease keeps it; parking stops the NEXT allocation, and the
// reply says so.

// parkView is the reply to both routes: the device, the park row as it now
// stands, and the live lease the hold is waiting on, if any.
type parkView struct {
	DeviceID   string  `json:"device_id"`
	FarmUID    string  `json:"farm_uid"`
	RackSlot   *string `json:"rack_slot,omitempty"`
	ParkID     int64   `json:"park_id"`
	Parked     bool    `json:"parked"`
	AdminState string  `json:"admin_state"`

	OpenedBy    string     `json:"opened_by"`
	OpenedAt    time.Time  `json:"opened_at"`
	Reason      string     `json:"reason"`
	ClosedBy    *string    `json:"closed_by,omitempty"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	CloseReason *string    `json:"close_reason,omitempty"`

	LiveLease *fleetLease `json:"live_lease,omitempty"`
	Note      string      `json:"note"`
}

// handleDevicePark serves POST /api/v1/devices/{id}/park.
func (s *Server) handleDevicePark(w http.ResponseWriter, r *http.Request) {
	s.setParked(w, r, true)
}

// handleDeviceUnpark serves POST /api/v1/devices/{id}/unpark.
func (s *Server) handleDeviceUnpark(w http.ResponseWriter, r *http.Request) {
	s.setParked(w, r, false)
}

func (s *Server) setParked(w http.ResponseWriter, r *http.Request, park bool) {
	action := "device.unpark"
	if park {
		action = "device.park"
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}
	var req revokeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if park && reason == "" {
		// The function refuses this too; saying it here keeps the refusal in
		// the words of the request rather than of a CHECK constraint.
		badRequest(w, "reason is required: a parked device with no reason is indistinguishable from a fault", nil)
		return
	}
	who := actor(r.Context())

	// Resolved first, by uuid or farm_uid, so the reply can name the rack
	// slot and the live lease — and so a device that does not exist is a 404
	// here rather than something to infer from the function's no_data_found,
	// which unpark also raises for "exists but is not parked".
	d, err := s.lookupDevice(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such device", map[string]string{"device": id})
			return
		}
		s.fail(w, r, action+": resolve device", err)
		return
	}

	var parkID int64
	if park {
		err = s.pool.QueryRow(r.Context(),
			`SELECT farm.device_park($1::uuid, $2::text, $3::text, false)`,
			d.DeviceID, who, reason).Scan(&parkID)
	} else {
		err = s.pool.QueryRow(r.Context(),
			`SELECT farm.device_unpark($1::uuid, $2::text, nullif($3::text, ''), false)`,
			d.DeviceID, who, reason).Scan(&parkID)
	}
	if err != nil {
		if s.writeParkRefusal(w, r, park, d, err) {
			s.metrics.operatorActions.WithLabelValues(action, "refused").Inc()
			return
		}
		s.fail(w, r, action, err)
		return
	}

	v := parkView{
		DeviceID:  d.DeviceID,
		FarmUID:   d.FarmUID,
		RackSlot:  d.RackSlot,
		ParkID:    parkID,
		Parked:    park,
		LiveLease: d.Lease,
	}
	if err := s.pool.QueryRow(r.Context(), `
SELECT p.opened_by, p.opened_at, p.reason, p.closed_by, p.closed_at, p.close_reason, d.admin_state
  FROM farm.device_parks p
  JOIN farm.devices d ON d.id = p.device_id
 WHERE p.id = $1`, parkID).
		Scan(&v.OpenedBy, &v.OpenedAt, &v.Reason, &v.ClosedBy, &v.ClosedAt, &v.CloseReason, &v.AdminState); err != nil {
		s.fail(w, r, action+": read back", err)
		return
	}

	// The function already wrote the audit row and the event under `who`;
	// only the counter and the log line are this layer's.
	s.metrics.operatorActions.WithLabelValues(action, "ok").Inc()
	s.log.InfoContext(r.Context(), "device park state changed by operator",
		"action", action, "device_id", d.DeviceID, "farm_uid", d.FarmUID, "park_id", parkID,
		"actor", who, "reason", reason, "live_lease", d.Lease != nil)

	switch {
	case park && d.Lease != nil:
		v.Note = "parked. The live lease on this device is untouched: that run ends on its own " +
			"terms, and no new lease will be placed here afterwards until the device is unparked."
	case park:
		v.Note = "parked. No lease will be placed on this device until it is unparked; the " +
			"watchdog keeps observing it, and the recovery ladder leaves it alone."
	default:
		v.Note = "unparked. Health is 'unknown' until the watchdog looks again — a device nobody " +
			"has observed since it was shelved is not 'healthy' on anyone's say-so."
	}
	writeJSON(w, http.StatusOK, v)
}

// writeParkRefusal turns the functions' RAISEs into the codes a client acts
// on, and reports whether it did. Anything it does not recognise is left to
// the caller's generic classification.
//
// The SQLSTATEs are the ones farm.device_park and farm.device_unpark choose
// deliberately (00008): unique_violation for a double park, no_data_found for
// a park that is not there, check_violation for a device whose state is
// already somebody else's decision, insufficient_privilege for automation
// trying to reverse a human. The last cannot happen from here — p_auto is
// always false — and falls through to the 403 classifyPgError already gives
// it, so a future caller that passes true gets an honest answer.
func (s *Server) writeParkRefusal(w http.ResponseWriter, r *http.Request, park bool, d fleetDevice, err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	subject := map[string]any{"device_id": d.DeviceID, "farm_uid": d.FarmUID, "admin_state": d.AdminState}

	switch {
	case park && pgErr.Code == "23505":
		// Name the park that already holds it: the operator's next question is
		// "who, and why", and the answer is one read away.
		var (
			parkID   int64
			openedBy string
			openedAt time.Time
			why      string
			auto     bool
		)
		if readErr := s.pool.QueryRow(r.Context(), `
SELECT id, opened_by, opened_at, reason, auto
  FROM farm.device_parks WHERE device_id = $1::uuid AND closed_at IS NULL`, d.DeviceID).
			Scan(&parkID, &openedBy, &openedAt, &why, &auto); readErr == nil {
			subject["park_id"] = parkID
			subject["opened_by"] = openedBy
			subject["opened_at"] = openedAt
			subject["reason"] = why
			subject["auto"] = auto
		}
		writeError(w, http.StatusConflict, CodeConflict,
			"this device is already parked; unpark it first if the hold is over", subject)
		return true

	case park && pgErr.Code == "23514" && strings.Contains(pgErr.Message, "not enabled"):
		// Parking a quarantined, disabled or retired device would overwrite
		// the reason it is out of service with a weaker one, and unparking
		// would then hand it back as 'enabled'. A state conflict, not a
		// malformed request.
		writeError(w, http.StatusConflict, CodeConflict,
			"only an enabled device can be parked: this one is "+d.AdminState+
				", which is already somebody's decision about it", subject)
		return true

	case pgErr.Code == "23514":
		writeError(w, http.StatusBadRequest, CodeBadRequest, pgErr.Message, subject)
		return true

	case pgErr.Code == "P0002":
		if park {
			// The device was there a moment ago and is not now.
			writeError(w, http.StatusNotFound, CodeNotFound, "no such device", subject)
			return true
		}
		writeError(w, http.StatusConflict, CodeConflict,
			"this device is not parked, so there is nothing to reverse; its admin_state is "+d.AdminState,
			subject)
		return true
	}
	return false
}
