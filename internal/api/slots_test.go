package api

// The operator surface for slots, exercised end to end: a request with a
// bearer token, through the router, the handler, the SQL functions of
// migration 00017 and — for rebrand — a real adbwire client talking to
// test/fakeadb.
//
// These tests need DATABASE_URL to name a migrated database they may write
// fixture rows into. They skip without one; a DATABASE_URL that is set but
// unreachable is a failure, because somebody asked for these to run. Every
// fixture lives under ids of its own and is deleted afterwards, so a shared
// scratch database is left as it was found, audit rows aside.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/enroll"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

const (
	operatorToken = "u13-operator-token"
	tenantToken   = "u13-tenant-token"
)

func slotsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
	if dsn == "" {
		t.Skip("no DATABASE_URL; the slot surface needs a migrated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("DATABASE_URL is set but does not parse: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("DATABASE_URL is set but unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// slotsServer builds a server whose router is real, whose authenticator knows
// one operator and one tenant, and whose executor dials whatever endpoint the
// fixture's host carries.
func slotsServer(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	auth, err := NewStaticBearer([]string{
		operatorToken + ":operator:alice",
		tenantToken + ":tenant:bob",
	})
	if err != nil {
		t.Fatalf("static bearer: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(&config.Config{Component: "api-test"}, pool,
		WithAuthenticator(auth), WithLogger(quiet))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.Handler()
}

// slotsCall sends one request and decodes the JSON reply into a generic map.
func slotsCall(t *testing.T, h http.Handler, method, path, token string, body any) (int, map[string]any, string) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		payload = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	raw := rec.Body.String()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s %s: the reply is not a JSON object: %v\n%s", method, path, err, raw)
	}
	return rec.Code, out, raw
}

// errorDetail reads the detail object of an error envelope.
func errorDetail(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	env, _ := out["error"].(map[string]any)
	if env == nil {
		t.Fatalf("reply carries no error envelope: %v", out)
	}
	detail, _ := env["detail"].(map[string]any)
	return detail
}

// slotFixture is one host of its own, with slots registered through
// farm.register_slot and devices sitting in the first of them.
type slotFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context

	host, poolID, tenant, queue string
	endpoint                    string
	slots                       []int64
	devices                     []string // uuids
	uids                        []string
}

func newSlotFixture(t *testing.T, pool *pgxpool.Pool, endpoint string, slots, devices int) *slotFixture {
	t.Helper()
	tag := fmt.Sprintf("u13-%d-%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	f := &slotFixture{
		t: t, pool: pool, ctx: context.Background(),
		host: "h-" + tag, poolID: "p-" + tag, tenant: "t-" + tag, queue: "q-" + tag,
		endpoint: endpoint,
	}
	t.Cleanup(f.drop)

	f.exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	f.exec(`INSERT INTO farm.tenants (id) VALUES ($1)`, f.tenant)
	f.exec(`INSERT INTO farm.queues (id, tenant_id) VALUES ($1, $2)`, f.queue, f.tenant)
	f.exec(`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, $2)`, f.host, endpoint)

	for i := 1; i <= slots; i++ {
		var id int64
		f.scan(&id, `SELECT farm.register_slot($1, $2, '3-1', $3, 'Test Hub', 7, true, $4)`,
			f.host, fmt.Sprintf("3-1.%d", i), i, fmt.Sprintf("%s-P%d", tag, i))
		f.slots = append(f.slots, id)
	}
	for i := 0; i < devices; i++ {
		uid := fmt.Sprintf("df-%032x", time.Now().UnixNano()+int64(i))
		var id string
		f.scan(&id, `
INSERT INTO farm.devices (farm_uid, pool_id, host_id, current_slot_id, model)
VALUES ($1, $2, $3, $4, 'Test Device') RETURNING id::text`, uid, f.poolID, f.host, f.slots[i])
		f.exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, last_seen_at)
VALUES ($1::uuid, $2, $3, 'device', 'healthy', now())`, id, f.host, f.slots[i])
		f.exec(`INSERT INTO farm.slot_occupancy (slot_id, device_id) VALUES ($1, $2::uuid)`, f.slots[i], id)
		f.devices = append(f.devices, id)
		f.uids = append(f.uids, uid)
	}
	return f
}

// lease pins a job to a device and acquires through farm.lease_acquire, so
// the lease the refusals are about is a real one with a real fence.
func (f *slotFixture) lease(device string) (leaseID string, fence int64) {
	f.t.Helper()
	var job string
	f.scan(&job, `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, pin_device, expected_duration)
VALUES ($1, $2, $3, $4::uuid, interval '6 hours') RETURNING id::text`, f.tenant, f.queue, f.poolID, device)
	if err := f.pool.QueryRow(f.ctx, `
SELECT lease_id::text, fence FROM farm.lease_acquire($1::uuid, 'u13-runner', gen_random_uuid())`, job).
		Scan(&leaseID, &fence); err != nil {
		f.t.Fatalf("acquire the fixture lease: %v", err)
	}
	return leaseID, fence
}

func (f *slotFixture) drop() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM farm.leases WHERE device_id IN (SELECT id FROM farm.devices WHERE host_id = $1)`, []any{f.host}},
		{`DELETE FROM farm.jobs WHERE pool_id = $1`, []any{f.poolID}},
		{`DELETE FROM farm.devices WHERE host_id = $1`, []any{f.host}},
		{`DELETE FROM farm.slots WHERE host_id = $1`, []any{f.host}},
		{`DELETE FROM farm.hosts WHERE id = $1`, []any{f.host}},
		{`DELETE FROM farm.queues WHERE tenant_id = $1`, []any{f.tenant}},
		{`DELETE FROM farm.tenants WHERE id = $1`, []any{f.tenant}},
		{`DELETE FROM farm.pools WHERE id = $1`, []any{f.poolID}},
	} {
		if _, err := f.pool.Exec(ctx, q.sql, q.args...); err != nil {
			f.t.Errorf("fixture cleanup %.50q: %v", q.sql, err)
		}
	}
}

func (f *slotFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func (f *slotFixture) scan(dest any, sql string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(dest); err != nil {
		f.t.Fatalf("query %.60q: %v", sql, err)
	}
}

func (f *slotFixture) currentSlot(device string) *int64 {
	f.t.Helper()
	var slot *int64
	f.scan(&slot, `SELECT current_slot_id FROM farm.devices WHERE id = $1::uuid`, device)
	return slot
}

func (f *slotFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	f.scan(&n, sql, args...)
	return n
}

// TestSlotListIsTenantReadableAndCarriesNoLease pins the two halves of the
// listing's contract: a tenant may read it, and it says nothing about who
// holds what. The write routes stay behind the operator role.
//
// Falsify: register GET /api/v1/slots with operator() instead of tenant(), or
// add a lease column to slotColumns.
func TestSlotListIsTenantReadableAndCarriesNoLease(t *testing.T) {
	pool := slotsTestPool(t)
	f := newSlotFixture(t, pool, "127.0.0.1:1", 2, 2)
	f.lease(f.devices[0])
	h := slotsServer(t, pool)

	status, out, raw := slotsCall(t, h, http.MethodGet, "/api/v1/slots?host="+f.host, tenantToken, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /slots as a tenant = %d, want 200:\n%s", status, raw)
	}
	slots, _ := out["slots"].([]any)
	if len(slots) != 2 {
		t.Fatalf("listing carries %d slots, want 2:\n%s", len(slots), raw)
	}
	first, _ := slots[0].(map[string]any)
	if first["device_id"] != f.devices[0] || first["farm_uid"] != f.uids[0] {
		t.Errorf("the first slot does not name its occupant: %v", first)
	}
	if first["adb_devpath"] != "usb:3-1.1" || first["power_kind"] != "per_port" {
		t.Errorf("the first slot's physical fields are wrong: %v", first)
	}
	for _, forbidden := range []string{"lease", "holder", "fence", "job_id", "tenant_id"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the slot listing mentions %q; allocation is the lease listing's business:\n%s", forbidden, raw)
		}
	}

	for _, w := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/slots"},
		{http.MethodPost, fmt.Sprintf("/api/v1/slots/%d/label", f.slots[0])},
		{http.MethodPost, "/api/v1/devices/" + f.devices[1] + "/reslot"},
		{http.MethodPost, "/api/v1/devices/" + f.devices[1] + "/rebrand"},
	} {
		status, _, raw := slotsCall(t, h, w.method, w.path, tenantToken, map[string]any{"reason": "x"})
		if status != http.StatusForbidden {
			t.Errorf("%s %s as a tenant = %d, want 403:\n%s", w.method, w.path, status, raw)
		}
	}
}

// TestReslotRefusesALiveLeaseAndMovesAFreeDevice is the invariant and the
// ordinary case side by side. The refusal must name the lease, and must leave
// the lease exactly as it was; the move must leave every table that says where
// a device is in agreement.
//
// Falsify: delete the `case d.Lease != nil` arm of handleDeviceReslot. The
// SQL function still refuses, so the status stays 409 — but the reply no
// longer names the lease, and the assertion on detail.lease_id fails.
func TestReslotRefusesALiveLeaseAndMovesAFreeDevice(t *testing.T) {
	pool := slotsTestPool(t)
	f := newSlotFixture(t, pool, "127.0.0.1:1", 3, 2)
	leaseID, fence := f.lease(f.devices[0])
	h := slotsServer(t, pool)
	reslot := func(device string, body map[string]any) (int, map[string]any, string) {
		return slotsCall(t, h, http.MethodPost, "/api/v1/devices/"+device+"/reslot", operatorToken, body)
	}

	// The leased device keeps its slot.
	status, out, raw := reslot(f.devices[0], map[string]any{"slot_id": f.slots[2], "reason": "test"})
	if status != http.StatusConflict {
		t.Fatalf("re-slotting a leased device = %d, want 409:\n%s", status, raw)
	}
	if got := errorDetail(t, out)["lease_id"]; got != leaseID {
		t.Errorf("the refusal names lease %v, want %s:\n%s", got, leaseID, raw)
	}
	if got := f.currentSlot(f.devices[0]); got == nil || *got != f.slots[0] {
		t.Errorf("the refusal still moved the device: slot %v", got)
	}
	var state string
	var fenceNow int64
	f.scan(&state, `SELECT state FROM farm.leases WHERE id = $1::uuid`, leaseID)
	f.scan(&fenceNow, `SELECT fence FROM farm.leases WHERE id = $1::uuid`, leaseID)
	if state != "held" || fenceNow != fence {
		t.Errorf("the refusal touched the lease: state %s, fence %d -> %d", state, fence, fenceNow)
	}
	if n := f.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'device.reslot' AND subject = $1 AND detail->>'outcome' = 'refused'`,
		"device:"+f.devices[0]); n != 1 {
		t.Errorf("the refusal left %d audit rows, want 1", n)
	}

	// An occupied slot is refused, naming the occupant.
	status, out, raw = reslot(f.devices[1], map[string]any{"slot_id": f.slots[0], "reason": "test"})
	if status != http.StatusConflict {
		t.Fatalf("re-slotting onto an occupied slot = %d, want 409:\n%s", status, raw)
	}
	if got := errorDetail(t, out)["occupant_device_id"]; got != f.devices[0] {
		t.Errorf("the refusal names occupant %v, want %s", got, f.devices[0])
	}

	// No reason, no move.
	if status, _, raw := reslot(f.devices[1], map[string]any{"slot_id": f.slots[2]}); status != http.StatusBadRequest {
		t.Errorf("a re-slot without a reason = %d, want 400:\n%s", status, raw)
	}

	// The free device moves, and everything agrees about where it went.
	status, out, raw = reslot(f.devices[1], map[string]any{"slot_id": f.slots[2], "reason": "re-cabled to port 3"})
	if status != http.StatusOK {
		t.Fatalf("re-slotting a free device = %d, want 200:\n%s", status, raw)
	}
	if out["moved"] != true || int64(out["to_slot_id"].(float64)) != f.slots[2] {
		t.Errorf("the reply does not report the move: %s", raw)
	}
	if got := f.currentSlot(f.devices[1]); got == nil || *got != f.slots[2] {
		t.Errorf("devices.current_slot_id = %v, want %d", got, f.slots[2])
	}
	if n := f.count(`SELECT count(*) FROM farm.slot_occupancy WHERE slot_id = $1 AND until IS NULL`, f.slots[1]); n != 0 {
		t.Errorf("the old slot still has %d open occupancy row(s)", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.slot_occupancy WHERE slot_id = $1 AND device_id = $2::uuid AND until IS NULL`,
		f.slots[2], f.devices[1]); n != 1 {
		t.Errorf("the new slot has %d open occupancy row(s) for the device, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'device.reslot' AND subject = $1 AND actor = 'alice' AND reason = 're-cabled to port 3' AND (detail->>'to_slot_id')::bigint = $2`,
		"device:"+f.devices[1], f.slots[2]); n != 1 {
		t.Errorf("the move left %d audit rows, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_reslotted' AND device_id = $1::uuid AND slot_id = $2`,
		f.devices[1], f.slots[2]); n != 1 {
		t.Errorf("the move left %d device_reslotted events, want 1", n)
	}

	// The listing agrees too.
	_, listing, raw := slotsCall(t, h, http.MethodGet, "/api/v1/slots?host="+f.host, tenantToken, nil)
	slots, _ := listing["slots"].([]any)
	if len(slots) != 3 {
		t.Fatalf("listing carries %d slots, want 3:\n%s", len(slots), raw)
	}
	if second, _ := slots[1].(map[string]any); second["device_id"] != nil {
		t.Errorf("the vacated slot still lists a device: %v", second)
	}
	if third, _ := slots[2].(map[string]any); third["device_id"] != f.devices[1] {
		t.Errorf("the destination slot does not list the device: %v", third)
	}

	// And back out of any slot at all.
	status, _, raw = reslot(f.devices[1], map[string]any{"unslot": true, "reason": "bench"})
	if status != http.StatusOK {
		t.Fatalf("unslotting = %d, want 200:\n%s", status, raw)
	}
	if got := f.currentSlot(f.devices[1]); got != nil {
		t.Errorf("an unslotted device is still in slot %d", *got)
	}
}

// TestRelabelWritesTheAuditRow: a label is what an alert prints, so the row
// that changes it carries the label it replaced.
//
// Falsify: drop the INSERT INTO farm.audit_log from farm.relabel_slot.
func TestRelabelWritesTheAuditRow(t *testing.T) {
	pool := slotsTestPool(t)
	f := newSlotFixture(t, pool, "127.0.0.1:1", 2, 0)
	h := slotsServer(t, pool)
	label := func(slot int64, body map[string]any) (int, map[string]any, string) {
		return slotsCall(t, h, http.MethodPost, fmt.Sprintf("/api/v1/slots/%d/label", slot), operatorToken, body)
	}

	var before string
	f.scan(&before, `SELECT rack_slot FROM farm.slots WHERE id = $1`, f.slots[0])

	status, out, raw := label(f.slots[0], map[string]any{"rack_slot": "U13-BENCH", "reason": "shelf moved"})
	if status != http.StatusOK {
		t.Fatalf("relabel = %d, want 200:\n%s", status, raw)
	}
	if out["previous_rack_slot"] != before {
		t.Errorf("previous_rack_slot = %v, want %s", out["previous_rack_slot"], before)
	}
	var after string
	f.scan(&after, `SELECT rack_slot FROM farm.slots WHERE id = $1`, f.slots[0])
	if after != "U13-BENCH" {
		t.Errorf("rack_slot = %q after relabel, want U13-BENCH", after)
	}
	if n := f.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'slot.relabel' AND subject = $1 AND actor = 'alice' AND reason = 'shelf moved' AND detail->>'previous_rack_slot' = $2 AND detail->>'rack_slot' = 'U13-BENCH'`,
		fmt.Sprintf("slot:%d", f.slots[0]), before); n != 1 {
		t.Errorf("relabel left %d audit rows naming old and new, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'slot_relabelled' AND slot_id = $1`, f.slots[0]); n != 1 {
		t.Errorf("relabel left %d slot_relabelled events, want 1", n)
	}

	if status, _, raw := label(f.slots[1], map[string]any{"rack_slot": "U13-BENCH", "reason": "dup"}); status != http.StatusConflict {
		t.Errorf("a second slot under the same label = %d, want 409:\n%s", status, raw)
	}
	if status, _, raw := label(f.slots[1], map[string]any{"rack_slot": "U13-OTHER"}); status != http.StatusBadRequest {
		t.Errorf("a relabel without a reason = %d, want 400:\n%s", status, raw)
	}
}

// TestRegisterSlotBuildsTheTopologyOnce: the register route is
// farm.register_slot behind a request body — a new position answers 201 with
// the controller, hub and power domain built around it, and the same position
// again answers 200 with its label intact.
//
// Falsify: drop the `usb_path != hub_path+"."+port` check, and the request
// below with usb_path 5-2.9 on port 3 answers 201 instead of 400.
func TestRegisterSlotBuildsTheTopologyOnce(t *testing.T) {
	pool := slotsTestPool(t)
	f := newSlotFixture(t, pool, "127.0.0.1:1", 0, 0)
	h := slotsServer(t, pool)
	register := func(body map[string]any) (int, map[string]any, string) {
		return slotsCall(t, h, http.MethodPost, "/api/v1/slots", operatorToken, body)
	}
	base := map[string]any{"host_id": f.host, "usb_path": "5-2.3", "hub_path": "5-2", "port": 3, "switchable": true}
	with := func(extra map[string]any) map[string]any {
		return mergeDetail(base, extra)
	}

	status, out, raw := register(with(map[string]any{"rack_slot": "U13-NEW", "reason": "new tray"}))
	if status != http.StatusCreated {
		t.Fatalf("registering a new slot = %d, want 201:\n%s", status, raw)
	}
	slot, _ := out["slot"].(map[string]any)
	if out["created"] != true || slot["adb_devpath"] != "usb:5-2.3" || slot["rack_slot"] != "U13-NEW" ||
		slot["power_kind"] != "per_port" || slot["hub_path"] != "5-2" {
		t.Errorf("the created slot is wrong: %s", raw)
	}
	slotID := int64(slot["slot_id"].(float64))
	if n := f.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'slot.register' AND subject = $1 AND (detail->>'created')::boolean`,
		fmt.Sprintf("slot:%d", slotID)); n != 1 {
		t.Errorf("registration left %d audit rows, want 1", n)
	}

	status, out, raw = register(with(nil))
	if status != http.StatusOK || out["created"] != false {
		t.Fatalf("registering the same slot again = %d created=%v, want 200 and false:\n%s", status, out["created"], raw)
	}
	if slot, _ := out["slot"].(map[string]any); slot["rack_slot"] != "U13-NEW" {
		t.Errorf("re-registering without a label erased the label: %s", raw)
	}

	if status, _, raw := register(with(map[string]any{"usb_path": "5-2.9"})); status != http.StatusBadRequest {
		t.Errorf("a usb_path that disagrees with hub_path and port = %d, want 400:\n%s", status, raw)
	}
	if status, _, raw := register(with(map[string]any{"host_id": f.host + "-missing"})); status != http.StatusNotFound {
		t.Errorf("an unknown host = %d, want 404:\n%s", status, raw)
	}
}

// shellV2 frames a scripted shell reply the way a device would: stdout, then
// the exit status.
func shellV2(t *testing.T, stdout string, exit byte) string {
	t.Helper()
	var b bytes.Buffer
	if stdout != "" {
		if err := adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout)); err != nil {
			t.Fatalf("framing stdout: %v", err)
		}
	}
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{exit}); err != nil {
		t.Fatalf("framing exit: %v", err)
	}
	return b.String()
}

// fakePhone is the brand file on a fakeadb device, answering the exact shell
// commands enroll.Brander sends: the read says by exit status whether there is
// a file, and the write evaluates its own guard the way the shell would — the
// file may hold any uid the guard names and nothing else — so a brand in place
// at the instant of writing is refused unless the command was written to
// replace exactly that brand.
type fakePhone struct {
	mu    sync.Mutex
	brand string
}

// quotedUIDRe picks the single-quoted uids out of a brand command's guard.
var quotedUIDRe = regexp.MustCompile(`'(df-[0-9a-f]{32})'`)

func (p *fakePhone) install(t *testing.T, srv *fakeadb.Server, devpath string) {
	t.Helper()
	srv.RespondWith(devpath, "shell,v2,raw:", func(service string) string {
		cmd := strings.TrimPrefix(service, "shell,v2,raw:")
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case strings.HasPrefix(cmd, "if [ -e "+enroll.BrandPath+" ]"):
			if p.brand == "" {
				return shellV2(t, "", 44)
			}
			return shellV2(t, p.brand+"\n", 0)
		case strings.HasPrefix(cmd, "if [ -s "+enroll.BrandPath+" ]"):
			guard, install, _ := strings.Cut(cmd, "; mkdir -p ")
			admitted := false
			for _, m := range quotedUIDRe.FindAllStringSubmatch(guard, -1) {
				admitted = admitted || m[1] == p.brand
			}
			if p.brand != "" && !admitted {
				return shellV2(t, "", 17)
			}
			_, rest, _ := strings.Cut(install, "printf '%s' '")
			uid, _, _ := strings.Cut(rest, "'")
			p.brand = uid
			return shellV2(t, "", 0)
		}
		return shellV2(t, "", 127)
	})
}

func (p *fakePhone) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.brand
}

func (p *fakePhone) set(uid string) {
	p.mu.Lock()
	p.brand = uid
	p.mu.Unlock()
}

// TestRebrandWritesTheRowsUIDOntoThePhone drives the whole path: the API's own
// adbwire client, fakeadb, and enroll.Brander's read-check-write-verify. A
// stranger's brand is replaced and reported; a brand belonging to another row
// of the fleet is replaced and that row is named, with neither row's farm_uid
// changed; a matching brand is left alone; a brand other than the one the
// operator authorised against is refused untouched; and a leased device is
// refused before a single byte reaches the phone.
//
// Falsify: delete the `case d.Lease != nil` arm of handleDeviceRebrand. The
// leased subtest then sees a 200 and a shell request on the wire. Or make
// Brander.Rebrand write with brandWriteCmd instead of brandReplaceCmd: the
// device's own guard then refuses the stranger's brand and the first subtest
// sees a 409.
func TestRebrandWritesTheRowsUIDOntoThePhone(t *testing.T) {
	pool := slotsTestPool(t)
	srv := fakeadb.Start(t)
	const devpath = "usb:3-1.1"
	srv.Add(fakeadb.Device{Serial: "U13SERIAL", Devpath: devpath, State: fakeadb.StateDevice})
	phone := &fakePhone{}
	phone.install(t, srv, devpath)

	// Two rows: the one whose slot holds the phone, and one whose uid the
	// phone will be found carrying.
	f := newSlotFixture(t, pool, srv.Addr(), 2, 2)
	device, uid := f.devices[0], f.uids[0]
	h := slotsServer(t, pool)
	rebrand := func(body map[string]any) (int, map[string]any, string) {
		return slotsCall(t, h, http.MethodPost, "/api/v1/devices/"+device+"/rebrand", operatorToken, body)
	}
	stranger := "df-" + strings.Repeat("a", 32)

	t.Run("a stranger's brand is replaced and named", func(t *testing.T) {
		phone.set(stranger)
		status, out, raw := rebrand(map[string]any{"reason": "this row is the phone"})
		if status != http.StatusOK {
			t.Fatalf("rebrand = %d, want 200:\n%s", status, raw)
		}
		if out["outcome"] != string(enroll.BrandWritten) || out["previous_uid"] != stranger {
			t.Errorf("reply = %s; want outcome written and the stranger's uid as previous", raw)
		}
		if got := phone.get(); got != uid {
			t.Errorf("the phone carries %q after the rebrand, want the row's uid %s", got, uid)
		}
		if n := f.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'device.rebrand' AND subject = $1 AND actor = 'alice' AND detail->>'previous_uid' = $2 AND detail->>'outcome' = 'written'`,
			"device:"+device, stranger); n != 1 {
			t.Errorf("rebrand left %d audit rows, want 1", n)
		}
		if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_rebranded' AND device_id = $1::uuid`, device); n != 1 {
			t.Errorf("rebrand left %d device_rebranded events, want 1", n)
		}
	})

	t.Run("the row that owned the abandoned brand is named, and no row's uid changes", func(t *testing.T) {
		// The contested-identity case: the phone in this row's slot carries
		// the other row's uid. The brand IS farm_uid, so the only correct
		// bookkeeping is on the phone — this row's uid goes onto it — and in
		// the record: the reply and the audit row name the row whose uid was
		// abandoned, because that row now points at no phone and somebody
		// has to retire it. Neither row's farm_uid moves; fusing the two
		// histories is what this endpoint exists to avoid.
		other := f.devices[1]
		phone.set(f.uids[1])
		status, out, raw := rebrand(map[string]any{"reason": "row 0 is the phone; row 1 was adopted from a clone serial"})
		if status != http.StatusOK {
			t.Fatalf("rebrand over another row's brand = %d, want 200:\n%s", status, raw)
		}
		if out["outcome"] != string(enroll.BrandWritten) || out["previous_uid"] != f.uids[1] ||
			out["previous_device_id"] != other {
			t.Errorf("reply = %s; want outcome written, previous_uid %s and previous_device_id %s",
				raw, f.uids[1], other)
		}
		if note, _ := out["note"].(string); !strings.Contains(note, other) || !strings.Contains(note, "retire") {
			t.Errorf("the note does not tell the operator to retire %s: %q", other, note)
		}
		if got := phone.get(); got != uid {
			t.Errorf("the phone carries %q after the rebrand, want the row's uid %s", got, uid)
		}
		var uid0, uid1 string
		f.scan(&uid0, `SELECT farm_uid FROM farm.devices WHERE id = $1::uuid`, device)
		f.scan(&uid1, `SELECT farm_uid FROM farm.devices WHERE id = $1::uuid`, other)
		if uid0 != uid || uid1 != f.uids[1] {
			t.Errorf("a rebrand changed farm.devices.farm_uid: row 0 %s (want %s), row 1 %s (want %s)",
				uid0, uid, uid1, f.uids[1])
		}
		if n := f.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'device.rebrand' AND subject = $1 AND detail->>'outcome' = 'written' AND detail->>'previous_uid' = $2 AND detail->>'previous_device_id' = $3`,
			"device:"+device, f.uids[1], other); n != 1 {
			t.Errorf("the audit row does not name the row whose brand was abandoned (%d rows)", n)
		}
	})

	t.Run("a phone already carrying the uid is left alone", func(t *testing.T) {
		status, out, raw := rebrand(map[string]any{"reason": "again"})
		if status != http.StatusOK || out["outcome"] != string(enroll.BrandAlready) {
			t.Fatalf("rebrand of a branded phone = %d %v, want 200 already:\n%s", status, out["outcome"], raw)
		}
	})

	t.Run("an unbranded phone is branded", func(t *testing.T) {
		phone.set("")
		status, out, raw := rebrand(map[string]any{"reason": "wiped"})
		if status != http.StatusOK || out["outcome"] != string(enroll.BrandWritten) || out["previous_uid"] != "" {
			t.Fatalf("rebrand of an unbranded phone = %d:\n%s", status, raw)
		}
		if got := phone.get(); got != uid {
			t.Errorf("the phone carries %q, want %s", got, uid)
		}
	})

	t.Run("a brand other than the one authorised against is refused untouched", func(t *testing.T) {
		other := "df-" + strings.Repeat("b", 32)
		phone.set(other)
		status, _, raw := rebrand(map[string]any{"reason": "x", "previous_uid": stranger})
		if status != http.StatusConflict {
			t.Fatalf("rebrand against the wrong previous uid = %d, want 409:\n%s", status, raw)
		}
		if got := phone.get(); got != other {
			t.Errorf("the refusal still wrote to the phone: %q", got)
		}
	})

	t.Run("a leased device is refused before the wire is touched", func(t *testing.T) {
		phone.set(stranger)
		leaseID, _ := f.lease(device)
		before := len(srv.RequestsTo(devpath))
		status, out, raw := rebrand(map[string]any{"reason": "x"})
		if status != http.StatusConflict {
			t.Fatalf("rebrand of a leased device = %d, want 409:\n%s", status, raw)
		}
		if got := errorDetail(t, out)["lease_id"]; got != leaseID {
			t.Errorf("the refusal names lease %v, want %s", got, leaseID)
		}
		if after := len(srv.RequestsTo(devpath)); after != before {
			t.Errorf("%d shell request(s) reached a leased device", after-before)
		}
		if got := phone.get(); got != stranger {
			t.Errorf("the phone changed under a live lease: %q", got)
		}
	})
}
