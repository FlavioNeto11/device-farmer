package node

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hostSeq keeps each test's host apart in the shared scratch database.
var hostSeq atomic.Int64

// fenceFixture is one host with a hub, a pool, and however many slotted
// devices a test asks for, inserted directly: the enrolment path is not
// what is under test here.
type fenceFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	hostID string
	poolID string
	hubID  int64
	n      int
}

func newFenceFixture(t *testing.T) *fenceFixture {
	t.Helper()
	pool := requireDB(t)
	seq := hostSeq.Add(1)
	f := &fenceFixture{
		t: t, pool: pool, ctx: context.Background(),
		hostID: fmt.Sprintf("fh%03d", seq),
		poolID: fmt.Sprintf("fp%03d", seq),
	}
	f.exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	f.exec(`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, '127.0.0.1:5037')`, f.hostID)
	f.scan(&f.hubID,
		`INSERT INTO farm.hubs (host_id, usb_path, port_count) VALUES ($1, '3-1', 8) RETURNING id`, f.hostID)
	return f
}

// device seats a device in a fresh slot of this host's hub and pins its
// fence floor. It returns the slot's adb_devpath, which is what the proxy keys
// on: the physical position, never the serial.
func (f *fenceFixture) device(floor int64) string {
	f.t.Helper()
	f.n++
	var slotID int64
	var devpath string
	f.pool.QueryRow(f.ctx, `
INSERT INTO farm.slots (host_id, hub_id, port_number, usb_path, topo_path)
VALUES ($1, $2, $3, $4, $5::ltree)
RETURNING id, adb_devpath`,
		f.hostID, f.hubID, f.n, fmt.Sprintf("3-1.%d", f.n),
		fmt.Sprintf("%s.p3_1_%d", f.hostID, f.n)).Scan(&slotID, &devpath)
	f.exec(`
INSERT INTO farm.devices (farm_uid, pool_id, host_id, current_slot_id, fence_floor, model)
VALUES ($1, $2, $3, $4, $5, 'Test Device')`,
		fmt.Sprintf("df-%032x", hostSeq.Load()*100+int64(f.n)), f.poolID, f.hostID, slotID, floor)
	return devpath
}

func (f *fenceFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func (f *fenceFixture) scan(dest any, sql string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(dest); err != nil {
		f.t.Fatalf("query %.60q: %v", sql, err)
	}
}

func (f *fenceFixture) source() hostFloors {
	return hostFloors{pool: f.pool, hostID: f.hostID, timeout: 10 * time.Second}
}

// TestHostFloorsReadsEveryPositionOnTheHost: the snapshot is keyed by
// adb_devpath and carries farm.devices.fence_floor as written, for this host
// only. A device with no slot and a device on another host are absent — not
// zero, absent — because the proxy treats a missing position as unknown and
// refuses it retryably, never as fenced.
func TestHostFloorsReadsEveryPositionOnTheHost(t *testing.T) {
	f := newFenceFixture(t)
	a := f.device(41207)
	b := f.device(7)
	c := f.device(0)

	// A device that belongs to this host but sits in no slot.
	f.exec(`INSERT INTO farm.devices (farm_uid, pool_id, host_id, fence_floor, model)
VALUES ($1, $2, $3, 999, 'Unslotted')`, fmt.Sprintf("df-%032x", hostSeq.Load()*100+50), f.poolID, f.hostID)
	// A neighbour's device, in a neighbour's slot.
	other := newFenceFixture(t)
	other.device(555)

	snap, err := f.source().Floors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{a: 41207, b: 7, c: 0}
	if len(snap.Floors) != len(want) {
		t.Errorf("got %d positions %v, want %d", len(snap.Floors), snap.Floors, len(want))
	}
	for dp, floor := range want {
		if got, ok := snap.Floors[dp]; !ok || got != floor {
			t.Errorf("floor for %s = %d (present %v), want %d", dp, got, ok, floor)
		}
	}
	for dp := range snap.Floors {
		if !strings.HasPrefix(dp, "usb:3-1.") {
			t.Errorf("snapshot keyed by %q, want a devpath", dp)
		}
	}

	// The floor moves and the next read shows it: the proxy's knowledge is
	// whatever the database says, one poll later.
	f.exec(`UPDATE farm.devices SET fence_floor = 41300 WHERE host_id = $1 AND fence_floor = 41207`, f.hostID)
	snap, err = f.source().Floors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Floors[a] != 41300 {
		t.Errorf("after the floor rose, read %d for %s, want 41300", snap.Floors[a], a)
	}
}

// TestHostFloorsIsEmptyForAHostWithNoSlots: an empty host is an empty
// snapshot, not an error. An error here would count as blindness and, past
// the staleness budget, refuse every new connection to a host that simply has
// nothing plugged in yet.
func TestHostFloorsIsEmptyForAHostWithNoSlots(t *testing.T) {
	f := newFenceFixture(t)
	snap, err := f.source().Floors(context.Background())
	if err != nil {
		t.Fatalf("an empty host is not an error: %v", err)
	}
	if snap.Floors == nil || len(snap.Floors) != 0 {
		t.Errorf("got %v, want an empty, non-nil map", snap.Floors)
	}

	// A host that does not exist at all reads the same way.
	none := hostFloors{pool: f.pool, hostID: "no-such-host", timeout: 10 * time.Second}
	if snap, err := none.Floors(context.Background()); err != nil || len(snap.Floors) != 0 {
		t.Errorf("an unknown host: %v, %v", snap.Floors, err)
	}
}

// TestRegisterHostAdvertisesTheProxyWhenItIsOn is the row-level half of the
// endpoint decision: farm.hosts.adb_endpoint carries the proxy's address when
// the proxy is configured and the adb server's when it is not. Every reader of
// that column — the jobrunner, the watchdog, the recovery ladder, the API —
// dials whatever is written there.
func TestRegisterHostAdvertisesTheProxyWhenItIsOn(t *testing.T) {
	pool := requireDB(t)
	pki := newTestPKI(t)
	ctx := context.Background()

	hostID := fmt.Sprintf("fh%03d", hostSeq.Add(1))
	endpoint := func() string {
		var got string
		if err := pool.QueryRow(ctx, `SELECT adb_endpoint FROM farm.hosts WHERE id = $1`, hostID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	on, _ := newTestAgent(t, Config{Pool: pool, HostID: hostID, ADBEndpoint: "127.0.0.1:5037",
		Fence: FenceConfig{
			CertFile: pki.certFile, KeyFile: pki.keyFile, CAFile: pki.caFile,
			Listen: ":5038", Advertise: "h01.lab.example:5038",
		}})
	if err := on.registerHost(ctx, true, "test"); err != nil {
		t.Fatal(err)
	}
	if got := endpoint(); got != "h01.lab.example:5038" {
		t.Errorf("with the proxy on, farm.hosts.adb_endpoint = %q, want the proxy's address", got)
	}

	off, _ := newTestAgent(t, Config{Pool: pool, HostID: hostID, ADBEndpoint: "127.0.0.1:5037"})
	if err := off.registerHost(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := endpoint(); got != "127.0.0.1:5037" {
		t.Errorf("with the proxy off, farm.hosts.adb_endpoint = %q, want the adb server", got)
	}
}
