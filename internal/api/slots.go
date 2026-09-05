package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/flaviopadilha/device-farmer/internal/enroll"
)

// The operator surface for slots: what is registered where, and the three
// corrections a human makes by hand — a slot's label, a device's slot, and the
// brand on a phone.
//
// The slot is the primary physical object in this schema, and until this file
// it could be read but not written from the API. Registering one went through
// topology discovery or psql; moving a device went through psql or waited for
// a host to notice; rebranding went through nothing at all, so the brand
// conflict enroll refuses to resolve ("a human must retire it") had no command
// for the human to run.
//
// Every write here is refused while the device it concerns holds a live
// lease, and none of them can end one. A re-slot changes the address every
// recovery action for that device will use; a rebrand writes to the phone. A
// job three hours into its run gets neither done to it, however sure the
// operator is, and the refusal names the lease so the operator can decide
// whether to revoke it — which is audited, and is a different endpoint.

// usbPathRe is farm.slots.usb_path's CHECK constraint, restated so a
// malformed path is a 400 naming the field rather than a 23514 naming a
// constraint.
var usbPathRe = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)

// slotView is one row of GET /api/v1/slots.
//
// It carries the occupant's identity and nothing about its lease. The listing
// is tenant-readable so a client can turn a rack label into a devpath; who
// holds what is the lease listing's business and is scoped there.
type slotView struct {
	SlotID        int64     `json:"slot_id"`
	HostID        string    `json:"host_id"`
	HubID         int64     `json:"hub_id"`
	HubPath       string    `json:"hub_path"`
	PortNumber    int       `json:"port_number"`
	USBPath       string    `json:"usb_path"`
	ADBDevpath    string    `json:"adb_devpath"`
	RackSlot      *string   `json:"rack_slot,omitempty"`
	State         string    `json:"state"`
	RearmAt       time.Time `json:"rearm_at"`
	PowerDomainID *int64    `json:"power_domain_id,omitempty"`
	PowerKind     *string   `json:"power_kind,omitempty"`
	PowerControl  *string   `json:"power_control,omitempty"`
	DeviceID      *string   `json:"device_id,omitempty"`
	FarmUID       *string   `json:"farm_uid,omitempty"`
}

const slotColumns = `
  s.id, s.host_id, s.hub_id, hb.usb_path, s.port_number, s.usb_path, s.adb_devpath, s.rack_slot,
  s.state, s.rearm_at, s.power_domain_id, pd.kind, pd.control, d.id::text, d.farm_uid`

const slotFrom = `
  FROM farm.slots s
  JOIN farm.hubs hb ON hb.id = s.hub_id
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
  LEFT JOIN farm.devices d ON d.current_slot_id = s.id`

func scanSlot(sc scanner) (slotView, error) {
	var v slotView
	err := sc.Scan(&v.SlotID, &v.HostID, &v.HubID, &v.HubPath, &v.PortNumber, &v.USBPath,
		&v.ADBDevpath, &v.RackSlot, &v.State, &v.RearmAt, &v.PowerDomainID, &v.PowerKind,
		&v.PowerControl, &v.DeviceID, &v.FarmUID)
	return v, err
}

// readSlot reads one slot by id. The error is pgx.ErrNoRows when there is none.
func (s *Server) readSlot(ctx context.Context, id int64) (slotView, error) {
	return scanSlot(s.pool.QueryRow(ctx, "SELECT"+slotColumns+slotFrom+" WHERE s.id = $1", id))
}

// slotRefusal recognises the SQLSTATEs farm.reslot_device and farm.relabel_slot
// raise when they decline: object_in_use for a live lease or an occupant,
// object_not_in_prerequisite_state for a slot that is not active or is on
// another host. Neither is a server fault, so neither may become a 500.
func slotRefusal(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55000", "55006":
			return pgErr.Message, true
		}
	}
	return "", false
}

// auditRefusal records an operator action the server declined. A refusal is an
// answer, and "we declined to do that, and here is why" must be as findable
// afterwards as the actions that went through.
func (s *Server) auditRefusal(ctx context.Context, who, action, subject, reason, refusal string, detail map[string]any) {
	bookCtx, bookCancel := detachedCtx(ctx)
	s.auditAction(bookCtx, who, action, subject, reason,
		mergeDetail(detail, map[string]any{"outcome": "refused", "refusal": refusal}))
	bookCancel()
	s.metrics.operatorActions.WithLabelValues(action, "refused").Inc()
}

// ---------------------------------------------------------------------------
// GET /api/v1/slots
// ---------------------------------------------------------------------------

// handleSlotList serves GET /api/v1/slots: every registered position, occupied
// or not, with its power domain and the device sitting in it.
func (s *Server) handleSlotList(w http.ResponseWriter, r *http.Request) {
	var (
		host  = queryString(r, "host")
		hub   = queryString(r, "hub")
		limit = queryInt(r, "limit", 5000, 1, 20000)
	)

	var (
		conds []string
		args  []any
	)
	if host != "" {
		args = append(args, host)
		conds = append(conds, fmt.Sprintf("s.host_id = $%d", len(args)))
	}
	if hub != "" {
		// Either spelling, as the fleet listing accepts: the grid groups by
		// path while the topology view links by id.
		args = append(args, hub)
		conds = append(conds, fmt.Sprintf("(hb.usb_path = $%d OR hb.id::text = $%d)", len(args), len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	query := fmt.Sprintf("SELECT%s%s%s ORDER BY s.host_id, hb.usb_path, s.port_number LIMIT $%d",
		slotColumns, slotFrom, where, len(args))

	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.fail(w, r, "list slots", err)
		return
	}
	defer rows.Close()

	slots := make([]slotView, 0, 64)
	occupied := 0
	for rows.Next() {
		v, err := scanSlot(rows)
		if err != nil {
			s.fail(w, r, "scan slot", err)
			return
		}
		if v.DeviceID != nil {
			occupied++
		}
		slots = append(slots, v)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read slots", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"slots":     slots,
		"total":     len(slots),
		"occupied":  occupied,
		"truncated": len(slots) == limit,
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/slots
// ---------------------------------------------------------------------------

type slotRegisterRequest struct {
	HostID  string `json:"host_id"`
	USBPath string `json:"usb_path"`
	HubPath string `json:"hub_path"`
	Port    int    `json:"port"`
	// HubModel, Ports and Switchable describe the hub the slot hangs from and
	// are only consulted when the hub is new to the schema — except
	// Switchable, which farm.register_slot always applies, because it decides
	// whether the hub gets a per-port or a ganged power domain.
	HubModel   string `json:"hub_model,omitempty"`
	Ports      int    `json:"ports,omitempty"`
	Switchable bool   `json:"switchable,omitempty"`
	// RackSlot is the human label. Empty means "not stated": an existing
	// label is kept, never erased. relabel is the way to clear one.
	RackSlot string `json:"rack_slot,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// handleSlotRegister serves POST /api/v1/slots.
//
// It is the same call topology discovery makes for every port it finds, taken
// one port at a time for the rack that has no host agent yet, or the hub the
// agent's filter skipped. farm.register_slot is idempotent on (host, usb_path):
// a position already registered answers 200 with the row as it now stands, a
// new one answers 201.
func (s *Server) handleSlotRegister(w http.ResponseWriter, r *http.Request) {
	var req slotRegisterRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	req.HostID = strings.TrimSpace(req.HostID)
	req.USBPath = strings.TrimSpace(req.USBPath)
	req.HubPath = strings.TrimSpace(req.HubPath)
	req.HubModel = strings.TrimSpace(req.HubModel)
	req.RackSlot = strings.TrimSpace(req.RackSlot)
	reason := strings.TrimSpace(req.Reason)
	who := actor(r.Context())

	switch {
	case req.HostID == "":
		badRequest(w, "host_id is required", nil)
		return
	case !usbPathRe.MatchString(req.USBPath):
		badRequest(w, "usb_path must look like 3-1.4 (bus-port, then one port per hub level)",
			map[string]string{"usb_path": req.USBPath})
		return
	case !usbPathRe.MatchString(req.HubPath):
		badRequest(w, "hub_path must look like 3-1 (the hub's own USB position)",
			map[string]string{"hub_path": req.HubPath})
		return
	case req.Port < 1 || req.Port > 32:
		badRequest(w, "port must be between 1 and 32", map[string]int{"port": req.Port})
		return
	case req.USBPath != req.HubPath+"."+strconv.Itoa(req.Port):
		// The kernel names a device by its hub's path plus the port it is on.
		// A slot that disagrees with that is a slot no host will ever report
		// a device in, and a recovery action aimed at it lands nowhere.
		badRequest(w, "usb_path must be hub_path followed by \".\" and the port: "+
			req.HubPath+"."+strconv.Itoa(req.Port),
			map[string]any{"usb_path": req.USBPath, "hub_path": req.HubPath, "port": req.Port})
		return
	case req.Ports != 0 && (req.Ports < 1 || req.Ports > 32):
		badRequest(w, "ports must be between 1 and 32", map[string]int{"ports": req.Ports})
		return
	}
	ports := req.Ports
	if ports == 0 {
		ports = 7
	}

	var hostExists bool
	if err := s.pool.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM farm.hosts WHERE id = $1)`, req.HostID).Scan(&hostExists); err != nil {
		s.fail(w, r, "register slot: read host", err)
		return
	}
	if !hostExists {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such host",
			map[string]string{"host_id": req.HostID})
		return
	}

	// Existence is read and the upsert made in one transaction, so "created"
	// in the response describes this call and not a concurrent one.
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.fail(w, r, "register slot: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.Context())) }()

	var existing *int64
	if err := tx.QueryRow(r.Context(),
		`SELECT id FROM farm.slots WHERE host_id = $1 AND usb_path = $2 FOR UPDATE`,
		req.HostID, req.USBPath).Scan(&existing); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, "register slot: read slot", err)
		return
	}

	var slotID int64
	if err := tx.QueryRow(r.Context(),
		`SELECT farm.register_slot($1, $2, $3, $4, nullif($5, ''), $6, $7, nullif($8, ''))`,
		req.HostID, req.USBPath, req.HubPath, req.Port, req.HubModel, ports, req.Switchable,
		req.RackSlot).Scan(&slotID); err != nil {
		s.fail(w, r, "register slot", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, r, "register slot: commit", err)
		return
	}

	slot, err := s.readSlot(r.Context(), slotID)
	if err != nil {
		s.fail(w, r, "register slot: read back", err)
		return
	}

	created := existing == nil
	detail := map[string]any{
		"slot_id":     slotID,
		"host_id":     req.HostID,
		"usb_path":    req.USBPath,
		"adb_devpath": slot.ADBDevpath,
		"hub_path":    req.HubPath,
		"port":        req.Port,
		"hub_model":   req.HubModel,
		"ports":       ports,
		"switchable":  req.Switchable,
		"rack_slot":   slot.RackSlot,
		"power_kind":  slot.PowerKind,
		"created":     created,
	}
	// The row is committed; its audit trail must not depend on the operator
	// still being connected.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, "slot.register", fmt.Sprintf("slot:%d", slotID), reason, detail)
	s.recordEvent(bookCtx, eventRow{Kind: "slot_registered", SlotID: &slotID, Actor: who, Detail: detail})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues("slot.register", "ok").Inc()
	s.log.InfoContext(r.Context(), "slot registered",
		"slot_id", slotID, "host_id", req.HostID, "usb_path", req.USBPath, "created", created, "actor", who)

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"slot":    slot,
		"created": created,
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/slots/{id}/label
// ---------------------------------------------------------------------------

type slotLabelRequest struct {
	// RackSlot is the new label. Empty clears the label; this is the one
	// place an empty label means "remove it" rather than "not stated".
	RackSlot string `json:"rack_slot"`
	Reason   string `json:"reason"`
}

// handleSlotLabel serves POST /api/v1/slots/{id}/label.
//
// A label is what an alert prints and what an operator walks to, so changing
// one is audited with the label it replaced: the next person reading a
// month-old alert needs to know that "R1-U14-H2-P3" then is "R1-U14-H2-P7" now.
func (s *Server) handleSlotLabel(w http.ResponseWriter, r *http.Request) {
	slotID, ok := pathInt64(r, "id")
	if !ok {
		badRequest(w, "slot id must be an integer", nil)
		return
	}
	var req slotLabelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required", nil)
		return
	}
	who := actor(r.Context())

	before, err := s.readSlot(r.Context(), slotID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such slot", nil)
			return
		}
		s.fail(w, r, "relabel slot: read slot", err)
		return
	}

	// farm.relabel_slot writes the audit row and the event in the same
	// transaction as the label, so only a refusal is recorded from here.
	if _, err := s.pool.Exec(r.Context(),
		`SELECT farm.relabel_slot($1, $2, $3, $4)`, slotID, req.RackSlot, who, reason); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Two sockets under one label sends an operator confidently to the
			// wrong one; the function refuses it and names the other slot.
			s.auditRefusal(r.Context(), who, "slot.relabel", fmt.Sprintf("slot:%d", slotID), reason,
				pgErr.Message, map[string]any{"slot_id": slotID, "rack_slot": req.RackSlot})
			writeError(w, http.StatusConflict, CodeConflict, pgErr.Message,
				map[string]any{"slot_id": slotID, "rack_slot": strings.TrimSpace(req.RackSlot)})
			return
		}
		s.fail(w, r, "relabel slot", err)
		return
	}
	s.metrics.operatorActions.WithLabelValues("slot.relabel", "ok").Inc()

	after, err := s.readSlot(r.Context(), slotID)
	if err != nil {
		s.fail(w, r, "relabel slot: read back", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"slot":               after,
		"previous_rack_slot": before.RackSlot,
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/devices/{id}/reslot
// ---------------------------------------------------------------------------

type deviceReslotRequest struct {
	// SlotID is the destination. It is a pointer so that an omitted field and
	// a zero can be told apart; zero is not a slot id.
	SlotID *int64 `json:"slot_id,omitempty"`
	// Unslot takes the device out of its slot without putting it anywhere.
	// It exists for a row that resolution keeps placing in a socket the
	// operator has decided belongs to a different row: the occupant cannot
	// be overwritten, so it has to be moved out first.
	Unslot bool   `json:"unslot,omitempty"`
	Reason string `json:"reason"`
}

// handleDeviceReslot serves POST /api/v1/devices/{id}/reslot.
//
// This changes the physical address every recovery action, power cycle and
// exec for the device will use from now on, which is why it is refused while
// the device holds a live lease: a job three hours in must not have its
// device's address changed under it. The refusal names the lease.
//
// The checks are made here for the quality of the answer — a 409 that names
// the occupying device, or the lease and its holder — and made again inside
// farm.reslot_device under a row lock, which is the check that actually holds.
func (s *Server) handleDeviceReslot(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}
	var req deviceReslotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required", nil)
		return
	}
	if (req.SlotID == nil) == !req.Unslot {
		badRequest(w, "give exactly one of slot_id (the destination) or unslot: true", nil)
		return
	}
	who := actor(r.Context())

	d, err := s.lookupDevice(r.Context(), id)
	if err != nil {
		s.fail(w, r, "reslot: resolve device", err)
		return
	}

	var target *slotView
	if req.SlotID != nil {
		t, err := s.readSlot(r.Context(), *req.SlotID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, CodeNotFound, "no such slot",
					map[string]int64{"slot_id": *req.SlotID})
				return
			}
			s.fail(w, r, "reslot: read slot", err)
			return
		}
		target = &t
	}

	subject := "device:" + d.DeviceID
	detail := map[string]any{
		"device_id":      d.DeviceID,
		"farm_uid":       d.FarmUID,
		"host_id":        d.HostID,
		"from_slot_id":   d.SlotID,
		"from_rack_slot": d.RackSlot,
	}
	if target != nil {
		detail["to_slot_id"] = target.SlotID
		detail["to_rack_slot"] = target.RackSlot
		detail["to_usb_path"] = target.USBPath
		detail["to_host_id"] = target.HostID
	} else {
		detail["to_slot_id"] = nil
	}

	refuse := func(message string, extra map[string]any) {
		s.auditRefusal(r.Context(), who, "device.reslot", subject, reason, message, mergeDetail(detail, extra))
		writeError(w, http.StatusConflict, CodeConflict, message, mergeDetail(detail, extra))
	}

	switch {
	case d.Lease != nil:
		// THE REFUSAL THIS ENDPOINT EXISTS FOR.
		refuse("this device holds a live lease; its slot cannot change while a job is using it. "+
			"Nothing here ends a lease: wait for it, or revoke it (audited) and try again.",
			map[string]any{
				"lease_id":  d.Lease.ID,
				"job_id":    d.Lease.JobID,
				"tenant_id": d.Lease.TenantID,
				"holder":    d.Lease.Holder,
				"protected": d.Lease.Protected,
			})
		return
	case target != nil && target.State != "active":
		refuse(fmt.Sprintf("slot %d is %s, not active", target.SlotID, target.State), nil)
		return
	case target != nil && d.HostID != nil && *d.HostID != target.HostID:
		refuse(fmt.Sprintf("slot %d is on host %s, but this device belongs to host %s; "+
			"a device row never changes host by hand — the host that sees the phone adopts it",
			target.SlotID, target.HostID, *d.HostID), nil)
		return
	case target != nil && target.DeviceID != nil && *target.DeviceID != d.DeviceID:
		refuse(fmt.Sprintf("slot %d is occupied by device %s", target.SlotID, *target.DeviceID),
			map[string]any{"occupant_device_id": *target.DeviceID, "occupant_farm_uid": target.FarmUID})
		return
	}

	// farm.reslot_device re-checks everything above under a row lock and
	// writes the audit row and the event in the same transaction as the move.
	var slotArg *int64
	if target != nil {
		slotArg = &target.SlotID
	}
	if _, err := s.pool.Exec(r.Context(),
		`SELECT farm.reslot_device($1::uuid, $2, $3, $4)`, d.DeviceID, slotArg, who, reason); err != nil {
		if msg, refused := slotRefusal(err); refused {
			refuse(msg, nil)
			return
		}
		s.fail(w, r, "reslot device", err)
		return
	}
	s.metrics.operatorActions.WithLabelValues("device.reslot", "ok").Inc()

	after, err := s.lookupDevice(r.Context(), d.DeviceID)
	if err != nil {
		s.fail(w, r, "reslot: read back", err)
		return
	}
	moved := (d.SlotID == nil) != (after.SlotID == nil) ||
		(d.SlotID != nil && after.SlotID != nil && *d.SlotID != *after.SlotID)
	s.log.WarnContext(r.Context(), "device re-slotted by hand",
		"device_id", d.DeviceID, "from_slot_id", d.SlotID, "to_slot_id", after.SlotID,
		"actor", who, "reason", reason)

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":    after.DeviceID,
		"farm_uid":     after.FarmUID,
		"host_id":      after.HostID,
		"from_slot_id": d.SlotID,
		"to_slot_id":   after.SlotID,
		"rack_slot":    after.RackSlot,
		"adb_devpath":  after.ADBDevpath,
		"moved":        moved,
		"note": "every recovery action for this device now addresses its new position. " +
			"No lease was touched.",
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/devices/{id}/rebrand
// ---------------------------------------------------------------------------

type deviceRebrandRequest struct {
	Reason string `json:"reason"`
	// PreviousUID, when given, is the brand the operator expects to find on
	// the phone. A phone carrying anything else is refused untouched: the
	// authorisation was for a particular phone, and the brand is how a phone
	// says which one it is.
	PreviousUID string `json:"previous_uid,omitempty"`
}

// handleDeviceRebrand serves POST /api/v1/devices/{id}/rebrand.
//
// The brand IS farm.devices.farm_uid: it is minted by farm.resolve_device when
// the row is adopted and written to the phone by the enrolment loop, so that
// the strongest rung of the identity ladder holds the next time the phone is
// seen. Rebranding writes this row's uid onto the phone in this row's slot.
// The row does not change; the phone does.
//
// It exists for the case enroll.Brander refuses on its own — the phone carries
// a different uid, and overwriting it would fuse two rows' histories — after a
// human has established that THIS row is the phone. The previous uid is
// reported back, with the row it names if any, because that row now points at
// no phone and somebody has to retire it.
//
// A phone in a slot is addressed by devpath, never by serial, and never while
// its device holds a live lease: writing to a phone in the middle of somebody's
// run is a write into their run.
func (s *Server) handleDeviceRebrand(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}
	var req deviceRebrandRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required: overwriting a brand abandons an identity", nil)
		return
	}
	req.PreviousUID = strings.TrimSpace(req.PreviousUID)
	if req.PreviousUID != "" && !enroll.IsFarmUID(req.PreviousUID) {
		badRequest(w, "previous_uid is not a farm uid", map[string]string{"previous_uid": req.PreviousUID})
		return
	}
	who := actor(r.Context())

	d, err := s.lookupDevice(r.Context(), id)
	if err != nil {
		s.fail(w, r, "rebrand: resolve device", err)
		return
	}

	subject := "device:" + d.DeviceID
	detail := map[string]any{
		"device_id": d.DeviceID,
		"farm_uid":  d.FarmUID,
		"host_id":   d.HostID,
		"slot_id":   d.SlotID,
		"devpath":   d.ADBDevpath,
	}
	refuse := func(message string, extra map[string]any) {
		s.auditRefusal(r.Context(), who, "device.rebrand", subject, reason, message, mergeDetail(detail, extra))
		writeError(w, http.StatusConflict, CodeConflict, message, mergeDetail(detail, extra))
	}

	switch {
	case d.Lease != nil:
		refuse("this device holds a live lease; nothing is written to a phone in the middle of "+
			"somebody's run. Nothing here ends a lease: wait for it, or revoke it (audited).",
			map[string]any{
				"lease_id":  d.Lease.ID,
				"job_id":    d.Lease.JobID,
				"tenant_id": d.Lease.TenantID,
				"holder":    d.Lease.Holder,
				"protected": d.Lease.Protected,
			})
		return
	case d.AdminState == "retired":
		// Writing a retired row's uid onto a phone would bring the row back
		// into the identity ladder with a history somebody chose to close.
		refuse("this device is retired; a retired identity is not written back onto a phone", nil)
		return
	case d.ADBDevpath == nil || *d.ADBDevpath == "":
		refuse("this device is not in a slot, so there is no physical position to write to; "+
			"re-slot it first", nil)
		return
	case d.ADBEndpoint == nil || *d.ADBEndpoint == "":
		refuse("this device's host has no ADB endpoint recorded", nil)
		return
	}
	devpath := *d.ADBDevpath

	// One shell round trip per step, three steps at most (read, write, read
	// back), each on its own deadline inside the brander; the outer bound is
	// there so a phone that answers nothing cannot hold the request forever.
	ctx, cancel := context.WithTimeout(r.Context(), 4*defaultExecTimeout)
	defer cancel()
	brander := enroll.NewBrander(s.newExecutor(*d.ADBEndpoint, defaultExecTimeout, s.execMaxOutput),
		defaultExecTimeout, s.log)

	// What the phone says it is, before anything is decided. A brand that
	// cannot be read is not overwritten: "unreadable" and "absent" are the
	// difference between a fresh phone and a phone with a story.
	have, err := brander.Read(ctx, devpath)
	if err != nil {
		s.auditRefusal(r.Context(), who, "device.rebrand", subject, reason, err.Error(), detail)
		writeError(w, http.StatusBadGateway, CodeADBError,
			"the brand on this device could not be read, so nothing was written: "+err.Error()+
				". No lease was affected.", mergeDetail(detail, map[string]any{"endpoint": *d.ADBEndpoint}))
		return
	}
	detail["previous_uid"] = have
	if req.PreviousUID != "" && have != req.PreviousUID {
		refuse(fmt.Sprintf("the device carries %q, not the %q this rebrand was authorised against; "+
			"nothing was written — establish which phone this is before trying again",
			have, req.PreviousUID), nil)
		return
	}

	// The row the abandoned brand belongs to, if the fleet has one. It is the
	// row that now points at no phone, and it goes in the answer by name.
	var previousDeviceID *string
	if have != "" && have != d.FarmUID {
		if err := s.pool.QueryRow(ctx,
			`SELECT id::text FROM farm.devices WHERE farm_uid = $1`, have).Scan(&previousDeviceID); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			s.fail(w, r, "rebrand: resolve previous uid", err)
			return
		}
		detail["previous_device_id"] = previousDeviceID
	}

	var (
		outcome  enroll.BrandOutcome
		brandErr error
	)
	switch {
	case have == d.FarmUID:
		outcome = enroll.BrandAlready
	case have == "":
		// Nothing is abandoned by branding an unbranded phone, so this is the
		// ordinary write, not a rebrand: it refuses a conflict on the phone
		// rather than overriding one.
		outcome, brandErr = brander.Brand(ctx, devpath, d.FarmUID)
	default:
		outcome, brandErr = brander.Rebrand(ctx, devpath, d.FarmUID, have, reason)
	}
	detail["outcome"] = string(outcome)

	if brandErr != nil {
		var conflict *enroll.ConflictError
		if errors.As(brandErr, &conflict) {
			refuse(brandErr.Error(), map[string]any{"on_device_uid": conflict.Have})
			return
		}
		detail["error"] = brandErr.Error()
		bookCtx, bookCancel := detachedCtx(r.Context())
		s.auditAction(bookCtx, who, "device.rebrand", subject, reason, detail)
		bookCancel()
		s.metrics.operatorActions.WithLabelValues("device.rebrand", "error").Inc()
		s.log.WarnContext(r.Context(), "rebrand failed",
			"device_id", d.DeviceID, "devpath", devpath, "err", brandErr)
		writeError(w, http.StatusBadGateway, CodeADBError,
			"the rebrand did not complete against this device: "+brandErr.Error()+
				". Read "+enroll.BrandPath+" on the phone to see what it holds now. No lease was affected.",
			mergeDetail(detail, map[string]any{"endpoint": *d.ADBEndpoint}))
		return
	}

	// The phone is already written; the record of who abandoned which identity
	// must not depend on the operator's connection outliving the write.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, "device.rebrand", subject, reason, detail)
	s.recordEvent(bookCtx, eventRow{
		Kind: "device_rebranded", DeviceID: &d.DeviceID, SlotID: d.SlotID, Actor: who, Detail: detail,
	})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues("device.rebrand", "ok").Inc()
	if outcome == enroll.BrandWritten && have != "" {
		s.log.WarnContext(r.Context(), "device rebranded by hand: a previous identity was abandoned",
			"device_id", d.DeviceID, "devpath", devpath, "previous_uid", have, "new_uid", d.FarmUID,
			"previous_device_id", previousDeviceID, "actor", who, "reason", reason)
	}

	note := "the phone now carries this row's uid and will resolve to it on its next sighting. No lease was affected."
	switch {
	case outcome == enroll.BrandAlready:
		note = "the phone already carried this row's uid; nothing was written. No lease was affected."
	case previousDeviceID != nil:
		note += " Device " + *previousDeviceID + " carried the abandoned uid and now names no phone; retire it."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":          d.DeviceID,
		"farm_uid":           d.FarmUID,
		"host_id":            d.HostID,
		"adb_devpath":        devpath,
		"previous_uid":       have,
		"previous_device_id": previousDeviceID,
		"outcome":            string(outcome),
		"note":               note,
	})
}
