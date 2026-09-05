package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// scanner is the shared shape of pgx.Row and pgx.Rows, so one scan function
// serves both the single-device and the list query.
type scanner interface {
	Scan(dest ...any) error
}

// fleetColumns is farm.v_fleet's projection, in the order fleetDevice scans it.
//
// The view already joins identity, physical position, health and allocation, so
// the fleet grid is one query and the client never stitches three lists
// together and gets a device's slot wrong.
const fleetColumns = `
  f.device_id::text, f.farm_uid, f.adb_serial, f.serial_ambiguous, f.model, f.manufacturer,
  f.android_release, f.sdk_int, f.pool_id, f.admin_state, f.labels, f.failure_score::float8,
  f.fence_floor, f.slot_id, f.rack_slot, f.usb_path, f.adb_devpath, f.slot_state, f.rearm_at,
  f.hub_id, f.hub_path, f.vbus_switchable, f.host_id, f.adb_endpoint, f.host_admin_state,
  f.adb_state, f.health, f.health_since, f.battery_pct::int, f.battery_temp_dc::int,
  f.consec_bad, f.ladder_tier, f.last_seen_at,
  f.lease_id::text, f.fence, f.lease_state, f.protected, f.job_id::text, f.tenant_id, f.holder,
  f.acquired_at, f.expires_at, f.reclaimable_at, f.quarantine_id, f.quarantine_reason`

// fleetDevice is one row of farm.v_fleet.
//
// Health and allocation are separate objects in the JSON for the same reason
// they are separate tables in the schema: a client that can accidentally read
// "device is offline" as "lease is over" is a client that will eventually be
// written to act on it.
type fleetDevice struct {
	DeviceID        string          `json:"device_id"`
	FarmUID         string          `json:"farm_uid"`
	ADBSerial       *string         `json:"adb_serial,omitempty"`
	SerialAmbiguous bool            `json:"serial_ambiguous"`
	Model           *string         `json:"model,omitempty"`
	Manufacturer    *string         `json:"manufacturer,omitempty"`
	AndroidRelease  *string         `json:"android_release,omitempty"`
	SDKInt          *int            `json:"sdk_int,omitempty"`
	Pool            string          `json:"pool"`
	AdminState      string          `json:"admin_state"`
	Labels          json.RawMessage `json:"labels,omitempty"`
	FailureScore    float64         `json:"failure_score"`
	FenceFloor      int64           `json:"fence_floor"`

	SlotID     *int64     `json:"slot_id,omitempty"`
	RackSlot   *string    `json:"rack_slot,omitempty"`
	USBPath    *string    `json:"usb_path,omitempty"`
	ADBDevpath *string    `json:"adb_devpath,omitempty"`
	SlotState  *string    `json:"slot_state,omitempty"`
	RearmAt    *time.Time `json:"rearm_at,omitempty"`

	HubID           *int64     `json:"hub_id,omitempty"`
	HubPath         *string    `json:"hub_path,omitempty"`
	VbusSwitchable  *bool      `json:"vbus_switchable,omitempty"`
	HostID          *string    `json:"host_id,omitempty"`
	ADBEndpoint     *string    `json:"adb_endpoint,omitempty"`
	HostAdminState  *string    `json:"host_admin_state,omitempty"`
	ADBState        *string    `json:"adb_state,omitempty"`
	Health          *string    `json:"health,omitempty"`
	HealthSince     *time.Time `json:"health_since,omitempty"`
	BatteryPct      *int       `json:"battery_pct,omitempty"`
	BatteryTempDeci *int       `json:"battery_temp_dc,omitempty"`
	ConsecBad       *int       `json:"consec_bad,omitempty"`
	LadderTier      *int       `json:"ladder_tier,omitempty"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`

	// Lease is nil when the device is free. It is the denormalised live lease
	// farm.devices.current_lease_id points at, so "free" here means "no live
	// lease", never "unreachable".
	Lease *fleetLease `json:"lease,omitempty"`

	QuarantineID     *int64  `json:"quarantine_id,omitempty"`
	QuarantineReason *string `json:"quarantine_reason,omitempty"`
}

type fleetLease struct {
	ID            string    `json:"id"`
	Fence         int64     `json:"fence"`
	State         string    `json:"state"`
	Protected     bool      `json:"protected"`
	JobID         string    `json:"job_id"`
	TenantID      string    `json:"tenant_id"`
	Holder        string    `json:"holder"`
	AcquiredAt    time.Time `json:"acquired_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	ReclaimableAt time.Time `json:"reclaimable_at"`
}

func scanFleetDevice(sc scanner) (fleetDevice, error) {
	var d fleetDevice
	var labels []byte
	var (
		leaseID       *string
		fence         *int64
		leaseState    *string
		protected     *bool
		jobID         *string
		tenantID      *string
		holder        *string
		acquiredAt    *time.Time
		expiresAt     *time.Time
		reclaimableAt *time.Time
	)

	err := sc.Scan(
		&d.DeviceID, &d.FarmUID, &d.ADBSerial, &d.SerialAmbiguous, &d.Model, &d.Manufacturer,
		&d.AndroidRelease, &d.SDKInt, &d.Pool, &d.AdminState, &labels, &d.FailureScore,
		&d.FenceFloor, &d.SlotID, &d.RackSlot, &d.USBPath, &d.ADBDevpath, &d.SlotState, &d.RearmAt,
		&d.HubID, &d.HubPath, &d.VbusSwitchable, &d.HostID, &d.ADBEndpoint, &d.HostAdminState,
		&d.ADBState, &d.Health, &d.HealthSince, &d.BatteryPct, &d.BatteryTempDeci,
		&d.ConsecBad, &d.LadderTier, &d.LastSeenAt,
		&leaseID, &fence, &leaseState, &protected, &jobID, &tenantID, &holder,
		&acquiredAt, &expiresAt, &reclaimableAt, &d.QuarantineID, &d.QuarantineReason,
	)
	if err != nil {
		return fleetDevice{}, err
	}
	if len(labels) > 0 {
		d.Labels = json.RawMessage(labels)
	}
	if leaseID != nil {
		d.Lease = &fleetLease{ID: *leaseID}
		if fence != nil {
			d.Lease.Fence = *fence
		}
		if leaseState != nil {
			d.Lease.State = *leaseState
		}
		if protected != nil {
			d.Lease.Protected = *protected
		}
		if jobID != nil {
			d.Lease.JobID = *jobID
		}
		if tenantID != nil {
			d.Lease.TenantID = *tenantID
		}
		if holder != nil {
			d.Lease.Holder = *holder
		}
		if acquiredAt != nil {
			d.Lease.AcquiredAt = *acquiredAt
		}
		if expiresAt != nil {
			d.Lease.ExpiresAt = *expiresAt
		}
		if reclaimableAt != nil {
			d.Lease.ReclaimableAt = *reclaimableAt
		}
	}
	return d, nil
}

// hubHealth is one row of farm.v_hub_health.
//
// It is folded into the fleet response so the dashboard can render one hub
// banner instead of six device alerts. Devices that fail together almost always
// share a hub or a power domain; presenting them as unrelated incidents is how
// an operator spends an hour rebooting phones that were never the problem.
type hubHealth struct {
	HubID          int64      `json:"hub_id"`
	HostID         string     `json:"host_id"`
	USBPath        string     `json:"usb_path"`
	Model          *string    `json:"model,omitempty"`
	VbusSwitchable bool       `json:"vbus_switchable"`
	Devices        int        `json:"devices"`
	Healthy        int        `json:"healthy"`
	Unhealthy      int        `json:"unhealthy"`
	WorstSince     *time.Time `json:"worst_since,omitempty"`
	// Correlated marks a hub where more than one device is unhealthy: the
	// signature of a hub, cable or power-domain fault rather than N phone
	// faults.
	Correlated bool `json:"correlated"`
}

type fleetCounts struct {
	Total       int            `json:"total"`
	Health      map[string]int `json:"health"`
	Host        map[string]int `json:"host"`
	LeaseState  map[string]int `json:"lease_state"`
	Leased      int            `json:"leased"`
	Free        int            `json:"free"`
	Unhealthy   int            `json:"unhealthy"`
	Quarantined int            `json:"quarantined"`
	Protected   int            `json:"protected"`
}

// handleFleet serves GET /api/v1/fleet.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	var (
		host   = queryString(r, "host")
		hub    = queryString(r, "hub")
		health = queryString(r, "health")
		pool   = queryString(r, "pool")
		q      = queryString(r, "q")
		limit  = queryInt(r, "limit", 1000, 1, 5000)
	)

	var (
		conds []string
		args  []any
	)
	add := func(format string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}
	if host != "" {
		add("f.host_id = $%d", host)
	}
	if hub != "" {
		// Accept either the hub's usb_path ("3-1") or its numeric id, because
		// the grid groups by path while the topology view links by id.
		args = append(args, hub)
		conds = append(conds, fmt.Sprintf("(f.hub_path = $%d OR f.hub_id::text = $%d)", len(args), len(args)))
	}
	switch health {
	case "":
	case "unhealthy":
		// The operational question is "what is not fine", which is every state
		// except healthy — and except the two that are decisions rather than
		// faults: 'retired', and 'parked', which is a charge limiter holding a
		// battery or an operator who took a handset out and said why. Counting
		// a deliberate hold here fills the grid with devices that are fine,
		// during exactly the incident the filter exists for.
		conds = append(conds, "f.health IS NOT NULL AND f.health NOT IN ('healthy','retired','parked')")
	default:
		add("f.health = $%d", health)
	}
	if pool != "" {
		add("f.pool_id = $%d", pool)
	}
	if q != "" {
		args = append(args, q)
		n := len(args)
		conds = append(conds, fmt.Sprintf(`(
		     f.farm_uid ILIKE '%%' || $%d || '%%'
		  OR f.adb_serial ILIKE '%%' || $%d || '%%'
		  OR f.model ILIKE '%%' || $%d || '%%'
		  OR f.rack_slot ILIKE '%%' || $%d || '%%'
		  OR f.holder ILIKE '%%' || $%d || '%%'
		  OR f.device_id::text = $%d
		  OR f.job_id::text = $%d)`, n, n, n, n, n, n, n))
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, "\n   AND ")
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
SELECT %s
  FROM farm.v_fleet f
  %s
 ORDER BY f.host_id NULLS LAST, f.hub_path NULLS LAST, f.rack_slot NULLS LAST, f.farm_uid
 LIMIT $%d`, fleetColumns, where, len(args))

	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.fail(w, r, "list fleet", err)
		return
	}
	defer rows.Close()

	devices := make([]fleetDevice, 0, 128)
	counts := fleetCounts{
		Health:     map[string]int{},
		Host:       map[string]int{},
		LeaseState: map[string]int{},
	}
	for rows.Next() {
		d, err := scanFleetDevice(rows)
		if err != nil {
			s.fail(w, r, "scan fleet row", err)
			return
		}
		devices = append(devices, d)

		counts.Total++
		h := "unknown"
		if d.Health != nil {
			h = *d.Health
		}
		counts.Health[h]++
		// Same exclusion as the "unhealthy" filter above and as
		// farm.v_hub_health. These three must agree, or one response reports
		// four unhealthy devices in counts and zero on the hubs they sit on.
		if h != "healthy" && h != "retired" && h != "parked" {
			counts.Unhealthy++
		}
		hostID := "unassigned"
		if d.HostID != nil {
			hostID = *d.HostID
		}
		counts.Host[hostID]++
		if d.Lease != nil {
			counts.Leased++
			counts.LeaseState[d.Lease.State]++
			if d.Lease.Protected {
				counts.Protected++
			}
		} else {
			counts.Free++
		}
		if d.QuarantineID != nil {
			counts.Quarantined++
		}
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read fleet rows", err)
		return
	}
	// Released before the next query rather than at return: holding two pool
	// connections for one dashboard refresh is one fewer a renewal can borrow,
	// and a holder that cannot renew for ttl+grace loses its device.
	rows.Close()

	hubs, err := s.hubHealth(r.Context(), host, hub)
	if err != nil {
		s.fail(w, r, "read hub health", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"devices":   devices,
		"counts":    counts,
		"hubs":      hubs,
		"truncated": len(devices) == limit,
	})
}

// hubHealth reads farm.v_hub_health for the same slice of the farm the caller
// filtered the grid to.
func (s *Server) hubHealth(ctx context.Context, host, hub string) ([]hubHealth, error) {
	const query = `
SELECT v.hub_id, v.host_id, v.usb_path, v.model, v.vbus_switchable,
       v.devices, v.healthy, v.unhealthy, v.worst_since
  FROM farm.v_hub_health v
 WHERE ($1 = '' OR v.host_id = $1)
   AND ($2 = '' OR v.usb_path = $2 OR v.hub_id::text = $2)
 ORDER BY v.host_id, v.usb_path`

	rows, err := s.pool.Query(ctx, query, host, hub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]hubHealth, 0, 16)
	for rows.Next() {
		var h hubHealth
		if err := rows.Scan(&h.HubID, &h.HostID, &h.USBPath, &h.Model, &h.VbusSwitchable,
			&h.Devices, &h.Healthy, &h.Unhealthy, &h.WorstSince); err != nil {
			return nil, err
		}
		h.Correlated = h.Unhealthy > 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// handleDevice serves GET /api/v1/devices/{id}, accepting either the uuid or
// the branded farm_uid.
func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}

	d, err := s.lookupDevice(r.Context(), id)
	if err != nil {
		s.fail(w, r, "get device", err)
		return
	}

	hubs, err := s.hubHealth(r.Context(), derefString(d.HostID), hubKeyOf(d))
	if err != nil {
		s.fail(w, r, "get device hub health", err)
		return
	}
	var hub *hubHealth
	if len(hubs) == 1 {
		hub = &hubs[0]
	}

	attempts, err := s.recoveryAttempts(r.Context(), recoveryFilter{deviceID: d.DeviceID, limit: 20})
	if err != nil {
		s.fail(w, r, "get device recovery history", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device":   d,
		"hub":      hub,
		"recovery": attempts,
	})
}

// lookupDevice reads one row of farm.v_fleet by uuid or farm_uid.
func (s *Server) lookupDevice(ctx context.Context, id string) (fleetDevice, error) {
	query := fmt.Sprintf(`
SELECT %s
  FROM farm.v_fleet f
 WHERE f.device_id::text = $1 OR f.farm_uid = $1`, fleetColumns)

	return scanFleetDevice(s.pool.QueryRow(ctx, query, id))
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func hubKeyOf(d fleetDevice) string {
	if d.HubID == nil {
		return ""
	}
	return fmt.Sprintf("%d", *d.HubID)
}

// ---------------------------------------------------------------------------
// POST /api/v1/devices/{id}/exec
// ---------------------------------------------------------------------------

type execRequest struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	// Force runs the command even though the device holds a live lease. It is
	// opt-in because an operator shell on a device in the middle of somebody's
	// six-hour run can wreck that run — and unlike a lease revoke, the holder
	// gets no signal that it happened.
	Force bool `json:"force,omitempty"`
	// Reason is recorded in farm.audit_log. Optional for a read-only probe,
	// required when Force overrides a live lease.
	Reason string `json:"reason,omitempty"`
}

type execResponse struct {
	DeviceID   string `json:"device_id"`
	Devpath    string `json:"adb_devpath"`
	Endpoint   string `json:"adb_endpoint"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"`
	Stderr     string `json:"stderr,omitempty"`
	Exited     bool   `json:"exited"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	// LeaseID names the live lease this command ran alongside, when Force was
	// used. It is in the response so the caller sees whose run they just
	// touched.
	LeaseID string `json:"lease_id,omitempty"`
}

// handleDeviceExec serves POST /api/v1/devices/{id}/exec.
//
// This runs one shell command at a PHYSICAL POSITION — slots.adb_devpath, never
// a serial, because duplicate OEM serials are real and a serial-addressed exec
// can land on a different device than the one on screen.
//
// It touches no lease. A command that fails, times out, or finds the device
// wedged returns an error about the command; the lease of whoever holds the
// device is exactly as valid afterwards as it was before.
func (s *Server) handleDeviceExec(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}

	var req execRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		badRequest(w, "command is required", nil)
		return
	}

	timeout := defaultExecTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout > maxExecTimeout {
			timeout = maxExecTimeout
		}
	}

	d, err := s.lookupDevice(r.Context(), id)
	if err != nil {
		s.fail(w, r, "exec: resolve device", err)
		return
	}
	if d.ADBDevpath == nil || *d.ADBDevpath == "" {
		writeError(w, http.StatusConflict, CodeConflict,
			"this device is not in a slot, so it has no physical address to run against",
			map[string]string{"device_id": d.DeviceID, "farm_uid": d.FarmUID})
		return
	}
	if d.ADBEndpoint == nil || *d.ADBEndpoint == "" {
		writeError(w, http.StatusConflict, CodeConflict,
			"this device's host has no ADB endpoint recorded",
			map[string]string{"device_id": d.DeviceID, "host_id": derefString(d.HostID)})
		return
	}
	if d.Lease != nil && !req.Force {
		writeError(w, http.StatusConflict, CodeConflict,
			"this device holds a live lease; running a command on it can corrupt that job's run. "+
				"Retry with \"force\": true and a reason if you mean to do it anyway.",
			map[string]any{
				"lease_id":  d.Lease.ID,
				"job_id":    d.Lease.JobID,
				"tenant_id": d.Lease.TenantID,
				"holder":    d.Lease.Holder,
				"protected": d.Lease.Protected,
			})
		return
	}
	if d.Lease != nil && strings.TrimSpace(req.Reason) == "" {
		badRequest(w, "a reason is required to run a command on a leased device", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	exec := s.newExecutor(*d.ADBEndpoint, timeout, s.execMaxOutput)
	start := time.Now()
	res, execErr := exec.Shell(ctx, *d.ADBDevpath, req.Command)
	elapsed := time.Since(start)

	// The audit row outlives the request: an operator command that ran on a
	// device is a fact about the farm even if the caller hung up before the
	// output arrived.
	auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer auditCancel()

	detail := map[string]any{
		"command":     req.Command,
		"devpath":     *d.ADBDevpath,
		"host_id":     derefString(d.HostID),
		"duration_ms": elapsed.Milliseconds(),
		"forced":      req.Force,
	}
	if d.Lease != nil {
		detail["lease_id"] = d.Lease.ID
		detail["job_id"] = d.Lease.JobID
	}
	if res != nil {
		detail["exit_code"] = res.ExitCode
	}
	if execErr != nil {
		detail["error"] = execErr.Error()
	}

	outcome := "ok"
	if execErr != nil {
		outcome = "error"
	}
	s.metrics.execs.WithLabelValues(outcome).Inc()
	s.auditAction(auditCtx, actor(r.Context()), "device.exec", "device:"+d.DeviceID, req.Reason, detail)
	s.recordEvent(auditCtx, eventRow{
		Kind:     "device_exec",
		DeviceID: &d.DeviceID,
		SlotID:   d.SlotID,
		Actor:    actor(r.Context()),
		Detail:   detail,
	})

	if execErr != nil {
		// An ADB failure is a statement about the wire, not about the lease.
		// It is reported as a bad gateway with the transport's own
		// classification so the caller can tell a timeout from a refusal.
		s.log.WarnContext(r.Context(), "device exec failed",
			"device_id", d.DeviceID, "devpath", *d.ADBDevpath, "err", execErr)

		errDetail := map[string]any{
			"device_id": d.DeviceID,
			"devpath":   *d.ADBDevpath,
			"endpoint":  *d.ADBEndpoint,
		}
		if te, ok := adbwire.AsTransport(execErr); ok {
			errDetail["transport_kind"] = te.Kind.String()
		}
		if errors.Is(execErr, context.DeadlineExceeded) {
			errDetail["timeout_ms"] = timeout.Milliseconds()
		}
		writeError(w, http.StatusBadGateway, CodeADBError,
			"the command did not complete against this device: "+execErr.Error()+
				". No lease was affected.", errDetail)
		return
	}

	if res == nil {
		// An Executor that reports neither a result nor an error is broken.
		// Saying so beats dereferencing nil and turning one bad ADB client into
		// a 500 that looks like a control-plane fault.
		writeError(w, http.StatusBadGateway, CodeADBError,
			"the executor returned no result and no error for this command. No lease was affected.",
			map[string]string{"device_id": d.DeviceID, "devpath": *d.ADBDevpath})
		return
	}

	resp := execResponse{
		DeviceID:   d.DeviceID,
		Devpath:    *d.ADBDevpath,
		Endpoint:   *d.ADBEndpoint,
		Command:    req.Command,
		ExitCode:   res.ExitCode,
		Output:     string(res.Stdout),
		Stderr:     string(res.Stderr),
		Exited:     res.Exited,
		Truncated:  res.Truncated,
		DurationMS: elapsed.Milliseconds(),
	}
	if d.Lease != nil {
		resp.LeaseID = d.Lease.ID
	}
	writeJSON(w, http.StatusOK, resp)
}
