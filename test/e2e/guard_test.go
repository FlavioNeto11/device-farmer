package e2e

// The harness's own guards.
//
// Four adversarial reviews of the first version of this package found one
// blocker and several majors, and every one of them was a defect in the TEST
// EQUIPMENT that would have been read as a defect in the farm. That is the
// worst kind of bug a harness can have: it spends somebody's afternoon on the
// wrong file. These tests hold the fixes, and they run without a database,
// because equipment that only proves itself when Postgres is up is equipment
// nobody checks.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestAResponseLargerThanTheQuotingLimitStillDecodes is the blocker, exercised
// through the harness's own read path.
//
// The cap used to sit on the READ, so an answer larger than it was decoded from
// a truncation, json.Unmarshal failed, and every accessor reported that the API
// had not returned JSON. Measured on this package's own default seed at the
// time: GET /api/v1/fleet was 15,588 bytes against a 16,384-byte cap — under
// one device row of headroom, so one extra port per hub would have started it.
//
// It goes through f.get against an httptest server rather than building an
// apiResponse by hand. The first version of this guard did the latter, and it
// PASSED with the bug reintroduced: it was testing encoding/json, not the
// harness. No database is involved, so this still runs on a bare machine.
//
// Falsify: put the limit back on the read in request()
// (`io.ReadAll(io.LimitReader(hres.Body, maxBody))`). This fails with the
// message the reviewers saw.
func TestAResponseLargerThanTheQuotingLimitStillDecodes(t *testing.T) {
	t.Parallel()

	// Shaped like a fleet listing: one big array of small objects, which is
	// exactly the response that outgrew the cap.
	devices := make([]map[string]any, 0, 4000)
	for i := 0; i < 4000; i++ {
		devices = append(devices, map[string]any{
			"device_id": "00000000-0000-0000-0000-000000000000",
			"rack_slot": "R1-U04-H1-P1",
			"health":    "healthy",
		})
	}
	body, err := json.Marshal(map[string]any{"devices": devices})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= maxBody {
		t.Fatalf("this fixture is %d bytes, which does not exceed maxBody (%d); it cannot "+
			"test what it is here to test", len(body), maxBody)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// The smallest farm that can make a request: no database, no roles, no
	// hardware. Everything f.get touches is here.
	f := &farm{t: t, apiURL: srv.URL, tokens: map[string]string{"operator": "t"}}

	res := f.get(t, "operator", "/api/v1/fleet").mustStatus(t, http.StatusOK)
	got, ok := res.value(t, "devices").([]any)
	if !ok || len(got) != len(devices) {
		t.Fatalf("the harness read %d devices out of a %d-byte answer, want %d",
			len(got), len(body), len(devices))
	}

	// And the failure message is still bounded, and says who bounded it. A
	// message that ends mid-token invites the reader to debug a response that
	// was never malformed.
	msg := res.text()
	if len(msg) > maxBody+200 {
		t.Errorf("text() returned %d bytes; the quoting limit is not being applied", len(msg))
	}
	if !strings.HasSuffix(strings.TrimSpace(msg), "elided by the test harness, not by the API)") {
		t.Errorf("text() truncated without saying who did it; it ends:\n%s",
			msg[max(0, len(msg)-160):])
	}
}

// TestTheHarnessRefusesToLetAScenarioStealItsOwnBookkeeping guards the majors
// all four reviewers converged on.
//
// farmOpts.Env is applied last and wins, which is the point of the field — but
// six names are the harness's own. Overriding them does not FAIL, it
// desynchronises: the role comes up on the address the scenario asked for and
// the harness spends its readiness timeout probing the one it remembers, then
// blames the role. DATABASE_URL is worse than confusing — it points a role at
// another scenario's database and puts a farm-wide sweep into somebody else's
// fixtures.
//
// Falsify: delete an entry from envFor's `reserved` map; this test names it.
func TestTheHarnessRefusesToLetAScenarioStealItsOwnBookkeeping(t *testing.T) {
	t.Parallel()

	// Read from the source rather than by running a farm: this has to hold on
	// a machine with no Postgres, and the guard is a list, not a behaviour.
	//
	// Scoped to the `reserved` literal, not to the whole file. The first
	// version of this test looked for "config.EnvDatabaseURL:" anywhere in
	// harness_test.go and so matched the env map's own
	// `config.EnvDatabaseURL: f.dsn` — it passed with the reservation deleted,
	// which is the exact failure it exists to catch.
	src := readSource(t, "harness_test.go")
	const marker = "reserved := map[string]string{"
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("envFor no longer has a `reserved` map. A scenario can now silently take " +
			"over the addresses, the database and the credentials the harness is watching.")
	}
	literal := src[i+len(marker):]
	if end := strings.Index(literal, "\n\t}"); end > 0 {
		literal = literal[:end]
	}

	for _, name := range []string{
		"config.EnvDatabaseURL",
		"config.EnvAPIAddr",
		"config.EnvAPIBaseURL",
		"config.EnvMetricsAddr",
		"config.EnvComponent",
		"api.EnvAuthTokens",
	} {
		if !strings.Contains(literal, name+":") {
			t.Errorf("envFor no longer reserves %s. A scenario that sets it gets a role "+
				"running perfectly on one address and a harness watching another — or, for "+
				"DATABASE_URL, a reaper sweeping another scenario's leases.", name)
		}
	}
}

// TestEverySeededHostGetsFakeHardware is the finding that would have reached
// off this machine.
//
// A seeded host with no devices used to be skipped, so farm.hosts.adb_endpoint
// kept the seeder's placeholder 127.0.0.1:5037 — the default port of a REAL adb
// daemon. A developer with a handset plugged in would have had the watchdog and
// the jobrunner dial their own hardware from a test, and the failure would have
// read as a farm bug.
//
// Falsify: put back the `if len(devs) == 0 { continue }` in startHardware.
func TestEverySeededHostGetsFakeHardware(t *testing.T) {
	t.Parallel()

	src := readSource(t, "hardware_test.go")
	start := strings.Index(src, "func (f *farm) startHardware(")
	if start < 0 {
		t.Fatal("startHardware is gone; this guard needs rewriting, not deleting")
	}
	body := src[start:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "len(devs) == 0") && strings.Contains(body, "continue") {
		t.Error("startHardware skips a host with no devices again. Its adb_endpoint then " +
			"stays at the seeder's 127.0.0.1:5037, which is a real adb daemon's port on " +
			"whatever machine runs this.")
	}
}

// TestConsistentlyExists guards the shape most of this package's assertions
// need. Nearly every claim the system makes about a lease is negative — the
// device went offline and the lease did NOT end — and a negative checked once,
// at a moment the test chose, passes on a farm where the wrong thing is about
// to happen.
func TestConsistentlyExists(t *testing.T) {
	t.Parallel()
	if !strings.Contains(readSource(t, "client_test.go"), "func (f *farm) Consistently(") {
		t.Error("Consistently is gone. Without it every negative assertion in this package " +
			"degrades to a single sample, which passes on a farm that is one second away " +
			"from violating the invariant.")
	}
}

// TestCtlKeepsTheStreamsApart guards a subtler one. internal/ctl puts warnings
// and the blast-radius block on stderr on purpose, so a listing stays
// machine-readable when it is piped into jq. CombinedOutput merged them, and a
// scenario parsing ctl's JSON would have worked until the first run in which
// ctl warned about something.
func TestCtlKeepsTheStreamsApart(t *testing.T) {
	t.Parallel()
	src := readSource(t, "harness_test.go")
	if strings.Contains(src, "cmd.CombinedOutput()") {
		t.Error("runBinary is back on CombinedOutput; ctl's stdout is no longer parseable " +
			"on any run where it writes a warning")
	}
	if !strings.Contains(src, "cmd.Stdout, cmd.Stderr = &outBuf, &errBuf") {
		t.Error("runBinary no longer gives the child separate buffers")
	}
}

// readSource reads one file of this package.
//
// These guards read the SOURCE rather than run a farm, for the same reason
// obs.TestEveryCollectorGroupIsRegistered does: they are about a list, and they
// have to hold on a machine with no Postgres. A guard that needs a database to
// prove the equipment is a guard nobody runs.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}
