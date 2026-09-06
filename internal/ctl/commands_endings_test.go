package ctl

// `ctl endings` — the one command that answers "how did this lease end".
//
// The assertions are the ones an operator at 3am depends on: the filters they
// typed actually reach the server rather than being applied to whatever came
// back, and the two findings worth waking somebody for — an ending nobody
// accounted for, and a terminal lease the ledger has no row for — are said out
// loud on stderr where a pipeline cannot swallow them.

import (
	"net/http"
	"strings"
	"testing"
)

func endingRow(lease, reason, class, holder string) map[string]any {
	return map[string]any{
		"ended_at": "2026-09-05T02:14:03Z", "lease_id": lease,
		"device_id": "d1", "slot_id": 7, "job_id": "job-" + lease,
		"tenant_id": "acme", "fence": 41, "release_reason": reason, "ended_by": class,
		"held_seconds": 3600.5, "heartbeat_age_s": 2.25, "holder": holder,
		"protected": false, "backfilled": false,
	}
}

// TestEndingsFiltersReachTheServer: every filter is the server's. Applying one
// here to a page the server already chose would turn "no reaper endings among
// the newest fifty" into "no reaper endings", which is the answer an operator
// would act on.
func TestEndingsFiltersReachTheServer(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/leases/endings", http.StatusOK, map[string]any{
		"endings":     []map[string]any{endingRow("L1", "holder_expired", "reaper", "runner-7")},
		"counts":      map[string]int{"reaper": 1},
		"reasons":     map[string]int{"holder_expired": 1},
		"holders":     map[string]int{"runner-7": 1},
		"unaccounted": 0,
		"truncated":   false,
	})

	out, errOut, err := api.run(t, "endings", "--ended-by", "reaper", "--reason", "holder_expired",
		"--since", "24h", "--holder", "runner-7", "--device", "df-aaaa", "--job", "J1", "--tenant", "acme")
	if err != nil {
		t.Fatalf("endings failed: %v\nstderr: %s", err, errOut)
	}
	q := api.requests()[0].Query
	for _, want := range []string{"ended_by=reaper", "reason=holder_expired", "since=24h",
		"holder=runner-7", "device=df-aaaa", "job=J1", "tenant=acme", "limit=50"} {
		if !strings.Contains(q, want) {
			t.Errorf("query ?%s omits %s", q, want)
		}
	}
	// The class of ender is the answer, so it is in the table, and so is the
	// vocabulary that explains what the four words mean.
	for _, want := range []string{"reaper", "holder_expired", "runner-7", "a max_runtime the user wrote down"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not mention %q:\n%s", want, out)
		}
	}

	if _, _, err := api.run(t, "endings", "--since", "yesterday"); ExitCode(err) != 2 {
		t.Fatalf("--since yesterday was accepted: %v", err)
	}
}

// TestEndingsRefusesAFilterBesideALeaseID: a lease id and a filter are two
// different questions. Answering the first and dropping the second would print
// that lease's ending whoever ended it, under a command line that says
// "--ended-by reaper" — which reads as confirmation that the reaper did.
func TestEndingsRefusesAFilterBesideALeaseID(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/leases/L1/ending", http.StatusOK, map[string]any{
		"lease_id": "L1", "ended": true, "lease_state": "released",
		"ending": endingRow("L1", "completed", "job", "runner-1"),
	})

	for _, filter := range [][]string{{"--ended-by", "reaper"}, {"--since", "24h"}, {"--limit", "10"}} {
		_, _, err := api.run(t, append([]string{"endings", "L1"}, filter...)...)
		if ExitCode(err) != 2 {
			t.Errorf("`ctl endings L1 %s` was accepted: %v", filter[0], err)
		}
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("a refused invocation still called the server %d time(s)", n)
	}
	// The id on its own still works.
	if _, _, err := api.run(t, "endings", "L1"); err != nil {
		t.Fatalf("a bare lease id was refused: %v", err)
	}
}

// TestEndingsTruncationReportsTheServersLimit: the server clamps --limit, so
// echoing back what was asked for would name a number the operator cannot
// raise past, at the moment they are trying to see further back.
func TestEndingsTruncationReportsTheServersLimit(t *testing.T) {
	api := newFakeAPI(t)
	api.reply("GET /api/v1/leases/endings", http.StatusOK, map[string]any{
		"endings":     []map[string]any{endingRow("L1", "completed", "job", "runner-1")},
		"counts":      map[string]int{"job": 1},
		"reasons":     map[string]int{"completed": 1},
		"holders":     map[string]int{"runner-1": 1},
		"unaccounted": 0,
		"limit":       2000,
		"truncated":   true,
	})

	_, errOut, err := api.run(t, "endings", "--limit", "5000")
	if err != nil {
		t.Fatalf("endings failed: %v\nstderr: %s", err, errOut)
	}
	if !strings.Contains(errOut, "limit of 2000") {
		t.Errorf("the warning did not name the limit the server applied:\n%s", errOut)
	}
	if strings.Contains(errOut, "5000") {
		t.Errorf("the warning told the operator to raise a limit that is already clamped:\n%s", errOut)
	}
}

// TestEndingsWithNoReasonIsLoud: an ending that names none of the three ways a
// lease may end is the failure this system exists to prevent, so the command
// says so rather than printing an em dash in a column.
func TestEndingsWithNoReasonIsLoud(t *testing.T) {
	api := newFakeAPI(t)
	row := endingRow("L2", "", "unaccounted", "runner-9")
	row["release_reason"] = nil
	api.reply("GET /api/v1/leases/endings", http.StatusOK, map[string]any{
		"endings":     []map[string]any{row},
		"counts":      map[string]int{"unaccounted": 1},
		"reasons":     map[string]int{},
		"holders":     map[string]int{"runner-9": 1},
		"unaccounted": 1,
		"truncated":   true,
	})

	out, errOut, err := api.run(t, "endings")
	if err != nil {
		t.Fatalf("endings failed: %v\nstderr: %s", err, errOut)
	}
	if !strings.Contains(errOut, "NO release reason recorded") || !strings.Contains(errOut, "Escalate") {
		t.Errorf("an unaccounted ending was not escalated:\n%s", errOut)
	}
	if !strings.Contains(errOut, "hit its limit") {
		t.Errorf("a truncated page was not reported:\n%s", errOut)
	}
	// A missing reason still prints as visibly missing rather than as blank.
	if !strings.Contains(out, "—") {
		t.Errorf("the absent reason was printed as an empty cell:\n%s", out)
	}
}

// TestOneEndingAnswersTheThreeCases: a lease id switches the verb to the
// single-lease route, and each of the server's three answers reaches the
// operator — including the one that is an incident.
func TestOneEndingAnswersTheThreeCases(t *testing.T) {
	t.Run("it ended", func(t *testing.T) {
		api := newFakeAPI(t)
		api.reply("GET /api/v1/leases/L1/ending", http.StatusOK, map[string]any{
			"lease_id": "L1", "ended": true, "lease_state": "released",
			"ending": endingRow("L1", "max_runtime", "deadline", "runner-3"),
		})
		out, errOut, err := api.run(t, "endings", "L1")
		if err != nil {
			t.Fatalf("endings L1 failed: %v\nstderr: %s", err, errOut)
		}
		if api.requests()[0].Path != "/api/v1/leases/L1/ending" {
			t.Fatalf("a lease id did not reach the single-lease route: %s", api.requests()[0].Path)
		}
		for _, want := range []string{"deadline", "max_runtime", "runner-3", "1h00m", "written when the lease ended"} {
			if !strings.Contains(out, want) {
				t.Errorf("the answer does not mention %q:\n%s", want, out)
			}
		}
	})

	t.Run("it has not ended", func(t *testing.T) {
		api := newFakeAPI(t)
		api.reply("GET /api/v1/leases/L2/ending", http.StatusOK, map[string]any{
			"lease_id": "L2", "ended": false, "lease_state": "suspect", "ending": nil,
			"note": "this lease has not ended: it is suspect. A suspect lease is still LIVE.",
		})
		out, errOut, err := api.run(t, "endings", "L2")
		if err != nil {
			t.Fatalf("endings L2 failed: %v\nstderr: %s", err, errOut)
		}
		if !strings.Contains(out, "suspect") || !strings.Contains(out, "still LIVE") {
			t.Errorf("a live lease was not reported as live:\n%s", out)
		}
		if errOut != "" {
			t.Errorf("a lease that simply has not ended produced a warning:\n%s", errOut)
		}
	})

	// Terminal with nothing in the ledger. Exit 0 — it is an answer — with the
	// finding on stderr, because a tool that exits non-zero while explaining an
	// outage is one somebody stops trusting during one.
	t.Run("terminal with no ledger row", func(t *testing.T) {
		api := newFakeAPI(t)
		api.reply("GET /api/v1/leases/L3/ending", http.StatusOK, map[string]any{
			"lease_id": "L3", "ended": false, "lease_state": "expired", "ending": nil,
			"note": "this lease is expired and the ledger has no row for it.",
		})
		_, errOut, err := api.run(t, "endings", "L3")
		if err != nil {
			t.Fatalf("endings L3 exited non-zero: %v\nstderr: %s", err, errOut)
		}
		if !strings.Contains(errOut, "out of band") || !strings.Contains(errOut, "lease-fenced.md") {
			t.Errorf("a terminal lease with no ledger row was not escalated:\n%s", errOut)
		}
	})
}
