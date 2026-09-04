package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// jobView is one row of farm.jobs.
//
// Durations are seconds rather than ISO intervals because every client of this
// API is either a dashboard or a test harness, and both want arithmetic. Two of
// them mean very different things:
//
//	expected_duration_s  a hint. Over 30 minutes, farm.lease_acquire marks the
//	                     lease protected, which means the reaper will never take
//	                     it back automatically — it holds and pages instead.
//	max_runtime_s        a deadline the user wrote down, and the ONLY
//	                     user-supplied clock in the system that may end a lease
//	                     automatically.
type jobView struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	QueueID           string          `json:"queue_id"`
	PoolID            string          `json:"pool_id"`
	State             string          `json:"state"`
	Spec              json.RawMessage `json:"spec,omitempty"`
	Selector          json.RawMessage `json:"selector,omitempty"`
	PinDevice         *string         `json:"pin_device,omitempty"`
	Protected         bool            `json:"protected"`
	DisruptionPolicy  string          `json:"disruption_policy"`
	ExpectedDurationS *int64          `json:"expected_duration_s,omitempty"`
	MaxRuntimeS       *int64          `json:"max_runtime_s,omitempty"`
	TTLSeconds        int64           `json:"ttl_s"`
	GraceSeconds      int64           `json:"grace_s"`
	CreatedBy         *string         `json:"created_by,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at,omitempty"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`

	// Lease is the job's live lease, if it has one.
	Lease *jobLease `json:"lease,omitempty"`
}

type jobLease struct {
	ID        string `json:"id"`
	Fence     int64  `json:"fence"`
	State     string `json:"state"`
	DeviceID  string `json:"device_id"`
	Protected bool   `json:"protected"`
}

const jobColumns = `
  j.id::text, j.tenant_id, j.queue_id, j.pool_id, j.state, j.spec, j.selector,
  j.pin_device::text, j.protected, j.disruption_policy,
  EXTRACT(EPOCH FROM j.expected_duration)::bigint,
  EXTRACT(EPOCH FROM j.max_runtime)::bigint,
  EXTRACT(EPOCH FROM j.ttl)::bigint, EXTRACT(EPOCH FROM j.grace)::bigint,
  j.created_by, j.created_at, j.started_at, j.finished_at,
  l.id::text, l.fence, l.state, l.device_id::text, l.protected`

const jobFrom = `
  FROM farm.jobs j
  LEFT JOIN farm.leases l ON l.job_id = j.id AND l.state IN ('held','suspect')`

func scanJob(sc scanner) (jobView, error) {
	var (
		v                   jobView
		spec, selector      []byte
		leaseID, leaseState *string
		leaseFence          *int64
		leaseDevice         *string
		leaseProtected      *bool
	)
	err := sc.Scan(
		&v.ID, &v.TenantID, &v.QueueID, &v.PoolID, &v.State, &spec, &selector,
		&v.PinDevice, &v.Protected, &v.DisruptionPolicy,
		&v.ExpectedDurationS, &v.MaxRuntimeS, &v.TTLSeconds, &v.GraceSeconds,
		&v.CreatedBy, &v.CreatedAt, &v.StartedAt, &v.FinishedAt,
		&leaseID, &leaseFence, &leaseState, &leaseDevice, &leaseProtected,
	)
	if err != nil {
		return jobView{}, err
	}
	if len(spec) > 0 {
		v.Spec = json.RawMessage(spec)
	}
	if len(selector) > 0 {
		v.Selector = json.RawMessage(selector)
	}
	if leaseID != nil {
		l := &jobLease{ID: *leaseID}
		if leaseFence != nil {
			l.Fence = *leaseFence
		}
		if leaseState != nil {
			l.State = *leaseState
		}
		if leaseDevice != nil {
			l.DeviceID = *leaseDevice
		}
		if leaseProtected != nil {
			l.Protected = *leaseProtected
		}
		v.Lease = l
	}
	return v, nil
}

// handleJobList serves GET /api/v1/jobs?state=.
func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	state := queryString(r, "state")
	limit := queryInt(r, "limit", 200, 1, 2000)

	var (
		conds []string
		args  []any
	)
	switch state {
	case "", "all":
	case "live":
		conds = append(conds, "j.state IN ('queued','allocating','running')")
	case "terminal":
		conds = append(conds, "j.state IN ('succeeded','failed','cancelled')")
	case "queued", "allocating", "running", "succeeded", "failed", "cancelled":
		args = append(args, state)
		conds = append(conds, fmt.Sprintf("j.state = $%d", len(args)))
	default:
		badRequest(w, "state must be one of all, live, terminal, queued, allocating, running, "+
			"succeeded, failed, cancelled", nil)
		return
	}
	if tenant := tenantScope(r.Context()); tenant != "" {
		args = append(args, tenant)
		conds = append(conds, fmt.Sprintf("j.tenant_id = $%d", len(args)))
	} else if t := queryString(r, "tenant"); t != "" {
		args = append(args, t)
		conds = append(conds, fmt.Sprintf("j.tenant_id = $%d", len(args)))
	}
	if pool := queryString(r, "pool"); pool != "" {
		args = append(args, pool)
		conds = append(conds, fmt.Sprintf("j.pool_id = $%d", len(args)))
	}
	if queue := queryString(r, "queue"); queue != "" {
		args = append(args, queue)
		conds = append(conds, fmt.Sprintf("j.queue_id = $%d", len(args)))
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	query := fmt.Sprintf("SELECT %s %s %s ORDER BY j.created_at DESC LIMIT $%d",
		jobColumns, jobFrom, where, len(args))

	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.fail(w, r, "list jobs", err)
		return
	}
	defer rows.Close()

	out := make([]jobView, 0, 64)
	counts := map[string]int{}
	for rows.Next() {
		v, err := scanJob(rows)
		if err != nil {
			s.fail(w, r, "scan job", err)
			return
		}
		counts[v.State]++
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read jobs", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":      out,
		"counts":    counts,
		"truncated": len(out) == limit,
	})
}

// jobCreateRequest is the body of POST /api/v1/jobs.
type jobCreateRequest struct {
	Pool   string `json:"pool"`
	Queue  string `json:"queue"`
	Tenant string `json:"tenant"`

	Spec     json.RawMessage `json:"spec,omitempty"`
	Selector json.RawMessage `json:"selector,omitempty"`

	PinDevice        string `json:"pin_device,omitempty"`
	Protected        bool   `json:"protected,omitempty"`
	DisruptionPolicy string `json:"disruption_policy,omitempty"`

	ExpectedDurationS int64 `json:"expected_duration_s,omitempty"`
	MaxRuntimeS       int64 `json:"max_runtime_s,omitempty"`
	TTLS              int64 `json:"ttl_s,omitempty"`
	GraceS            int64 `json:"grace_s,omitempty"`

	CreatedBy string `json:"created_by,omitempty"`

	// The four farm.jobs columns internal/runner reads and no caller could
	// set. profile_id in particular was written by nothing in the tree, so a
	// reset tier had no package list to reset against.
	JobSubmissionOptions
}

// handleJobCreate serves POST /api/v1/jobs.
//
// Nothing is scheduled here: creating a job puts a row in farm.jobs, and the
// scheduler is what turns it into a lease through farm.lease_acquire. Splitting
// those two keeps allocation — with its row locks and its partial unique
// indexes — inside one database function instead of spread across an HTTP
// handler.
func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	var req jobCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}

	req.Pool = strings.TrimSpace(req.Pool)
	req.Queue = strings.TrimSpace(req.Queue)
	req.Tenant = strings.TrimSpace(req.Tenant)

	// A tenant-scoped caller may only file work under its own tenant, whatever
	// the body says.
	if scoped := tenantScope(r.Context()); scoped != "" {
		if req.Tenant != "" && req.Tenant != scoped {
			writeError(w, http.StatusForbidden, CodeForbidden,
				"you may only create jobs for your own tenant", nil)
			return
		}
		req.Tenant = scoped
	}

	switch {
	case req.Pool == "":
		badRequest(w, "pool is required", nil)
		return
	case req.Queue == "":
		badRequest(w, "queue is required", nil)
		return
	case req.Tenant == "":
		badRequest(w, "tenant is required", nil)
		return
	}
	if req.PinDevice != "" && !looksLikeUUID(req.PinDevice) {
		badRequest(w, "pin_device must be a device uuid", nil)
		return
	}

	// Resolve the three references in one round trip so the caller is told
	// which one is wrong instead of reading a foreign-key violation.
	var tenants, pools, queues, queueInTenant int
	err := s.pool.QueryRow(r.Context(), `
SELECT (SELECT count(*) FROM farm.tenants WHERE id = $1),
       (SELECT count(*) FROM farm.pools   WHERE id = $2),
       (SELECT count(*) FROM farm.queues  WHERE id = $3),
       (SELECT count(*) FROM farm.queues  WHERE id = $3 AND tenant_id = $1)`,
		req.Tenant, req.Pool, req.Queue).Scan(&tenants, &pools, &queues, &queueInTenant)
	if err != nil {
		s.fail(w, r, "create job: resolve references", err)
		return
	}
	switch {
	case tenants == 0:
		badRequest(w, "no such tenant: "+req.Tenant, nil)
		return
	case pools == 0:
		badRequest(w, "no such pool: "+req.Pool, nil)
		return
	case queues == 0:
		badRequest(w, "no such queue: "+req.Queue, nil)
		return
	case queueInTenant == 0:
		badRequest(w, "queue "+req.Queue+" does not belong to tenant "+req.Tenant, nil)
		return
	}

	// TTL and grace default to this deployment's configured values rather than
	// to the schema's, so one place decides how long a holder may be silent.
	// Both are floors enforced again by CHECK constraints; a value under them
	// comes back as a 400 from the database, not a 500.
	ttl := req.TTLS
	if ttl <= 0 {
		ttl = int64(s.cfg.Lease.TTL.Seconds())
	}
	grace := req.GraceS
	if grace <= 0 {
		grace = int64(s.cfg.Lease.Grace.Seconds())
	}

	spec := req.Spec
	if len(spec) == 0 {
		spec = json.RawMessage("{}")
	}
	selector := req.Selector
	if len(selector) == 0 {
		selector = json.RawMessage("{}")
	}
	policy := strings.TrimSpace(req.DisruptionPolicy)
	if policy == "" {
		policy = "allow_port_power_cycle"
	}
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = actor(r.Context())
	}

	// THE GATE. specSubmissionError has existed and been documented as "the
	// gate POST /api/v1/jobs uses" since it was written, and nothing called
	// it: a spec that could never run was accepted, allocated a device, and
	// failed at the runner — having occupied a handset to do it. Validating
	// here costs one request; validating at the runner costs a device.
	opts, apiErr, verr := s.ValidateJobSubmission(r.Context(), spec, selector, req.JobSubmissionOptions)
	if verr != nil {
		s.fail(w, r, "validate job submission", verr)
		return
	}
	if apiErr != nil {
		RejectSubmission(w, apiErr)
		return
	}
	selector = opts.Selector

	const insert = `
INSERT INTO farm.jobs (
  tenant_id, queue_id, pool_id, spec, selector, pin_device, protected, disruption_policy,
  expected_duration, max_runtime, ttl, grace, created_by,
  profile_id, reset_tier, max_attempts, resumable)
VALUES (
  $1, $2, $3, $4::jsonb, $5::jsonb, nullif($6,'')::uuid, $7, $8,
  CASE WHEN $9::bigint  IS NULL THEN NULL ELSE make_interval(secs => $9::bigint)  END,
  CASE WHEN $10::bigint IS NULL THEN NULL ELSE make_interval(secs => $10::bigint) END,
  make_interval(secs => $11::bigint), make_interval(secs => $12::bigint), $13,
  nullif($14,''), $15, $16::int, $17::boolean)
RETURNING id::text`

	var jobID string
	err = s.pool.QueryRow(r.Context(), insert,
		req.Tenant, req.Queue, req.Pool, []byte(spec), []byte(selector), req.PinDevice,
		req.Protected, policy,
		secondsPtr(req.ExpectedDurationS), secondsPtr(req.MaxRuntimeS),
		ttl, grace, createdBy,
		opts.ProfileID, opts.ResetTier, opts.MaxAttempts, opts.Resumable).Scan(&jobID)
	if err != nil {
		s.fail(w, r, "create job", err)
		return
	}

	// The row is committed, so its timeline entry must not be lost because the
	// caller hung up between the insert and the response.
	eventCtx, eventCancel := detachedCtx(r.Context())
	s.recordEvent(eventCtx, eventRow{
		Kind:   "job_created",
		JobID:  &jobID,
		Actor:  actor(r.Context()),
		Detail: map[string]any{"pool": req.Pool, "queue": req.Queue, "tenant": req.Tenant},
	})
	eventCancel()

	job, err := s.lookupJob(r.Context(), jobID)
	if err != nil {
		// The row exists — the read-back failed. Report the id rather than
		// pretending the create failed, because a client that retries would
		// file a second job.
		s.log.ErrorContext(r.Context(), "job created but could not be read back",
			"job_id", jobID, "err", err)
		w.Header().Set("Location", "/api/v1/jobs/"+jobID)
		writeJSON(w, http.StatusCreated, map[string]any{"id": jobID})
		return
	}

	w.Header().Set("Location", "/api/v1/jobs/"+jobID)
	writeJSON(w, http.StatusCreated, map[string]any{"job": job})
}

func (s *Server) lookupJob(ctx context.Context, id string) (jobView, error) {
	query := fmt.Sprintf("SELECT %s %s WHERE j.id = $1::uuid", jobColumns, jobFrom)
	return scanJob(s.pool.QueryRow(ctx, query, id))
}

// handleJobGet serves GET /api/v1/jobs/{id}.
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		badRequest(w, "job id must be a uuid", nil)
		return
	}

	job, err := s.lookupJob(r.Context(), id)
	if err != nil {
		s.fail(w, r, "get job", err)
		return
	}
	if tenant := tenantScope(r.Context()); tenant != "" && job.TenantID != tenant {
		// 404 rather than 403: whether another tenant's job id exists is not
		// this caller's business.
		writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
		return
	}

	// The lease history matters for a post-mortem: it shows whether a run ended
	// with 'completed' or with 'holder_expired', which is the difference
	// between work finishing and work being destroyed.
	const historyQuery = `
SELECT l.id::text, l.fence, l.state, l.device_id::text, l.holder, l.acquired_at,
       l.released_at, l.release_reason
  FROM farm.leases l
 WHERE l.job_id = $1::uuid
 ORDER BY l.acquired_at DESC
 LIMIT 10`

	rows, err := s.pool.Query(r.Context(), historyQuery, id)
	if err != nil {
		s.fail(w, r, "get job leases", err)
		return
	}
	defer rows.Close()

	type historyRow struct {
		ID            string     `json:"id"`
		Fence         int64      `json:"fence"`
		State         string     `json:"state"`
		DeviceID      string     `json:"device_id"`
		Holder        string     `json:"holder"`
		AcquiredAt    time.Time  `json:"acquired_at"`
		ReleasedAt    *time.Time `json:"released_at,omitempty"`
		ReleaseReason *string    `json:"release_reason,omitempty"`
	}
	history := make([]historyRow, 0, 4)
	for rows.Next() {
		var h historyRow
		if err := rows.Scan(&h.ID, &h.Fence, &h.State, &h.DeviceID, &h.Holder,
			&h.AcquiredAt, &h.ReleasedAt, &h.ReleaseReason); err != nil {
			s.fail(w, r, "scan job lease", err)
			return
		}
		history = append(history, h)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read job leases", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"job":    job,
		"leases": history,
	})
}

// handleJobCancel serves POST /api/v1/jobs/{id}/cancel.
//
// Cancelling releases the job's live lease with reason 'job_cancelled'. That is
// one of the three legitimate endings — the job is over because someone said so
// — and it goes through farm.lease_release with the lease's own fence, so the
// device's fence_floor is raised and the slot is quarantined for rearm exactly
// as it is on a normal completion.
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		badRequest(w, "job id must be a uuid", nil)
		return
	}
	var req revokeRequest // {"reason": ...}, optional here
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	who := actor(r.Context())

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.fail(w, r, "cancel job: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.Context())) }()

	var tenantID string
	err = tx.QueryRow(r.Context(), `
UPDATE farm.jobs
   SET state = 'cancelled', finished_at = now()
 WHERE id = $1::uuid AND state IN ('queued','allocating','running')
RETURNING tenant_id`, id).Scan(&tenantID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var state string
			readErr := s.pool.QueryRow(r.Context(),
				`SELECT state FROM farm.jobs WHERE id = $1::uuid`, id).Scan(&state)
			if errors.Is(readErr, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
				return
			}
			if readErr != nil {
				s.fail(w, r, "cancel job: read state", readErr)
				return
			}
			writeError(w, http.StatusConflict, CodeConflict,
				"this job is already in a terminal state",
				map[string]string{"job_id": id, "state": state})
			return
		}
		s.fail(w, r, "cancel job", err)
		return
	}

	if scoped := tenantScope(r.Context()); scoped != "" && tenantID != scoped {
		// The UPDATE is rolled back by the deferred Rollback, so the job is
		// untouched.
		writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
		return
	}

	// Release the live lease, if there is one, inside the same transaction:
	// a cancelled job that kept its device would leave a phone allocated to
	// work that no longer exists.
	var (
		leaseID  string
		fence    int64
		deviceID string
		released bool
	)
	err = tx.QueryRow(r.Context(), `
SELECT id::text, fence, device_id::text
  FROM farm.leases
 WHERE job_id = $1::uuid AND state IN ('held','suspect')
 FOR NO KEY UPDATE`, id).Scan(&leaseID, &fence, &deviceID)
	switch {
	case err == nil:
		// A zero rearm would send farm.lease_release an interval of zero and
		// leave the slot immediately schedulable, so the next job could land on
		// a device the cancelled job's sockets have not yet been severed from.
		rearm := s.cfg.Lease.SlotRearm
		if rearm <= 0 {
			rearm = lease.DefaultRearm
		}
		if err := tx.QueryRow(r.Context(),
			`SELECT farm.lease_release($1::uuid, $2::bigint, 'job_cancelled', $3::interval)`,
			leaseID, fence, intervalSeconds(rearm)).Scan(&released); err != nil {
			s.fail(w, r, "cancel job: release lease", err)
			return
		}
	case errors.Is(err, pgx.ErrNoRows):
		// Nothing allocated yet, or it already ended. Cancelling is still valid.
	default:
		s.fail(w, r, "cancel job: read lease", err)
		return
	}

	detail := map[string]any{"job_id": id, "tenant_id": tenantID}
	if leaseID != "" {
		detail["lease_id"] = leaseID
		detail["device_id"] = deviceID
		detail["lease_released"] = released
	}
	if _, err := tx.Exec(r.Context(), `
INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
VALUES ($1, 'job.cancel', $2, nullif($3,''), $4::jsonb)`,
		who, "job:"+id, reason, mustJSON(detail)); err != nil {
		s.fail(w, r, "cancel job: audit", err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
INSERT INTO farm.events (kind, job_id, lease_id, actor, detail)
VALUES ('job_cancelled', $1::uuid, nullif($2,'')::uuid, $3, $4::jsonb)`,
		id, leaseID, who, mustJSON(detail)); err != nil {
		s.fail(w, r, "cancel job: event", err)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, r, "cancel job: commit", err)
		return
	}

	if released {
		obs.LeaseReaped(obs.ReasonJobCancelled)
	}
	s.metrics.operatorActions.WithLabelValues("job.cancel", "ok").Inc()

	body := map[string]any{"job_id": id, "state": "cancelled"}
	if leaseID != "" {
		body["lease_id"] = leaseID
		body["lease_released"] = released
		body["device_id"] = deviceID
	}
	writeJSON(w, http.StatusOK, body)
}
