package api

// The operator surfaces for quarantine and park, driven through the real
// router against a real schema.
//
// Every test here needs DATABASE_URL pointing at a migrated scratch database
// and skips otherwise. Each seeds its own namespaced topology — one host, one
// hub, six slots, three on a ganged power domain and three on a per-port one
// — and removes it afterwards. The assertions read farm.v_fleet, because that
// view is what the dashboard, the bulk selector and the lease allocator's
// inputs all see: a quarantine the view does not show is a quarantine the
// operator cannot find.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// scratchServer is a Server wired the way farmd wires it, minus listeners,
// with an open authenticator that names every caller "tester".
func scratchServer(t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; this case needs a migrated scratch database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(&config.Config{APIAddr: "127.0.0.1:0", Component: "api-test"}, pool,
		WithAuthenticator(NewAllowAll(quiet, "tester")), WithLogger(quiet))
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return s, pool
}

// callJSON sends one JSON request through the full handler chain.
func callJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: response is not a JSON object: %v\n%s", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// num reads an integer field out of a decoded reply.
func num(t *testing.T, body map[string]any, key string) int64 {
	t.Helper()
	v, ok := body[key].(float64)
	if !ok {
		t.Fatalf("reply has no numeric %q: %v", key, body)
	}
	return int64(v)
}

// errDetail reads detail.<key> out of an error envelope.
func errDetail(body map[string]any, key string) any {
	env, _ := body["error"].(map[string]any)
	detail, _ := env["detail"].(map[string]any)
	return detail[key]
}

type fleetState struct {
	health     string
	adminState string
	quarantine *int64
}

func fleetStateOf(t *testing.T, pool *pgxpool.Pool, deviceID string) fleetState {
	t.Helper()
	var (
		st     fleetState
		health *string
	)
	if err := pool.QueryRow(context.Background(), `
SELECT f.health, f.admin_state, f.quarantine_id FROM farm.v_fleet f WHERE f.device_id = $1::uuid`, deviceID).
		Scan(&health, &st.adminState, &st.quarantine); err != nil {
		t.Fatalf("reading v_fleet for %s: %v", deviceID, err)
	}
	if health != nil {
		st.health = *health
	}
	return st
}

func expectState(t *testing.T, pool *pgxpool.Pool, step, deviceID, health, admin string, quarantine *int64) {
	t.Helper()
	st := fleetStateOf(t, pool, deviceID)
	if st.health != health || st.adminState != admin {
		t.Fatalf("%s: device %s is health=%q admin_state=%q, want %q/%q", step, deviceID, st.health, st.adminState, health, admin)
	}
	switch {
	case quarantine == nil && st.quarantine != nil:
		t.Fatalf("%s: device %s is shown under quarantine %d, want none", step, deviceID, *st.quarantine)
	case quarantine != nil && (st.quarantine == nil || *st.quarantine != *quarantine):
		got := "none"
		if st.quarantine != nil {
			got = fmt.Sprint(*st.quarantine)
		}
		t.Fatalf("%s: device %s is shown under quarantine %s, want %d", step, deviceID, got, *quarantine)
	}
}

func ptr(v int64) *int64 { return &v }

// TestOperatorQuarantineAtPowerDomainScope: a power-domain quarantine covers
// every slot wired to that switch and no other; a duplicate is refused with
// the id that exists; closing a row releases exactly the devices no other
// open row still covers.
//
// Falsify, one at a time: delete the power_domain arm from the close
// handler's NOT EXISTS (closing the hub row then frees all six); delete it
// from the close handler's covered set (closing the domain row then frees
// none); delete it from quarantineSweep (opening covers zero); put the old
// `q.slot_id IS NOT DISTINCT FROM s.id` arm back in farm.v_fleet (the view
// then shows no device under the domain quarantine).
func TestOperatorQuarantineAtPowerDomainScope(t *testing.T) {
	s, pool := scratchServer(t)
	fx := seedQuarantineTopology(t, pool)
	h := s.Handler()

	code, body := callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "power_domain", "power_domain_id": fx.pdA, "reason": "ganged switch browns out"})
	if code != http.StatusCreated {
		t.Fatalf("open power_domain: status %d, body %v", code, body)
	}
	pdQ := num(t, body, "quarantine_id")
	if got := num(t, body, "devices_covered"); got != 3 {
		t.Fatalf("a quarantine over a domain of three slots covered %d devices", got)
	}
	for i := 0; i < 3; i++ {
		expectState(t, pool, "domain open", fx.devices[i], "quarantined", "quarantined", ptr(pdQ))
	}
	for i := 3; i < 6; i++ {
		expectState(t, pool, "domain open", fx.devices[i], "healthy", "enabled", nil)
	}

	var audited int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM farm.audit_log
 WHERE action = 'quarantine.open' AND subject = $1 AND actor = 'tester'`, fmt.Sprintf("quarantine:%d", pdQ)).Scan(&audited); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if audited != 1 {
		t.Fatalf("opening a quarantine wrote %d audit rows, want 1", audited)
	}

	// A second open on the same domain names the row that exists.
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "power_domain", "power_domain_id": fx.pdA, "reason": "again"})
	if code != http.StatusConflict {
		t.Fatalf("duplicate open: status %d, want 409; body %v", code, body)
	}
	if got, _ := errDetail(body, "quarantine_id").(float64); int64(got) != pdQ {
		t.Fatalf("duplicate open named quarantine %v, want %d", errDetail(body, "quarantine_id"), pdQ)
	}

	// The subject must be the one the scope names, and must exist.
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "power_domain", "hub_id": fx.hub, "reason": "wrong field"})
	if code != http.StatusBadRequest {
		t.Fatalf("scope/subject mismatch: status %d, want 400; body %v", code, body)
	}
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "power_domain", "power_domain_id": 2147483647, "reason": "no such domain"})
	if code != http.StatusNotFound {
		t.Fatalf("unknown domain: status %d, want 404; body %v", code, body)
	}

	// Overlap: a device row on device 0 and a hub row over all six.
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "device", "device_id": fx.uids[0], "reason": "this one is also cracked"})
	if code != http.StatusCreated {
		t.Fatalf("open device: status %d, body %v", code, body)
	}
	devQ := num(t, body, "quarantine_id")
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "hub", "hub_id": fx.hub, "reason": "whole hub while we look"})
	if code != http.StatusCreated {
		t.Fatalf("open hub: status %d, body %v", code, body)
	}
	hubQ := num(t, body, "quarantine_id")
	if got := num(t, body, "devices_covered"); got != 6 {
		t.Fatalf("a hub quarantine covered %d devices, want 6", got)
	}

	// Closing the hub row frees only the three the domain row does not cover.
	code, body = callJSON(t, h, http.MethodPost, fmt.Sprintf("/api/v1/quarantines/%d/close", hubQ),
		map[string]any{"reason": "hub was fine"})
	if code != http.StatusOK {
		t.Fatalf("close hub: status %d, body %v", code, body)
	}
	if got := num(t, body, "devices_released"); got != 3 {
		t.Fatalf("closing the hub row released %d devices, want 3: the domain row still covers the other three", got)
	}
	// Device 0 is under two open rows, and the view names the narrowest: its
	// own device row. Devices 1 and 2 are under the domain row alone.
	expectState(t, pool, "hub closed", fx.devices[0], "quarantined", "quarantined", ptr(devQ))
	for i := 1; i < 3; i++ {
		expectState(t, pool, "hub closed", fx.devices[i], "quarantined", "quarantined", ptr(pdQ))
	}
	for i := 3; i < 6; i++ {
		expectState(t, pool, "hub closed", fx.devices[i], "unknown", "enabled", nil)
	}

	// Closing the domain row frees two: device 0 is still under its own row.
	code, body = callJSON(t, h, http.MethodPost, fmt.Sprintf("/api/v1/quarantines/%d/close", pdQ),
		map[string]any{"reason": "switch replaced"})
	if code != http.StatusOK {
		t.Fatalf("close domain: status %d, body %v", code, body)
	}
	if got := num(t, body, "devices_released"); got != 2 {
		t.Fatalf("closing the domain row released %d devices, want 2: device 0 has its own open quarantine", got)
	}
	expectState(t, pool, "domain closed", fx.devices[0], "quarantined", "quarantined", ptr(devQ))
	expectState(t, pool, "domain closed", fx.devices[1], "unknown", "enabled", nil)
	expectState(t, pool, "domain closed", fx.devices[2], "unknown", "enabled", nil)

	code, body = callJSON(t, h, http.MethodPost, fmt.Sprintf("/api/v1/quarantines/%d/close", devQ),
		map[string]any{"reason": "screen replaced"})
	if code != http.StatusOK || num(t, body, "devices_released") != 1 {
		t.Fatalf("close device: status %d, body %v", code, body)
	}
	expectState(t, pool, "all closed", fx.devices[0], "unknown", "enabled", nil)
}

// TestOperatorQuarantineAtSlotScope: a slot quarantine covers exactly that
// slot, and the neighbour on the same domain and hub is untouched.
//
// Falsify: change the slot arm of quarantineSweep to compare s.hub_id.
func TestOperatorQuarantineAtSlotScope(t *testing.T) {
	s, pool := scratchServer(t)
	fx := seedQuarantineTopology(t, pool)
	h := s.Handler()

	code, body := callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "slot", "slot_id": fx.slots[3], "reason": "port chews cables"})
	if code != http.StatusCreated {
		t.Fatalf("open slot: status %d, body %v", code, body)
	}
	slotQ := num(t, body, "quarantine_id")
	if got := num(t, body, "devices_covered"); got != 1 {
		t.Fatalf("a slot quarantine covered %d devices, want exactly 1", got)
	}
	expectState(t, pool, "slot open", fx.devices[3], "quarantined", "quarantined", ptr(slotQ))
	expectState(t, pool, "slot open", fx.devices[4], "healthy", "enabled", nil)
	expectState(t, pool, "slot open", fx.devices[0], "healthy", "enabled", nil)

	// And it is listed with its slot, so an operator can find it.
	code, body = callJSON(t, h, http.MethodGet, "/api/v1/recovery", nil)
	if code != http.StatusOK {
		t.Fatalf("recovery listing: status %d", code)
	}
	listed := false
	for _, raw := range body["quarantines"].([]any) {
		q := raw.(map[string]any)
		if int64(q["id"].(float64)) == slotQ {
			listed = true
			if got, _ := q["slot_id"].(float64); int64(got) != fx.slots[3] {
				t.Fatalf("the listing shows slot %v for quarantine %d, want %d", q["slot_id"], slotQ, fx.slots[3])
			}
		}
	}
	if !listed {
		t.Fatalf("quarantine %d is open and absent from GET /api/v1/recovery", slotQ)
	}

	code, body = callJSON(t, h, http.MethodPost, fmt.Sprintf("/api/v1/quarantines/%d/close", slotQ),
		map[string]any{"reason": "port replaced"})
	if code != http.StatusOK || num(t, body, "devices_released") != 1 {
		t.Fatalf("close slot: status %d, body %v", code, body)
	}
	expectState(t, pool, "slot closed", fx.devices[3], "unknown", "enabled", nil)
}

// TestParkAndUnparkThroughTheAPI: the two routes call farm.device_park and
// farm.device_unpark under the token's name, and the functions' refusals
// come back as the codes a client can act on. A human's park cannot be
// reversed by automation, and the API never passes p_auto=true.
//
// Falsify: map unique_violation to 200 in writeParkRefusal; or pass true as
// the fourth argument of farm.device_unpark in setParked (the automation
// check then refuses a human's own unpark with 403).
func TestParkAndUnparkThroughTheAPI(t *testing.T) {
	s, pool := scratchServer(t)
	fx := seedQuarantineTopology(t, pool)
	h := s.Handler()
	ctx := context.Background()
	dev, uid := fx.devices[5], fx.uids[5]

	code, body := callJSON(t, h, http.MethodPost, "/api/v1/devices/"+uid+"/park", map[string]any{})
	if code != http.StatusBadRequest {
		t.Fatalf("park without a reason: status %d, want 400; body %v", code, body)
	}
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/devices/df-00000000000000000000000000000000/park",
		map[string]any{"reason": "nobody home"})
	if code != http.StatusNotFound {
		t.Fatalf("park an unknown device: status %d, want 404; body %v", code, body)
	}

	code, body = callJSON(t, h, http.MethodPost, "/api/v1/devices/"+uid+"/park",
		map[string]any{"reason": "shelf is being rewired at 18:00"})
	if code != http.StatusOK {
		t.Fatalf("park: status %d, body %v", code, body)
	}
	if body["admin_state"] != "parked" || body["parked"] != true || body["opened_by"] != "tester" {
		t.Fatalf("park reply does not describe a park by tester: %v", body)
	}
	parkID := num(t, body, "park_id")
	expectState(t, pool, "parked", dev, "parked", "parked", nil)

	var audited int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM farm.audit_log
 WHERE action = 'device.park' AND subject = $1 AND actor = 'tester'`, "device:"+dev).Scan(&audited); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if audited != 1 {
		t.Fatalf("parking wrote %d audit rows under the operator's name, want exactly 1", audited)
	}

	code, body = callJSON(t, h, http.MethodPost, "/api/v1/devices/"+dev+"/park",
		map[string]any{"reason": "parking it again"})
	if code != http.StatusConflict {
		t.Fatalf("double park: status %d, want 409; body %v", code, body)
	}
	if got, _ := errDetail(body, "park_id").(float64); int64(got) != parkID {
		t.Fatalf("double park named park %v, want %d", errDetail(body, "park_id"), parkID)
	}

	// Automation may not reverse a human's park, whichever door it uses.
	var pgErr *pgconn.PgError
	_, err := pool.Exec(ctx, `SELECT farm.device_unpark($1::uuid, 'charge-limiter', NULL, true)`, dev)
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("automation reversing a human's park: err %v, want SQLSTATE 42501", err)
	}
	expectState(t, pool, "after automation was refused", dev, "parked", "parked", nil)

	code, body = callJSON(t, h, http.MethodPost, "/api/v1/devices/"+uid+"/unpark", map[string]any{"reason": "rewiring finished"})
	if code != http.StatusOK {
		t.Fatalf("unpark: status %d, body %v", code, body)
	}
	if body["admin_state"] != "enabled" || body["parked"] != false || body["closed_by"] != "tester" {
		t.Fatalf("unpark reply does not describe a closed park: %v", body)
	}
	expectState(t, pool, "unparked", dev, "unknown", "enabled", nil)

	code, body = callJSON(t, h, http.MethodPost, "/api/v1/devices/"+uid+"/unpark", map[string]any{})
	if code != http.StatusConflict {
		t.Fatalf("unpark twice: status %d, want 409; body %v", code, body)
	}

	// A quarantined device is somebody else's decision; parking would
	// overwrite the reason it is out of service.
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/quarantines",
		map[string]any{"scope": "device", "device_id": uid, "reason": "cracked"})
	if code != http.StatusCreated {
		t.Fatalf("open device quarantine: status %d, body %v", code, body)
	}
	qid := num(t, body, "quarantine_id")
	code, body = callJSON(t, h, http.MethodPost, "/api/v1/devices/"+uid+"/park", map[string]any{"reason": "shelving it"})
	if code != http.StatusConflict {
		t.Fatalf("park a quarantined device: status %d, want 409; body %v", code, body)
	}
	if code, body = callJSON(t, h, http.MethodPost, fmt.Sprintf("/api/v1/quarantines/%d/close", qid),
		map[string]any{"reason": "done"}); code != http.StatusOK {
		t.Fatalf("close: status %d, body %v", code, body)
	}
}

// quarantineTopology is one host with one hub and six slots: slots 0-2 share
// a ganged power domain, slots 3-5 are on a per-port one. Every device is
// healthy and enabled.
type quarantineTopology struct {
	rack, host string
	pdA, pdB   int64
	hub        int64
	slots      []int64
	devices    []string // uuids
	uids       []string // farm_uids
}

func seedQuarantineTopology(t *testing.T, pool *pgxpool.Pool) quarantineTopology {
	t.Helper()
	ctx := context.Background()

	// Alphanumeric only: the id also becomes the first ltree label of every
	// slot's topo_path, and ltree labels take neither hyphen nor dot.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	fx := quarantineTopology{rack: "raq" + suffix, host: "haq" + suffix}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("fixture: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO farm.racks (id) VALUES ($1)`, fx.rack)
	exec(`INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ($1, $2, '127.0.0.1:5037')`, fx.host, fx.rack)
	exec(`INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING`)

	var controller int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3) RETURNING id`, fx.host).Scan(&controller); err != nil {
		t.Fatalf("fixture controller: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO farm.power_domains (host_id, kind, control) VALUES ($1, 'ganged', 'uhubctl') RETURNING id`, fx.host).Scan(&fx.pdA); err != nil {
		t.Fatalf("fixture power domain A: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO farm.power_domains (host_id, kind, control) VALUES ($1, 'per_port', 'uhubctl') RETURNING id`, fx.host).Scan(&fx.pdB); err != nil {
		t.Fatalf("fixture power domain B: %v", err)
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
VALUES ($1, $2, '3-1', 7, true) RETURNING id`, fx.host, controller).Scan(&fx.hub); err != nil {
		t.Fatalf("fixture hub: %v", err)
	}

	for i := 0; i < 6; i++ {
		port := i + 1
		pd := fx.pdA
		if i >= 3 {
			pd = fx.pdB
		}
		var slot int64
		if err := tx.QueryRow(ctx, `
INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path, topo_path, rack_slot)
VALUES ($1, $2, $3, $4, $5, $6::ltree, $7) RETURNING id`,
			fx.host, fx.hub, pd, port, fmt.Sprintf("3-1.%d", port),
			fmt.Sprintf("%s.c3.p3_1.p3_1_%d", fx.host, port), fmt.Sprintf("AQ-U1-H1-P%d", port)).Scan(&slot); err != nil {
			t.Fatalf("fixture slot %d: %v", port, err)
		}
		var dev, uid string
		if err := tx.QueryRow(ctx, `
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
VALUES ('df-' || md5($1), $2, 'default', $3, $4, 'Pixel Test') RETURNING id::text, farm_uid`,
			fmt.Sprintf("%s-%d", fx.host, port), fmt.Sprintf("SER%s%d", suffix, port), fx.host, slot).Scan(&dev, &uid); err != nil {
			t.Fatalf("fixture device %d: %v", port, err)
		}
		exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
VALUES ($1::uuid, $2, $3, 'device', 'healthy')`, dev, fx.host, slot)
		fx.slots = append(fx.slots, slot)
		fx.devices = append(fx.devices, dev)
		fx.uids = append(fx.uids, uid)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	t.Cleanup(func() {
		// Devices first (their runtime, park and quarantine rows cascade), then
		// the slots, which RESTRICT the host and the power domains, then the
		// host, which cascades to everything else it owns.
		for _, q := range []string{
			`DELETE FROM farm.devices WHERE host_id = $1`,
			`DELETE FROM farm.slots WHERE host_id = $1`,
			`DELETE FROM farm.hosts WHERE id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, fx.host); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
		if _, err := pool.Exec(ctx, `DELETE FROM farm.racks WHERE id = $1`, fx.rack); err != nil {
			t.Logf("cleanup rack: %v", err)
		}
	})
	return fx
}
