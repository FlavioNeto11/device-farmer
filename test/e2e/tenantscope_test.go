package e2e

// Tenant scope, proved by two tenants that actually hold leases (SEC-07).
//
// The rule is one sentence — read surfaces are tenant-scoped — and it has a
// precise shape that is wrong if it is missed in either direction. The fleet
// is shared infrastructure: every tenant may see that a device exists and that
// it is busy. WHOSE work is on it is not shared. So the answer to another
// tenant's lease is a MASK — the device is listed, and its lease_id, fence,
// job_id, tenant_id and holder are withheld — and never a hidden device, which
// would be a different feature and a worse one: a tenant that cannot see a
// device cannot understand why it is queueing for one.
//
// # What this adds to what already exists
//
// internal/api pins the mask three ways already: unit tests on the two structs
// and the stream digest, TestEveryTenantReadableRouteIsScoped, which fails the
// build when a tenant-readable GET is added whose handler never calls
// tenantScope, and TestTenantScopeBeyondEvents, which drives every read route
// against a real database. All three are in-process, and the last one INSERTS
// the leases it then reads back.
//
// What none of them can fail on is the deployment. That `farmd api` parses a
// FARM_API_TOKENS spec carrying three credentials and confines each to the
// tenant it names; that the leases being masked were placed by the real
// scheduler on real devices out of the real allocator, rather than written by
// the test that reads them; and that `farmd ctl` — which opens no database
// connection and can only repeat what the API told it — shows a human the same
// cut. A mask that lived only in the in-process handler and a token spec that
// dropped the tenant field would both pass every existing test and serve this
// farm open.
//
// The operator's view is the control, and it is not decoration. Without it,
// "tenant B cannot see this" is equally consistent with "nobody can see this",
// which is not scoping — it is a broken query, and it would pass every
// assertion that only compared A against B.
//
// # The roles, and the two that are missing
//
// api, scheduler and jobrunner. The omissions matter more:
//
//   - No reaper. farm.lease_mark_suspect is the reaper's sweep and nothing
//     else in the tree calls it, so with the reaper absent both leases stay
//     'held' for the whole scenario however the jobrunner's heartbeats happen
//     to land. These assertions are about a mask; a lease that changed state
//     underneath them would fail them for a reason that has nothing to do with
//     scope.
//   - No recovery ladder. The ladder climbs for hardware that is failing and
//     would never touch two healthy handsets that are busy running jobs, so
//     this file writes the recovery attempts it reads back — see
//     scopeRecordAttempt for why that is the same evidence to the filter under
//     test.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The two tenancies this scenario runs as. They are deliberately NOT the
// seeded tenant: the harness's own "tenant" credential is confined to that
// one, and a scenario about a boundary needs to own both sides of it.
const (
	scopeTenantA = "e2e-scope-a"
	scopeTenantB = "e2e-scope-b"
)

// scopeLeaseIdentity are the five keys of a /fleet lease object that say WHOSE
// the lease is. They move together — the mask is all or nothing — and every
// assertion below names the one that differed, so a regression reads as
// "lease.holder leaked to tenantB" rather than as a response mismatch.
var scopeLeaseIdentity = []string{"id", "fence", "job_id", "tenant_id", "holder"}

// scopeParty is one caller: the credential name f.get and f.CtlAs sign with,
// the farm.tenants row it is confined to, and the work it owns on the farm.
type scopeParty struct {
	cred   string
	tenant string
	queue  string

	job     string
	lease   string
	fence   int64
	device  string
	host    string
	holder  string
	attempt int64
}

// scopeCaller is one credential and what it may know of each tenant's work.
// scopeSight is one cell of that matrix.
type scopeCaller struct {
	cred string
	of   []scopeSight
}

type scopeSight struct {
	tn   *scopeParty
	sees bool
}

func TestTenantScopeSeparatesTwoTenantsHoldingLeases(t *testing.T) {
	f := newFarm(t, farmOpts{
		Roles: []string{"api", "scheduler", "jobrunner"},
		// The harness mints a credential per name and creates the farm.tenants
		// row each is scoped to, then hands all of them to every role in one
		// FARM_API_TOKENS spec — which is the deployment detail this scenario
		// exists to exercise.
		Tenants: map[string]string{"tenantA": scopeTenantA, "tenantB": scopeTenantB},
	})
	db := f.DB()
	ctx := t.Context()

	a := &scopeParty{cred: "tenantA", tenant: scopeTenantA, queue: scopeTenantA + "-q"}
	b := &scopeParty{cred: "tenantB", tenant: scopeTenantB, queue: scopeTenantB + "-q"}
	tenants := []*scopeParty{a, b}

	// The whole claim of this file, as a table: every surface below is read
	// three times over, once per credential, and asked for exactly this. It is
	// written once because it is the same claim everywhere — a surface that
	// re-spelled it could get it backwards in its own copy, and a matrix with
	// one wrong cell is a test that certifies the leak it was written against.
	callers := []scopeCaller{
		{a.cred, []scopeSight{{a, true}, {b, false}}},
		{b.cred, []scopeSight{{a, false}, {b, true}}},
		{"operator", []scopeSight{{a, true}, {b, true}}},
	}

	// -----------------------------------------------------------------
	// Two tenants, each holding a lease the scheduler gave it.
	// -----------------------------------------------------------------

	// A job's queue must belong to its tenant — POST /api/v1/jobs refuses the
	// pair otherwise — and the seeder writes one queue for one tenant. The
	// pool is shared, which is the point: both tenants compete for the same
	// hardware, which is what makes the boundary between them interesting.
	for _, tn := range tenants {
		if _, err := db.Exec(ctx,
			`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
			tn.queue, tn.tenant); err != nil {
			t.Fatalf("creating queue %s for tenant %s: %v", tn.queue, tn.tenant, err)
		}
	}

	// Checked before anything is submitted, because the alternative is a
	// two-minute wait that ends in "the scheduler placed nothing" — which
	// reads as a scheduler bug when it is really a seed with no capacity. The
	// predicate is farm.lease_acquire's own (migrations/00009_reattach_auth.sql),
	// restated whole rather than approximated: a device is placeable when it
	// and its HOST are enabled, when it is free, attached and healthy, and
	// when its slot is active and past its rearm. Dropping the host from it
	// would make this check pass on a drained farm and hand the wait back
	// exactly the misdiagnosis it exists to prevent.
	var placeable int
	if err := db.QueryRow(ctx, `
SELECT count(*)::int
  FROM farm.devices d
  JOIN farm.device_runtime r ON r.device_id = d.id
  JOIN farm.slots s          ON s.id = d.current_slot_id
  JOIN farm.hosts h          ON h.id = d.host_id
 WHERE d.pool_id = $1 AND d.admin_state = 'enabled' AND h.admin_state = 'enabled'
   AND d.current_lease_id IS NULL
   AND r.adb_state = 'device' AND r.health = 'healthy'
   AND s.state = 'active' AND s.rearm_at <= now()`, f.Seed().Pool).Scan(&placeable); err != nil {
		t.Fatalf("counting placeable devices in pool %s: %v", f.Seed().Pool, err)
	}
	if placeable < 2 {
		t.Fatalf("pool %s has %d placeable device(s) and this scenario needs 2, one per tenant. "+
			"The seed cannot hold two tenants apart if only one of them can be given a device.",
			f.Seed().Pool, placeable)
	}

	// The work is a long sleep and nothing else. Nothing about what the job
	// DOES matters here; what matters is that a lease exists, that a real
	// allocator created it, and that it is still there when every read below
	// happens. The sleep is what buys that, and the step timeout is longer
	// still so the runner never ends the step before the sleep does.
	//
	// Fifteen minutes for a scenario that runs in twenty seconds is not
	// padding: it is longer than every wait below could take TOGETHER in the
	// worst case — two minutes per placement plus a minute per stream — so a
	// slow farm makes this scenario slow and never makes it wrong. A sleep
	// merely comfortable would let the job end on a loaded machine, and the
	// closing check would then blame a lease that moved when the truth was
	// that the fixture expired underneath the reads. The farm is torn down
	// when the test returns, so nothing waits out the remainder.
	for _, tn := range tenants {
		res := f.post(t, tn.cred, "/api/v1/jobs", map[string]any{
			"pool":   f.Seed().Pool,
			"queue":  tn.queue,
			"tenant": tn.tenant,
			"spec": map[string]any{
				"version": 1,
				"steps": []any{
					map[string]any{
						"id": "hold", "kind": "sleep", "timeout": "20m",
						"sleep": map[string]any{"duration": "15m"},
					},
				},
			},
		}).mustStatus(t, http.StatusCreated)
		tn.job = res.str(t, "job", "id")
		t.Logf("%s filed job %s under tenant %s", tn.cred, tn.job, tn.tenant)
	}

	for _, tn := range tenants {
		tn.awaitPlacement(t, f)
	}

	// Two tenants on one device would make every assertion below vacuous, and
	// the partial unique index on farm.leases forbids it — so this is a check
	// that the fixture is the fixture, not a check on the farm.
	if a.device == b.device {
		t.Fatalf("both tenants were placed on device %s; there is no boundary to test", a.device)
	}

	// A device NEITHER tenant holds, so that "the operator sees more than
	// either tenant" has a witness. An implementation that answered the
	// recovery filter with everything would satisfy every assertion that only
	// compared A against B.
	//
	// Joined to farm.hosts rather than reading d.host_id straight: the column
	// is nullable (ON DELETE SET NULL), and a NULL would fail the scan below
	// with an error that blames the search rather than the row it found.
	var freeDevice, freeHost string
	if err := db.QueryRow(ctx, `
SELECT d.id::text, h.id
  FROM farm.devices d JOIN farm.hosts h ON h.id = d.host_id
 WHERE d.current_lease_id IS NULL ORDER BY d.id LIMIT 1`).Scan(&freeDevice, &freeHost); err != nil {
		t.Fatalf("finding a device neither tenant holds: %v", err)
	}

	// One recovery attempt per device, and one open quarantine, so /recovery
	// has something to scope and something to keep whole.
	for _, tn := range tenants {
		tn.attempt = scopeRecordAttempt(t, f, tn.device, tn.host)
	}
	freeAttempt := scopeRecordAttempt(t, f, freeDevice, freeHost)

	opened := f.post(t, "operator", "/api/v1/quarantines", map[string]any{
		"scope":     "device",
		"device_id": freeDevice,
		"reason":    "e2e tenant scope: a quarantine both tenants must be able to read",
	}).mustStatus(t, http.StatusCreated)
	quarantineID, ok := opened.value(t, "quarantine_id").(float64)
	if !ok {
		t.Fatalf("POST /api/v1/quarantines answered a non-numeric quarantine_id\nbody: %s", opened.text())
	}
	quarantine := int64(quarantineID)

	// -----------------------------------------------------------------
	// GET /fleet: the whole fleet to everyone, the allocation to its owner.
	// -----------------------------------------------------------------

	for _, c := range callers {
		res := f.get(t, c.cred, "/api/v1/fleet").mustStatus(t, http.StatusOK)
		devices := scopeIndex(t, res, "device_id", "devices")

		// The device is not hidden from anybody. This is the assertion that
		// separates masking from filtering, and the one a "fix" that dropped
		// the row entirely would fail.
		if len(devices) != f.Seed().Devices {
			t.Fatalf("%s sees %d devices on /fleet, want the %d the seed wrote. A tenant that "+
				"cannot see a busy device cannot tell a full farm from a small one.",
				c.cred, len(devices), f.Seed().Devices)
		}
		for _, v := range c.of {
			scopeAssertFleetLease(t, c.cred, devices[v.tn.device], v.tn, v.sees)
		}

		// Counts are taken before the mask: how many devices are busy is a
		// fact about the farm, and a tenant reading "15 free" on a farm with
		// two leases would plan against a fiction.
		counts, ok := res.value(t, "counts").(map[string]any)
		if !ok {
			t.Fatalf("%s: /fleet answered no counts object\nbody: %s", c.cred, res.text())
		}
		if counts["leased"] != float64(2) {
			t.Errorf("%s: /fleet counts.leased = %v, want 2 — the mask withholds whose the "+
				"leases are, not that they exist", c.cred, counts["leased"])
		}
	}

	// -----------------------------------------------------------------
	// GET /topology: the same cut, on the object a human can find in a rack.
	// -----------------------------------------------------------------

	for _, c := range callers {
		slots := scopeTopologySlots(t, f.get(t, c.cred, "/api/v1/topology").mustStatus(t, http.StatusOK))
		for _, v := range c.of {
			scopeAssertTopologySlot(t, c.cred, slots[v.tn.device], v.tn, v.sees)
		}
	}

	// -----------------------------------------------------------------
	// GET /hosts: "how many of MY runs would a drain wait on".
	// -----------------------------------------------------------------

	// The hardware columns are read once, from the operator, and every tenant
	// is compared against them: the counts that describe the RACK must not
	// move with the credential, only the counts that describe work.
	operatorHosts := scopeIndex(t, f.get(t, "operator", "/api/v1/hosts").mustStatus(t, http.StatusOK),
		"id", "hosts")
	for _, c := range callers {
		hosts := scopeIndex(t, f.get(t, c.cred, "/api/v1/hosts").mustStatus(t, http.StatusOK), "id", "hosts")

		// Every host, to every caller. Without this the rest of the block
		// passes on a /hosts that FILTERED — a tenant handed only the one host
		// carrying its own lease still matches the operator on every surviving
		// row and still totals one live lease, and the rule this whole file
		// defends would be broken in the one place nothing checked.
		if len(hosts) != len(f.Seed().Hosts) {
			t.Errorf("%s sees %d of the %d seeded hosts on /hosts. A tenant is told how busy "+
				"ITS work makes a host, never which hosts exist.",
				c.cred, len(hosts), len(f.Seed().Hosts))
		}

		live := 0.0
		for id, row := range hosts {
			n, ok := row["live_leases"].(float64)
			if !ok {
				t.Fatalf("%s: host %s reports live_leases %v, which is not a number", c.cred, id, row["live_leases"])
			}
			live += n
			if row["devices"] != operatorHosts[id]["devices"] || row["healthy"] != operatorHosts[id]["healthy"] {
				t.Errorf("%s: host %s reports %v devices / %v healthy where the operator sees %v / %v. "+
					"The hardware is described whole to every tenant; only the lease counts are theirs.",
					c.cred, id, row["devices"], row["healthy"],
					operatorHosts[id]["devices"], operatorHosts[id]["healthy"])
			}
		}

		// Derived from the matrix rather than restated: one live lease per
		// tenancy this caller may see.
		want := 0.0
		for _, v := range c.of {
			if v.sees {
				want++
			}
		}
		if live != want {
			t.Errorf("%s: /hosts totals %v live leases across the farm, want %v", c.cred, live, want)
		}
	}
	// And the tenant's one lease is counted on the host it is actually on,
	// which is what makes the total above a scope rather than a cap.
	for _, tn := range tenants {
		hosts := scopeIndex(t, f.get(t, tn.cred, "/api/v1/hosts").mustStatus(t, http.StatusOK), "id", "hosts")
		if hosts[tn.host]["live_leases"] != float64(1) {
			t.Errorf("%s holds a lease on host %s but /hosts reports %v live leases there",
				tn.cred, tn.host, hosts[tn.host]["live_leases"])
		}
	}

	// -----------------------------------------------------------------
	// GET /recovery: attempts follow the live lease; the ladder does not.
	// -----------------------------------------------------------------

	for _, c := range callers {
		res := f.get(t, c.cred, "/api/v1/recovery").mustStatus(t, http.StatusOK)
		attempts := scopeNumberSet(t, res, "id", "attempts")

		for _, v := range c.of {
			if attempts[v.tn.attempt] != v.sees {
				t.Errorf("%s: attempt %d on %s's device %s visible = %v, want %v. The recovery "+
					"history of a device is the history of somebody's run on it.",
					c.cred, v.tn.attempt, v.tn.cred, v.tn.device, attempts[v.tn.attempt], v.sees)
			}
		}
		// The device nobody holds. Only the operator has any business reading
		// its history, and neither tenant may claim it by proximity.
		if want := c.cred == "operator"; attempts[freeAttempt] != want {
			t.Errorf("%s: attempt %d on unleased device %s visible = %v, want %v",
				c.cred, freeAttempt, freeDevice, attempts[freeAttempt], want)
		}

		// Quarantines and the tier table are whole for everyone: they describe
		// the hardware and the ladder, and a tenant choosing where to run needs
		// both. A tenant that could not see the quarantine would keep filing
		// work for a device that will never be allocated.
		if quarantines := scopeNumberSet(t, res, "id", "quarantines"); !quarantines[quarantine] {
			t.Errorf("%s cannot see the open quarantine %d on device %s; it describes the "+
				"hardware, not a tenancy", c.cred, quarantine, freeDevice)
		}
		if tiers, ok := res.value(t, "tiers").([]any); !ok || len(tiers) == 0 {
			t.Errorf("%s: the recovery tier table came back empty. It is what the ladder WILL "+
				"try, and it names nobody's work.", c.cred)
		}
	}

	// -----------------------------------------------------------------
	// GET /leases and /jobs: filtered rather than masked, because a lease row
	// and a job row are nothing BUT the identity of somebody's work.
	// -----------------------------------------------------------------

	for _, c := range callers {
		leases := scopeIndex(t, f.get(t, c.cred, "/api/v1/leases").mustStatus(t, http.StatusOK), "id", "leases")
		jobs := scopeIndex(t, f.get(t, c.cred, "/api/v1/jobs?state=live").mustStatus(t, http.StatusOK), "id", "jobs")
		for _, v := range c.of {
			if _, ok := leases[v.tn.lease]; ok != v.sees {
				t.Errorf("%s: %s's lease %s listed on /leases = %v, want %v",
					c.cred, v.tn.cred, v.tn.lease, ok, v.sees)
			}
			if _, ok := jobs[v.tn.job]; ok != v.sees {
				t.Errorf("%s: %s's job %s listed on /jobs = %v, want %v",
					c.cred, v.tn.cred, v.tn.job, ok, v.sees)
			}
		}
	}

	// -----------------------------------------------------------------
	// The operator-only surfaces. A shell across the fleet, and the switch
	// that controls the only automatic release path, are operator output.
	// -----------------------------------------------------------------

	for _, tn := range tenants {
		for _, path := range []string{"/api/v1/reaper", "/api/v1/bulk", "/api/v1/bulk/" + tn.job} {
			// 403 and not 404: the route exists and this caller may not have
			// it. A client that cannot tell those apart retries forever.
			f.get(t, tn.cred, path).mustStatus(t, http.StatusForbidden)
		}
	}
	f.get(t, "operator", "/api/v1/reaper").mustStatus(t, http.StatusOK)
	f.get(t, "operator", "/api/v1/bulk").mustStatus(t, http.StatusOK)

	// -----------------------------------------------------------------
	// GET /stream: one poller, rendered per client.
	// -----------------------------------------------------------------
	//
	// This is the subtlest part of the feature. Every other surface runs its
	// own query per request, so a missing scope shows up as a wrong row. The
	// stream reads the database ONCE and renders that single state for every
	// connected client in the client's own scope, which means a mask applied
	// to the shared state rather than to a copy — or a snapshot cached across
	// scopes — leaks here and nowhere else. It is affordable inside the
	// budget because subscribing kicks the poller, so the first frame arrives
	// in about the time one poll takes rather than at the next tick.

	for _, c := range callers {
		snap := scopeSnapshot(t, f, c.cred, "fleet", "lease", "job")

		leases := scopeObjectSet(t, c.cred, snap["lease"], "leases", "lease_id")
		jobs := scopeObjectSet(t, c.cred, snap["job"], "jobs", "job_id")
		devices := scopeObjectIndex(t, c.cred, snap["fleet"], "devices", "device_id")

		for _, v := range c.of {
			if leases[v.tn.lease] != v.sees {
				t.Errorf("%s: %s's lease %s in the stream snapshot = %v, want %v",
					c.cred, v.tn.cred, v.tn.lease, leases[v.tn.lease], v.sees)
			}
			if jobs[v.tn.job] != v.sees {
				t.Errorf("%s: %s's job %s in the stream snapshot = %v, want %v",
					c.cred, v.tn.cred, v.tn.job, jobs[v.tn.job], v.sees)
			}

			row := devices[v.tn.device]
			if row == nil {
				t.Fatalf("%s: device %s is missing from the stream's fleet snapshot; the mask "+
					"took the device with it", c.cred, v.tn.device)
			}
			// The digest spells "withheld" as the empty string, and it must
			// still read as held: a masked row that came through as free would
			// tell a dashboard the farm had capacity it does not have.
			if row["lease_state"] != "held" {
				t.Errorf("%s: device %s streams lease_state %v, want \"held\"",
					c.cred, v.tn.device, row["lease_state"])
			}
			for key, want := range map[string]string{
				"lease_id": v.tn.lease, "job_id": v.tn.job,
				"holder": v.tn.holder, "tenant_id": v.tn.tenant,
			} {
				got := ""
				if s, isString := row[key].(string); isString {
					got = s
				}
				if (got == want) != v.sees {
					t.Errorf("%s: device %s streams %s = %q (%s's is %q); visible should be %v",
						c.cred, v.tn.device, key, got, v.tn.cred, want, v.sees)
				}
				if !v.sees && got != "" {
					t.Errorf("%s: device %s streams %s = %q, want it withheld (empty)",
						c.cred, v.tn.device, key, got)
				}
			}
		}
	}

	// -----------------------------------------------------------------
	// `farmd ctl`, which agrees with the API or the API is not what it says.
	// -----------------------------------------------------------------
	//
	// ctl opens no database connection: everything it prints came out of the
	// same HTTP response asserted above, through a second decoder and a second
	// renderer. So a ctl that agrees is independent evidence, and a ctl that
	// disagrees means one of the two is reading a field the other is not.

	if a.job[:8] == b.job[:8] {
		// ctl abbreviates job ids to eight characters in the table. Two that
		// collide would make the text assertions below unfalsifiable.
		t.Fatalf("the two jobs share the abbreviation %q that `ctl fleet` prints; "+
			"this scenario cannot tell them apart in the table", a.job[:8])
	}
	for _, c := range callers {
		out, _, code := f.CtlAs(t, c.cred, "fleet")
		if code != 0 {
			t.Fatalf("ctl fleet as %s exited %d, want 0", c.cred, code)
		}
		for _, v := range c.of {
			if strings.Contains(out, v.tn.job[:8]) != v.sees {
				t.Errorf("`ctl fleet` as %s prints %s's job %s = %v, want %v. The operator run "+
					"prints both, so the id is printable and this is scope.",
					c.cred, v.tn.cred, v.tn.job[:8], !v.sees, v.sees)
			}
		}
	}

	// The holder cannot be asserted from the table the way a job id can: every
	// lease on this farm was placed by the same scheduler process, so both
	// leases carry the SAME holder string and its presence proves nothing. It
	// is asserted per device instead, off ctl's own JSON — which is what a
	// script consuming this listing would read.
	for _, c := range callers {
		out, _, code := f.CtlAs(t, c.cred, "fleet", "-o", "json")
		if code != 0 {
			t.Fatalf("ctl fleet -o json as %s exited %d, want 0", c.cred, code)
		}
		var listing struct {
			Devices []struct {
				DeviceID string `json:"device_id"`
				Lease    *struct {
					State    string  `json:"state"`
					ID       *string `json:"id"`
					JobID    *string `json:"job_id"`
					TenantID *string `json:"tenant_id"`
					Holder   *string `json:"holder"`
				} `json:"lease"`
			} `json:"devices"`
		}
		if err := json.Unmarshal([]byte(out), &listing); err != nil {
			t.Fatalf("decoding `ctl fleet -o json` as %s: %v\n%s", c.cred, err, firstLines(out, 20))
		}
		rows := map[string]*scopeCtlLease{}
		for _, d := range listing.Devices {
			if d.Lease == nil {
				continue
			}
			rows[d.DeviceID] = &scopeCtlLease{
				state: d.Lease.State, id: d.Lease.ID, job: d.Lease.JobID,
				tenant: d.Lease.TenantID, holder: d.Lease.Holder,
			}
		}
		for _, v := range c.of {
			row := rows[v.tn.device]
			if row == nil {
				t.Fatalf("`ctl fleet -o json` as %s shows no lease on device %s, which %s holds",
					c.cred, v.tn.device, v.tn.cred)
			}
			if row.state != "held" {
				t.Errorf("`ctl fleet -o json` as %s reads device %s as %q, want \"held\"",
					c.cred, v.tn.device, row.state)
			}
			for name, got := range map[string]*string{
				"holder": row.holder, "id": row.id, "job_id": row.job, "tenant_id": row.tenant,
			} {
				if (got != nil) != v.sees {
					t.Errorf("`ctl fleet -o json` as %s: %s's lease.%s on device %s = %s, "+
						"want visible=%v", c.cred, v.tn.cred, name, v.tn.device,
						scopeQuote(got), v.sees)
				}
			}
		}
	}

	// -----------------------------------------------------------------
	// And the fixture held: the same two leases, still live, still where the
	// scheduler put them. Everything above is about a mask, and a lease that
	// ended halfway through would have made the later reads describe a
	// different farm than the earlier ones.
	// -----------------------------------------------------------------

	for _, tn := range tenants {
		var state, device string
		if err := db.QueryRow(ctx,
			`SELECT state, device_id::text FROM farm.leases WHERE id = $1::uuid`, tn.lease).
			Scan(&state, &device); err != nil {
			t.Fatalf("re-reading %s's lease %s: %v", tn.cred, tn.lease, err)
		}
		if state != "held" || device != tn.device {
			t.Errorf("%s's lease %s is now %q on device %s; it was \"held\" on %s when the reads "+
				"began, so they did not all describe the same farm", tn.cred, tn.lease, state, device, tn.device)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// awaitPlacement blocks until the scheduler has given this tenant a device,
// and records what it was given.
func (p *scopeParty) awaitPlacement(t *testing.T, f *farm) {
	t.Helper()
	f.Eventually(t, 2*time.Minute, "the scheduler to place "+p.cred+"'s job on a device", func() error {
		err := f.DB().QueryRow(t.Context(), `
SELECT id::text, fence, device_id::text, holder
  FROM farm.leases WHERE job_id = $1::uuid AND state IN ('held','suspect')`, p.job).
			Scan(&p.lease, &p.fence, &p.device, &p.holder)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("job %s (%s) has no live lease yet", p.job, p.tenant)
		}
		return err
	})
	p.host, _ = f.DevicePosition(t, p.device)
	t.Logf("%s holds lease %s (fence %d, holder %q) on device %s at host %s",
		p.cred, p.lease, p.fence, p.holder, p.device, p.host)
}

// scopeRecordAttempt writes one finished recovery attempt against a device.
//
// It is written rather than run because the ladder climbs only for hardware
// that is failing, and it would never touch a healthy handset that is busy
// running a job — so a scenario that waited for a real attempt on a leased
// device would wait forever. That costs nothing here: GET /api/v1/recovery
// scopes an attempt not by how it was made but by which tenant holds the
// device's CURRENT lease, so a row inserted here is exactly the same evidence
// to the filter under test as a row the ladder wrote.
func scopeRecordAttempt(t *testing.T, f *farm, deviceID, host string) int64 {
	t.Helper()
	var id int64
	// Tier 0 is 'observe', the rung the ladder spends before it disturbs
	// anything; migration 00003 writes the whole table.
	if err := f.DB().QueryRow(t.Context(), `
INSERT INTO farm.recovery_attempts (device_id, host_id, tier, started_at, finished_at, outcome, detail)
VALUES ($1::uuid, $2, 0, now(), now(), 'no_change', jsonb_build_object('source','e2e tenant scope'))
RETURNING id`, deviceID, host).Scan(&id); err != nil {
		t.Fatalf("recording a recovery attempt on device %s: %v", deviceID, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// scopeAssertFleetLease checks one /fleet device against what the caller is
// allowed to know about the lease on it.
//
// The five identity fields are checked one at a time and by name, because a
// mask that withheld four of them would otherwise read as a pass, and because
// the operator debugging a failure needs to be told which field escaped.
func scopeAssertFleetLease(t *testing.T, cred string, row map[string]any, p *scopeParty, visible bool) {
	t.Helper()
	if row == nil {
		t.Fatalf("%s: device %s, which %s holds, is missing from /fleet entirely", cred, p.device, p.cred)
	}
	lease, _ := row["lease"].(map[string]any)
	if lease == nil {
		t.Fatalf("%s: device %s carries no lease on /fleet. The mask withholds WHOSE a lease "+
			"is; the device must still read as busy.\nrow: %v", cred, p.device, row)
	}
	// State and protection survive the mask on purpose: "this device is held,
	// and the reaper will not take it" is a fact about the fleet.
	if lease["state"] != "held" {
		t.Errorf("%s: device %s reads lease state %v on /fleet, want \"held\"", cred, p.device, lease["state"])
	}

	want := map[string]any{
		"id": p.lease, "fence": float64(p.fence), "job_id": p.job,
		"tenant_id": p.tenant, "holder": p.holder,
	}
	for _, key := range scopeLeaseIdentity {
		got, present := lease[key]
		if !present {
			t.Errorf("%s: lease.%s was OMITTED from device %s rather than nulled; a client "+
				"cannot tell \"withheld\" from \"no such field\"", cred, key, p.device)
			continue
		}
		switch {
		case visible && got != want[key]:
			t.Errorf("%s: device %s reads lease.%s = %v, want %v (it is %s's own lease)",
				cred, p.device, key, got, want[key], p.cred)
		case !visible && got != nil:
			t.Errorf("%s: device %s LEAKED lease.%s = %v, which belongs to %s. It must be null.",
				cred, p.device, key, got, p.cred)
		}
	}
}

// scopeAssertTopologySlot is the same cut on a slot.
//
// Two differences from /fleet are deliberate and are asserted as such. The
// slot carries three identity fields rather than five — there is no fence or
// holder on it — and they are OMITTED when withheld rather than nulled,
// because topologySlot marks them omitempty. What must survive is the lease's
// state, protection and disruption policy: that is what tells a tenant which
// recovery rungs the slot's current occupant has taken off the table, which is
// exactly what it needs to decide where its own job may land.
func scopeAssertTopologySlot(t *testing.T, cred string, slot map[string]any, p *scopeParty, visible bool) {
	t.Helper()
	if slot == nil {
		t.Fatalf("%s: the slot holding device %s (%s's) is missing from /topology; the mask "+
			"removed a rack position", cred, p.device, p.cred)
	}
	if slot["lease_state"] != "held" {
		t.Errorf("%s: the slot holding device %s reads lease_state %v, want \"held\"",
			cred, p.device, slot["lease_state"])
	}
	if _, ok := slot["disruption_policy"]; !ok {
		t.Errorf("%s: the slot holding device %s lost its disruption_policy. A tenant deciding "+
			"where to run needs to know which rungs the live lease forbids.", cred, p.device)
	}
	for key, want := range map[string]string{"lease_id": p.lease, "job_id": p.job, "tenant_id": p.tenant} {
		got, present := slot[key]
		if present != visible {
			t.Errorf("%s: the slot holding device %s has %s = %v (present %v), want visible=%v "+
				"(%s's value is %q)", cred, p.device, key, got, present, visible, p.cred, want)
			continue
		}
		if visible && got != want {
			t.Errorf("%s: the slot holding device %s reads %s = %v, want %q", cred, p.device, key, got, want)
		}
	}
}

// scopeCtlLease is one lease as `farmd ctl fleet -o json` renders it. The four
// pointers are the point: ctl must reproduce null for a withheld field rather
// than an empty string, or a script reading it cannot tell "not yours" from
// "no holder".
type scopeCtlLease struct {
	state                   string
	id, job, tenant, holder *string
}

func scopeQuote(p *string) string {
	if p == nil {
		return "null"
	}
	return `"` + *p + `"`
}

// ---------------------------------------------------------------------------
// Readers
// ---------------------------------------------------------------------------

// scopeIndex turns an array of objects in an API response into a map keyed by
// one string field, so an assertion can name the device or host it is about
// instead of an offset into a listing whose order it does not control.
func scopeIndex(t *testing.T, res apiResponse, key string, path ...string) map[string]map[string]any {
	t.Helper()
	items, ok := res.value(t, path...).([]any)
	if !ok {
		t.Fatalf("%s: %s is not an array\nbody: %s", res.Request, strings.Join(path, "."), res.text())
	}
	out := make(map[string]map[string]any, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s holds a %T, want objects\nbody: %s",
				res.Request, strings.Join(path, "."), item, res.text())
		}
		id, ok := obj[key].(string)
		if !ok {
			t.Fatalf("%s: an entry of %s has no string %q to key on: %v",
				res.Request, strings.Join(path, "."), key, obj)
		}
		out[id] = obj
	}
	return out
}

// scopeNumberSet is scopeIndex for the listings keyed by a bigint — recovery
// attempts and quarantines.
func scopeNumberSet(t *testing.T, res apiResponse, key string, path ...string) map[int64]bool {
	t.Helper()
	items, ok := res.value(t, path...).([]any)
	if !ok {
		t.Fatalf("%s: %s is not an array\nbody: %s", res.Request, strings.Join(path, "."), res.text())
	}
	out := make(map[int64]bool, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s holds a %T, want objects", res.Request, strings.Join(path, "."), item)
		}
		n, ok := obj[key].(float64)
		if !ok {
			t.Fatalf("%s: an entry of %s has no numeric %q: %v", res.Request, strings.Join(path, "."), key, obj)
		}
		out[int64(n)] = true
	}
	return out
}

// scopeTopologySlots flattens GET /api/v1/topology to the slots that hold a
// device, keyed by device id. The tree is host -> hub -> slot, and every
// assertion in this file is about a device, so walking it once here keeps the
// assertions about scope rather than about nesting.
func scopeTopologySlots(t *testing.T, res apiResponse) map[string]map[string]any {
	t.Helper()
	hosts, ok := res.value(t, "hosts").([]any)
	if !ok {
		t.Fatalf("%s: hosts is not an array\nbody: %s", res.Request, res.text())
	}
	out := map[string]map[string]any{}
	for _, h := range hosts {
		host, _ := h.(map[string]any)
		hubs, _ := host["hubs"].([]any)
		for _, hb := range hubs {
			hub, _ := hb.(map[string]any)
			slots, _ := hub["slots"].([]any)
			for _, s := range slots {
				slot, _ := s.(map[string]any)
				if id, ok := slot["device_id"].(string); ok {
					out[id] = slot
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no slot in the topology holds a device\nbody: %s", res.Request, res.text())
	}
	return out
}

// scopeObjectIndex keys one array inside a decoded stream frame.
func scopeObjectIndex(t *testing.T, cred string, frame map[string]any, field, key string) map[string]map[string]any {
	t.Helper()
	items, ok := frame[field].([]any)
	if !ok {
		t.Fatalf("%s: the stream frame has no %q array: %v", cred, field, frame)
	}
	out := make(map[string]map[string]any, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: the stream's %q holds a %T, want objects", cred, field, item)
		}
		if id, ok := obj[key].(string); ok {
			out[id] = obj
		}
	}
	return out
}

// scopeObjectSet is scopeObjectIndex when only membership matters.
func scopeObjectSet(t *testing.T, cred string, frame map[string]any, field, key string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for id := range scopeObjectIndex(t, cred, frame, field, key) {
		out[id] = true
	}
	return out
}

// scopeSnapshot opens GET /api/v1/stream as one credential and returns the
// first frame of each named event — the snapshot every client is sent on
// connect, rendered in that client's own scope.
//
// It is written here rather than added to client_test.go because it is the
// only reader in this package that must NOT consume the whole body: an event
// stream never ends, so f.get would hold it open until the request timeout and
// then report a stream that was working perfectly as a failure.
func scopeSnapshot(t *testing.T, f *farm, as string, want ...string) map[string]map[string]any {
	t.Helper()
	token, ok := f.tokens[as]
	if !ok {
		t.Fatalf("no %q credential on this farm", as)
	}

	// Generous, because the first frame waits on one poll of a database the
	// control plane is also using. It is a ceiling on a failure, not a delay:
	// subscribing kicks the poller, so the normal path returns in well under a
	// second.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.API(t)+"/api/v1/stream", nil)
	if err != nil {
		t.Fatalf("building the stream request for %s: %v", as, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the event stream as %s: %v", as, err)
	}
	// Cancelling the context is what actually ends the stream; closing the
	// body alone would leave the server writing heartbeats into a reader that
	// is gone.
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/stream as %s = %d, want 200", as, res.StatusCode)
	}

	pending := map[string]bool{}
	for _, name := range want {
		pending[name] = true
	}
	out := make(map[string]map[string]any, len(want))

	var event string
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if !pending[event] {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("decoding the %q frame of the stream as %s: %v", event, as, err)
			}
			// Only the FIRST frame of each event is read, and it must be the
			// snapshot: a delta that arrived while this loop was still reading
			// answers a different question — what changed — and asserting on
			// it would make this test depend on the poll timing.
			if payload["snapshot"] != true {
				t.Fatalf("the first %q frame the stream sent %s is a delta, not a snapshot: %v",
					event, as, payload)
			}
			out[event] = payload
			delete(pending, event)
			if len(pending) == 0 {
				return out
			}
		}
	}
	t.Fatalf("the event stream ended before it sent %s a snapshot of %v (got %v): %v",
		as, pending, out, sc.Err())
	return nil
}
