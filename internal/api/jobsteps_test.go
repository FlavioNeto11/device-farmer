package api

// The job execution views: GET /api/v1/jobs/{id}/steps and /attempts (JOB-08).
//
// These two routes are the whole answer to "which step failed, and what did it
// print" without a database session, and they were shipped with nothing
// asserting them. What is covered here, in the order it matters:
//
//   - the log budget, driven directly, with the traffic shape that broke it;
//   - tenant scoping, because a job names a tenant's work and these are
//     tenant-readable reads;
//   - the ordering rule, which decides what a truncated response LOSES;
//   - the ?attempt= parser, which refuses rather than clamps;
//   - and the ordinary reads: a job's steps, a job with none, a job that is
//     not this caller's, and the per-device verdict on /attempts.
//
// The budget and the parser need no database. Everything else does, and skips
// without DATABASE_URL pointing at a migrated one, exactly as the tenant-scope
// tests do.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// ---------------------------------------------------------------------------
// The log budget, without a database
// ---------------------------------------------------------------------------

// TestLogBudgetKeepsTheErrorsWhenOutputFloodsIt is the regression the reserve
// exists for, replayed at the size it was measured at: 80 steps, 60 KB of
// output and 2 KB of error apiece. Charged from one pot in row order, the
// outputs drain the budget and most steps lose the field that says WHY they
// failed.
func TestLogBudgetKeepsTheErrorsWhenOutputFloodsIt(t *testing.T) {
	const (
		steps     = 80
		outputLen = 60 << 10
		errorLen  = 2 << 10
	)
	b := newLogBudget()

	errorsKept, outputsKept := 0, 0
	for i := 0; i < steps; i++ {
		// The handler charges the error first, and that order is part of the
		// rule: an error that queued behind its own step's output would lose
		// the reserve's protection on the row where the budget runs out.
		if b.spendError(errorLen) {
			errorsKept++
		}
		if b.spend(outputLen) {
			outputsKept++
		}
	}

	if errorsKept != steps {
		t.Errorf("%d of %d errors survived a flood of output; the reserve is not protecting them. "+
			"An operator reading this response sees which steps failed and not why.", errorsKept, steps)
	}
	// The reserve is not free: the outputs have to be the thing that is lost.
	if outputsKept >= steps {
		t.Errorf("every one of %d outputs (%d bytes each) fit in a %d byte budget; the budget is not "+
			"being charged", steps, outputLen, maxRenderedLogBytes)
	}
	if b.omitted != steps-outputsKept {
		t.Errorf("omitted = %d, want %d — logs_omitted must count every dropped field, or a missing "+
			"log reads as a silent step", b.omitted, steps-outputsKept)
	}
}

// TestLogBudgetSpendsNoMoreThanItHas walks the pots themselves: what is
// rendered never exceeds maxRenderedLogBytes, an error may overflow into the
// shared pot once its reserve is gone, and a request with no output at all
// does not strand the reserve.
func TestLogBudgetSpendsNoMoreThanItHas(t *testing.T) {
	t.Run("nothing is rendered past the ceiling", func(t *testing.T) {
		b := newLogBudget()
		const chunk = 64 << 10
		spent := 0
		for i := 0; i < 200; i++ {
			if b.spend(chunk) {
				spent += chunk
			}
			if b.spendError(chunk) {
				spent += chunk
			}
		}
		if spent > maxRenderedLogBytes {
			t.Errorf("rendered %d bytes against a %d byte budget", spent, maxRenderedLogBytes)
		}
		// writeJSON buffers the whole body before it writes a byte, so a
		// budget that is merely "close" is the OOM this cap exists to prevent.
		if spent < maxRenderedLogBytes-2*chunk {
			t.Errorf("rendered only %d of %d bytes; the budget is being wasted, and steps lose "+
				"logs there was room for", spent, maxRenderedLogBytes)
		}
	})

	t.Run("an error draws on the shared pot once its reserve is gone", func(t *testing.T) {
		b := newLogBudget()
		if !b.spendError(maxReservedErrorBytes) {
			t.Fatalf("an error the exact size of the reserve was refused")
		}
		if !b.spendError(1024) {
			t.Errorf("an error was dropped with the shared pot untouched: the reserve is a floor, "+
				"not a ceiling (shared %d)", b.shared)
		}
	})

	t.Run("a response with no output does not strand the reserve", func(t *testing.T) {
		// The failure this rules out: sizing the shared pot at the full budget
		// and the reserve on top of it, so a response that is all errors spends
		// only a quarter of what it was allowed.
		b := newLogBudget()
		if b.shared+b.reserved != maxRenderedLogBytes {
			t.Fatalf("the two pots hold %d bytes, want exactly maxRenderedLogBytes (%d)",
				b.shared+b.reserved, maxRenderedLogBytes)
		}
		spent := 0
		for i := 0; i < 4096; i++ {
			if b.spendError(1024) {
				spent += 1024
			}
		}
		if spent != maxRenderedLogBytes {
			t.Errorf("an all-error response rendered %d bytes of a %d byte budget", spent, maxRenderedLogBytes)
		}
	})

	t.Run("a field that does not fit is dropped whole", func(t *testing.T) {
		b := newLogBudget()
		b.shared = 10
		if b.spend(11) {
			t.Errorf("an 11 byte field was rendered against 10 bytes of budget")
		}
		if b.shared != 10 {
			t.Errorf("shared = %d after a refused spend, want 10: a dropped field must cost nothing, "+
				"or a later small field is charged for one it never got", b.shared)
		}
		if !b.spend(10) {
			t.Errorf("a field the exact size of the remaining budget was refused")
		}
	})
}

// ---------------------------------------------------------------------------
// ?attempt=, without a database
// ---------------------------------------------------------------------------

// TestAttemptFilterRefusesRatherThanClamps pins the one place in this file
// that deliberately does NOT behave like queryInt. Answering about attempt 4
// when somebody asked about "4a" is a lie told at the exact moment they are
// working out what went wrong.
func TestAttemptFilterRefusesRatherThanClamps(t *testing.T) {
	parse := func(raw string) (pinned *int, all, ok bool, status int) {
		target := "/api/v1/jobs/x/steps"
		if raw != "" {
			target += "?attempt=" + url.QueryEscape(raw)
		}
		rec := httptest.NewRecorder()
		p, a, o := attemptFilter(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return p, a, o, rec.Code
	}

	t.Run("accepted", func(t *testing.T) {
		for raw, want := range map[string]struct {
			pinned *int
			all    bool
		}{
			"":           {nil, false},
			"1":          {intp(1), false},
			"7":          {intp(7), false},
			"2147483647": {intp(2147483647), false},
			"all":        {nil, true},
			"ALL":        {nil, true},
			" \t 3 ":     {intp(3), false}, // queryString trims
			" all ":      {nil, true},
		} {
			pinned, all, ok, _ := parse(raw)
			if !ok {
				t.Errorf("attempt=%q was refused", raw)
				continue
			}
			if all != want.all {
				t.Errorf("attempt=%q: all = %v, want %v", raw, all, want.all)
			}
			switch {
			case want.pinned == nil && pinned != nil:
				t.Errorf("attempt=%q pinned %d, want the newest attempt", raw, *pinned)
			case want.pinned != nil && (pinned == nil || *pinned != *want.pinned):
				t.Errorf("attempt=%q pinned %s, want %d", raw, showPin(pinned), *want.pinned)
			}
		}
	})

	t.Run("refused with a 400 that says what is legal", func(t *testing.T) {
		// 3000000000 is the one that mattered: it reached the driver, failed to
		// encode into an int4 and came back as "internal error" — a typo in a
		// query string reported as a broken server.
		for _, raw := range []string{"0", "-1", "4a", "abc", "3000000000", "2147483648", "1.5", "latest"} {
			pinned, all, ok, status := parse(raw)
			if ok {
				t.Errorf("attempt=%q was accepted as pinned=%s all=%v; it was clamped to a number "+
					"nobody asked about", raw, showPin(pinned), all)
				continue
			}
			if status != http.StatusBadRequest {
				t.Errorf("attempt=%q = %d, want 400 (a bad query string is not a broken server)", raw, status)
			}
		}
	})
}

func intp(n int) *int { return &n }

// showPin renders a pinned attempt for a failure message. The value matters
// here, and %v on a *int prints an address.
func showPin(p *int) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// ---------------------------------------------------------------------------
// The routes, against a real database
// ---------------------------------------------------------------------------

// stepFixture is one host, one device, and two tenants' jobs on it. Tenant A's
// job has been attempted twice — failing on attempt 1 and running on attempt 2
// — which is the shape every interesting assertion below needs.
type stepFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context

	host, poolID     string
	tenantA, tenantB string
	hubID            int64

	devA, devB string
	// jobA has two attempts of steps; jobB belongs to the other tenant; jobBare
	// has no step and no attempt rows at all.
	jobA, jobB, jobBare string
	leaseA              string
	fenceA              int64
}

func newStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; the job step route tests need a migrated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	sfx := fmt.Sprintf("%d%06d", os.Getpid()%100000, time.Now().UnixNano()%1_000_000)
	f := &stepFixture{
		t: t, pool: pool, ctx: ctx,
		host: "u9h" + sfx, poolID: "u9p" + sfx,
		tenantA: "u9a" + sfx, tenantB: "u9b" + sfx,
	}
	t.Cleanup(f.teardown)

	f.exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	for _, tenant := range []string{f.tenantA, f.tenantB} {
		f.exec(`INSERT INTO farm.tenants (id, name) VALUES ($1, $1)`, tenant)
		f.exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $1)`, tenant)
	}
	f.exec(`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, '127.0.0.1:5037')`, f.host)
	f.scan(&f.hubID, `INSERT INTO farm.hubs (host_id, usb_path, port_count) VALUES ($1, '1-1', 16) RETURNING id`, f.host)

	f.devA = f.device(1)
	f.devB = f.device(2)

	f.jobA = f.job(f.tenantA, "running", 2)
	f.jobB = f.job(f.tenantB, "running", 1)
	f.jobBare = f.job(f.tenantA, "queued", 0)

	f.leaseA, f.fenceA = f.lease(f.devA, f.jobA)
	return f
}

func (f *stepFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatalf("fixture: %v\n%s", err, q)
	}
}

func (f *stepFixture) scan(dst any, q string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, q, args...).Scan(dst); err != nil {
		f.t.Fatalf("fixture: %v\n%s", err, q)
	}
}

func (f *stepFixture) device(n int) string {
	f.t.Helper()
	var slotID int64
	// adb_devpath is generated from usb_path, which is what makes it the
	// position the device occupies NOW rather than anything recorded on an
	// attempt.
	f.scan(&slotID, `
INSERT INTO farm.slots (host_id, hub_id, port_number, usb_path, topo_path, rack_slot)
VALUES ($1, $2, $3, $4, $5::ltree, $6)
RETURNING id`,
		f.host, f.hubID, n, fmt.Sprintf("1-1.%d", n), fmt.Sprintf("%s.p%d", f.host, n),
		fmt.Sprintf("R9-U1-H1-P%d", n))

	var id string
	f.scan(&id, `
INSERT INTO farm.devices (farm_uid, pool_id, host_id, current_slot_id, model)
VALUES ($1, $2, $3, $4, 'Step Test')
RETURNING id::text`,
		fmt.Sprintf("df-%032x", time.Now().UnixNano()+int64(n)), f.poolID, f.host, slotID)
	f.exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, last_seen_at)
VALUES ($1::uuid, $2, $3, 'device', 'healthy', now())`, id, f.host, slotID)
	return id
}

func (f *stepFixture) job(tenant, state string, attempt int) string {
	f.t.Helper()
	var id string
	f.scan(&id, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, attempt, max_attempts, started_at)
VALUES ($1, $1, $2, $3, $4, 5, now())
RETURNING id::text`, tenant, f.poolID, state, attempt)
	return id
}

func (f *stepFixture) lease(deviceID, jobID string) (string, int64) {
	f.t.Helper()
	var (
		id    string
		fence int64
	)
	if err := f.pool.QueryRow(f.ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, ttl, grace, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, j.id, j.tenant_id, j.queue_id,
       'runner-u9', gen_random_uuid(), j.ttl, j.grace, now() + j.ttl, now() + j.ttl + j.grace
  FROM farm.devices d, farm.jobs j
 WHERE d.id = $1::uuid AND j.id = $2::uuid
RETURNING id::text, fence`, deviceID, jobID).Scan(&id, &fence); err != nil {
		f.t.Fatalf("fixture lease: %v", err)
	}
	return id, fence
}

// step writes one row of farm.job_steps. Everything variable about it is
// explicit, because half these tests are about which of those fields survives
// the response budget.
type stepRow struct {
	attempt   int
	index     int
	id        string
	kind      string
	state     string
	exitCode  *int64
	output    string
	errText   string
	detail    string
	running   bool // started but not finished: duration_s is computed against now()
	finished  bool
	startedAt bool
}

func (f *stepFixture) step(jobID string, s stepRow) {
	f.t.Helper()
	kind := s.kind
	if kind == "" {
		kind = "shell"
	}
	detail := s.detail
	if detail == "" {
		detail = "{}"
	}
	var started, finished any
	if s.startedAt || s.running || s.finished {
		started = time.Now().Add(-90 * time.Second)
	}
	if s.finished {
		finished = time.Now().Add(-30 * time.Second)
	}
	f.exec(`
INSERT INTO farm.job_steps (job_id, attempt, step_index, step_id, kind, state,
                            started_at, finished_at, exit_code, output, error, detail)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10,''), nullif($11,''), $12::jsonb)`,
		jobID, s.attempt, s.index, s.id, kind, s.state, started, finished,
		s.exitCode, s.output, s.errText, detail)
}

func (f *stepFixture) attemptRow(jobID, deviceID, leaseID string, attempt int, fence int64, outcome, errText string) int64 {
	f.t.Helper()
	var id int64
	var device, lease any
	if deviceID != "" {
		device = deviceID
	}
	if leaseID != "" {
		lease = leaseID
	}
	var finished any
	if outcome != "" {
		finished = time.Now().Add(-10 * time.Second)
	}
	f.scan(&id, `
INSERT INTO farm.job_attempts (job_id, attempt, device_id, lease_id, fence,
                               started_at, finished_at, outcome, error)
VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, now() - interval '5 minutes', $6,
        nullif($7,''), nullif($8,''))
RETURNING id`, jobID, attempt, device, lease, fence, finished, outcome, errText)
	return id
}

func (f *stepFixture) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	steps := []struct {
		q    string
		args []any
	}{
		// job_steps and job_attempts cascade from farm.jobs; leases do not.
		{`DELETE FROM farm.leases WHERE tenant_id IN ($1, $2)`, []any{f.tenantA, f.tenantB}},
		{`DELETE FROM farm.jobs WHERE tenant_id IN ($1, $2)`, []any{f.tenantA, f.tenantB}},
		{`DELETE FROM farm.devices WHERE host_id = $1`, []any{f.host}},
		{`DELETE FROM farm.slots WHERE host_id = $1`, []any{f.host}},
		{`DELETE FROM farm.hosts WHERE id = $1`, []any{f.host}},
		{`DELETE FROM farm.tenants WHERE id IN ($1, $2)`, []any{f.tenantA, f.tenantB}},
		{`DELETE FROM farm.pools WHERE id = $1`, []any{f.poolID}},
	}
	for _, s := range steps {
		if _, err := f.pool.Exec(ctx, s.q, s.args...); err != nil {
			f.t.Errorf("teardown: %v\n%s", err, s.q)
		}
	}
}

// newStepServer is the real router with the job execution routes contributed
// exactly as cmd/farmd contributes them: through WithRoutes, because they are
// not in the router's own table and a test that mounted them by hand would
// prove nothing about the shipped binary.
func newStepServer(t *testing.T, f *stepFixture) *scopeServer {
	t.Helper()
	bearer := bearerFor(t,
		"op-token:operator:alice",
		"ta-token:tenant:ci-a:"+f.tenantA,
		"tb-token:tenant:ci-b:"+f.tenantB)
	s, err := New(&config.Config{}, f.pool,
		WithAuthenticator(bearer),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithRoutes(func(srv *Server, mux *http.ServeMux) {
			js, jerr := NewJobStepsAPI(srv)
			if jerr != nil {
				t.Fatalf("NewJobStepsAPI: %v", jerr)
			}
			js.Register(mux)
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &scopeServer{t: t, url: srv.URL}
}

// stepsOf indexes the rendered steps by "attempt/index" and also returns them
// in the order the server sent them, which is itself an assertion below.
func stepsOf(t *testing.T, body map[string]any) ([]map[string]any, map[string]map[string]any) {
	t.Helper()
	list, _ := body["steps"].([]any)
	ordered := make([]map[string]any, 0, len(list))
	byKey := map[string]map[string]any{}
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("a step is not an object: %#v", it)
		}
		ordered = append(ordered, m)
		byKey[fmt.Sprintf("%v/%v", m["attempt"], m["step_index"])] = m
	}
	return ordered, byKey
}

func f64(v any) float64 {
	f, _ := v.(float64)
	return f
}

func txt(v any) string {
	s, _ := v.(string)
	return s
}

// TestJobStepsRoute covers the ordinary reads of GET /jobs/{id}/steps: what
// one attempt looks like, which attempt is the default, and the two ways a
// caller is told the response is not the whole story.
func TestJobStepsRoute(t *testing.T) {
	f := newStepFixture(t)
	ss := newStepServer(t, f)

	// Attempt 1 failed at step 1 and skipped the rest; attempt 2 is running.
	exit := int64(1)
	f.step(f.jobA, stepRow{attempt: 1, index: 0, id: "install", kind: "install", state: "ok",
		output: "Success", finished: true, exitCode: new(int64)})
	f.step(f.jobA, stepRow{attempt: 1, index: 1, id: "probe", kind: "shell", state: "failed",
		output: "line one\nline two", errText: "exit status 1", exitCode: &exit, finished: true,
		detail: `{"command":"am start -n com.example/.Main"}`})
	f.step(f.jobA, stepRow{attempt: 1, index: 2, id: "collect", kind: "pull", state: "skipped"})
	f.step(f.jobA, stepRow{attempt: 2, index: 0, id: "install", kind: "install", state: "ok",
		output: "Success", finished: true, exitCode: new(int64)})
	f.step(f.jobA, stepRow{attempt: 2, index: 1, id: "probe", kind: "shell", state: "running", running: true})
	f.step(f.jobA, stepRow{attempt: 2, index: 2, id: "collect", kind: "pull", state: "pending"})

	path := "/api/v1/jobs/" + f.jobA + "/steps"

	t.Run("the default is the newest attempt that ran", func(t *testing.T) {
		body := ss.mustGet(tokenTenantA, path)
		if body["scope"] != "latest" {
			t.Errorf("scope = %v, want latest", body["scope"])
		}
		ordered, byKey := stepsOf(t, body)
		if len(ordered) != 3 {
			t.Fatalf("got %d steps, want the 3 of attempt 2: %v", len(ordered), body["steps"])
		}
		for i, s := range ordered {
			if f64(s["attempt"]) != 2 {
				t.Errorf("step %d belongs to attempt %v; ?attempt= was not applied", i, s["attempt"])
			}
			if f64(s["step_index"]) != float64(i) {
				t.Errorf("step %d rendered at step_index %v; the list is a chronology and must be "+
					"ordered by step_index", i, s["step_index"])
			}
		}
		// attempts_with_steps is how a caller learns which numbers exist
		// without guessing one to find out it is empty.
		var have []float64
		for _, a := range body["attempts_with_steps"].([]any) {
			have = append(have, a.(float64))
		}
		if len(have) != 2 || have[0] != 1 || have[1] != 2 {
			t.Errorf("attempts_with_steps = %v, want [1 2]", have)
		}
		if f64(body["attempt"]) != 2 || f64(body["max_attempts"]) != 5 || body["job_state"] != "running" {
			t.Errorf("the job header is wrong: attempt=%v max=%v state=%v",
				body["attempt"], body["max_attempts"], body["job_state"])
		}

		// A running step reports how long it HAS been running, computed by
		// Postgres against now(): a job that is wedged and a job that is
		// working look identical without it.
		running := byKey["2/1"]
		if running == nil {
			t.Fatalf("the running step is missing: %v", body["steps"])
		}
		if _, ok := running["finished_at"]; ok {
			t.Errorf("a running step reported finished_at: %v", running)
		}
		if d := f64(running["duration_s"]); d <= 0 {
			t.Errorf("duration_s = %v on a running step; it must be now() - started_at", running["duration_s"])
		}
		// A pending step has never started, so it has no duration to report.
		if pend := byKey["2/2"]; pend == nil || pend["duration_s"] != nil || pend["state"] != "pending" {
			t.Errorf("the pending step is wrong: %v", pend)
		}
		if body["truncated"] != false || f64(body["logs_omitted"]) != 0 {
			t.Errorf("a 3-step response claims to be capped: truncated=%v logs_omitted=%v",
				body["truncated"], body["logs_omitted"])
		}
	})

	t.Run("a pinned attempt carries the failure whole", func(t *testing.T) {
		body := ss.mustGet(tokenTenantA, path+"?attempt=1")
		if body["scope"] != "1" {
			t.Errorf("scope = %v, want \"1\"", body["scope"])
		}
		_, byKey := stepsOf(t, body)
		failed := byKey["1/1"]
		if failed == nil {
			t.Fatalf("the failed step is missing from attempt 1: %v", body["steps"])
		}
		// This one object is the entire requirement: which step, what it
		// printed, why it stopped, and the context the runner attached.
		if failed["state"] != "failed" || f64(failed["exit_code"]) != 1 {
			t.Errorf("state/exit_code = %v/%v, want failed/1", failed["state"], failed["exit_code"])
		}
		if txt(failed["error"]) != "exit status 1" {
			t.Errorf("error = %q, want the runner's message", failed["error"])
		}
		if !strings.Contains(txt(failed["output"]), "line two") {
			t.Errorf("output = %q; the step's log did not survive", failed["output"])
		}
		detail, _ := failed["detail"].(map[string]any)
		if detail == nil || !strings.Contains(txt(detail["command"]), "am start") {
			t.Errorf("detail = %v, want the command the runner ran", failed["detail"])
		}
		states, _ := body["states"].(map[string]any)
		if f64(states["failed"]) != 1 || f64(states["ok"]) != 1 || f64(states["skipped"]) != 1 {
			t.Errorf("states = %v, want one each of ok/failed/skipped", states)
		}
	})

	t.Run("attempt=all walks every placement, oldest first", func(t *testing.T) {
		body := ss.mustGet(tokenTenantA, path+"?attempt=all")
		if body["scope"] != "all" {
			t.Errorf("scope = %v, want all", body["scope"])
		}
		ordered, _ := stepsOf(t, body)
		if len(ordered) != 6 {
			t.Fatalf("got %d steps, want all 6", len(ordered))
		}
		last := -1.0
		for _, s := range ordered {
			a := f64(s["attempt"])
			if a < last {
				t.Fatalf("attempt %v rendered after attempt %v; ?attempt=all is read as a "+
					"chronology and must run oldest first", a, last)
			}
			last = a
		}
	})

	t.Run("an attempt that never ran is empty rather than an error", func(t *testing.T) {
		body := ss.mustGet(tokenTenantA, path+"?attempt=4")
		steps, _ := body["steps"].([]any)
		if len(steps) != 0 {
			t.Errorf("attempt 4 never ran and returned %d steps", len(steps))
		}
		// It still says which attempts DO have steps, so the caller's next
		// request is an informed one.
		if len(body["attempts_with_steps"].([]any)) != 2 {
			t.Errorf("attempts_with_steps was withheld from an empty answer: %v", body["attempts_with_steps"])
		}
	})

	t.Run("a job with no steps at all", func(t *testing.T) {
		body := ss.mustGet(tokenTenantA, "/api/v1/jobs/"+f.jobBare+"/steps")
		steps, ok := body["steps"].([]any)
		if !ok {
			// json.Marshal of a nil slice is null, and a dashboard that does
			// list.length on null throws rather than saying "no steps yet".
			t.Fatalf("steps = %v, want an empty list rather than null", body["steps"])
		}
		if len(steps) != 0 {
			t.Errorf("a job that never ran reported %d steps", len(steps))
		}
		if body["job_state"] != "queued" || f64(body["attempt"]) != 0 {
			t.Errorf("header = %v/%v, want queued/0", body["job_state"], body["attempt"])
		}
		if list, ok := body["attempts_with_steps"].([]any); !ok || len(list) != 0 {
			t.Errorf("attempts_with_steps = %v, want an empty list", body["attempts_with_steps"])
		}
	})

	t.Run("output_chars caps what is rendered and says so", func(t *testing.T) {
		long := strings.Repeat("x", 5000)
		f.step(f.jobA, stepRow{attempt: 3, index: 0, id: "chatty", state: "ok", output: long, finished: true})

		body := ss.mustGet(tokenTenantA, path+"?attempt=3&output_chars=100")
		_, byKey := stepsOf(t, body)
		s := byKey["3/0"]
		if s == nil {
			t.Fatalf("the chatty step is missing: %v", body["steps"])
		}
		if got := len(txt(s["output"])); got != 100 {
			t.Errorf("rendered %d characters against ?output_chars=100", got)
		}
		// The true size beside the cap is the whole point: a cut log must
		// never read as a complete one.
		if f64(s["output_chars"]) != 5000 || s["output_truncated"] != true {
			t.Errorf("output_chars/output_truncated = %v/%v, want 5000/true",
				s["output_chars"], s["output_truncated"])
		}

		// left() and length() count CHARACTERS. A step that printed UTF-8 and
		// fitted must not be reported as truncated because its bytes outnumber
		// its runes.
		f.step(f.jobA, stepRow{attempt: 4, index: 0, id: "utf8", state: "ok",
			output: strings.Repeat("é", 40), finished: true})
		body = ss.mustGet(tokenTenantA, path+"?attempt=4&output_chars=100")
		_, byKey = stepsOf(t, body)
		if s := byKey["4/0"]; s == nil || s["output_truncated"] == true || f64(s["output_chars"]) != 40 {
			t.Errorf("a 40-character UTF-8 log was reported as %v (chars %v); left() and length() "+
				"count characters and the truncation test must too", s["output_truncated"], s["output_chars"])
		}
	})

	t.Run("a limit that cuts the list keeps the newest attempt", func(t *testing.T) {
		// The ordering rule the query comment is about: rows arrive newest
		// attempt first so a cut loses the TAIL of the oldest attempt shown,
		// never the attempt that just failed.
		body := ss.mustGet(tokenTenantA, path+"?attempt=all&limit=2")
		if body["truncated"] != true {
			t.Errorf("a 2-row answer to a 8-step job does not say truncated: %v", body)
		}
		_, byKey := stepsOf(t, body)
		if byKey["4/0"] == nil {
			t.Errorf("the cut threw away the newest attempt and kept an older one: %v", body["steps"])
		}
	})
}

// TestJobStepsAreTenantScoped is the property that makes these routes safe to
// mount at RoleTenant at all: a job names a tenant's work, its output is that
// tenant's output, and the answer to somebody else's job id is 404 — not 403,
// which would confirm the id exists.
func TestJobStepsAreTenantScoped(t *testing.T) {
	f := newStepFixture(t)
	ss := newStepServer(t, f)

	f.step(f.jobB, stepRow{attempt: 1, index: 0, id: "secret", state: "failed",
		output: "PASSWORD=hunter2", errText: "tenant B's failure", finished: true})
	f.attemptRow(f.jobB, f.devB, "", 1, 0, "failed", "tenant B's failure")

	for _, suffix := range []string{"/steps", "/attempts"} {
		path := "/api/v1/jobs/" + f.jobB + suffix

		if code, body := ss.get(tokenTenantA, path); code != http.StatusNotFound {
			t.Errorf("tenant A GET %s = %d, want 404: %v", path, code, body)
		}
		if code, _ := ss.get(tokenTenantB, path); code != http.StatusOK {
			t.Errorf("tenant B GET its own %s = %d, want 200", path, code)
		}
		if code, _ := ss.get(tokenOperator, path); code != http.StatusOK {
			t.Errorf("operator GET %s = %d, want 200", path, code)
		}

		// Same status and same message as an id that does not exist: whether
		// another tenant's job id is real is not this caller's business.
		missing := "/api/v1/jobs/00000000-0000-4000-8000-000000000000" + suffix
		codeOther, other := ss.get(tokenTenantA, path)
		codeMissing, gone := ss.get(tokenTenantA, missing)
		if codeOther != codeMissing || fmt.Sprint(other) != fmt.Sprint(gone) {
			t.Errorf("%s distinguishes another tenant's job (%d %v) from a missing one (%d %v)",
				suffix, codeOther, other, codeMissing, gone)
		}

		// A malformed id is a client error, not a database round trip.
		if code, _ := ss.get(tokenTenantA, "/api/v1/jobs/not-a-uuid"+suffix); code != http.StatusBadRequest {
			t.Errorf("GET a non-uuid job id%s = %d, want 400", suffix, code)
		}
	}

	// And nothing tenant B printed reaches tenant A through the body.
	_, body := ss.get(tokenTenantA, "/api/v1/jobs/"+f.jobB+"/steps")
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "hunter2") {
		t.Errorf("tenant B's step output leaked into tenant A's 404: %s", raw)
	}
}

// TestJobStepsLogBudgetOverTheWire drives the budget through the handler with
// more stored log than one response may render, and asserts the property the
// budget exists to protect: the SKELETON of what happened survives whole, only
// the text is lost, and the caller is told how much.
func TestJobStepsLogBudgetOverTheWire(t *testing.T) {
	f := newStepFixture(t)
	ss := newStepServer(t, f)

	// 60 KB of output and 2 KB of error apiece: the shape that lost 52 of 80
	// error messages before the reserve existed.
	const (
		steps  = 80
		outLen = 60 << 10
		errLen = 2 << 10
	)
	output := strings.Repeat("o", outLen)
	errText := strings.Repeat("e", errLen)
	for n := 0; n < steps; n++ {
		f.step(f.jobA, stepRow{attempt: 1, index: n, id: fmt.Sprintf("s%d", n), state: "failed",
			output: output, errText: errText, finished: true})
	}

	body := ss.mustGet(tokenTenantA,
		"/api/v1/jobs/"+f.jobA+"/steps?attempt=1&output_chars="+fmt.Sprint(maxStepOutputChars))
	ordered, _ := stepsOf(t, body)

	if len(ordered) != steps {
		t.Fatalf("got %d steps, want all %d: the budget must drop LOGS, never steps", len(ordered), steps)
	}
	if f64(body["logs_omitted"]) == 0 {
		t.Fatalf("%d steps of %d KB rendered with logs_omitted = 0; nothing is being charged against "+
			"the budget", steps, outLen>>10)
	}

	var (
		errorsRendered, outputsRendered, outputsOmitted int
		renderedBytes                                   int
	)
	for _, s := range ordered {
		if e := txt(s["error"]); e != "" {
			errorsRendered++
			renderedBytes += len(e)
			if len(e) != errLen {
				t.Errorf("an error was rendered at %d of %d characters; a dropped field is dropped "+
					"WHOLE, never cut to what was left", len(e), errLen)
			}
		} else if s["error_omitted"] != true {
			t.Errorf("a step has neither an error nor error_omitted: %v", s)
		}
		if o := txt(s["output"]); o != "" {
			outputsRendered++
			renderedBytes += len(o)
		} else if s["output_omitted"] == true {
			outputsOmitted++
		}
		// Whatever the budget takes, the step itself is still identifiable.
		if s["step_id"] == "" || s["state"] != "failed" {
			t.Errorf("a step lost its identity to the budget: %v", s)
		}
		// The stored size is reported even when the text is not, so "gone"
		// and "empty" never look alike.
		if f64(s["output_chars"]) != outLen {
			t.Errorf("output_chars = %v, want %d even on a dropped log", s["output_chars"], outLen)
		}
	}

	if errorsRendered != steps {
		t.Errorf("%d of %d error messages survived; the reserve is not holding, and the response says "+
			"which steps failed without saying why", errorsRendered, steps)
	}
	if outputsOmitted == 0 {
		t.Errorf("every one of %d %d KB outputs was rendered: that is %d MB in one response, and "+
			"writeJSON buffers all of it before writing a byte", steps, outLen>>10, steps*outLen>>20)
	}
	if renderedBytes > maxRenderedLogBytes {
		t.Errorf("rendered %d bytes of log against a %d byte budget", renderedBytes, maxRenderedLogBytes)
	}
	if got := int(f64(body["logs_omitted"])); got != outputsOmitted {
		t.Errorf("logs_omitted = %d but %d outputs were dropped; the count is what tells an operator "+
			"to narrow the request", got, outputsOmitted)
	}
}

// TestJobAttemptsRoute covers the verdict the endpoint exists to deliver: four
// failures on four devices is a job problem, four on one device is a device
// problem, and farm.jobs alone cannot tell them apart.
func TestJobAttemptsRoute(t *testing.T) {
	f := newStepFixture(t)
	ss := newStepServer(t, f)

	// The same handset failed twice, then a second one succeeded.
	f.attemptRow(f.jobA, f.devA, "", 1, 11, "failed", "adb: device offline")
	f.attemptRow(f.jobA, f.devA, f.leaseA, 2, f.fenceA, "failed", "adb: device offline")
	f.attemptRow(f.jobA, f.devB, "", 3, 13, "succeeded", "")

	body := ss.mustGet(tokenTenantA, "/api/v1/jobs/"+f.jobA+"/attempts")

	list, _ := body["attempts"].([]any)
	if len(list) != 3 {
		t.Fatalf("got %d attempts, want 3: %v", len(list), body["attempts"])
	}
	// Newest first: the placement somebody is asking about is the last one.
	prev := 1e9
	for _, it := range list {
		a := f64(it.(map[string]any)["attempt"])
		if a > prev {
			t.Fatalf("attempt %v rendered after %v; the list runs newest first", a, prev)
		}
		prev = a
	}

	outcomes, _ := body["outcomes"].(map[string]any)
	if f64(outcomes["failed"]) != 2 || f64(outcomes["succeeded"]) != 1 {
		t.Errorf("outcomes = %v, want 2 failed and 1 succeeded", outcomes)
	}
	if f64(body["distinct_devices"]) != 2 {
		t.Errorf("distinct_devices = %v, want 2 — against 3 attempts, that is the whole diagnosis",
			body["distinct_devices"])
	}

	byDevice := objects(body["by_device"], "device_id")
	if len(byDevice) != 2 {
		t.Fatalf("by_device has %d rows, want 2: %v", len(byDevice), body["by_device"])
	}
	// Busiest device first, so the handset worth suspecting is at the top.
	first, _ := body["by_device"].([]any)
	if txt(first[0].(map[string]any)["device_id"]) != f.devA {
		t.Errorf("by_device leads with %v; the device with the most attempts belongs at the top",
			first[0])
	}
	if n := f64(byDevice[f.devA]["attempts"]); n != 2 {
		t.Errorf("device A was placed %v times, want 2", n)
	}
	tally, _ := byDevice[f.devA]["outcomes"].(map[string]any)
	if f64(tally["failed"]) != 2 {
		t.Errorf("device A's outcome tally = %v, want 2 failed", tally)
	}

	// The fence is the only thing that orders two placements on the same
	// handset, and where the phone sits NOW is the only position we have.
	rows := objects(body["attempts"], "lease_id")
	held := rows[f.leaseA]
	if held == nil {
		t.Fatalf("the attempt holding the fixture lease is missing: %v", body["attempts"])
	}
	if f64(held["fence"]) != float64(f.fenceA) {
		t.Errorf("fence = %v, want %d", held["fence"], f.fenceA)
	}
	if held["lease_state"] != "held" {
		t.Errorf("lease_state = %v, want held — 'completed' and 'holder_expired' are the same row "+
			"shape and opposite stories", held["lease_state"])
	}
	if !strings.HasPrefix(txt(held["current_devpath"]), "usb:") {
		t.Errorf("current_devpath = %v, want the slot the device occupies now", held["current_devpath"])
	}
	if txt(held["model"]) != "Step Test" || txt(held["farm_uid"]) == "" {
		t.Errorf("the device identity is missing from the attempt: %v", held)
	}

	t.Run("a retired handset keeps its placement", func(t *testing.T) {
		// farm.job_attempts.device_id is ON DELETE SET NULL: the placement is
		// still a fact after the phone leaves the rack. It stays in the list
		// and drops out of the per-device tally, which has nothing to key on.
		f.attemptRow(f.jobA, "", "", 4, 14, "abandoned", "the device was retired")
		body := ss.mustGet(tokenTenantA, "/api/v1/jobs/"+f.jobA+"/attempts")
		if list, _ := body["attempts"].([]any); len(list) != 4 {
			t.Errorf("got %d attempts, want 4: a placement on a retired device is still a placement", len(list))
		}
		if f64(body["distinct_devices"]) != 2 {
			t.Errorf("distinct_devices = %v after a NULL device_id, want 2", body["distinct_devices"])
		}
	})
}

// ---------------------------------------------------------------------------
// The step on the event stream
// ---------------------------------------------------------------------------

// jobDigestFields names every field the stream's job digest carries, with the
// reason it is safe to compare with ==.
//
// The two-poll test below catches a field that moves within a second or two.
// It cannot catch one that moves once a minute, and a field like that is just
// as wrong: it would still dirty every live job on a schedule nobody asked
// for. So the shape of the struct is pinned here as well. Adding a field means
// adding a line here and saying WHEN it changes — which is the moment to
// notice that "elapsed", "progress" or "last seen" is a clock in disguise.
var jobDigestFields = map[string]string{
	"JobID":     "the identity; never changes",
	"State":     "farm.jobs.state; changes on a real transition",
	"TenantID":  "assigned at submission",
	"QueueID":   "assigned at submission",
	"PoolID":    "assigned at submission",
	"Attempt":   "farm.jobs.attempt; changes on a retry",
	"StepIndex": "changes when the runner moves to another step",
	"StepID":    "the step's id from the spec; changes with the step",
	"StepKind":  "the step's kind; changes with the step",
	"StepState": "farm.job_steps.state; changes on a real transition",
}

func TestJobDigestCarriesNothingThatMovesOnItsOwn(t *testing.T) {
	rt := reflect.TypeOf(jobDigest{})
	if !rt.Comparable() {
		t.Fatalf("jobDigest is no longer comparable; the poller's diff is a map lookup and a ==")
	}
	for i := 0; i < rt.NumField(); i++ {
		fld := rt.Field(i)
		if _, known := jobDigestFields[fld.Name]; !known {
			t.Errorf("jobDigest.%s is new. Every field here is compared on every poll to decide "+
				"whether a job is dirty: add it to jobDigestFields with the answer to \"when does "+
				"this change?\", and if the answer involves a clock, it does not belong in the digest.",
				fld.Name)
		}
		switch fld.Type.Kind() {
		case reflect.String, reflect.Int, reflect.Int64, reflect.Bool:
		default:
			t.Errorf("jobDigest.%s is a %s. The digest is diffed with ==, which a slice or a map "+
				"cannot be, and a time.Time compares monotonic clocks that differ between polls.",
				fld.Name, fld.Type)
		}
	}
	for name := range jobDigestFields {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("jobDigestFields names %s, which no longer exists; remove the entry", name)
		}
	}
}

// names reports whether any job frame in events mentions this job.
//
// Scoped to one id on purpose: the poller reads the whole database, so a farm
// with anything else happening in it — a demo feeder, another test — produces
// job frames that have nothing to do with the fixture. Asserting "no job frame
// at all" would fail on somebody else's work.
func names(events []*sseEvent, jobID string) bool {
	for _, ev := range events {
		if ev.name == "job" && strings.Contains(string(ev.data), jobID) {
			return true
		}
	}
	return false
}

// pollJobs runs one poller pass and returns the digest of one job.
func pollJobs(t *testing.T, s *Server, jobID string) (streamState, jobDigest) {
	t.Helper()
	st, err := s.pollStreamState(context.Background())
	if err != nil {
		t.Fatalf("pollStreamState: %v", err)
	}
	d, ok := st.jobs[jobID]
	if !ok {
		t.Fatalf("job %s is missing from the poll; the stream cannot report a step it never read", jobID)
	}
	return st, d
}

// TestStreamCarriesTheCurrentStep is the live half of "which step is it on":
// a dashboard should not have to poll a route to answer it.
//
// The second assertion here matters more than the first. This digest is
// compared with == to decide whether a job is dirty, so anything that moves on
// its own — an elapsed time, a percentage, now() in any form — would mark every
// live job changed on every tick and turn the delta stream into a full stream
// several times a second. Two polls with nothing happening in between must
// produce the identical digest.
func TestStreamCarriesTheCurrentStep(t *testing.T) {
	f := newStepFixture(t)
	s, err := New(&config.Config{}, f.pool,
		WithAuthenticator(bearerFor(t, "op-token:operator:alice")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// jobA is on attempt 2: step 0 finished, step 1 is running.
	f.step(f.jobA, stepRow{attempt: 2, index: 0, id: "install", kind: "install", state: "ok", finished: true})
	f.step(f.jobA, stepRow{attempt: 2, index: 1, id: "probe", kind: "shell", state: "running", running: true})
	f.step(f.jobA, stepRow{attempt: 2, index: 2, id: "collect", kind: "pull", state: "pending"})

	prev, running := pollJobs(t, s, f.jobA)
	if running.StepIndex != 1 || running.StepID != "probe" || running.StepKind != "shell" || running.StepState != "running" {
		t.Fatalf("the stream reports step %d %q (%s) %q; want the running step 1 probe/shell",
			running.StepIndex, running.StepID, running.StepKind, running.StepState)
	}
	if running.Attempt != 2 {
		t.Errorf("attempt = %d, want 2: a step index means nothing without the placement it belongs to",
			running.Attempt)
	}

	t.Run("an unchanged job produces no delta", func(t *testing.T) {
		cur, again := pollJobs(t, s, f.jobA)
		if again != running {
			t.Fatalf("two polls of an unchanged job produced different digests:\n %+v\n %+v\n"+
				"Something in jobDigest moves on its own. Every live job would be marked dirty on "+
				"every tick, and each connected dashboard would refetch the job list several times "+
				"a second.", running, again)
		}
		if names(deltaEvents(prev, cur, ""), f.jobA) {
			t.Errorf("an idle poll published a delta for this job")
		}

		// And again across a second boundary. Two polls inside the same second
		// agree even when the digest carries a whole-second elapsed time —
		// which is a field that WOULD dirty every live job on every tick, just
		// more slowly. Waiting past one second is what makes the assertion mean
		// what it says.
		time.Sleep(1100 * time.Millisecond)
		later, third := pollJobs(t, s, f.jobA)
		if third != running {
			t.Fatalf("the digest of an idle job changed over %s of wall clock:\n %+v\n %+v\n"+
				"A clock in this struct marks every live job dirty on every tick and turns the "+
				"delta stream into a full stream.", "1.1s", running, third)
		}
		if names(deltaEvents(cur, later, ""), f.jobA) {
			t.Errorf("an idle poll a second later published a delta for this job")
		}
	})

	t.Run("a step transition is a delta", func(t *testing.T) {
		f.exec(`UPDATE farm.job_steps SET state = 'ok', finished_at = now()
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 1`, f.jobA)
		f.exec(`UPDATE farm.job_steps SET state = 'running', started_at = now()
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 2`, f.jobA)

		cur, moved := pollJobs(t, s, f.jobA)
		if moved.StepIndex != 2 || moved.StepID != "collect" || moved.StepState != "running" {
			t.Fatalf("after step 1 finished the stream reports step %d %q %q; want step 2 collect/running",
				moved.StepIndex, moved.StepID, moved.StepState)
		}
		var named bool
		for _, ev := range deltaEvents(prev, cur, "") {
			if ev.name == "job" && strings.Contains(string(ev.data), f.jobA) &&
				strings.Contains(string(ev.data), `"step_id":"collect"`) {
				named = true
			}
		}
		if !named {
			t.Errorf("the job moved to a new step and no job delta said so; live \"which step is it " +
				"on\" is exactly what this field is for")
		}
	})

	t.Run("a failed step outranks the skipped tail behind it", func(t *testing.T) {
		// The runner marks the rest of a failed attempt skipped, so the
		// furthest-along step is the last thing the job did NOT do. The one
		// worth naming is the one that broke.
		f.exec(`UPDATE farm.job_steps SET state = 'failed', finished_at = now(), error = 'boom'
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 1`, f.jobA)
		f.exec(`UPDATE farm.job_steps SET state = 'skipped'
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 2`, f.jobA)

		_, d := pollJobs(t, s, f.jobA)
		if d.StepIndex != 1 || d.StepState != "failed" {
			t.Errorf("the stream names step %d (%s); an operator asking which step failed is told "+
				"about one that never ran", d.StepIndex, d.StepState)
		}
	})

	t.Run("a tolerated failure does not pin the job to it", func(t *testing.T) {
		// A step with continue_on_error is recorded failed and tagged
		// detail.tolerated, and the runner carries on. If that outranked the
		// step actually executing, a 30-step job would report the failure it
		// shrugged off at step 1 for the rest of its life — motionless and red
		// while running perfectly well.
		f.exec(`UPDATE farm.job_steps SET detail = '{"tolerated":true}'::jsonb
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 1`, f.jobA)
		f.exec(`UPDATE farm.job_steps SET state = 'running', finished_at = NULL
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 2`, f.jobA)

		_, d := pollJobs(t, s, f.jobA)
		if d.StepIndex != 2 || d.StepState != "running" {
			t.Errorf("the stream names step %d (%s); the job tolerated the failure at step 1 and is "+
				"running step 2, and a dashboard reading this sees a job that has stopped moving",
				d.StepIndex, d.StepState)
		}

		// It is still nameable once it is the furthest thing that happened:
		// tolerated means "do not stop", not "never mention".
		f.exec(`UPDATE farm.job_steps SET state = 'pending', started_at = NULL
                 WHERE job_id = $1::uuid AND attempt = 2 AND step_index = 2`, f.jobA)
		if _, d := pollJobs(t, s, f.jobA); d.StepIndex != 1 {
			t.Errorf("with nothing after it, the tolerated failure is still the furthest step that "+
				"ran; the stream names step %d instead", d.StepIndex)
		}
	})

	t.Run("a job that has started no step says so rather than guessing", func(t *testing.T) {
		// -1, not 0: step 0 is a real step, and a queued job is not on it.
		_, d := pollJobs(t, s, f.jobBare)
		if d.StepIndex != -1 || d.StepID != "" || d.StepState != "" {
			t.Errorf("a queued job with no step rows reports step %d %q %q, want -1 and nothing else",
				d.StepIndex, d.StepID, d.StepState)
		}
	})

	t.Run("another tenant's step never leaves its scope", func(t *testing.T) {
		// The step travels inside jobDigest, which fullEvents already filters
		// by tenant. This pins that the new fields did not arrive with a route
		// around that filter.
		f.step(f.jobB, stepRow{attempt: 1, index: 0, id: "tenant-b-secret", state: "running", running: true})
		st, _ := pollJobs(t, s, f.jobB)
		for _, ev := range fullEvents(st, f.tenantA) {
			if ev.name == "job" && strings.Contains(string(ev.data), "tenant-b-secret") {
				t.Errorf("tenant A's snapshot names a step of tenant B's job: %s", ev.data)
			}
		}
		var seen bool
		for _, ev := range fullEvents(st, f.tenantB) {
			if ev.name == "job" && strings.Contains(string(ev.data), "tenant-b-secret") {
				seen = true
			}
		}
		if !seen {
			t.Errorf("tenant B cannot see the step of its own job")
		}
	})
}
