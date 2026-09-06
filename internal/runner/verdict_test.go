package runner

// JOB-10: a job's reported state agrees with its step rows.
//
// Three properties are asserted here, and they are asserted against a real
// PostgreSQL because two of them are about rows and the third is a SQL
// predicate:
//
//   - every ENDING writes the steps it never reached, so a step list stops
//     trailing off — and an abandonment, which is not an ending, does not,
//     because those steps are about to be run by the resume;
//   - 'succeeded' is written only when the rows support it, and withheld —
//     never downgraded — when they do not;
//   - a supervisor's safety net does not write the verdict the runner refused.
//
// The fixture builds a whole placement (host, hub, slot, device, lease) rather
// than the tenant-and-job minimum, because the last property is only visible in
// farm.jobs: writeJobState's fence guard needs a live lease, so without one a
// success that WAS written and a success that was WITHHELD look identical.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// placedJob is one job that has been placed on one device under one live
// lease: everything farm.jobs, farm.job_attempts and farm.leases need before
// [Runner.Run] can write a verdict anybody can read back.
type placedJob struct {
	jobID    string
	deviceID string
	leaseID  string
	fence    int64
}

func (p placedJob) placement() Placement {
	return Placement{
		JobID: p.jobID, DeviceID: p.deviceID, LeaseID: p.leaseID,
		Fence: p.fence, Devpath: "usb:3-1.1", Endpoint: "127.0.0.1:5037",
	}
}

// newPlacedJob seeds the whole chain. The topology rows exist because
// farm.leases and farm.devices demand them, not because anything here is about
// topology; the job's spec is the one part a test cares to vary.
func newPlacedJob(t *testing.T, pool *pgxpool.Pool, spec jobspec.Spec) placedJob {
	t.Helper()
	ctx := t.Context()
	tag := fmt.Sprintf("v%04d", fixtureSeq.Add(1))

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seeding %s failed: %v\nstatement: %s", tag, err, q)
		}
	}
	exec(`INSERT INTO farm.racks (id) VALUES ($1)`, "rack-"+tag)
	exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ($1, $2, '127.0.0.1:5037')`,
		"host-"+tag, "rack-"+tag)
	exec(`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3)`, "host-"+tag)
	exec(`INSERT INTO farm.power_domains (host_id, kind, control)
	      VALUES ($1, 'per_port', 'uhubctl')`, "host-"+tag)
	exec(`INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
	      SELECT $1, c.id, '3-1', 8, true FROM farm.controllers c WHERE c.host_id = $1`, "host-"+tag)
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, "pool-"+tag)
	exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, "tenant-"+tag)
	exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, "queue-"+tag, "tenant-"+tag)
	exec(`INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number,
	                              usb_path, topo_path, rack_slot)
	      SELECT $1, h.id, pd.id, 1, '3-1.1', ('x' || $2 || '.p1')::ltree, 'R-' || $2 || '-P1'
	        FROM farm.hubs h, farm.power_domains pd
	       WHERE h.host_id = $1 AND pd.host_id = $1`, "host-"+tag, tag)

	var p placedJob
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id,
                          manufacturer, model, sdk_int)
SELECT 'df-' || md5($1), 'SER-' || $1, $2, $3, s.id, 'Google', 'Pixel Test', 34
  FROM farm.slots s WHERE s.host_id = $3
RETURNING id::text`, tag, "pool-"+tag, "host-"+tag).Scan(&p.deviceID); err != nil {
		t.Fatalf("seeding device for %s: %v", tag, err)
	}

	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshalling the spec: %v", err)
	}
	// attempt 0: Run claims attempt 1, which is the shape a first placement has.
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, attempt, max_attempts,
                       started_at, spec)
VALUES ($1, $2, $3, 'running', 0, 3, now(), $4::jsonb)
RETURNING id::text`,
		"tenant-"+tag, "queue-"+tag, "pool-"+tag, string(raw)).Scan(&p.jobID); err != nil {
		t.Fatalf("seeding job for %s: %v", tag, err)
	}

	if err := pool.QueryRow(ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, state,
                         ttl, grace, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, $1::uuid, $2, $3,
       'pod-test', gen_random_uuid(), 'held',
       interval '15 minutes', interval '30 minutes',
       now() + interval '15 minutes', now() + interval '45 minutes'
  FROM farm.devices d WHERE d.id = $4::uuid
RETURNING id::text, fence`,
		p.jobID, "tenant-"+tag, "queue-"+tag, p.deviceID).Scan(&p.leaseID, &p.fence); err != nil {
		t.Fatalf("seeding lease for %s: %v", tag, err)
	}
	return p
}

// stepRow is one farm.job_steps row as these tests read it.
type stepRow struct {
	Index  int
	ID     string
	State  string
	Reason string
}

func readSteps(t *testing.T, pool *pgxpool.Pool, jobID string, attempt int) []stepRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT step_index, step_id, state, COALESCE(detail->>'reason', '')
  FROM farm.job_steps
 WHERE job_id = $1::uuid AND attempt = $2
 ORDER BY step_index`, jobID, attempt)
	if err != nil {
		t.Fatalf("reading step rows: %v", err)
	}
	defer rows.Close()

	var out []stepRow
	for rows.Next() {
		var s stepRow
		if err := rows.Scan(&s.Index, &s.ID, &s.State, &s.Reason); err != nil {
			t.Fatalf("scanning step rows: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func wantStates(t *testing.T, got []stepRow, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d step row(s), want %d: %+v", len(got), len(want), got)
	}
	for i, s := range got {
		if s.Index != i || s.State != want[i] {
			t.Fatalf("row %d = %+v, want index %d in state %q", i, s, i, want[i])
		}
	}
}

func jobStateOf(t *testing.T, pool *pgxpool.Pool, jobID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(),
		`SELECT state FROM farm.jobs WHERE id = $1::uuid`, jobID).Scan(&state); err != nil {
		t.Fatalf("reading job state: %v", err)
	}
	return state
}

func eventCount(t *testing.T, pool *pgxpool.Pool, jobID, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM farm.events WHERE job_id = $1::uuid AND kind = $2`,
		jobID, kind).Scan(&n); err != nil {
		t.Fatalf("counting %s events: %v", kind, err)
	}
	return n
}

// fourShellSteps is a spec whose every step is addressable from a fakeConn by
// its command, so a test can decide what happens at a chosen step without
// counting round trips.
func fourShellSteps() jobspec.Spec {
	spec := jobspec.New(
		jobspec.Step{ID: "one", Payload: jobspec.Shell{Command: "echo one"}},
		jobspec.Step{ID: "two", Payload: jobspec.Shell{Command: "echo two"}},
		jobspec.Step{ID: "three", Payload: jobspec.Shell{Command: "echo three"}},
		jobspec.Step{ID: "four", Payload: jobspec.Shell{Command: "echo four"}},
	)
	// jobspec.Validate refuses a step with no effective timeout, and Run
	// validates the spec before it touches the device.
	spec.DefaultTimeout = jobspec.Duration(time.Minute)
	return spec
}

// ---------------------------------------------------------------------------
// Endings write the steps they never reached
// ---------------------------------------------------------------------------

// The job's own max_runtime elapsing is an ENDING: the user's clock is spent,
// the job goes to 'failed' and nothing will run the rest of the spec. Before
// this, that path wrote a row for the step it was standing on and then returned,
// leaving the steps after it with no row at all — so a job that died two steps
// into four read as a two-step job.
func TestAMaxRuntimeEndingRecordsTheStepsItNeverReached(t *testing.T) {
	pool := requireDB(t)
	spec := fourShellSteps()
	p := newPlacedJob(t, pool, spec).placement()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	kinds, err := r.loadKinds(ctx)
	if err != nil {
		t.Fatalf("loading step kinds: %v", err)
	}

	run, endRun := context.WithCancelCause(ctx)
	defer endRun(nil)

	dev := &fakeConn{shell: func(_ context.Context, _ int, command string) (ShellOutput, error) {
		if command == "echo two" {
			// The user's clock runs out while step 1 is on the wire. This is
			// not a transport failure: the run context itself is over.
			endRun(ErrMaxRuntime)
			return ShellOutput{}, transportErr("connection reset")
		}
		return okShell(""), nil
	}}

	out := Outcome{Attempt: 1}
	res := r.execute(run, ctx, r.log, fakeHolder{ctx: run}, p, jobRow{}, spec, kinds,
		newCheckpoint(1, p, spec), false, dev, &out)

	if out.State != StateFailed || !res.permanent {
		t.Fatalf("out = %+v, res = %+v; an elapsed max_runtime is a permanent failure", out, res)
	}
	rows := readSteps(t, pool, p.JobID, 1)
	wantStates(t, rows, "ok", "failed", "skipped", "skipped")

	for _, s := range rows[2:] {
		if !strings.Contains(s.Reason, "was stopped mid-run") || !strings.Contains(s.Reason, "step 1 (two/shell)") {
			t.Fatalf("row %d reason = %q, want it to name step 1 as stopped rather than failed",
				s.Index, s.Reason)
		}
	}
}

// The counterpart, and the reason the tail is written AFTER classifyAbort has
// spoken. A fenced attempt is not an ending: the same attempt number is resumed
// from the checkpoint by whoever holds the device next, and it will rewrite
// these very rows. Marking the steps it has not reached 'skipped' would tell an
// operator that work about to happen never will.
func TestAFencedAttemptDoesNotWriteOffTheStepsItWillResume(t *testing.T) {
	pool := requireDB(t)
	spec := fourShellSteps()
	p := newPlacedJob(t, pool, spec).placement()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	kinds, err := r.loadKinds(ctx)
	if err != nil {
		t.Fatalf("loading step kinds: %v", err)
	}

	run, endRun := context.WithCancelCause(ctx)
	defer endRun(nil)

	dev := &fakeConn{shell: func(_ context.Context, _ int, command string) (ShellOutput, error) {
		if command == "echo two" {
			endRun(lease.ErrFenced)
			return ShellOutput{}, transportErr("connection reset")
		}
		return okShell(""), nil
	}}

	out := Outcome{Attempt: 1}
	r.execute(run, ctx, r.log, fakeHolder{ctx: run, fenced: true}, p, jobRow{}, spec, kinds,
		newCheckpoint(1, p, spec), false, dev, &out)

	if out.State != StateAbandoned || !out.Fenced {
		t.Fatalf("out = %+v, want an abandoned, fenced attempt", out)
	}
	// Two rows and no more: the step that was interrupted, and the one before
	// it. Steps 2 and 3 belong to the resume.
	wantStates(t, readSteps(t, pool, p.JobID, 1), "ok", "aborted")
}

// A refused resume ends the attempt permanently — the job is never placed
// again — so everything after the step it refused to repeat is work nobody
// will do, and says so.
func TestARefusedResumeRecordsTheStepsItWillNeverRun(t *testing.T) {
	pool := requireDB(t)
	// A plain shell step is NOT idempotent — farm.step_kinds says so — because
	// its command may have had an effect nobody can undo, so a resume that
	// finds one in flight refuses rather than risk repeating it.
	spec := fourShellSteps()
	p := newPlacedJob(t, pool, spec).placement()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	kinds, err := r.loadKinds(ctx)
	if err != nil {
		t.Fatalf("loading step kinds: %v", err)
	}

	ckpt := newCheckpoint(1, p, spec)
	ckpt.markDone(0, spec.Steps[0])
	ckpt.markInFlight(1, spec.Steps[1])

	out := Outcome{Attempt: 1}
	res := r.execute(ctx, ctx, r.log, fakeHolder{ctx: ctx}, p, jobRow{}, spec, kinds,
		ckpt, true, &fakeConn{}, &out)

	if out.State != StateFailed || !res.permanent {
		t.Fatalf("out = %+v, res = %+v; a refused resume is a permanent failure", out, res)
	}
	// No row for step 0: it completed in the run that was interrupted, and
	// that run wrote its own. The refusal is at index 1, and 2 and 3 follow it.
	rows := readSteps(t, pool, p.JobID, 1)
	if len(rows) != 3 {
		t.Fatalf("%d step row(s), want the refused step and the two after it: %+v", len(rows), rows)
	}
	for i, want := range []stepRow{{Index: 1, State: "failed"}, {Index: 2, State: "skipped"}, {Index: 3, State: "skipped"}} {
		if rows[i].Index != want.Index || rows[i].State != want.State {
			t.Fatalf("row %d = %+v, want index %d in state %q", i, rows[i], want.Index, want.State)
		}
	}
	for _, s := range rows[1:] {
		if !strings.Contains(s.Reason, "could not be resumed safely") {
			t.Fatalf("row %d reason = %q, want it to name the refusal", s.Index, s.Reason)
		}
	}
}

// ---------------------------------------------------------------------------
// The verdict is checked against the rows
// ---------------------------------------------------------------------------

// attemptStepsAgree is the predicate the whole requirement rests on, so it is
// tested against rows rather than through a run: every shape it must recognise
// is a shape a crash can leave behind.
func TestAgreementIsJudgedOnTheRowsThemselves(t *testing.T) {
	pool := requireDB(t)
	steps := fourSteps()
	p := newPlacedJob(t, pool, jobspec.New(steps...)).placement()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Nothing recorded at all, which is what a run whose every write failed
	// leaves behind. Four steps expected, none evidenced.
	why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps))
	if !strings.Contains(why, "0 of the spec's 4") {
		t.Fatalf("with no rows at all, attemptStepsAgree said %q", why)
	}

	// Three of four settled: the shape a run leaves when one terminal write
	// never happened and no row was ever inserted for that index.
	for i := 0; i < 3; i++ {
		r.recordStepStart(ctx, r.log, p, 1, i, steps[i])
		r.recordStep(ctx, r.log, p, 1, i, steps[i], "ok", nil, nil, "", nil)
	}
	why = r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps))
	if !strings.Contains(why, "3 of the spec's 4") || !strings.Contains(why, "1 step(s) left no record") {
		t.Fatalf("with one step unrecorded, attemptStepsAgree said %q", why)
	}

	// The reported shape of JOB-10: the row was inserted 'running' and its
	// terminal write was swallowed, so the loop ran on and the step still
	// claims to be executing.
	r.recordStepStart(ctx, r.log, p, 1, 3, steps[3])
	why = r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps))
	if !strings.Contains(why, "still say 'pending' or 'running'") {
		t.Fatalf("with a step row left at 'running', attemptStepsAgree said %q", why)
	}

	// And the honest success: every step settled.
	r.recordStep(ctx, r.log, p, 1, 3, steps[3], "ok", nil, nil, "", nil)
	if why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps)); why != "" {
		t.Fatalf("a complete, settled attempt was called a disagreement: %q", why)
	}

	// A row at an index the spec does not have is not evidence for it: it is a
	// leftover from a run this verdict is not about.
	r.recordStep(ctx, r.log, p, 1, 9, steps[0], "ok", nil, nil, "", nil)
	if why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps)+1); !strings.Contains(why, "4 of the spec's 5") {
		t.Fatalf("an out-of-range row was counted as evidence: %q", why)
	}

	// And it is not evidence AGAINST it either, which is the half that would
	// hurt: an out-of-range row in a state that contradicts success would
	// withhold this job's verdict on this attempt and on every attempt after
	// it, forever, with nothing an operator could do to clear it.
	r.recordStep(ctx, r.log, p, 1, 9, steps[0], "aborted", nil, nil, "", nil)
	if why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps)); why != "" {
		t.Fatalf("an out-of-range row was read as a contradiction: %q", why)
	}
}

// A step row that says the work did not finish is not evidence for a success
// either, however settled it is.
//
// This is the predicate internal/janitor's stepBeliesSuccess names, spelled the
// same on this side. If it were not — if a settled-but-bad row counted here as
// a verdict — this package would write 'succeeded' and the very next janitor
// cycle would reverse it, each of the two logging that the other must be at
// fault. A test that only checked "every index has some row" would pass while
// that happened.
func TestARowSayingTheWorkDidNotFinishIsNotEvidenceForASuccess(t *testing.T) {
	pool := requireDB(t)
	steps := fourSteps()
	p := newPlacedJob(t, pool, jobspec.New(steps...)).placement()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	for i := range steps {
		r.recordStep(ctx, r.log, p, 1, i, steps[i], "ok", nil, nil, "", nil)
	}
	if why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps)); why != "" {
		t.Fatalf("four settled 'ok' rows were called a disagreement: %q", why)
	}

	for _, tc := range []struct {
		name   string
		state  string
		detail map[string]any
		want   string
	}{
		// Left by an earlier run of this same attempt that the current run
		// should have overwritten and could not.
		{"aborted", "aborted", nil, "record work that did not finish"},
		{"failed", "failed", nil, "record work that did not finish"},
		// Unless the spec said so, in which case it is exactly what a
		// successful job's rows look like.
		{"failed but tolerated", "failed", map[string]any{"tolerated": true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.recordStep(ctx, r.log, p, 1, 2, steps[2], tc.state, nil, nil, "", tc.detail)
			why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps))
			switch {
			case tc.want == "" && why != "":
				t.Fatalf("a %s row was called a disagreement: %q", tc.name, why)
			case tc.want != "" && !strings.Contains(why, tc.want):
				t.Fatalf("a %s row produced %q, want it to say %q", tc.name, why, tc.want)
			case tc.want != "" && !strings.Contains(why, steps[2].ID):
				t.Fatalf("a %s row produced %q, want it to name the step", tc.name, why)
			}
		})
	}
}

// The message names the step the operator should look at, and the two kinds of
// bad row are not the same step. A row still 'running' is one somebody might
// find a process for; an 'aborted' one is not, and pointing at the aborted step
// while saying "still executing" sends them looking for a process that never
// existed.
func TestTheDisagreementNamesTheRightStep(t *testing.T) {
	pool := requireDB(t)
	steps := fourSteps()
	p := newPlacedJob(t, pool, jobspec.New(steps...)).placement()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Index 1 aborted, index 2 still running: the lowest-indexed CONTRADICTING
	// row is 1, and the lowest-indexed LIVE one is 2.
	r.recordStep(ctx, r.log, p, 1, 0, steps[0], "ok", nil, nil, "", nil)
	r.recordStep(ctx, r.log, p, 1, 1, steps[1], "aborted", nil, nil, "", nil)
	r.recordStepStart(ctx, r.log, p, 1, 2, steps[2])
	r.recordStep(ctx, r.log, p, 1, 3, steps[3], "ok", nil, nil, "", nil)

	why := r.attemptStepsAgree(ctx, ctx, r.log, p, 1, len(steps))
	if !strings.Contains(why, "still say 'pending' or 'running'") {
		t.Fatalf("a live row did not win the report: %q", why)
	}
	if !strings.Contains(why, steps[2].ID) || strings.Contains(why, steps[1].ID) {
		t.Fatalf("the live-row message names %q; it must name %q, the step that is "+
			"actually recorded as executing", why, steps[2].ID)
	}
}

// The end-to-end property, through Run: 'succeeded' reaches farm.jobs only when
// the rows support it, and is WITHHELD rather than downgraded when they do not.
//
// The disagreement is manufactured the way the real one arises — a step row
// that is not there when the verdict is written — but deterministically: the
// last step's shell call deletes the first step's row. In production the same
// gap comes from recordStep failing on the wire and being swallowed, which is
// deliberate and pinned by TestABookkeepingFailureIsNeverAVerdictOnTheJob.
func TestSucceededIsWrittenOnlyWhenTheStepRowsSupportIt(t *testing.T) {
	pool := requireDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	jobError := func(t *testing.T, jobID string) string {
		t.Helper()
		var e *string
		if err := pool.QueryRow(ctx,
			`SELECT error FROM farm.jobs WHERE id = $1::uuid`, jobID).Scan(&e); err != nil {
			t.Fatalf("reading the job's error: %v", err)
		}
		if e == nil {
			return ""
		}
		return *e
	}

	run := func(t *testing.T, sabotage bool) (Outcome, string, placedJob) {
		t.Helper()
		spec := fourShellSteps()
		job := newPlacedJob(t, pool, spec)

		r, logs := testRunner(t, func(c *Config) { c.Pool = pool })
		dev := &fakeConn{shell: func(_ context.Context, _ int, command string) (ShellOutput, error) {
			if sabotage && command == "echo four" {
				if _, err := pool.Exec(ctx,
					`DELETE FROM farm.job_steps WHERE job_id = $1::uuid AND step_index = 0`,
					job.jobID); err != nil {
					t.Errorf("deleting the step row: %v", err)
				}
			}
			return okShell(""), nil
		}}

		out, err := r.Run(ctx, fakeHolder{ctx: ctx}, job.placement(), dev)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out.State != StateSucceeded {
			t.Fatalf("out = %+v; every step ran, so the ATTEMPT succeeded either way", out)
		}
		_ = logs
		return out, jobStateOf(t, pool, job.jobID), job
	}

	t.Run("rows agree", func(t *testing.T) {
		out, state, job := run(t, false)
		if out.VerdictWithheld {
			t.Fatalf("the verdict was withheld from a job whose rows are complete: %q", out.Error)
		}
		if state != "succeeded" {
			t.Fatalf("farm.jobs.state = %q, want succeeded", state)
		}
		wantStates(t, readSteps(t, pool, job.jobID, 1), "ok", "ok", "ok", "ok")
		if n := eventCount(t, pool, job.jobID, "job_success_not_asserted"); n != 0 {
			t.Fatalf("%d refusal event(s) for a job that was reported succeeded", n)
		}
		if e := jobError(t, job.jobID); e != "" {
			t.Fatalf("a succeeded job carries an error: %q", e)
		}
	})

	t.Run("rows disagree", func(t *testing.T) {
		out, state, job := run(t, true)

		if !out.VerdictWithheld {
			t.Fatal("a success was reported although a step of it left no record; " +
				"the API would tell an operator this job succeeded")
		}
		// Withheld, NOT downgraded. 'running' is the absence of a verdict, and
		// the absence is what keeps a bookkeeping failure from deciding
		// anything about the job in either direction.
		if state != "running" {
			t.Fatalf("farm.jobs.state = %q, want it left at 'running': a verdict of "+
				"'failed' here would say a job failed when every step of it ran", state)
		}
		if !strings.Contains(out.Error, "3 of the spec's 4") {
			t.Fatalf("Outcome.Error = %q, want it to count the evidence", out.Error)
		}
		// The attempt's own truth is untouched: it did succeed.
		var outcome string
		if err := pool.QueryRow(ctx,
			`SELECT outcome FROM farm.job_attempts WHERE job_id = $1::uuid AND attempt = 1`,
			job.jobID).Scan(&outcome); err != nil {
			t.Fatalf("reading the attempt row: %v", err)
		}
		if outcome != "succeeded" {
			t.Fatalf("farm.job_attempts.outcome = %q; the attempt ran every step it was given", outcome)
		}
		// And the operator can find out why from the API, not only from a pod:
		// on the job's own row while it waits, and in an event that survives
		// the janitor overwriting that row when it places the job again.
		if n := eventCount(t, pool, job.jobID, "job_success_not_asserted"); n != 1 {
			t.Fatalf("%d job_success_not_asserted event(s), want exactly one", n)
		}
		e := jobError(t, job.jobID)
		if !strings.Contains(e, "do not support") || !strings.Contains(e, "3 of the spec's 4") {
			t.Fatalf("farm.jobs.error = %q; the job's own row has to say why it is not "+
				"finished, or an operator has only a pod's logs to go on", e)
		}
	})
}

// The two packages that ask "do these rows support a success?" must ask it in
// exactly the same words.
//
// internal/runner asks before a success is written; internal/janitor asks over
// successes already written. A row one accepts and the other rejects is a job
// this package reports succeeded and the next janitor cycle records as failed,
// with each side's comment blaming the other. Both files say at length that the
// two must not diverge, and a comment is not a guard: the constants are
// unexported, in different packages, with no shared source between them —
// internal/janitor imports nothing that could end a lease and is not going to
// start importing this package for a string.
//
// So the other package's source is read. That is the same barrier technique
// janitor_test.go's TestPackageCannotEndALease uses, applied across the one
// seam these two share.
func TestTheAgreementPredicateMatchesTheJanitorsWordForWord(t *testing.T) {
	t.Parallel()

	pick := func(path string) string {
		t.Helper()
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		const marker = "stepBeliesSuccess = `"
		i := strings.Index(string(src), marker)
		if i < 0 {
			t.Fatalf("%s no longer declares stepBeliesSuccess; the two predicates "+
				"cannot be compared, so nothing is holding them together", path)
		}
		rest := string(src[i+len(marker):])
		j := strings.Index(rest, "`")
		if j < 0 {
			t.Fatalf("%s: stepBeliesSuccess has no closing backtick", path)
		}
		return rest[:j]
	}

	mine := pick("runner.go")
	theirs := pick("../janitor/janitor.go")
	if mine != theirs {
		t.Fatalf("the two spellings of stepBeliesSuccess have diverged.\n"+
			"internal/runner:\n%s\ninternal/janitor:\n%s\n"+
			"A row one of them accepts and the other rejects is a job reported "+
			"succeeded here and reversed there on the next sweep.", mine, theirs)
	}
	// And it is the predicate it claims to be, not an empty string that would
	// match itself in both files.
	for _, part := range []string{"pending", "running", "aborted", "failed", "tolerated"} {
		if !strings.Contains(mine, part) {
			t.Fatalf("stepBeliesSuccess does not mention %q: %s", part, mine)
		}
	}
}

// A pool that cannot answer is not agreement. The runner's other bookkeeping
// swallows a database failure on purpose, and must keep doing so — but a
// verdict of 'succeeded' rests on rows nobody could read, so it is withheld
// like any other disagreement — after a few tries, because the cost of giving
// up is a whole re-run of the job.
func TestAnUnreadableLedgerWithholdsTheVerdictRatherThanAssertingIt(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("building the unreachable pool: %v", err)
	}
	defer pool.Close()

	r, logs := testRunner(t, func(c *Config) {
		c.Pool = pool
		c.CallTimeout = 2 * time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	why := r.attemptStepsAgree(ctx, ctx, r.log, Placement{
		JobID: "11111111-1111-4111-8111-111111111111", DeviceID: "d", Devpath: "usb:3-1.4", Fence: 7,
	}, 1, 4)
	if !strings.Contains(why, "could not be read") {
		t.Fatalf("attemptStepsAgree said %q about a ledger it could not read", why)
	}
	if n := logs.count("could not read the step rows that a 'succeeded' verdict rests on"); n != 1 {
		t.Fatalf("the unreadable ledger was logged %d time(s); an operator has to see this", n)
	}
	// Tried more than once before giving up: withholding costs the job a full
	// re-execution on another device, which is far too much to spend on one
	// lost socket. The literal is deliberate — asserting agreementTries-1 alone
	// would make a one-shot read pass by agreeing with itself.
	if agreementTries < 2 {
		t.Fatalf("agreementTries = %d; the one read whose failure costs a whole re-run "+
			"is the one read this package retries", agreementTries)
	}
	if n := logs.count("retrying rather than costing the job a whole re-run"); n != agreementTries-1 {
		t.Fatalf("the read was retried %d time(s), want %d", n, agreementTries-1)
	}
}
