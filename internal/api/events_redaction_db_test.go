package api

// The renew secret must not reach the timeline (SEC-05), end to end against a
// real database.
//
// TestEventDetailWithholdsTheRenewSecret in bulk_test.go asserts on the SQL,
// which is where the exclusion is written. This asserts on the thing that
// actually matters: the bytes GET /api/v1/events puts on the wire for a real
// lease_reattached row. Removing the redaction from events_scope.go must fail
// HERE with the leaked uuid in the message, not merely fail a string match
// against a query nobody ran.
//
// It needs DATABASE_URL pointing at a MIGRATED database and skips without one,
// exactly like tenant_scope_db_test.go, whose fixture it reuses.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// reattachedRows returns the lease_reattached entries the timeline served for
// one lease, alongside the whole response re-marshalled — the wire form, which
// is what the leak was made of.
func reattachedRows(t *testing.T, body map[string]any, leaseID string) ([]map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal events response: %v", err)
	}
	var out []map[string]any
	items, _ := body["events"].([]any)
	for _, it := range items {
		e, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if e["action"] == "lease_reattached" && e["lease_id"] == leaseID {
			out = append(out, e)
		}
	}
	return out, string(raw)
}

func TestEventTimelineWithholdsHolderInstance(t *testing.T) {
	f := newScopeFixture(t)
	ss := newScopeServer(t, f)

	// The row a farm that has been running since 00009 already has. It is
	// written here in the exact shape that migration emits, because those rows
	// are the reason the fix is a redaction on READ: no migration can unwrite
	// them, and they are precisely what an incident review opens.
	const (
		priorInstance = "11111111-2222-3333-4444-555555555555"
		newInstance   = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	)
	f.exec(`
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
SELECT 'lease_reattached', l.device_id, l.slot_id, l.id, l.job_id, 'ci-bot@a',
       jsonb_build_object(
         'authorised',      'adopted',
         'fence',           l.fence,
         'holder_epoch',    l.holder_epoch + 1,
         'prior_holder',    l.holder,
         'prior_principal', 'ci-bot@a',
         'prior_instance',  $2::uuid,
         'new_holder',      'replacement-pod',
         'new_instance',    $3::uuid,
         'prior_state',     l.state)
  FROM farm.leases l WHERE l.id = $1::uuid`, f.leaseA, priorInstance, newInstance)

	// And a row written by the production path, so this test keeps covering
	// whatever farm.lease_acquire emits rather than only what it emitted when
	// the test was written.
	liveInstance := "c0ffee00-0000-4000-8000-000000000001"
	var reattached bool
	f.scan(&reattached, `
SELECT reattached FROM farm.lease_acquire($1::uuid, 'live-pod', $2::uuid, $3, 'ci-bot@a')`,
		f.jobA, liveInstance, f.tenantA)
	if !reattached {
		t.Fatal("the fixture re-attach allocated a new lease instead of re-attaching, " +
			"so no handover row was written")
	}

	// farm.events has no foreign keys — it is the forensic trail, and it
	// outlives the rows it names — so the fixture teardown would leave these
	// behind. Registered after the fixture's own cleanup, so it runs first.
	t.Cleanup(func() {
		if _, err := f.pool.Exec(f.ctx,
			`DELETE FROM farm.events WHERE lease_id = $1::uuid`, f.leaseA); err != nil {
			t.Errorf("teardown events: %v", err)
		}
	})

	// The historical row still HOLDS the secret. Without this the test could
	// pass against a timeline that is simply empty, which is the failure mode
	// every "the response does not contain X" assertion has.
	var stored string
	f.scan(&stored, `
SELECT detail::text FROM farm.events
 WHERE lease_id = $1::uuid AND detail->>'prior_instance' IS NOT NULL
 ORDER BY at, id LIMIT 1`, f.leaseA)
	if !strings.Contains(stored, priorInstance) || !strings.Contains(stored, newInstance) {
		t.Fatalf("the stored row is not the one this test is about: %s", stored)
	}

	path := "/api/v1/events?subject=lease:" + f.leaseA

	// The two callers who could read it: an operator, who is unscoped and so
	// read every tenant's, and the lease's own tenant, whose two CI shards
	// share a tenant_id and so are told apart by nothing else.
	for _, token := range []string{tokenOperator, tokenTenantA} {
		body := ss.mustGet(token, path)
		rows, wire := reattachedRows(t, body, f.leaseA)
		if len(rows) != 2 {
			t.Fatalf("%s: %d lease_reattached row(s) for the fixture lease, want 2", token, len(rows))
		}

		for _, secret := range []string{priorInstance, newInstance, liveInstance} {
			if strings.Contains(wire, secret) {
				t.Errorf("%s: the timeline served holder_instance %s — with the lease id and the "+
					"fence in the same response, that is the whole renew triple:\n%s",
					token, secret, wire)
			}
		}

		for _, e := range rows {
			var detail map[string]any
			if err := json.Unmarshal([]byte(mustRaw(t, e["detail"])), &detail); err != nil {
				t.Fatalf("%s: detail is not an object: %v", token, err)
			}
			for _, k := range []string{"prior_instance", "new_instance"} {
				if _, found := detail[k]; found {
					t.Errorf("%s: a handover row still carries %q: %v", token, k, detail)
				}
			}
			// The row must still answer "who took my lease". A redaction that
			// emptied the document would pass every check above and cost the
			// timeline the reason it exists.
			for _, k := range []string{"authorised", "prior_holder", "new_holder", "fence"} {
				if _, found := detail[k]; !found {
					t.Errorf("%s: a handover row lost %q, which names no secret: %v", token, k, detail)
				}
			}
		}
	}

	// The other tenant sees no handover at all: the redaction is on top of the
	// scoping, not instead of it.
	body := ss.mustGet(tokenTenantB, path)
	if rows, wire := reattachedRows(t, body, f.leaseA); len(rows) != 0 {
		t.Errorf("tenant B read %d handover row(s) for tenant A's lease:\n%s", len(rows), wire)
	}

	// A caller with no credential reaches nothing, so the timeline is not a way
	// around the token either.
	if code, _ := ss.get("", path); code != http.StatusUnauthorized {
		t.Errorf("GET %s without a credential = %d, want 401", path, code)
	}

	// A detail document that is not an object must not take the endpoint down.
	// `jsonb - text[]` raises 22023 on a scalar, and farm.events.detail is jsonb
	// NOT NULL with no shape constraint: internal/topo json.Marshals a Go map
	// straight into it, and a nil map marshals to `null`. One such row must
	// cost that row's keys, not every caller's timeline — this is the
	// diagnostic endpoint, and it is opened when something is already wrong.
	f.exec(`
INSERT INTO farm.events (kind, lease_id, actor, detail)
VALUES ('lease_probe', $1::uuid, 'u6', 'null'::jsonb)`, f.leaseA)
	code, scalarBody := ss.get(tokenOperator, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s with a scalar detail row = %d, want 200: %v", path, code, scalarBody)
	}
	var probes int
	for _, it := range scalarBody["events"].([]any) {
		if it.(map[string]any)["action"] == "lease_probe" {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("the scalar-detail row appears %d time(s) in the timeline, want 1", probes)
	}
}

// mustRaw re-marshals one decoded JSON value. The events response carries
// detail as an opaque document, and the assertions above want it back as bytes.
func mustRaw(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	return string(b)
}
