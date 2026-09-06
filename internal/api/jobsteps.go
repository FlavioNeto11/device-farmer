package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// JobStepsAPI serves the execution half of a job: what it ran, and where.
//
// farm.job_steps and farm.job_attempts are written on every placement and read
// by nothing — an operator with a failed job could see that it failed and not
// which step, what it printed, or which handsets it had already burned. These
// two endpoints are how one question gets answered:
//
//	four failures on four devices  → the job is wrong
//	four failures on one device    → the device is wrong
//
// Neither answer is reachable from farm.jobs alone, and the difference decides
// whether a human fixes a spec or pulls a phone out of the rack.
//
// It is a separate type with its own Register for the same reason ArtifactAPI
// is: the routes it owns are a subsystem, and mounting them is the parent's
// decision rather than something buried in the router.
type JobStepsAPI struct {
	srv *Server
}

// NewJobStepsAPI binds the job execution views to a Server.
func NewJobStepsAPI(srv *Server) (*JobStepsAPI, error) {
	if srv == nil {
		return nil, errors.New("api: nil server")
	}
	return &JobStepsAPI{srv: srv}, nil
}

// Register mounts the job execution routes on mux.
//
// Both are tenant reads: they show a caller their own job's history, scoped by
// the same rule GET /api/v1/jobs/{id} uses. Neither writes anything, and
// neither can touch a lease.
func (j *JobStepsAPI) Register(mux *http.ServeMux) {
	s := j.srv
	mux.Handle("GET /api/v1/jobs/{id}/steps", s.requireRole(RoleTenant, http.HandlerFunc(j.handleSteps)))
	mux.Handle("GET /api/v1/jobs/{id}/attempts", s.requireRole(RoleTenant, http.HandlerFunc(j.handleAttempts)))
}

// Bounds on one response.
//
// A spec may carry jobspec.MaxSteps steps and the runner stores up to 64 KiB
// of output per step, so an unbounded rendering of one attempt is 64 MB of
// JSON marshalled into a buffer before a byte of it is written. A few of those
// concurrently is an OOM, and a control plane that dies cannot answer renewals
// — which is how a read-only debugging endpoint ends somebody's lease. The
// cure is the one specs.go already uses: cap what is RENDERED, and report the
// true size beside the cap so nobody is told a step was quiet when it was not.
const (
	// defaultStepOutputChars is what one step's output and error contribute
	// unless the caller asks for more. It is characters, not bytes, because
	// left() and length() are.
	defaultStepOutputChars = 4096

	// maxStepOutputChars matches runner.DefaultMaxOutput, the ceiling the
	// runner itself stores. Asking for more cannot return more.
	maxStepOutputChars = 64 << 10

	// defaultJobStepLimit renders one full attempt of the largest spec the
	// validator accepts, so the common request needs no paging at all.
	defaultJobStepLimit = 1000
	maxJobStepLimit     = 5000

	// maxRenderedLogBytes is the budget for the variable-size fields of one
	// response — every step's output, error and detail TOGETHER, counted in
	// bytes of stored log. Past it a step keeps its identity, state, timing and
	// exit code, and each dropped field is flagged, so the skeleton of what
	// happened survives even when the text does not.
	//
	// All three have to be charged against it, not output alone. A job with
	// 1200 steps carrying a 60 KB error and a 60 KB detail apiece — and no
	// output whatsoever — rendered a 144 MB response while this budget counted
	// output only. That is measured, not estimated, and writeJSON marshals the
	// whole body into a buffer before it writes a byte, so the live peak is
	// about twice that per request.
	maxRenderedLogBytes = 2 << 20

	// maxReservedErrorBytes is the share of that budget output and detail may
	// not touch.
	//
	// Charging all three from one pot in row order is not enough. With 80 steps
	// carrying a 60 KB output and a 2 KB error apiece, the first 25 outputs
	// drained the budget and only 28 steps kept their error — the field that
	// says WHY a step failed, and the smallest of the three, lost to the bulk
	// of steps that merely printed a lot. Errors spend this reserve first and
	// the shared remainder afterwards, so nothing is wasted on a response that
	// has no output to render.
	maxReservedErrorBytes = maxRenderedLogBytes / 4

	defaultJobAttemptLimit = 200
	maxJobAttemptLimit     = 2000
)

// logBudget is maxRenderedLogBytes held in the two pots described above and
// spent in the order rows arrive — newest attempt first — so the logs that
// survive a busy job are the recent ones.
//
// It is a named type rather than a pair of closures inside the handler because
// the rule it encodes is the one part of this file that cannot be checked by
// reading a response: whether an error message survived depends on how many
// bytes of somebody else's output arrived before it. As a type it can be
// driven directly, with the exact shape of traffic that broke it — 80 steps
// carrying 60 KB of output and 2 KB of error apiece — and asserted on without
// a database, a job, or 5 MB of fixture text.
type logBudget struct {
	// shared is what output and detail draw from, and what an error falls back
	// to once the reserve is gone.
	shared int
	// reserved is the share only an error may spend.
	reserved int
	// omitted counts fields dropped whole, reported to the caller as
	// logs_omitted so a missing log never reads as a silent one.
	omitted int
}

func newLogBudget() logBudget {
	return logBudget{
		shared:   maxRenderedLogBytes - maxReservedErrorBytes,
		reserved: maxReservedErrorBytes,
	}
}

// spend charges n bytes of output or detail to the shared pot, reporting
// whether the field may be rendered.
//
// A field that does not fit is dropped WHOLE and counted. Cutting it to
// whatever was left would hand back a fragment that looks like a complete log,
// which is the one outcome worse than saying it is gone.
func (b *logBudget) spend(n int) bool {
	if n > b.shared {
		b.omitted++
		return false
	}
	b.shared -= n
	return true
}

// spendError charges n bytes of a step's error, the reserve first.
//
// Nothing is wasted on a response with no output to render: once the reserve
// is exhausted an error competes for the shared pot like anything else.
func (b *logBudget) spendError(n int) bool {
	if n <= b.reserved {
		b.reserved -= n
		return true
	}
	return b.spend(n)
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// jobStepView is one row of farm.job_steps.
//
// DurationS is computed by Postgres against now(), so a step still running
// reports how long it has been running rather than nothing. That number is the
// one an operator reads at 3am to decide whether a job is working or wedged,
// and it must not come from a client's clock.
type jobStepView struct {
	Attempt    int        `json:"attempt"`
	StepIndex  int        `json:"step_index"`
	StepID     string     `json:"step_id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationS  *float64   `json:"duration_s,omitempty"`
	ExitCode   *int64     `json:"exit_code,omitempty"`

	Output string `json:"output,omitempty"`
	// OutputChars is the true length of what the runner stored. It is present
	// whenever there is output at all, so "empty" and "cut" never look alike.
	OutputChars     int64 `json:"output_chars,omitempty"`
	OutputTruncated bool  `json:"output_truncated,omitempty"`
	// OutputOmitted marks a step whose output was dropped because the response
	// had already spent its maxRenderedLogBytes budget. The step is still here;
	// only its log is missing, and asking for that attempt directly with a
	// smaller ?limit returns it.
	//
	// A dropped field is dropped whole. Cutting it to whatever was left of the
	// budget would hand back a fragment that looks like a complete log, which
	// is the one outcome worse than saying it is gone.
	OutputOmitted bool `json:"output_omitted,omitempty"`

	Error          string `json:"error,omitempty"`
	ErrorChars     int64  `json:"error_chars,omitempty"`
	ErrorTruncated bool   `json:"error_truncated,omitempty"`
	ErrorOmitted   bool   `json:"error_omitted,omitempty"`

	// Detail is farm.job_steps.detail: the structured context the runner
	// attached — the command that ran, the reset tier it expanded, the
	// detached handle it re-attached to.
	Detail json.RawMessage `json:"detail,omitempty"`
	// DetailOmitted marks a detail document dropped for the same budget. It is
	// the only one of the three with no ?output_chars to bound it — the column
	// is jsonb and nothing caps what the runner accumulates in it — so it is
	// charged last and is the first thing a busy response loses.
	DetailOmitted bool `json:"detail_omitted,omitempty"`
}

// jobAttemptView is one row of farm.job_attempts: one time this job was placed
// on a device.
//
// Fence is here because it is the only thing that orders two placements
// against the same handset, and ReleaseReason because it is the difference
// between work that finished and work that was taken away — 'completed' and
// 'holder_expired' are the same row shape and opposite stories.
type jobAttemptView struct {
	ID       int64  `json:"id"`
	Attempt  int    `json:"attempt"`
	DeviceID string `json:"device_id,omitempty"`

	// FarmUID is the device's identity. AdbSerial is evidence about it and is
	// deliberately not unique — duplicate OEM serials are real — so a reader
	// comparing two attempts must compare farm_uid, never the serial.
	FarmUID   string `json:"farm_uid,omitempty"`
	Model     string `json:"model,omitempty"`
	AdbSerial string `json:"adb_serial,omitempty"`

	// CurrentDevpath is where that device sits NOW, not where it sat during
	// this attempt. A phone that has been re-slotted since is a different
	// position with the same identity, and pretending otherwise would send a
	// reader to the wrong port of the wrong hub.
	CurrentDevpath string `json:"current_devpath,omitempty"`

	LeaseID       string `json:"lease_id,omitempty"`
	Fence         *int64 `json:"fence,omitempty"`
	LeaseState    string `json:"lease_state,omitempty"`
	ReleaseReason string `json:"release_reason,omitempty"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationS  *float64   `json:"duration_s,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// attemptDeviceView is the per-device tally, which is the point of the
// endpoint. It is computed from the rows that were rendered, so it never
// claims more than it can show.
type attemptDeviceView struct {
	DeviceID string         `json:"device_id"`
	FarmUID  string         `json:"farm_uid,omitempty"`
	Model    string         `json:"model,omitempty"`
	Attempts int            `json:"attempts"`
	Outcomes map[string]int `json:"outcomes,omitempty"`
}

// ---------------------------------------------------------------------------
// GET /api/v1/jobs/{id}/steps
// ---------------------------------------------------------------------------

// handleSteps serves the ordered step log of one attempt.
//
// The default is the newest attempt that actually ran, because that is the one
// somebody is asking about; ?attempt=N pins an older one and ?attempt=all
// walks every placement in order. Which attempts exist is reported either way,
// so a caller never has to guess a number to find out it is empty.
func (j *JobStepsAPI) handleSteps(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		badRequest(w, "job id must be a uuid", nil)
		return
	}
	pinned, all, ok := attemptFilter(w, r)
	if !ok {
		return
	}

	head, err := j.header(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
			return
		}
		j.srv.fail(w, r, "get job steps: read job", err)
		return
	}
	if tenant := tenantScope(r.Context()); tenant != "" && head.TenantID != tenant {
		// 404 rather than 403, exactly as handleJobGet does: whether another
		// tenant's job id exists is not this caller's business.
		writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
		return
	}

	chars := queryInt(r, "output_chars", defaultStepOutputChars, 0, maxStepOutputChars)
	limit := queryInt(r, "limit", defaultJobStepLimit, 1, maxJobStepLimit)

	// NEWEST attempt first, then re-sorted below for rendering.
	//
	// The ordering decides what a truncated ?attempt=all loses, and both the
	// LIMIT and the log budget are spent in the order rows arrive. Ascending,
	// a job with more steps than one page rendered attempt 1 in full and
	// dropped the attempt that just failed — the only one anybody opens this
	// endpoint to read. Within an attempt the order is still step_index
	// ascending, so what a cut removes is the tail of the OLDEST attempt shown.
	const q = `
SELECT s.attempt, s.step_index, s.step_id, s.kind, s.state,
       s.started_at, s.finished_at,
       CASE WHEN s.started_at IS NULL THEN NULL
            ELSE EXTRACT(EPOCH FROM (COALESCE(s.finished_at, now()) - s.started_at))::float8
       END,
       s.exit_code::bigint,
       left(s.output, $2::int), length(s.output)::bigint,
       left(s.error,  $2::int), length(s.error)::bigint,
       s.detail
  FROM farm.job_steps s
 WHERE s.job_id = $1::uuid
   AND ($3::boolean
        OR s.attempt = COALESCE($4::int,
             (SELECT max(x.attempt) FROM farm.job_steps x WHERE x.job_id = $1::uuid)))
 ORDER BY s.attempt DESC, s.step_index
 LIMIT $5::int`

	rows, err := j.srv.pool.Query(r.Context(), q, id, chars, all, pinned, limit)
	if err != nil {
		j.srv.fail(w, r, "get job steps", err)
		return
	}
	defer rows.Close()

	out := make([]jobStepView, 0, 64)
	states := map[string]int{}

	// One budget for the whole response, in two pots: spend serves output and
	// detail; spendError may also draw on the reserve the other two cannot
	// reach.
	budget := newLogBudget()

	for rows.Next() {
		var (
			v                 jobStepView
			output, errText   *string
			outputLen, errLen *int64
			detail            []byte
		)
		if err := rows.Scan(&v.Attempt, &v.StepIndex, &v.StepID, &v.Kind, &v.State,
			&v.StartedAt, &v.FinishedAt, &v.DurationS, &v.ExitCode,
			&output, &outputLen, &errText, &errLen, &detail); err != nil {
			j.srv.fail(w, r, "scan job step", err)
			return
		}
		// The error is charged first, and against its own reserve, because it is
		// the field that says why a step failed. Spending the budget on a
		// chatty successful step and then dropping the message of the one that
		// broke would answer the wrong question.
		if errText != nil {
			v.ErrorChars = derefInt64(errLen)
			v.ErrorTruncated = v.ErrorChars > int64(len([]rune(*errText)))
			if budget.spendError(len(*errText)) {
				v.Error = *errText
			} else {
				v.ErrorOmitted = true
			}
		}
		if output != nil {
			// length() and left() both count CHARACTERS, so the comparison
			// that decides "was this cut" has to count them too — a step that
			// printed UTF-8 would otherwise be reported as truncated when it
			// was not.
			v.OutputChars = derefInt64(outputLen)
			v.OutputTruncated = v.OutputChars > int64(len([]rune(*output)))
			if budget.spend(len(*output)) {
				v.Output = *output
			} else {
				v.OutputOmitted = true
			}
		}
		if len(detail) > 0 {
			if budget.spend(len(detail)) {
				v.Detail = json.RawMessage(detail)
			} else {
				v.DetailOmitted = true
			}
		}
		states[v.State]++
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		j.srv.fail(w, r, "read job steps", err)
		return
	}

	// Rows arrived newest-attempt-first so the budget would be spent there;
	// they are read as a chronology, so they are rendered oldest first.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Attempt != out[b].Attempt {
			return out[a].Attempt < out[b].Attempt
		}
		return out[a].StepIndex < out[b].StepIndex
	})

	body := map[string]any{
		"job_id":              id,
		"job_state":           head.State,
		"attempt":             head.Attempt,
		"max_attempts":        head.MaxAttempts,
		"attempts_with_steps": head.Attempts,
		"steps":               out,
		// states tallies the steps this response RENDERED. When truncated is
		// true it is a tally of the page, not of the attempt, and the attempts
		// actually covered are the ones in the steps list — compare them
		// against attempts_with_steps to see what is missing.
		"states":    states,
		"truncated": len(out) == limit,
		// logs_omitted counts the output, error and detail fields dropped for
		// the response budget. Non-zero means the steps are all here and some
		// of their text is not; a narrower ?attempt= or a smaller ?limit
		// returns it.
		"logs_omitted": budget.omitted,
	}
	if all {
		body["scope"] = "all"
	} else if pinned != nil {
		body["scope"] = strconv.Itoa(*pinned)
	} else {
		body["scope"] = "latest"
	}
	writeJSON(w, http.StatusOK, body)
}

// attemptFilter reads ?attempt=.
//
// A bad value is refused rather than clamped. queryInt's habit of falling back
// to a default is right for a page size and wrong here: silently answering
// about attempt 4 when somebody asked about attempt "4a" is a lie told at the
// exact moment they are trying to work out what went wrong.
func attemptFilter(w http.ResponseWriter, r *http.Request) (pinned *int, all bool, ok bool) {
	raw := queryString(r, "attempt")
	switch {
	case raw == "":
		return nil, false, true
	case strings.EqualFold(raw, "all"):
		return nil, true, true
	}
	// The upper bound is farm.job_steps.attempt's own type. Without it a
	// parameter of 3000000000 reached the driver, failed to encode into an
	// int4, and came back as a 500 with the message "internal error" — a typo
	// in a query string reported as a broken server, at the moment somebody is
	// trying to work out what broke.
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > math.MaxInt32 {
		badRequest(w, "attempt must be a whole number from 1 to 2147483647 (the range of "+
			"farm.job_steps.attempt) or the word all; omit it for the newest attempt that ran",
			map[string]any{"attempt": raw})
		return nil, false, false
	}
	return &n, false, true
}

// jobStepsHeader is what the steps view needs about the job itself.
type jobStepsHeader struct {
	TenantID    string
	State       string
	Attempt     int
	MaxAttempts int
	Attempts    []int32
}

// header reads the job row and the attempts that have step rows in one round
// trip. It is also the existence and tenancy check, so a job that is not this
// caller's never reaches a second query.
func (j *JobStepsAPI) header(ctx context.Context, id string) (jobStepsHeader, error) {
	const q = `
SELECT j.tenant_id, j.state, j.attempt, j.max_attempts,
       COALESCE((SELECT array_agg(DISTINCT s.attempt ORDER BY s.attempt)
                   FROM farm.job_steps s WHERE s.job_id = j.id), '{}')
  FROM farm.jobs j
 WHERE j.id = $1::uuid`

	var h jobStepsHeader
	err := j.srv.pool.QueryRow(ctx, q, id).
		Scan(&h.TenantID, &h.State, &h.Attempt, &h.MaxAttempts, &h.Attempts)
	return h, err
}

// ---------------------------------------------------------------------------
// GET /api/v1/jobs/{id}/attempts
// ---------------------------------------------------------------------------

// handleAttempts serves every placement this job has had.
//
// Newest first, with the device it ran on and the fence it held, plus the
// per-device tally that turns the list into a verdict: one device appearing
// four times is a device to pull; four devices appearing once each is a spec
// to fix.
func (j *JobStepsAPI) handleAttempts(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		badRequest(w, "job id must be a uuid", nil)
		return
	}

	head, err := j.header(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
			return
		}
		j.srv.fail(w, r, "get job attempts: read job", err)
		return
	}
	if tenant := tenantScope(r.Context()); tenant != "" && head.TenantID != tenant {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such job", nil)
		return
	}

	limit := queryInt(r, "limit", defaultJobAttemptLimit, 1, maxJobAttemptLimit)

	// The slot join answers "where is this handset now". It is a join through
	// devices.current_slot_id rather than anything recorded on the attempt,
	// because the attempt row does not record a position and inventing one
	// from the present would be worse than admitting the present is all we
	// have.
	const q = `
SELECT a.id, a.attempt, a.device_id::text,
       d.farm_uid, d.model, d.adb_serial, sl.adb_devpath,
       a.lease_id::text, a.fence, l.state, l.release_reason,
       a.started_at, a.finished_at,
       EXTRACT(EPOCH FROM (COALESCE(a.finished_at, now()) - a.started_at))::float8,
       a.outcome, a.error
  FROM farm.job_attempts a
  LEFT JOIN farm.devices d  ON d.id  = a.device_id
  LEFT JOIN farm.slots   sl ON sl.id = d.current_slot_id
  LEFT JOIN farm.leases  l  ON l.id  = a.lease_id
 WHERE a.job_id = $1::uuid
 ORDER BY a.attempt DESC
 LIMIT $2::int`

	rows, err := j.srv.pool.Query(r.Context(), q, id, limit)
	if err != nil {
		j.srv.fail(w, r, "get job attempts", err)
		return
	}
	defer rows.Close()

	out := make([]jobAttemptView, 0, 8)
	byDevice := map[string]*attemptDeviceView{}
	outcomes := map[string]int{}
	for rows.Next() {
		var (
			v                                     jobAttemptView
			device, farmUID, model, serial        *string
			devpath, leaseID, leaseState, release *string
			outcome, errText                      *string
			duration                              *float64
		)
		if err := rows.Scan(&v.ID, &v.Attempt, &device,
			&farmUID, &model, &serial, &devpath,
			&leaseID, &v.Fence, &leaseState, &release,
			&v.StartedAt, &v.FinishedAt, &duration, &outcome, &errText); err != nil {
			j.srv.fail(w, r, "scan job attempt", err)
			return
		}
		v.DeviceID = derefString(device)
		v.FarmUID = derefString(farmUID)
		v.Model = derefString(model)
		v.AdbSerial = derefString(serial)
		v.CurrentDevpath = derefString(devpath)
		v.LeaseID = derefString(leaseID)
		v.LeaseState = derefString(leaseState)
		v.ReleaseReason = derefString(release)
		v.Outcome = derefString(outcome)
		v.Error = derefString(errText)
		v.DurationS = duration

		if v.Outcome != "" {
			outcomes[v.Outcome]++
		}
		// A device_id of NULL is a retired handset: farm.job_attempts sets it
		// to NULL on delete, so the placement is still a fact even though the
		// hardware is gone. It is counted in the list and left out of the
		// per-device tally, which has nothing left to key on.
		if v.DeviceID != "" {
			d, seen := byDevice[v.DeviceID]
			if !seen {
				d = &attemptDeviceView{
					DeviceID: v.DeviceID,
					FarmUID:  v.FarmUID,
					Model:    v.Model,
					Outcomes: map[string]int{},
				}
				byDevice[v.DeviceID] = d
			}
			d.Attempts++
			if v.Outcome != "" {
				d.Outcomes[v.Outcome]++
			}
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		j.srv.fail(w, r, "read job attempts", err)
		return
	}

	devices := make([]attemptDeviceView, 0, len(byDevice))
	for _, d := range byDevice {
		devices = append(devices, *d)
	}
	// Busiest device first, then by id, so the row worth suspecting is at the
	// top and two identical calls render identically.
	sort.Slice(devices, func(a, b int) bool {
		if devices[a].Attempts != devices[b].Attempts {
			return devices[a].Attempts > devices[b].Attempts
		}
		return devices[a].DeviceID < devices[b].DeviceID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":       id,
		"job_state":    head.State,
		"attempt":      head.Attempt,
		"max_attempts": head.MaxAttempts,
		"attempts":     out,
		"outcomes":     outcomes,
		// distinct_devices against the number of attempts is the whole
		// diagnosis: equal means every placement was somewhere new, one means
		// the same handset failed over and over.
		"distinct_devices": len(devices),
		"by_device":        devices,
		"truncated":        len(out) == limit,
	})
}
