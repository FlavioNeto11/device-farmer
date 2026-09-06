package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// After a failure that ends the attempt, every step that was not reached gets
// a row, so the job's step list is complete without the reader knowing the
// spec. The pure half — which rows, saying what — runs everywhere; the SQL
// half runs against the scratch database and proves the rows are not live.

func fourSteps() []jobspec.Step {
	return []jobspec.Step{
		{ID: "prep", Payload: jobspec.Shell{Command: "echo ready"}},
		{ID: "app", Payload: jobspec.Install{SHA256: strings.Repeat("a", 64), Reinstall: true}},
		{ID: "smoke", Payload: jobspec.Shell{Command: "am instrument -w com.acme/.Runner"}},
		{ID: "collect", Payload: jobspec.Pull{Path: "/data/local/tmp/out.log", Artifact: "out.log"}},
	}
}

func TestStepsAfterAFailureAreListedAsSkippedNamingTheFailure(t *testing.T) {
	t.Parallel()

	steps := fourSteps()
	rows := skippedAfter(steps, 1, endedByFailure)
	if len(rows) != 2 {
		t.Fatalf("skippedAfter(steps, 1, endedByFailure) = %d rows, want the two steps after the install", len(rows))
	}
	for i, row := range rows {
		want := steps[i+2]
		if row.Index != i+2 || row.ID != want.ID || row.Kind != string(want.Kind()) {
			t.Fatalf("row %d = %+v, want index %d, id %q, kind %q", i, row, i+2, want.ID, want.Kind())
		}
		reason, _ := row.Detail["reason"].(string)
		for _, part := range []string{"not run", "step 1", "app", "install"} {
			if !strings.Contains(reason, part) {
				t.Fatalf("row %d reason = %q, want it to say %q", i, reason, part)
			}
		}
		if row.Detail["failed_step"] != "app" || row.Detail["failed_step_index"] != 1 {
			t.Fatalf("row %d detail = %v, want failed_step \"app\" at index 1", i, row.Detail)
		}
	}
	// The step's own detail — what an operator would have seen had it run —
	// is still there beside the reason.
	if rows[0].Detail["command"] != "am instrument -w com.acme/.Runner" {
		t.Fatalf("the shell step's command is missing from its skipped row: %v", rows[0].Detail)
	}
	if rows[1].Detail["path"] != "/data/local/tmp/out.log" {
		t.Fatalf("the pull step's path is missing from its skipped row: %v", rows[1].Detail)
	}

	// The last step failing leaves nothing to skip, and nonsense indexes
	// produce nothing rather than a panic.
	for _, failed := range []int{3, 4, -1} {
		if got := skippedAfter(steps, failed, endedByFailure); got != nil {
			t.Fatalf("skippedAfter(steps, %d, endedByFailure) = %v, want nothing", failed, got)
		}
	}
	if got := skippedAfter(nil, 0, endedByFailure); got != nil {
		t.Fatalf("skippedAfter(nil, 0, endedByFailure) = %v, want nothing", got)
	}
}

// The rows as written: state, timestamps, detail, and — the property the
// janitor and the job_steps_live index depend on — never in a live state.
func TestSkippedRowsCompleteTheAttemptAndAreNotLive(t *testing.T) {
	pool := requireDB(t)
	f := newJobFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	r, logs := testRunner(t, func(c *Config) { c.Pool = pool })
	p := Placement{JobID: f.jobID, DeviceID: "dev", Devpath: "usb:3-1.4", LeaseID: "l", Fence: 1}
	steps := fourSteps()

	// A stale row at index 3 from an earlier run of this same attempt: it
	// must come out 'skipped' with no start, not merged with what it said.
	if _, err := pool.Exec(ctx, `
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state, started_at, error, detail)
VALUES ($1::uuid, 1, 3, 'collect', 'pull', 'running', now() - interval '1 hour', 'stale', '{"old": true}')`,
		f.jobID); err != nil {
		t.Fatalf("seeding the stale row: %v", err)
	}

	// As execute writes it: the first step ran, the second failed, the rest
	// were not reached.
	r.recordStepStart(ctx, r.log, p, 1, 0, steps[0])
	r.recordStep(ctx, r.log, p, 1, 0, steps[0], "ok", nil, nil, "", nil)
	r.recordStepStart(ctx, r.log, p, 1, 1, steps[1])
	r.recordStep(ctx, r.log, p, 1, 1, steps[1], "failed", nil, nil, "blob missing", nil)
	r.recordSkipped(ctx, r.log, p, 1, steps, 1, endedByFailure)

	type row struct {
		index         int
		id, kind      string
		state         string
		started       bool
		finished      bool
		reason        string
		failedStep    string
		failedIndex   int
		stalePreserve bool
	}
	read := func() []row {
		t.Helper()
		rows, err := pool.Query(ctx, `
SELECT step_index, step_id, kind, state, started_at IS NOT NULL, finished_at IS NOT NULL,
       COALESCE(detail->>'reason', ''), COALESCE(detail->>'failed_step', ''),
       COALESCE((detail->>'failed_step_index')::int, -1), detail ? 'old'
  FROM farm.job_steps WHERE job_id = $1::uuid ORDER BY step_index`, f.jobID)
		if err != nil {
			t.Fatalf("reading the rows: %v", err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var x row
			if err := rows.Scan(&x.index, &x.id, &x.kind, &x.state, &x.started, &x.finished,
				&x.reason, &x.failedStep, &x.failedIndex, &x.stalePreserve); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, x)
		}
		return out
	}

	check := func(rows []row) {
		t.Helper()
		if len(rows) != 4 {
			t.Fatalf("%d rows, want one per step: %+v", len(rows), rows)
		}
		wantState := []string{"ok", "failed", "skipped", "skipped"}
		for i, x := range rows {
			if x.index != i || x.id != steps[i].ID || x.kind != string(steps[i].Kind()) || x.state != wantState[i] {
				t.Fatalf("row %d = %+v, want %s %q/%s", i, x, wantState[i], steps[i].ID, steps[i].Kind())
			}
		}
		for _, x := range rows[2:] {
			if x.started {
				t.Fatalf("skipped row %d has a started_at; it never started: %+v", x.index, x)
			}
			if !x.finished {
				t.Fatalf("skipped row %d has no finished_at; a verdict was written: %+v", x.index, x)
			}
			if !strings.Contains(x.reason, "step 1 (app/install)") || x.failedStep != "app" || x.failedIndex != 1 {
				t.Fatalf("skipped row %d does not name the failure: %+v", x.index, x)
			}
			if x.stalePreserve {
				t.Fatalf("skipped row %d kept detail from an earlier run of the attempt: %+v", x.index, x)
			}
		}
	}
	check(read())

	// Not live: the predicate shared by the job_steps_live index, the
	// janitor's orphan scan and its job sweep. A skipped row matching it
	// would be swept as an orphan and shown as a step still executing.
	var live int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM farm.job_steps
 WHERE job_id = $1::uuid AND state IN ('pending','running')`, f.jobID).Scan(&live); err != nil {
		t.Fatalf("counting live rows: %v", err)
	}
	if live != 0 {
		t.Fatalf("%d row(s) of a finished attempt still look live", live)
	}

	// Writing the tail again — a resumed run of the same attempt failing at
	// the same place — is the same four rows, not eight and not an error.
	r.recordSkipped(ctx, r.log, p, 1, steps, 1, endedByFailure)
	check(read())
	if n := logs.count("could not record the steps"); n != 0 {
		t.Fatalf("recording the tail was logged as failing %d time(s)", n)
	}

	// The last step failing writes nothing and says nothing.
	before := len(read())
	r.recordSkipped(ctx, r.log, p, 1, steps, 3, endedByFailure)
	if after := len(read()); after != before {
		t.Fatalf("a failure at the last step wrote %d row(s)", after-before)
	}
}
