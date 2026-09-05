package watchdog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// ---------------------------------------------------------------------------
// The detector, over synthetic series
// ---------------------------------------------------------------------------

// t0 is an arbitrary epoch for synthetic samples. Every decision in
// detectSwell is relative, so its value is irrelevant; what matters is that
// no test compares a sample against the wall clock.
var t0 = time.Date(2026, 9, 5, 3, 0, 0, 0, time.UTC)

// series builds samples one minute apart. A nil in temps or pcts is a value
// the device did not report, exactly as it would arrive from the table.
func series(temps, pcts []*int32) []batterySample {
	n := max(len(temps), len(pcts))
	out := make([]batterySample, n)
	for i := range out {
		out[i].At = t0.Add(time.Duration(i) * time.Minute)
		if i < len(temps) {
			out[i].TempDC = temps[i]
		}
		if i < len(pcts) {
			out[i].Pct = pcts[i]
		}
	}
	return out
}

func vals(vs ...int32) []*int32 {
	out := make([]*int32, len(vs))
	for i := range vs {
		out[i] = &vs[i]
	}
	return out
}

// flat is n samples of the same value.
func flat(v int32, n int) []*int32 {
	out := make([]*int32, n)
	for i := range out {
		out[i] = &v
	}
	return out
}

// ramp is n samples going from a to b in equal steps.
func ramp(a, b int32, n int) []*int32 {
	out := make([]*int32, n)
	for i := range out {
		v := a + (b-a)*int32(i)/int32(n-1)
		out[i] = &v
	}
	return out
}

func kinds(as []swellAnomaly) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Kind)
	}
	return out
}

func find(as []swellAnomaly, kind string) (swellAnomaly, bool) {
	for _, a := range as {
		if a.Kind == kind {
			return a, true
		}
	}
	return swellAnomaly{}, false
}

func TestDetectSwell(t *testing.T) {
	t.Parallel()

	var th BatteryThresholds
	th.applyDefaults()

	// A device sitting idle on a powered port, which is the only shape the
	// drain rule judges. Cases that are about temperature use it too, so
	// that a temperature case cannot pass by accident of the drain rule
	// being switched off.
	idle := swellSeries{Idle: true, ChargeGate: "on"}

	cases := []struct {
		name      string
		s         swellSeries
		samples   []batterySample
		wantKinds []string
		// wantValue, when set, is checked on the first kind to one decimal.
		wantValue float64
	}{
		{
			name:    "a quiet phone",
			s:       idle,
			samples: series(flat(300, 30), flat(80, 30)),
		},
		{
			// The series the live proof inserts by hand: five readings a
			// minute apart, climbing 3.0 C a minute. Well under the ceiling
			// and unmistakably on its way to it.
			name:      "climbing three degrees a minute",
			s:         idle,
			samples:   series(vals(300, 330, 360, 390, 420), nil),
			wantKinds: []string{SwellKindTempRise},
			wantValue: 30,
		},
		{
			// A phone warming under a test does this. Half a degree a
			// minute is a CPU, not a cell.
			name:    "warming up under a test",
			s:       idle,
			samples: series(ramp(300, 325, 6), nil),
		},
		{
			name:      "hot and steady",
			s:         idle,
			samples:   series(flat(460, 10), nil),
			wantKinds: []string{SwellKindTempMax},
			wantValue: 460,
		},
		{
			name:      "hot and climbing",
			s:         idle,
			samples:   series(vals(430, 455, 480), nil),
			wantKinds: []string{SwellKindTempMax, SwellKindTempRise},
		},
		{
			// The ceiling is a strict "above": a reading AT it is the top
			// of the charging band, not past it.
			name:    "exactly at the ceiling",
			s:       idle,
			samples: series(flat(450, 5), nil),
		},
		{
			// The rise window ends at the newest reading and is five
			// minutes long. A climb that finished twenty-five minutes ago
			// and has been flat since is over, whatever it looked like.
			name:    "a climb that ended long ago",
			s:       idle,
			samples: series(append(vals(300, 330, 360, 390, 420), flat(420, 25)...), nil),
		},
		{
			// A slope needs three points across two minutes. Two readings
			// a minute apart could be one thermistor stutter.
			name:    "two readings a minute apart",
			s:       idle,
			samples: series(vals(300, 340), nil),
		},
		{
			// One wild reading at the end of a flat run moves a fitted
			// slope by a fraction of its error. Endpoint arithmetic would
			// read this as 8 dC/min; the fit reads it as under 6, and
			// neither is a page — but the margin is the point.
			name:    "a single stutter at the end of a flat run",
			s:       idle,
			samples: series(vals(300, 300, 300, 300, 300, 340), nil),
		},
		{
			// A device that stopped answering three minutes ago is judged
			// on the minutes it did answer for: the window is anchored to
			// its newest reading, not to the wall clock.
			name: "a climb in the readings before the device went silent",
			s:    idle,
			samples: []batterySample{
				{At: t0, TempDC: ptr(300)},
				{At: t0.Add(1 * time.Minute), TempDC: ptr(340)},
				{At: t0.Add(2 * time.Minute), TempDC: ptr(380)},
				{At: t0.Add(3 * time.Minute), TempDC: ptr(420)},
			},
			wantKinds: []string{SwellKindTempRise},
			wantValue: 40,
		},
		{
			// Samples arrive from the table ordered, but nothing here may
			// depend on it: every decision is order-independent.
			name:      "the same climb, shuffled",
			s:         idle,
			samples:   shuffled(series(vals(300, 330, 360, 390, 420), nil)),
			wantKinds: []string{SwellKindTempRise},
			wantValue: 30,
		},
		{
			name:    "no temperature at all",
			s:       idle,
			samples: series(nil, flat(50, 10)),
		},

		// Charge.
		{
			// Ten points in twenty-nine minutes on a powered, idle port is
			// about twenty an hour: a cell that cannot hold what it is
			// given. The fit through whole-percent steps reads 20.2.
			name:      "an idle phone on a powered port losing charge",
			s:         idle,
			samples:   series(nil, ramp(80, 70, 30)),
			wantKinds: []string{SwellKindDrain},
			wantValue: 20.2,
		},
		{
			// The same drop while a job holds the device is the job's
			// doing. A test can outrun a charger, and the rule must not
			// page a tenant's workload.
			name:    "the same drop mid-job",
			s:       swellSeries{Idle: false, ChargeGate: "on"},
			samples: series(nil, ramp(80, 70, 30)),
		},
		{
			// The charge limiter turned the port off on purpose; a falling
			// level is what it asked for.
			name:    "the same drop with the port gated off",
			s:       swellSeries{Idle: true, ChargeGate: "off"},
			samples: series(nil, ramp(80, 70, 30)),
		},
		{
			// A gate nobody has written is a port that is powering its
			// phone. Only an explicit "off" exempts the device.
			name:      "the same drop with the gate never written",
			s:         swellSeries{Idle: true, ChargeGate: "unknown"},
			samples:   series(nil, ramp(80, 70, 30)),
			wantKinds: []string{SwellKindDrain},
		},
		{
			name:    "an idle phone charging",
			s:       idle,
			samples: series(nil, ramp(40, 60, 30)),
		},
		{
			// Three points in half an hour is six an hour: a phone whose
			// charger is merely slow, or a level that stepped down once.
			name:    "an idle phone losing three points in half an hour",
			s:       idle,
			samples: series(nil, ramp(80, 77, 30)),
		},
		{
			// A steep drop over too short a span. Ten points in nine
			// minutes would be sixty-six an hour, but nine minutes of
			// one-percent steps is not enough to fit through.
			name:    "a steep drop over too short a span",
			s:       idle,
			samples: series(nil, ramp(80, 70, 10)),
		},
		{
			// Both at once: the shape of a cell that is failing and hot.
			name:      "hot and draining",
			s:         idle,
			samples:   series(flat(470, 30), ramp(80, 60, 30)),
			wantKinds: []string{SwellKindTempMax, SwellKindDrain},
		},
		{
			name: "nothing at all",
			s:    idle,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := c.s
			s.Samples = c.samples
			got := detectSwell(s, th)

			if gk := kinds(got); !sameSet(gk, c.wantKinds) {
				t.Fatalf("kinds = %v, want %v (findings %+v)", gk, c.wantKinds, got)
			}
			if c.wantValue != 0 {
				a, _ := find(got, c.wantKinds[0])
				if math.Abs(a.Value-c.wantValue) > 0.05 {
					t.Errorf("%s value = %.2f, want %.1f", a.Kind, a.Value, c.wantValue)
				}
			}
			// Every finding is self-describing: a farm.events row must
			// still mean something after the thresholds have been retuned.
			for _, a := range got {
				if a.Unit == "" || a.Threshold == 0 {
					t.Errorf("finding %+v carries no unit or threshold", a)
				}
				if a.Value <= a.Threshold {
					t.Errorf("finding %+v was raised without exceeding its threshold", a)
				}
			}
		})
	}
}

// TestDetectSwellHonoursTheThresholds: the knobs are the policy, and the
// detector must read them rather than the defaults it was written against.
func TestDetectSwellHonoursTheThresholds(t *testing.T) {
	t.Parallel()

	s := swellSeries{Idle: true, ChargeGate: "on",
		Samples: series(vals(300, 310, 320, 330, 340), ramp(80, 75, 30))}

	strict := BatteryThresholds{TempRiseDCPerMin: 5, TempMaxDC: 320, DrainPctPerHour: 5}
	got := kinds(detectSwell(s, strict))
	if !sameSet(got, []string{SwellKindTempRise, SwellKindTempMax, SwellKindDrain}) {
		t.Fatalf("strict thresholds found %v, want all three kinds", got)
	}

	lax := BatteryThresholds{TempRiseDCPerMin: 100, TempMaxDC: 1000, DrainPctPerHour: 100}
	if got := detectSwell(s, lax); len(got) != 0 {
		t.Fatalf("lax thresholds found %+v, want nothing", got)
	}
}

func TestSlopePerMinute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		xs, ys  []float64
		minSpan time.Duration
		want    float64
		ok      bool
	}{
		{"a line", []float64{0, 1, 2, 3}, []float64{10, 12, 14, 16}, 2 * time.Minute, 2, true},
		{"a falling line", []float64{0, 1, 2}, []float64{9, 6, 3}, 2 * time.Minute, -3, true},
		{"irregular spacing", []float64{0, 0.5, 3}, []float64{0, 1, 6}, 2 * time.Minute, 2, true},
		{"flat", []float64{0, 1, 2}, []float64{5, 5, 5}, 2 * time.Minute, 0, true},
		{"too few points", []float64{0, 5}, []float64{0, 50}, time.Minute, 0, false},
		{"too short a span", []float64{0, 0.5, 1}, []float64{0, 5, 10}, 2 * time.Minute, 0, false},
		{"every point at one instant", []float64{1, 1, 1}, []float64{0, 5, 10}, 0, 0, false},
		{"mismatched lengths", []float64{0, 1, 2}, []float64{0, 1}, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := slopePerMinute(c.xs, c.ys, c.minSpan)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("slope = %v, want %v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Publishing
// ---------------------------------------------------------------------------

// TestPublishZeroesWhatCleared: a finding that goes away must leave its gauge
// child AT zero, not merely stop updating it. A child frozen at 1 after the
// phone was pulled is a page that never resolves; a child that vanished is a
// resolution Prometheus infers from silence, which this project does not do.
func TestPublishZeroesWhatCleared(t *testing.T) {
	t.Parallel()

	// Label values unique to this test, so the package-level vec shared with
	// every other test does not make this one order-dependent.
	const host = "publish-host"
	a := anomalyKey{DeviceID: "pub-a", Kind: SwellKindTempRise}
	b := anomalyKey{DeviceID: "pub-b", Kind: SwellKindDrain}
	la := anomalyLabels{Host: host, RackSlot: "R9-U1-H1-P1", Kind: SwellKindTempRise}
	lb := anomalyLabels{Host: host, RackSlot: "R9-U1-H1-P2", Kind: SwellKindDrain}

	c := &swellChecker{
		active: map[anomalyKey]anomalyLabels{}, raised: map[anomalyKey]time.Time{},
		tally: newAnomalyTally(prometheus.NewGauge(prometheus.GaugeOpts{Name: "publish_test_count"})),
	}

	c.publish(map[anomalyKey]anomalyLabels{a: la, b: lb})
	if got := gaugeValue(t, batteryAnomalyGauge.WithLabelValues(host, la.RackSlot, la.Kind)); got != 1 {
		t.Fatalf("a = %v after publish, want 1", got)
	}
	if got := gaugeValue(t, batteryAnomalyGauge.WithLabelValues(host, lb.RackSlot, lb.Kind)); got != 1 {
		t.Fatalf("b = %v after publish, want 1", got)
	}
	if len(c.active) != 2 {
		t.Fatalf("active = %v, want both", c.active)
	}

	// a clears; b persists.
	c.publish(map[anomalyKey]anomalyLabels{b: lb})
	if got := gaugeValue(t, batteryAnomalyGauge.WithLabelValues(host, la.RackSlot, la.Kind)); got != 0 {
		t.Fatalf("a = %v after clearing, want 0", got)
	}
	if got := gaugeValue(t, batteryAnomalyGauge.WithLabelValues(host, lb.RackSlot, lb.Kind)); got != 1 {
		t.Fatalf("b = %v while still flagged, want 1", got)
	}
	if _, still := c.active[a]; still {
		t.Fatalf("a is still recorded as active after clearing: %v", c.active)
	}

	c.publish(nil)
	if got := gaugeValue(t, batteryAnomalyGauge.WithLabelValues(host, lb.RackSlot, lb.Kind)); got != 0 {
		t.Fatalf("b = %v after everything cleared, want 0", got)
	}
	if len(c.active) != 0 {
		t.Fatalf("active = %v after everything cleared, want empty", c.active)
	}
}

// TestTheCountSumsAcrossCheckers is the live demo's bug, kept. One process
// runs one checker per host, and a count each of them Set() on a shared gauge
// is whichever host published last — h02's quiet rack erasing h01's hot
// phone, which is what /metrics showed while farm_battery_anomaly{h01,...}
// was at 1. The tally sums, and a checker that stops takes only its own
// share with it.
func TestTheCountSumsAcrossCheckers(t *testing.T) {
	t.Parallel()

	count := prometheus.NewGauge(prometheus.GaugeOpts{Name: "tally_test_count"})
	tally := newAnomalyTally(count)
	h1 := &swellChecker{active: map[anomalyKey]anomalyLabels{}, raised: map[anomalyKey]time.Time{}, tally: tally}
	h2 := &swellChecker{active: map[anomalyKey]anomalyLabels{}, raised: map[anomalyKey]time.Time{}, tally: tally}

	a := anomalyKey{DeviceID: "tally-a", Kind: SwellKindTempRise}
	b := anomalyKey{DeviceID: "tally-b", Kind: SwellKindDrain}
	la := anomalyLabels{Host: "tally-h1", RackSlot: "R8-U1-H1-P1", Kind: SwellKindTempRise}
	lb := anomalyLabels{Host: "tally-h2", RackSlot: "R8-U2-H1-P1", Kind: SwellKindDrain}

	h1.publish(map[anomalyKey]anomalyLabels{a: la})
	if got := gaugeValue(t, count); got != 1 {
		t.Fatalf("count = %v after h1 found one, want 1", got)
	}
	// h2 reports a quiet rack a moment later. This is the line that read 0
	// on the live demo.
	h2.publish(nil)
	if got := gaugeValue(t, count); got != 1 {
		t.Fatalf("count = %v after h2 published nothing, want still 1: h2 erased h1's finding", got)
	}
	h2.publish(map[anomalyKey]anomalyLabels{b: lb})
	if got := gaugeValue(t, count); got != 2 {
		t.Fatalf("count = %v with one finding on each host, want 2", got)
	}
	h1.publish(nil)
	if got := gaugeValue(t, count); got != 1 {
		t.Fatalf("count = %v after h1 cleared, want 1", got)
	}
	h2.stop()
	if got := gaugeValue(t, count); got != 0 {
		t.Fatalf("count = %v after h2 stopped, want 0", got)
	}
}

// TestTheCountExistsBeforeAnyPhoneRunsHot is the arming guarantee behind
// DeviceFarmerBatteryAnomaly. The per-position vec has no child until the
// first finding, so a rule over it alone is disarmed until the first
// casualty; the plain count is what a fresh process exports on its first
// scrape, and it must be there.
func TestTheCountExistsBeforeAnyPhoneRunsHot(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	reg.MustRegister(Collectors()...)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	// The families are walked without naming their protobuf type, for the
	// reason internal/adbwire's tests give: client_model is an indirect
	// requirement and must stay one.
	present := map[string]bool{}
	seenKind := map[string]bool{}
	for _, f := range families {
		present[f.GetName()] = true
		if f.GetName() != "farm_watchdog_battery_anomaly_events_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "kind" {
					seenKind[l.GetValue()] = true
				}
			}
		}
	}

	if !present["farm_battery_anomalies"] {
		t.Fatal("farm_battery_anomalies is not exported by Collectors(); the rule over it can never fire")
	}
	// farm_battery_anomaly is deliberately NOT asserted present: a vec with
	// no children exports nothing, and in a process where no phone has run
	// hot it has none. That absence is the reason the count above exists.
	// The per-kind ledger counters are seeded so a dashboard reads zero
	// rather than nothing.
	for _, kind := range []string{SwellKindTempRise, SwellKindTempMax, SwellKindDrain} {
		if !seenKind[kind] {
			t.Errorf("battery_anomaly_events_total{kind=%q} is not seeded", kind)
		}
	}
}

// ---------------------------------------------------------------------------
// Invariants of the design itself
// ---------------------------------------------------------------------------

// TestBatteryDefaultsAgreeWithConfig pins the two copies of the three
// thresholds to each other. internal/config owns the reasoning and the
// environment; this package owns the fallback for a caller that supplies
// nothing. A drift between them is a watchdog that believes one policy while
// the startup summary prints another.
func TestBatteryDefaultsAgreeWithConfig(t *testing.T) {
	t.Parallel()

	if DefaultBatteryTempRiseDCPerMin != config.DefaultBatteryTempRiseDCPerMin {
		t.Errorf("temp rise default: watchdog %d, config %d",
			DefaultBatteryTempRiseDCPerMin, config.DefaultBatteryTempRiseDCPerMin)
	}
	if DefaultBatteryTempMaxDC != config.DefaultBatteryTempMaxDC {
		t.Errorf("temp max default: watchdog %d, config %d",
			DefaultBatteryTempMaxDC, config.DefaultBatteryTempMaxDC)
	}
	if DefaultBatteryDrainPctPerHour != config.DefaultBatteryDrainPctPerHour {
		t.Errorf("drain default: watchdog %d, config %d",
			DefaultBatteryDrainPctPerHour, config.DefaultBatteryDrainPctPerHour)
	}
	// And the ceiling's own ceiling: a default the column cannot hold could
	// never fire.
	if DefaultBatteryTempMaxDC > maxBatteryTempDC {
		t.Errorf("the default ceiling %d is above what the reader will ever write (%d)",
			DefaultBatteryTempMaxDC, maxBatteryTempDC)
	}
}

// TestSwellWindowsFitTheReader states the cadence arithmetic as an assertion.
// The detector needs three points across two minutes for a rise and fifteen
// minutes for a drain; at the reader's interval those have to be reachable,
// or the rules are written for a cadence nobody runs.
func TestSwellWindowsFitTheReader(t *testing.T) {
	t.Parallel()

	if swellRiseWindow < time.Duration(swellMinSamples)*DefaultBatteryInterval {
		t.Fatalf("the rise window %s cannot hold %d readings at one every %s",
			swellRiseWindow, swellMinSamples, DefaultBatteryInterval)
	}
	if swellRiseMinSpan > swellRiseWindow {
		t.Fatalf("rise min span %s exceeds the rise window %s", swellRiseMinSpan, swellRiseWindow)
	}
	if swellDrainMinSpan > swellWindow {
		t.Fatalf("drain min span %s exceeds the window %s", swellDrainMinSpan, swellWindow)
	}
	if swellRaiseTTL < swellWindow {
		t.Fatalf("an anomaly is re-raised (%s) before its evidence has aged out (%s)",
			swellRaiseTTL, swellWindow)
	}
}

// TestSwellSQLReachesNoLease is the package's one rule, checked in the file
// that reads the one lease-adjacent fact the rule permits. The drain rule
// needs to know whether a device is idle, and that is farm.devices'
// trigger-maintained pointer plus its release timestamp — two columns on the
// identity table, which this role may SELECT. They are the ONLY lease
// vocabulary allowed in a string here; farm.leases itself, the lease
// functions, fences and holders remain unnameable.
//
// Only string literals are examined, as in TestBatterySQLNeverNamesALease:
// the prose discusses leases in order to explain their absence.
func TestSwellSQLReachesNoLease(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "swell.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing swell.go: %v", err)
	}

	allowed := []string{"current_lease_id", "last_released_at"}
	banned := []string{"lease", "fence", "holder", "quarantin"}

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		lower := strings.ToLower(lit.Value)
		for _, col := range allowed {
			lower = strings.ReplaceAll(lower, col, "")
		}
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Errorf("%s: a string in swell.go contains %q beyond the two idle-ness columns: %s",
					fset.Position(lit.Pos()), word, lit.Value)
			}
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func ptr(v int32) *int32 { return &v }

func shuffled(s []batterySample) []batterySample {
	out := make([]batterySample, 0, len(s))
	// A fixed permutation, so a failure reproduces: last, first, then the
	// middle in reverse.
	if len(s) > 1 {
		out = append(out, s[len(s)-1], s[0])
		for i := len(s) - 2; i >= 1; i-- {
			out = append(out, s[i])
		}
		return out
	}
	return append(out, s...)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// gaugeValue reads one gauge through a scratch registry rather than
// Gauge.Write, which would need the client_model type named here.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(g); err != nil {
		t.Fatalf("registering gauge for a read: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf("expected exactly one gauge sample, got %d families", len(families))
	}
	return families[0].GetMetric()[0].GetGauge().GetValue()
}
