package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// CodeInvalidSpec accompanies 400 when a job spec cannot run.
//
// It is distinct from CodeBadRequest because its Detail carries a list a
// client is meant to iterate — every problem in the document, not the first —
// and a caller that renders problems needs to know the field is there.
const CodeInvalidSpec = "invalid_spec"

// CodeProfileUnusable accompanies 409 from GET /api/v1/specs/resets when a
// profile's package list cannot be expanded into steps. The request was fine;
// the stored row is wrong, and the answer to "what will medium run on my
// device" is honestly "nothing, until somebody fixes farm.profiles".
const CodeProfileUnusable = "profile_unusable"

// maxValidateProblems bounds what one validation response renders.
//
// jobspec.Validate deliberately returns every problem, and this endpoint
// exists to hand every one of them back — but a thousand-step spec where every
// step is wrong would produce a response nobody can read and a response body
// nobody wants to allocate. The count is always exact even when the list is
// cut, so an author is never told there are fewer problems than there are.
const maxValidateProblems = 500

// maxResetExpansions and maxRenderedResetSteps bound what the reset half of a
// validation response renders.
//
// They exist because this endpoint is the one place in the API where a bounded
// request buys unbounded work. A 1 MiB body holds about 19 000 reset steps;
// a profile with 300 packages expands each of them into 307. Before these
// bounds one authenticated tenant request produced a 1.4 GB response body in
// 21 seconds of CPU — measured, not hypothesised — and writeJSON marshals into
// a buffer before writing, so that is 1.4 GB resident in one allocation. A few
// of those concurrently is an OOM, and a control plane that dies cannot answer
// renewals; every holder then runs out its TTL and grace and loses its device.
// That is STF #663 reached through the front door, so the response is capped
// and the true counts are reported beside the cap.
const (
	maxResetExpansions    = 16
	maxRenderedResetSteps = 2000
)

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// specReport is the verdict on one spec document.
type specReport struct {
	Valid    bool              `json:"valid"`
	Problems []jobspec.Problem `json:"problems"`

	// ProblemCount is the true number, which is larger than len(Problems) when
	// the list was cut at maxValidateProblems.
	ProblemCount int  `json:"problem_count"`
	Truncated    bool `json:"problems_truncated,omitempty"`

	Steps         int     `json:"steps"`
	ResetSteps    int     `json:"reset_steps,omitempty"`
	TotalTimeoutS float64 `json:"total_timeout_s,omitempty"`

	// Artifacts is every digest the spec names, with whether the farm actually
	// holds it. A spec whose content has not been uploaded is a spec that will
	// fail at provisioning, and finding that out here costs one request rather
	// than one lease.
	Artifacts []specArtifactRef `json:"artifacts,omitempty"`

	// Resets is what each reset step in the spec expands to under the profile
	// that was named. Absent when no profile was given: without one the farm
	// does not know which packages the tier owns, and guessing would show an
	// operator a plan that is not the plan.
	Resets []resetExpansion `json:"resets,omitempty"`

	// ResetsTruncated marks a spec with more reset steps than one response
	// renders. ResetSteps above is still the true number, and every one of
	// them was checked for id collisions whether or not it is described here.
	ResetsTruncated bool `json:"resets_truncated,omitempty"`
}

// specArtifactRef is one artifact a spec names by digest.
//
// Present is a pointer because "the farm does not hold this" and "nobody
// asked" are different answers, and a client that treated the second as the
// first would tell an author to upload content that is already there.
type specArtifactRef struct {
	Path    string `json:"path"`
	StepID  string `json:"step_id"`
	SHA256  string `json:"sha256"`
	Present *bool  `json:"present,omitempty"`
}

// resetExpansion is what one reset tier becomes for one profile.
type resetExpansion struct {
	Tier          string         `json:"tier"`
	StepCount     int            `json:"step_count"`
	TotalTimeoutS float64        `json:"total_timeout_s"`
	Steps         []jobspec.Step `json:"steps"`

	// StepID names the spec step this expansion belongs to, and is empty for
	// the standalone tier listing at GET /api/v1/specs/resets.
	StepID string `json:"step_id,omitempty"`
	Path   string `json:"path,omitempty"`

	// StepsOmitted marks an expansion whose Steps were left out because the
	// response had already rendered maxRenderedResetSteps of them. StepCount
	// and TotalTimeoutS are still exact, so the shape of the plan survives
	// even when the plan itself does not fit.
	StepsOmitted bool `json:"steps_omitted,omitempty"`
}

// stepKindView is one row of farm.step_kinds, plus what this build makes of it.
type stepKindView struct {
	Kind          string `json:"kind"`
	Description   string `json:"description"`
	Idempotent    bool   `json:"idempotent"`
	NeedsArtifact bool   `json:"needs_artifact"`

	// Supported is false when the database offers a kind this binary has no
	// payload type for. Such a step is REFUSED rather than skipped — see
	// jobspec.Step.UnmarshalJSON — so a client must not build a spec around
	// it, and this flag is how it finds out without submitting one.
	Supported bool `json:"supported"`

	// DisagreesWithBuild marks a row whose flags differ from the vocabulary
	// compiled into this binary. It means the schema and the process that
	// executes it have drifted, which decides whether a resume may re-run a
	// step — the difference between a repeated install and a repeated payment.
	DisagreesWithBuild bool `json:"disagrees_with_build,omitempty"`
}

// assertOperators mirrors the Operator constants in internal/jobspec.
//
// Spelled with the constants rather than with strings, so renaming one breaks
// this build instead of quietly publishing a vocabulary the server does not
// accept. Numeric is derived, never repeated.
var assertOperators = []jobspec.Operator{
	jobspec.OpEQ, jobspec.OpNE, jobspec.OpContains, jobspec.OpNotContains,
	jobspec.OpMatches, jobspec.OpGT, jobspec.OpGE, jobspec.OpLT, jobspec.OpLE,
}

// resetTiers is farm.jobs.reset_tier's CHECK, in escalation order.
var resetTiers = []jobspec.ResetTier{
	jobspec.TierNone, jobspec.TierSoft, jobspec.TierMedium, jobspec.TierHard,
}

// ---------------------------------------------------------------------------
// POST /api/v1/specs/validate
// ---------------------------------------------------------------------------

// specValidateRequest is the wrapped form of the request body.
type specValidateRequest struct {
	Spec json.RawMessage `json:"spec"`

	// Profile expands the spec's reset steps against farm.profiles.packages,
	// so an author sees the commands a tier will actually run.
	Profile string `json:"profile,omitempty"`

	// CheckArtifacts defaults to true. It is a pointer because "false" and
	// "not stated" must be distinguishable for a default of true.
	CheckArtifacts *bool `json:"check_artifacts,omitempty"`
}

// handleSpecValidate serves POST /api/v1/specs/validate.
//
// The body may be the spec document itself or {"spec": …, "profile": …}. Both
// are accepted because an author pastes the file they are editing and a tool
// sends a request it composed, and refusing one of them turns a validator into
// a thing you have to read the docs for before it will help you.
//
// A spec that cannot run is answered with 200 and valid:false, not with 4xx.
// The question was "can this run", the server answered it, and the request
// succeeded; reserving the error envelope for requests the server could not
// answer is what lets a client tell "your spec is wrong" from "the control
// plane is down" without parsing prose. POST /api/v1/jobs is where an invalid
// spec becomes a 400, because there the client asked for something to happen.
func (s *Server) handleSpecValidate(w http.ResponseWriter, r *http.Request) {
	body, err := readBoundedBody(w, r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			badJSON(w, err)
			return
		}
		s.fail(w, r, "validate spec: read body", err)
		return
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidJSON,
			"the request body must be a JSON object: either the spec document itself, "+
				"or {\"spec\": {…}} with the optional profile and check_artifacts fields: "+err.Error(), nil)
		return
	}

	req := specValidateRequest{Spec: body}
	if _, wrapped := probe["spec"]; wrapped {
		req = specValidateRequest{}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			badJSON(w, err)
			return
		}
	}

	checkArtifacts := req.CheckArtifacts == nil || *req.CheckArtifacts
	report, err := s.checkSpec(r.Context(), req.Spec, strings.TrimSpace(req.Profile), checkArtifacts)
	if err != nil {
		s.fail(w, r, "validate spec", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// readBoundedBody reads a request body under the same cap decodeJSON applies.
//
// The whole body is needed at once here — the wrapped and bare forms cannot be
// told apart from a stream — which is safe only because the cap makes "the
// whole body" a bounded quantity.
func readBoundedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, maxRequestBody)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(limited); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// checkSpec is the whole verdict: decode, validate, then the two questions
// jobspec deliberately does not answer because the database owns them —
// whether the content exists, and what a reset tier expands to here.
func (s *Server) checkSpec(ctx context.Context, raw []byte, profile string, checkArtifacts bool) (specReport, error) {
	report := specReport{Problems: []jobspec.Problem{}}

	if !specSupplied(raw) {
		report.Problems = append(report.Problems, jobspec.Problem{
			Path: "spec",
			Message: "no spec document was supplied; send the spec itself as the body, " +
				"or {\"spec\": {…}}",
		})
		return finishReport(report), nil
	}

	var spec jobspec.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		// A document that will not decode has exactly one problem, and it is
		// this one: the strict decoder in internal/jobspec already names the
		// step and the key it choked on.
		report.Problems = append(report.Problems, jobspec.Problem{Path: "spec", Message: err.Error()})
		return finishReport(report), nil
	}

	report.Steps = len(spec.Steps)
	report.TotalTimeoutS = spec.TotalTimeout().Seconds()
	report.Problems = append(report.Problems, specProblems(spec)...)

	report.Artifacts = specArtifactRefs(spec)
	if checkArtifacts && len(report.Artifacts) > 0 {
		present, err := s.artifactsPresent(ctx, report.Artifacts)
		if err != nil {
			return specReport{}, err
		}
		for i := range report.Artifacts {
			ref := &report.Artifacts[i]
			held := present[ref.SHA256]
			ref.Present = &held
			if held {
				continue
			}
			report.Problems = append(report.Problems, jobspec.Problem{
				Path: ref.Path,
				Message: "farm.artifacts holds no content with this digest; upload the bytes " +
					"before submitting a job that names them, or the step fails at provisioning " +
					"with a device already leased",
			})
		}
	}

	for _, st := range spec.Steps {
		if _, ok := st.Payload.(jobspec.Reset); ok {
			report.ResetSteps++
		}
	}
	if profile != "" && report.ResetSteps > 0 {
		expansions, truncated, problems, err := s.expandSpecResets(ctx, spec, profile)
		if err != nil {
			return specReport{}, err
		}
		report.Resets = expansions
		report.ResetsTruncated = truncated
		report.Problems = append(report.Problems, problems...)
	}

	return finishReport(report), nil
}

// scanSteps is the prefix of a spec that the database-backed checks inspect.
//
// jobspec.Validate stops at MaxSteps and says so, because past the cap the
// document is already rejected and walking a runaway one turns a single bad
// submission into unbounded work. The two checks this package adds — does the
// content exist, what does a reset expand to — have to stop at the same place
// for the same reason, and stopping anywhere else would report a spec's
// artifacts against steps the validator never looked at.
func scanSteps(spec jobspec.Spec) []jobspec.Step {
	if len(spec.Steps) > jobspec.MaxSteps {
		return spec.Steps[:jobspec.MaxSteps]
	}
	return spec.Steps
}

// finishReport fills in the counts and applies the render cap.
func finishReport(r specReport) specReport {
	r.ProblemCount = len(r.Problems)
	r.Valid = r.ProblemCount == 0
	if r.ProblemCount > maxValidateProblems {
		r.Problems = r.Problems[:maxValidateProblems]
		r.Truncated = true
	}
	return r
}

// specProblems runs jobspec.Validate and flattens its error.
func specProblems(spec jobspec.Spec) []jobspec.Problem {
	err := jobspec.Validate(spec)
	if err == nil {
		return nil
	}
	var ve *jobspec.ValidationError
	if errors.As(err, &ve) {
		return ve.Problems
	}
	// Validate returns *ValidationError for every well-formed document, so
	// this is a shape the package does not currently produce. Reporting it as
	// a problem rather than dropping it means a future error type shows up as
	// an unhelpful message instead of as a spec that validated clean.
	return []jobspec.Problem{{Path: "spec", Message: err.Error()}}
}

// specArtifactRefs collects every digest the spec names.
//
// The switch is over the payload types rather than over
// farm.step_kinds.needs_artifact because the sha lives in a differently named
// field per payload; the flag says THAT a step needs content, the type says
// WHERE the digest is. Malformed digests are skipped: jobspec.Validate already
// reports those, and asking the database about a value that cannot be a
// primary key adds a second message about one mistake.
func specArtifactRefs(spec jobspec.Spec) []specArtifactRef {
	var out []specArtifactRef
	for i, st := range scanSteps(spec) {
		var sha, field string
		switch p := st.Payload.(type) {
		case jobspec.Push:
			sha, field = p.SHA256, "push"
		case jobspec.Install:
			sha, field = p.SHA256, "install"
		default:
			continue
		}
		if len(sha) != 64 {
			continue
		}
		out = append(out, specArtifactRef{
			Path:   fmt.Sprintf("steps[%d].%s.sha256", i, field),
			StepID: st.ID,
			SHA256: strings.ToLower(sha),
		})
	}
	return out
}

// artifactsPresent asks farm.artifacts which of these digests it holds, in one
// round trip rather than one per step.
func (s *Server) artifactsPresent(ctx context.Context, refs []specArtifactRef) (map[string]bool, error) {
	shas := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !seen[ref.SHA256] {
			seen[ref.SHA256] = true
			shas = append(shas, ref.SHA256)
		}
	}

	rows, err := s.pool.Query(ctx,
		`SELECT sha256 FROM farm.artifacts WHERE sha256 = ANY($1::text[])`, shas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	present := make(map[string]bool, len(shas))
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		present[sha] = true
	}
	return present, rows.Err()
}

// expandSpecResets expands each reset step in the spec under one profile.
//
// The id collision check is the reason this is worth doing at validation time.
// ResetSteps produces ids that are stable for a tier and a package list, so a
// spec with two resets of the same tier expands into two sets of identical
// ids — which farm.jobs.checkpoint could not tell apart on a resume. Reporting
// it here names the reset that collided and the first id it took; discovering
// it later means a job that cannot be resumed after a crash it has not had yet.
//
// Every reset step in the scanned prefix is checked for collisions. Only the
// RENDERING of the expansions is capped, so a response that ran out of room to
// describe a plan never reports the plan as sound.
func (s *Server) expandSpecResets(ctx context.Context, spec jobspec.Spec, profile string) (
	[]resetExpansion, bool, []jobspec.Problem, error) {

	packages, err := s.profilePackages(ctx, profile)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, []jobspec.Problem{{
				Path:    "profile",
				Message: fmt.Sprintf("no such profile in farm.profiles: %q", profile),
			}}, nil
		}
		return nil, false, nil, err
	}

	steps := scanSteps(spec)

	// Seeded with the spec's own ids, so an expansion that collides with a
	// hand-written step is caught as well as one that collides with another
	// expansion.
	ids := make(map[string]string, len(steps)*2)
	for i, st := range steps {
		if st.ID != "" {
			ids[st.ID] = fmt.Sprintf("steps[%d]", i)
		}
	}

	// Expanded once per TIER, not once per reset step. ResetSteps is a pure
	// function of the tier and the package list, and the package list is fixed
	// for one request — so a spec carrying a thousand resets was building the
	// same thousand-step answer a thousand times, which is where the
	// gigabytes came from. The vocabulary has four tiers.
	type expansion struct {
		steps []jobspec.Step
		err   error
	}
	byTier := make(map[jobspec.ResetTier]expansion, len(resetTiers))

	var (
		expansions []resetExpansion
		problems   []jobspec.Problem
		budget     = maxRenderedResetSteps
		truncated  bool
	)
	for i, st := range steps {
		reset, ok := st.Payload.(jobspec.Reset)
		if !ok {
			continue
		}
		path := fmt.Sprintf("steps[%d].reset.tier", i)

		ex, cached := byTier[reset.Tier]
		if !cached {
			ex.steps, ex.err = jobspec.ResetSteps(reset.Tier, packages)
			byTier[reset.Tier] = ex
		}
		if ex.err != nil {
			problems = append(problems, jobspec.Problem{Path: path, Message: ex.err.Error()})
			continue
		}

		// One message per offending reset step, not one per colliding id: a
		// soft reset over a 300-package profile collides 300 times over a
		// single mistake, and 300 messages about it is a report nobody reads
		// and a slice nobody wants to allocate.
		clashes := 0
		firstClash := ""
		for _, expanded := range ex.steps {
			if owner, clash := ids[expanded.ID]; clash {
				clashes++
				if firstClash == "" {
					firstClash = fmt.Sprintf("%q (already used by %s)", expanded.ID, owner)
				}
				continue
			}
			ids[expanded.ID] = path
		}
		if clashes > 0 {
			problems = append(problems, jobspec.Problem{
				Path: path,
				Message: fmt.Sprintf("expanding this reset produces %d step id(s) that are already taken, "+
					"starting with %s; step ids are unique within a spec because checkpoints resolve them, "+
					"so rename the conflicting step or expand only one reset per spec", clashes, firstClash),
			})
		}

		// The collision check above ran for every reset step; only the
		// RENDERING is capped, so a spec is never told its ids are fine
		// merely because the response ran out of room to describe them.
		if len(expansions) >= maxResetExpansions {
			truncated = true
			continue
		}
		view := resetExpansion{
			Tier:          string(reset.Tier),
			StepID:        st.ID,
			Path:          fmt.Sprintf("steps[%d]", i),
			StepCount:     len(ex.steps),
			TotalTimeoutS: jobspec.Spec{Steps: ex.steps}.TotalTimeout().Seconds(),
		}
		if len(ex.steps) <= budget {
			view.Steps = ex.steps
			budget -= len(ex.steps)
		} else {
			view.StepsOmitted = true
		}
		expansions = append(expansions, view)
	}
	return expansions, truncated, problems, nil
}

// profilePackages reads farm.profiles.packages.
func (s *Server) profilePackages(ctx context.Context, profile string) ([]string, error) {
	var packages []string
	err := s.pool.QueryRow(ctx,
		`SELECT packages FROM farm.profiles WHERE id = $1`, profile).Scan(&packages)
	return packages, err
}

// specSupplied reports whether raw carries a spec at all.
//
// Absent, null and {} all mean "no automated instructions", which is what
// farm.jobs.spec's own DEFAULT '{}' means and what an interactive lease looks
// like: a human takes the device, and the job carries no steps for a runner to
// execute. Those are accepted. Anything else is a document whose author
// intended something, and it has to be able to run.
func specSupplied(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err == nil && len(probe) == 0 {
		return false
	}
	return true
}

// specSubmissionError is the gate POST /api/v1/jobs applies to a spec.
//
// It returns nil when the job may be filed, and the 400 body otherwise. A job
// whose spec cannot run is worse than a rejected request: it sits in
// farm.jobs, the scheduler allocates a device to it through farm.lease_acquire,
// and the runner discovers at the first step that the document is nonsense —
// so a phone was taken out of the pool to fail. Refusing at submission costs a
// round trip and nothing else.
//
// It touches no database, so the caller can apply it before resolving pools,
// queues and tenants and hand back every problem in one reply.
func specSubmissionError(raw json.RawMessage) *APIError {
	if !specSupplied(raw) {
		return nil
	}

	var spec jobspec.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return invalidSpecError([]jobspec.Problem{{Path: "spec", Message: err.Error()}})
	}
	if problems := specProblems(spec); len(problems) > 0 {
		return invalidSpecError(problems)
	}
	return nil
}

// invalidSpecError renders a problem list as the API's error envelope.
func invalidSpecError(problems []jobspec.Problem) *APIError {
	shown := problems
	truncated := false
	if len(shown) > maxValidateProblems {
		shown = shown[:maxValidateProblems]
		truncated = true
	}
	detail := map[string]any{
		"problems":      shown,
		"problem_count": len(problems),
	}
	if truncated {
		detail["problems_truncated"] = true
	}
	return &APIError{
		Code: CodeInvalidSpec,
		Message: fmt.Sprintf("this job spec cannot run: %d problem(s). Every one of them is in "+
			"detail.problems so the spec can be fixed in one round trip; "+
			"POST /api/v1/specs/validate answers the same question without filing a job.",
			len(problems)),
		Detail: detail,
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/specs/kinds
// ---------------------------------------------------------------------------

// handleSpecKinds serves the step vocabulary from farm.step_kinds.
//
// It reads the DATABASE, not the table compiled into this binary, because the
// database is what farm.job_steps.kind is a foreign key against — and then it
// compares the two and says where they disagree. That comparison is the useful
// part: a control plane whose migration is newer than its binary will accept a
// spec naming a kind it cannot execute, and a client building a spec from this
// list deserves to know which kinds this server can actually run.
func (s *Server) handleSpecKinds(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(),
		`SELECT kind, description, idempotent, needs_artifact FROM farm.step_kinds ORDER BY kind`)
	if err != nil {
		s.fail(w, r, "list step kinds", err)
		return
	}
	defer rows.Close()

	build := make(map[jobspec.Kind]jobspec.KindInfo, len(jobspec.Kinds()))
	for _, info := range jobspec.Kinds() {
		build[info.Kind] = info
	}

	out := make([]stepKindView, 0, len(build))
	inDatabase := map[jobspec.Kind]bool{}
	for rows.Next() {
		var v stepKindView
		if err := rows.Scan(&v.Kind, &v.Description, &v.Idempotent, &v.NeedsArtifact); err != nil {
			s.fail(w, r, "scan step kind", err)
			return
		}
		info, known := build[jobspec.Kind(v.Kind)]
		v.Supported = known
		inDatabase[jobspec.Kind(v.Kind)] = true
		if known && (info.Idempotent != v.Idempotent || info.NeedsArtifact != v.NeedsArtifact) {
			v.DisagreesWithBuild = true
			s.log.WarnContext(r.Context(),
				"farm.step_kinds disagrees with the vocabulary compiled into this binary; "+
					"a resume decides whether it may re-run a step from these flags",
				"kind", v.Kind,
				"db_idempotent", v.Idempotent, "build_idempotent", info.Idempotent,
				"db_needs_artifact", v.NeedsArtifact, "build_needs_artifact", info.NeedsArtifact)
		}
		if !known {
			s.log.WarnContext(r.Context(),
				"farm.step_kinds offers a step kind this binary cannot execute; "+
					"a spec naming it is refused at parse time, not skipped",
				"kind", v.Kind)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read step kinds", err)
		return
	}

	missing := make([]string, 0)
	for _, info := range jobspec.Kinds() {
		if !inDatabase[info.Kind] {
			missing = append(missing, string(info.Kind))
		}
	}

	operators := make([]map[string]any, 0, len(assertOperators))
	for _, op := range assertOperators {
		operators = append(operators, map[string]any{
			"op":      string(op),
			"numeric": op.Numeric(),
		})
	}
	tiers := make([]string, 0, len(resetTiers))
	for _, t := range resetTiers {
		tiers = append(tiers, string(t))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"spec_version":     jobspec.SpecVersion,
		"kinds":            out,
		"assert_operators": operators,
		"reset_tiers":      tiers,
		// Named so a client can see why a spec was refused for a bound rather
		// than for its content, and size a generated spec before sending it.
		"limits": map[string]any{
			"max_steps":             jobspec.MaxSteps,
			"max_step_id_len":       jobspec.MaxStepIDLen,
			"max_artifact_name_len": jobspec.MaxArtifactNameLen,
			"max_step_timeout_s":    jobspec.MaxStepTimeout.Seconds(),
			"max_total_timeout_s":   jobspec.MaxTotalTimeout.Seconds(),
			"max_expect_exit":       jobspec.MaxExpectExit,
		},
		// Non-empty means this binary is older or newer than the schema it is
		// running against.
		"missing_from_database": missing,
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/specs/resets
// ---------------------------------------------------------------------------

// handleSpecResets shows what a reset tier will actually run on a device.
//
// "soft", "medium" and "hard" are words until somebody expands them, and the
// expansion depends on a profile's package list, which is a row an operator
// can edit. This endpoint is how they read the consequence before a job does:
// the exact shell commands, in order, with the timeouts they will be given.
func (s *Server) handleSpecResets(w http.ResponseWriter, r *http.Request) {
	profile := queryString(r, "profile")
	if profile == "" {
		badRequest(w, "profile is required: a reset tier has no meaning until the profile's "+
			"package list says which packages it owns", nil)
		return
	}

	var (
		description *string
		packages    []string
	)
	err := s.pool.QueryRow(r.Context(),
		`SELECT description, packages FROM farm.profiles WHERE id = $1`, profile).
		Scan(&description, &packages)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound,
				"no such profile: "+profile, nil)
			return
		}
		s.fail(w, r, "get profile", err)
		return
	}

	tiers := resetTiers
	if raw := strings.ToLower(queryString(r, "tier")); raw != "" {
		tier := jobspec.ResetTier(raw)
		if !tier.Valid() {
			badRequest(w, "tier must be one of none, soft, medium, hard",
				map[string]any{"permitted_tiers": resetTiers})
			return
		}
		tiers = []jobspec.ResetTier{tier}
	}

	out := make([]resetExpansion, 0, len(tiers))
	for _, tier := range tiers {
		steps, err := jobspec.ResetSteps(tier, packages)
		if err != nil {
			// The package list itself is unusable, so every tier fails the
			// same way. This is a refusal to run commands built out of names
			// that were never checked, on a device somebody else may be
			// holding.
			writeError(w, http.StatusConflict, CodeProfileUnusable, err.Error(),
				map[string]any{"profile": profile, "packages": packages})
			return
		}
		out = append(out, resetExpansion{
			Tier:          string(tier),
			StepCount:     len(steps),
			TotalTimeoutS: jobspec.Spec{Steps: steps}.TotalTimeout().Seconds(),
			Steps:         steps,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"profile":     profile,
		"description": derefString(description),
		"packages":    packages,
		"resets":      out,
		// medium and hard run this script, and it is not an artifact: the
		// runner writes it from farm.device_state after reading the device's
		// current row, so it cannot be named by content hash in a spec.
		"device_state_script": jobspec.DeviceStateScript,
	})
}
