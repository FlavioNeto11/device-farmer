package recovery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The fidelity tests pin the TEXT of coveredByQuarantine. This pins its
// EFFECT, against the real schema: a power-domain quarantine makes every device
// wired to that switch — and no other — invisible to the candidate query, a
// slot quarantine hides exactly one, and closing a row brings its devices back
// without touching what another row still covers. Nothing here changes a
// device's health: the rows alone must be enough, because that is what the
// NOT EXISTS in candidates() is for.
//
// It needs DATABASE_URL pointing at a migrated scratch database, and skips
// otherwise. The fixture is namespaced and removed afterwards.
//
// Falsify: delete the power_domain arm from coveredByQuarantine; the three
// devices on the ganged domain stay candidates while their quarantine is open.
func TestQuarantineScopesHideExactlyTheirDevicesFromTheLadder(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; this case needs a migrated scratch database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	fx := seedLadderTopology(t, pool)

	l := &Ladder{
		cfg: Config{Pool: pool, CallTimeout: 10 * time.Second, Debounce: time.Millisecond, Batch: 1000},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	visible := func(step string) []string {
		t.Helper()
		cands, err := l.candidates(ctx)
		if err != nil {
			t.Fatalf("%s: candidates: %v", step, err)
		}
		var ids []string
		for _, c := range cands {
			if slices.Contains(fx.devices, c.DeviceID) {
				ids = append(ids, c.DeviceID)
			}
		}
		slices.Sort(ids)
		return ids
	}
	expect := func(step string, idx ...int) {
		t.Helper()
		want := make([]string, 0, len(idx))
		for _, i := range idx {
			want = append(want, fx.devices[i])
		}
		slices.Sort(want)
		if got := visible(step); !slices.Equal(got, want) {
			t.Fatalf("%s: the ladder sees %v, want %v (fixture order %v)", step, got, want, fx.devices)
		}
	}

	expect("before any quarantine", 0, 1, 2, 3, 4, 5)

	var pdQ int64
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.quarantines (scope, power_domain_id, host_id, reason, auto)
VALUES ('power_domain', $1, $2, 'ladder_db_test: ganged switch', false)
RETURNING id`, fx.pdA, fx.host).Scan(&pdQ); err != nil {
		t.Fatalf("open power-domain quarantine: %v", err)
	}
	expect("with the ganged domain quarantined", 3, 4, 5)

	var slotQ int64
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.quarantines (scope, slot_id, host_id, reason, auto)
VALUES ('slot', $1, $2, 'ladder_db_test: one bad port', false)
RETURNING id`, fx.slots[3], fx.host).Scan(&slotQ); err != nil {
		t.Fatalf("open slot quarantine: %v", err)
	}
	expect("with one slot quarantined as well", 4, 5)

	if _, err := pool.Exec(ctx,
		`UPDATE farm.quarantines SET closed_at = now(), closed_by = 'ladder_db_test' WHERE id = $1`, pdQ); err != nil {
		t.Fatalf("close power-domain quarantine: %v", err)
	}
	expect("after closing the power-domain row, with the slot row still open", 0, 1, 2, 4, 5)

	if _, err := pool.Exec(ctx,
		`UPDATE farm.quarantines SET closed_at = now(), closed_by = 'ladder_db_test' WHERE id = $1`, slotQ); err != nil {
		t.Fatalf("close slot quarantine: %v", err)
	}
	expect("after closing both", 0, 1, 2, 3, 4, 5)
}

// ladderTopology is one host with one hub and six slots: slots 0-2 share a
// ganged power domain, slots 3-5 are on a per-port one. Every device is
// offline, an hour past the debounce, so all six are candidates until a
// quarantine says otherwise.
type ladderTopology struct {
	rack, host string
	pdA, pdB   int64
	slots      []int64
	devices    []string
}

func seedLadderTopology(t *testing.T, pool *pgxpool.Pool) ladderTopology {
	t.Helper()
	ctx := context.Background()

	// Alphanumeric only: the id also becomes the first ltree label of every
	// slot's topo_path, and ltree labels take neither hyphen nor dot.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	fx := ladderTopology{rack: "rrq" + suffix, host: "hrq" + suffix}

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

	var controller, hub int64
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
VALUES ($1, $2, '3-1', 7, true) RETURNING id`, fx.host, controller).Scan(&hub); err != nil {
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
			fx.host, hub, pd, port, fmt.Sprintf("3-1.%d", port),
			fmt.Sprintf("%s.c3.p3_1.p3_1_%d", fx.host, port), fmt.Sprintf("RQ-U1-H1-P%d", port)).Scan(&slot); err != nil {
			t.Fatalf("fixture slot %d: %v", port, err)
		}
		var dev string
		if err := tx.QueryRow(ctx, `
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
VALUES ('df-' || md5($1), $2, 'default', $3, $4, 'Pixel Test') RETURNING id::text`,
			fmt.Sprintf("%s-%d", fx.host, port), fmt.Sprintf("SER%s%d", suffix, port), fx.host, slot).Scan(&dev); err != nil {
			t.Fatalf("fixture device %d: %v", port, err)
		}
		exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, health_since)
VALUES ($1::uuid, $2, $3, 'offline', 'offline', now() - interval '1 hour')`, dev, fx.host, slot)
		fx.slots = append(fx.slots, slot)
		fx.devices = append(fx.devices, dev)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	t.Cleanup(func() {
		// Devices first (their runtime and quarantine rows cascade), then the
		// slots, which RESTRICT the host and the power domains, then the host,
		// which cascades to everything else it owns.
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
