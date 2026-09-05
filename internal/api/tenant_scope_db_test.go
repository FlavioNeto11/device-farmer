package api

// Tenant scoping past /events (SEC-07), end to end against a real database.
//
// Two tenants share one host. Each holds one device; a third is free; the
// recovery ladder has touched all three. Three credentials — the two tenants
// and an operator — read every tenant-readable route, and each tenant must see
// its own fence and never the other's, while the operator sees the farm.
//
// These tests need DATABASE_URL pointing at a MIGRATED database and skip
// without one. They write rows with a per-run suffix and delete exactly those
// rows afterwards; nothing else in the database is touched, and every query
// they assert on is filtered to the host they created.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

type scopeFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context

	host, poolID     string
	tenantA, tenantB string
	hubID            int64

	devA, devB, devFree string
	jobA, jobB          string
	leaseA, leaseB      string
	fenceA, fenceB      int64
	holderA, holderB    string

	attemptA, attemptB, attemptFree int64
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; the tenant-scope tests need a migrated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	sfx := fmt.Sprintf("%d%06d", os.Getpid()%100000, time.Now().UnixNano()%1_000_000)
	f := &scopeFixture{
		t: t, pool: pool, ctx: ctx,
		host: "u6h" + sfx, poolID: "u6p" + sfx,
		tenantA: "u6a" + sfx, tenantB: "u6b" + sfx,
		holderA: "runner-a-" + sfx, holderB: "runner-b-" + sfx,
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
	f.devFree = f.device(3)

	f.jobA = f.job(f.tenantA)
	f.jobB = f.job(f.tenantB)
	f.leaseA, f.fenceA = f.lease(f.devA, f.jobA, f.holderA)
	f.leaseB, f.fenceB = f.lease(f.devB, f.jobB, f.holderB)

	f.attemptA = f.attempt(f.devA)
	f.attemptB = f.attempt(f.devB)
	f.attemptFree = f.attempt(f.devFree)
	return f
}

func (f *scopeFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatalf("fixture: %v\n%s", err, q)
	}
}

func (f *scopeFixture) scan(dst any, q string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, q, args...).Scan(dst); err != nil {
		f.t.Fatalf("fixture: %v\n%s", err, q)
	}
}

// device creates slot n on the fixture hub with a healthy device in it.
func (f *scopeFixture) device(n int) string {
	f.t.Helper()
	var slotID int64
	f.scan(&slotID, `
INSERT INTO farm.slots (host_id, hub_id, port_number, usb_path, topo_path, rack_slot)
VALUES ($1, $2, $3, $4, $5::ltree, $6)
RETURNING id`,
		f.host, f.hubID, n, fmt.Sprintf("1-1.%d", n), fmt.Sprintf("%s.p%d", f.host, n),
		fmt.Sprintf("R1-U1-H1-P%d", n))

	var id string
	f.scan(&id, `
INSERT INTO farm.devices (farm_uid, pool_id, host_id, current_slot_id, model)
VALUES ($1, $2, $3, $4, 'Scope Test')
RETURNING id::text`,
		fmt.Sprintf("df-%032x", time.Now().UnixNano()+int64(n)), f.poolID, f.host, slotID)
	f.exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, last_seen_at)
VALUES ($1::uuid, $2, $3, 'device', 'healthy', now())`, id, f.host, slotID)
	return id
}

func (f *scopeFixture) job(tenant string) string {
	f.t.Helper()
	var id string
	f.scan(&id, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, started_at)
VALUES ($1, $1, $2, 'running', now())
RETURNING id::text`, tenant, f.poolID)
	return id
}

// lease writes the row directly, as the reaper's fixtures do: the AFTER INSERT
// trigger points devices.current_lease_id at it exactly as in production.
func (f *scopeFixture) lease(deviceID, jobID, holder string) (string, int64) {
	f.t.Helper()
	var (
		id    string
		fence int64
	)
	if err := f.pool.QueryRow(f.ctx, `
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, ttl, grace, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, j.id, j.tenant_id, j.queue_id,
       $3, gen_random_uuid(), j.ttl, j.grace, now() + j.ttl, now() + j.ttl + j.grace
  FROM farm.devices d, farm.jobs j
 WHERE d.id = $1::uuid AND j.id = $2::uuid
RETURNING id::text, fence`, deviceID, jobID, holder).Scan(&id, &fence); err != nil {
		f.t.Fatalf("fixture lease: %v", err)
	}
	return id, fence
}

func (f *scopeFixture) attempt(deviceID string) int64 {
	f.t.Helper()
	var id int64
	f.scan(&id, `
INSERT INTO farm.recovery_attempts (device_id, slot_id, hub_id, host_id, tier, finished_at, outcome)
SELECT d.id, d.current_slot_id, $2, $3, 1, now(), 'recovered'
  FROM farm.devices d WHERE d.id = $1::uuid
RETURNING id`, deviceID, f.hubID, f.host)
	return id
}

// teardown removes exactly the rows this fixture wrote, in dependency order.
func (f *scopeFixture) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	steps := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM farm.recovery_attempts WHERE host_id = $1`, []any{f.host}},
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

// scopeServer is the real router over the fixture's database, with one
// credential per role and the stream poller running.
type scopeServer struct {
	t   *testing.T
	url string
}

const (
	tokenOperator = "Bearer op-token"
	tokenTenantA  = "Bearer ta-token"
	tokenTenantB  = "Bearer tb-token"
)

func newScopeServer(t *testing.T, f *scopeFixture) *scopeServer {
	t.Helper()
	bearer := bearerFor(t,
		"op-token:operator:alice",
		"ta-token:tenant:ci-a:"+f.tenantA,
		"tb-token:tenant:ci-b:"+f.tenantB)
	s, err := New(&config.Config{}, f.pool,
		WithAuthenticator(bearer),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStreamInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	go s.runStream(ctx)
	t.Cleanup(func() {
		s.stream.closeAll()
		cancel()
		srv.Close()
	})
	return &scopeServer{t: t, url: srv.URL}
}

// get performs one request and decodes the JSON body.
func (ss *scopeServer) get(token, path string) (int, map[string]any) {
	ss.t.Helper()
	req, err := http.NewRequest(http.MethodGet, ss.url+path, nil)
	if err != nil {
		ss.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ss.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			ss.t.Fatalf("GET %s: body is not JSON: %v\n%s", path, err, body)
		}
	}
	return resp.StatusCode, out
}

// mustGet is get with a 200 asserted.
func (ss *scopeServer) mustGet(token, path string) map[string]any {
	ss.t.Helper()
	code, body := ss.get(token, path)
	if code != http.StatusOK {
		ss.t.Fatalf("GET %s = %d: %v", path, code, body)
	}
	return body
}

// objects indexes a JSON list by one of its string fields.
func objects(list any, key string) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, _ := list.([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if k, ok := m[key].(string); ok {
				out[k] = m
			}
		}
	}
	return out
}

// fence reads a lease object's fence, or -1 when the lease is absent or the
// fence is withheld.
func fence(lease any) float64 {
	m, ok := lease.(map[string]any)
	if !ok {
		return -1
	}
	if v, ok := m["fence"].(float64); ok {
		return v
	}
	return -1
}

func TestTenantScopeBeyondEvents(t *testing.T) {
	f := newScopeFixture(t)
	ss := newScopeServer(t, f)
	fleetPath := "/api/v1/fleet?host=" + f.host

	t.Run("fleet shows every device and only the caller's leases", func(t *testing.T) {
		for _, tc := range []struct {
			token        string
			seesA, seesB bool
		}{
			{tokenTenantA, true, false},
			{tokenTenantB, false, true},
			{tokenOperator, true, true},
		} {
			body := ss.mustGet(tc.token, fleetPath)
			devices := objects(body["devices"], "device_id")
			if len(devices) != 3 {
				t.Fatalf("%s: %d devices on the fixture host, want 3", tc.token, len(devices))
			}
			if got := fence(devices[f.devA]["lease"]); (got == float64(f.fenceA)) != tc.seesA {
				t.Errorf("%s: device A fence = %v, want visible=%v (fence %d)", tc.token, got, tc.seesA, f.fenceA)
			}
			if got := fence(devices[f.devB]["lease"]); (got == float64(f.fenceB)) != tc.seesB {
				t.Errorf("%s: device B fence = %v, want visible=%v (fence %d)", tc.token, got, tc.seesB, f.fenceB)
			}
			if devices[f.devFree]["lease"] != nil {
				t.Errorf("%s: the free device grew a lease: %v", tc.token, devices[f.devFree]["lease"])
			}
			// A withheld lease is still a lease: the device reads as held.
			for _, id := range []string{f.devA, f.devB} {
				lease, _ := devices[id]["lease"].(map[string]any)
				if lease == nil || lease["state"] != "held" {
					t.Errorf("%s: device %s does not read as held: %v", tc.token, id, devices[id]["lease"])
				}
			}
			if counts := body["counts"].(map[string]any); counts["leased"] != float64(2) {
				t.Errorf("%s: leased count = %v, want 2 — busy devices are a fact about the farm", tc.token, counts["leased"])
			}
		}

		// The withheld fields are present and null, so a client can tell
		// "not yours" from "not a field".
		body := ss.mustGet(tokenTenantA, fleetPath)
		lease := objects(body["devices"], "device_id")[f.devB]["lease"].(map[string]any)
		for _, k := range leaseIdentity {
			if v, present := lease[k]; !present || v != nil {
				t.Errorf("tenant A reading tenant B's lease: %s = %v (present %v), want null", k, v, present)
			}
		}
	})

	t.Run("fleet search cannot probe another tenant's holder or job", func(t *testing.T) {
		for _, q := range []string{f.holderB, f.jobB} {
			path := fleetPath + "&q=" + q
			if n := len(ss.mustGet(tokenTenantA, path)["devices"].([]any)); n != 0 {
				t.Errorf("tenant A found %d device(s) searching for tenant B's %q", n, q)
			}
			if n := len(ss.mustGet(tokenTenantB, path)["devices"].([]any)); n != 1 {
				t.Errorf("tenant B found %d device(s) searching for its own %q, want 1", n, q)
			}
			if n := len(ss.mustGet(tokenOperator, path)["devices"].([]any)); n != 1 {
				t.Errorf("the operator found %d device(s) searching for %q, want 1", n, q)
			}
		}
	})

	t.Run("device detail follows the same cut", func(t *testing.T) {
		path := "/api/v1/devices/" + f.devB
		body := ss.mustGet(tokenTenantA, path)
		if got := fence(body["device"].(map[string]any)["lease"]); got != -1 {
			t.Errorf("tenant A read tenant B's fence %v off the device page", got)
		}
		if attempts, _ := body["recovery"].([]any); len(attempts) != 0 {
			t.Errorf("tenant A saw %d recovery attempt(s) on tenant B's device", len(attempts))
		}

		body = ss.mustGet(tokenTenantB, path)
		if got := fence(body["device"].(map[string]any)["lease"]); got != float64(f.fenceB) {
			t.Errorf("tenant B's own fence = %v, want %d", got, f.fenceB)
		}
		if attempts, _ := body["recovery"].([]any); len(attempts) != 1 {
			t.Errorf("tenant B saw %d recovery attempt(s) on its own device, want 1", len(attempts))
		}
	})

	t.Run("topology slots", func(t *testing.T) {
		slotsFor := func(token string) map[string]map[string]any {
			t.Helper()
			body := ss.mustGet(token, "/api/v1/topology?host="+f.host)
			hosts, _ := body["hosts"].([]any)
			if len(hosts) != 1 {
				t.Fatalf("%s: %d hosts, want 1", token, len(hosts))
			}
			hubs, _ := hosts[0].(map[string]any)["hubs"].([]any)
			if len(hubs) != 1 {
				t.Fatalf("%s: %d hubs, want 1", token, len(hubs))
			}
			return objects(hubs[0].(map[string]any)["slots"], "device_id")
		}

		slots := slotsFor(tokenTenantA)
		if slots[f.devA]["lease_id"] != f.leaseA {
			t.Errorf("tenant A's own slot lost its lease id: %v", slots[f.devA])
		}
		if slots[f.devB]["lease_id"] != nil || slots[f.devB]["job_id"] != nil || slots[f.devB]["tenant_id"] != nil {
			t.Errorf("tenant B's lease identity leaked through topology: %v", slots[f.devB])
		}
		if slots[f.devB]["lease_state"] != "held" {
			t.Errorf("the masked slot no longer reads as held: %v", slots[f.devB])
		}

		slots = slotsFor(tokenOperator)
		if slots[f.devA]["lease_id"] != f.leaseA || slots[f.devB]["lease_id"] != f.leaseB {
			t.Errorf("the operator's topology lost a lease id: %v / %v", slots[f.devA], slots[f.devB])
		}
	})

	t.Run("hosts count only the caller's leases", func(t *testing.T) {
		for token, want := range map[string]float64{tokenTenantA: 1, tokenTenantB: 1, tokenOperator: 2} {
			hosts := objects(ss.mustGet(token, "/api/v1/hosts")["hosts"], "id")
			row := hosts[f.host]
			if row == nil {
				t.Fatalf("%s: fixture host missing from /hosts", token)
			}
			if row["live_leases"] != want {
				t.Errorf("%s: live_leases = %v, want %v", token, row["live_leases"], want)
			}
			// The hardware is described whole to everyone.
			if row["devices"] != float64(3) || row["healthy"] != float64(3) {
				t.Errorf("%s: devices/healthy = %v/%v, want 3/3", token, row["devices"], row["healthy"])
			}
		}
	})

	t.Run("recovery attempts follow the live lease", func(t *testing.T) {
		attemptIDs := func(token string) map[float64]bool {
			t.Helper()
			body := ss.mustGet(token, "/api/v1/recovery?host="+f.host)
			out := map[float64]bool{}
			for _, a := range body["attempts"].([]any) {
				out[a.(map[string]any)["id"].(float64)] = true
			}
			// The ladder itself is infrastructure and goes to everyone.
			if tiers, _ := body["tiers"].([]any); len(tiers) == 0 {
				t.Errorf("%s: the tier table was withheld", token)
			}
			return out
		}
		a, b, free := float64(f.attemptA), float64(f.attemptB), float64(f.attemptFree)

		got := attemptIDs(tokenTenantA)
		if !got[a] || got[b] || got[free] {
			t.Errorf("tenant A sees attempts %v; want only its own (%v)", got, a)
		}
		got = attemptIDs(tokenTenantB)
		if got[a] || !got[b] || got[free] {
			t.Errorf("tenant B sees attempts %v; want only its own (%v)", got, b)
		}
		got = attemptIDs(tokenOperator)
		if !got[a] || !got[b] || !got[free] {
			t.Errorf("the operator sees attempts %v; want all three", got)
		}
	})

	t.Run("bulk is operator-only in both directions", func(t *testing.T) {
		for _, path := range []string{"/api/v1/bulk", "/api/v1/bulk/" + f.jobA} {
			if code, _ := ss.get(tokenTenantA, path); code != http.StatusForbidden {
				t.Errorf("tenant GET %s = %d, want 403", path, code)
			}
		}
		if code, _ := ss.get(tokenOperator, "/api/v1/bulk"); code != http.StatusOK {
			t.Errorf("operator GET /api/v1/bulk = %d, want 200", code)
		}
	})

	t.Run("stream renders the first snapshot in the caller's scope", func(t *testing.T) {
		for _, tc := range []struct {
			token        string
			seesA, seesB bool
		}{
			{tokenTenantA, true, false},
			{tokenTenantB, false, true},
			{tokenOperator, true, true},
		} {
			snap := readSnapshot(t, ss.url, tc.token)

			leases := objects(snap["lease"]["leases"], "lease_id")
			if _, ok := leases[f.leaseA]; ok != tc.seesA {
				t.Errorf("%s: lease A in the snapshot = %v, want %v", tc.token, ok, tc.seesA)
			}
			if _, ok := leases[f.leaseB]; ok != tc.seesB {
				t.Errorf("%s: lease B in the snapshot = %v, want %v", tc.token, ok, tc.seesB)
			}
			jobs := objects(snap["job"]["jobs"], "job_id")
			if _, ok := jobs[f.jobA]; ok != tc.seesA {
				t.Errorf("%s: job A in the snapshot = %v, want %v", tc.token, ok, tc.seesA)
			}
			if _, ok := jobs[f.jobB]; ok != tc.seesB {
				t.Errorf("%s: job B in the snapshot = %v, want %v", tc.token, ok, tc.seesB)
			}

			devices := objects(snap["fleet"]["devices"], "device_id")
			rowB := devices[f.devB]
			if rowB == nil {
				t.Fatalf("%s: device B missing from the fleet snapshot", tc.token)
			}
			if (rowB["lease_id"] == f.leaseB) != tc.seesB || (rowB["holder"] == f.holderB) != tc.seesB {
				t.Errorf("%s: device B row = %v, want lease identity visible=%v", tc.token, rowB, tc.seesB)
			}
			if rowB["lease_state"] != "held" {
				t.Errorf("%s: device B no longer reads as held on the stream: %v", tc.token, rowB)
			}
		}
	})
}

// readSnapshot opens the event stream and returns the first full frame of each
// of the snapshot events, by event name.
func readSnapshot(t *testing.T, base, token string) map[string]map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/stream", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}

	want := map[string]bool{"fleet": true, "lease": true, "job": true}
	got := map[string]map[string]any{}
	var name, data string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if want[name] && data != "" {
				var payload map[string]any
				if err := json.Unmarshal([]byte(data), &payload); err != nil {
					t.Fatalf("frame %q is not JSON: %v", name, err)
				}
				if payload["snapshot"] == true {
					got[name] = payload
					delete(want, name)
				}
			}
			name, data = "", ""
			if len(want) == 0 {
				return got
			}
		}
	}
	t.Fatalf("stream ended before the snapshot arrived; still waiting for %v (%v)", want, sc.Err())
	return nil
}
