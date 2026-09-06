package api

// The ledger of endings, over HTTP (LEASE-14), against a real database.
//
// The point of the routes under test is that "how did this lease end" is
// answerable at 3am without psql, so the assertions are the ones a person at
// 3am depends on: the reason and the class of ender are the ones the database
// recorded, a tenant sees its own endings and never another tenant's, a
// filter's vocabulary is closed so a typo cannot read as "nothing ended", and
// the two absences — not ended yet, terminal with no ledger row — are told
// apart rather than collapsed into one 404.
//
// These need DATABASE_URL pointing at a MIGRATED database and skip without
// one. They reuse the tenant-scope fixture, whose rows carry a per-run suffix
// and are deleted afterwards.

import (
	"net/http"
	"strings"
	"testing"
)

// endLease closes one of the fixture's leases exactly as production does: one
// UPDATE that writes state and release_reason together, which is what the
// trigger in 00007_lease_events.sql fires on. Nothing here inserts into
// farm.events — if the ledger row does not appear, the test has found that.
func (f *scopeFixture) endLease(leaseID, state, reason string) {
	f.t.Helper()
	f.exec(`
UPDATE farm.leases
   SET state = $2, release_reason = $3, released_at = now()
 WHERE id = $1::uuid`, leaseID, state, reason)
	// farm.events has no foreign key to farm.leases — it is a timeline, and
	// history that vanished with the row it describes would be no history at
	// all — so the fixture's own teardown cannot reach the row this UPDATE
	// caused. Registered here, after newScopeFixture's cleanup, so it runs
	// before it: LIFO.
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(f.ctx,
			`DELETE FROM farm.events WHERE kind = 'lease_ended' AND lease_id = $1::uuid`, leaseID)
	})
}

// endings reads the list route and returns the rows indexed by lease id.
func endingsByLease(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	return objects(body["endings"], "lease_id")
}

func TestLeaseEndingsAreReadableOverHTTP(t *testing.T) {
	f := newScopeFixture(t)
	// tenant-a's lease is ended by its job; tenant-b's is taken back by a
	// human. Two different classes, so a mix-up between them cannot pass.
	f.endLease(f.leaseA, "released", "completed")
	f.endLease(f.leaseB, "released", "operator_revoked")
	ss := newScopeServer(t, f)

	t.Run("the operator sees both, with the class the database derived", func(t *testing.T) {
		body := ss.mustGet(tokenOperator, "/api/v1/leases/endings?limit=2000")
		rows := endingsByLease(t, body)
		for _, tc := range []struct{ lease, reason, class, tenant string }{
			{f.leaseA, "completed", "job", f.tenantA},
			{f.leaseB, "operator_revoked", "operator", f.tenantB},
		} {
			row := rows[tc.lease]
			if row == nil {
				t.Fatalf("lease %s has no ending; the trigger from 00007 did not write one", tc.lease)
			}
			if row["release_reason"] != tc.reason || row["ended_by"] != tc.class {
				t.Errorf("lease %s ended %v/%v, want %v/%v",
					tc.lease, row["release_reason"], row["ended_by"], tc.reason, tc.class)
			}
			if row["tenant_id"] != tc.tenant {
				t.Errorf("lease %s carries tenant %v, want %v", tc.lease, row["tenant_id"], tc.tenant)
			}
			// held_seconds is what an incident review reads first. The
			// fixture's leases were acquired seconds ago, so the only thing
			// worth asserting is that a number is there at all.
			if _, ok := row["held_seconds"].(float64); !ok {
				t.Errorf("lease %s has no held_seconds: %v", tc.lease, row["held_seconds"])
			}
			// Every key is present even when null, so a client can tell
			// "withheld or unknown" from "this API does not carry the field".
			for _, k := range []string{"ended_at", "lease_id", "device_id", "job_id", "tenant_id",
				"fence", "release_reason", "ended_by", "held_seconds", "heartbeat_age_s",
				"holder", "protected", "backfilled"} {
				if _, present := row[k]; !present {
					t.Errorf("lease %s: the ending omits %q", tc.lease, k)
				}
			}
		}
	})

	t.Run("a tenant sees its own ending and not the other's", func(t *testing.T) {
		for _, tc := range []struct{ token, mine, theirs string }{
			{tokenTenantA, f.leaseA, f.leaseB},
			{tokenTenantB, f.leaseB, f.leaseA},
		} {
			rows := endingsByLease(t, ss.mustGet(tc.token, "/api/v1/leases/endings?limit=2000"))
			if rows[tc.mine] == nil {
				t.Errorf("%s cannot see its own lease's ending", tc.token)
			}
			if rows[tc.theirs] != nil {
				t.Errorf("%s read another tenant's ending: %v", tc.token, rows[tc.theirs])
			}
		}
	})

	t.Run("?tenant= narrows an operator and is ignored for a tenant", func(t *testing.T) {
		rows := endingsByLease(t, ss.mustGet(tokenOperator, "/api/v1/leases/endings?limit=2000&tenant="+f.tenantA))
		if rows[f.leaseA] == nil || rows[f.leaseB] != nil {
			t.Errorf("?tenant=%s did not narrow the operator's view: %v", f.tenantA, rows)
		}
		// A tenant cannot widen itself by asking for somebody else.
		rows = endingsByLease(t, ss.mustGet(tokenTenantA, "/api/v1/leases/endings?limit=2000&tenant="+f.tenantB))
		if rows[f.leaseB] != nil {
			t.Errorf("a tenant read another tenant's endings through ?tenant=: %v", rows)
		}
	})

	t.Run("filters narrow, and the summaries count what came back", func(t *testing.T) {
		body := ss.mustGet(tokenOperator, "/api/v1/leases/endings?limit=2000&ended_by=operator&tenant="+f.tenantB)
		rows := endingsByLease(t, body)
		if len(rows) != 1 || rows[f.leaseB] == nil {
			t.Fatalf("?ended_by=operator returned %d rows, want just the revoked one", len(rows))
		}
		counts, _ := body["counts"].(map[string]any)
		if counts["operator"] != float64(1) {
			t.Errorf("counts = %v, want operator 1", counts)
		}
		reasons, _ := body["reasons"].(map[string]any)
		if reasons["operator_revoked"] != float64(1) {
			t.Errorf("reasons = %v, want operator_revoked 1", reasons)
		}
		holders, _ := body["holders"].(map[string]any)
		if holders[f.holderB] != float64(1) {
			t.Errorf("holders = %v, want %s 1", holders, f.holderB)
		}
		// An ending that named no reason is the failure the whole system
		// exists to prevent, so it is counted at the top level.
		if body["unaccounted"] != float64(0) {
			t.Errorf("unaccounted = %v, want 0", body["unaccounted"])
		}

		// The other filters, each reduced to the one lease it should keep.
		for _, q := range []string{
			"reason=completed&tenant=" + f.tenantA,
			"holder=" + f.holderA,
			"job=" + f.jobA,
			"device=" + f.devA,
		} {
			rows := endingsByLease(t, ss.mustGet(tokenOperator, "/api/v1/leases/endings?limit=2000&"+q))
			if len(rows) != 1 || rows[f.leaseA] == nil {
				t.Errorf("?%s returned %d rows, want just lease A", q, len(rows))
			}
		}
	})

	// A misspelt filter value must be refused. Answering it with an empty list
	// says "no lease ended that way" about a question nobody asked, and at 3am
	// that is the most expensive wrong answer this route could give.
	t.Run("a filter outside the vocabulary is a 400, not an empty ledger", func(t *testing.T) {
		for _, q := range []string{"ended_by=operator_revoked", "reason=operator", "reason=device_offline", "since=yesterday"} {
			code, body := ss.get(tokenOperator, "/api/v1/leases/endings?"+q)
			if code != http.StatusBadRequest {
				t.Errorf("?%s = %d, want 400: %v", q, code, body)
			}
		}
	})
}

func TestOneLeaseEndingTellsTheThreeAbsencesApart(t *testing.T) {
	f := newScopeFixture(t)
	f.endLease(f.leaseA, "released", "completed")
	ss := newScopeServer(t, f)

	t.Run("an ended lease answers with its ledger row", func(t *testing.T) {
		body := ss.mustGet(tokenOperator, "/api/v1/leases/"+f.leaseA+"/ending")
		if body["ended"] != true {
			t.Fatalf("ended = %v, want true: %v", body["ended"], body)
		}
		ending, _ := body["ending"].(map[string]any)
		if ending == nil || ending["release_reason"] != "completed" || ending["ended_by"] != "job" {
			t.Errorf("ending = %v, want completed/job", ending)
		}
		if body["lease_state"] != "released" {
			t.Errorf("lease_state = %v, want released", body["lease_state"])
		}
	})

	// leaseB is still held. "It has not ended" is an answer, not a 404: a tool
	// that exits non-zero while explaining an incident is one somebody stops
	// trusting during one.
	t.Run("a live lease answers 200 with no ending", func(t *testing.T) {
		code, body := ss.get(tokenOperator, "/api/v1/leases/"+f.leaseB+"/ending")
		if code != http.StatusOK || body["ended"] != false || body["ending"] != nil {
			t.Fatalf("a held lease answered %d %v", code, body)
		}
		if body["lease_state"] != "held" {
			t.Errorf("lease_state = %v, want held", body["lease_state"])
		}
		if note, _ := body["note"].(string); !strings.Contains(note, "has not ended") {
			t.Errorf("note does not say the lease is still live: %q", note)
		}
	})

	// A terminal lease with no ledger row means the state or the row moved out
	// of band, which docs/runbooks/lease-fenced.md escalates on. It is still a
	// 200: the absence IS the finding, and spelling a finding as a 404 hides
	// it behind "no such thing".
	t.Run("terminal with no ledger row is reported, not hidden", func(t *testing.T) {
		f.endLease(f.leaseB, "released", "completed")
		f.exec(`DELETE FROM farm.events WHERE kind = 'lease_ended' AND lease_id = $1::uuid`, f.leaseB)

		code, body := ss.get(tokenOperator, "/api/v1/leases/"+f.leaseB+"/ending")
		if code != http.StatusOK || body["ended"] != false {
			t.Fatalf("answered %d %v", code, body)
		}
		note, _ := body["note"].(string)
		if !strings.Contains(note, "out of band") || !strings.Contains(note, "Escalate") {
			t.Errorf("the missing ledger row was not called out: %q", note)
		}
	})

	t.Run("another tenant's lease does not exist", func(t *testing.T) {
		code, body := ss.get(tokenTenantB, "/api/v1/leases/"+f.leaseA+"/ending")
		if code != http.StatusNotFound {
			t.Fatalf("tenant B read tenant A's ending: %d %v", code, body)
		}
		// The owning tenant still reaches it.
		if got := ss.mustGet(tokenTenantA, "/api/v1/leases/"+f.leaseA+"/ending"); got["ended"] != true {
			t.Errorf("the owning tenant was refused its own ending: %v", got)
		}
	})

	t.Run("an id that is no lease at all is a 404", func(t *testing.T) {
		code, _ := ss.get(tokenOperator, "/api/v1/leases/00000000-0000-0000-0000-000000000000/ending")
		if code != http.StatusNotFound {
			t.Errorf("an unknown lease answered %d, want 404", code)
		}
		code, _ = ss.get(tokenOperator, "/api/v1/leases/not-a-uuid/ending")
		if code != http.StatusBadRequest {
			t.Errorf("a malformed id answered %d, want 400", code)
		}
	})
}
