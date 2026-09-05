package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Exit codes
// ---------------------------------------------------------------------------

// TestExitCodeTable is the contract the package doc states. A script keys on
// these numbers, so every row here is a promise: 3 must never become 1, and 4
// must never collapse into either.
func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, 0},
		{"a plain error is failure", errors.New("boom"), 1},
		{"usage", ErrUsage, 2},
		{"usage, wrapped", fmt.Errorf("%w: missing --reason", ErrUsage), 2},
		{"refused", ErrRefused, 3},
		{"a 409 from the API is refused", &RemoteError{Status: http.StatusConflict}, 3},
		{"a 500 from the API is failure, not refusal", &RemoteError{Status: http.StatusInternalServerError}, 1},
		{"a 404 from the API is failure", &RemoteError{Status: http.StatusNotFound}, 1},
		{"partial", ErrPartial, 4},
		{"partial, wrapped with the count", fmt.Errorf("%w: 9 of 60 targets failed", ErrPartial), 4},
		{"partial wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("%w: inner", ErrPartial)), 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCode(c.err); got != c.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestBulkOutcomeIsPartialNotFailure: nine phones that errored and an API
// that could not be reached used to exit the same way. They must not: the
// first is a completed run whose rows are on the server, the second is a run
// whose state is unknown, and a script retries exactly one of them.
func TestBulkOutcomeIsPartialNotFailure(t *testing.T) {
	if err := bulkOutcome(bulkRun{Targets: 60, OK: 60}); err != nil {
		t.Fatalf("a clean run returned %v", err)
	}
	if err := bulkOutcome(bulkRun{Targets: 60, OK: 51, Skipped: 0}); err != nil {
		t.Fatalf("a run with no errors returned %v", err)
	}

	err := bulkOutcome(bulkRun{Targets: 60, OK: 51, Errors: 9})
	if err == nil {
		t.Fatal("a run with nine failed targets returned nil")
	}
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("a run with failed targets is not ErrPartial: %v", err)
	}
	if got := ExitCode(err); got != 4 {
		t.Fatalf("exit code = %d, want 4", got)
	}
	if !strings.Contains(err.Error(), "9 of 60") {
		t.Fatalf("the message does not carry the count: %q", err.Error())
	}
	// It is not a refusal either: the server did not decline anything.
	if errors.Is(err, ErrRefused) {
		t.Fatalf("a partial run reads as refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A fake control plane
// ---------------------------------------------------------------------------

// recorded is one request the fake API saw.
type recorded struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// fakeAPI is a mux with a memory. Tests assert on what ctl SENT as much as
// on what it printed: a --reason that never reaches the wire is a flag that
// audits nothing.
type fakeAPI struct {
	*httptest.Server
	mux *http.ServeMux

	mu   sync.Mutex
	seen []recorded
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{mux: http.NewServeMux()}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		if r.Body != nil {
			_, _ = body.ReadFrom(r.Body)
		}
		f.mu.Lock()
		f.seen = append(f.seen, recorded{r.Method, r.URL.Path, r.URL.RawQuery, body.String()})
		f.mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// reply registers a handler that answers status with v as JSON.
func (f *fakeAPI) reply(pattern string, status int, v any) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	})
}

// requests returns what the server saw, in order.
func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.seen...)
}

// run invokes ctl against the fake API with no terminal on stdin, which is
// what a script looks like — so a destructive command needs --yes here, the
// same as it does in CI.
func (f *fakeAPI) run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = runWith(context.Background(), nil, append(args, "--url", f.URL), &out, &errb, nil)
	return out.String(), errb.String(), err
}

// errorEnvelope is the API's error shape, so a fake 409 reads as ErrRefused
// through the real decoder.
func errorEnvelope(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

const testSHA = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

func sampleGCReport(dry bool) map[string]any {
	rep := map[string]any{
		"dry_run":           dry,
		"root":              "/var/lib/farm/blobs",
		"grace_seconds":     3600,
		"started_at":        "2026-09-05T10:00:00Z",
		"duration_ms":       1234,
		"scanned":           12,
		"scanned_bytes":     900 << 20,
		"unrecognised":      2,
		"referenced":        9,
		"within_grace":      1,
		"collectable":       2,
		"reclaimable_bytes": 300 << 20,
		"deleted":           0,
		"freed_bytes":       0,
		"adopted":           0,
		"blobs": []map[string]any{
			{"sha256": testSHA, "size_bytes": 200 << 20, "modified_at": "2026-09-01T08:00:00Z", "deleted": !dry},
			{"sha256": strings.Repeat("a", 64), "size_bytes": 100 << 20, "modified_at": "2026-09-02T08:00:00Z", "deleted": !dry},
		},
		"truncated": false,
	}
	if !dry {
		rep["deleted"] = 2
		rep["freed_bytes"] = 300 << 20
	}
	return rep
}

// ---------------------------------------------------------------------------
// ctl artifacts gc
// ---------------------------------------------------------------------------

// TestArtifactsGCDryRunIsTheDefault: the sweep's safe direction is the one a
// bare command performs. No ?apply on the wire, no reason required, no
// confirmation, and the report says nothing was removed.
func TestArtifactsGCDryRunIsTheDefault(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("POST /api/v1/artifacts/gc", http.StatusOK, sampleGCReport(true))

	out, errOut, err := api.run(t, "artifacts", "gc")
	if err != nil {
		t.Fatalf("dry run failed: %v\nstderr: %s", err, errOut)
	}

	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("a dry run made %d requests, want 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != http.MethodPost || reqs[0].Path != "/api/v1/artifacts/gc" {
		t.Fatalf("dry run sent %s %s", reqs[0].Method, reqs[0].Path)
	}
	if strings.Contains(reqs[0].Query, "apply") {
		t.Fatalf("a bare `artifacts gc` sent ?%s — the default must be the dry run", reqs[0].Query)
	}

	for _, want := range []string{
		"DRY RUN", // the mode, said out loud
		"nothing was removed",
		"/var/lib/farm/blobs", // where
		"1h00m",               // the grace, so the operator knows why a young blob was kept
		"12 blob(s), 900.0 MiB",
		"2 entries not filed",
		"9 — a row names them",
		"1 — too young",
		"2 blob(s), 300.0 MiB", // collectable, exact
		testSHA,                // the digest, whole — never clipped
		"--apply --reason",     // what to do next
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run report omits %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "freed") {
		t.Errorf("a dry run reports bytes freed:\n%s", out)
	}
}

// TestArtifactsGCApplyAsksThenSendsApplyAndReason: --apply is a preflight
// dry run, a confirmation, and then the sweep with ?apply=true and the
// reason beside it — the reason is what lands in farm.audit_log, so a run
// where it stayed on the client is an unaudited deletion.
func TestArtifactsGCApplyAsksThenSendsApplyAndReason(t *testing.T) {
	api := newFakeAPI(t)
	api.mux.HandleFunc("POST /api/v1/artifacts/gc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sampleGCReport(r.URL.Query().Get("apply") != "true"))
	})

	// No --yes and no terminal: refused before anything is sent with apply.
	_, _, err := api.run(t, "artifacts", "gc", "--apply", "--reason", "quarterly reclaim")
	if ExitCode(err) != 2 {
		t.Fatalf("apply with nobody to confirm exited %d (%v), want 2", ExitCode(err), err)
	}
	for _, r := range api.requests() {
		if strings.Contains(r.Query, "apply=true") {
			t.Fatalf("an unconfirmed apply reached the wire: %+v", r)
		}
	}

	// No reason: refused before the preflight.
	_, _, err = api.run(t, "artifacts", "gc", "--apply", "--yes")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("apply without a reason exited %d (%v), want usage naming --reason", ExitCode(err), err)
	}

	api.mu.Lock()
	api.seen = nil
	api.mu.Unlock()

	out, errOut, err := api.run(t, "artifacts", "gc", "--apply", "--yes", "--reason", "quarterly reclaim")
	if err != nil {
		t.Fatalf("apply failed: %v\nstderr: %s", err, errOut)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("apply made %d requests, want a dry-run preflight and then the sweep: %+v", len(reqs), reqs)
	}
	if strings.Contains(reqs[0].Query, "apply") {
		t.Fatalf("the preflight carried ?%s; it must be the dry run", reqs[0].Query)
	}
	if !strings.Contains(reqs[1].Query, "apply=true") {
		t.Fatalf("the sweep was sent without apply=true: ?%s", reqs[1].Query)
	}
	if !strings.Contains(reqs[1].Query, "reason=quarterly+reclaim") {
		t.Fatalf("the reason did not reach the wire: ?%s", reqs[1].Query)
	}

	for _, want := range []string{"APPLIED", "2 blob(s), 300.0 MiB freed", "DELETED", testSHA} {
		if !strings.Contains(out, want) {
			t.Errorf("apply report omits %q:\n%s", want, out)
		}
	}
	// The blast radius was printed before the question, even with --yes.
	for _, want := range []string{"About to DELETE 2 blob(s), 300.0 MiB", "reason: quarterly reclaim", "proceeding (--yes)"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the confirmation omits %q:\n%s", want, errOut)
		}
	}
}

// TestArtifactsGCProblemsArePartial: a sweep that could not read one fan-out
// directory swept the rest. That is neither success nor "the API is down",
// and it exits 4 in both output formats — the JSON caller is the one most
// likely to be reading the exit status.
func TestArtifactsGCProblemsArePartial(t *testing.T) {
	api := newFakeAPI(t)
	rep := sampleGCReport(true)
	rep["problems"] = []string{"read /var/lib/farm/blobs/7f: permission denied"}
	api.reply("POST /api/v1/artifacts/gc", http.StatusOK, rep)

	_, errOut, err := api.run(t, "artifacts", "gc")
	if got := ExitCode(err); got != 4 {
		t.Fatalf("a sweep with problems exited %d (%v), want 4", got, err)
	}
	if !strings.Contains(errOut, "permission denied") {
		t.Errorf("the problem was not shown:\n%s", errOut)
	}

	out, _, err := api.run(t, "artifacts", "gc", "-o", "json")
	if got := ExitCode(err); got != 4 {
		t.Fatalf("with -o json a sweep with problems exited %d (%v), want 4", got, err)
	}
	// The body still came through: exit 4 is the outcome, not a substitute
	// for the report.
	var decoded map[string]any
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil || decoded["scanned"] != float64(12) {
		t.Fatalf("-o json did not pass the report through: %v\n%s", jerr, out)
	}
}

// TestArtifactsGCBusyIsRefused: two sweeps cannot run at once, and the
// server's 409 is an answer — exit 3, not a failure to retry blindly.
func TestArtifactsGCBusyIsRefused(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("POST /api/v1/artifacts/gc", http.StatusConflict,
		errorEnvelope("conflict", "a blob sweep is already running"))

	_, _, err := api.run(t, "artifacts", "gc")
	if got := ExitCode(err); got != 3 {
		t.Fatalf("a busy sweep exited %d (%v), want 3", got, err)
	}
}

// ---------------------------------------------------------------------------
// ctl artifacts delete
// ---------------------------------------------------------------------------

func TestArtifactsDeleteSendsTheReasonAndSaysTheBytesStayed(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/artifacts/{sha}", http.StatusOK, map[string]any{
		"artifact": map[string]any{
			"sha256": testSHA, "kind": "apk", "name": "app-debug.apk", "size_bytes": 200 << 20,
			"package": "com.example.app", "version_code": 42, "uploaded_by": "alice",
			"created_at": "2026-09-01T08:00:00Z",
		},
		"device_count": 0, "job_count": 0, "deletable": true,
	})
	api.reply("DELETE /api/v1/artifacts/{sha}", http.StatusOK, map[string]any{
		"sha256": testSHA, "deleted": true, "blob_retained": true,
		"note": "the row was removed; the content-addressed bytes remain in the blob backend",
	})

	// A prefix is not an identity.
	_, _, err := api.run(t, "artifacts", "delete", testSHA[:12], "--yes", "--reason", "stale build")
	if ExitCode(err) != 2 {
		t.Fatalf("a 12-character prefix was accepted: %v", err)
	}
	if len(api.requests()) != 0 {
		t.Fatalf("a refused prefix reached the wire: %+v", api.requests())
	}

	out, errOut, err := api.run(t, "artifacts", "delete", testSHA, "--yes", "--reason", "stale build")
	if err != nil {
		t.Fatalf("delete failed: %v\nstderr: %s", err, errOut)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[1].Method != http.MethodDelete {
		t.Fatalf("delete made %+v, want a GET preflight and then the DELETE", reqs)
	}
	if reqs[1].Path != "/api/v1/artifacts/"+testSHA {
		t.Fatalf("DELETE went to %s", reqs[1].Path)
	}
	if !strings.Contains(reqs[1].Query, "reason=stale+build") {
		t.Fatalf("the reason did not reach the wire: ?%s", reqs[1].Query)
	}
	for _, want := range []string{"row deleted:", "yes", "blob retained:", "bytes remain"} {
		if !strings.Contains(out, want) {
			t.Errorf("delete output omits %q:\n%s", want, out)
		}
	}
	// The preflight named what was about to be forgotten.
	for _, want := range []string{"app-debug.apk", "com.example.app versionCode 42", "devices holding it: 0"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the preflight omits %q:\n%s", want, errOut)
		}
	}
}

// TestArtifactsDeleteRefusalIsExit3: a referenced artifact is one the server
// declines to forget. The answer is 3, with the server's sentence intact.
func TestArtifactsDeleteRefusalIsExit3(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/artifacts/{sha}", http.StatusOK, map[string]any{
		"artifact":     map[string]any{"sha256": testSHA, "kind": "apk", "name": "app.apk", "size_bytes": 1},
		"device_count": 60, "job_count": 2, "deletable": false,
	})
	api.reply("DELETE /api/v1/artifacts/{sha}", http.StatusConflict,
		errorEnvelope("conflict", "this artifact is still referenced by 60 device ledger row(s) and 2 job spec(s)"))

	_, errOut, err := api.run(t, "artifacts", "delete", testSHA, "--yes", "--reason", "stale build")
	if got := ExitCode(err); got != 3 {
		t.Fatalf("a refused delete exited %d (%v), want 3", got, err)
	}
	if !strings.Contains(errOut, "the server will refuse this (exit 3)") {
		t.Errorf("the preflight did not warn that the server would refuse:\n%s", errOut)
	}
}

// ---------------------------------------------------------------------------
// ctl power
// ---------------------------------------------------------------------------

func powerFleet() map[string]any {
	return map[string]any{
		"devices": []map[string]any{
			{"device_id": "d1", "farm_uid": "df-aaaa", "slot_id": 7, "rack_slot": "R1-U3-P2", "host_id": "h01",
				"hub_id": 3, "hub_path": "1-1.4", "vbus_switchable": false, "slot_state": "occupied", "health": "healthy",
				"lease": map[string]any{"id": "L1", "job_id": "J1", "tenant_id": "acme", "holder": "runner-1", "protected": false}},
			{"device_id": "d2", "farm_uid": "df-bbbb", "slot_id": 8, "rack_slot": "R1-U3-P3", "host_id": "h01",
				"hub_id": 3, "hub_path": "1-1.4", "vbus_switchable": false, "slot_state": "occupied", "health": "healthy"},
		},
		"hubs": []map[string]any{}, "counts": map[string]any{}, "truncated": false,
	}
}

// TestPowerRendersWhateverTheServerAnswers covers both replies the endpoint
// gives across its own change: the hand-off (202, state requested) and the
// synchronous outcome (200, outcome from farm.recovery_attempts). Neither
// shape is insisted on, and nothing the server says is dropped.
func TestPowerRendersWhateverTheServerAnswers(t *testing.T) {
	handoff := map[string]any{
		"attempt_id": 91, "state": "requested", "slot_id": 7, "rack_slot": "R1-U3-P2", "host_id": "h01",
		"hub_id": 3, "power_domain_id": 12, "slots_in_domain": 7, "live_leases": 1, "tier": 4,
		"tier_name": "port_power", "blast_radius": "hub", "requires_policy": "allow_port_power_cycle",
		"note": "the host agent performs the switch and closes recovery attempt 91 with its outcome.",
	}
	synchronous := map[string]any{
		"attempt_id": 92, "outcome": "recovered", "slot_id": 7, "rack_slot": "R1-U3-P2", "host_id": "h01",
		"slots_in_domain": 7, "live_leases": 1, "tier": 4, "tier_name": "port_power",
		"duration_ms": 4200, "reconnected_devices": 7, "agent": "h01-agent",
	}

	for _, c := range []struct {
		name   string
		status int
		body   map[string]any
		want   []string
	}{
		{"hand-off", http.StatusAccepted, handoff, []string{
			"attempt id:", "91", "state:", "requested", "slots in domain:", "7", "blast radius:", "hub",
			"closes recovery attempt 91"}},
		{"synchronous", http.StatusOK, synchronous, []string{
			"attempt id:", "92", "outcome:", "recovered", "duration ms:", "4200",
			// keys this package has never heard of still come out
			"reconnected devices:", "agent:", "h01-agent"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.reply("GET /api/v1/fleet", http.StatusOK, powerFleet())
			api.reply("POST /api/v1/slots/{id}/power", c.status, c.body)

			out, errOut, err := api.run(t, "power", "7", "--yes", "--reason", "port wedged")
			if err != nil {
				t.Fatalf("power failed: %v\nstderr: %s", err, errOut)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("output omits %q:\n%s", want, out)
				}
			}
			reqs := api.requests()
			last := reqs[len(reqs)-1]
			if last.Method != http.MethodPost || last.Path != "/api/v1/slots/7/power" {
				t.Fatalf("the cycle was sent as %s %s", last.Method, last.Path)
			}
			if !strings.Contains(last.Body, `"reason":"port wedged"`) {
				t.Fatalf("the reason did not reach the wire: %s", last.Body)
			}
			// The preflight said what shares the domain and who holds it.
			for _, want := range []string{"R1-U3-P2", "GANGED", "2 device(s) on it", "live lease:", "L1", "acme"} {
				if !strings.Contains(errOut, want) {
					t.Errorf("the preflight omits %q:\n%s", want, errOut)
				}
			}
		})
	}
}

// TestPowerOutcomeAndRefusal: a 409 is the refusal this endpoint exists for
// and exits 3; a 2xx whose outcome says the cycle failed is a failure of the
// action and exits 1; a non-integer slot never reaches the wire.
func TestPowerOutcomeAndRefusal(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/fleet", http.StatusOK, powerFleet())
	api.reply("POST /api/v1/slots/7/power", http.StatusConflict,
		errorEnvelope("disruption_refused", "this power cycle would disturb 7 device(s) in power domain 12"))
	api.reply("POST /api/v1/slots/8/power", http.StatusOK, map[string]any{
		"attempt_id": 93, "outcome": "failed", "refusal": nil, "slot_id": 8,
	})

	_, _, err := api.run(t, "power", "7", "--yes", "--reason", "port wedged")
	if got := ExitCode(err); got != 3 {
		t.Fatalf("a refused cycle exited %d (%v), want 3", got, err)
	}

	out, _, err := api.run(t, "power", "8", "--yes", "--reason", "port wedged")
	if got := ExitCode(err); got != 1 {
		t.Fatalf("a cycle that finished failed exited %d (%v), want 1", got, err)
	}
	if !strings.Contains(out, "outcome:") || !strings.Contains(out, "failed") {
		t.Errorf("the failed outcome was not rendered before the exit:\n%s", out)
	}

	_, _, err = api.run(t, "power", "R1-U3-P2", "--yes", "--reason", "port wedged")
	if ExitCode(err) != 2 {
		t.Fatalf("a rack position was accepted as a slot id: %v", err)
	}
	_, _, err = api.run(t, "power", "7", "--yes")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("a cycle without a reason exited %d (%v), want usage naming --reason", ExitCode(err), err)
	}
}

// ---------------------------------------------------------------------------
// ctl recovery filters
// ---------------------------------------------------------------------------

func recoveryBody(attempts ...map[string]any) map[string]any {
	return map[string]any{
		"attempts":    attempts,
		"quarantines": []map[string]any{},
		"tiers": []map[string]any{{"tier": 4, "name": "port_power", "description": "VBUS off and on",
			"blast_radius": "hub", "requires_policy": "allow_port_power_cycle", "cooldown_s": 300, "max_per_hour": 4, "enabled": true}},
	}
}

func attemptRow(id int, tier int, outcome string, hub int) map[string]any {
	return map[string]any{
		"id": id, "device_id": "d1", "farm_uid": "df-aaaa", "slot_id": 7, "rack_slot": "R1-U3-P2",
		"hub_id": hub, "host_id": "h01", "tier": tier, "tier_name": "t", "blast_radius": "hub",
		"started_at": "2026-09-05T10:00:00Z", "outcome": outcome,
	}
}

// TestRecoveryFiltersReachTheServer: the filters are the server's, and are
// sent as query parameters exactly as typed.
func TestRecoveryFiltersReachTheServer(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/recovery", http.StatusOK, recoveryBody(attemptRow(1, 4, "refused", 3)))

	_, errOut, err := api.run(t, "recovery", "--outcome", "refused", "--tier", "4", "--hub", "3", "--since", "24h", "--host", "h01")
	if err != nil {
		t.Fatalf("recovery failed: %v\nstderr: %s", err, errOut)
	}
	q := api.requests()[0].Query
	for _, want := range []string{"outcome=refused", "tier=4", "hub=3", "since=24h", "host=h01", "limit=100"} {
		if !strings.Contains(q, want) {
			t.Errorf("query ?%s omits %s", q, want)
		}
	}
	if strings.Contains(errOut, "does not filter") {
		t.Errorf("a server that honoured the filter was reported as ignoring it:\n%s", errOut)
	}

	_, _, err = api.run(t, "recovery", "--since", "yesterday")
	if ExitCode(err) != 2 {
		t.Fatalf("--since yesterday was accepted: %v", err)
	}
}

// TestRecoveryOnAServerWithoutFiltersSaysSo: a server that predates the
// filters answers with the unfiltered newest rows. Showing them as the
// filtered set would turn "no refused attempts among the last hundred" into
// "no refused attempts", so the mismatch is dropped from the table and said
// on stderr — in both formats, because the JSON view carries the server's
// own bytes.
func TestRecoveryOnAServerWithoutFiltersSaysSo(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/recovery", http.StatusOK, recoveryBody(
		attemptRow(1, 4, "refused", 3), attemptRow(2, 2, "recovered", 3), attemptRow(3, 4, "recovered", 5)))

	out, errOut, err := api.run(t, "recovery", "--outcome", "refused")
	if err != nil {
		t.Fatalf("recovery failed: %v\nstderr: %s", err, errOut)
	}
	if !strings.Contains(errOut, "does not filter recovery attempts") || !strings.Contains(errOut, "2 of the 3") {
		t.Errorf("the ignored filter was not reported:\n%s", errOut)
	}
	if strings.Count(out, "R1-U3-P2") != 1 {
		t.Errorf("the table did not keep exactly the one matching attempt:\n%s", out)
	}

	out, errOut, err = api.run(t, "recovery", "--tier", "4", "--hub", "3", "-o", "json")
	if err != nil {
		t.Fatalf("recovery -o json failed: %v", err)
	}
	if !strings.Contains(errOut, "2 of the 3") {
		t.Errorf("with -o json the ignored filter was not reported on stderr:\n%s", errOut)
	}
	var decoded struct {
		Attempts []json.RawMessage `json:"attempts"`
	}
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil || len(decoded.Attempts) != 3 {
		t.Errorf("-o json must pass the server's own bytes through unfiltered: %v\n%s", jerr, out)
	}
}

// TestArtifactsStillListsByDefault: the new verbs are sub-verbs; a bare
// `artifacts` is the listing it always was.
func TestArtifactsStillListsByDefault(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/artifacts", http.StatusOK, map[string]any{"artifacts": []map[string]any{{
		"sha256": testSHA, "kind": "apk", "name": "app.apk", "size_bytes": 1024, "created_at": "2026-09-01T08:00:00Z",
	}}})

	out, _, err := api.run(t, "artifacts")
	if err != nil {
		t.Fatalf("artifacts failed: %v", err)
	}
	if !strings.Contains(out, testSHA) {
		t.Fatalf("the listing omits the artifact:\n%s", out)
	}
	_, _, err = api.run(t, "artifacts", "prune")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "gc or delete") {
		t.Fatalf("an unknown sub-verb was not a usage error naming the real ones: %v", err)
	}
}
