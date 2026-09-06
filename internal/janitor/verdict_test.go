package janitor

// The pass that closes the permanent half of JOB-10.
//
// Every other sweep in this package is bounded by a row that is still OPEN, so
// a job that reached 'succeeded' is outside all of them: sweepSteps turns the
// orphaned 'running' step of a false success into 'aborted' — tidier, equally
// wrong — and sweepJobs only ever looks at `state = 'running'`. Nothing
// revisited the pair afterwards, which is why "the API will tell you a job
// succeeded when its APK never landed" survived every cycle.
//
// These tests do not call t.Parallel(), for the reason the harness comment in
// janitor_test.go gives: a sweep is table-wide, and a parallel fixture's
// janitor would close this one's rows.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newSucceededJob inserts a job already reported 'succeeded', with a spec of
// `steps` steps and the closed farm.job_attempts row a real placement leaves
// behind. Only the length of spec->'steps' is read by this package, so the
// steps carry ids and nothing else — writing a runnable spec here would
// suggest the sweep understands one, and it does not.
func (f *fixture) newSucceededJob(t *testing.T, steps int, finishedAgo time.Duration) string {
	t.Helper()
	id := f.newUnplacedSucceededJob(t, steps, finishedAgo)
	if _, err := f.pool.Exec(t.Context(), `
INSERT INTO farm.job_attempts (job_id, attempt, device_id, fence, started_at, finished_at, outcome)
VALUES ($1::uuid, 1, $2::uuid, 1, now() - interval '2 hours', now() - interval '1 hour', 'succeeded')`,
		id, f.deviceID); err != nil {
		t.Fatalf("insert attempt for job %s: %v", id, err)
	}
	return id
}

// newUnplacedSucceededJob is the same job with NO attempt row: a success no
// runner in this system wrote, which is what internal/demo's simulated worker
// produces on the demo box.
func (f *fixture) newUnplacedSucceededJob(t *testing.T, steps int, finishedAgo time.Duration) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(), `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, attempt, max_attempts,
                       started_at, finished_at, spec)
VALUES ($1, $2, $3, 'succeeded', 1, 3,
        now() - $4::interval - interval '1 hour', now() - $4::interval,
        jsonb_build_object('steps',
            COALESCE((SELECT jsonb_agg(jsonb_build_object('id', 's/' || i))
                        FROM generate_series(0, $5::int - 1) i), '[]'::jsonb)))
RETURNING id::text`,
		f.tenantID, f.queueID, f.poolID,
		fmt.Sprintf("%d microseconds", int64(finishedAgo/time.Microsecond)), steps).Scan(&id)
	if err != nil {
		t.Fatalf("insert succeeded job: %v", err)
	}
	return id
}

// newStep writes one finished step row in whatever state the test needs.
func (f *fixture) newStep(t *testing.T, jobID string, attempt, index int, state, detail string) {
	t.Helper()
	if detail == "" {
		detail = "{}"
	}
	if _, err := f.pool.Exec(t.Context(), `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state,
                            started_at, finished_at, detail)
VALUES ($1::uuid, $2, $3, $4, 'shell', $5,
        now() - interval '2 hours', now() - interval '1 hour', $6::jsonb)`,
		jobID, attempt, index, fmt.Sprintf("s/%03d", index), state, detail); err != nil {
		t.Fatalf("insert %s step %d for job %s: %v", state, index, jobID, err)
	}
}

// TestASucceededJobContradictedByItsOwnStepsIsCorrected is the reported bug,
// one sweep later than it was reported: the install's row has already been
// moved from 'running' to 'aborted' by sweepSteps, and the job still says it
// succeeded.
func TestASucceededJobContradictedByItsOwnStepsIsCorrected(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 4, time.Minute)
	f.newStep(t, jobID, 1, 0, "ok", "")
	f.newStep(t, jobID, 1, 1, "aborted", "") // the install nothing finished
	f.newStep(t, jobID, 1, 2, "skipped", "")
	f.newStep(t, jobID, 1, 3, "skipped", "")

	f.sweep(t, f.janitor(t))

	state, errText, finished := f.jobState(t, jobID)
	if state != "failed" {
		t.Fatalf("job state = %q; a success no row supports is exactly the status "+
			"JOB-10 says must not stand", state)
	}
	if finished == nil {
		t.Fatal("the corrected job has no finished_at")
	}
	if errText == nil || !strings.Contains(*errText, "reported succeeded") ||
		!strings.Contains(*errText, "s/001") {
		t.Fatalf("job error = %v, want it to name the contradiction and the step", errText)
	}

	// The evidence itself is untouched: the sweep corrects the claim, never
	// the rows the claim is judged against.
	for _, want := range []struct {
		index int
		state string
	}{{0, "ok"}, {1, "aborted"}, {2, "skipped"}, {3, "skipped"}} {
		if got, _, _ := f.stepState(t, jobID, 1, want.index); got != want.state {
			t.Fatalf("step %d = %q after the sweep, want it left at %q", want.index, got, want.state)
		}
	}

	// And an operator can find out why from the API rather than from a pod.
	var events int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM farm.events WHERE job_id = $1::uuid AND kind = 'job_success_unevidenced'`,
		jobID).Scan(&events); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 1 {
		t.Fatalf("%d job_success_unevidenced event(s), want exactly one", events)
	}
}

// A step that left no row at all is the other half of the same lie, and the
// one the step sweep can never see: there is no row for it to find.
func TestASucceededJobMissingStepRowsIsCorrected(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 4, time.Minute)
	f.newStep(t, jobID, 1, 0, "ok", "")
	f.newStep(t, jobID, 1, 1, "ok", "")
	f.newStep(t, jobID, 1, 2, "ok", "")

	f.sweep(t, f.janitor(t))

	state, errText, _ := f.jobState(t, jobID)
	if state != "failed" {
		t.Fatalf("job state = %q; three rows cannot evidence a four-step spec", state)
	}
	if errText == nil || !strings.Contains(*errText, "only 3 of the spec's 4") {
		t.Fatalf("job error = %v, want it to count the missing evidence", errText)
	}
}

// The negative control, and the one that keeps this pass from being a machine
// for failing successful jobs. Every shape below is legitimate under a job that
// really did succeed:
//
//   - 'ok', obviously;
//   - 'skipped', which is what a resumed attempt writes for a step an earlier
//     run of the same attempt completed;
//   - 'failed' carrying detail.tolerated, which is what internal/runner writes
//     for a step whose spec said continue_on_error.
func TestASucceededJobWhoseRowsSupportItIsLeftAlone(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 4, time.Minute)
	f.newStep(t, jobID, 1, 0, "ok", "")
	f.newStep(t, jobID, 1, 1, "skipped", `{"reason": "completed in an earlier attempt of this placement"}`)
	f.newStep(t, jobID, 1, 2, "failed", `{"tolerated": true}`)
	f.newStep(t, jobID, 1, 3, "ok", "")

	// A row at an index the current spec does not have is a leftover from a run
	// this verdict is not about, and it is judged neither way: counted as
	// evidence it would stand in for a step that is missing, and counted as a
	// contradiction it would fail this job on every cycle from now until
	// somebody deleted the row by hand.
	stale := f.newSucceededJob(t, 2, time.Minute)
	f.newStep(t, stale, 1, 0, "ok", "")
	f.newStep(t, stale, 1, 1, "ok", "")
	f.newStep(t, stale, 1, 7, "aborted", "")

	// An untolerated failure among them IS a contradiction. All three jobs are
	// put in front of the SAME cycle, so the discrimination is made by the
	// clauses under test rather than by runs that could have differed some
	// other way.
	other := f.newSucceededJob(t, 1, time.Minute)
	f.newStep(t, other, 1, 0, "failed", "")

	f.sweep(t, f.janitor(t))

	if state, errText, _ := f.jobState(t, stale); state != "succeeded" {
		t.Fatalf("job state = %q (error %q); a row outside the spec's index range is not "+
			"evidence about this spec in either direction", state, derefErr(errText))
	}

	if state, errText, _ := f.jobState(t, jobID); state != "succeeded" {
		t.Fatalf("job state = %q (error %q); a job whose every step is accounted for "+
			"must keep its verdict, or this pass is just a machine for failing jobs",
			state, derefErr(errText))
	}
	if state, _, _ := f.jobState(t, other); state != "failed" {
		t.Fatalf("a success resting on an untolerated step failure survived as %q", state)
	}
}

// The same discipline every pass in this file carries: a live lease means
// something may still be writing these rows, and this loop never overrules a
// live holder. A resumed attempt rewrites step rows as it goes, so a job read
// mid-resume can look contradictory for a moment.
func TestALiveLeaseProtectsASucceededJobFromCorrection(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 4, time.Minute)
	f.newStep(t, jobID, 1, 0, "ok", "")
	f.newStep(t, jobID, 1, 1, "aborted", "")
	f.newLease(t, jobID, "held", 0)

	// 'suspect' is live too — it releases nothing and one heartbeat heals it —
	// so the protection has to survive it. It needs a second fixture because
	// the schema allows one live lease per device, which is the point of that
	// constraint.
	g := newFixture(t)
	quiet := g.newSucceededJob(t, 4, time.Minute)
	g.newStep(t, quiet, 1, 0, "aborted", "")
	g.newLease(t, quiet, "suspect", 0)

	f.sweep(t, f.janitor(t))

	if state, _, _ := f.jobState(t, jobID); state != "succeeded" {
		t.Fatalf("job state = %q; a job with a live lease was corrected out from under "+
			"whatever still holds it", state)
	}
	if state, _, _ := g.jobState(t, quiet); state != "succeeded" {
		t.Fatalf("job state = %q; 'suspect' is a live lease", state)
	}
}

// The evidence clause, and the reason it is not paranoia.
//
// "This success is not evidenced" is a claim about a RUNNER, and a job no
// runner ever opened an attempt on has no runner to have failed.
// internal/runner writes that row before it does anything at all, so a success
// it wrote always has one — and internal/demo's simulated worker, which runs in
// the same process as this loop on the demo box, writes farm.jobs.state
// directly from a model of its own and creates neither attempt nor step rows
// while its jobs carry full specs. Without this clause the demo's dashboard
// would show every completed job as failed a minute after it finished.
func TestASuccessNoRunnerEverPlacedIsNotThisLoopsToJudge(t *testing.T) {
	f := newFixture(t)

	// Four steps in the spec, not one step row, no attempt row: the exact
	// shape internal/demo leaves behind.
	simulated := f.newUnplacedSucceededJob(t, 4, time.Minute)

	// And the same shape WITH the attempt row a real placement writes, so the
	// two differ in one fact and the test says which fact decides.
	placed := f.newSucceededJob(t, 4, time.Minute)

	f.sweep(t, f.janitor(t))

	if state, errText, _ := f.jobState(t, simulated); state != "succeeded" {
		t.Fatalf("job state = %q (error %q); a success this control plane never placed "+
			"is not this loop's to correct", state, derefErr(errText))
	}
	if state, _, _ := f.jobState(t, placed); state != "failed" {
		t.Fatalf("job state = %q; the very same shape WITH an attempt row behind it is "+
			"a placement whose record is missing, and must be corrected", state)
	}
}

// A job nobody ever gave steps to is not a contradiction. Its spec has no
// steps, it has no step rows, and the two agree — the pass must have no opinion
// rather than fail every such job on its first cycle.
func TestASucceededJobWithNoStepsAtAllIsNotAContradiction(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 0, time.Minute)

	f.sweep(t, f.janitor(t))

	if state, errText, _ := f.jobState(t, jobID); state != "succeeded" {
		t.Fatalf("job state = %q (error %q), want it left alone", state, derefErr(errText))
	}
}

// derefErr renders a nullable error column for a failure message: the pointer
// itself says nothing to whoever reads the test output.
func derefErr(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// The message names the step the operator should look at, and the two kinds of
// bad row are not the same step: Bad is a superset of Live. Reporting the first
// CONTRADICTING row as the one "still 'pending' or 'running'" would send
// somebody looking for a process behind an aborted step, which by definition
// has none.
func TestTheCorrectionNamesTheRightStep(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 4, time.Minute)
	f.newStep(t, jobID, 1, 0, "aborted", "") // lowest-indexed contradiction
	f.newStep(t, jobID, 1, 1, "running", "") // lowest-indexed live row
	f.newStep(t, jobID, 1, 2, "ok", "")
	f.newStep(t, jobID, 1, 3, "ok", "")

	// The step sweep would ordinarily close the 'running' row first; this
	// fixture's lease is already gone, so the assertion is made on the scan's
	// own reading rather than on what survives it.
	rows, err := f.janitor(t).scanUnevidenced(t.Context())
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	var got *unevidenced
	for i := range rows {
		if rows[i].JobID == jobID {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("the contradicted job was not scanned at all")
	}
	if got.LiveStep != "s/001" || got.BadStep != "s/000" {
		t.Fatalf("live_step = %q, contradicting_step = %q; want s/001 and s/000",
			got.LiveStep, got.BadStep)
	}
	if why := got.why("janitor"); !strings.Contains(why, "s/001") || strings.Contains(why, "s/000") {
		t.Fatalf("the message reads %q; the row it calls still executing must be the "+
			"live one, not the first contradicting one", why)
	}
}

// The window is the only bound in this package that can make a repair NOT
// happen, so it is asserted from both sides: a contradiction inside it is
// corrected, one older than it is left, and widening the window reaches it.
//
// It exists because succeeded jobs are the one thing here that accumulates
// forever. Without an upper bound the scan re-reads the whole job history every
// cycle and eventually exceeds CallTimeout, at which point the backstop stops
// running at all — silently, on the busiest farm.
func TestTheVerdictWindowBoundsWhatThePassLooksAt(t *testing.T) {
	f := newFixture(t)

	recent := f.newSucceededJob(t, 1, time.Minute)
	f.newStep(t, recent, 1, 0, "aborted", "")
	old := f.newSucceededJob(t, 1, 3*time.Hour)
	f.newStep(t, old, 1, 0, "aborted", "")

	f.sweep(t, f.janitorWith(t, func(c *Config) { c.VerdictWindow = time.Hour }))

	if state, _, _ := f.jobState(t, recent); state != "failed" {
		t.Fatalf("a contradiction inside the window was left at %q", state)
	}
	if state, _, _ := f.jobState(t, old); state != "succeeded" {
		t.Fatalf("a contradiction older than the window was corrected (%q); the bound "+
			"is what keeps this pass from re-reading the whole job history every cycle", state)
	}

	// And it is a window, not an amnesty: an operator recovering from a longer
	// outage widens it and the older row is reached. A second fixture, because
	// a Janitor holds its advisory lock until the test ends and two on one key
	// would leave the wider-windowed one a standby that sweeps nothing.
	g := newFixture(t)
	g.sweep(t, g.janitorWith(t, func(c *Config) { c.VerdictWindow = 24 * time.Hour }))
	if state, _, _ := f.jobState(t, old); state != "failed" {
		t.Fatalf("widening the window did not reach the older contradiction (%q)", state)
	}
}

// The window is only a bound if the index behind it can be used, and whether it
// can turns on successClock being spelled exactly as migration 00020 spells the
// expression jobs_recent_success is built on. Nothing about a drift between the
// two is visible at runtime: the sweep keeps working, just by reading the whole
// job history every thirty seconds until it outgrows CallTimeout and stops.
//
// So the planner is asked directly. Sequential scans are disabled for the
// question, which does not make an unusable index usable — it only removes the
// answer "the table is small enough not to bother", which is true of every test
// database and of no real one.
func TestTheVerdictScanCanUseItsIndex(t *testing.T) {
	f := newFixture(t)

	conn, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()
	// The setting is per-session and this connection goes back to a shared
	// pool, so it is put back the way it was found.
	if _, err := conn.Exec(t.Context(), `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disabling sequential scans: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET enable_seqscan`) }()

	rows, err := conn.Query(t.Context(), "EXPLAIN "+successScan,
		pgInterval(DefaultSettle), pgInterval(DefaultVerdictWindow), DefaultBatch)
	if err != nil {
		t.Fatalf("explaining the verdict scan: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("reading the plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}

	// The index NAME on its own proves nothing: with seqscans off the planner
	// will happily read the whole partial index end to end, which is the same
	// unbounded read this window exists to avoid. What has to be there is the
	// Index Cond — the window pushed into the index — and that only appears
	// when the scan's expression matches the indexed one.
	lines := strings.Split(strings.TrimSpace(plan.String()), "\n")
	cond := ""
	for i, line := range lines {
		if strings.Contains(line, "using jobs_recent_success") && i+1 < len(lines) {
			cond = strings.TrimSpace(lines[i+1])
		}
	}
	if !strings.HasPrefix(cond, "Index Cond:") ||
		!strings.Contains(cond, "COALESCE(finished_at, started_at, created_at)") {
		t.Fatalf("the verdict scan's window is not pushed into jobs_recent_success, so it "+
			"reads every succeeded job ever run, every cycle. successClock and the "+
			"expression in migration 00020 have to match exactly.\nplan:\n%s", plan.String())
	}
}

// The one pass here that does not run every cycle, and the reason: its cost is
// proportional to how much the farm has SUCCEEDED at, so a healthy farm pays
// for it in full and pays it every time. What the interval delays is the
// correction of a record — no device is held while it waits — which is what
// makes spacing it out safe when spacing out the other passes would not be.
func TestTheVerdictPassIsSpacedOutRatherThanRunEveryCycle(t *testing.T) {
	f := newFixture(t)

	jobID := f.newSucceededJob(t, 1, time.Minute)
	f.newStep(t, jobID, 1, 0, "aborted", "")

	// An hour between passes: the first cycle takes it, the second must not.
	j := f.janitorWith(t, func(c *Config) { c.VerdictInterval = time.Hour })
	f.sweep(t, j)
	if state, _, _ := f.jobState(t, jobID); state != "failed" {
		t.Fatalf("the first cycle did not run the pass at all (job is %q); the interval "+
			"must not delay a process's first sweep", state)
	}

	// Put a second contradiction in front of it and cycle again. The pass is
	// not due, so nothing about this one changes.
	second := f.newSucceededJob(t, 1, time.Minute)
	f.newStep(t, second, 1, 0, "aborted", "")
	f.sweep(t, j)
	if state, _, _ := f.jobState(t, second); state != "succeeded" {
		t.Fatalf("job state = %q; the pass ran twice inside its own interval", state)
	}

	// And it is a delay, not a skip: the same janitor closes it once due.
	j.verdictDue = time.Now().Add(-time.Second)
	f.sweep(t, j)
	if state, _, _ := f.jobState(t, second); state != "failed" {
		t.Fatalf("job state = %q; the interval dropped a repair instead of delaying it", state)
	}
}

// A window that cannot outlive its own settle would scan an empty range and
// report a clean farm forever.
func TestAVerdictWindowInsideTheSettleIsWidened(t *testing.T) {
	t.Parallel()

	cfg := Config{Settle: 10 * time.Minute, VerdictWindow: time.Minute}
	cfg.applyDefaults()
	if cfg.VerdictWindow <= cfg.Settle {
		t.Fatalf("VerdictWindow = %s with Settle = %s; the pass would look at a range "+
			"that cannot contain a row", cfg.VerdictWindow, cfg.Settle)
	}

	var zero Config
	zero.applyDefaults()
	if zero.VerdictWindow != DefaultVerdictWindow {
		t.Fatalf("VerdictWindow defaulted to %s, want %s", zero.VerdictWindow, DefaultVerdictWindow)
	}
}

// The pass is bounded like every other one, so a control plane that comes back
// to thousands of these does not close them in one statement that monopolises
// the pool the renewal path borrows from.
func TestTheVerdictSweepIsBounded(t *testing.T) {
	f := newFixture(t)

	ids := make([]string, 3)
	for i := range ids {
		ids[i] = f.newSucceededJob(t, 1, time.Minute)
		f.newStep(t, ids[i], 1, 0, "aborted", "")
	}

	j := f.janitorWith(t, func(c *Config) { c.Batch = 1 })

	// Counted per cycle rather than asserted as "exactly one after the first",
	// because rows outlive their test here by design and an earlier fixture's
	// job can legitimately take this cycle's single slot. The property is that
	// no cycle takes more than the batch, and that the bound delays the repair
	// without dropping it.
	corrected := 0
	for cycle := 1; cycle <= 6 && corrected < len(ids); cycle++ {
		before := corrected
		f.sweep(t, j)
		corrected = 0
		for _, id := range ids {
			if state, _, _ := f.jobState(t, id); state == "failed" {
				corrected++
			}
		}
		if corrected-before > 1 {
			t.Fatalf("one cycle with Batch=1 corrected %d job(s)", corrected-before)
		}
	}
	if corrected != len(ids) {
		t.Fatalf("%d of %d contradicted jobs were corrected across six bounded cycles; "+
			"the bound must delay the repair, not drop it", corrected, len(ids))
	}
}
