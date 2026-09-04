package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// ---------------------------------------------------------------------------
// The gate POST /api/v1/jobs applies before a row reaches farm.jobs.
//
// Everything checked here is checked because the alternative is checking it on
// a phone. A job is filed, the scheduler hands it a device through
// farm.lease_acquire, and only then does something read the document: an
// unparseable spec fails at the runner's first step, an unknown selector key
// is refused inside lease_acquire itself, a profile that does not exist makes
// a reset expand to nothing. Every one of those costs a handset, and every one
// of them is knowable from the request body plus two columns. The author is
// sitting at a terminal when they press send; that is when they should hear
// about it, and they should hear about ALL of it at once rather than one
// problem per round trip.
//
// Nothing here can end a lease and nothing here talks to a device.
// ---------------------------------------------------------------------------

// Defaults for the three NOT NULL columns a submission may leave out.
//
// They mirror farm.jobs' own DEFAULTs rather than replace them, because the
// INSERT has to send a value for a NOT NULL column and a caller that said
// nothing must land on the same behaviour it lands on today. The CHECK
// constraints remain the authority: a value this file let through that the
// schema refuses comes back as a 400 from classifyPgError, not as a surprise.
// Stating them here follows what handleJobCreate already does for ttl and
// grace — one visible place answers "what does this farm do when the caller
// says nothing".
const (
	defaultResetTier   = string(jobspec.TierSoft)
	defaultMaxAttempts = 3
	defaultResumable   = true
)

// supportedSelectorKeys mirrors the key list inside farm.selector_unknown_keys.
//
// It is HELP, not policy. The refusal below is decided by the DATABASE's
// answer — the same function farm.lease_acquire calls — so this list going
// stale can only make an error message less useful. It can never let an
// unsupported key through or refuse a supported one.
var supportedSelectorKeys = []string{
	"abi", "android_release", "host_in", "labels", "manufacturer",
	"model", "model_in", "not_host_in", "sdk_max", "sdk_min",
}

// JobSubmissionOptions carries the four farm.jobs columns the runner reads and
// that no caller could previously set.
//
// All four are already load-bearing: runner.loadJob selects max_attempts,
// resumable, reset_tier and profile_id on every placement, and until this
// struct existed farm.jobs.profile_id was written by nothing at all — so every
// job in the farm reset against an empty package list and cleaned nothing.
//
// MaxAttempts and Resumable are pointers because "omitted" and "zero" are
// different requests. resumable:false is a real choice — a job whose steps
// must never be re-run from a checkpoint — and max_attempts:0 is a bug worth
// refusing rather than silently reading as "unset".
type JobSubmissionOptions struct {
	ProfileID   string `json:"profile_id,omitempty"`
	ResetTier   string `json:"reset_tier,omitempty"`
	MaxAttempts *int   `json:"max_attempts,omitempty"`
	Resumable   *bool  `json:"resumable,omitempty"`

	// Selector is an OUTPUT: ValidateJobSubmission fills it with the selector
	// document to store, and no caller can set it from a request body.
	//
	// It exists because "absent", "null" and "{}" all mean the same request and
	// only one of them is safe in the column. A literal null reaching
	// farm.jobs.selector is not an empty constraint — farm.lease_acquire opens
	// with farm.selector_unknown_keys(j.selector), which raises "cannot call
	// jsonb_object_keys on a scalar" on it, so EVERY allocation of that job
	// dies in the allocator instead of the job being placed. Returning the
	// normalised document is what keeps the promise made below: what comes back
	// here is what may be inserted.
	Selector json.RawMessage `json:"-"`
}

// ValidateJobSubmission is the gate. It returns the options normalised for the
// INSERT, a 400 body when the submission cannot run, and an error when the
// database could not be asked.
//
// The three results are separate because they mean different things to the
// caller: a refusal is the client's fault and is rendered, an error is the
// server's and goes through Server.fail as a 500 or a 503. Conflating them
// would report a database outage as "your spec is invalid".
//
// On a nil refusal the returned options are safe to insert directly:
// ResetTier is non-empty, MaxAttempts is within the range farm.jobs holds and
// non-nil, Resumable is non-nil, Selector is a jsonb object, and ProfileID is
// trimmed (empty meaning SQL NULL, which is what farm.jobs.profile_id's
// nullable column means — this job names no profile).
func (s *Server) ValidateJobSubmission(ctx context.Context, spec, selector json.RawMessage,
	opts JobSubmissionOptions) (JobSubmissionOptions, *APIError, error) {

	opts.ProfileID = strings.TrimSpace(opts.ProfileID)
	opts.ResetTier = strings.ToLower(strings.TrimSpace(opts.ResetTier))
	if opts.ResetTier == "" {
		opts.ResetTier = defaultResetTier
	}
	if opts.MaxAttempts == nil {
		n := defaultMaxAttempts
		opts.MaxAttempts = &n
	}
	if opts.Resumable == nil {
		b := defaultResumable
		opts.Resumable = &b
	}

	var (
		problems  []jobspec.Problem
		showTiers bool
		showKeys  bool
	)

	if !jobspec.ResetTier(opts.ResetTier).Valid() {
		showTiers = true
		problems = append(problems, jobspec.Problem{
			Path: "reset_tier",
			Message: fmt.Sprintf("%q is not a reset tier; farm.jobs.reset_tier permits %s",
				opts.ResetTier, strings.Join(resetTierNames(), ", ")),
		})
	}
	switch {
	case *opts.MaxAttempts < 1:
		problems = append(problems, jobspec.Problem{
			Path: "max_attempts",
			Message: fmt.Sprintf("max_attempts is %d and must be at least 1: the runner refuses "+
				"any attempt above this number, so a job allowed none can only ever be "+
				"placed on a device in order to be abandoned there",
				*opts.MaxAttempts),
		})
	case *opts.MaxAttempts > math.MaxInt32:
		// The CHECK constraint cannot catch this one. farm.jobs.max_attempts is
		// an int4, so a larger number never reaches Postgres at all: it fails
		// while the driver encodes the INSERT, and a driver-side encoding error
		// is not a PgError, so classifyError has nothing to work with and the
		// author of a mistyped submission is told "internal error".
		problems = append(problems, jobspec.Problem{
			Path: "max_attempts",
			Message: fmt.Sprintf("max_attempts is %d; farm.jobs.max_attempts holds at most "+
				"2147483647. Send the number of times this job may be placed on a device — "+
				"a farm this size is exhausted long before three digits",
				*opts.MaxAttempts),
		})
	}

	// A selector that is not a JSON object is worse than one with a bad key:
	// farm.lease_acquire hands it straight to farm.selector_unknown_keys, whose
	// jsonb_object_keys raises on an array or a scalar, so the job is not
	// refused at submission and not placed either — it dies in the allocator on
	// every sweep, for as long as it exists.
	object, fields, selectorOK := selectorObject(selector)
	opts.Selector = object
	if !selectorOK {
		problems = append(problems, jobspec.Problem{
			Path: "selector",
			Message: "selector must be a JSON object; farm.lease_acquire raises on anything " +
				"else, so a job carrying one is never placed and never refused. Use {} to " +
				"accept any device in the pool",
		})
	}
	problems = append(problems, selectorValueProblems(fields)...)

	if selectorOK || opts.ProfileID != "" {
		unknown, profileExists, err := s.submissionReferences(ctx, opts.ProfileID, object)
		if err != nil {
			return opts, nil, err
		}
		if opts.ProfileID != "" && !profileExists {
			problems = append(problems, jobspec.Problem{
				Path: "profile_id",
				Message: fmt.Sprintf("no such profile in farm.profiles: %q; the profile's package "+
					"list is the whole input to a reset, so a job naming one that does not "+
					"exist would clean nothing and report success", opts.ProfileID),
			})
		}
		for _, key := range unknown {
			showKeys = true
			problems = append(problems, jobspec.Problem{
				Path: "selector." + key,
				Message: fmt.Sprintf("%q is not a selector key this farm understands; "+
					"farm.lease_acquire refuses the job rather than ignoring the key, so a "+
					"submission carrying it can only ever fail at allocation", key),
			})
		}
	}

	extra := map[string]any{}
	if showTiers {
		extra["permitted_reset_tiers"] = resetTierNames()
	}
	if showKeys {
		extra["supported_selector_keys"] = supportedSelectorKeys
	}

	// The spec is checked last because it is the only half that needs no
	// database: if the round trip above failed, the caller gets a 503 and has
	// learned nothing misleading about their document.
	specErr := specSubmissionError(spec)
	switch {
	case specErr == nil && len(problems) == 0:
		return opts, nil, nil
	case specErr == nil:
		return opts, submissionRefusal(problems, extra), nil
	}

	if len(problems) > 0 {
		// Both halves are wrong, and they go back in one reply. specErr is
		// freshly built by invalidSpecError for this call, so extending it is
		// safe and keeps detail.problems meaning exactly what it means at
		// POST /api/v1/specs/validate — the spec's problems, nothing else.
		shown, truncated := renderProblems(problems)
		detail, ok := specErr.Detail.(map[string]any)
		if !ok {
			// invalidSpecError builds a map today. The message below promises
			// detail.field_problems, so if that ever stops being true the
			// problems still have to arrive: a body that contradicts its own
			// message is read by somebody who is already debugging.
			detail = map[string]any{"spec_detail": specErr.Detail}
			specErr.Detail = detail
		}
		detail["field_problems"] = shown
		detail["field_problem_count"] = len(problems)
		if truncated {
			detail["field_problems_truncated"] = true
		}
		for k, v := range extra {
			detail[k] = v
		}
		specErr.Message += fmt.Sprintf(" %d field(s) of the submission are wrong as well, "+
			"in detail.field_problems.", len(problems))
	}
	return opts, specErr, nil
}

// RejectSubmission writes a submission refusal.
//
// Every refusal this gate produces is a 400: the request named something that
// cannot work, and no amount of retrying the same body changes that. The code
// inside the envelope is what a client branches on — invalid_spec when the
// document is the problem, bad_request when a field is.
func RejectSubmission(w http.ResponseWriter, e *APIError) {
	writeError(w, http.StatusBadRequest, e.Code, e.Message, e.Detail)
}

// submissionReferences asks the database the two questions this gate cannot
// answer by itself, in one round trip.
//
// The selector is handed to farm.selector_unknown_keys rather than compared
// against a list in this file on purpose: that function is what
// farm.lease_acquire calls, so the answer here and the answer at allocation
// cannot disagree. A binary older than its schema refuses exactly what the
// schema refuses.
func (s *Server) submissionReferences(ctx context.Context, profile string, selector []byte) (
	unknownKeys []string, profileExists bool, err error) {

	const q = `
SELECT EXISTS (SELECT 1 FROM farm.profiles p WHERE p.id = $1::text),
       farm.selector_unknown_keys($2::jsonb)`

	err = s.pool.QueryRow(ctx, q, profile, selector).Scan(&profileExists, &unknownKeys)
	return unknownKeys, profileExists, err
}

// selectorObject reports whether raw is a JSON object, returns the bytes to
// store and send to Postgres, and returns its fields for value checking.
//
// Absent, null and {} all mean "any device in the pool", which is what
// farm.jobs.selector's own DEFAULT '{}' means, and all three are normalised to
// {} so neither the database nor the column is ever handed a scalar. A document
// that is neither absent nor an object is reported, not normalised: doc is {}
// so the round trip below is still safe to make, and the caller must not file
// the job at all.
func selectorObject(raw json.RawMessage) (doc []byte, fields map[string]json.RawMessage, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("{}"), nil, true
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return []byte("{}"), nil, false
	}
	return trimmed, probe, true
}

// selectorValueProblems reports selector VALUES that farm.device_matches
// cannot evaluate.
//
// The key list is the database's answer because farm.selector_unknown_keys is
// the function allocation itself calls. There is no equivalent to ask about
// values: the casts are in farm.device_matches' body and reaching them needs a
// real device row, so the shapes are stated here — each one named after the SQL
// that fails on it.
//
// The consequence is identical to an unknown key and less obvious. A selector
// of {"sdk_min":"thirteen"} passes every key check, and then
// (s->>'sdk_min')::int raises 22P02 inside the allocator's candidate scan: the
// job is never placed, the scheduler logs a cast error on every sweep, and the
// author reads a farm that looks merely busy. Verified against the live schema
// — the raise is real for each case below.
//
// It is deliberately narrow. Only a value the function provably cannot use is
// refused; anything this list has no opinion about is left to the database, so
// a shape added to device_matches later is never refused here by accident.
func selectorValueProblems(fields map[string]json.RawMessage) []jobspec.Problem {
	var problems []jobspec.Problem
	for key, raw := range fields {
		switch key {
		case "sdk_min", "sdk_max":
			// Only the cast is checked, not the range of SDK levels a farm
			// actually holds: farm.devices.sdk_int has its own CHECK, and a
			// second copy of that number here would start refusing valid
			// submissions the day the schema widens it.
			if !jsonIsInt32(raw) {
				problems = append(problems, jobspec.Problem{
					Path: "selector." + key,
					Message: fmt.Sprintf("%s must be a whole number an int4 can hold: "+
						"farm.device_matches compares it as (selector->>'%s')::int, which "+
						"raises on %s and takes the whole allocation attempt with it",
						key, key, briefJSON(raw)),
				})
			}
		case "model_in", "host_in", "not_host_in":
			if jsonKind(raw) != '[' {
				problems = append(problems, jobspec.Problem{
					Path: "selector." + key,
					Message: fmt.Sprintf("%s must be a JSON array of strings: farm.device_matches "+
						"expands it with jsonb_array_elements_text, which raises on %s and "+
						"takes the whole allocation attempt with it", key, briefJSON(raw)),
				})
			}
		case "labels":
			if jsonKind(raw) != '{' {
				problems = append(problems, jobspec.Problem{
					Path: "selector.labels",
					Message: fmt.Sprintf("labels must be a JSON object: farm.device_matches tests "+
						"it with devices.labels @> selector->'labels', and containment of %s "+
						"is false for every device, so the job would wait forever for a match "+
						"that cannot happen", briefJSON(raw)),
				})
			}
		}
	}
	// Map iteration is unordered and a refusal is read by a human, so the list
	// is sorted: two identical submissions must produce identical replies.
	sort.Slice(problems, func(a, b int) bool { return problems[a].Path < problems[b].Path })
	return problems
}

// briefJSON renders a selector value for an error message.
//
// The value is the caller's own document and there is no bound on how big one
// key of it can be, so it is cut: a refusal that quotes half a megabyte back at
// somebody explains nothing they could not already see. The cut is on a rune
// boundary, because the message is JSON and a split code point renders as a
// replacement character in the middle of the evidence.
func briefJSON(raw json.RawMessage) string {
	const max = 80
	v := string(bytes.TrimSpace(raw))
	if len(v) <= max {
		return v
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + "…"
}

// jsonKind returns the first byte of a JSON value, which is all that is needed
// to tell an object from an array from a scalar. The value came out of
// encoding/json, so it is well formed and the byte is decisive.
func jsonKind(raw json.RawMessage) byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

// jsonIsInt32 reports whether raw is a JSON number that (selector->>'k')::int
// would accept.
//
// jsonb normalises a number before ->> renders it, so 1e2 arrives at the cast
// as 100 and succeeds while 13.7 stays fractional and raises. The test is
// therefore "numeric, integral, and inside int4", not "looks like digits" —
// refusing 1e2 would be refusing a selector that works.
func jsonIsInt32(raw json.RawMessage) bool {
	if k := jsonKind(raw); k != '-' && (k < '0' || k > '9') {
		return false
	}
	f, err := strconv.ParseFloat(string(bytes.TrimSpace(raw)), 64)
	return err == nil && f == math.Trunc(f) && f >= math.MinInt32 && f <= math.MaxInt32
}

// submissionRefusal renders a field problem list as the API's error envelope.
//
// It mirrors invalidSpecError deliberately: a client that already iterates
// detail.problems for a bad spec iterates the same field for a bad profile id,
// and the true count sits beside the list whether or not the list was cut.
func submissionRefusal(problems []jobspec.Problem, extra map[string]any) *APIError {
	shown, truncated := renderProblems(problems)
	detail := map[string]any{
		"problems":      shown,
		"problem_count": len(problems),
	}
	if truncated {
		detail["problems_truncated"] = true
	}
	for k, v := range extra {
		detail[k] = v
	}
	return &APIError{
		Code: CodeBadRequest,
		Message: fmt.Sprintf("this job cannot be filed: %d problem(s) with the submission. "+
			"Every one of them is in detail.problems, so the request can be fixed in one "+
			"round trip.", len(problems)),
		Detail: detail,
	}
}

// renderProblems applies the cap invalidSpecError applies, so one submission
// can never produce a response body nobody can read.
func renderProblems(problems []jobspec.Problem) ([]jobspec.Problem, bool) {
	if len(problems) > maxValidateProblems {
		return problems[:maxValidateProblems], true
	}
	return problems, false
}

// resetTierNames is farm.jobs.reset_tier's CHECK list as strings, in
// escalation order, for rendering in a refusal.
func resetTierNames() []string {
	out := make([]string, 0, len(resetTiers))
	for _, t := range resetTiers {
		out = append(out, string(t))
	}
	return out
}
