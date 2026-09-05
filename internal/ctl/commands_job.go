package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// ctl job steps, ctl job attempts — the execution half of a job.
//
// farm.job_steps and farm.job_attempts are written on every placement, and
// until these two commands existed nothing read them back: an operator with a
// failed job could see THAT it failed and not which step, what it printed, or
// which handsets it had already burned through.
//
// The two views answer one question between them, and the answer decides who
// gets paged:
//
//	four failures on four different devices  → the job is wrong; fix the spec
//	four failures on the same device         → the device is wrong; pull it
//
// Neither is reachable from farm.jobs alone. `ctl job <id>` shows a state and a
// lease history; it cannot tell those two stories apart, and guessing between
// them is how a fleet loses an afternoon to a spec bug or keeps handing work to
// a phone whose USB port is failing.
//
// Both are reads. Nothing here writes anything, and nothing here ends a lease:
// a job that failed four times has already stopped holding devices, and a job
// still running keeps its lease while these commands look at it.
// ---------------------------------------------------------------------------

// outputTailLines is how much of a step's captured output the table shows.
//
// The tail, not the head. A step that failed says why in its last lines — the
// stack trace, the "No such file or directory", the exit banner — while its
// first lines are a build header that is identical on every device in the rack.
const outputTailLines = 1

// stepOutputCell bounds the output column. It is wider than defaultCellWidth
// because the point of the column is to make the failing row self-explanatory,
// and narrower than a terminal so the grid still lines up.
const stepOutputCell = 90

// farmUIDCellChars is how much of a farm_uid an identity column shows.
//
// Wider than shortID, because every farm_uid in a deployment carries the same
// "df-" prefix: cutting one at eight characters leaves five that actually
// distinguish handsets, and an operator comparing two rows of the attempts
// table to decide whether the same phone failed twice needs more than five.
const farmUIDCellChars = 16

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// jobStep is one row of farm.job_steps.
//
// DurationS is computed by Postgres against its own now(), so a step still
// running reports how long it has been running rather than nothing. That is
// the number an operator reads at three in the morning to decide whether a job
// is working or wedged, and it must never come from this machine's clock.
type jobStep struct {
	Attempt    int        `json:"attempt"`
	StepIndex  int        `json:"step_index"`
	StepID     string     `json:"step_id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationS  *float64   `json:"duration_s"`
	ExitCode   *int64     `json:"exit_code"`

	Output          string `json:"output"`
	OutputChars     int64  `json:"output_chars"`
	OutputTruncated bool   `json:"output_truncated"`
	OutputOmitted   bool   `json:"output_omitted"`

	Error          string `json:"error"`
	ErrorChars     int64  `json:"error_chars"`
	ErrorTruncated bool   `json:"error_truncated"`
	ErrorOmitted   bool   `json:"error_omitted"`

	Detail        json.RawMessage `json:"detail"`
	DetailOmitted bool            `json:"detail_omitted"`
}

type jobStepsResponse struct {
	JobID             string         `json:"job_id"`
	JobState          string         `json:"job_state"`
	Attempt           int            `json:"attempt"`
	MaxAttempts       int            `json:"max_attempts"`
	AttemptsWithSteps []int          `json:"attempts_with_steps"`
	Steps             []jobStep      `json:"steps"`
	States            map[string]int `json:"states"`
	Scope             string         `json:"scope"`
	Truncated         bool           `json:"truncated"`
	LogsOmitted       int            `json:"logs_omitted"`
}

// jobAttempt is one placement of a job on a device.
//
// FarmUID is the identity; AdbSerial is only evidence about it and is
// deliberately not unique, because duplicate OEM serials are real. Two rows are
// the same handset when their farm_uid matches, never when their serial does.
type jobAttempt struct {
	Attempt  int    `json:"attempt"`
	DeviceID string `json:"device_id"`

	FarmUID   string `json:"farm_uid"`
	Model     string `json:"model"`
	AdbSerial string `json:"adb_serial"`

	// CurrentDevpath is where that device sits NOW, not where it sat during
	// this attempt. A phone re-slotted since is the same identity at a new
	// position, and pretending otherwise sends somebody to the wrong port.
	CurrentDevpath string `json:"current_devpath"`

	LeaseID       string `json:"lease_id"`
	Fence         *int64 `json:"fence"`
	LeaseState    string `json:"lease_state"`
	ReleaseReason string `json:"release_reason"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationS  *float64   `json:"duration_s"`
	Outcome    string     `json:"outcome"`
	Error      string     `json:"error"`
}

type jobAttemptDevice struct {
	DeviceID string         `json:"device_id"`
	FarmUID  string         `json:"farm_uid"`
	Model    string         `json:"model"`
	Attempts int            `json:"attempts"`
	Outcomes map[string]int `json:"outcomes"`
}

type jobAttemptsResponse struct {
	JobID           string             `json:"job_id"`
	JobState        string             `json:"job_state"`
	Attempt         int                `json:"attempt"`
	MaxAttempts     int                `json:"max_attempts"`
	Attempts        []jobAttempt       `json:"attempts"`
	Outcomes        map[string]int     `json:"outcomes"`
	DistinctDevices int                `json:"distinct_devices"`
	ByDevice        []jobAttemptDevice `json:"by_device"`
	Truncated       bool               `json:"truncated"`
}

// ---------------------------------------------------------------------------
// ctl job steps
// ---------------------------------------------------------------------------

// cmdJobSteps renders the ordered step log of one attempt.
//
// Ordered is the load-bearing word. A job's steps are a chronology — this one
// installed, that one waited, the next one asserted and failed — and the shape
// of the failure is usually visible in where the ok rows stop, not in the
// message of the row that broke. So the table is printed in step order with
// nothing filtered out, and the failing step's full error is printed underneath
// where it does not have to survive a column width.
func cmdJobSteps(ctx context.Context, s *session, args []string) error {
	fs := newFlags("job steps", s.err)
	var g globals
	g.bind(fs)
	attempt := fs.String("attempt", "",
		"which placement to show: a number, or all; default is the newest attempt that ran")
	limit := fs.Int("limit", 0, "maximum steps to render")
	chars := fs.Int("output-chars", 0, "characters of output and error to fetch per step")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("ctl job steps <job-id> [--attempt N|all] takes exactly one job id")
	}
	// The GRAMMAR of --attempt is checked here; its RANGE stays the server's,
	// which owns farm.job_steps.attempt's type. Without this a typo made a
	// round trip and came back as a 400, which ExitCode reports as 1 — "it
	// failed, try again" — for an invocation no retry can fix. A mistyped flag
	// is exit 2, and the server never needed to be asked.
	scope := strings.ToLower(strings.TrimSpace(*attempt))
	if scope != "" && scope != "all" {
		if _, err := strconv.Atoi(scope); err != nil {
			return usageErrf("--attempt takes a whole number or the word all, not %q; omit it "+
				"for the newest attempt that ran", *attempt)
		}
	}
	// Negative bounds were silently dropped, so `--limit -5` rendered the
	// server's default and looked like it had been honoured. A limit that
	// cannot be sent is a typo, not a request.
	if *limit < 0 {
		return usageErrf("--limit is %d; it is a maximum number of steps to render", *limit)
	}
	if *chars < 0 {
		return usageErrf("--output-chars is %d; it is how much of each step's log to fetch", *chars)
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "attempt", scope)
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	if *chars > 0 {
		q.Set("output_chars", strconv.Itoa(*chars))
	}

	path := apiPrefix + "/jobs/" + url.PathEscape(rest[0]) + "/steps"
	resp, raw, err := fetch[jobStepsResponse](ctx, e.client, path, q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	f := &Fields{}
	f.Add("job", resp.JobID)
	f.Add("state", resp.JobState)
	f.Addf("attempt", "%d of at most %d", resp.Attempt, resp.MaxAttempts)
	f.Add("showing", attemptScope(resp.Scope))
	f.Add("attempts with steps", attemptList(resp.AttemptsWithSteps))
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if len(resp.Steps) == 0 {
		e.out.Blank()
		e.out.Text("no steps were recorded for this attempt. A job whose spec carries no steps " +
			"is an interactive lease: a human holds the device and nothing runs on it.")
		return nil
	}

	e.out.Blank()
	t := NewTable("#", "STEP", "KIND", "STATE", "DURATION", "EXIT", "OUTPUT (tail)").
		MaxCell(stepOutputCell)
	lastAttempt := -1
	for _, st := range resp.Steps {
		// Sectioned only when more than one placement is on screen, so the
		// ordinary "show me the last attempt" rendering is one clean grid.
		if resp.Scope == "all" && st.Attempt != lastAttempt {
			t.Section("attempt %d", st.Attempt)
			lastAttempt = st.Attempt
		}
		t.Row(
			strconv.Itoa(st.StepIndex),
			st.StepID,
			st.Kind,
			st.State,
			stepDuration(st),
			dashInt64(st.ExitCode),
			stepOutputCellText(st),
		)
	}
	if err := e.out.Table(t); err != nil {
		return err
	}

	e.out.Blank()
	e.out.Text("%s: %s", plural(len(resp.Steps), "step", "steps"),
		countsLine(resp.States, "ok", "running", "failed", "aborted", "skipped", "pending"))

	// The full text of whatever went wrong, under the table, unclipped. A
	// column cannot hold a stack trace and an operator reading this command has
	// already decided they want the whole thing.
	if err := renderStepFailures(e, resp.Steps); err != nil {
		return err
	}

	if resp.Truncated {
		e.warnf("the step list was cut at the response limit; narrow it with --attempt N or " +
			"raise --limit. The states line above counts only what was rendered.")
	}
	if resp.LogsOmitted > 0 {
		e.warnf("%d log field(s) were dropped for the response size budget — the steps are all "+
			"here and some of their text is not. Ask for one attempt with --attempt N, or a "+
			"smaller --limit, to get it back.", resp.LogsOmitted)
	}
	return nil
}

// stepDuration renders how long a step took, or has been taking.
//
// Milliseconds, because an adb shell command on a healthy phone finishes in a
// few hundred of them: truncating to whole seconds reports every one of them as
// "0s" and erases the only number in the row that separates a fast device from
// one that took twenty seconds to answer.
func stepDuration(st jobStep) string {
	if st.DurationS == nil {
		return "—"
	}
	out := millis(int64(*st.DurationS * 1000))
	if st.FinishedAt == nil && st.StartedAt != nil {
		// Server-side now() minus started_at: this step has not finished, and
		// the number is still climbing.
		return out + "…"
	}
	return out
}

// stepOutputCellText picks the one line of a step worth putting in the grid.
//
// The error wins over the output when there is one, because a row that failed
// is the row being looked for and its message is the reason. An omitted field
// is named as omitted rather than left blank: "this step printed nothing" and
// "the response could not afford to send what it printed" are opposite facts
// and a blank cell reads as the first.
func stepOutputCellText(st jobStep) string {
	switch {
	case st.ErrorOmitted:
		return "(error omitted: response budget)"
	case st.Error != "":
		return tailLines(st.Error, outputTailLines) + truncationMark(st.ErrorTruncated)
	case st.OutputOmitted:
		return "(output omitted: response budget)"
	case st.Output != "":
		tail := tailLines(st.Output, outputTailLines)
		if tail == "" {
			// The runner stored something and every line of it is whitespace.
			// An empty cell here would read as "this step printed nothing",
			// which is a different fact about the device.
			return "(blank)"
		}
		return tail + truncationMark(st.OutputTruncated)
	case st.State == "skipped":
		return skipReason(st)
	}
	return ""
}

// skipReason is the one line a skipped row carries in detail.reason: why it
// was not run. Two writers produce 'skipped' — a resumed attempt, for a step
// that already completed in an earlier run, and a failure, for every step
// after it — and the state alone does not say which, so the reason is the
// cell. A row with no reason renders blank, as before.
func skipReason(st jobStep) string {
	if len(st.Detail) == 0 {
		return ""
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(st.Detail, &d); err != nil {
		return ""
	}
	return d.Reason
}

func truncationMark(truncated bool) string {
	if truncated {
		return " […]"
	}
	return ""
}

// tailLines returns the last n non-blank lines of s.
//
// Blank lines are skipped rather than counted: a shell command that ends with a
// newline would otherwise put an empty cell in the column, which reads as a
// step that printed nothing when in fact it printed the answer one line up.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	kept := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append([]string{strings.TrimSpace(lines[i])}, kept...)
	}
	return strings.Join(kept, " / ")
}

// renderStepFailures prints, in full, everything a step said about failing.
func renderStepFailures(e *env, steps []jobStep) error {
	for _, st := range steps {
		if st.State == "ok" || st.State == "pending" || st.State == "running" {
			continue
		}
		e.out.Blank()
		e.out.Text("step %d %q (%s) is %s:", st.StepIndex, st.StepID, st.Kind, st.State)
		f := &Fields{}
		if st.ExitCode != nil {
			f.Addf("exit code", "%d", *st.ExitCode)
		}
		f.Add("started", stamp(st.StartedAt))
		f.Add("finished", stamp(st.FinishedAt))
		if st.Error != "" {
			f.Addf("error", "%s%s", st.Error, charCount(st.ErrorTruncated, st.ErrorChars))
		} else if st.ErrorOmitted {
			f.Add("error", "omitted for the response size budget; re-run with --attempt "+
				strconv.Itoa(st.Attempt))
		}
		if strings.TrimSpace(st.Output) != "" {
			f.Addf("output", "%s%s", st.Output, charCount(st.OutputTruncated, st.OutputChars))
		} else if st.Output != "" {
			// Whitespace, collapsed to a space by the Fields renderer, would
			// print as a blank value indistinguishable from a step that
			// printed nothing at all.
			f.Addf("output", "(whitespace only, %d character(s) stored)", st.OutputChars)
		} else if st.OutputOmitted {
			f.Add("output", "omitted for the response size budget; re-run with --attempt "+
				strconv.Itoa(st.Attempt))
		}
		if len(st.Detail) > 0 {
			// The runner's own context: the command it ran, the tier it
			// expanded, and — the reason this line matters — how many transport
			// retries it absorbed WITHOUT the lease ever being released.
			f.Add("detail", string(st.Detail))
		} else if st.DetailOmitted {
			f.Add("detail", "omitted for the response size budget")
		}
		if err := e.out.Fields(f); err != nil {
			return err
		}
	}
	return nil
}

// charCount says how much of a field was not sent, so a cut log never passes
// for a complete one.
func charCount(truncated bool, total int64) string {
	if !truncated {
		return ""
	}
	return fmt.Sprintf("\n  […] cut; the runner stored %d characters. Fetch more with "+
		"--output-chars N", total)
}

func attemptScope(scope string) string {
	switch scope {
	case "", "latest":
		return "the newest attempt that ran"
	case "all":
		return "every attempt"
	}
	return "attempt " + scope
}

func attemptList(attempts []int) string {
	if len(attempts) == 0 {
		return "none — this job has never been placed on a device"
	}
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, strconv.Itoa(a))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// ctl job attempts
// ---------------------------------------------------------------------------

// cmdJobAttempts renders every placement this job has had, with the DEVICE each
// one ran on.
//
// The device column is the entire reason the command exists. A job that failed
// four times tells you nothing on its own; a job that failed four times on four
// different handsets is a job to fix, and a job that failed four times on the
// same handset is a handset to pull out of the rack. Those two rows of the same
// count send an operator to two different places, and farm.jobs cannot tell
// them apart because it only remembers the current attempt number.
func cmdJobAttempts(ctx context.Context, s *session, args []string) error {
	fs := newFlags("job attempts", s.err)
	var g globals
	g.bind(fs)
	limit := fs.Int("limit", 0, "maximum attempts to render")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("ctl job attempts <job-id> takes exactly one job id")
	}
	if *limit < 0 {
		return usageErrf("--limit is %d; it is a maximum number of attempts to render", *limit)
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}

	path := apiPrefix + "/jobs/" + url.PathEscape(rest[0]) + "/attempts"
	resp, raw, err := fetch[jobAttemptsResponse](ctx, e.client, path, q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	f := &Fields{}
	f.Add("job", resp.JobID)
	f.Add("state", resp.JobState)
	f.Addf("attempt", "%d of at most %d", resp.Attempt, resp.MaxAttempts)
	f.Add("outcomes", countsLine(resp.Outcomes, "succeeded", "failed", "cancelled", "abandoned"))
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if len(resp.Attempts) == 0 {
		e.out.Blank()
		e.out.Text("this job has never been placed on a device. It is waiting for the scheduler, " +
			"or nothing in its pool matches its selector.")
		return nil
	}

	e.out.Blank()
	t := NewTable("#", "DEVICE", "MODEL", "DEVPATH NOW", "STARTED", "DURATION", "OUTCOME",
		"FENCE", "RELEASE", "ERROR").MaxCell(72)
	for _, a := range resp.Attempts {
		t.Row(
			strconv.Itoa(a.Attempt),
			attemptDeviceCell(a),
			firstNonEmpty(a.Model, "—"),
			firstNonEmpty(a.CurrentDevpath, "—"),
			stamp(&a.StartedAt),
			attemptDuration(a),
			firstNonEmpty(a.Outcome, "running"),
			dashInt64(a.Fence),
			releaseCell(a),
			firstNonEmpty(a.Error, "—"),
		)
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Text("devices are identified here by an abbreviated farm_uid; the adb serial is not " +
		"unique and never identifies one. -o json carries the full uid and the serial.")

	e.out.Blank()
	e.out.Text("per device:")
	dt := NewTable("DEVICE", "MODEL", "ATTEMPTS", "OUTCOMES")
	for _, d := range resp.ByDevice {
		dt.Row(
			abbreviate(firstNonEmpty(d.FarmUID, d.DeviceID), farmUIDCellChars),
			firstNonEmpty(d.Model, "—"),
			strconv.Itoa(d.Attempts),
			countsLine(d.Outcomes, "succeeded", "failed", "cancelled", "abandoned"),
		)
	}
	if err := e.out.Table(dt); err != nil {
		return err
	}

	e.out.Blank()
	e.out.Text("%s", attemptVerdict(resp))

	if resp.Truncated {
		e.warnf("the attempt list was cut at the response limit; raise --limit. The per-device " +
			"tally counts only the attempts that were rendered, so it under-reports.")
	}
	return nil
}

// attemptDeviceCell identifies the handset. A NULL device_id is a retired
// phone: farm.job_attempts keeps the placement and drops the reference, so the
// attempt is still a fact even though the hardware is gone.
func attemptDeviceCell(a jobAttempt) string {
	if a.FarmUID != "" {
		return abbreviate(a.FarmUID, farmUIDCellChars)
	}
	if a.DeviceID != "" {
		return shortID(a.DeviceID)
	}
	return "(retired)"
}

// abbreviate cuts an identifier to n characters without adding an ellipsis,
// because every row in the column is cut at the same place and a column of
// ellipses says nothing the caption below the table does not already say.
func abbreviate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// attemptDuration renders how long a placement lasted, or has lasted.
//
// Milliseconds below a second, for the same reason stepDuration uses them: an
// attempt that ended in 200ms did not run a job, it died in provisioning, and
// rendering it as "0s" beside an attempt that ran for an hour hides the one
// number that says which happened.
func attemptDuration(a jobAttempt) string {
	if a.DurationS == nil {
		return "—"
	}
	out := millis(int64(*a.DurationS * 1000))
	if a.FinishedAt == nil {
		return out + "…"
	}
	return out
}

// releaseCell says how the attempt's lease ended.
//
// It is a column of its own because 'completed' and 'holder_expired' are the
// same row shape and opposite stories: the first is work that finished, the
// second is work that was taken away from a holder that stopped renewing. A
// job whose attempts all read 'holder_expired' is not a job with a bug, it is a
// runner that could not keep talking to the control plane.
func releaseCell(a jobAttempt) string {
	switch {
	case a.ReleaseReason != "":
		return a.ReleaseReason
	case a.LeaseState != "":
		return "(" + a.LeaseState + ")"
	}
	return "—"
}

// attemptVerdict states, in one sentence, which of the two stories the table
// tells. It is the reading an operator would do anyway, written down so it is
// done the same way at three in the morning as at noon.
//
// Which is exactly why it counts what it is looking at and nothing else. The
// verdict turns on the ratio of placements to distinct handsets, and both
// numbers here describe the rows this response RENDERED — so a truncated list
// or a placement whose device has since been deleted can make a page of the
// evidence read like all of it. A wrong verdict is worse than none: it sends
// somebody to pull a healthy phone out of a rack, or to hunt a spec bug that
// is really a failing USB port.
func attemptVerdict(resp jobAttemptsResponse) string {
	attempts, devices := len(resp.Attempts), resp.DistinctDevices

	// Every attempt whose device_id survived. farm.job_attempts NULLs the
	// reference when a handset is retired and keeps the placement, so these
	// are the only rows distinct_devices could ever have counted — and the
	// only ones the ratio below is entitled to reason about.
	placed := 0
	for _, a := range resp.Attempts {
		if a.DeviceID != "" {
			placed++
		}
	}
	gone := attempts - placed

	// Truncation first, because it invalidates every count underneath it. The
	// caller asked for a page and the page cannot answer a question about the
	// job; saying "one placement so far" under a warning that the list was cut
	// is a contradiction printed as a conclusion.
	if resp.Truncated {
		return fmt.Sprintf("no verdict: the list was cut at %d, so how many devices this job "+
			"has been through is not knowable from what is on screen. Raise --limit until the "+
			"cut warning goes away.", attempts)
	}

	switch {
	case attempts < 2:
		return "one placement so far: there is nothing yet to compare across devices."
	case devices == 0:
		return fmt.Sprintf("%d placements, and every device they ran on has since been removed "+
			"from the farm, so there is nothing left to compare them against.", attempts)
	case devices == 1 && placed == attempts && len(resp.ByDevice) > 0:
		return fmt.Sprintf("all %d placements ran on ONE device: suspect the hardware before "+
			"the spec. Look at it with: ctl device %s",
			attempts, firstNonEmpty(resp.ByDevice[0].FarmUID, resp.ByDevice[0].DeviceID))
	case devices == 1 && len(resp.ByDevice) > 0:
		// The surviving placements are all on one handset, but some ran on
		// hardware that is gone — so "all of them on one device", the sentence
		// that gets a phone pulled, is not a claim this data supports.
		return fmt.Sprintf("the %d placements still traceable to a device ran on ONE of them "+
			"(%s), and %d more ran on hardware since removed from the farm: suspect that "+
			"handset, but the tally is incomplete.",
			placed, firstNonEmpty(resp.ByDevice[0].FarmUID, resp.ByDevice[0].DeviceID), gone)
	case devices == attempts:
		return fmt.Sprintf("%d placements on %d different devices: the same outcome on "+
			"unrelated hardware is a job problem, not a device problem. Read the failing step "+
			"with: ctl job steps %s --attempt all", attempts, devices, resp.JobID)
	case devices == placed:
		return fmt.Sprintf("the %d placements still traceable to a device ran on %d different "+
			"ones, and %d more ran on hardware since removed: unrelated hardware failing the "+
			"same way is a job problem. Read the failing step with: "+
			"ctl job steps %s --attempt all", placed, devices, gone, resp.JobID)
	}
	return fmt.Sprintf("%d placements across %d devices: the per-device tally above shows which "+
		"handset carries more of them than its share.", attempts, devices)
}
