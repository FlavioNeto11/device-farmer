package e2e

// The scenario that proves the harness is real: a farm comes up, a tenant
// files a job over HTTP, the scheduler places it on a phone, the jobrunner
// runs it there, and the lease goes back because the JOB ended.
//
// The middle of it is the part worth reading. While the job runs, two ordinary
// things happen to the farm: the handset drops off the USB bus and comes back,
// and the scheduler is restarted the way a rolling deploy restarts it. Neither
// is allowed to touch the lease, and the assertions at the end are written to
// fail loudly if either one did — one lease row, one attempt, and an ending
// farm.lease_ended_by classifies as 'job'.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

func TestFarmComesUpAndPlacesAJob(t *testing.T) {
	f := newFarm(t, farmOpts{
		// The four roles this story needs, and no others. A reaper or a
		// recovery ladder here would put a farm-wide sweep beside every
		// assertion below, and "the job succeeded" would stop meaning "the
		// job succeeded".
		Roles: []string{"api", "scheduler", "jobrunner", "watchdog"},
	})
	db := f.DB()
	ctx := t.Context()

	// -----------------------------------------------------------------
	// The control plane is up, and it is closed.
	// -----------------------------------------------------------------

	// Not a formality: the harness hands every role FARM_API_TOKENS, and an
	// api that ignored it would serve this farm — and a real one — open.
	f.get(t, "", "/api/v1/fleet").mustStatus(t, http.StatusUnauthorized)

	fleet := f.get(t, "operator", "/api/v1/fleet").mustStatus(t, http.StatusOK)
	devices, ok := fleet.value(t, "devices").([]any)
	if !ok || len(devices) != f.Seed().Devices {
		t.Fatalf("GET /api/v1/fleet listed %d devices, want the %d the seed wrote\nbody: %s",
			len(devices), f.Seed().Devices, fleet.text())
	}

	// The operator command line, against the same API and over the same
	// socket. `ctl` opens no database connection, so a ctl that works is
	// evidence the API works.
	out, _, code := f.Ctl(t, "fleet")
	if code != 0 {
		t.Fatalf("ctl fleet exited %d, want 0", code)
	}
	if !strings.Contains(out, f.Seed().Hosts[0]) {
		t.Errorf("ctl fleet did not mention host %s; it grouped by something else:\n%s",
			f.Seed().Hosts[0], out)
	}

	// -----------------------------------------------------------------
	// A tenant files work.
	// -----------------------------------------------------------------

	// The sleep is not padding. It is the window in which the farm is allowed
	// to misbehave below, and it is long enough that the misbehaviour lands
	// inside the run rather than after it. The shell step is what proves the
	// work reached a physical position: the fake hardware answers every
	// command with the devpath that ran it.
	jobID := f.SubmitJob(t, map[string]any{
		"version": 1,
		"steps": []any{
			map[string]any{
				"id": "settle", "kind": "sleep", "timeout": "60s",
				"sleep": map[string]any{"duration": "8s"},
			},
			map[string]any{
				"id": "probe", "kind": "shell", "timeout": "60s",
				"shell": map[string]any{"command": "getprop ro.build.version.sdk"},
			},
		},
	})

	// -----------------------------------------------------------------
	// The scheduler places it.
	// -----------------------------------------------------------------

	var leaseID, deviceID string
	var fence int64
	f.Eventually(t, 2*time.Minute, "the scheduler to place the job on a device", func() error {
		return db.QueryRow(ctx,
			`SELECT id::text, device_id::text, fence FROM farm.leases WHERE job_id = $1::uuid`,
			jobID).Scan(&leaseID, &deviceID, &fence)
	})
	host, devpath := f.DevicePosition(t, deviceID)
	t.Logf("job %s placed on %s at %s (lease %s, fence %d)", jobID, host, devpath, leaseID, fence)

	// The runner has to be ON the device before the farm is disturbed;
	// otherwise the disturbance lands on an idle phone and proves nothing.
	f.Eventually(t, 2*time.Minute, "the jobrunner to start a step on the device", func() error {
		var n int
		if err := db.QueryRow(ctx,
			`SELECT count(*)::int FROM farm.job_steps WHERE job_id = $1::uuid`, jobID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("farm.job_steps has no row for job %s yet", jobID)
		}
		return nil
	})

	// -----------------------------------------------------------------
	// Now the farm misbehaves, in the two most ordinary ways it can.
	// -----------------------------------------------------------------

	// 1. The handset falls off the USB bus mid-run. This is the input STF
	//    issue #663 turns into destroyed work. Here it is a fact about a
	//    socket and about nothing else: the watchdog is expected to SEE it —
	//    that is what the wait below asserts — and the lease is expected not
	//    to move.
	adb := f.ADB(t, host)
	// The precondition, so that "the watchdog saw it drop" below cannot pass
	// on a device that was already down when the job was placed on it.
	if err := expectADBState(t, f, deviceID, "device"); err != nil {
		t.Fatalf("before disturbing anything, the leased device is not attached: %v", err)
	}
	if !adb.SetState(devpath, fakeadb.StateOffline) {
		t.Fatalf("the fake hardware for %s has no device at %s", host, devpath)
	}
	f.Eventually(t, 60*time.Second, "the watchdog to observe the device drop offline", func() error {
		return expectADBState(t, f, deviceID, "offline")
	})
	if !adb.SetState(devpath, fakeadb.StateDevice) {
		t.Fatalf("the fake hardware for %s lost its device at %s", host, devpath)
	}
	f.Eventually(t, 60*time.Second, "the watchdog to observe the device come back", func() error {
		return expectADBState(t, f, deviceID, "device")
	})

	// 2. The scheduler is redeployed under the running job. A pod eviction is
	//    the most ordinary event in a Kubernetes control plane, and this
	//    binary's whole shutdown story is that it releases nothing on the way
	//    out.
	f.StopRole(t, "scheduler")
	f.StartRole(t, "scheduler")

	// -----------------------------------------------------------------
	// The job finishes anyway.
	// -----------------------------------------------------------------

	var jobState, jobError string
	f.Eventually(t, 3*time.Minute, "the job to reach a terminal state", func() error {
		if err := db.QueryRow(ctx,
			`SELECT state, COALESCE(error,'') FROM farm.jobs WHERE id = $1::uuid`,
			jobID).Scan(&jobState, &jobError); err != nil {
			return err
		}
		switch jobState {
		case "succeeded":
			return nil
		case "failed", "cancelled":
			// A terminal wrong answer will not become right, so stop here
			// rather than at the timeout three minutes from now.
			t.Fatalf("job %s ended %q: %s", jobID, jobState, jobError)
		}
		return fmt.Errorf("job is %q", jobState)
	})

	// Every step ran, in order, and the shell step's output carries the
	// devpath of the device it ran on — which is how this asserts the work
	// reached a physical POSITION rather than a serial. The seeded rack holds
	// two handsets sharing one OEM serial precisely because a serial cannot
	// carry that meaning.
	steps := readSteps(t, f, jobID)
	if len(steps) != 2 {
		t.Fatalf("farm.job_steps has %d rows for job %s, want 2:\n%s", len(steps), jobID, formatSteps(steps))
	}
	for _, s := range steps {
		if s.state != "ok" {
			t.Errorf("step %s (%s) is %q, want \"ok\": %s\nall steps:\n%s",
				s.stepID, s.kind, s.state, s.errText, formatSteps(steps))
		}
	}
	if probe := steps[1]; !strings.Contains(probe.output, devpath) {
		t.Errorf("the shell step's output does not name the device it ran on (%s); "+
			"the work may have been addressed by something other than the devpath.\noutput: %q",
			devpath, probe.output)
	}

	// One attempt. A job re-placed after the drop would have two, and the
	// second would be evidence that a connectivity event had cost the run its
	// device — which is the whole failure this system is built against.
	var attempts int
	var outcome string
	if err := db.QueryRow(ctx,
		`SELECT count(*)::int, COALESCE(max(outcome),'') FROM farm.job_attempts WHERE job_id = $1::uuid`,
		jobID).Scan(&attempts, &outcome); err != nil {
		t.Fatalf("reading farm.job_attempts: %v", err)
	}
	if attempts != 1 || outcome != "succeeded" {
		t.Errorf("farm.job_attempts has %d attempt(s) ending %q, want exactly 1 ending \"succeeded\": "+
			"the job was placed more than once, so something took its device away", attempts, outcome)
	}

	// -----------------------------------------------------------------
	// THE ASSERTION THIS WHOLE SCENARIO EXISTS FOR.
	//
	// One lease, ended by the job. farm.leases.release_reason has no
	// connectivity value at all — the CHECK in migrations/00001_core.sql
	// permits seven reasons and none of them is a socket — so the way this
	// fails is not "reason = 'device_offline'": it is a SECOND lease row for
	// the same job, or a first one that reads 'holder_expired', which
	// farm.lease_ended_by classifies as 'reaper' rather than 'job'.
	// -----------------------------------------------------------------

	f.Eventually(t, 60*time.Second, "the job to give its lease back", func() error {
		var released int
		if err := db.QueryRow(ctx,
			`SELECT count(*)::int FROM farm.leases WHERE job_id = $1::uuid AND released_at IS NOT NULL`,
			jobID).Scan(&released); err != nil {
			return err
		}
		if released == 0 {
			return fmt.Errorf("no lease of job %s has been released yet", jobID)
		}
		return nil
	})

	leases := readLeases(t, f, jobID)
	if len(leases) != 1 {
		t.Fatalf("job %s has %d lease rows, want exactly 1. A device was handed to this job "+
			"more than once:\n%s", jobID, len(leases), formatLeases(leases))
	}
	l := leases[0]
	switch {
	case l.id != leaseID:
		t.Errorf("the job ended on lease %s but was placed on %s; it changed devices mid-run", l.id, leaseID)
	case l.state != "released":
		t.Errorf("lease %s is %q, want \"released\"", l.id, l.state)
	case l.reason != "completed":
		t.Errorf("lease %s ended with release_reason %q, want \"completed\". The job succeeded, "+
			"so anything else here is some other clock ending a lease the job still owned.",
			l.id, l.reason)
	case l.endedBy != "job":
		t.Errorf("farm.lease_ended_by(%q) is %q, want \"job\"", l.reason, l.endedBy)
	}

	// -----------------------------------------------------------------
	// And the harness itself: a role with no HTTP surface of its own is
	// scrapeable on the port this fixture chose for it.
	// -----------------------------------------------------------------
	if m := f.Metrics(t, "scheduler"); !strings.Contains(m, "farm_metrics_listener_up 1") {
		t.Errorf("the scheduler's own /metrics does not report farm_metrics_listener_up 1; "+
			"the harness gave it a port it could not bind.\n%s", firstLines(m, 40))
	}
}

// ---------------------------------------------------------------------------
// Readers. They exist so the assertions above can quote whole rows: an
// operator reading a failure needs the row, not the field that differed.
// ---------------------------------------------------------------------------

func expectADBState(t *testing.T, f *farm, deviceID, want string) error {
	t.Helper()
	var got string
	if err := f.DB().QueryRow(t.Context(),
		`SELECT adb_state FROM farm.device_runtime WHERE device_id = $1::uuid`, deviceID).Scan(&got); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("farm.device_runtime.adb_state is %q, want %q", got, want)
	}
	return nil
}

type stepRow struct {
	stepID, kind, state, output, errText string
	attempt, index                       int
}

func readSteps(t *testing.T, f *farm, jobID string) []stepRow {
	t.Helper()
	rows, err := f.DB().Query(t.Context(), `
SELECT attempt, step_index, step_id, kind, state, COALESCE(output,''), COALESCE(error,'')
  FROM farm.job_steps WHERE job_id = $1::uuid ORDER BY attempt, step_index`, jobID)
	if err != nil {
		t.Fatalf("reading farm.job_steps: %v", err)
	}
	defer rows.Close()

	var out []stepRow
	for rows.Next() {
		var s stepRow
		if err := rows.Scan(&s.attempt, &s.index, &s.stepID, &s.kind, &s.state, &s.output, &s.errText); err != nil {
			t.Fatalf("scanning farm.job_steps: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading farm.job_steps: %v", err)
	}
	return out
}

func formatSteps(steps []stepRow) string {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "  attempt %d step %d %s/%s: %s %s %s\n",
			s.attempt, s.index, s.stepID, s.kind, s.state,
			strings.TrimSpace(s.output), strings.TrimSpace(s.errText))
	}
	if b.Len() == 0 {
		return "  (no rows)\n"
	}
	return b.String()
}

type leaseRow struct {
	id, state, reason, endedBy, holder string
}

func readLeases(t *testing.T, f *farm, jobID string) []leaseRow {
	t.Helper()
	rows, err := f.DB().Query(t.Context(), `
SELECT id::text, state, COALESCE(release_reason,''),
       farm.lease_ended_by(release_reason), holder
  FROM farm.leases WHERE job_id = $1::uuid ORDER BY acquired_at`, jobID)
	if err != nil {
		t.Fatalf("reading farm.leases: %v", err)
	}
	defer rows.Close()

	var out []leaseRow
	for rows.Next() {
		var l leaseRow
		if err := rows.Scan(&l.id, &l.state, &l.reason, &l.endedBy, &l.holder); err != nil {
			t.Fatalf("scanning farm.leases: %v", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading farm.leases: %v", err)
	}
	return out
}

func formatLeases(leases []leaseRow) string {
	var b strings.Builder
	for _, l := range leases {
		fmt.Fprintf(&b, "  %s state=%s reason=%q ended_by=%s holder=%s\n",
			l.id, l.state, l.reason, l.endedBy, l.holder)
	}
	if b.Len() == 0 {
		return "  (no rows)\n"
	}
	return b.String()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
