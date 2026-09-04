package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// mustJSON marshals a detail map for a jsonb column. A detail that cannot be
// encoded must not lose the audit row it belongs to, so it degrades rather than
// failing the operation it is describing.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Degraded, but not silently: an empty detail reads as "there was
		// nothing to say", which is a different claim from "the detail could
		// not be encoded" and would send whoever reads this row looking in the
		// wrong place.
		enc, encErr := json.Marshal(map[string]string{"detail_encoding_error": err.Error()})
		if encErr != nil {
			return []byte(`{"detail_encoding_error":"unprintable"}`)
		}
		return enc
	}
	if string(b) == "null" {
		// A nil detail map marshals to JSON null, which is a legal jsonb value
		// and would make detail->>'k' behave differently from every other row.
		return []byte(`{}`)
	}
	return b
}

// detachedCtx bounds bookkeeping that must outlive the request that caused it.
//
// Every operator action here commits its state change BEFORE writing the audit
// row that names who did it. If that write inherited the request context, a
// caller hanging up in the gap — a closed laptop, a proxy timeout, a killed
// curl — would leave a drained host, a closed quarantine or a requested power
// cycle with nobody's name on it. "Who power-cycled the hub at 02:14" is the
// one question farm.audit_log exists to answer, and it must not depend on the
// operator's TCP connection outliving their own command.
func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// eventRow is one row of farm.events: the operational timeline the dashboard
// and the stream both read.
type eventRow struct {
	Kind     string
	DeviceID *string
	SlotID   *int64
	LeaseID  *string
	JobID    *string
	Actor    string
	Detail   map[string]any
}

// recordEvent writes to farm.events. A failure here is logged and swallowed:
// losing a timeline entry is bad, but failing an operator action that already
// took effect — or a release that already happened — is worse.
func (s *Server) recordEvent(ctx context.Context, e eventRow) {
	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
VALUES ($1, $2::uuid, $3, $4::uuid, $5::uuid, $6, $7::jsonb)`

	if _, err := s.pool.Exec(ctx, q, e.Kind, e.DeviceID, e.SlotID, e.LeaseID, e.JobID,
		e.Actor, mustJSON(e.Detail)); err != nil {
		s.log.WarnContext(ctx, "could not record event", "kind", e.Kind, "err", err)
	}
}

// auditAction writes farm.audit_log. Every operator action in this package
// calls it, because "who power-cycled the hub at 02:14" is a question that gets
// asked and must have an answer.
func (s *Server) auditAction(ctx context.Context, who, action, subject, reason string, detail map[string]any) {
	const q = `
INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
VALUES ($1, $2, $3, nullif($4,''), $5::jsonb)`

	if _, err := s.pool.Exec(ctx, q, who, action, subject, reason, mustJSON(detail)); err != nil {
		s.log.ErrorContext(ctx, "could not write audit row",
			"action", action, "subject", subject, "actor", who, "err", err)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/topology
// ---------------------------------------------------------------------------

type topologySlot struct {
	SlotID        int64      `json:"slot_id"`
	PortNumber    int        `json:"port_number"`
	USBPath       string     `json:"usb_path"`
	ADBDevpath    string     `json:"adb_devpath"`
	RackSlot      *string    `json:"rack_slot,omitempty"`
	State         string     `json:"state"`
	RearmAt       time.Time  `json:"rearm_at"`
	PowerDomainID *int64     `json:"power_domain_id,omitempty"`
	PowerKind     *string    `json:"power_kind,omitempty"`
	PowerControl  *string    `json:"power_control,omitempty"`
	DeviceID      *string    `json:"device_id,omitempty"`
	FarmUID       *string    `json:"farm_uid,omitempty"`
	Model         *string    `json:"model,omitempty"`
	ADBState      *string    `json:"adb_state,omitempty"`
	Health        *string    `json:"health,omitempty"`
	LeaseID       *string    `json:"lease_id,omitempty"`
	LeaseState    *string    `json:"lease_state,omitempty"`
	JobID         *string    `json:"job_id,omitempty"`
	TenantID      *string    `json:"tenant_id,omitempty"`
	Protected     *bool      `json:"protected,omitempty"`
	Policy        *string    `json:"disruption_policy,omitempty"`
	ExpiresAt     *time.Time `json:"lease_expires_at,omitempty"`
}

type topologyHub struct {
	HubID          int64          `json:"hub_id"`
	USBPath        string         `json:"usb_path"`
	Model          *string        `json:"model,omitempty"`
	PortCount      int            `json:"port_count"`
	VbusSwitchable bool           `json:"vbus_switchable"`
	Devices        int            `json:"devices"`
	Healthy        int            `json:"healthy"`
	Unhealthy      int            `json:"unhealthy"`
	WorstSince     *time.Time     `json:"worst_since,omitempty"`
	Correlated     bool           `json:"correlated"`
	Slots          []topologySlot `json:"slots"`
}

type topologyHost struct {
	ID            string        `json:"id"`
	RackID        *string       `json:"rack_id,omitempty"`
	RackUnit      *int          `json:"rack_unit,omitempty"`
	HostEpoch     int64         `json:"host_epoch"`
	ADBEndpoint   string        `json:"adb_endpoint"`
	AdminState    string        `json:"admin_state"`
	KernelRelease *string       `json:"kernel_release,omitempty"`
	AgentVersion  *string       `json:"agent_version,omitempty"`
	LastSeenAt    *time.Time    `json:"last_seen_at,omitempty"`
	Hubs          []topologyHub `json:"hubs"`
}

// handleTopology serves GET /api/v1/topology: hosts, their hubs with
// farm.v_hub_health folded in, and the slots under each hub.
//
// The slot is the primary object here, not the device. A slot is what a human
// can find in a rack, what a power switch controls, and what survives a phone
// being swapped out — which is why an alert names a rack position rather than a
// serial.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	host := queryString(r, "host")

	hosts, err := s.readHosts(r.Context(), host)
	if err != nil {
		s.fail(w, r, "topology: read hosts", err)
		return
	}

	const hubQuery = `
SELECT h.id, h.host_id, h.usb_path, h.model, h.port_count, h.vbus_switchable,
       v.devices, v.healthy, v.unhealthy, v.worst_since
  FROM farm.hubs h
  JOIN farm.v_hub_health v ON v.hub_id = h.id
 WHERE ($1 = '' OR h.host_id = $1)
 ORDER BY h.host_id, h.usb_path`

	hubRows, err := s.pool.Query(r.Context(), hubQuery, host)
	if err != nil {
		s.fail(w, r, "topology: read hubs", err)
		return
	}
	defer hubRows.Close()

	hubsByHost := map[string][]topologyHub{}
	hubIndex := map[int64]*topologyHub{}
	for hubRows.Next() {
		var (
			h      topologyHub
			hostID string
		)
		if err := hubRows.Scan(&h.HubID, &hostID, &h.USBPath, &h.Model, &h.PortCount,
			&h.VbusSwitchable, &h.Devices, &h.Healthy, &h.Unhealthy, &h.WorstSince); err != nil {
			s.fail(w, r, "topology: scan hub", err)
			return
		}
		h.Correlated = h.Unhealthy > 1
		h.Slots = []topologySlot{}
		hubsByHost[hostID] = append(hubsByHost[hostID], h)
	}
	if err := hubRows.Err(); err != nil {
		s.fail(w, r, "topology: read hubs", err)
		return
	}
	// Released before the slot query rather than at return: the two would
	// otherwise hold two pool connections at once for a read-only view, and
	// those are connections a renewal cannot borrow.
	hubRows.Close()

	for hostID := range hubsByHost {
		for i := range hubsByHost[hostID] {
			hubIndex[hubsByHost[hostID][i].HubID] = &hubsByHost[hostID][i]
		}
	}

	const slotQuery = `
SELECT s.id, s.hub_id, s.port_number, s.usb_path, s.adb_devpath, s.rack_slot, s.state, s.rearm_at,
       s.power_domain_id, pd.kind, pd.control,
       d.id::text, d.farm_uid, d.model, r.adb_state, r.health,
       l.id::text, l.state, l.job_id::text, l.tenant_id, l.protected, l.disruption_policy, l.expires_at
  FROM farm.slots s
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
  LEFT JOIN farm.devices d        ON d.current_slot_id = s.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.leases l         ON l.id = d.current_lease_id
 WHERE ($1 = '' OR s.host_id = $1)
 ORDER BY s.host_id, s.hub_id, s.port_number`

	slotRows, err := s.pool.Query(r.Context(), slotQuery, host)
	if err != nil {
		s.fail(w, r, "topology: read slots", err)
		return
	}
	defer slotRows.Close()

	for slotRows.Next() {
		var (
			sl    topologySlot
			hubID int64
		)
		if err := slotRows.Scan(&sl.SlotID, &hubID, &sl.PortNumber, &sl.USBPath, &sl.ADBDevpath,
			&sl.RackSlot, &sl.State, &sl.RearmAt, &sl.PowerDomainID, &sl.PowerKind, &sl.PowerControl,
			&sl.DeviceID, &sl.FarmUID, &sl.Model, &sl.ADBState, &sl.Health,
			&sl.LeaseID, &sl.LeaseState, &sl.JobID, &sl.TenantID, &sl.Protected, &sl.Policy,
			&sl.ExpiresAt); err != nil {
			s.fail(w, r, "topology: scan slot", err)
			return
		}
		if hub, ok := hubIndex[hubID]; ok {
			hub.Slots = append(hub.Slots, sl)
		}
	}
	if err := slotRows.Err(); err != nil {
		s.fail(w, r, "topology: read slots", err)
		return
	}

	for i := range hosts {
		hosts[i].Hubs = hubsByHost[hosts[i].ID]
		if hosts[i].Hubs == nil {
			hosts[i].Hubs = []topologyHub{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (s *Server) readHosts(ctx context.Context, host string) ([]topologyHost, error) {
	const q = `
SELECT h.id, h.rack_id, h.rack_unit, h.host_epoch, h.adb_endpoint, h.admin_state,
       h.kernel_release, h.agent_version, h.last_seen_at
  FROM farm.hosts h
 WHERE ($1 = '' OR h.id = $1)
 ORDER BY h.rack_id NULLS LAST, h.rack_unit NULLS LAST, h.id`

	rows, err := s.pool.Query(ctx, q, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]topologyHost, 0, 8)
	for rows.Next() {
		var h topologyHost
		if err := rows.Scan(&h.ID, &h.RackID, &h.RackUnit, &h.HostEpoch, &h.ADBEndpoint,
			&h.AdminState, &h.KernelRelease, &h.AgentVersion, &h.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// GET /api/v1/hosts, POST /api/v1/hosts/{id}/drain, .../undrain
// ---------------------------------------------------------------------------

// handleHosts serves GET /api/v1/hosts with the counts an operator needs before
// draining one: how many devices are on it, how many are healthy, and how many
// live leases would have to be waited out.
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	const q = `
SELECT h.id, h.rack_id, h.rack_unit, h.host_epoch, h.adb_endpoint, h.admin_state,
       h.kernel_release, h.agent_version, h.last_seen_at,
       count(d.id) AS devices,
       count(*) FILTER (WHERE r.health = 'healthy') AS healthy,
       -- 'parked' is excluded with 'retired': both are decisions, not faults.
       -- This is the number an operator reads before draining a host, and a
       -- shelf of charge-limited handsets must not look like a host falling
       -- apart. Same exclusion as farm.v_hub_health and the fleet counts.
       count(*) FILTER (WHERE r.health IS NOT NULL AND r.health NOT IN ('healthy','retired','parked')) AS unhealthy,
       count(*) FILTER (WHERE l.state IN ('held','suspect')) AS live_leases,
       count(*) FILTER (WHERE l.state IN ('held','suspect') AND l.protected) AS protected_leases
  FROM farm.hosts h
  LEFT JOIN farm.devices d        ON d.host_id = h.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.leases l         ON l.id = d.current_lease_id
 GROUP BY h.id
 ORDER BY h.rack_id NULLS LAST, h.rack_unit NULLS LAST, h.id`

	rows, err := s.pool.Query(r.Context(), q)
	if err != nil {
		s.fail(w, r, "list hosts", err)
		return
	}
	defer rows.Close()

	type hostRow struct {
		ID              string     `json:"id"`
		RackID          *string    `json:"rack_id,omitempty"`
		RackUnit        *int       `json:"rack_unit,omitempty"`
		HostEpoch       int64      `json:"host_epoch"`
		ADBEndpoint     string     `json:"adb_endpoint"`
		AdminState      string     `json:"admin_state"`
		KernelRelease   *string    `json:"kernel_release,omitempty"`
		AgentVersion    *string    `json:"agent_version,omitempty"`
		LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
		Devices         int        `json:"devices"`
		Healthy         int        `json:"healthy"`
		Unhealthy       int        `json:"unhealthy"`
		LiveLeases      int        `json:"live_leases"`
		ProtectedLeases int        `json:"protected_leases"`
	}

	out := make([]hostRow, 0, 8)
	for rows.Next() {
		var h hostRow
		if err := rows.Scan(&h.ID, &h.RackID, &h.RackUnit, &h.HostEpoch, &h.ADBEndpoint,
			&h.AdminState, &h.KernelRelease, &h.AgentVersion, &h.LastSeenAt,
			&h.Devices, &h.Healthy, &h.Unhealthy, &h.LiveLeases, &h.ProtectedLeases); err != nil {
			s.fail(w, r, "scan host", err)
			return
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read hosts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

// handleHostDrain serves POST /api/v1/hosts/{id}/drain.
//
// Draining stops NEW leases from landing on the host. It does not touch the
// live ones: they run to completion and the response says how many there are,
// because "drained" meaning "I took eleven phones away from running jobs" is
// the failure this whole system is built against.
func (s *Server) handleHostDrain(w http.ResponseWriter, r *http.Request) {
	s.setHostAdminState(w, r, "draining", "host.drain")
}

// handleHostUndrain serves POST /api/v1/hosts/{id}/undrain.
func (s *Server) handleHostUndrain(w http.ResponseWriter, r *http.Request) {
	s.setHostAdminState(w, r, "enabled", "host.undrain")
}

func (s *Server) setHostAdminState(w http.ResponseWriter, r *http.Request, state, action string) {
	hostID := strings.TrimSpace(r.PathValue("id"))
	if hostID == "" {
		badRequest(w, "host id is required", nil)
		return
	}
	var req revokeRequest
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

	// The previous state comes from a CTE rather than from a sub-SELECT in
	// RETURNING: a CTE is evaluated against the snapshot before the UPDATE by
	// definition, so "what was it before" cannot quietly become "what is it
	// now" under a different plan.
	var previous string
	err := s.pool.QueryRow(r.Context(), `
WITH prev AS (SELECT admin_state FROM farm.hosts WHERE id = $1)
UPDATE farm.hosts SET admin_state = $2
 WHERE id = $1
RETURNING (SELECT admin_state FROM prev)`, hostID, state).Scan(&previous)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such host", nil)
			return
		}
		s.fail(w, r, action, err)
		return
	}

	var liveLeases, protectedLeases int
	if err := s.pool.QueryRow(r.Context(), `
SELECT count(*) FILTER (WHERE l.state IN ('held','suspect')),
       count(*) FILTER (WHERE l.state IN ('held','suspect') AND l.protected)
  FROM farm.leases l
  JOIN farm.devices d ON d.id = l.device_id
 WHERE d.host_id = $1`, hostID).Scan(&liveLeases, &protectedLeases); err != nil {
		s.fail(w, r, action+": count leases", err)
		return
	}

	detail := map[string]any{
		"host_id":          hostID,
		"previous_state":   previous,
		"new_state":        state,
		"live_leases":      liveLeases,
		"protected_leases": protectedLeases,
	}
	// The admin state has already changed; its audit row must not depend on the
	// operator still being connected.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, action, "host:"+hostID, reason, detail)
	s.recordEvent(bookCtx, eventRow{Kind: strings.ReplaceAll(action, ".", "_"), Actor: who, Detail: detail})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues(action, "ok").Inc()
	s.log.InfoContext(r.Context(), "host admin state changed",
		"host_id", hostID, "state", state, "actor", who, "reason", reason, "live_leases", liveLeases)

	body := map[string]any{
		"host_id":          hostID,
		"admin_state":      state,
		"previous_state":   previous,
		"live_leases":      liveLeases,
		"protected_leases": protectedLeases,
	}
	if state == "draining" {
		body["note"] = "no new leases will be placed on this host. The live leases above are " +
			"untouched and will run to completion; nothing here releases them."
	}
	writeJSON(w, http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// POST /api/v1/slots/{id}/power
// ---------------------------------------------------------------------------

// powerCycleTier is the rung of the recovery ladder a slot power cycle occupies
// (farm.recovery_tiers row 4, 'port_power'). Its blast radius and its required
// disruption policy are read from the table rather than hard-coded here, so an
// operator retuning the ladder retunes this refusal with it.
const powerCycleTier = 4

// policyRank orders farm.jobs.disruption_policy from least to most permissive.
// A lease permits a tier only when its policy rank is at least the tier's
// requires_policy rank.
func policyRank(p string) int {
	switch p {
	case "no_disruption":
		return 1
	case "allow_soft_reset":
		return 2
	case "allow_port_power_cycle":
		return 3
	default:
		// An unknown policy is treated as the most restrictive. Refusing a
		// power cycle we could have allowed costs a few minutes; allowing one
		// we should have refused costs somebody's six-hour run.
		return 0
	}
}

type powerOffender struct {
	LeaseID   string  `json:"lease_id"`
	JobID     string  `json:"job_id"`
	TenantID  string  `json:"tenant_id"`
	Holder    string  `json:"holder"`
	Policy    string  `json:"disruption_policy"`
	Protected bool    `json:"protected"`
	DeviceID  string  `json:"device_id"`
	FarmUID   string  `json:"farm_uid"`
	SlotID    int64   `json:"slot_id"`
	RackSlot  *string `json:"rack_slot,omitempty"`
}

// handleSlotPower serves POST /api/v1/slots/{id}/power.
//
// A power cycle is not a per-device action. On a hub without per-port
// switching, every port shares one power domain, so "power-cycle this slot" is
// really "power-cycle these seven devices" — and this handler REFUSES with 409
// when any live lease anywhere in that domain carries a disruption_policy that
// forbids it.
//
// The refusal body names the offending lease, its job, its tenant and its
// policy. That is the difference between a dashboard that can say "tier 4
// refused: lease abc for job def is no_disruption" and one that shows a dead
// button an operator will eventually work around with a physical power switch.
//
// The API does not itself toggle VBUS: uhubctl runs on the host. What this
// handler does is decide whether the action is permitted, record the decision
// as a farm.recovery_attempts row at tier 4, and audit it. The host agent
// consumes the attempt.
func (s *Server) handleSlotPower(w http.ResponseWriter, r *http.Request) {
	slotID, ok := pathInt64(r, "id")
	if !ok {
		badRequest(w, "slot id must be an integer", nil)
		return
	}
	var req revokeRequest
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

	// The slot, its power domain, and its hub's switching capability.
	var (
		hostID         string
		hubID          int64
		powerDomainID  *int64
		rackSlot       *string
		usbPath        string
		adbDevpath     string
		slotState      string
		pdKind         *string
		pdControl      *string
		pdAddr         *string
		vbusSwitchable bool
	)
	err := s.pool.QueryRow(r.Context(), `
SELECT s.host_id, s.hub_id, s.power_domain_id, s.rack_slot, s.usb_path, s.adb_devpath, s.state,
       pd.kind, pd.control, pd.control_addr, hb.vbus_switchable
  FROM farm.slots s
  JOIN farm.hubs hb ON hb.id = s.hub_id
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
 WHERE s.id = $1`, slotID).
		Scan(&hostID, &hubID, &powerDomainID, &rackSlot, &usbPath, &adbDevpath, &slotState,
			&pdKind, &pdControl, &pdAddr, &vbusSwitchable)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such slot", nil)
			return
		}
		s.fail(w, r, "slot power: read slot", err)
		return
	}

	// The tier definition, from the table an operator can retune.
	var (
		tierName       string
		blastRadius    string
		requiresPolicy string
		tierEnabled    bool
	)
	if err := s.pool.QueryRow(r.Context(), `
SELECT name, blast_radius, requires_policy, enabled
  FROM farm.recovery_tiers WHERE tier = $1`, powerCycleTier).
		Scan(&tierName, &blastRadius, &requiresPolicy, &tierEnabled); err != nil {
		s.fail(w, r, "slot power: read tier", err)
		return
	}

	// Everything in the power domain, and every live lease inside it.
	const domainQuery = `
SELECT l.id::text, l.job_id::text, l.tenant_id, l.holder, l.disruption_policy, l.protected,
       d.id::text, d.farm_uid, s.id, s.rack_slot
  FROM farm.slots s
  JOIN farm.devices d ON d.current_slot_id = s.id
  JOIN farm.leases  l ON l.id = d.current_lease_id
 WHERE l.state IN ('held','suspect')
   AND (CASE WHEN $2::bigint IS NULL THEN s.id = $1 ELSE s.power_domain_id = $2 END)`

	rows, err := s.pool.Query(r.Context(), domainQuery, slotID, powerDomainID)
	if err != nil {
		s.fail(w, r, "slot power: read domain leases", err)
		return
	}
	defer rows.Close()

	var (
		liveLeases []powerOffender
		offenders  []powerOffender
	)
	for rows.Next() {
		var o powerOffender
		if err := rows.Scan(&o.LeaseID, &o.JobID, &o.TenantID, &o.Holder, &o.Policy, &o.Protected,
			&o.DeviceID, &o.FarmUID, &o.SlotID, &o.RackSlot); err != nil {
			s.fail(w, r, "slot power: scan domain lease", err)
			return
		}
		liveLeases = append(liveLeases, o)
		if policyRank(o.Policy) < policyRank(requiresPolicy) {
			offenders = append(offenders, o)
		}
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "slot power: read domain leases", err)
		return
	}

	var domainSlots int
	if err := s.pool.QueryRow(r.Context(), `
SELECT count(*) FROM farm.slots s
 WHERE (CASE WHEN $2::bigint IS NULL THEN s.id = $1 ELSE s.power_domain_id = $2 END)`,
		slotID, powerDomainID).Scan(&domainSlots); err != nil {
		s.fail(w, r, "slot power: size power domain", err)
		return
	}

	domain := map[string]any{
		"slot_id":         slotID,
		"rack_slot":       rackSlot,
		"host_id":         hostID,
		"hub_id":          hubID,
		"power_domain_id": powerDomainID,
		"power_kind":      pdKind,
		"power_control":   pdControl,
		"slot_state":      slotState,
		"usb_path":        usbPath,
		"slots_in_domain": domainSlots,
		"live_leases":     len(liveLeases),
		"tier":            powerCycleTier,
		"tier_name":       tierName,
		"blast_radius":    blastRadius,
		"requires_policy": requiresPolicy,
	}

	refuse := func(status int, code, message string, detail map[string]any) {
		refusal := message
		bookCtx, bookCancel := detachedCtx(r.Context())
		s.recordRecoveryRefusal(bookCtx, slotID, hubID, hostID, refusal, detail)
		s.auditAction(bookCtx, who, "slot.power", fmt.Sprintf("slot:%d", slotID), reason,
			mergeDetail(domain, map[string]any{"outcome": "refused", "refusal": refusal}))
		bookCancel()
		s.metrics.operatorActions.WithLabelValues("slot.power", "refused").Inc()
		writeError(w, status, code, message, mergeDetail(domain, detail))
	}

	switch {
	case !tierEnabled:
		refuse(http.StatusConflict, CodeConflict,
			"tier "+tierName+" is disabled in farm.recovery_tiers", nil)
		return
	case powerDomainID == nil || pdControl == nil || *pdControl == "none":
		refuse(http.StatusConflict, CodeConflict,
			"this slot has no controllable power domain, so its port power cannot be cycled from here",
			map[string]any{"vbus_switchable": vbusSwitchable})
		return
	case !vbusSwitchable:
		refuse(http.StatusConflict, CodeConflict,
			"this slot's hub does not support switching VBUS per port",
			map[string]any{"vbus_switchable": false})
		return
	case len(offenders) > 0:
		// THE REFUSAL THIS ENDPOINT EXISTS FOR.
		refusalCtx, refusalCancel := detachedCtx(r.Context())
		s.recordRecoveryRefusal(refusalCtx, slotID, hubID, hostID,
			fmt.Sprintf("tier %d (%s) refused: %d live lease(s) in this power domain forbid it",
				powerCycleTier, tierName, len(offenders)),
			map[string]any{"offenders": offenders, "requires_policy": requiresPolicy})
		s.auditAction(refusalCtx, who, "slot.power", fmt.Sprintf("slot:%d", slotID), reason,
			mergeDetail(domain, map[string]any{"outcome": "refused", "offenders": offenders}))
		refusalCancel()
		s.metrics.operatorActions.WithLabelValues("slot.power", "refused").Inc()
		writeError(w, http.StatusConflict, CodeDisruptionRefused,
			fmt.Sprintf("this power cycle would disturb %d device(s) in power domain %d, and %d live "+
				"lease(s) there forbid it. Tier %d (%s) requires disruption_policy %q.",
				domainSlots, *powerDomainID, len(offenders), powerCycleTier, tierName, requiresPolicy),
			mergeDetail(domain, map[string]any{
				"offenders":        offenders,
				"live_leases_list": liveLeases,
				"remedy": "wait for the offending leases to end, or have their holders release them; " +
					"an operator may also revoke a specific lease, which is audited",
			}))
		return
	}

	// Permitted. Record the intent as an open attempt at tier 4; the host agent
	// performs the switch and closes the row with its outcome.
	var attemptID int64
	if err := s.pool.QueryRow(r.Context(), `
INSERT INTO farm.recovery_attempts (slot_id, hub_id, host_id, tier, detail)
VALUES ($1, $2, $3, $4, $5::jsonb)
RETURNING id`, slotID, hubID, hostID, powerCycleTier,
		mustJSON(mergeDetail(domain, map[string]any{
			"requested_by": who, "reason": reason, "adb_devpath": adbDevpath,
		}))).Scan(&attemptID); err != nil {
		s.fail(w, r, "slot power: record attempt", err)
		return
	}

	detail := mergeDetail(domain, map[string]any{
		"attempt_id":   attemptID,
		"outcome":      "requested",
		"adb_devpath":  adbDevpath,
		"usb_path":     usbPath,
		"live_leases":  len(liveLeases),
		"control_addr": pdAddr,
	})
	// The attempt row is committed and the host agent may already be acting on
	// it, so the audit trail must not depend on the caller still being here.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, "slot.power", fmt.Sprintf("slot:%d", slotID), reason, detail)
	s.recordEvent(bookCtx, eventRow{
		Kind: "slot_power_requested", SlotID: &slotID, Actor: who, Detail: detail,
	})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues("slot.power", "requested").Inc()
	s.log.WarnContext(r.Context(), "slot power cycle requested",
		"slot_id", slotID, "host_id", hostID, "actor", who, "reason", reason,
		"slots_in_domain", domainSlots, "live_leases", len(liveLeases))

	writeJSON(w, http.StatusAccepted, mergeDetail(domain, map[string]any{
		"attempt_id": attemptID,
		"state":      "requested",
		"note": "the host agent performs the switch and closes recovery attempt " +
			fmt.Sprint(attemptID) + " with its outcome. Any lease in this power domain keeps " +
			"its device, its fence and its deadline throughout.",
	}))
}

// recordRecoveryRefusal writes the refused attempt. farm.recovery_attempts has
// a refusal column precisely so a refusal is data rather than a gap in the
// timeline: "nothing happened here" and "we declined to act, and here is why"
// must not look the same to whoever reads this at 3am.
func (s *Server) recordRecoveryRefusal(ctx context.Context, slotID, hubID int64, hostID, refusal string, detail map[string]any) {
	const q = `
INSERT INTO farm.recovery_attempts (slot_id, hub_id, host_id, tier, finished_at, outcome, refusal, detail)
VALUES ($1, $2, $3, $4, now(), 'refused', $5, $6::jsonb)`

	if _, err := s.pool.Exec(ctx, q, slotID, hubID, hostID, powerCycleTier, refusal,
		mustJSON(detail)); err != nil {
		s.log.ErrorContext(ctx, "could not record refused recovery attempt",
			"slot_id", slotID, "err", err)
	}
}

func mergeDetail(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// GET /api/v1/recovery, POST /api/v1/quarantines/{id}/close
// ---------------------------------------------------------------------------

type recoveryAttempt struct {
	ID          int64           `json:"id"`
	DeviceID    *string         `json:"device_id,omitempty"`
	FarmUID     *string         `json:"farm_uid,omitempty"`
	SlotID      *int64          `json:"slot_id,omitempty"`
	RackSlot    *string         `json:"rack_slot,omitempty"`
	HubID       *int64          `json:"hub_id,omitempty"`
	HostID      *string         `json:"host_id,omitempty"`
	Tier        int             `json:"tier"`
	TierName    string          `json:"tier_name"`
	BlastRadius string          `json:"blast_radius"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	Outcome     *string         `json:"outcome,omitempty"`
	Refusal     *string         `json:"refusal,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
}

type recoveryFilter struct {
	deviceID string
	hostID   string
	limit    int
}

func (s *Server) recoveryAttempts(ctx context.Context, f recoveryFilter) ([]recoveryAttempt, error) {
	if f.limit <= 0 {
		f.limit = 100
	}
	const q = `
SELECT a.id, a.device_id::text, d.farm_uid, a.slot_id, s.rack_slot, a.hub_id, a.host_id,
       a.tier, t.name, t.blast_radius, a.started_at, a.finished_at, a.outcome, a.refusal, a.detail
  FROM farm.recovery_attempts a
  JOIN farm.recovery_tiers t ON t.tier = a.tier
  LEFT JOIN farm.devices d ON d.id = a.device_id
  LEFT JOIN farm.slots   s ON s.id = a.slot_id
 WHERE ($1 = '' OR a.device_id::text = $1)
   AND ($2 = '' OR a.host_id = $2)
 ORDER BY a.started_at DESC
 LIMIT $3`

	rows, err := s.pool.Query(ctx, q, f.deviceID, f.hostID, f.limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]recoveryAttempt, 0, 32)
	for rows.Next() {
		var (
			a      recoveryAttempt
			detail []byte
		)
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.FarmUID, &a.SlotID, &a.RackSlot, &a.HubID,
			&a.HostID, &a.Tier, &a.TierName, &a.BlastRadius, &a.StartedAt, &a.FinishedAt,
			&a.Outcome, &a.Refusal, &detail); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			a.Detail = json.RawMessage(detail)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// handleRecovery serves GET /api/v1/recovery: what the ladder tried recently,
// what is still quarantined, and the ladder itself.
//
// The tier table is part of the response on purpose: it lets the dashboard show
// what the system WILL try before it tries it, including the disruption policy
// each rung needs, so an operator can see in advance which rungs a given lease
// has taken off the table.
func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100, 1, 1000)

	attempts, err := s.recoveryAttempts(r.Context(), recoveryFilter{
		deviceID: queryString(r, "device"),
		hostID:   queryString(r, "host"),
		limit:    limit,
	})
	if err != nil {
		s.fail(w, r, "list recovery attempts", err)
		return
	}

	quarantines, err := s.openQuarantines(r.Context())
	if err != nil {
		s.fail(w, r, "list quarantines", err)
		return
	}

	const tierQuery = `
SELECT t.tier, t.name, t.description, t.blast_radius, t.requires_policy,
       EXTRACT(EPOCH FROM t.cooldown)::bigint, t.max_per_hour, t.enabled
  FROM farm.recovery_tiers t ORDER BY t.tier`

	rows, err := s.pool.Query(r.Context(), tierQuery)
	if err != nil {
		s.fail(w, r, "list recovery tiers", err)
		return
	}
	defer rows.Close()

	type tierRow struct {
		Tier           int    `json:"tier"`
		Name           string `json:"name"`
		Description    string `json:"description"`
		BlastRadius    string `json:"blast_radius"`
		RequiresPolicy string `json:"requires_policy"`
		CooldownS      int64  `json:"cooldown_s"`
		MaxPerHour     int    `json:"max_per_hour"`
		Enabled        bool   `json:"enabled"`
	}
	tiers := make([]tierRow, 0, 9)
	for rows.Next() {
		var t tierRow
		if err := rows.Scan(&t.Tier, &t.Name, &t.Description, &t.BlastRadius, &t.RequiresPolicy,
			&t.CooldownS, &t.MaxPerHour, &t.Enabled); err != nil {
			s.fail(w, r, "scan recovery tier", err)
			return
		}
		tiers = append(tiers, t)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read recovery tiers", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"attempts":    attempts,
		"quarantines": quarantines,
		"tiers":       tiers,
	})
}

type quarantineView struct {
	ID       int64     `json:"id"`
	Scope    string    `json:"scope"`
	DeviceID *string   `json:"device_id,omitempty"`
	FarmUID  *string   `json:"farm_uid,omitempty"`
	SlotID   *int64    `json:"slot_id,omitempty"`
	RackSlot *string   `json:"rack_slot,omitempty"`
	HubID    *int64    `json:"hub_id,omitempty"`
	HostID   *string   `json:"host_id,omitempty"`
	Reason   string    `json:"reason"`
	OpenedAt time.Time `json:"opened_at"`
	Auto     bool      `json:"auto"`
}

func (s *Server) openQuarantines(ctx context.Context) ([]quarantineView, error) {
	const q = `
SELECT q.id, q.scope, q.device_id::text, d.farm_uid, q.slot_id, s.rack_slot, q.hub_id, q.host_id,
       q.reason, q.opened_at, q.auto
  FROM farm.quarantines q
  LEFT JOIN farm.devices d ON d.id = q.device_id
  LEFT JOIN farm.slots   s ON s.id = q.slot_id
 WHERE q.closed_at IS NULL
 ORDER BY q.opened_at DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]quarantineView, 0, 16)
	for rows.Next() {
		var v quarantineView
		if err := rows.Scan(&v.ID, &v.Scope, &v.DeviceID, &v.FarmUID, &v.SlotID, &v.RackSlot,
			&v.HubID, &v.HostID, &v.Reason, &v.OpenedAt, &v.Auto); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// handleQuarantineClose serves POST /api/v1/quarantines/{id}/close.
//
// Closing a quarantine makes a device schedulable again, which is a claim that
// somebody fixed something. It is audited with the human's name and reason for
// exactly that reason.
func (s *Server) handleQuarantineClose(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		badRequest(w, "quarantine id must be an integer", nil)
		return
	}
	var req revokeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required: closing a quarantine claims the fault is fixed", nil)
		return
	}
	who := actor(r.Context())

	var (
		scope     string
		deviceID  *string
		slotID    *int64
		hubID     *int64
		hostID    *string
		openedFor string
		openedAt  time.Time
	)
	err := s.pool.QueryRow(r.Context(), `
UPDATE farm.quarantines
   SET closed_at = now(), closed_by = $2
 WHERE id = $1 AND closed_at IS NULL
RETURNING scope, device_id::text, slot_id, hub_id, host_id, reason, opened_at`, id, who).
		Scan(&scope, &deviceID, &slotID, &hubID, &hostID, &openedFor, &openedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var closedAt *time.Time
			readErr := s.pool.QueryRow(r.Context(),
				`SELECT closed_at FROM farm.quarantines WHERE id = $1`, id).Scan(&closedAt)
			if errors.Is(readErr, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, CodeNotFound, "no such quarantine", nil)
				return
			}
			if readErr != nil {
				s.fail(w, r, "close quarantine: read state", readErr)
				return
			}
			writeError(w, http.StatusConflict, CodeConflict,
				"this quarantine is already closed",
				map[string]any{"quarantine_id": id, "closed_at": closedAt})
			return
		}
		s.fail(w, r, "close quarantine", err)
		return
	}

	detail := map[string]any{
		"quarantine_id": id,
		"scope":         scope,
		"opened_for":    openedFor,
		"opened_at":     openedAt,
		"device_id":     deviceID,
		"slot_id":       slotID,
		"hub_id":        hubID,
		"host_id":       hostID,
	}
	// The quarantine is already closed and the device is already schedulable
	// again; the claim that somebody fixed it must be recorded regardless of
	// what the caller's connection does next.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, "quarantine.close", fmt.Sprintf("quarantine:%d", id), reason, detail)
	s.recordEvent(bookCtx, eventRow{
		Kind: "quarantine_closed", DeviceID: deviceID, SlotID: slotID, Actor: who, Detail: detail,
	})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues("quarantine.close", "ok").Inc()

	writeJSON(w, http.StatusOK, map[string]any{
		"quarantine_id": id,
		"closed":        true,
		"scope":         scope,
		"opened_for":    openedFor,
	})
}

// ---------------------------------------------------------------------------
// Bulk execution
// ---------------------------------------------------------------------------

type bulkSelector struct {
	Pool      string   `json:"pool,omitempty"`
	Host      string   `json:"host,omitempty"`
	Hub       string   `json:"hub,omitempty"`
	Health    string   `json:"health,omitempty"`
	Model     string   `json:"model,omitempty"`
	DeviceIDs []string `json:"device_ids,omitempty"`
	// IncludeLeased runs the command on devices that hold a live lease. Off by
	// default: a bulk shell across the farm that lands inside somebody's
	// six-hour run is the most expensive mistake this endpoint can make.
	IncludeLeased bool `json:"include_leased,omitempty"`
}

type bulkCreateRequest struct {
	Selector  bulkSelector `json:"selector"`
	Command   string       `json:"command"`
	MaxPerHub int          `json:"max_per_hub,omitempty"`
	TimeoutMS int          `json:"timeout_ms,omitempty"`
	Reason    string       `json:"reason,omitempty"`
}

type bulkTarget struct {
	DeviceID string
	HubID    *int64
	Endpoint string
	Devpath  string
	LeaseID  *string
	JobID    *string
}

// handleBulkCreate serves POST /api/v1/bulk.
//
// max_per_hub is not a politeness knob. Sixty devices answering one command at
// once browns out a power domain and produces a wave of transport errors that
// looks exactly like a hardware fault; capping concurrency per hub keeps a
// bulk command from manufacturing the incident it was run to investigate.
func (s *Server) handleBulkCreate(w http.ResponseWriter, r *http.Request) {
	var req bulkCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		badRequest(w, "command is required", nil)
		return
	}
	maxPerHub := req.MaxPerHub
	if maxPerHub <= 0 {
		maxPerHub = 4
	}
	if maxPerHub > 32 {
		maxPerHub = 32
	}
	timeout := defaultExecTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout > maxExecTimeout {
			timeout = maxExecTimeout
		}
	}
	who := actor(r.Context())

	targets, skipped, err := s.expandBulkSelector(r.Context(), req.Selector)
	if err != nil {
		s.fail(w, r, "bulk: expand selector", err)
		return
	}
	if len(targets) == 0 && len(skipped) == 0 {
		badRequest(w, "the selector matched no addressable devices", nil)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.fail(w, r, "bulk: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.Context())) }()

	var runID string
	if err := tx.QueryRow(r.Context(), `
INSERT INTO farm.bulk_runs (created_by, selector, command, max_per_hub, timeout)
VALUES ($1, $2::jsonb, $3, $4, make_interval(secs => $5::bigint))
RETURNING id::text`,
		who, mustJSON(req.Selector), req.Command, maxPerHub, int64(timeout.Seconds())).Scan(&runID); err != nil {
		s.fail(w, r, "bulk: create run", err)
		return
	}

	// Both inserts are ONE statement each, not one per device. A selector can
	// match a thousand devices, and a thousand sequential round trips would
	// hold a pool connection — and an open transaction — for as long as they
	// took. That connection is one a renewal cannot borrow, and a holder that
	// cannot renew for ttl+grace loses its device: an operator's bulk command
	// must never be able to squeeze the renewal path.
	if len(targets) > 0 {
		devIDs, hubIDs := targetArrays(targets)
		if _, err := tx.Exec(r.Context(), `
INSERT INTO farm.bulk_targets (run_id, device_id, hub_id)
SELECT $1::uuid, t.device_id::uuid, t.hub_id
  FROM unnest($2::text[], $3::bigint[]) AS t(device_id, hub_id)`,
			runID, devIDs, hubIDs); err != nil {
			s.fail(w, r, "bulk: create targets", err)
			return
		}
	}
	if len(skipped) > 0 {
		// Recorded as targets in state 'skipped' rather than dropped, so the
		// run says out loud which devices it declined to touch and why.
		devIDs, hubIDs := targetArrays(skipped)
		notes := make([]string, len(skipped))
		for i, t := range skipped {
			notes[i] = "device holds a live lease"
			if t.LeaseID != nil {
				notes[i] += " " + *t.LeaseID
			}
		}
		if _, err := tx.Exec(r.Context(), `
INSERT INTO farm.bulk_targets (run_id, device_id, hub_id, state, finished_at, error)
SELECT $1::uuid, t.device_id::uuid, t.hub_id, 'skipped', now(), t.note
  FROM unnest($2::text[], $3::bigint[], $4::text[]) AS t(device_id, hub_id, note)`,
			runID, devIDs, hubIDs, notes); err != nil {
			s.fail(w, r, "bulk: create skipped targets", err)
			return
		}
		s.metrics.bulkTargets.WithLabelValues("skipped").Add(float64(len(skipped)))
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, r, "bulk: commit", err)
		return
	}

	truncated := len(targets)+len(skipped) >= bulkSelectorLimit
	detail := map[string]any{
		"run_id":      runID,
		"command":     req.Command,
		"targets":     len(targets),
		"skipped":     len(skipped),
		"truncated":   truncated,
		"max_per_hub": maxPerHub,
		"timeout_ms":  timeout.Milliseconds(),
	}
	// The run row is committed and its goroutines are about to touch real
	// devices, so the audit row is written on a context that survives the
	// caller.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, "bulk.create", "bulk:"+runID, strings.TrimSpace(req.Reason), detail)
	bookCancel()
	s.metrics.operatorActions.WithLabelValues("bulk.create", "ok").Inc()

	s.startBulkRun(runID, req.Command, timeout, maxPerHub, targets)

	w.Header().Set("Location", "/api/v1/bulk/"+runID)
	body := map[string]any{
		"run_id":      runID,
		"state":       "running",
		"targets":     len(targets),
		"skipped":     len(skipped),
		"truncated":   truncated,
		"max_per_hub": maxPerHub,
		"timeout_ms":  timeout.Milliseconds(),
	}
	if truncated {
		body["note"] = fmt.Sprintf("the selector matched at least %d devices and was cut at that "+
			"limit; devices beyond it were NOT addressed by this run", bulkSelectorLimit)
	}
	writeJSON(w, http.StatusAccepted, body)
}

// targetArrays projects targets into the parallel arrays the batched inserts
// unnest. hub_id is nullable — a device that is not in a slot has no hub — so
// it travels as []*int64 and keeps its NULLs.
func targetArrays(ts []bulkTarget) ([]string, []*int64) {
	devIDs := make([]string, len(ts))
	hubIDs := make([]*int64, len(ts))
	for i, t := range ts {
		devIDs[i] = t.DeviceID
		hubIDs[i] = t.HubID
	}
	return devIDs, hubIDs
}

// bulkSelectorLimit caps how many devices one run may address. A selector that
// hits the cap is reported as truncated rather than quietly shortened: an
// operator who believes a command reached the whole fleet, when it reached the
// first thousand rows, will draw a conclusion about the devices it never
// touched.
const bulkSelectorLimit = 1000

// expandBulkSelector resolves a selector into addressable targets, separating
// out the devices that hold a live lease.
func (s *Server) expandBulkSelector(ctx context.Context, sel bulkSelector) (targets, skipped []bulkTarget, err error) {
	var (
		conds = []string{
			"f.adb_devpath IS NOT NULL",
			"f.adb_endpoint IS NOT NULL",
			"f.admin_state = 'enabled'",
		}
		args []any
	)
	add := func(format, value string) {
		args = append(args, value)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}
	if sel.Pool != "" {
		add("f.pool_id = $%d", sel.Pool)
	}
	if sel.Host != "" {
		add("f.host_id = $%d", sel.Host)
	}
	if sel.Hub != "" {
		args = append(args, sel.Hub)
		conds = append(conds, fmt.Sprintf("(f.hub_path = $%d OR f.hub_id::text = $%d)", len(args), len(args)))
	}
	if sel.Health != "" {
		add("f.health = $%d", sel.Health)
	}
	if sel.Model != "" {
		add("f.model ILIKE '%%' || $%d || '%%'", sel.Model)
	}
	if len(sel.DeviceIDs) > 0 {
		args = append(args, sel.DeviceIDs)
		conds = append(conds, fmt.Sprintf("(f.device_id::text = ANY($%d) OR f.farm_uid = ANY($%d))",
			len(args), len(args)))
	}

	query := fmt.Sprintf(`
SELECT f.device_id::text, f.hub_id, f.adb_endpoint, f.adb_devpath, f.lease_id::text, f.job_id::text
  FROM farm.v_fleet f
 WHERE %s
 ORDER BY f.host_id, f.hub_path, f.rack_slot
 LIMIT %d`, strings.Join(conds, "\n   AND "), bulkSelectorLimit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var t bulkTarget
		if err := rows.Scan(&t.DeviceID, &t.HubID, &t.Endpoint, &t.Devpath, &t.LeaseID, &t.JobID); err != nil {
			return nil, nil, err
		}
		if t.LeaseID != nil && !sel.IncludeLeased {
			skipped = append(skipped, t)
			continue
		}
		targets = append(targets, t)
	}
	return targets, skipped, rows.Err()
}

// startBulkRun executes a run in the background.
//
// It hangs off the server's background context, not the request's: an operator
// closing their laptop must not abort a farm-wide command halfway through, and
// the run's own bookkeeping has to survive the HTTP response. Shutdown asks it
// to stop and waits, bounded by the shutdown grace.
func (s *Server) startBulkRun(runID, command string, timeout time.Duration, maxPerHub int, targets []bulkTarget) {
	if len(targets) == 0 {
		s.finishBulkRun(s.bgCtx, runID, false)
		return
	}

	byHub := map[int64][]bulkTarget{}
	for _, t := range targets {
		key := int64(-1)
		if t.HubID != nil {
			key = *t.HubID
		}
		byHub[key] = append(byHub[key], t)
	}

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx := s.bgCtx

		// A global ceiling on top of the per-hub cap: forty hubs times four
		// each is a hundred and sixty simultaneous ADB sessions, which is a
		// load test of our own control plane rather than a bulk command.
		global := make(chan struct{}, 64)
		var wg sync.WaitGroup

		for _, hubTargets := range byHub {
			wg.Add(1)
			go func(list []bulkTarget) {
				defer wg.Done()
				perHub := make(chan struct{}, maxPerHub)
				var hubWG sync.WaitGroup
				for _, t := range list {
					if ctx.Err() != nil {
						// break, never return: returning here would skip
						// hubWG.Wait() and orphan the target goroutines already
						// in flight, so Shutdown would stop waiting while they
						// were still driving devices and writing rows through a
						// pool the parent is about to close.
						break
					}
					hubWG.Add(1)
					go func(t bulkTarget) {
						defer hubWG.Done()
						select {
						case perHub <- struct{}{}:
						case <-ctx.Done():
							return
						}
						defer func() { <-perHub }()
						select {
						case global <- struct{}{}:
						case <-ctx.Done():
							return
						}
						defer func() { <-global }()
						s.execBulkTarget(ctx, runID, t, command, timeout)
					}(t)
				}
				hubWG.Wait()
			}(hubTargets)
		}
		wg.Wait()

		s.finishBulkRun(ctx, runID, ctx.Err() != nil)
	}()
}

func (s *Server) execBulkTarget(ctx context.Context, runID string, t bulkTarget, command string, timeout time.Duration) {
	// Bookkeeping is written on a context that survives shutdown, so a run
	// interrupted mid-command still records what it did rather than leaving a
	// row in 'running' forever.
	book := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}

	markCtx, cancelMark := book()
	_, err := s.pool.Exec(markCtx, `
UPDATE farm.bulk_targets SET state = 'running', started_at = now()
 WHERE run_id = $1::uuid AND device_id = $2::uuid AND state = 'pending'`, runID, t.DeviceID)
	cancelMark()
	if err != nil {
		s.log.WarnContext(ctx, "bulk: could not mark target running",
			"run_id", runID, "device_id", t.DeviceID, "err", err)
	}

	runCtx, cancelRun := context.WithTimeout(ctx, timeout)
	exec := s.newExecutor(t.Endpoint, timeout, s.execMaxOutput)
	res, execErr := exec.Shell(runCtx, t.Devpath, command)
	cancelRun()

	var (
		state    = "ok"
		exitCode *int
		output   string
		errText  *string
	)
	switch {
	case execErr != nil:
		state = "error"
		msg := execErr.Error()
		if te, ok := adbwire.AsTransport(execErr); ok {
			msg = te.Kind.String() + ": " + msg
		}
		errText = &msg
		if res != nil {
			output = string(res.Stdout) + string(res.Stderr)
		}
	case res == nil:
		// An Executor that returns neither a result nor an error is broken.
		// This runs on a background goroutine with no recoverer above it, so a
		// nil dereference here would take the whole control plane down — and
		// with it every renewal in flight — over one misbehaving ADB client.
		state = "error"
		msg := "the executor returned no result and no error"
		errText = &msg
	default:
		code := res.ExitCode
		exitCode = &code
		output = string(res.Stdout)
		if len(res.Stderr) > 0 {
			output += string(res.Stderr)
		}
		if code != 0 {
			state = "error"
		}
	}

	finCtx, cancelFin := book()
	defer cancelFin()
	if _, err := s.pool.Exec(finCtx, `
UPDATE farm.bulk_targets
   SET state = $3, finished_at = now(), exit_code = $4, output = $5, error = $6
 WHERE run_id = $1::uuid AND device_id = $2::uuid`,
		runID, t.DeviceID, state, exitCode, output, errText); err != nil {
		s.log.WarnContext(ctx, "bulk: could not record target result",
			"run_id", runID, "device_id", t.DeviceID, "err", err)
	}
	s.metrics.bulkTargets.WithLabelValues(state).Inc()
}

func (s *Server) finishBulkRun(ctx context.Context, runID string, aborted bool) {
	finCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	state := "done"
	if aborted {
		state = "cancelled"
		if _, err := s.pool.Exec(finCtx, `
UPDATE farm.bulk_targets
   SET state = 'skipped', finished_at = now(),
       error = 'the control plane shut down before this target ran'
 WHERE run_id = $1::uuid AND state IN ('pending','running')`, runID); err != nil {
			s.log.WarnContext(ctx, "bulk: could not close outstanding targets",
				"run_id", runID, "err", err)
		}
	}
	if _, err := s.pool.Exec(finCtx, `
UPDATE farm.bulk_runs SET state = $2, finished_at = now() WHERE id = $1::uuid AND state = 'running'`,
		runID, state); err != nil {
		s.log.WarnContext(ctx, "bulk: could not finish run", "run_id", runID, "err", err)
	}
}

type bulkRunView struct {
	ID         string          `json:"id"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	Selector   json.RawMessage `json:"selector,omitempty"`
	Command    string          `json:"command"`
	MaxPerHub  int             `json:"max_per_hub"`
	TimeoutS   int64           `json:"timeout_s"`
	State      string          `json:"state"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Targets    int             `json:"targets"`
	Pending    int             `json:"pending"`
	Running    int             `json:"running"`
	OK         int             `json:"ok"`
	Errors     int             `json:"errors"`
	Skipped    int             `json:"skipped"`
}

const bulkRunColumns = `
  r.id::text, r.created_by, r.created_at, r.selector, r.command, r.max_per_hub,
  EXTRACT(EPOCH FROM r.timeout)::bigint, r.state, r.finished_at,
  count(t.device_id),
  count(*) FILTER (WHERE t.state = 'pending'),
  count(*) FILTER (WHERE t.state = 'running'),
  count(*) FILTER (WHERE t.state = 'ok'),
  count(*) FILTER (WHERE t.state = 'error'),
  count(*) FILTER (WHERE t.state = 'skipped')`

func scanBulkRun(sc scanner) (bulkRunView, error) {
	var (
		v        bulkRunView
		selector []byte
	)
	err := sc.Scan(&v.ID, &v.CreatedBy, &v.CreatedAt, &selector, &v.Command, &v.MaxPerHub,
		&v.TimeoutS, &v.State, &v.FinishedAt,
		&v.Targets, &v.Pending, &v.Running, &v.OK, &v.Errors, &v.Skipped)
	if err != nil {
		return bulkRunView{}, err
	}
	if len(selector) > 0 {
		v.Selector = json.RawMessage(selector)
	}
	return v, nil
}

// handleBulkList serves GET /api/v1/bulk.
func (s *Server) handleBulkList(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 1, 500)

	query := fmt.Sprintf(`
SELECT %s
  FROM farm.bulk_runs r
  LEFT JOIN farm.bulk_targets t ON t.run_id = r.id
 GROUP BY r.id
 ORDER BY r.created_at DESC
 LIMIT $1`, bulkRunColumns)

	rows, err := s.pool.Query(r.Context(), query, limit)
	if err != nil {
		s.fail(w, r, "list bulk runs", err)
		return
	}
	defer rows.Close()

	out := make([]bulkRunView, 0, 16)
	for rows.Next() {
		v, err := scanBulkRun(rows)
		if err != nil {
			s.fail(w, r, "scan bulk run", err)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read bulk runs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// handleBulkGet serves GET /api/v1/bulk/{id}: the run and every target result.
func (s *Server) handleBulkGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		badRequest(w, "bulk run id must be a uuid", nil)
		return
	}

	query := fmt.Sprintf(`
SELECT %s
  FROM farm.bulk_runs r
  LEFT JOIN farm.bulk_targets t ON t.run_id = r.id
 WHERE r.id = $1::uuid
 GROUP BY r.id`, bulkRunColumns)

	run, err := scanBulkRun(s.pool.QueryRow(r.Context(), query, id))
	if err != nil {
		s.fail(w, r, "get bulk run", err)
		return
	}

	const targetQuery = `
SELECT t.device_id::text, d.farm_uid, s.rack_slot, d.host_id, t.hub_id, t.state,
       t.started_at, t.finished_at, t.exit_code, t.output, t.error
  FROM farm.bulk_targets t
  JOIN farm.devices d ON d.id = t.device_id
  LEFT JOIN farm.slots s ON s.id = d.current_slot_id
 WHERE t.run_id = $1::uuid
 ORDER BY d.host_id, s.rack_slot NULLS LAST, d.farm_uid`

	rows, err := s.pool.Query(r.Context(), targetQuery, id)
	if err != nil {
		s.fail(w, r, "get bulk targets", err)
		return
	}
	defer rows.Close()

	type targetRow struct {
		DeviceID   string     `json:"device_id"`
		FarmUID    string     `json:"farm_uid"`
		RackSlot   *string    `json:"rack_slot,omitempty"`
		HostID     *string    `json:"host_id,omitempty"`
		HubID      *int64     `json:"hub_id,omitempty"`
		State      string     `json:"state"`
		StartedAt  *time.Time `json:"started_at,omitempty"`
		FinishedAt *time.Time `json:"finished_at,omitempty"`
		ExitCode   *int       `json:"exit_code,omitempty"`
		Output     *string    `json:"output,omitempty"`
		Error      *string    `json:"error,omitempty"`
	}
	targets := make([]targetRow, 0, 32)
	for rows.Next() {
		var t targetRow
		if err := rows.Scan(&t.DeviceID, &t.FarmUID, &t.RackSlot, &t.HostID, &t.HubID, &t.State,
			&t.StartedAt, &t.FinishedAt, &t.ExitCode, &t.Output, &t.Error); err != nil {
			s.fail(w, r, "scan bulk target", err)
			return
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read bulk targets", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"run": run, "targets": targets})
}

// ---------------------------------------------------------------------------
// GET /api/v1/events
// ---------------------------------------------------------------------------

// handleEvents serves GET /api/v1/events?limit=: farm.events and
// farm.audit_log interleaved.
//
// They are merged rather than offered separately because the question being
// asked is always chronological — "what happened to this device before it went
// bad" — and answering it from two lists means an operator manually zipping
// timestamps at 3am.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// This endpoint was farm-wide with no tenant scoping while tenantScope was
	// applied in eight other places: any tenant could read every other
	// tenant's job and device history. EventScope carries that scoping plus
	// the filters (kind, since, subject) the handler never had.
	scope, err := EventScopeFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
		return
	}
	scope.Tenant = tenantScope(r.Context())
	limit := scope.PageSize()

	q, args := scope.Query()
	rows, err := s.pool.Query(r.Context(), q, args...)
	if err != nil {
		s.fail(w, r, "list events", err)
		return
	}
	defer rows.Close()

	type entry struct {
		At       time.Time       `json:"at"`
		Source   string          `json:"source"`
		Action   string          `json:"action"`
		Actor    string          `json:"actor,omitempty"`
		DeviceID string          `json:"device_id,omitempty"`
		SlotID   *int64          `json:"slot_id,omitempty"`
		LeaseID  string          `json:"lease_id,omitempty"`
		JobID    string          `json:"job_id,omitempty"`
		Subject  string          `json:"subject,omitempty"`
		Reason   string          `json:"reason,omitempty"`
		Detail   json.RawMessage `json:"detail,omitempty"`
	}
	out := make([]entry, 0, 64)
	for rows.Next() {
		var (
			e      entry
			detail []byte
		)
		if err := rows.Scan(&e.At, &e.Source, &e.Action, &e.Actor, &e.DeviceID, &e.SlotID,
			&e.LeaseID, &e.JobID, &e.Subject, &e.Reason, &detail); err != nil {
			s.fail(w, r, "scan event", err)
			return
		}
		if len(detail) > 0 {
			e.Detail = json.RawMessage(detail)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":    out,
		"truncated": len(out) == limit,
		// What the reader is actually looking at. A scoped list that does not
		// say it is scoped reads as the whole farm.
		"scope": scope.Describe(),
	})
}
