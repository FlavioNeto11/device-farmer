package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// ---------------------------------------------------------------------------
// The spec surface: validate, kinds, resets, submit.
//
// Every command in this file asks the control plane. That is the entire point
// of it. ctl used to answer "is this spec valid" out of its own linked copy of
// internal/jobspec, which made the answer a fact about the binary on the
// laptop rather than about the farm the job would run on. Those are different
// questions, and four of the differences cost hardware:
//
//   - jobspec.Validate cannot know whether farm.artifacts holds the bytes an
//     install or a push names. A spec that passes locally and names a digest
//     nobody uploaded is accepted, filed, allocated a device, and fails at
//     provisioning WITH THE PHONE ALREADY LEASED — a handset taken out of the
//     pool in order to fail.
//   - it cannot expand a reset tier. "medium" is a word until farm.profiles
//     says which packages the profile owns.
//   - it cannot know which selector keys this farm understands, and
//     farm.lease_acquire REFUSES a job carrying an unknown one rather than
//     ignoring it — so the job is never placed and never rejected either.
//   - a ctl older than its control plane has an older step vocabulary, and a
//     ctl newer than it has one the database has never heard of.
//
// So the server decides every time and this file renders what it decided. The
// converse mistake is just as bad and is why `ctl kinds` exists: a CLI with the
// vocabulary hard-coded refuses specs the farm would happily run.
//
// Nothing here can end a lease. `ctl submit` files a row in farm.jobs; the
// scheduler is what turns it into a lease.
// ---------------------------------------------------------------------------

// specPrefix is the route root the three read-only spec endpoints hang from.
const specPrefix = apiPrefix + "/specs"

// resetCommandCell is how wide the command column of a reset expansion prints.
//
// Far past defaultCellWidth, because a reset command is the thing being
// inspected rather than context for it, and clipping one at sixty characters
// hides which packages the uninstall loop keeps. It is still a clip and not a
// wrap: the grid is what makes a list of thirty commands scannable, and -o json
// carries every command in full.
const resetCommandCell = 150

// ---------------------------------------------------------------------------
// Wire types
//
// These mirror what internal/api emits and carry only what a rendering reads.
// They are not shared with that package: ctl is a client, and a client that
// compiles against the server's structs cannot notice a response it no longer
// understands. -o json passes the server's own bytes through regardless.
// ---------------------------------------------------------------------------

// specReport is the body of POST /api/v1/specs/validate.
type specReport struct {
	Valid             bool              `json:"valid"`
	Problems          []jobspec.Problem `json:"problems"`
	ProblemCount      int               `json:"problem_count"`
	ProblemsTruncated bool              `json:"problems_truncated"`

	Steps         int     `json:"steps"`
	ResetSteps    int     `json:"reset_steps"`
	TotalTimeoutS float64 `json:"total_timeout_s"`

	Artifacts       []specArtifactRef `json:"artifacts"`
	Resets          []specResetPlan   `json:"resets"`
	ResetsTruncated bool              `json:"resets_truncated"`
}

// specArtifactRef is one digest a spec names, and whether the farm holds it.
//
// Present is a pointer because "the store does not have this" and "nobody
// asked" are different answers; rendering the second as the first would tell an
// author to upload content that is already there.
type specArtifactRef struct {
	Path    string `json:"path"`
	StepID  string `json:"step_id"`
	SHA256  string `json:"sha256"`
	Present *bool  `json:"present"`
}

// specResetPlan is what one reset tier expands to for one profile. It is the
// same shape at both endpoints that produce one; StepID and Path are filled in
// only when the expansion belongs to a step of a submitted spec.
type specResetPlan struct {
	Tier          string         `json:"tier"`
	StepCount     int            `json:"step_count"`
	TotalTimeoutS float64        `json:"total_timeout_s"`
	Steps         []specStepView `json:"steps"`
	StepID        string         `json:"step_id"`
	Path          string         `json:"path"`
	StepsOmitted  bool           `json:"steps_omitted"`
}

// specStepView is one step of an expansion, decoded permissively.
//
// It is deliberately NOT jobspec.Step. That type's decoder REFUSES a kind this
// build has no payload for, which is right for a document about to be executed
// and wrong here: `ctl kinds` and `ctl resets` are the two commands somebody
// runs to discover that their ctl and their control plane disagree, and a
// client that cannot display an answer it could not execute turns a version
// skew into silence.
type specStepView struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Timeout         string `json:"timeout"`
	ContinueOnError bool   `json:"continue_on_error"`

	// Payload is the object stored under the key named by Kind — the shell
	// command, the probe, the package. It is read structurally rather than
	// through a type switch over a vocabulary this file would then own.
	Payload map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON reads the fixed fields, then the payload the kind names.
func (v *specStepView) UnmarshalJSON(b []byte) error {
	// A local type without this method, so decoding the fixed fields does not
	// call back into it.
	type plain specStepView
	var head plain
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	*v = specStepView(head)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if raw, ok := fields[v.Kind]; ok {
		// A payload that is not an object leaves Payload nil rather than
		// failing the whole response: the step's id, kind and timeout are
		// still worth showing.
		_ = json.Unmarshal(raw, &v.Payload)
	}
	return nil
}

// specKindRow is one row of GET /api/v1/specs/kinds.
type specKindRow struct {
	Kind               string `json:"kind"`
	Description        string `json:"description"`
	Idempotent         bool   `json:"idempotent"`
	NeedsArtifact      bool   `json:"needs_artifact"`
	Supported          bool   `json:"supported"`
	DisagreesWithBuild bool   `json:"disagrees_with_build"`
}

type specAssertOperator struct {
	Op      string `json:"op"`
	Numeric bool   `json:"numeric"`
}

type specKindsResponse struct {
	SpecVersion     int                  `json:"spec_version"`
	Kinds           []specKindRow        `json:"kinds"`
	AssertOperators []specAssertOperator `json:"assert_operators"`
	ResetTiers      []string             `json:"reset_tiers"`

	// Limits holds whole numbers and second counts together, so the values are
	// kept as json.Number and printed as the server wrote them rather than
	// pushed through a float that renders 2147483647 in exponent notation.
	Limits              map[string]json.Number `json:"limits"`
	MissingFromDatabase []string               `json:"missing_from_database"`
}

type specResetsResponse struct {
	Profile           string          `json:"profile"`
	Description       string          `json:"description"`
	Packages          []string        `json:"packages"`
	Resets            []specResetPlan `json:"resets"`
	DeviceStateScript string          `json:"device_state_script"`
}

// jobSubmitRequest is the body of POST /api/v1/jobs.
//
// The four fields at the bottom are the reason it exists. farm.jobs.profile_id,
// reset_tier, max_attempts and resumable are read by internal/runner on every
// placement, and no ctl could set any of them: every job this tool filed ran
// with a profile_id of NULL, which is a reset with no package list, which
// cleans nothing and reports success.
//
// MaxAttempts and Resumable are pointers because "omitted" and "zero" are
// different requests: resumable:false is a real choice, and max_attempts:0 is a
// mistake the server refuses with a better sentence than any default could.
type jobSubmitRequest struct {
	Pool   string `json:"pool"`
	Queue  string `json:"queue"`
	Tenant string `json:"tenant"`

	Spec     json.RawMessage `json:"spec,omitempty"`
	Selector json.RawMessage `json:"selector,omitempty"`

	ExpectedDurationS int64 `json:"expected_duration_s,omitempty"`
	MaxRuntimeS       int64 `json:"max_runtime_s,omitempty"`

	ProfileID   string `json:"profile_id,omitempty"`
	ResetTier   string `json:"reset_tier,omitempty"`
	MaxAttempts *int   `json:"max_attempts,omitempty"`
	Resumable   *bool  `json:"resumable,omitempty"`
}

// ---------------------------------------------------------------------------
// ctl validate
// ---------------------------------------------------------------------------

func cmdSpecValidate(ctx context.Context, s *session, args []string) error {
	fs := newFlags("validate", s.err)
	var g globals
	g.bind(fs)
	file := fs.String("f", "", "path to the job spec, or - for stdin")
	profile := fs.String("profile", "", "expand this spec's reset steps against this farm.profiles row")
	skipArtifacts := fs.Bool("no-artifact-check", false,
		"do not ask whether the artifacts this spec names are in the store")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("validate takes no positional arguments; pass the spec with -f %s", rest[0])
	}
	if strings.TrimSpace(*file) == "" {
		return usageErrf("validate needs -f <spec.json>")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	raw, err := readSpecFile(*file, s.in)
	if err != nil {
		return err
	}
	if specAbsent(raw) {
		interactiveLeaseNote(e.out.Text, specSource(*file))
		if e.format == FormatJSON {
			return e.out.JSON(specReport{Valid: true, Problems: []jobspec.Problem{}})
		}
		return nil
	}

	report, body, err := e.validateSpec(ctx, raw, strings.TrimSpace(*profile), !*skipArtifacts)
	if err != nil {
		return err
	}

	if e.format == FormatJSON {
		// The report is emitted and THEN the command fails. A gate in CI reads
		// the exit code, not the document; printing a list of defects and
		// exiting 0 would let a broken spec through the one check that exists
		// to stop it.
		if err := e.out.RawJSON(body); err != nil {
			return err
		}
		return specVerdict(e, specSource(*file), report)
	}

	if report.ProblemCount > 0 {
		e.out.Text("%s: %s", specSource(*file), plural(report.ProblemCount, "problem", "problems"))
		e.out.Blank()
		if err := e.out.Fields(problemFields(report.Problems)); err != nil {
			return err
		}
		if report.ProblemsTruncated {
			e.warnf("only the first %d of %d problems are listed; fix these and validate again",
				len(report.Problems), report.ProblemCount)
		}
		e.out.Blank()
	}

	f := &Fields{}
	f.Addf("steps", "%d", report.Steps)
	if report.TotalTimeoutS > 0 {
		f.Add("total timeout", duration(int64(report.TotalTimeoutS)))
	}
	if report.ResetSteps > 0 {
		f.Addf("reset steps", "%d", report.ResetSteps)
	}
	f.Add("checked by", e.client.BaseURL()+specPrefix+"/validate")
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if err := renderSpecArtifacts(e, report.Artifacts); err != nil {
		return err
	}
	if err := renderResetPlans(e, report.Resets, report.ResetsTruncated); err != nil {
		return err
	}
	if report.ResetSteps > 0 && len(report.Resets) == 0 && strings.TrimSpace(*profile) == "" {
		e.out.Blank()
		e.out.Text("this spec resets a device (%s), and with no --profile what those steps "+
			"would actually run is unknown: the package list lives in farm.profiles.",
			plural(report.ResetSteps, "reset step", "reset steps"))
	}
	// --profile that did nothing has to say it did nothing.
	//
	// The endpoint expands resets only when there ARE resets, so a profile named
	// against a spec with none is never looked up — and a misspelled one comes
	// back as a clean exit 0. That is the wrong answer in the one place it
	// costs: this command is what a CI gate runs, the gate goes green, and
	// POST /api/v1/jobs then refuses the identical profile with "no such
	// profile in farm.profiles". A flag that was ignored must not read as a
	// flag that was checked.
	if strings.TrimSpace(*profile) != "" && report.ResetSteps == 0 {
		e.warnf("\n--profile %s had no effect and was NOT verified: this spec carries no reset "+
			"steps, so there was nothing to expand and the profile itself was never looked up. "+
			"A misspelling here still passes this check and is refused at submission.",
			strings.TrimSpace(*profile))
	}
	return specVerdict(e, specSource(*file), report)
}

// specAbsent reports that a file carries no spec document at all.
//
// Absent, null and {} all mean the same thing and all three are legal: {} is
// farm.jobs.spec's own DEFAULT, and a job carrying it is an INTERACTIVE
// LEASE — the scheduler allocates a device, a human holds it, and nothing runs
// on it automatically. POST /api/v1/jobs files one; verified against the live
// farm, the row is created and queued.
//
// It is tested here so that neither command sends an empty body to
// POST /specs/validate. That endpoint answers one with "no spec document was
// supplied", which is a complaint about the REQUEST — the caller handed the
// validator nothing to check — and rendering it as a verdict would leave ctl
// refusing a job the control plane accepts. That is the same defect this file
// exists to remove, pointing the other way.
func specAbsent(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var probe map[string]json.RawMessage
	return json.Unmarshal(trimmed, &probe) == nil && len(probe) == 0
}

// interactiveLeaseNote says what a document with no steps means.
func interactiveLeaseNote(w func(string, ...any), source string) {
	w("%s carries no steps, so there is nothing to validate: a job filed with it is an "+
		"INTERACTIVE LEASE. A device is allocated, a human holds it, and nothing runs on it "+
		"automatically. farm.jobs.spec defaults to {} for exactly that.", source)
}

// validateSpec asks the control plane. It returns the decoded report and the
// server's own bytes, so -o json renders the API's schema rather than a second
// one this package would then have to keep in step.
//
// A document that is not JSON is sent BARE rather than wrapped. The endpoint
// accepts either form, and its decoder reports the syntax error with its
// offset — which is the useful answer — whereas wrapping unparseable bytes in
// {"spec": …} fails inside encoding/json here and reports a marshalling error
// about ctl instead of a syntax error about the file.
func (e *env) validateSpec(ctx context.Context, raw []byte, profile string, checkArtifacts bool) (
	specReport, json.RawMessage, error) {

	const path = specPrefix + "/validate"

	var payload []byte
	if json.Valid(raw) {
		req := struct {
			Spec           json.RawMessage `json:"spec"`
			Profile        string          `json:"profile,omitempty"`
			CheckArtifacts *bool           `json:"check_artifacts,omitempty"`
		}{Spec: json.RawMessage(raw), Profile: profile}
		if !checkArtifacts {
			req.CheckArtifacts = &checkArtifacts
		}
		var err error
		if payload, err = json.Marshal(req); err != nil {
			return specReport{}, nil, fmt.Errorf("encode validation request: %w", err)
		}
	} else {
		payload = raw
		if profile != "" {
			e.warnf("--profile was not sent: the document is not JSON, so it has no reset " +
				"steps to expand until it parses")
		}
	}

	// readJSON rather than Post, because Post marshals its argument and the
	// body here is already bytes — including, in the branch above, bytes that
	// deliberately are not valid JSON.
	body, err := e.client.readJSON(ctx, http.MethodPost, path, nil,
		bytes.NewReader(payload), "application/json")
	if err != nil {
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Status != http.StatusBadRequest {
			return specReport{}, nil, err
		}
		// This endpoint answers a document it can READ with 200 and
		// valid:false, so a 400 means the body was not a spec document at all
		// — a truncated file, a YAML file, a shell redirect that wrote
		// nothing. That is still a verdict on the file rather than a failure of
		// the request, and it has to exit the way an invalid spec exits: no
		// retry makes an unparseable document parse.
		report := specReport{
			Problems:     []jobspec.Problem{{Path: "spec", Message: remote.Message}},
			ProblemCount: 1,
		}
		synth, merr := json.Marshal(report)
		if merr != nil {
			return specReport{}, nil, err
		}
		return report, synth, nil
	}
	var report specReport
	if err := json.Unmarshal(body, &report); err != nil {
		return specReport{}, body, fmt.Errorf("POST %s: the response did not decode: %w", path, err)
	}
	return report, body, nil
}

// specVerdict turns a report into this package's exit code.
//
// A spec the control plane will not run exits 3, not 1. ExitCode calls 3 "the
// remote refused the action", and that is exactly what happened here: the
// server read the document and answered. The distinction is the whole point for
// the script running this in CI — 1 means something broke and asking again may
// work, 3 means no amount of retrying changes the answer. Reporting an invalid
// spec the way a dead socket is reported gets it retried until the pipeline
// times out.
func specVerdict(e *env, source string, report specReport) error {
	if report.Valid {
		e.out.Blank()
		e.out.Text("%s will run: %s, validated by %s", source,
			plural(report.Steps, "step", "steps"), e.client.BaseURL())
		return nil
	}
	if report.ProblemCount == 0 {
		// valid:false with an empty list is a shape this API does not produce —
		// finishReport derives one from the other — so reaching it means the
		// answer came from something that is not this control plane, or from a
		// version of it that changed the contract. Exiting 3 with "0 problems"
		// and nothing above it would be a refusal with no stated reason, which
		// is the failure this whole file exists to remove.
		return fmt.Errorf("%w: %s was refused by %s, which reported no problems to go with the "+
			"refusal; read the answer with -o json", ErrRefused, source, e.client.BaseURL())
	}
	return fmt.Errorf("%w: %s cannot run (%s)", ErrRefused, source,
		plural(report.ProblemCount, "problem", "problems"))
}

// specSource names the document in prose. "-" is the conventional stdin path
// and reads as a stray dash in the middle of a sentence.
func specSource(file string) string {
	if file == "-" {
		return "the spec on stdin"
	}
	return file
}

// problemFields renders every problem with its path, and every problem in
// full.
//
// Every one of them, always: internal/jobspec returns the complete list rather
// than the first defect precisely so that fixing a spec is one edit instead of
// ten round trips, and a client that printed only the first would throw that
// away at the last step.
//
// A Fields block rather than a Table, because Table clips a cell and these
// messages are the answer. "farm.artifacts holds no content with this digest;
// upload the bytes before submitting a job that names them, or the step fails
// at provisioning with a device already leased" cut at a column boundary loses
// the clause that says what it costs — and a validator that hides what the
// server said is the exact defect this file exists to remove.
func problemFields(problems []jobspec.Problem) *Fields {
	f := &Fields{}
	for _, p := range problems {
		f.Add(firstNonEmpty(p.Path, "(document)"), p.Message)
	}
	return f
}

// renderSpecArtifacts shows every digest the spec names and whether the farm
// holds the bytes.
//
// A missing one is the defect this command exists to catch: it is invisible to
// any validator that does not talk to the farm, and it is paid for with a
// leased device at provisioning time rather than with a round trip here.
func renderSpecArtifacts(e *env, refs []specArtifactRef) error {
	if len(refs) == 0 {
		return nil
	}
	e.out.Blank()
	e.out.Text("artifacts this spec names:")
	t := NewTable("WHERE", "STEP", "SHA256", "IN STORE")
	missing := 0
	for _, ref := range refs {
		held := "not asked"
		if ref.Present != nil {
			held = yesNo(*ref.Present)
			if !*ref.Present {
				missing++
			}
		}
		t.Row(ref.Path, firstNonEmpty(ref.StepID, "—"), shortID(ref.SHA256), held)
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	if missing > 0 {
		e.out.Text("upload the missing bytes before submitting: ctl push <file>")
	}
	e.out.Text("digests are abbreviated here; -o json carries them in full")
	return nil
}

// renderResetPlans prints the commands a reset tier expands to, in order.
func renderResetPlans(e *env, plans []specResetPlan, truncated bool) error {
	if len(plans) == 0 {
		return nil
	}
	for _, plan := range plans {
		e.out.Blank()
		where := ""
		if plan.Path != "" {
			where = fmt.Sprintf(" (%s%s)", plan.Path, stepIDSuffix(plan.StepID))
		}
		e.out.Text("reset tier %q%s runs %s, %s of timeout:", plan.Tier, where,
			plural(plan.StepCount, "step", "steps"), duration(int64(plan.TotalTimeoutS)))
		if plan.StepsOmitted {
			e.out.Text("  the commands were left out of the response for its size budget; "+
				"ask for this tier alone: ctl resets --tier %s", plan.Tier)
			continue
		}
		if len(plan.Steps) == 0 {
			continue
		}
		t := NewTable("#", "STEP", "KIND", "TIMEOUT", "COMMAND").MaxCell(resetCommandCell)
		for i, st := range plan.Steps {
			t.Row(strconv.Itoa(i), st.ID, st.Kind, firstNonEmpty(st.Timeout, "—"), specStepDetail(st))
		}
		if err := e.out.Table(t); err != nil {
			return err
		}
	}
	if truncated {
		e.warnf("more reset steps than one response renders were expanded; every one of them was " +
			"still checked for id collisions, and --tier narrows what is described")
	}
	return nil
}

func stepIDSuffix(id string) string {
	if id == "" {
		return ""
	}
	return ", step " + id
}

// specStepDetail renders a step's payload without owning a vocabulary.
//
// The payload keys are the step's own, so a single-field payload prints its
// value — which for every reset step is the shell command about to run on
// somebody's phone — and a wider one prints key=value pairs. A kind added to
// the server after this binary was built therefore still renders.
func specStepDetail(v specStepView) string {
	if len(v.Payload) == 0 {
		return ""
	}
	keys := make([]string, 0, len(v.Payload))
	for k := range v.Payload {
		keys = append(keys, k)
	}
	if len(keys) == 1 {
		return jsonScalar(v.Payload[keys[0]])
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+jsonScalar(v.Payload[k]))
	}
	return strings.Join(parts, "  ")
}

// jsonScalar unquotes a JSON string and passes anything else through, so a
// shell command renders as itself rather than as an escaped literal.
func jsonScalar(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// ---------------------------------------------------------------------------
// ctl kinds
// ---------------------------------------------------------------------------

// cmdSpecKinds prints the step vocabulary THIS control plane accepts.
//
// It exists so that nothing — not a script, not a generator, not this CLI — has
// to hard-code the list. A hard-coded vocabulary is wrong in both directions:
// it refuses a kind the farm gained in a migration, and it cheerfully builds a
// spec around a kind this server cannot execute. The endpoint reads
// farm.step_kinds and compares it against what the server binary can run, so
// the two disagreement columns below answer a class of bug that is otherwise
// only visible as a job failing at its first step.
func cmdSpecKinds(ctx context.Context, s *session, args []string) error {
	fs := newFlags("kinds", s.err)
	var g globals
	g.bind(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("kinds takes no arguments, and got %q", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	resp, raw, err := fetch[specKindsResponse](ctx, e.client, specPrefix+"/kinds", nil)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	t := NewTable("KIND", "RUNNABLE", "IDEMPOTENT", "ARTIFACT", "DESCRIPTION").MaxCell(96)
	unsupported, disagreeing := 0, 0
	for _, k := range resp.Kinds {
		runnable := yesNo(k.Supported)
		if !k.Supported {
			// Shouted, because a spec naming this kind is refused at parse
			// time rather than skipped: the farm offers a word its own runner
			// cannot execute.
			runnable = "NO"
			unsupported++
		}
		if k.DisagreesWithBuild {
			disagreeing++
		}
		t.Row(k.Kind, runnable, yesNo(k.Idempotent), yesNo(k.NeedsArtifact), k.Description)
	}
	if err := e.out.Table(t); err != nil {
		return err
	}

	e.out.Blank()
	f := &Fields{}
	f.Addf("spec version", "%d", resp.SpecVersion)
	f.Add("reset tiers", strings.Join(resp.ResetTiers, ", "))
	f.Add("assert operators", assertOperatorList(resp.AssertOperators))
	f.Add("from", e.client.BaseURL()+specPrefix+"/kinds")
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if len(resp.Limits) > 0 {
		e.out.Blank()
		e.out.Text("limits — a spec over one of these is refused for its size, not its content:")
		keys := make([]string, 0, len(resp.Limits))
		for k := range resp.Limits {
			keys = append(keys, k)
		}
		sortStrings(keys)
		lf := &Fields{}
		for _, k := range keys {
			lf.Add(k, resp.Limits[k].String())
		}
		if err := e.out.Fields(lf); err != nil {
			return err
		}
	}

	// The three skew reports go to stderr. They are not the answer to "what may
	// I write", they are a warning about this deployment, and a pipeline
	// reading the table must not swallow them.
	if unsupported > 0 {
		e.warnf("\n%s in farm.step_kinds cannot be executed by the server binary running this "+
			"farm: a spec naming one is REFUSED at parse time, not skipped. The schema is newer "+
			"than the process; upgrade the control plane before writing a spec around it.",
			plural(unsupported, "kind", "kinds"))
	}
	if disagreeing > 0 {
		e.warnf("\n%s carry flags in farm.step_kinds that differ from the binary's own. Those "+
			"flags decide whether a resume may RE-RUN a step, which is the difference between a "+
			"repeated install and a repeated payment. Reconcile the migration and the build.",
			plural(disagreeing, "kind", "kinds"))
	}
	if len(resp.MissingFromDatabase) > 0 {
		e.warnf("\nthe server can execute %s that farm.step_kinds does not list: %s. "+
			"farm.job_steps.kind is a foreign key against that table, so a job using one cannot "+
			"be recorded; the migration is older than the binary.",
			plural(len(resp.MissingFromDatabase), "kind", "kinds"),
			strings.Join(resp.MissingFromDatabase, ", "))
	}
	return nil
}

func assertOperatorList(ops []specAssertOperator) string {
	if len(ops) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		if op.Numeric {
			parts = append(parts, op.Op+" (numeric)")
			continue
		}
		parts = append(parts, op.Op)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// ctl resets
// ---------------------------------------------------------------------------

// cmdSpecResets shows exactly what a reset tier will run on a device, before it
// runs.
//
// "soft", "medium" and "hard" are words. What they mean is a function of a
// package list in farm.profiles that an operator can edit, and the gap between
// the tiers is the gap between clearing one app's data and uninstalling every
// package the profile does not claim. Reading the commands beforehand is how
// somebody discovers that a profile edit turned "medium" into something that
// removes the test harness from every phone in the pool.
func cmdSpecResets(ctx context.Context, s *session, args []string) error {
	fs := newFlags("resets", s.err)
	var g globals
	g.bind(fs)
	profile := fs.String("profile", "", "the farm.profiles row whose package list the tier owns (required)")
	tier := fs.String("tier", "", "one tier only: none, soft, medium or hard (default: all of them)")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("resets takes no positional arguments; name the profile with --profile %s", rest[0])
	}
	if strings.TrimSpace(*profile) == "" {
		return usageErrf("resets needs --profile: a reset tier has no meaning until a profile's " +
			"package list says which packages it owns")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "profile", strings.TrimSpace(*profile))
	setIf(q, "tier", strings.ToLower(strings.TrimSpace(*tier)))

	resp, raw, err := fetch[specResetsResponse](ctx, e.client, specPrefix+"/resets", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	f := &Fields{}
	f.Add("profile", resp.Profile)
	if resp.Description != "" {
		f.Add("description", resp.Description)
	}
	f.Add("packages", packageList(resp.Packages))
	if resp.DeviceStateScript != "" {
		f.Add("device state script", resp.DeviceStateScript)
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}
	if len(resp.Packages) == 0 {
		e.out.Blank()
		e.out.Text("this profile owns no packages, so the tiers below clear nothing and " +
			"uninstall everything a device did not ship with.")
	}
	if err := renderResetPlans(e, resp.Resets, false); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("these commands run inside the lease the job already holds; none of them ends one.")
	return nil
}

func packageList(packages []string) string {
	if len(packages) == 0 {
		return "none"
	}
	return strings.Join(packages, ", ")
}

// ---------------------------------------------------------------------------
// ctl submit
// ---------------------------------------------------------------------------

// cmdSpecSubmit validates against the server and then files the job.
//
// The order is the point. The server validates a submission anyway and answers
// a bad one with a 400, but a 400 arrives as one sentence pointing at a detail
// object, while POST /specs/validate answers with the complete problem list and
// the reset plan. Asking the cheap question first means the author reads every
// defect at once instead of fixing them one round trip at a time — and it means
// the artifact check, which nothing local can perform, happens before a device
// is ever allocated to a job that cannot finish.
func cmdSpecSubmit(ctx context.Context, s *session, args []string) error {
	fs := newFlags("submit", s.err)
	var g globals
	g.bind(fs)
	file := fs.String("f", "", "path to the job spec, or - for stdin")
	pool := fs.String("pool", "", "pool to allocate from (required)")
	queue := fs.String("queue", "", "queue to file under (required)")
	tenant := fs.String("tenant", "", "tenant that owns the job (required)")
	profile := fs.String("profile", "", "farm.jobs.profile_id: the package list every reset in this job expands against")
	resetTier := fs.String("reset-tier", "", "farm.jobs.reset_tier: none, soft, medium or hard")
	maxAttempts := fs.Int("max-attempts", 0, "farm.jobs.max_attempts: how many devices this job may be placed on")
	resumable := fs.Bool("resumable", true, "farm.jobs.resumable: may a retry continue from the last checkpoint")
	var selectors repeatable
	fs.Var(&selectors, "selector", "device requirement as k=v, repeatable; a value that is JSON is sent as JSON")
	expect := fs.Duration("expect-duration", 0, "how long the job is expected to take; over 30m the lease is protected")
	maxRuntime := fs.Duration("max-runtime", 0, "hard deadline; the only user-supplied clock that may end the lease")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("submit takes no positional arguments; pass the spec with -f %s", rest[0])
	}
	if strings.TrimSpace(*file) == "" {
		return usageErrf("submit needs -f <spec.json>")
	}
	switch {
	case strings.TrimSpace(*pool) == "":
		return usageErrf("submit needs --pool")
	case strings.TrimSpace(*queue) == "":
		return usageErrf("submit needs --queue")
	case strings.TrimSpace(*tenant) == "":
		return usageErrf("submit needs --tenant")
	}
	// A negative max runtime is a deadline in the past. farm.jobs.max_runtime
	// is an unconstrained interval and the reaper expires a lease once
	// now() > acquired_at + max_runtime, so `--max-runtime -1h` files a job
	// whose lease dies on the first pass after it is acquired — and it dies
	// with release_reason 'max_runtime', which reads as the deadline working
	// rather than as a typed minus sign. The only user-supplied clock that may
	// end a lease has to be a clock the user meant.
	if *maxRuntime < 0 {
		return usageErrf("--max-runtime is %s: a negative deadline is already in the past, and "+
			"this job's lease would be expired as soon as it was acquired", *maxRuntime)
	}
	if *expect < 0 {
		return usageErrf("--expect-duration is %s; it is how long the job is expected to take", *expect)
	}
	// A deadline that cannot survive the wire is refused rather than rounded.
	//
	// The request carries whole seconds and the API turns a zero back into SQL
	// NULL, so `--max-runtime 500ms` truncates to 0, is dropped as absent, and
	// files a job with NO deadline at all — while this command goes on to print
	// that the deadline was set. The only user-supplied clock permitted to end a
	// lease must not be able to disappear between the flag and the column, and
	// it must never disappear quietly.
	if secs := int64(maxRuntime.Seconds()); *maxRuntime > 0 && secs == 0 {
		return usageErrf("--max-runtime is %s, and farm.jobs.max_runtime is counted in whole "+
			"seconds: anything under 1s reaches the column as no deadline whatsoever. Write "+
			"at least 1s, or leave the flag out to say this lease ends when the job does",
			*maxRuntime)
	}
	if secs := int64(expect.Seconds()); *expect > 0 && secs == 0 {
		return usageErrf("--expect-duration is %s, and it is sent as whole seconds: anything "+
			"under 1s arrives as no estimate at all, which is not what a job you expect to "+
			"take %s should tell the scheduler", *expect, *expect)
	}

	selector, err := parseJobSelector(selectors)
	if err != nil {
		return err
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	raw, err := readSpecFile(*file, s.in)
	if err != nil {
		return err
	}

	// THE PREFLIGHT. Nothing is filed until the control plane has said this
	// document can run on it — unless there is no document, which is a job the
	// server accepts and there is nothing to ask about.
	spec := json.RawMessage(raw)
	report := specReport{Valid: true}
	if specAbsent(raw) {
		// Normalised to {}, because the file may hold whitespace or the word
		// null and neither survives being marshalled back out as a raw
		// message. {} is what the column defaults to and what the server
		// stores for all three.
		spec = json.RawMessage("{}")
		interactiveLeaseNote(e.warnf, specSource(*file))
	} else if report, _, err = e.validateSpec(ctx, raw, strings.TrimSpace(*profile), true); err != nil {
		return err
	}
	if !report.Valid {
		// To stderr, unlike `ctl validate`: here the problem list is a
		// diagnostic on a command that produced nothing, and a script capturing
		// this command's stdout for the job id must not get a table instead.
		fmt.Fprintf(e.err, "%s: %s; nothing was submitted\n\n", *file,
			plural(report.ProblemCount, "problem", "problems"))
		if err := problemFields(report.Problems).Render(e.err); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s cannot run, so no job was filed", ErrRefused, specSource(*file))
	}

	req := jobSubmitRequest{
		Pool:              strings.TrimSpace(*pool),
		Queue:             strings.TrimSpace(*queue),
		Tenant:            strings.TrimSpace(*tenant),
		Spec:              spec,
		Selector:          selector,
		ExpectedDurationS: int64(expect.Seconds()),
		MaxRuntimeS:       int64(maxRuntime.Seconds()),
		ProfileID:         strings.TrimSpace(*profile),
		ResetTier:         strings.ToLower(strings.TrimSpace(*resetTier)),
	}
	// Sent only when typed. The server distinguishes "omitted" from "zero" and
	// refuses max_attempts:0 with a sentence explaining that a job allowed no
	// attempts can only be placed on a device in order to be abandoned there.
	// Giving the flag a non-zero default here would hide that, and would freeze
	// one farm's default into every copy of this binary.
	if flagGiven(fs, "max-attempts") {
		n := *maxAttempts
		req.MaxAttempts = &n
	}
	if flagGiven(fs, "resumable") {
		b := *resumable
		req.Resumable = &b
	}

	body, err := e.client.Post(ctx, apiPrefix+"/jobs", req)
	if err != nil {
		return submissionError(e, req, err)
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(body)
	}

	var res struct {
		Job job    `json:"job"`
		ID  string `json:"id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return e.out.RawJSON(body)
	}
	id := firstNonEmpty(res.Job.ID, res.ID)

	f := &Fields{}
	f.Add("job", id)
	f.Add("state", firstNonEmpty(res.Job.State, "queued"))
	f.Add("pool", req.Pool)
	f.Add("queue", req.Queue)
	f.Add("tenant", req.Tenant)
	f.Gap()
	f.Addf("steps", "%d", report.Steps)
	if report.TotalTimeoutS > 0 {
		f.Add("spec total timeout", duration(int64(report.TotalTimeoutS)))
	}
	f.Add("profile", firstNonEmpty(req.ProfileID,
		"none — a reset in this job expands against an empty package list and cleans nothing"))
	f.Add("reset tier", firstNonEmpty(req.ResetTier, "(this farm's default)"))
	if req.MaxAttempts != nil {
		f.Addf("max attempts", "%d", *req.MaxAttempts)
	}
	if req.Resumable != nil {
		f.Add("resumable", yesNo(*req.Resumable))
	}
	f.Add("selector", selectorSummary(req.Selector))
	f.Gap()
	// Both clocks are reported as the SECONDS THAT WERE SENT, never as the
	// duration that was typed. They are the same number now that a sub-second
	// value is refused above, and they have to be read off the request anyway:
	// the summary of a submission is the operator's record of what the farm was
	// told, and a line that echoes the flag instead of the field is a line that
	// cannot notice the two drifting apart.
	if req.ExpectedDurationS > 0 {
		note := ""
		if req.ExpectedDurationS > int64(protectionThreshold.Seconds()) {
			note = "  (over 30m: the lease will be protected and is never reclaimed automatically)"
		}
		f.Addf("expected duration", "%s%s", duration(req.ExpectedDurationS), note)
	}
	if req.MaxRuntimeS > 0 {
		f.Addf("max runtime", "%s  (this deadline, and nothing else, may end the lease on a clock)",
			duration(req.MaxRuntimeS))
	} else {
		f.Add("max runtime", "none — this lease ends when the job ends or a human revokes it")
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if err := renderResetPlans(e, report.Resets, report.ResetsTruncated); err != nil {
		return err
	}
	// The job is already filed at this point, so this is a warning about a row
	// that exists rather than a refusal — but it goes to stderr, because it is
	// the one outcome of this command that looks like success and is not. A
	// reset with no package list clears nothing, uninstalls nothing, and
	// reports OK; the device is handed to whoever holds it next still carrying
	// the last tenant's data and a green step log saying it was cleaned.
	if report.ResetSteps > 0 && req.ProfileID == "" {
		e.warnf("\nthis job carries %s and names NO profile, so farm.jobs.profile_id is NULL and "+
			"every one of them expands against an empty package list: they will clear nothing, "+
			"uninstall nothing, and report success. Cancel it and submit again with --profile, "+
			"or read what a profile would have run: ctl resets --profile <id>",
			plural(report.ResetSteps, "reset step", "reset steps"))
	}
	e.out.Blank()
	e.out.Text("nothing is scheduled yet: the scheduler turns this row into a lease.")
	e.out.Text("follow it with:   ctl job %s", id)
	e.out.Text("what it ran:      ctl job steps %s", id)
	e.out.Text("where it ran:     ctl job attempts %s", id)
	if req.ProfileID != "" && req.ResetTier != "" {
		e.out.Text("what %q does: ctl resets --profile %s --tier %s",
			req.ResetTier, req.ProfileID, req.ResetTier)
	}
	return nil
}

// submissionError renders a refused submission.
//
// The API's gate answers with 400 and puts EVERY defect in detail.problems and
// detail.field_problems — a promise its own message makes out loud, and one ctl
// used to break by printing the message and dropping the detail, so an operator
// read "1 problem(s). Every one of them is in detail.problems" followed by no
// problems at all.
//
// It is reported as a refusal rather than a failure for the same reason an
// invalid spec is: a submission naming a pool that does not exist, or a
// selector key this farm cannot understand, is an answer, and re-sending the
// identical body cannot change it.
//
// A transport failure is the opposite case and gets the opposite advice. The
// server never answered, which is NOT the same as "nothing happened": the
// INSERT may have committed and only the reply been lost. There is no
// idempotency key on POST /api/v1/jobs, so a blind retry files a SECOND job,
// and a second job is a second device taken out of the pool to do the work
// twice. The one command in this package that creates work has to say so.
func submissionError(e *env, req jobSubmitRequest, err error) error {
	var remote *RemoteError
	if !errors.As(err, &remote) {
		fmt.Fprintf(e.err, "\nthe control plane did not answer, so this job MAY OR MAY NOT have "+
			"been filed: the row is written before the reply is sent, and nothing here can tell "+
			"a lost request from a lost response. Look before you send it again — a duplicate is "+
			"a second device doing the same work:\n  ctl jobs --tenant %s --queue %s --state queued\n",
			req.Tenant, req.Queue)
		return err
	}
	if remote.Status != http.StatusBadRequest {
		return err
	}
	var detail struct {
		Problems      []jobspec.Problem `json:"problems"`
		FieldProblems []jobspec.Problem `json:"field_problems"`
		Supported     []string          `json:"supported_selector_keys"`
		Tiers         []string          `json:"permitted_reset_tiers"`
	}
	if uerr := json.Unmarshal(remote.Detail, &detail); uerr != nil {
		return err
	}
	problems := append(append([]jobspec.Problem{}, detail.Problems...), detail.FieldProblems...)
	if len(problems) == 0 {
		return err
	}

	fmt.Fprintf(e.err, "the control plane refused this submission: %s; nothing was filed\n\n",
		plural(len(problems), "problem", "problems"))
	if rerr := problemFields(problems).Render(e.err); rerr != nil {
		return rerr
	}
	if len(detail.Supported) > 0 {
		fmt.Fprintf(e.err, "\nselector keys this farm understands: %s\n",
			strings.Join(detail.Supported, ", "))
	}
	if len(detail.Tiers) > 0 {
		fmt.Fprintf(e.err, "reset tiers: %s\n", strings.Join(detail.Tiers, ", "))
	}
	return fmt.Errorf("%w: the submission was rejected (request %s)", ErrRefused, remote.RequestID)
}

// parseJobSelector builds farm.jobs.selector from repeated --selector k=v.
//
// The value is read as JSON when it parses as JSON and as a plain string
// otherwise. That is one rule instead of a table of per-key shapes, and it has
// to exist because farm.device_matches CASTS: sdk_min is compared as
// (selector->>'sdk_min')::int, model_in is expanded with
// jsonb_array_elements_text, labels is tested with @>. A string where an int is
// cast raises 22P02 inside the allocator's candidate scan, and that is not a
// refusal — the job is simply never placed, on every sweep, for as long as it
// exists, while the farm merely looks busy.
//
//	--selector model=Pixel 7         {"model": "Pixel 7"}
//	--selector sdk_min=33            {"sdk_min": 33}
//	--selector model_in=["A","B"]    {"model_in": ["A", "B"]}
//	--selector labels.env=ci         {"labels": {"env": "ci"}}
//	--selector model="33"            {"model": "33"}  — quoting forces a string
//
// The keys themselves are NOT checked here. farm.selector_unknown_keys is the
// function farm.lease_acquire itself calls and the submission gate asks it, so
// a list copied into this file could only ever go stale in the direction that
// refuses a key the farm has since learned.
func parseJobSelector(items []string) (json.RawMessage, error) {
	if len(items) == 0 {
		return nil, nil
	}
	doc := map[string]any{}
	nested := map[string]map[string]any{}

	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			return nil, usageErrf("--selector takes k=v, not %q", item)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, usageErrf("--selector %q names no key", item)
		}
		if value == "" {
			// An empty value is a script whose variable never got set, and in a
			// selector that is not harmless: {"model":""} matches no device, so
			// the job sits queued forever rather than failing where somebody
			// would see it.
			return nil, usageErrf("--selector %s= has no value; a selector that cannot match "+
				"any device leaves the job queued forever rather than failing", key)
		}

		outer, inner, isNested := strings.Cut(key, ".")
		if !isNested {
			if _, taken := nested[key]; taken {
				return nil, usageErrf("--selector %s was given both as a whole document and as "+
					"%s.<field>; pick one", key, key)
			}
			// A repeated key is refused, not last-one-wins.
			//
			// A selector is a JSON object and an object holds one value per
			// key, so `--selector model=A --selector model=B` can only ever
			// mean B — and it reads like "either". The job is then filed
			// against half of what was asked for: placed on a B and never on
			// an A, or, if no B exists, left queued forever with nothing
			// anywhere reporting that the A was dropped. The farm spells a
			// choice of several as an _in key, which this refusal names.
			if _, dup := doc[key]; dup {
				return nil, usageErrf("--selector %s was given more than once, and a selector "+
					"is a JSON object: it holds one value per key, so every earlier %s would "+
					"be silently discarded. This farm spells \"any of these\" as an _in key — "+
					"model_in, host_in, not_host_in — taking a JSON array: "+
					"--selector %s_in=[\"…\",\"…\"]", key, key, key)
			}
			doc[key] = selectorValue(value)
			continue
		}
		if inner == "" {
			return nil, usageErrf("--selector %q names no field after the dot", key)
		}
		if _, taken := doc[outer]; taken {
			return nil, usageErrf("--selector %s was given both as a whole document and as "+
				"%s.%s; pick one", outer, outer, inner)
		}
		if _, dup := nested[outer][inner]; dup {
			return nil, usageErrf("--selector %s.%s was given more than once; %s holds one "+
				"value per field, so every earlier %s would be silently discarded",
				outer, inner, outer, inner)
		}
		if nested[outer] == nil {
			nested[outer] = map[string]any{}
		}
		nested[outer][inner] = selectorValue(value)
	}
	for outer, fields := range nested {
		doc[outer] = fields
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode selector: %w", err)
	}
	return out, nil
}

// selectorValue keeps a JSON value exactly as written and falls back to a
// string.
//
// json.RawMessage rather than a decoded any, so a number reaches Postgres as
// typed: decoding through float64 would render a large integer in exponent
// notation and hand the ::int cast something it raises on.
func selectorValue(v string) any {
	if json.Valid([]byte(v)) {
		return json.RawMessage(v)
	}
	return v
}

func selectorSummary(selector json.RawMessage) string {
	if len(selector) == 0 || isEmptyJSON(selector) {
		return "{} — any healthy device in the pool"
	}
	return string(selector)
}

// flagGiven reports whether name was actually typed, as opposed to holding its
// default. parseArgs re-parses the tail after each positional argument and
// flag.FlagSet records what was Set across every one of those passes, so this
// stays accurate however the flags were interleaved.
func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}
