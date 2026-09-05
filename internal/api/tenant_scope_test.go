package api

// Tenant scoping past /events (SEC-07), the half that needs no database.
//
// The rule every read route applies is the same: the fleet is shared
// infrastructure and every tenant may see it whole; the allocation on it is
// not, and a lease the caller does not own is reduced to its state. These
// tests pin the rule at the three places it is applied in Go — the fleet
// row, the topology slot and the event stream — and add a guard that fails
// the build when a tenant-readable GET route is added without either calling
// tenantScope or being listed here with a reason.

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func strp(s string) *string { return &s }
func i64p(n int64) *int64   { return &n }

// leaseIdentity are the JSON keys a tenant may not read off another tenant's
// lease. Everything else on the lease object is about the device.
var leaseIdentity = []string{"id", "fence", "job_id", "tenant_id", "holder"}

func TestFleetLeaseIsWithheldOutsideItsTenant(t *testing.T) {
	held := func() fleetDevice {
		return fleetDevice{DeviceID: "d1", Lease: &fleetLease{
			ID: strp("L1"), Fence: i64p(42), State: "suspect", Protected: true,
			JobID: strp("J1"), TenantID: strp("tenant-a"), Holder: strp("runner-1"),
		}}
	}
	cases := map[string]struct {
		scope   string
		visible bool
	}{
		"an operator sees the lease whole":       {"", true},
		"the owning tenant sees its lease whole": {"tenant-a", true},
		"another tenant sees the state only":     {"tenant-b", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := held()
			d.maskForTenant(tc.scope)
			lease := jsonObject(t, d)["lease"].(map[string]any)
			for _, k := range leaseIdentity {
				if (lease[k] != nil) != tc.visible {
					t.Errorf("lease.%s = %v for scope %q; visible should be %v", k, lease[k], tc.scope, tc.visible)
				}
			}
			// The keys are present and null, never omitted: a client must be
			// able to tell "withheld" from "this field does not exist".
			for _, k := range leaseIdentity {
				if _, present := lease[k]; !present {
					t.Errorf("lease.%s was omitted rather than nulled", k)
				}
			}
			if lease["state"] != "suspect" || lease["protected"] != true {
				t.Errorf("state/protected did not survive the mask: %v", lease)
			}
		})
	}

	t.Run("a free device is untouched", func(t *testing.T) {
		d := fleetDevice{DeviceID: "free"}
		d.maskForTenant("tenant-b")
		if d.Lease != nil {
			t.Fatalf("masking invented a lease: %+v", d.Lease)
		}
	})
}

func TestTopologySlotIsWithheldOutsideItsTenant(t *testing.T) {
	held := func() topologySlot {
		return topologySlot{
			SlotID: 7, LeaseID: strp("L1"), LeaseState: strp("held"),
			JobID: strp("J1"), TenantID: strp("tenant-a"),
			Protected: new(bool), Policy: strp("no_disruption"),
		}
	}
	for scope, visible := range map[string]bool{"": true, "tenant-a": true, "tenant-b": false} {
		sl := held()
		sl.maskForTenant(scope)
		for k, v := range map[string]*string{"lease_id": sl.LeaseID, "job_id": sl.JobID, "tenant_id": sl.TenantID} {
			if (v != nil) != visible {
				t.Errorf("scope %q: %s = %v, visible should be %v", scope, k, v, visible)
			}
		}
		// What a tenant needs to decide where its job may land: whether the
		// slot is held, and which recovery rungs that lease forbids.
		if sl.LeaseState == nil || *sl.LeaseState != "held" || sl.Policy == nil || sl.Protected == nil {
			t.Errorf("scope %q: the lease's state, policy or protection was masked: %+v", scope, sl)
		}
	}

	free := topologySlot{SlotID: 8}
	free.maskForTenant("tenant-b")
	if free.LeaseID != nil || free.LeaseState != nil {
		t.Fatalf("masking a free slot invented a lease: %+v", free)
	}
}

// scopedState is a farm two tenants share: tenant-a holds dA, tenant-b holds
// dB, dF is free, and the ladder is working on dB under a quarantine.
func scopedState() streamState {
	return streamState{
		fleet: map[string]fleetDigest{
			"dA": {DeviceID: "dA", LeaseID: "LA", LeaseState: "held", JobID: "JA", Holder: "hA", TenantID: "tenant-a"},
			"dB": {DeviceID: "dB", LeaseID: "LB", LeaseState: "suspect", JobID: "JB", Holder: "hB", TenantID: "tenant-b"},
			"dF": {DeviceID: "dF"},
		},
		leases: map[string]leaseDigest{
			"LA": {LeaseID: "LA", State: "held", Fence: 1, DeviceID: "dA", JobID: "JA", TenantID: "tenant-a"},
			"LB": {LeaseID: "LB", State: "suspect", Fence: 2, DeviceID: "dB", JobID: "JB", TenantID: "tenant-b", Protected: true},
		},
		jobs: map[string]jobDigest{
			"JA": {JobID: "JA", State: "running", TenantID: "tenant-a"},
			"JB": {JobID: "JB", State: "running", TenantID: "tenant-b"},
		},
		recovery:    map[int64]recoveryDigest{1: {ID: 1, Tier: 1, TierName: "adb_reconnect", DeviceID: "dB"}},
		quarantines: map[int64]quarantineDigest{7: {ID: 7, Scope: "device", DeviceID: "dB", Reason: "flapping"}},
		hubs:        map[int64]hubDigest{3: {HubID: 3, HostID: "h1", USBPath: "1-1", Devices: 4, Unhealthy: 2}},
	}
}

// frames decodes rendered SSE events by name.
func frames(t *testing.T, events []*sseEvent) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, ev := range events {
		var payload map[string]any
		if err := json.Unmarshal(ev.data, &payload); err != nil {
			t.Fatalf("frame %q is not JSON: %v", ev.name, err)
		}
		out[ev.name] = payload
	}
	return out
}

// ids collects one string field from a list of objects.
func ids(list any, key string) []string {
	var out []string
	items, _ := list.([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m[key].(string))
		}
	}
	return out
}

func alertKinds(payload map[string]any) []string {
	if payload == nil {
		return nil
	}
	return ids(payload["alerts"], "kind")
}

func TestStreamSnapshotIsRenderedPerScope(t *testing.T) {
	st := scopedState()

	t.Run("tenant", func(t *testing.T) {
		got := frames(t, fullEvents(st, "tenant-a"))

		fleet := got["fleet"]
		rows := map[string]map[string]any{}
		for _, r := range fleet["devices"].([]any) {
			m := r.(map[string]any)
			rows[m["device_id"].(string)] = m
		}
		if len(rows) != 3 {
			t.Fatalf("a tenant sees the whole fleet; got %d devices", len(rows))
		}
		if rows["dA"]["lease_id"] != "LA" || rows["dA"]["holder"] != "hA" {
			t.Errorf("the tenant's own lease was masked: %v", rows["dA"])
		}
		if rows["dB"]["lease_id"] != "" || rows["dB"]["job_id"] != "" || rows["dB"]["holder"] != "" || rows["dB"]["tenant_id"] != "" {
			t.Errorf("another tenant's lease identity leaked through the fleet row: %v", rows["dB"])
		}
		if rows["dB"]["lease_state"] != "suspect" {
			t.Errorf("the mask took the lease state with it: %v", rows["dB"])
		}
		// Counted before the mask: how many devices are busy is a fact about
		// the farm, not about who is using them.
		if counts := fleet["counts"].(map[string]any); counts["leased"] != float64(2) {
			t.Errorf("leased count = %v, want 2", counts["leased"])
		}

		if l := ids(got["lease"]["leases"], "lease_id"); len(l) != 1 || l[0] != "LA" {
			t.Errorf("lease snapshot = %v, want only LA", l)
		}
		if j := ids(got["job"]["jobs"], "job_id"); len(j) != 1 || j[0] != "JA" {
			t.Errorf("job snapshot = %v, want only JA", j)
		}
		// The ladder and the quarantine act on hardware; they pass whole even
		// though the device they name is held by somebody else.
		if len(got["recovery"]["attempts"].([]any)) != 1 || len(got["recovery"]["quarantines"].([]any)) != 1 {
			t.Errorf("recovery snapshot was filtered: %v", got["recovery"])
		}
		kinds := alertKinds(got["alert"])
		if !contains(kinds, "hub_correlation") {
			t.Errorf("the hub alert did not reach the tenant: %v", kinds)
		}
		if contains(kinds, "protected_lease_suspect") {
			t.Errorf("another tenant's protected-lease alert reached the tenant: %v", kinds)
		}
	})

	t.Run("operator", func(t *testing.T) {
		got := frames(t, fullEvents(st, ""))
		if l := ids(got["lease"]["leases"], "lease_id"); len(l) != 2 {
			t.Errorf("operator lease snapshot = %v, want both", l)
		}
		if j := ids(got["job"]["jobs"], "job_id"); len(j) != 2 {
			t.Errorf("operator job snapshot = %v, want both", j)
		}
		if kinds := alertKinds(got["alert"]); !contains(kinds, "protected_lease_suspect") {
			t.Errorf("the operator lost the protected-lease alert: %v", kinds)
		}
	})
}

func TestStreamDeltaIsRenderedPerScope(t *testing.T) {
	prev := scopedState()
	prev.leases["LB"] = leaseDigest{LeaseID: "LB", State: "held", Fence: 2, DeviceID: "dB", JobID: "JB", TenantID: "tenant-b"}
	prev.jobs["JB"] = jobDigest{JobID: "JB", State: "allocating", TenantID: "tenant-b"}
	prev.leases["LA"] = leaseDigest{LeaseID: "LA", State: "held", Fence: 1, DeviceID: "dA", JobID: "JA", TenantID: "tenant-a"}

	// Both leases go suspect in the same poll; tenant-b's job also moved.
	cur := scopedState()
	cur.leases["LA"] = leaseDigest{LeaseID: "LA", State: "suspect", Fence: 1, DeviceID: "dA", JobID: "JA", TenantID: "tenant-a"}
	cur.fleet["dA"] = fleetDigest{DeviceID: "dA", LeaseID: "LA", LeaseState: "suspect", JobID: "JA", Holder: "hA", TenantID: "tenant-a"}

	t.Run("tenant", func(t *testing.T) {
		got := frames(t, deltaEvents(prev, cur, "tenant-a"))
		if l := ids(got["lease"]["changed"], "lease_id"); len(l) != 1 || l[0] != "LA" {
			t.Errorf("lease delta = %v, want only LA", l)
		}
		if _, ok := got["job"]; ok {
			t.Errorf("another tenant's job change reached the tenant: %v", got["job"])
		}
		kinds := alertKinds(got["alert"])
		if n := count(kinds, "lease_suspect"); n != 1 {
			t.Errorf("lease_suspect alerts = %d, want exactly the tenant's own", n)
		}
		for _, a := range got["alert"]["alerts"].([]any) {
			if m := a.(map[string]any); m["lease_id"] == "LB" {
				t.Errorf("an alert named another tenant's lease: %v", m)
			}
		}
	})

	t.Run("operator", func(t *testing.T) {
		got := frames(t, deltaEvents(prev, cur, ""))
		if l := ids(got["lease"]["changed"], "lease_id"); len(l) != 2 {
			t.Errorf("operator lease delta = %v, want both", l)
		}
		if j := ids(got["job"]["changed"], "job_id"); len(j) != 1 || j[0] != "JB" {
			t.Errorf("operator job delta = %v, want JB", j)
		}
		if n := count(alertKinds(got["alert"]), "lease_suspect"); n != 2 {
			t.Errorf("operator lease_suspect alerts = %d, want 2", n)
		}
	})
}

// TestStreamHubServesEachClientItsOwnScope is the property the hub restructure
// exists for: ONE poll, and two clients on it read two different farms.
func TestStreamHubServesEachClientItsOwnScope(t *testing.T) {
	s := &Server{
		reg:  prometheus.NewRegistry(),
		auth: NewAllowAll(slog.New(slog.NewTextHandler(io.Discard, nil)), "test"),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := s.registerMetrics(true); err != nil {
		t.Fatalf("register metrics: %v", err)
	}
	h := newStreamHub(s.log, s.metrics)

	tenant := h.subscribe("tenant-a")
	operator := h.subscribe("")
	h.publish(streamState{}, scopedState(), false, false)

	drain := func(c *streamClient) []*sseEvent {
		var out []*sseEvent
		for {
			select {
			case ev := <-c.ch:
				out = append(out, ev)
			default:
				return out
			}
		}
	}
	tf := frames(t, drain(tenant))
	of := frames(t, drain(operator))

	if l := ids(tf["lease"]["leases"], "lease_id"); len(l) != 1 || l[0] != "LA" {
		t.Errorf("tenant client received leases %v, want only LA", l)
	}
	if l := ids(of["lease"]["leases"], "lease_id"); len(l) != 2 {
		t.Errorf("operator client received leases %v, want both", l)
	}
	// Both were served the same poll: the fleet is the same size for both.
	if len(tf["fleet"]["devices"].([]any)) != len(of["fleet"]["devices"].([]any)) {
		t.Errorf("the two clients were served different fleets")
	}
	h.closeAll()
}

// ---------------------------------------------------------------------------
// The route guard
// ---------------------------------------------------------------------------

// tenantReadAllowlist names the tenant-readable GET routes that legitimately
// call no tenantScope, each with the reason. A route added to the table
// without a tenantScope call and without an entry here fails the build; an
// entry whose route no longer exists fails too, so this list cannot rot.
var tenantReadAllowlist = map[string]string{
	"GET /api/v1/capabilities": "inventories the deployment — schema version, component beats, fleet size — and names no tenant's work",
	"GET /api/v1/specs/kinds":  "the step vocabulary from farm.step_kinds: shared configuration, no tenant data",
	"GET /api/v1/specs/resets": "a reset tier expanded against a profile's package list: shared configuration, no tenant data",
	"GET /api/v1/artifacts": "content-addressed builds are shared content; the job references that would name a tenant " +
		"are served by GET /api/v1/artifacts/{sha}, which is scoped",
	"GET /api/v1/artifacts/{sha}/content": "the bytes under a digest: shared content, as above",
	"GET /api/v1/devices/{id}/artifacts": "farm.device_artifacts is what is installed on a phone — a fact about the " +
		"hardware that names no job and no tenant",
}

var (
	// tenant("GET /api/v1/fleet", s.handleFleet)
	routerTenantGet = regexp.MustCompile(`tenant\("GET ([^"]+)",\s*s\.(\w+)\)`)
	// mux.Handle("GET /api/v1/artifacts", s.requireRole(RoleTenant, http.HandlerFunc(a.handleList)))
	// mux.Handle("GET .../content",\n a.srv.requireRole(RoleTenant, contentHandler(a.store.Get, a.failBlob)))
	mountTenantGet = regexp.MustCompile(`mux\.Handle\("GET ([^"]+)",\s*[\w.]*requireRole\(RoleTenant,\s*(?:http\.HandlerFunc\()?([\w.]+)`)
)

// packageSource is every non-test Go file in this package, concatenated.
func packageSource(t *testing.T) string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var b strings.Builder
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(src)
		b.WriteString("\n")
	}
	return b.String()
}

// funcBodies returns the source of every top-level function or method named
// name. gofmt puts a top-level closing brace at column 0, which is what ends
// the match.
func funcBodies(src, name string) []string {
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(.*?^}$`)
	return re.FindAllString(src, -1)
}

func TestEveryTenantReadableRouteIsScoped(t *testing.T) {
	src := packageSource(t)

	routes := map[string]string{}
	for _, m := range routerTenantGet.FindAllStringSubmatch(src, -1) {
		routes["GET "+m[1]] = m[2]
	}
	for _, m := range mountTenantGet.FindAllStringSubmatch(src, -1) {
		handler := m[2]
		if i := strings.LastIndex(handler, "."); i >= 0 {
			handler = handler[i+1:]
		}
		routes["GET "+m[1]] = handler
	}
	// A regexp that silently matches nothing would pass this test for free.
	// The table has more tenant-readable reads than this.
	if len(routes) < 12 {
		t.Fatalf("found only %d tenant-readable GET routes; the registration patterns have drifted: %v", len(routes), routes)
	}

	for route := range tenantReadAllowlist {
		if _, registered := routes[route]; !registered {
			t.Errorf("allowlist names %q, which is no longer a tenant-readable route; remove the entry", route)
		}
	}

	for route, handler := range routes {
		if _, allowed := tenantReadAllowlist[route]; allowed {
			continue
		}
		bodies := funcBodies(src, handler)
		if len(bodies) == 0 {
			t.Errorf("%s: handler %s not found in the package source", route, handler)
			continue
		}
		for _, body := range bodies {
			if !strings.Contains(body, "tenantScope(") {
				t.Errorf("%s is tenant-readable and %s never calls tenantScope: a tenant token reads "+
					"the farm through it. Scope it, or add it to tenantReadAllowlist with the reason.",
					route, handler)
			}
		}
	}
}

// TestBulkReadsAreOperatorOnly: a bulk run's output is the shell output of a
// command an operator ran across the farm, on devices holding other tenants'
// leases at the time. It went out on a tenant route while the route that
// creates a run was operator-only.
func TestBulkReadsAreOperatorOnly(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	for _, route := range []string{"GET /api/v1/bulk", "GET /api/v1/bulk/{id}", "POST /api/v1/bulk"} {
		if !strings.Contains(string(src), `operator("`+route+`"`) {
			t.Errorf("%s is not registered with operator(...)", route)
		}
		if strings.Contains(string(src), `tenant("`+route+`"`) {
			t.Errorf("%s is registered with tenant(...)", route)
		}
	}
}

func jsonObject(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func contains(list []string, want string) bool { return count(list, want) > 0 }

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
