package watchdog

// The SQL-backed half of this package's tests runs against a scratch database
// created, migrated and dropped by TestMain, on the same terms as
// internal/reaper's:
//
//   - Without DATABASE_URL every SQL-backed test SKIPS, so `go test ./...` is
//     green on a laptop with no Postgres. The wire-level and pure tests in
//     battery_test.go and swell_test.go still execute everywhere.
//
//   - The database is created fresh per run and dropped afterwards, FORCE
//     included, so a leaked connection cannot leak a database.
//
// The tests here do not call t.Parallel(): the swell checker publishes into
// package-level gauges that the parallel tests in swell_test.go also touch,
// and Go runs every sequential top-level test to completion before it resumes
// a parallel one, which is the ordering these assertions rely on.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver for goose.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/migrations"
)

var testPool *pgxpool.Pool

// setupLockKey serialises scratch-database creation across packages; the
// migration set creates cluster-wide roles behind an IF NOT EXISTS check, and
// two suites migrating at once can both see one missing. Same key as the
// other suites, on purpose: they share the admin database.
const setupLockKey int64 = 0x64665f74657374 // "df_test"

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) (code int) {
	base := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if base == "" {
		return m.Run()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watchdog tests: open DATABASE_URL: %v\n", err)
		return 1
	}
	defer admin.Close()
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "watchdog tests: connect to DATABASE_URL: %v\n", err)
		return 1
	}

	name := fmt.Sprintf("df_watchdog_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		fmt.Fprintf(os.Stderr, "watchdog tests: DATABASE_URL must be a postgres:// URL\n")
		return 1
	}
	u.Path = "/" + name
	dsn := u.String()

	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "watchdog tests: take setup lock: %v\n", err)
		return 1
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		//nolint:errcheck // best effort; the process is about to exit anyway
		admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		fmt.Fprintf(os.Stderr, "watchdog tests: create scratch database (the role needs CREATEDB): %v\n", err)
		return 1
	}
	defer func() {
		if testPool != nil {
			closed := make(chan struct{})
			go func() { testPool.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(15 * time.Second):
				fmt.Fprintln(os.Stderr, "watchdog tests: a connection was still checked out at teardown; forcing the drop")
			}
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if _, err := admin.ExecContext(dctx,
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "watchdog tests: drop scratch database %s: %v\n", name, err)
		}
	}()

	migrateErr := migrateScratch(ctx, dsn)
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "watchdog tests: release setup lock: %v\n", err)
	}
	if migrateErr != nil {
		fmt.Fprintf(os.Stderr, "watchdog tests: %v\n", migrateErr)
		return 1
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watchdog tests: connect to scratch database: %v\n", err)
		return 1
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "watchdog tests: ping scratch database: %v\n", err)
		return 1
	}
	testPool = pool
	return m.Run()
}

// migrateScratch applies the EMBEDDED migration set, the bytes the shipped
// binary carries, so these tests cannot pass against a schema no deployment
// ever gets.
func migrateScratch(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open scratch database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	goose.SetBaseFS(migrations.Goose())
	goose.SetLogger(quietGooseLogger{})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("migrate scratch database: %w", err)
	}
	return nil
}

type quietGooseLogger struct{}

func (quietGooseLogger) Printf(string, ...any) {}
func (quietGooseLogger) Fatalf(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "goose: "+format+"\n", v...)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL is not set; skipping the SQL-backed watchdog tests")
	}
	return testPool
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// rig is one host with one hub, into which devices are added one per slot.
// Every id is derived from the test name so tests share the database without
// sharing rows, and the scratch database is never truncated between them.
type rig struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context
	host string
	hub  int64
	n    int

	// count is the rig's own farm_battery_anomalies: each checker built by
	// checker() sums into a tally private to this rig, so the sequential DB
	// tests cannot read each other's findings through the package gauge.
	count prometheus.Gauge
}

type rigDevice struct {
	ID       string
	SlotID   int64
	RackSlot string
	Devpath  string
}

func newRig(t *testing.T, pool *pgxpool.Pool) *rig {
	t.Helper()
	r := &rig{t: t, pool: pool, ctx: context.Background()}
	r.host = strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	if len(r.host) > 40 {
		r.host = r.host[len(r.host)-40:]
	}
	r.exec(`INSERT INTO farm.pools (id) VALUES ('p') ON CONFLICT (id) DO NOTHING`)
	r.exec(`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, '127.0.0.1:5037')`, r.host)
	r.scan(&r.hub, `INSERT INTO farm.hubs (host_id, usb_path, port_count) VALUES ($1, '1-1', 16) RETURNING id`, r.host)
	return r
}

func (r *rig) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, q, args...); err != nil {
		r.t.Fatalf("exec %q: %v", firstLine(q), err)
	}
}

func (r *rig) scan(dst any, q string, args ...any) {
	r.t.Helper()
	if err := r.pool.QueryRow(r.ctx, q, args...).Scan(dst); err != nil {
		r.t.Fatalf("query %q: %v", firstLine(q), err)
	}
}

// device adds one device in the next slot, with the runtime row the reader
// expects and a charge gate of 'on'. releasedAgo, when non-zero, backdates
// farm.devices.last_released_at by that much, which is how a test says "a
// job ended on this device that long ago".
func (r *rig) device(releasedAgo time.Duration) rigDevice {
	r.t.Helper()
	r.n++
	d := rigDevice{
		RackSlot: fmt.Sprintf("R1-U1-H1.1-P%d", r.n),
		Devpath:  fmt.Sprintf("usb:1-1.%d", r.n),
	}
	topo := strings.NewReplacer("-", "_", ".", "_").Replace(r.host) + fmt.Sprintf(".p1_%d", r.n)
	r.scan(&d.SlotID, `
INSERT INTO farm.slots (host_id, hub_id, port_number, usb_path, topo_path, rack_slot)
VALUES ($1, $2, $3, $4, $5::ltree, $6) RETURNING id`,
		r.host, r.hub, r.n, fmt.Sprintf("1-1.%d", r.n), topo, d.RackSlot)

	var released *float64
	if releasedAgo > 0 {
		s := releasedAgo.Seconds()
		released = &s
	}
	r.scan(&d.ID, `
INSERT INTO farm.devices (farm_uid, pool_id, host_id, current_slot_id, model, last_released_at)
VALUES ('df-' || md5($1::text), 'p', $2, $3, 'Test Device',
        CASE WHEN $4::float8 IS NULL THEN NULL
             ELSE now() - make_interval(secs => $4::float8) END)
RETURNING id::text`, r.host+fmt.Sprint(r.n), r.host, d.SlotID, released)

	r.exec(`
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, charge_gate, last_seen_at)
VALUES ($1::uuid, $2, $3, 'device', 'healthy', 'on', now())`, d.ID, r.host, d.SlotID)
	return d
}

// readingsAgo inserts one history row per value, the oldest first, spaced a
// minute apart and ending `endAgo` before the server's now(). The timestamps
// are computed by the server: no test sends an instant it computed locally.
func (r *rig) readingsAgo(d rigDevice, endAgo time.Duration, temps, pcts []*int32) {
	r.t.Helper()
	n := max(len(temps), len(pcts))
	for i := 0; i < n; i++ {
		var temp, pct *int32
		if i < len(temps) {
			temp = temps[i]
		}
		if i < len(pcts) {
			pct = pcts[i]
		}
		ago := endAgo + time.Duration(n-1-i)*time.Minute
		r.exec(`
INSERT INTO farm.battery_readings (device_id, at, pct, temp_dc)
VALUES ($1::uuid, now() - make_interval(secs => $2::float8), $3, $4)`,
			d.ID, ago.Seconds(), pct, temp)
	}
}

func (r *rig) poller() *batteryPoller {
	return &batteryPoller{
		pool: r.pool, hostID: r.host, callTimeout: 5 * time.Second,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (r *rig) checker() *swellChecker {
	c := newSwellChecker(r.pool, r.host, "watchdog-test:"+r.host, BatteryThresholds{},
		5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r.count == nil {
		r.count = prometheus.NewGauge(prometheus.GaugeOpts{Name: "rig_battery_anomalies"})
	}
	c.tally = newAnomalyTally(r.count)
	return c
}

func (r *rig) historyCount(d rigDevice) int {
	var n int
	r.scan(&n, `SELECT count(*) FROM farm.battery_readings WHERE device_id = $1::uuid`, d.ID)
	return n
}

type anomalyRow struct {
	Kind, RackSlot, Devpath, Host, Unit, Actor string
	Value, Threshold                           float64
}

func (r *rig) anomalies(d rigDevice) []anomalyRow {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `
SELECT detail->>'kind', detail->>'rack_slot', detail->>'devpath', detail->>'host',
       detail->>'unit', actor, (detail->>'value')::float8, (detail->>'threshold')::float8
  FROM farm.events
 WHERE kind = 'battery_anomaly' AND device_id = $1::uuid
 ORDER BY id`, d.ID)
	if err != nil {
		r.t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var out []anomalyRow
	for rows.Next() {
		var a anomalyRow
		if err := rows.Scan(&a.Kind, &a.RackSlot, &a.Devpath, &a.Host, &a.Unit, &a.Actor,
			&a.Value, &a.Threshold); err != nil {
			r.t.Fatalf("scan event: %v", err)
		}
		out = append(out, a)
	}
	return out
}

func firstLine(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		return q[:i]
	}
	return q
}

// ---------------------------------------------------------------------------
// The write path
// ---------------------------------------------------------------------------

// TestWriteAppendsHistoryInTheSameStatement: the current value and the
// history row land together, a partial answer erases nothing on the runtime
// row and is recorded as partial in the history, and a device that vanished
// between the listing and the write costs nothing — not the batch, and not a
// dangling history row.
func TestWriteAppendsHistoryInTheSameStatement(t *testing.T) {
	pool := requireDB(t)
	r := newRig(t, pool)
	a, b := r.device(0), r.device(0)
	p := r.poller()

	if err := p.write(r.ctx, []batteryReading{
		{DeviceID: a.ID, Pct: ptr(87), TempDC: ptr(293)},
		{DeviceID: b.ID, TempDC: ptr(305)},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	var pct, temp *int32
	r.scan(&pct, `SELECT battery_pct FROM farm.device_runtime WHERE device_id = $1::uuid`, a.ID)
	r.scan(&temp, `SELECT battery_temp_dc FROM farm.device_runtime WHERE device_id = $1::uuid`, a.ID)
	if pct == nil || *pct != 87 || temp == nil || *temp != 293 {
		t.Fatalf("runtime row for a = %s/%s, want 87/293", showPtr(pct), showPtr(temp))
	}
	if n := r.historyCount(a); n != 1 {
		t.Fatalf("a has %d history rows after one write, want 1", n)
	}
	if n := r.historyCount(b); n != 1 {
		t.Fatalf("b has %d history rows after one write, want 1", n)
	}

	// A second cycle: a answers with a temperature only. The level it
	// reported a minute ago stays on the runtime row; the history says
	// honestly that this reading carried no level.
	if err := p.write(r.ctx, []batteryReading{{DeviceID: a.ID, TempDC: ptr(301)}}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	r.scan(&pct, `SELECT battery_pct FROM farm.device_runtime WHERE device_id = $1::uuid`, a.ID)
	r.scan(&temp, `SELECT battery_temp_dc FROM farm.device_runtime WHERE device_id = $1::uuid`, a.ID)
	if pct == nil || *pct != 87 || temp == nil || *temp != 301 {
		t.Fatalf("runtime row for a after a partial answer = %s/%s, want 87/301", showPtr(pct), showPtr(temp))
	}
	var histPct, histTemp *int32
	r.scan(&histPct, `SELECT pct FROM farm.battery_readings WHERE device_id = $1::uuid ORDER BY at DESC LIMIT 1`, a.ID)
	r.scan(&histTemp, `SELECT temp_dc FROM farm.battery_readings WHERE device_id = $1::uuid ORDER BY at DESC LIMIT 1`, a.ID)
	if histPct != nil || histTemp == nil || *histTemp != 301 {
		t.Fatalf("newest history row for a = %s/%s, want <none>/301", showPtr(histPct), showPtr(histTemp))
	}
	if n := r.historyCount(a); n != 2 {
		t.Fatalf("a has %d history rows after two writes, want 2", n)
	}

	// A device that was listed and then deleted before the write. Its
	// runtime row cascaded away, so the update matches nothing and the
	// history insert is never attempted against a foreign key that no
	// longer holds — and b's reading in the same batch still lands.
	gone := r.device(0)
	r.exec(`DELETE FROM farm.devices WHERE id = $1::uuid`, gone.ID)
	if err := p.write(r.ctx, []batteryReading{
		{DeviceID: gone.ID, Pct: ptr(50), TempDC: ptr(300)},
		{DeviceID: b.ID, Pct: ptr(64), TempDC: ptr(310)},
	}); err != nil {
		t.Fatalf("a batch carrying a deleted device failed outright: %v", err)
	}
	if n := r.historyCount(b); n != 2 {
		t.Fatalf("b has %d history rows, want 2: the batch was lost with the deleted device", n)
	}
	if n := r.historyCount(gone); n != 0 {
		t.Fatalf("the deleted device has %d history rows, want 0", n)
	}
}

// TestPruneKeepsTheWindow: the prune deletes what is older than the
// function's default keep and nothing younger, and stamps lastPrune so the
// next hour's cycles skip it.
func TestPruneKeepsTheWindow(t *testing.T) {
	pool := requireDB(t)
	r := newRig(t, pool)
	d := r.device(0)

	for _, ago := range []string{"8 days", "7 days 1 minute", "6 days 23 hours", "1 hour", "0 seconds"} {
		r.exec(`INSERT INTO farm.battery_readings (device_id, at, pct) VALUES ($1::uuid, now() - $2::interval, 50)`,
			d.ID, ago)
	}

	p := r.poller()
	if !p.lastPrune.IsZero() {
		t.Fatal("a fresh poller must prune on its first cycle")
	}
	p.prune(r.ctx)
	if p.lastPrune.IsZero() {
		t.Fatal("prune did not record when it ran")
	}
	if n := r.historyCount(d); n != 3 {
		t.Fatalf("%d rows survived the prune, want the 3 inside seven days", n)
	}
}

// ---------------------------------------------------------------------------
// The checker
// ---------------------------------------------------------------------------

// TestSwellCheckRaisesOnceAndClears is the whole detection loop against real
// rows: a hot series produces exactly one ledger row naming the rack slot, a
// second check an instant later produces no second row, the gauges follow
// the finding up and back down, and after the TTL the same finding is
// announced again.
func TestSwellCheckRaisesOnceAndClears(t *testing.T) {
	pool := requireDB(t)
	r := newRig(t, pool)
	hot, quiet := r.device(0), r.device(0)

	// The live proof's series: five readings a minute apart climbing 3.0 C
	// a minute, the newest one just now. And a neighbour that is fine.
	r.readingsAgo(hot, 0, vals(300, 330, 360, 390, 420), nil)
	r.readingsAgo(quiet, 0, flat(300, 5), flat(80, 5))

	c := r.checker()
	clock := time.Now()
	c.now = func() time.Time { return clock }

	if err := c.check(r.ctx); err != nil {
		t.Fatalf("check: %v", err)
	}

	got := r.anomalies(hot)
	if len(got) != 1 {
		t.Fatalf("hot device has %d battery_anomaly events, want 1: %+v", len(got), got)
	}
	a := got[0]
	if a.Kind != SwellKindTempRise || a.RackSlot != hot.RackSlot || a.Devpath != hot.Devpath ||
		a.Host != r.host || a.Unit != "dC/min" || a.Threshold != DefaultBatteryTempRiseDCPerMin {
		t.Fatalf("event detail = %+v, want temp_rise at %s/%s on %s", a, hot.RackSlot, hot.Devpath, r.host)
	}
	if a.Value < 29.9 || a.Value > 30.1 {
		t.Fatalf("event value = %.2f dC/min, want 30", a.Value)
	}
	if a.Actor != "watchdog-test:"+r.host {
		t.Fatalf("event actor = %q, want the component name", a.Actor)
	}
	if n := len(r.anomalies(quiet)); n != 0 {
		t.Fatalf("the quiet neighbour has %d events, want 0", n)
	}

	riseGauge := batteryAnomalyGauge.WithLabelValues(r.host, hot.RackSlot, SwellKindTempRise)
	if v := gaugeValue(t, riseGauge); v != 1 {
		t.Fatalf("farm_battery_anomaly for the hot device = %v, want 1", v)
	}
	if v := gaugeValue(t, r.count); v != 1 {
		t.Fatalf("farm_battery_anomalies = %v, want 1", v)
	}

	// The next cycle, a minute later: still hot, still one row.
	clock = clock.Add(time.Minute)
	if err := c.check(r.ctx); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if n := len(r.anomalies(hot)); n != 1 {
		t.Fatalf("a persisting anomaly was raised %d times inside the TTL, want 1", n)
	}
	if v := gaugeValue(t, riseGauge); v != 1 {
		t.Fatalf("gauge after the second check = %v, want still 1", v)
	}

	// The phone was pulled: its readings stop, and once they age out of the
	// window the finding clears to a visible zero.
	r.exec(`DELETE FROM farm.battery_readings WHERE device_id = $1::uuid`, hot.ID)
	if err := c.check(r.ctx); err != nil {
		t.Fatalf("third check: %v", err)
	}
	if v := gaugeValue(t, riseGauge); v != 0 {
		t.Fatalf("gauge after the evidence aged out = %v, want 0", v)
	}
	if v := gaugeValue(t, r.count); v != 0 {
		t.Fatalf("farm_battery_anomalies after clearing = %v, want 0", v)
	}

	// An hour and more later the same phone is hot again. That is news.
	clock = clock.Add(swellRaiseTTL + time.Minute)
	r.readingsAgo(hot, 0, vals(300, 330, 360, 390, 420), nil)
	if err := c.check(r.ctx); err != nil {
		t.Fatalf("fourth check: %v", err)
	}
	if n := len(r.anomalies(hot)); n != 2 {
		t.Fatalf("a finding past its TTL was not raised again: %d events", n)
	}
}

// TestSwellNamesAPositionForAnUnlabelledSlot: farm.slots.rack_slot is
// nullable, and farm.events refuses a battery_anomaly that names no position
// (migrations/00016). The detector therefore derives one for a slot nobody
// has labelled, the way topo.Labeler does for a host with no rack coordinates
// — host, hub token, port — so the ledger row still lands and the page still
// says where to walk.
func TestSwellNamesAPositionForAnUnlabelledSlot(t *testing.T) {
	pool := requireDB(t)
	r := newRig(t, pool)
	d := r.device(0)
	r.exec(`UPDATE farm.slots SET rack_slot = NULL WHERE id = $1`, d.SlotID)
	r.readingsAgo(d, 0, flat(470, 5), nil)

	c := r.checker()
	if err := c.check(r.ctx); err != nil {
		t.Fatalf("check: %v", err)
	}

	// The rig's hub sits at USB path 1-1 and this is its first port.
	want := r.host + "-H1.1-P1"
	got := r.anomalies(d)
	if len(got) != 1 || got[0].Kind != SwellKindTempMax {
		t.Fatalf("unlabelled device has %+v, want exactly one temp_max finding: "+
			"the ledger refused a row without a position and nothing derived one", got)
	}
	if got[0].RackSlot != want {
		t.Fatalf("event rack_slot = %q for an unlabelled slot, want the derived %q", got[0].RackSlot, want)
	}
	if v := gaugeValue(t, batteryAnomalyGauge.WithLabelValues(r.host, want, SwellKindTempMax)); v != 1 {
		t.Fatalf("farm_battery_anomaly{rack_slot=%q} = %v, want 1", want, v)
	}
}

// TestSwellDrainNeedsAnIdleDevice: the idle predicate lives in the SQL, on
// farm.devices.last_released_at, and it is what keeps a job that just ended
// from being read as a failing cell. The same falling level is judged for a
// device that has been idle all window and ignored for one whose job ended
// five minutes ago.
func TestSwellDrainNeedsAnIdleDevice(t *testing.T) {
	pool := requireDB(t)
	r := newRig(t, pool)
	idle := r.device(2 * time.Hour)
	busy := r.device(5 * time.Minute)

	r.readingsAgo(idle, 0, nil, ramp(80, 70, 30))
	r.readingsAgo(busy, 0, nil, ramp(80, 70, 30))

	c := r.checker()
	if err := c.check(r.ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	got := r.anomalies(idle)
	if len(got) != 1 || got[0].Kind != SwellKindDrain {
		t.Fatalf("the idle device has %+v, want exactly one drain finding", got)
	}
	if n := len(r.anomalies(busy)); n != 0 {
		t.Fatalf("a device whose job ended inside the window was judged for drain: %d events", n)
	}

	// The gate says off: the charge limiter asked for this.
	r.exec(`UPDATE farm.device_runtime SET charge_gate = 'off' WHERE device_id = $1::uuid`, idle.ID)
	gated := r.device(2 * time.Hour)
	r.exec(`UPDATE farm.device_runtime SET charge_gate = 'off' WHERE device_id = $1::uuid`, gated.ID)
	r.readingsAgo(gated, 0, nil, ramp(80, 70, 30))
	if err := c.check(r.ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	if n := len(r.anomalies(gated)); n != 0 {
		t.Fatalf("a device whose port is gated off was judged for drain: %d events", n)
	}
}
