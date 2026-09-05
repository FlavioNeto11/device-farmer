package e2e

// The scenario-facing clients: the HTTP API, `farmd ctl`, /metrics, and the
// one wait loop every scenario needs.
//
// Everything here reports failure the same way, and it is the reason the file
// exists: the message names the request, the status, and the body the farm
// actually sent back. A harness whose assertions fail with "unexpected status"
// makes an operator reproduce the run to find out what happened; at three in
// the morning that is the difference between a fix and a shrug.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/ctl"
)

// httpTimeout bounds one ordinary API call. It is generous because a scenario
// runs beside a control plane that is doing real work on a real database.
const httpTimeout = 30 * time.Second

// maxBody bounds what a failure message may quote. The fleet listing of a
// seeded farm is a few kilobytes; anything much larger is not evidence, it is
// scrollback.
const maxBody = 16 << 10

// apiResponse is one answer from the API, kept whole so that any assertion
// about it can quote it.
type apiResponse struct {
	// Request is "GET /api/v1/fleet as operator", for failure messages.
	Request string
	Status  int
	Body    []byte

	// JSON is the decoded body when it was a JSON object, nil otherwise. The
	// API answers objects everywhere, including for errors.
	JSON map[string]any
}

// get calls the API as one of the harness's two credentials: "operator",
// "tenant", or "" for no Authorization header at all — which is how a scenario
// asserts that a route is closed.
func (f *farm) get(t *testing.T, as, path string) apiResponse {
	t.Helper()
	return f.request(t, http.MethodGet, as, path, nil)
}

// post calls the API with a JSON body. body may be nil, a map, or any value
// json.Marshal accepts.
func (f *farm) post(t *testing.T, as, path string, body any) apiResponse {
	t.Helper()
	return f.request(t, http.MethodPost, as, path, body)
}

func (f *farm) request(t *testing.T, method, as, path string, body any) apiResponse {
	t.Helper()
	if f.apiURL == "" {
		t.Fatalf("this farm has no api role; add \"api\" to farmOpts.Roles")
	}

	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body of %s %s: %v", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}

	ctx, cancel := context.WithTimeout(t.Context(), httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, f.apiURL+path, payload)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	who := "anonymously"
	if as != "" {
		token, ok := f.tokens[as]
		if !ok {
			t.Fatalf("no %q credential; this farm has operator and tenant", as)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		who = "as " + as
	}

	res := apiResponse{Request: fmt.Sprintf("%s %s %s", method, path, who)}
	hres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", res.Request, err)
	}
	defer func() { _ = hres.Body.Close() }()

	res.Status = hres.StatusCode
	res.Body, err = io.ReadAll(io.LimitReader(hres.Body, maxBody))
	if err != nil {
		t.Fatalf("%s: reading the response: %v", res.Request, err)
	}
	var obj map[string]any
	if json.Unmarshal(res.Body, &obj) == nil {
		res.JSON = obj
	}
	return res
}

// mustStatus fails unless the status is want, quoting the body.
func (r apiResponse) mustStatus(t *testing.T, want int) apiResponse {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s = %d, want %d\nbody: %s", r.Request, r.Status, want, r.text())
	}
	return r
}

// text is the body as a printable string.
func (r apiResponse) text() string {
	s := strings.TrimSpace(string(r.Body))
	if s == "" {
		return "<empty>"
	}
	return s
}

// value walks the decoded body, e.g. res.value(t, "job", "id"). A missing key
// is a failure naming the whole path and the body, because "unexpected nil" a
// hundred lines later is not a diagnosis.
func (r apiResponse) value(t *testing.T, path ...string) any {
	t.Helper()
	if r.JSON == nil {
		t.Fatalf("%s did not answer a JSON object\nbody: %s", r.Request, r.text())
	}
	var cur any = r.JSON
	for i, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not an object, so %s cannot be read\nbody: %s",
				r.Request, strings.Join(path[:i], "."), strings.Join(path, "."), r.text())
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("%s: no %q in the answer at %s\nbody: %s",
				r.Request, key, strings.Join(path[:i+1], "."), r.text())
		}
	}
	return cur
}

// str reads a string out of the decoded body.
func (r apiResponse) str(t *testing.T, path ...string) string {
	t.Helper()
	v := r.value(t, path...)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s: %s is %T, want a string\nbody: %s",
			r.Request, strings.Join(path, "."), v, r.text())
	}
	return s
}

// ---------------------------------------------------------------------------
// The operator command line
// ---------------------------------------------------------------------------

// Ctl runs the REAL `farmd ctl` against this farm's API as the operator.
//
// It returns stdout and the exit code, and it does NOT assert the code:
// ctl's exit codes are part of its contract — 3 is "the remote refused", 4 is
// "the run completed and some targets failed" — and a harness that swallowed
// them would make the two cases scripts care most about untestable.
func (f *farm) Ctl(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return f.CtlAs(t, "operator", args...)
}

// CtlAs is Ctl with a chosen credential, for asserting what a tenant may not do.
func (f *farm) CtlAs(t *testing.T, as string, args ...string) (string, int) {
	t.Helper()
	token, ok := f.tokens[as]
	if !ok {
		t.Fatalf("no %q credential; this farm has operator and tenant", as)
	}
	if f.apiURL == "" {
		t.Fatalf("this farm has no api role; add \"api\" to farmOpts.Roles")
	}

	env := append(cleanEnv(),
		config.EnvAPIBaseURL+"="+f.apiURL,
		ctl.EnvAPIToken+"="+token,
	)
	out, code, err := runBinary(t, httpTimeout, env, append([]string{"ctl"}, args...)...)
	if err != nil {
		t.Fatalf("running farmd ctl %s: %v", strings.Join(args, " "), err)
	}
	t.Logf("ctl %s -> exit %d\n%s", strings.Join(args, " "), code, strings.TrimRight(out, "\r\n"))
	return out, code
}

// Metrics scrapes one role's own /metrics listener and returns the exposition
// text. Every role binds one on a port the harness chose, so a scenario can
// assert on the counters of a role that has no HTTP surface at all.
func (f *farm) Metrics(t *testing.T, role string) string {
	t.Helper()
	addr, ok := f.metricsAddr[role]
	if !ok {
		t.Fatalf("this farm has no %q role; it was built with %v", role, f.opts.Roles)
	}

	ctx, cancel := context.WithTimeout(t.Context(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		t.Fatalf("building the %s metrics request: %v", role, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scraping the %s role at %s: %v", role, addr, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the %s role's metrics: %v", role, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/metrics = %d, want 200\nbody: %s", addr, res.StatusCode, body)
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// Waiting
// ---------------------------------------------------------------------------

// pollInterval is how often Eventually re-asks. Fast enough that a scenario
// does not pay for it, slow enough that a condition running a query does not
// become load on the same database the farm is using.
const pollInterval = 250 * time.Millisecond

// Eventually polls cond until it returns nil, and fails with the LAST error it
// got — never with a bare "condition not met". desc completes the sentence
// "timed out waiting for ...", so write it as one: "the job to reach a
// terminal state", not "job check".
//
// It gives up early when a role that was supposed to be running has exited: a
// wait that can no longer succeed should say why now rather than in a minute.
func (f *farm) Eventually(t *testing.T, timeout time.Duration, desc string, cond func() error) {
	t.Helper()

	started := time.Now()
	deadline := started.Add(timeout)
	attempts := 0
	var last error

	for {
		attempts++
		last = cond()
		if last == nil {
			t.Logf("waited %s for %s (%d attempts)", time.Since(started).Round(time.Millisecond), desc, attempts)
			return
		}
		if dead := f.deadRole(); dead != "" {
			t.Fatalf("timed out waiting for %s: %s, so this can no longer become true.\n"+
				"last answer after %s: %v",
				desc, dead, time.Since(started).Round(time.Millisecond), last)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (%d attempts).\nlast answer: %v",
				time.Since(started).Round(time.Millisecond), desc, attempts, last)
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("the test ended while waiting for %s: %v (last answer: %v)",
				desc, t.Context().Err(), last)
		case <-time.After(pollInterval):
		}
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// SubmitJob files a job through the real HTTP API as the TENANT credential and
// returns its id.
//
// The pool, queue and tenant come from the seed, and the tenant is the one the
// tenant token is confined to — so a scenario cannot accidentally file work
// under a tenancy that does not exist and read the 400 as a scheduling
// failure. Anything else about the submission (a max_runtime, a selector, a
// pinned device) is a plain f.post to the same route.
func (f *farm) SubmitJob(t *testing.T, spec map[string]any) string {
	t.Helper()
	res := f.post(t, "tenant", "/api/v1/jobs", map[string]any{
		"pool":   f.seed.Pool,
		"queue":  f.seed.Queue,
		"tenant": f.seed.Tenant,
		"spec":   spec,
	}).mustStatus(t, http.StatusCreated)

	id := res.str(t, "job", "id")
	t.Logf("submitted job %s", id)
	return id
}
