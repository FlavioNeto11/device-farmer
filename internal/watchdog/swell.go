package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// Swell: the early-detection half of battery safety
// ---------------------------------------------------------------------------
//
// battery.go observes a level. This file watches the level MOVE, which is the
// only battery question that is about safety rather than capacity.
//
// The physics is in docs/siting.md §2 and it is the reason this file exists:
// a clean-agent suppression system does not stop a lithium event. Novec 1230
// at 8.5 vol% failed to suppress a cell array and failed to stop propagation
// through it, and propagation continued in pure nitrogen, because thermal
// runaway is the cell's own chemistry decomposing and carries its own
// oxidiser. The mitigations that work are physical — containment, spacing,
// charge limiting — and one of them is early detection, because runaway
// announces itself before it is visible: the case warms, and it warms FAST.
// A phone at 38 C is unremarkable; a phone that was 30 C four minutes ago is
// being heated from inside. Absolute temperature is therefore the second
// trigger here and rate is the first.
//
// The other precursor a fleet can see from software is a cell that cannot
// hold charge: a handset sitting idle on a powered port whose level is
// falling. That is not a runaway, it is a cell that is failing, and it is
// the one that swells in a drawer.
//
// # What this file does with a finding
//
// It writes a farm.events row an operator will find afterwards, sets a gauge
// an alert fires on, and logs a line with a rack_slot in it. Nothing else.
// It does not park the device, gate its charger, quarantine it, or — the
// package rule, unchanged — read or touch a lease. A hot cell is a reason
// for a human to walk to a shelf now, and the fastest way to get a human to
// a shelf is a page that names the shelf, not a control loop that decides
// something on the human's behalf and pages later.
//
// The only lease-adjacent facts read here are farm.devices.current_lease_id
// and .last_released_at, folded into one boolean: the drain rule applies only
// to a device that is IDLE, because a test can legitimately outrun a charger.
// Both are columns on the identity table — the first a trigger-maintained
// pointer — which this role may SELECT; the lease table itself remains
// revoked from it.

const (
	// swellWindow is how much history a check reads per device. Long enough
	// for the drain rule to see a real slope through one-percent steps;
	// short enough that a device pulled from the rack drops out of the
	// gauge within the hour.
	swellWindow = 30 * time.Minute

	// swellRiseWindow is the trailing span the temperature slope is fitted
	// over. Five minutes, not thirty: a runaway precursor is a matter of
	// minutes, and a slope averaged over half an hour of flat readings would
	// dilute a sharp final climb into nothing. At the reader's one-minute
	// cadence this is five or six points, which is enough for a fit and few
	// enough that one is not still waiting when the case is hot.
	swellRiseWindow = 5 * time.Minute

	// swellRiseMinSpan is the least time the fitted temperature samples must
	// cover. Two points one minute apart can be a sensor stutter; three
	// points across two minutes are a trend.
	swellRiseMinSpan = 2 * time.Minute

	// swellDrainMinSpan is the least time the fitted charge samples must
	// cover. Charge moves in whole percentage points, so a drain of fifteen
	// an hour is under four points in fifteen minutes; fitted over less than
	// that the quantisation is the signal.
	swellDrainMinSpan = 15 * time.Minute

	// swellMinSamples is the fewest points any slope is fitted through.
	swellMinSamples = 3

	// swellRaiseTTL is how long a raised anomaly is not raised again for the
	// same device. The gauge is re-evaluated every cycle regardless; this
	// only governs the farm.events row and the log line, so that a phone
	// that stays hot for an hour is one row in the ledger and not sixty.
	swellRaiseTTL = time.Hour

	// Defaults for BatteryThresholds. internal/config carries the same three
	// values as the environment's defaults and swell_test.go pins them
	// equal; the reasoning is written once, there.
	DefaultBatteryTempRiseDCPerMin = 20
	DefaultBatteryTempMaxDC        = 450
	DefaultBatteryDrainPctPerHour  = 15
)

// The closed set of anomaly kinds. They label a gauge and name a runbook
// section, so they are constants rather than strings assembled at the site.
const (
	SwellKindTempRise = "temp_rise"
	SwellKindTempMax  = "temp_max"
	SwellKindDrain    = "drain"
)

// BatteryThresholds is the policy the detector flags on, in the schema's
// units: decidegrees Celsius, and percentage points.
type BatteryThresholds struct {
	// TempRiseDCPerMin flags a temperature climbing faster than this over
	// swellRiseWindow, at any absolute value.
	TempRiseDCPerMin int
	// TempMaxDC flags the newest reading being above this.
	TempMaxDC int
	// DrainPctPerHour flags an idle device on a port whose charge gate is
	// not off losing charge faster than this over swellWindow.
	DrainPctPerHour int
}

func (t *BatteryThresholds) applyDefaults() {
	if t.TempRiseDCPerMin <= 0 {
		t.TempRiseDCPerMin = DefaultBatteryTempRiseDCPerMin
	}
	if t.TempMaxDC <= 0 {
		t.TempMaxDC = DefaultBatteryTempMaxDC
	}
	if t.DrainPctPerHour <= 0 {
		t.DrainPctPerHour = DefaultBatteryDrainPctPerHour
	}
}

// batterySample is one farm.battery_readings row. Either value may be absent,
// exactly as it may be in the table: a nil is a value the device did not
// report, never a zero.
type batterySample struct {
	At     time.Time
	Pct    *int32
	TempDC *int32
}

// swellSeries is everything the detector is told about one device: where it
// is, whether it is idle, whether its port is meant to be powering it, and
// what it has said about itself lately. Samples need not be ordered; every
// decision below is order-independent.
type swellSeries struct {
	DeviceID string
	SlotID   int64
	Devpath  string
	Host     string
	RackSlot string

	// Idle is true when the device holds no lease now AND none ended inside
	// the window, so every sample was taken while nothing was running on
	// it. A job that finished ten minutes ago drained the battery
	// legitimately for the twenty minutes before that.
	Idle bool
	// ChargeGate is farm.device_runtime.charge_gate, or "unknown" when it
	// has never been written. Only "off" exempts a device from the drain
	// rule: a port nobody has gated is a port that is powering its phone.
	ChargeGate string

	Samples []batterySample
}

// swellAnomaly is one finding. Value and Threshold are in Unit, which is what
// makes a farm.events row readable by a human a week later without the code.
type swellAnomaly struct {
	Kind      string
	Value     float64
	Threshold float64
	Unit      string
}

// detectSwell applies the three rules to one device's recent history. Pure,
// so that swell_test.go can drive it over synthetic series without a
// database: this is the function whose false negatives matter.
func detectSwell(s swellSeries, th BatteryThresholds) []swellAnomaly {
	var out []swellAnomaly

	// Temperature. The newest reading anchors both rules: the ceiling is
	// tested on it, and the rise window ends at it rather than at the wall
	// clock, so a device that stopped answering three minutes ago is judged
	// on the minutes it did answer for.
	var newest *batterySample
	for i := range s.Samples {
		p := &s.Samples[i]
		if p.TempDC == nil {
			continue
		}
		if newest == nil || p.At.After(newest.At) {
			newest = p
		}
	}
	if newest != nil {
		if int(*newest.TempDC) > th.TempMaxDC {
			out = append(out, swellAnomaly{
				Kind: SwellKindTempMax, Unit: "dC",
				Value: float64(*newest.TempDC), Threshold: float64(th.TempMaxDC),
			})
		}
		var xs, ys []float64
		origin := newest.At.Add(-swellRiseWindow)
		for _, p := range s.Samples {
			if p.TempDC == nil || p.At.Before(origin) {
				continue
			}
			xs = append(xs, p.At.Sub(origin).Minutes())
			ys = append(ys, float64(*p.TempDC))
		}
		if rate, ok := slopePerMinute(xs, ys, swellRiseMinSpan); ok && rate > float64(th.TempRiseDCPerMin) {
			out = append(out, swellAnomaly{
				Kind: SwellKindTempRise, Unit: "dC/min",
				Value: rate, Threshold: float64(th.TempRiseDCPerMin),
			})
		}
	}

	// Charge. Judged only where a falling level cannot be explained: nothing
	// is running on the device and nothing has turned its port off.
	if s.Idle && s.ChargeGate != "off" {
		var xs, ys []float64
		var origin time.Time
		for _, p := range s.Samples {
			if p.Pct != nil && (origin.IsZero() || p.At.Before(origin)) {
				origin = p.At
			}
		}
		for _, p := range s.Samples {
			if p.Pct == nil {
				continue
			}
			xs = append(xs, p.At.Sub(origin).Minutes())
			ys = append(ys, float64(*p.Pct))
		}
		if rate, ok := slopePerMinute(xs, ys, swellDrainMinSpan); ok {
			// Per hour, and positive when falling: the number an operator
			// reads is "how fast it is losing charge", not a signed slope.
			if drain := -rate * 60; drain > float64(th.DrainPctPerHour) {
				out = append(out, swellAnomaly{
					Kind: SwellKindDrain, Unit: "pct/h",
					Value: drain, Threshold: float64(th.DrainPctPerHour),
				})
			}
		}
	}
	return out
}

// slopePerMinute fits y = a + b·x by least squares and returns b, with x in
// minutes. It reports false when the points are too few or cover too little
// time to mean anything.
//
// A fit rather than "last minus first over the span" because a single wild
// sample — a thermistor that stuttered once — moves an endpoint difference
// by its whole error and moves a fit by a fraction of it. The reader already
// drops impossible values; this handles the merely wrong ones.
func slopePerMinute(xs, ys []float64, minSpan time.Duration) (float64, bool) {
	if len(xs) < swellMinSamples || len(xs) != len(ys) {
		return 0, false
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs[1:] {
		lo, hi = min(lo, x), max(hi, x)
	}
	if hi-lo < minSpan.Minutes() {
		return 0, false
	}

	n := float64(len(xs))
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/n, sy/n
	var sxy, sxx float64
	for i := range xs {
		dx := xs[i] - mx
		sxy += dx * (ys[i] - my)
		sxx += dx * dx
	}
	if sxx == 0 {
		return 0, false
	}
	return sxy / sxx, true
}

// ---------------------------------------------------------------------------
// The checker: history in, events and gauges out
// ---------------------------------------------------------------------------

// anomalyKey is one (device, kind) pair, the unit the ledger and the gauge
// are deduplicated on.
type anomalyKey struct {
	DeviceID string
	Kind     string
}

// anomalyLabels is what the gauge child for a key was set with, kept so the
// child can be zeroed when the anomaly clears. A vec child left at 1 after
// the phone was pulled is a page that never resolves.
type anomalyLabels struct {
	Host     string
	RackSlot string
	Kind     string
}

// anomalyTally is the process-wide sum behind farm_battery_anomalies.
//
// One process can run several checkers — one per host when FARM_HOST_ID is
// unset, and in the demo — and a plain gauge that each checker Set() to its
// own count reads whichever host published last: a hot phone on h01 is
// erased by h02 reporting zero a moment later. That is the overwrite
// publishTargets in battery.go avoids with a per-host label, and the count
// cannot take that label, because it has to exist before any host has been
// seen (see batteryAnomaliesGauge). So each checker owns one entry here and
// the gauge is always the sum. The live demo is where this was caught: two
// watchdogs, one finding, a count of zero.
type anomalyTally struct {
	mu    sync.Mutex
	by    map[*swellChecker]int
	gauge prometheus.Gauge
}

func newAnomalyTally(gauge prometheus.Gauge) *anomalyTally {
	return &anomalyTally{by: make(map[*swellChecker]int), gauge: gauge}
}

// swellTally is the tally behind the exported gauge. Tests build their own
// around a private gauge so they cannot see each other's findings.
var swellTally = newAnomalyTally(batteryAnomaliesGauge)

// set records one checker's current count and republishes the sum.
func (t *anomalyTally) set(c *swellChecker, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.by[c] = n
	t.publishLocked()
}

// forget withdraws a checker that has stopped, so a replacement in the same
// process does not double-count under a key nobody will update again.
func (t *anomalyTally) forget(c *swellChecker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.by, c)
	t.publishLocked()
}

func (t *anomalyTally) publishLocked() {
	total := 0
	for _, n := range t.by {
		total += n
	}
	t.gauge.Set(float64(total))
}

// swellChecker runs detectSwell over every device in its scope once per
// battery cycle.
//
// Its memory is in-process rather than a farm.events lookup, and that is a
// choice worth stating. The process that raised an anomaly is the one that
// re-evaluates it every minute — one poller per host in the per-node shape,
// or one for the fleet — so nothing else can raise the same finding; the
// gauge needs the same map anyway to know which children to zero; and the
// only thing a restart costs is one repeated ledger row for a phone that is
// STILL hot, which is the one duplicate an operator is glad of. A lookup
// would buy restart-proof deduplication at a query per finding, to suppress
// the row that a fresh process most ought to write.
type swellChecker struct {
	pool        *pgxpool.Pool
	hostID      string
	actor       string
	th          BatteryThresholds
	callTimeout time.Duration
	log         *slog.Logger

	// now feeds the raise TTL only. It is never written to the database and
	// never compared with a sample's At — those are the server's clock.
	now func() time.Time

	raised map[anomalyKey]time.Time
	active map[anomalyKey]anomalyLabels

	// tally is where this checker's count is summed with every other
	// checker's in the process; see anomalyTally.
	tally *anomalyTally
}

func newSwellChecker(pool *pgxpool.Pool, hostID, actor string, th BatteryThresholds,
	callTimeout time.Duration, log *slog.Logger) *swellChecker {

	th.applyDefaults()
	return &swellChecker{
		pool: pool, hostID: hostID, actor: actor, th: th,
		callTimeout: callTimeout, log: log, now: time.Now,
		raised: make(map[anomalyKey]time.Time),
		active: make(map[anomalyKey]anomalyLabels),
		tally:  swellTally,
	}
}

// stop withdraws this checker's share of the count. The poller that owns it
// calls this when its loop exits; the per-position children are left as
// they are, because a checker that has stopped knows nothing new about the
// phones it was watching and a zero here would claim that it did.
func (c *swellChecker) stop() { c.tally.forget(c) }

// check reads the window, runs the detector per device, raises what is new,
// and republishes the gauges from scratch.
func (c *swellChecker) check(ctx context.Context) error {
	all, err := c.series(ctx)
	if err != nil {
		return err
	}

	found := make(map[anomalyKey]anomalyLabels)
	for _, s := range all {
		for _, a := range detectSwell(s, c.th) {
			key := anomalyKey{DeviceID: s.DeviceID, Kind: a.Kind}
			found[key] = anomalyLabels{Host: s.Host, RackSlot: s.RackSlot, Kind: a.Kind}

			if last, ok := c.raised[key]; ok && c.now().Sub(last) < swellRaiseTTL {
				continue
			}
			if err := c.raise(ctx, s, a); err != nil {
				if ctx.Err() != nil {
					return err
				}
				// The gauge still goes up below: a ledger write that failed
				// must not keep a page from firing. The row is retried next
				// cycle because raised[key] is not set.
				c.log.Warn("could not record a battery anomaly in farm.events",
					"host", s.Host, "rack_slot", s.RackSlot, "kind", a.Kind, "err", err)
				continue
			}
			c.raised[key] = c.now()
		}
	}

	for key, at := range c.raised {
		if c.now().Sub(at) >= swellRaiseTTL {
			delete(c.raised, key)
		}
	}
	c.publish(found)
	return nil
}

// series reads the last swellWindow of readings for every device in scope,
// with the position and the two facts the drain rule needs.
//
// The position is farm.slots.rack_slot, and when nobody has labelled the
// slot it is derived the way topo.Labeler derives one for a host with no
// rack coordinates: the host, the hub's USB path as its token, and the port
// — "h07-H3.1.4-P5". A page without a position is a page nobody can act on,
// and migrations/00016 makes the ledger refuse the row; deriving it here
// is what keeps an unlabelled rack from being an unwatched one.
//
// Flat rows grouped in Go rather than array_agg per device: a rackful of
// devices is under two thousand rows a minute, and a nullable smallint scans
// into a *int32 without an array codec in the way.
func (c *swellChecker) series(ctx context.Context) ([]swellSeries, error) {
	const q = `
SELECT d.id::text, s.id, s.adb_devpath, h.id,
       COALESCE(NULLIF(s.rack_slot, ''),
                h.id || '-H' || replace(hb.usb_path, '-', '.') || '-P' || s.port_number),
       (d.current_lease_id IS NULL
          AND (d.last_released_at IS NULL
               OR d.last_released_at < now() - make_interval(secs => $2::float8))),
       COALESCE(r.charge_gate, 'unknown'),
       b.at, b.pct::int, b.temp_dc::int
  FROM farm.battery_readings b
  JOIN farm.devices        d ON d.id = b.device_id
  JOIN farm.device_runtime r ON r.device_id = d.id
  JOIN farm.slots          s ON s.id = d.current_slot_id
  JOIN farm.hubs           hb ON hb.id = s.hub_id
  JOIN farm.hosts          h ON h.id = s.host_id
 WHERE b.at >= now() - make_interval(secs => $2::float8)
   AND ($1::text = '' OR h.id = $1::text)
   AND d.admin_state <> 'retired'
 ORDER BY d.id, b.at`

	cctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	rows, err := c.pool.Query(cctx, q, c.hostID, swellWindow.Seconds())
	if err != nil {
		return nil, fmt.Errorf("watchdog: read battery history: %w", err)
	}
	defer rows.Close()

	var out []swellSeries
	for rows.Next() {
		var (
			s      swellSeries
			sample batterySample
		)
		if err := rows.Scan(&s.DeviceID, &s.SlotID, &s.Devpath, &s.Host, &s.RackSlot,
			&s.Idle, &s.ChargeGate, &sample.At, &sample.Pct, &sample.TempDC); err != nil {
			return nil, fmt.Errorf("watchdog: read battery history scan: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].DeviceID == s.DeviceID {
			out[n-1].Samples = append(out[n-1].Samples, sample)
			continue
		}
		s.Samples = []batterySample{sample}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("watchdog: read battery history: %w", err)
	}
	return out, nil
}

// raise writes the ledger row and the log line for one new finding.
//
// rack_slot is in the detail even though slot_id is on the row, because the
// row is read by a person at a rack and a person walks to a label, not to a
// join. The detail is self-describing for the same reason: value, threshold
// and unit together, so the row still means something after the thresholds
// have been retuned.
func (c *swellChecker) raise(ctx context.Context, s swellSeries, a swellAnomaly) error {
	detail, err := json.Marshal(map[string]any{
		"kind": a.Kind, "value": a.Value, "threshold": a.Threshold, "unit": a.Unit,
		"rack_slot": s.RackSlot, "devpath": s.Devpath, "host": s.Host,
	})
	if err != nil {
		return fmt.Errorf("watchdog: encode battery anomaly: %w", err)
	}

	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, actor, detail)
VALUES ('battery_anomaly', $1::uuid, $2::bigint, $3::text, $4::jsonb)`

	cctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	if _, err := c.pool.Exec(cctx, q, s.DeviceID, s.SlotID, c.actor, detail); err != nil {
		return fmt.Errorf("watchdog: record battery anomaly: %w", err)
	}
	batteryAnomalyEvents.WithLabelValues(a.Kind).Inc()
	c.log.Warn("battery anomaly: walk to the rack slot and unplug the device",
		"host", s.Host, "rack_slot", s.RackSlot, "devpath", s.Devpath, "device", s.DeviceID,
		"kind", a.Kind, "value", a.Value, "threshold", a.Threshold, "unit", a.Unit)
	return nil
}

// publish sets every current finding to 1, every finding that has cleared
// since the last check to 0, and this checker's share of the count.
func (c *swellChecker) publish(found map[anomalyKey]anomalyLabels) {
	for key, l := range c.active {
		if _, still := found[key]; !still {
			batteryAnomalyGauge.WithLabelValues(l.Host, l.RackSlot, l.Kind).Set(0)
			delete(c.active, key)
		}
	}
	for key, l := range found {
		batteryAnomalyGauge.WithLabelValues(l.Host, l.RackSlot, l.Kind).Set(1)
		c.active[key] = l
	}
	c.tally.set(c, len(found))
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	// The series an operator is paged on. Per position, because the page
	// has to say where to walk; a vec with per-device labels has no child
	// until the first anomaly, which is what batteryAnomaliesGauge is for.
	batteryAnomalyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Name: "battery_anomaly",
		Help: "1 while the swell detector's last check flagged this position for this kind " +
			"(temp_rise, temp_max, drain), 0 once it cleared. Walk to rack_slot. Nothing in " +
			"the control plane acts on this; a human does.",
	}, []string{"host", "rack_slot", "kind"})

	// A plain gauge exists from the first scrape at 0, so the alerting rule
	// has a series to be armed on before any phone has ever run hot. The
	// vec above cannot offer that — its children are positions, and a
	// pre-seeded placeholder position would be a rack_slot nobody can walk
	// to. It is written only through swellTally, which sums every checker
	// in the process; no checker sets it directly.
	batteryAnomaliesGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Name: "battery_anomalies",
		Help: "Number of (device, kind) battery anomalies the last swell checks found across " +
			"every host this process watches. Present at 0 from the first scrape; the " +
			"per-position detail is farm_battery_anomaly.",
	})

	batteryAnomalyEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_anomaly_events_total",
		Help: "farm.events rows of kind battery_anomaly written, by kind. One per (device, kind) " +
			"per hour while the anomaly persists.",
	}, []string{"kind"})

	swellCheckErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "swell_check_errors_total",
		Help: "Battery cycles whose swell check could not read the history. The gauges keep " +
			"their previous values across a failed check.",
	})
)

// swellCollectors returns this detector's metrics, for Collectors. The
// per-kind event counters are seeded so a dashboard reads zero rather than
// nothing before the first finding.
func swellCollectors() []prometheus.Collector {
	for _, kind := range []string{SwellKindTempRise, SwellKindTempMax, SwellKindDrain} {
		batteryAnomalyEvents.WithLabelValues(kind)
	}
	return []prometheus.Collector{
		batteryAnomalyGauge, batteryAnomaliesGauge, batteryAnomalyEvents, swellCheckErrors,
	}
}
