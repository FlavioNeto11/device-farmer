package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// leaseView is one row of farm.leases, joined with just enough of the device
// and slot to be legible in an alert: a page at 3am needs a rack position, not
// a uuid.
//
// The two "*_in_s" fields are computed by Postgres against now(), never here.
// Every deadline in this system belongs to the database clock; a countdown
// computed from a client's wall clock would be a second source of truth about
// when a lease ends, and the whole design has exactly one.
type leaseView struct {
	ID               string     `json:"id"`
	Fence            int64      `json:"fence"`
	State            string     `json:"state"`
	DeviceID         string     `json:"device_id"`
	FarmUID          string     `json:"farm_uid"`
	SlotID           *int64     `json:"slot_id,omitempty"`
	RackSlot         *string    `json:"rack_slot,omitempty"`
	HostID           *string    `json:"host_id,omitempty"`
	JobID            string     `json:"job_id"`
	TenantID         string     `json:"tenant_id"`
	QueueID          string     `json:"queue_id"`
	Holder           string     `json:"holder"`
	HolderInstance   string     `json:"holder_instance"`
	HolderEpoch      int        `json:"holder_epoch"`
	Protected        bool       `json:"protected"`
	DisruptionPolicy string     `json:"disruption_policy"`
	TTLSeconds       int64      `json:"ttl_s"`
	GraceSeconds     int64      `json:"grace_s"`
	AcquiredAt       time.Time  `json:"acquired_at"`
	HeartbeatAt      time.Time  `json:"heartbeat_at"`
	HeartbeatSeq     int64      `json:"heartbeat_seq"`
	ExpiresAt        time.Time  `json:"expires_at"`
	ReclaimableAt    time.Time  `json:"reclaimable_at"`
	ExpiresInS       int64      `json:"expires_in_s"`
	ReclaimableInS   int64      `json:"reclaimable_in_s"`
	WitnessAt        *time.Time `json:"witness_at,omitempty"`
	WitnessExts      int        `json:"witness_extensions"`
	ReleasedAt       *time.Time `json:"released_at,omitempty"`
	ReleaseReason    *string    `json:"release_reason,omitempty"`
}

const leaseColumns = `
  l.id::text, l.fence, l.state, l.device_id::text, d.farm_uid, l.slot_id, s.rack_slot, d.host_id,
  l.job_id::text, l.tenant_id, l.queue_id, l.holder, l.holder_instance::text, l.holder_epoch,
  l.protected, l.disruption_policy,
  EXTRACT(EPOCH FROM l.ttl)::bigint, EXTRACT(EPOCH FROM l.grace)::bigint,
  l.acquired_at, l.heartbeat_at, l.heartbeat_seq, l.expires_at, l.reclaimable_at,
  EXTRACT(EPOCH FROM (l.expires_at - now()))::bigint,
  EXTRACT(EPOCH FROM (l.reclaimable_at - now()))::bigint,
  l.witness_at, l.witness_extensions, l.released_at, l.release_reason`

const leaseFrom = `
  FROM farm.leases l
  JOIN farm.devices d ON d.id = l.device_id
  LEFT JOIN farm.slots s ON s.id = l.slot_id`

func scanLease(sc scanner) (leaseView, error) {
	var v leaseView
	err := sc.Scan(
		&v.ID, &v.Fence, &v.State, &v.DeviceID, &v.FarmUID, &v.SlotID, &v.RackSlot, &v.HostID,
		&v.JobID, &v.TenantID, &v.QueueID, &v.Holder, &v.HolderInstance, &v.HolderEpoch,
		&v.Protected, &v.DisruptionPolicy, &v.TTLSeconds, &v.GraceSeconds,
		&v.AcquiredAt, &v.HeartbeatAt, &v.HeartbeatSeq, &v.ExpiresAt, &v.ReclaimableAt,
		&v.ExpiresInS, &v.ReclaimableInS,
		&v.WitnessAt, &v.WitnessExts, &v.ReleasedAt, &v.ReleaseReason,
	)
	return v, err
}

// tenantScope returns the tenant a caller is confined to, or "" for an
// operator-wide view. A tenant-scoped token sees only its own work.
func tenantScope(ctx context.Context) string {
	id, ok := IdentityFrom(ctx)
	if !ok {
		return ""
	}
	if id.Role.AtLeast(RoleOperator) {
		return ""
	}
	return id.Tenant
}

// handleLeaseList serves GET /api/v1/leases?state=.
//
// The default is the live set (held plus suspect) because that is the question
// an operator is actually asking. Note that a suspect lease is still live: it
// has been marked for alerting and NOTHING has been released. A heartbeat
// anywhere in the grace band moves it back to held at the same fence.
func (s *Server) handleLeaseList(w http.ResponseWriter, r *http.Request) {
	state := queryString(r, "state")
	limit := queryInt(r, "limit", 200, 1, 2000)

	var (
		conds = []string{}
		args  []any
	)
	switch state {
	case "", "live":
		conds = append(conds, "l.state IN ('held','suspect')")
	case "all":
	case "held", "suspect", "released", "expired":
		args = append(args, state)
		conds = append(conds, fmt.Sprintf("l.state = $%d", len(args)))
	default:
		badRequest(w, "state must be one of live, all, held, suspect, released, expired", nil)
		return
	}
	if tenant := tenantScope(r.Context()); tenant != "" {
		args = append(args, tenant)
		conds = append(conds, fmt.Sprintf("l.tenant_id = $%d", len(args)))
	}
	if dev := queryString(r, "device"); dev != "" {
		args = append(args, dev)
		conds = append(conds, fmt.Sprintf("(l.device_id::text = $%d OR d.farm_uid = $%d)", len(args), len(args)))
	}
	if job := queryString(r, "job"); job != "" {
		args = append(args, job)
		conds = append(conds, fmt.Sprintf("l.job_id::text = $%d", len(args)))
	}
	if host := queryString(r, "host"); host != "" {
		args = append(args, host)
		conds = append(conds, fmt.Sprintf("d.host_id = $%d", len(args)))
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	query := fmt.Sprintf("SELECT %s %s %s ORDER BY l.acquired_at DESC LIMIT $%d",
		leaseColumns, leaseFrom, where, len(args))

	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.fail(w, r, "list leases", err)
		return
	}
	defer rows.Close()

	out := make([]leaseView, 0, 64)
	counts := map[string]int{}
	protectedSuspect := 0
	for rows.Next() {
		v, err := scanLease(rows)
		if err != nil {
			s.fail(w, r, "scan lease", err)
			return
		}
		counts[v.State]++
		if v.State == "suspect" && v.Protected {
			protectedSuspect++
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read leases", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"leases": out,
		"counts": counts,
		// A protected suspect lease will never be reclaimed automatically: the
		// reaper skips it and a human is expected to look. Surfacing the count
		// here is what makes "hold and page" visible instead of silent.
		"protected_suspect": protectedSuspect,
		"truncated":         len(out) == limit,
	})
}

// ---------------------------------------------------------------------------
// acquire
// ---------------------------------------------------------------------------

type acquireRequest struct {
	JobID          string `json:"job_id"`
	Holder         string `json:"holder"`
	HolderInstance string `json:"holder_instance,omitempty"`
}

// handleLeaseAcquire serves POST /api/v1/leases/acquire.
//
// Acquire is idempotent on job_id, and that is load bearing: a pod eviction —
// node drain, preemption, spot reclaim, cluster upgrade, OOM restart — is the
// most ordinary event in a Kubernetes control plane and must never cost a
// device. The replacement calls this with the same job_id and receives the same
// lease, the same device and the SAME FENCE, with "reattached": true. Its own
// work may still be running detached on the phone, so bumping the fence would
// fence the job out of its own process.
//
// Zero rows is 409 "no_capacity" — an ordinary scheduling outcome. Re-queue.
func (s *Server) handleLeaseAcquire(w http.ResponseWriter, r *http.Request) {
	var req acquireRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	req.JobID = strings.TrimSpace(req.JobID)
	req.Holder = strings.TrimSpace(req.Holder)
	req.HolderInstance = strings.TrimSpace(req.HolderInstance)

	if !looksLikeUUID(req.JobID) {
		badRequest(w, "job_id must be a uuid", nil)
		return
	}
	if req.Holder == "" {
		badRequest(w, "holder is required (the pod or process name; audit only, it confers no ownership)", nil)
		return
	}

	// holder_instance is what farm.lease_renew matches on. Minting one here
	// when the caller omitted it is safe only because the value is returned in
	// the response: the caller must renew with exactly this instance, and a
	// re-minted one would fence the caller out of its own lease.
	minted := false
	if req.HolderInstance == "" {
		id, err := lease.NewHolderInstance()
		if err != nil {
			s.fail(w, r, "mint holder instance", err)
			return
		}
		req.HolderInstance = id
		minted = true
	} else if !looksLikeUUID(req.HolderInstance) {
		badRequest(w, "holder_instance must be a uuid", nil)
		return
	}

	// A tenant-scoped caller may only acquire against its own job. The check is
	// a plain read; the allocation decision itself stays inside
	// farm.lease_acquire where the row locks and partial unique indexes are.
	if tenant := tenantScope(r.Context()); tenant != "" {
		var owner string
		err := s.pool.QueryRow(r.Context(),
			`SELECT tenant_id FROM farm.jobs WHERE id = $1::uuid`, req.JobID).Scan(&owner)
		if err != nil {
			s.fail(w, r, "acquire: check job tenant", err)
			return
		}
		if owner != tenant {
			writeError(w, http.StatusForbidden, CodeForbidden,
				"this job belongs to another tenant", nil)
			return
		}
	}

	// The AUTHENTICATED identity, not anything from the body, is what
	// farm.lease_acquire matches a re-attach against.
	//
	// Re-attach is idempotent on job_id and a job id is not a secret — this
	// endpoint's own list route, GET /api/v1/fleet and the event stream all
	// publish it — so without this, any caller who read a job id could take a
	// live lease, become its holder, and receive the fence that authorises
	// writes to the handset. The displaced holder's next renew would match
	// nothing and be reported to it as an ordinary fencing, so it would abort a
	// six-hour run believing the system had done it correctly.
	//
	// Subject rather than Holder, because Holder is a pod name and a pod name
	// is exactly what changes when a pod is evicted. The credential belongs to
	// the workload: the replacement pod mounts the same token and authenticates
	// as the same subject, so the eviction case still re-attaches at the same
	// fence while a stranger is refused. See migrations/00010_reattach_auth.sql.
	caller := lease.Caller{Tenant: tenantScope(r.Context())}
	if id, ok := IdentityFrom(r.Context()); ok {
		// An empty subject would be sent as SQL NULL and read by
		// farm.lease_acquire as "this call came from inside the control
		// plane", switching the gate off for every request. Authenticator is
		// the OIDC seam and a deployment-supplied verifier that maps no
		// subject claim is a real possibility, so refuse rather than
		// silently downgrade: an authenticated request with no subject is a
		// broken authenticator, not an anonymous one.
		if strings.TrimSpace(id.Subject) == "" {
			s.log.ErrorContext(r.Context(), "authenticator returned an identity with no subject",
				"authenticator", s.auth.Name(), "path", r.URL.Path)
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"this deployment's authenticator produced no subject, so a lease cannot be "+
					"bound to a caller. Acquire is refused rather than granted unattributed.", nil)
			return
		}
		caller.Principal = id.Subject
	}

	res, err := s.leases.AcquireAs(r.Context(), req.JobID, req.Holder, req.HolderInstance, caller)
	if err != nil {
		if errors.Is(err, lease.ErrNotPermitted) {
			// 403 and not 409: this is "I know who you are and you may not have
			// this lease", which a client must never retry. Logged with the
			// actor because an attempted takeover is worth as much to an
			// incident review as a successful one — and the database cannot
			// record the attempt itself, since the refusal rolls back with the
			// transaction that would have written the ledger row.
			s.log.WarnContext(r.Context(), "refused a lease re-attach",
				"job_id", req.JobID, "actor", actor(r.Context()), "holder", req.Holder)
			writeError(w, http.StatusForbidden, CodeForbidden,
				"you are not authorised for this job's lease. A re-attach must come from the "+
					"principal that acquired it; taking a device from a live holder is the "+
					"operator revoke, which is audited.",
				map[string]any{"job_id": req.JobID, "terminal": true})
			return
		}
		s.fail(w, r, "acquire lease", err)
		return
	}

	body := map[string]any{
		"lease_id":        res.Lease.ID,
		"device_id":       res.Lease.DeviceID,
		"slot_id":         res.Lease.SlotID,
		"job_id":          res.Lease.JobID,
		"fence":           res.Lease.Fence,
		"holder":          res.Lease.Holder,
		"holder_instance": res.Lease.HolderInstance,
		"expires_at":      res.Lease.ExpiresAt,
		"reclaimable_at":  res.Lease.ReclaimableAt,
		"reattached":      res.Reattached,
	}
	if res.Reattached {
		body["note"] = "re-attached to an existing lease at the same fence; the device may still " +
			"carry this job's own prior state, so resume from the job checkpoint rather than starting over"
	}
	if minted {
		body["holder_instance_minted"] = true
	}
	writeJSON(w, http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// renew — the hot path
// ---------------------------------------------------------------------------

type renewRequest struct {
	Fence          int64  `json:"fence"`
	HolderInstance string `json:"holder_instance"`
}

// handleLeaseRenew serves POST /api/v1/leases/{id}/renew.
//
// THE TWO OUTCOMES BELOW MUST NEVER BE CONFLATED.
//
//	410 Gone, code "fenced"       farm.lease_renew returned ZERO ROWS. The lease
//	                              is gone: released, reclaimed, or re-attached
//	                              by another instance. Terminal. Abort the job,
//	                              close every ADB socket, write nothing further.
//	                              Retrying cannot help and would be an attempt
//	                              to drive a device that belongs to another job.
//
//	503, code "transient"         The database did not answer: a dial failure, a
//	                              statement timeout, a failover, an exhausted
//	                              pool. This proves NOTHING about the lease. The
//	                              lease is untouched, no deadline moved, and the
//	                              holder must retry with backoff. Reporting this
//	                              as a fence is DeviceFarmer/STF #663 with a
//	                              different trigger: an infrastructure blip
//	                              destroying a multi-hour run.
//
// Both branches are counted, with the same distinction, in
// farm_lease_renew_failures_total{kind=...}.
func (s *Server) handleLeaseRenew(w http.ResponseWriter, r *http.Request) {
	leaseID := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(leaseID) {
		badRequest(w, "lease id must be a uuid", nil)
		return
	}

	var req renewRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	if req.Fence <= 0 {
		badRequest(w, "fence is required and must be the fence this lease was granted at", nil)
		return
	}
	if !looksLikeUUID(strings.TrimSpace(req.HolderInstance)) {
		badRequest(w, "holder_instance must be the uuid this holder acquired with", nil)
		return
	}

	res, err := s.leases.Renew(r.Context(), leaseID, req.Fence, strings.TrimSpace(req.HolderInstance))
	switch {
	case err == nil:
		if res.WasSuspect {
			// Self-healed. The lease never left its holder and no work was
			// lost; it is an alerting signal about the renewal path, not an
			// incident about the device.
			s.log.InfoContext(r.Context(), "renewal self-healed a suspect lease",
				"lease_id", leaseID, "fence", req.Fence)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"lease_id":       leaseID,
			"fence":          req.Fence,
			"expires_at":     res.ExpiresAt,
			"reclaimable_at": res.ReclaimableAt,
			"was_suspect":    res.WasSuspect,
		})

	case errors.Is(err, lease.ErrFenced):
		obs.LeaseRenewFailure(obs.KindFenced)
		s.log.WarnContext(r.Context(), "renewal fenced",
			"lease_id", leaseID, "fence", req.Fence, "actor", actor(r.Context()))
		writeError(w, http.StatusGone, CodeFenced,
			"this lease is no longer yours: abort the job, close every ADB socket, and write "+
				"nothing further to the device. Do not retry — the device now belongs to "+
				"another job.",
			map[string]any{"lease_id": leaseID, "presented_fence": req.Fence, "terminal": true})

	case r.Context().Err() != nil:
		// The caller hung up mid-renewal. Nothing was decided, and this is not
		// a renewal failure: counting it as one would put a fake spike in the
		// metric that pages a human. It is not a success either, so the request
		// is recorded as client-closed rather than as the 200 the recorder
		// starts at.
		noteClientGone(w)
		s.log.DebugContext(r.Context(), "renewal abandoned by caller", "lease_id", leaseID)

	default:
		// Everything that is not zero rows is the database being briefly
		// unavailable. The lease keeps its deadline; the caller retries.
		obs.LeaseRenewFailure(obs.KindTransient)
		s.log.ErrorContext(r.Context(), "renewal could not reach the database",
			"lease_id", leaseID, "err", err)
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusServiceUnavailable, CodeTransient,
			"the renewal could not be completed because the database did not answer. Your lease "+
				"is UNTOUCHED and no deadline moved: retry with backoff, and do not abort the job.",
			map[string]any{"lease_id": leaseID, "terminal": false, "retry": true})
	}
}

// ---------------------------------------------------------------------------
// release
// ---------------------------------------------------------------------------

type releaseRequest struct {
	Fence  int64  `json:"fence"`
	Reason string `json:"reason"`
	// RearmMS overrides the slot quarantine after release. It must exceed the
	// node proxy's self-fence timeout, which is asserted at startup by
	// internal/config; omitting it uses the configured value.
	RearmMS int64 `json:"rearm_ms,omitempty"`
}

// handleLeaseRelease serves POST /api/v1/leases/{id}/release: the normal end of
// a job.
//
// The reason is passed to the database EXACTLY as the caller wrote it, without
// a client-side allow-list. That is deliberate. farm.leases.release_reason has
// no word for connectivity, so a holder trying to release with
// "device_offline" gets SQLSTATE 23514 from Postgres and a 400 from here that
// names the seven permitted words. Validating it in Go instead would replace a
// loud, tested, server-side refusal with a quiet client-side one, and the whole
// point is that the refusal is enforced where the data lives.
func (s *Server) handleLeaseRelease(w http.ResponseWriter, r *http.Request) {
	leaseID := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(leaseID) {
		badRequest(w, "lease id must be a uuid", nil)
		return
	}

	var req releaseRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	if req.Fence <= 0 {
		badRequest(w, "fence is required", nil)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required", map[string]any{
			"permitted_release_reasons": permittedReleaseReasons,
		})
		return
	}

	// A tenant-scoped caller may only release its OWN lease.
	//
	// Without this the fence is the only thing standing between one tenant and
	// another tenant's run — and the fence is not a secret: GET /api/v1/fleet
	// and the event stream both report lease_id and fence for every live lease
	// in the farm, which is exactly the pair farm.lease_release matches on. Any
	// tenant token could therefore end any other tenant's lease with reason
	// "completed" and destroy a six-hour run, which is the failure this whole
	// system exists to prevent, reached through the front door.
	//
	// The check is a plain read because a lease's tenant_id never changes: the
	// row is inserted by farm.lease_acquire and nothing in the schema updates
	// that column, so there is no window between reading it and acting on it.
	// Renew needs no equivalent — it additionally requires holder_instance,
	// which is returned only to the acquirer and is never exposed by a
	// farm-wide view — and adding a second round trip to the renewal path
	// would slow the one request whose failure costs a device.
	if tenant := tenantScope(r.Context()); tenant != "" {
		var owner string
		err := s.pool.QueryRow(r.Context(),
			`SELECT tenant_id FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&owner)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// 404, not 403: whether another tenant's lease id exists is not
				// this caller's business.
				writeError(w, http.StatusNotFound, CodeNotFound, "no such lease", nil)
				return
			}
			s.fail(w, r, "release lease: check tenant", err)
			return
		}
		if owner != tenant {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such lease", nil)
			return
		}
	}

	// The caller may LENGTHEN the slot's rearm window but never shorten it.
	// internal/config asserts at startup that the configured value exceeds the
	// node proxy's self-fence timeout plus its safety margin; honouring a
	// smaller value from a request body would step straight around that
	// assertion and hand the slot to the next job while the previous holder's
	// sockets were still open — a fenced holder writing to somebody else's
	// device. The value actually applied is echoed back, because silently
	// ignoring what the caller asked for is its own kind of failure.
	rearm := s.cfg.Lease.SlotRearm
	if rearm <= 0 {
		rearm = lease.DefaultRearm
	}
	requestedRearm := time.Duration(req.RearmMS) * time.Millisecond
	if requestedRearm > rearm {
		rearm = requestedRearm
	}

	released, err := s.leases.Release(r.Context(), leaseID, req.Fence, lease.ReleaseReason(reason), rearm)
	if err != nil {
		s.fail(w, r, "release lease", err)
		return
	}

	if !released {
		// Idempotent, not an error: the lease was already terminal or the fence
		// did not match. A holder that was fenced and is now tidying up lands
		// here, and it must not be told to retry.
		state, stateErr := s.leaseState(r.Context(), leaseID)
		if stateErr != nil && !errors.Is(stateErr, pgx.ErrNoRows) {
			s.fail(w, r, "release lease: read state", stateErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"lease_id": leaseID,
			"released": false,
			"state":    state,
			"note": "nothing was released: this lease is already terminal, or the fence presented " +
				"is not the one it holds. This is the idempotent case, not a failure.",
		})
		return
	}

	if parsed, ok := obs.ParseReleaseReason(reason); ok {
		obs.LeaseReaped(parsed)
	}
	// The release is already committed, so its timeline entry must not depend
	// on the caller still being connected.
	eventCtx, eventCancel := detachedCtx(r.Context())
	s.recordEvent(eventCtx, eventRow{
		Kind:    "lease_released",
		LeaseID: &leaseID,
		Actor:   actor(r.Context()),
		Detail:  map[string]any{"reason": reason, "fence": req.Fence, "rearm_s": rearm.Seconds()},
	})
	eventCancel()

	body := map[string]any{
		"lease_id": leaseID,
		"released": true,
		"reason":   reason,
		"rearm_s":  rearm.Seconds(),
	}
	if requestedRearm > 0 && requestedRearm < rearm {
		body["rearm_note"] = "the requested rearm_ms was below this deployment's configured slot " +
			"rearm, which must exceed the node proxy's self-fence timeout, so the configured " +
			"value was applied instead"
	}
	writeJSON(w, http.StatusOK, body)
}

// leaseState reads just the state of a lease, for the explanatory half of an
// idempotent release.
func (s *Server) leaseState(ctx context.Context, leaseID string) (string, error) {
	var state string
	err := s.pool.QueryRow(ctx, `SELECT state FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&state)
	if err != nil {
		return "", err
	}
	return state, nil
}

// ---------------------------------------------------------------------------
// revoke — the human path
// ---------------------------------------------------------------------------

type revokeRequest struct {
	Reason string `json:"reason"`
}

// handleLeaseRevoke serves POST /api/v1/leases/{id}/revoke.
//
// This is one of the three ways a lease may end, and the only one where the
// decision is a person's. It is therefore:
//
//   - operator-only, enforced by middleware,
//   - always permitted while the lease is live — no fence is required, because
//     an operator taking a device back has no reason to know the holder's
//     fence, and a "revoke that needs the fence" is a revoke that fails exactly
//     when it is needed most,
//   - audited, with the human's name and their reason in farm.audit_log,
//   - fence-bumping: farm.devices.fence_floor is raised past the revoked fence,
//     so the previous holder's sockets are refused at the host proxy rather
//     than merely in the database, and the slot is quarantined for rearm so the
//     next job cannot land on a device the old holder is still writing to.
//
// It is written out here rather than through lease.Store.Release because that
// path requires the fence, deliberately: a holder must present its fence, a
// human must not have to.
func (s *Server) handleLeaseRevoke(w http.ResponseWriter, r *http.Request) {
	leaseID := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(leaseID) {
		badRequest(w, "lease id must be a uuid", nil)
		return
	}

	var req revokeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required: a revoke ends someone's run and the audit row must say why", nil)
		return
	}

	who := actor(r.Context())
	rearm := s.cfg.Lease.SlotRearm
	if rearm <= 0 {
		rearm = lease.DefaultRearm
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.fail(w, r, "revoke lease: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.Context())) }()

	var (
		deviceID string
		slotID   *int64
		oldFence int64
		jobID    string
		tenantID string
		holder   string
	)
	err = tx.QueryRow(r.Context(), `
UPDATE farm.leases
   SET state = 'released', released_at = now(), release_reason = 'operator_revoked'
 WHERE id = $1::uuid AND state IN ('held','suspect')
RETURNING device_id::text, slot_id, fence, job_id::text, tenant_id, holder`, leaseID).
		Scan(&deviceID, &slotID, &oldFence, &jobID, &tenantID, &holder)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either it does not exist or it is already terminal; say which,
			// because "already released" is a success from the operator's point
			// of view and "no such lease" is a typo.
			state, stateErr := s.leaseState(r.Context(), leaseID)
			if errors.Is(stateErr, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, CodeNotFound, "no such lease", nil)
				return
			}
			if stateErr != nil {
				s.fail(w, r, "revoke lease: read state", stateErr)
				return
			}
			writeError(w, http.StatusConflict, CodeConflict,
				"this lease is already terminal; there is nothing to revoke",
				map[string]string{"lease_id": leaseID, "state": state})
			return
		}
		s.fail(w, r, "revoke lease", err)
		return
	}

	// Raise the floor past the fence we just closed. Until this lands, a socket
	// the old holder still owns would be accepted at the host proxy.
	var newFloor int64
	if err := tx.QueryRow(r.Context(), `
UPDATE farm.devices SET fence_floor = nextval('farm.fence_seq'), updated_at = now()
 WHERE id = $1::uuid
RETURNING fence_floor`, deviceID).Scan(&newFloor); err != nil {
		s.fail(w, r, "revoke lease: bump fence floor", err)
		return
	}

	// Quarantine the slot so nothing is scheduled onto it until the previous
	// holder's sockets are certainly severed. The interval is the configured
	// rearm, which internal/config asserts exceeds the node proxy's self-fence
	// timeout.
	if slotID != nil {
		if _, err := tx.Exec(r.Context(), `
UPDATE farm.slots SET rearm_at = now() + $2::interval WHERE id = $1`,
			*slotID, intervalSeconds(rearm)); err != nil {
			s.fail(w, r, "revoke lease: rearm slot", err)
			return
		}
	}

	detail := map[string]any{
		"lease_id":        leaseID,
		"device_id":       deviceID,
		"job_id":          jobID,
		"tenant_id":       tenantID,
		"holder":          holder,
		"revoked_fence":   oldFence,
		"new_fence_floor": newFloor,
	}
	if _, err := tx.Exec(r.Context(), `
INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
VALUES ($1, 'lease.revoke', $2, $3, $4::jsonb)`,
		who, "lease:"+leaseID, reason, mustJSON(detail)); err != nil {
		s.fail(w, r, "revoke lease: audit", err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
VALUES ('lease_revoked', $1::uuid, $2, $3::uuid, $4::uuid, $5, $6::jsonb)`,
		deviceID, slotID, leaseID, jobID, who, mustJSON(detail)); err != nil {
		s.fail(w, r, "revoke lease: event", err)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, r, "revoke lease: commit", err)
		return
	}

	obs.LeaseReaped(obs.ReasonOperatorRevoked)
	s.metrics.operatorActions.WithLabelValues("lease.revoke", "ok").Inc()
	s.log.WarnContext(r.Context(), "lease revoked by operator",
		"lease_id", leaseID, "device_id", deviceID, "job_id", jobID,
		"actor", who, "reason", reason, "new_fence_floor", newFloor)

	writeJSON(w, http.StatusOK, map[string]any{
		"lease_id":        leaseID,
		"released":        true,
		"reason":          "operator_revoked",
		"operator_reason": reason,
		"device_id":       deviceID,
		"job_id":          jobID,
		"revoked_fence":   oldFence,
		"new_fence_floor": newFloor,
		"note": "the previous holder is now fenced: any socket still carrying fence " +
			fmt.Sprint(oldFence) + " is refused at the host proxy, and the slot is " +
			"unschedulable until its rearm window elapses",
	})
}

// intervalSeconds renders a Go duration as a Postgres interval literal.
//
// Durations are safe to send; timestamps are not. Every instant in this system
// comes from the database's own now(), so a client clock can never move a
// deadline.
func intervalSeconds(d time.Duration) string {
	return fmt.Sprintf("%f seconds", d.Seconds())
}
