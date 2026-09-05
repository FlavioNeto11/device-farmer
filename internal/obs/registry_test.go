package obs

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The collectors in this package are package-level vars, shared by every
// test in it: a fresh prometheus.Registry does not give a test a fresh
// counter, it gives it another view of the same one. Most tests here
// therefore assert on series they never touch. The two that must
// increment something — the fold and blip tests at the bottom — call
// restoreSeeded, which puts the family back the way zeroFill left it, so
// this file's result does not depend on the order `-shuffle` picks.

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// mustNameTheMetricFirst asserts that OUR message names the metric,
// rather than that the concatenated string happens to contain it.
//
// A plain strings.Contains is not that assertion and does not fail when
// the guard is removed: client_golang's own error embeds the whole
// descriptor, `Desc{fqName: "farm_recovery_attempts_total", help: "…"}`,
// roughly three hundred characters of help text and label lists. So an
// error reduced to "obs: registration failed: %w" still contains the
// name, and still buries the one word the reader needs in the text they
// were always going to skim. Requiring the name to appear before the
// wrapped Desc dump is what distinguishes the two.
func mustNameTheMetricFirst(t *testing.T, err error, metric string) {
	t.Helper()
	msg := err.Error()
	name := strings.Index(msg, metric)
	if name < 0 {
		t.Errorf("error does not name the metric %s: %v", metric, err)
		return
	}
	if dump := strings.Index(msg, "Desc{"); dump >= 0 && name > dump {
		t.Errorf("%s appears only inside the wrapped client_golang descriptor; an operator reading "+
			"the start of this error learns no metric name to grep for: %v", metric, err)
	}
	if !strings.HasPrefix(msg, "obs: ") {
		t.Errorf("error is not attributed to this package: %v", err)
	}
}

// restoreSeeded resets vecs and re-seeds them when the test ends. Without
// it, a test that increments a package-level vector decides the result of
// every mustBeZero assertion in this file that happens to run after it.
func restoreSeeded(t *testing.T, vecs ...interface{ Reset() }) {
	t.Helper()
	t.Cleanup(func() {
		for _, v := range vecs {
			v.Reset()
		}
		zeroFill()
	})
}

// seriesValue returns the value of the one series of family `name` whose
// label set is exactly want, and whether such a series exists at all.
// Presence and value are returned separately because the whole point of
// zero-filling is that "absent" and "0" are different answers.
func seriesValue(t *testing.T, r *prometheus.Registry, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			if len(labels) != len(want) {
				continue
			}
			match := true
			for _, l := range labels {
				if v, ok := want[l.GetName()]; !ok || v != l.GetValue() {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			case m.GetHistogram() != nil:
				return m.GetHistogram().GetSampleSum(), true
			}
			t.Fatalf("%s: series has no counter, gauge or histogram value", name)
		}
	}
	return 0, false
}

// mustBeZero asserts that a series exists and reads zero — the two halves
// of "this alert is armed".
func mustBeZero(t *testing.T, r *prometheus.Registry, name string, want map[string]string) {
	t.Helper()
	v, ok := seriesValue(t, r, name, want)
	if !ok {
		t.Errorf("%s%v is absent; an alerting rule over an absent series returns no data and never fires", name, want)
		return
	}
	if v != 0 {
		t.Errorf("%s%v = %v, want 0 on a fresh registry", name, want, v)
	}
}

// histogramSeries returns the sample count and sum of one histogram series.
func histogramSeries(t *testing.T, r *prometheus.Registry, name string, want map[string]string) (uint64, float64, bool) {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			if len(labels) != len(want) {
				continue
			}
			match := true
			for _, l := range labels {
				if v, ok := want[l.GetName()]; !ok || v != l.GetValue() {
					match = false
					break
				}
			}
			if !match || m.GetHistogram() == nil {
				continue
			}
			return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum(), true
		}
	}
	return 0, 0, false
}

// familySum adds up every series published under one metric name, so a
// test can assert that a whole family did not move rather than that three
// hand-picked children of it did not.
func familySum(t *testing.T, r *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			switch {
			case m.GetCounter() != nil:
				sum += m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				sum += m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				sum += m.GetHistogram().GetSampleSum()
			}
		}
	}
	return sum
}

// familySize counts the series published under one metric name.
func familySize(t *testing.T, r *prometheus.Registry, name string) int {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

// TestRegisterAllArmsTheSeriesOperatorsAlertOn names every series doc.go
// puts in an alerting rule and requires each to exist and read zero on a
// fresh process.
//
// This is the test that would have caught the gap. `farm_lease_suspect`
// was absent from /metrics until a lease first went suspect, so the rule
// meant to warn a human that a protected lease is waiting for them was
// armed by the very event it was supposed to pre-empt. Same for the two
// tombstone counters if Register is ever narrowed, and for the recovery
// and rearm series, which were never seeded at all.
func TestRegisterAllArmsTheSeriesOperatorsAlertOn(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterAll(reg, discardLog()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	// increase(farm_lease_reaped_total{reason="holder_expired"}[15m]) > 0
	// — the control plane destroyed work.
	mustBeZero(t, reg, "farm_lease_reaped_total", map[string]string{"reason": "holder_expired"})

	// increase(farm_lease_renew_failures_total{kind="fenced"}[10m]) > 0
	// — a job was fenced and aborted.
	mustBeZero(t, reg, "farm_lease_renew_failures_total", map[string]string{"kind": "fenced"})

	// farm_lease_suspect{protected="true"} > 0 for: 5m
	// — a protected lease is waiting for a human and will wait forever.
	mustBeZero(t, reg, "farm_lease_suspect", map[string]string{
		"pool": unknownLabel, "tenant": unknownLabel, "protected": "true",
	})
	mustBeZero(t, reg, "farm_lease_suspect", map[string]string{
		"pool": unknownLabel, "tenant": unknownLabel, "protected": "false",
	})

	// sum by (host, hub) (farm_device_health{state="healthy"}) == 0
	//   and sum by (host, hub) (farm_device_health) > 1
	// — a whole hub went dark. The guard clause is why seeding an
	// all-unknown row cannot page anyone: it sums to 0, not to > 1.
	mustBeZero(t, reg, "farm_device_health", map[string]string{
		"state": string(HealthHealthy), "host": unknownLabel, "hub": unknownLabel,
	})

	// sum by (host, hub) (rate(farm_recovery_attempts_total{outcome="failed"}[15m])) > 0.05
	// — correlated physical failure. Every tier can fail, so every tier's
	// failed row has to exist.
	for _, tier := range recoveryTiers {
		mustBeZero(t, reg, "farm_recovery_attempts_total", map[string]string{
			"tier": string(tier), "outcome": string(OutcomeFailed),
			"host": unknownLabel, "hub": unknownLabel, "rack_slot": unknownLabel,
		})
	}

	// increase(farm_control_plane_gap_seconds_sum[1h]) > 600 — our downtime.
	for _, c := range components {
		count, sum, ok := histogramSeries(t, reg, "farm_control_plane_gap_seconds",
			map[string]string{"component": string(c)})
		if !ok {
			t.Errorf("farm_control_plane_gap_seconds{component=%q} is absent", c)
			continue
		}
		if count != 0 || sum != 0 {
			t.Errorf("farm_control_plane_gap_seconds{component=%q} = count %d sum %v, want 0/0", c, count, sum)
		}
	}

	// sum(farm_slot_rearm_pending) > <fraction of fleet> — ticket, not page.
	// sum() over an absent family is empty, so this needs a seed too.
	mustBeZero(t, reg, "farm_slot_rearm_pending", map[string]string{
		"host": unknownLabel, "hub": unknownLabel,
	})

	// Present so a correlation dashboard reads flat zero rather than "No
	// data". doc.go forbids an alerting rule on this one; existing at zero
	// changes nothing about that, since the first real blip would create
	// the series regardless.
	mustBeZero(t, reg, "farm_adb_transport_blips_total", map[string]string{
		"host": unknownLabel, "hub": unknownLabel, "rack_slot": unknownLabel,
		"kind": string(BlipReset),
	})
}

// TestRegisterAllZeroFillsEveryClosedLabelCombination checks the seeding
// against the enums rather than against a copy of the list in
// registry.go, so a tier or health state added to one and forgotten in
// the other fails here.
func TestRegisterAllZeroFillsEveryClosedLabelCombination(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterAll(reg, discardLog()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	cases := []struct {
		name string
		want int
	}{
		{"farm_adb_transport_blips_total", len(blipKinds)},
		{"farm_lease_reaped_total", len(releaseReasons)},
		{"farm_lease_renew_failures_total", len(renewFailureKinds)},
		{"farm_lease_held", 1},
		{"farm_lease_suspect", 2}, // protected true and false
		{"farm_device_health", len(healthStates)},
		{"farm_recovery_attempts_total", len(recoveryTiers) * len(recoveryOutcomes)},
		{"farm_control_plane_gap_seconds", len(components)},
		{"farm_slot_rearm_pending", 1},
	}
	for _, c := range cases {
		if got := familySize(t, reg, c.name); got != c.want {
			t.Errorf("%s has %d series, want %d", c.name, got, c.want)
		}
	}

	// Every blip kind, health state, tier x outcome pair and reason,
	// individually — a family of the right size built from the wrong values
	// would otherwise pass.
	for _, k := range blipKinds {
		mustBeZero(t, reg, "farm_adb_transport_blips_total", map[string]string{
			"host": unknownLabel, "hub": unknownLabel, "rack_slot": unknownLabel, "kind": string(k),
		})
	}
	for _, r := range releaseReasons {
		mustBeZero(t, reg, "farm_lease_reaped_total", map[string]string{"reason": string(r)})
	}
	for _, k := range renewFailureKinds {
		mustBeZero(t, reg, "farm_lease_renew_failures_total", map[string]string{"kind": string(k)})
	}
	for _, s := range healthStates {
		mustBeZero(t, reg, "farm_device_health", map[string]string{
			"state": string(s), "host": unknownLabel, "hub": unknownLabel,
		})
	}
	for _, tier := range recoveryTiers {
		for _, outcome := range recoveryOutcomes {
			mustBeZero(t, reg, "farm_recovery_attempts_total", map[string]string{
				"tier": string(tier), "outcome": string(outcome),
				"host": unknownLabel, "hub": unknownLabel, "rack_slot": unknownLabel,
			})
		}
	}

	// The runtime collectors stay the binary's decision, as Register says.
	if n := familySize(t, reg, "go_goroutines"); n != 0 {
		t.Errorf("go_goroutines is registered; the Go collector is the binary's call, not this package's")
	}
}

// TestRegisterAllIsIdempotent covers the 'all' and 'demo' roles, where
// several roles share one process and one registry and the same collector
// legitimately arrives more than once.
//
// The "a value survives re-registration" half deliberately uses a
// collector created here rather than one of this package's. The vectors in
// metrics.go are package-level state shared by every test in the package,
// and the tests above assert that their series read zero on a fresh
// registry; incrementing one would make this file's result depend on the
// order the tests happen to run in. The path under test is the same either
// way — RegisterAll registers its own collectors and supplied ones through
// one code path.
func TestRegisterAllIsIdempotent(t *testing.T) {
	carried := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_carried_total", Help: "Incremented between two RegisterAll calls.",
	})
	group := []prometheus.Collector{carried}

	reg := prometheus.NewRegistry()
	if err := RegisterAll(reg, discardLog(), group); err != nil {
		t.Fatalf("first RegisterAll: %v", err)
	}
	before := len(gathered(t, reg))

	// Re-registration that quietly replaced a collector would erase the
	// evidence of everything that had already happened to it.
	carried.Add(3)

	if err := RegisterAll(reg, discardLog(), group); err != nil {
		t.Fatalf("second RegisterAll: %v", err)
	}
	if after := len(gathered(t, reg)); after != before {
		t.Errorf("families after second RegisterAll = %d, want %d", after, before)
	}
	got, ok := seriesValue(t, reg, "test_carried_total", map[string]string{})
	if !ok {
		t.Fatal("test_carried_total vanished on re-registration")
	}
	if got != 3 {
		t.Errorf("test_carried_total = %v after re-registration, want 3", got)
	}
}

// TestRegisterAllTreatsAlreadyRegisteredAsSuccess covers the other order:
// internal/api registers the shared collectors itself when it owns its
// registry, so RegisterAll can arrive second.
func TestRegisterAllTreatsAlreadyRegisteredAsSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := RegisterAll(reg, discardLog()); err != nil {
		t.Fatalf("RegisterAll after Register: %v", err)
	}
	// And the seeding still happened, which Register alone does not do.
	mustBeZero(t, reg, "farm_lease_suspect", map[string]string{
		"pool": unknownLabel, "tenant": unknownLabel, "protected": "true",
	})
}

// TestRegisterAllRegistersSuppliedGroups is the inverted dependency: the
// six packages that import this one hand their collectors in, and both a
// duplicate group and a nil slice have to be harmless.
func TestRegisterAllRegistersSuppliedGroups(t *testing.T) {
	first := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_group_first_total", Help: "First supplied collector.",
	})
	second := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_group_second", Help: "Second supplied collector.",
	})

	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(),
		[]prometheus.Collector{first},
		nil,
		[]prometheus.Collector{second, first}, // first arrives twice on purpose
	)
	if err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	for _, name := range []string{"test_group_first_total", "test_group_second"} {
		if familySize(t, reg, name) != 1 {
			t.Errorf("%s was not registered", name)
		}
	}
}

// TestRegisterAllNamesAConflictingMetric pins the behaviour that found the
// live bug.
//
// internal/recovery publishes farm/recovery/attempts_total, whose
// fully-qualified name is farm_recovery_attempts_total — the same name
// this package's recoveryAttempts already uses, but with labels
// {tier,outcome} instead of {tier,outcome,host,hub,rack_slot} and a
// different help string. Prometheus rejects the second one. The collector
// built here reproduces that collision exactly, so the assertion holds
// whichever of the two names is eventually changed.
//
// Two things are required of RegisterAll: the error has to name the metric
// (a conflict reported as "registration failed" sends an operator reading
// stack traces), and the other collectors in the same group have to end up
// registered anyway.
func TestRegisterAllNamesAConflictingMetric(t *testing.T) {
	clash := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "recovery", Name: "attempts_total",
		Help: "Same fully-qualified name as obs.recoveryAttempts, narrower labels.",
	}, []string{"tier", "outcome"})
	bystander := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_bystander_total", Help: "Registered despite a sibling conflicting.",
	})

	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(), []prometheus.Collector{clash, bystander})
	if err == nil {
		t.Fatal("RegisterAll accepted a metric name already taken with different labels")
	}
	mustNameTheMetricFirst(t, err, "farm_recovery_attempts_total")
	var dup prometheus.AlreadyRegisteredError
	if errors.As(err, &dup) {
		t.Errorf("a genuine name conflict was reported as AlreadyRegisteredError: %v", err)
	}
	if familySize(t, reg, "test_bystander_total") != 1 {
		t.Error("one conflicting collector stopped the rest of its group from registering")
	}
	// The real collector kept the name, so its physical-position labels —
	// which the hub-failure rule groups by — are still there.
	if _, ok := seriesValue(t, reg, "farm_recovery_attempts_total", map[string]string{
		"tier": string(TierPortPowerCycle), "outcome": string(OutcomeFailed),
		"host": unknownLabel, "hub": unknownLabel, "rack_slot": unknownLabel,
	}); !ok {
		t.Error("the conflict displaced obs's own farm_recovery_attempts_total")
	}
}

// TestRegisterAllRejectsNilCollector: Registry.Register calls Describe on
// whatever it is handed, so a nil entry in a Collectors() slice panics the
// process before /metrics is ever served. It has to come back as an error
// that says where it was.
func TestRegisterAllRejectsNilCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(), []prometheus.Collector{nil})
	if err == nil {
		t.Fatal("RegisterAll accepted a nil collector")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error does not identify the nil collector: %v", err)
	}
}

func TestRegisterAllRejectsNilRegistry(t *testing.T) {
	if err := RegisterAll(nil, discardLog()); err == nil {
		t.Fatal("RegisterAll accepted a nil registry")
	}
}

// TestRegisterAllAcceptsNilLogger: a nil logger is a caller that wants no
// commentary on the startup path, not a caller that wants a panic there.
func TestRegisterAllAcceptsNilLogger(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterAll(reg, nil); err != nil {
		t.Fatalf("RegisterAll with a nil logger: %v", err)
	}
}

// TestRegisterAllRejectsAnUninitialisedCollector covers the fault a nil
// check cannot see.
//
// A Collectors() slice whose entry is a struct field nobody assigned
// holds a TYPED nil — a nil *prometheus.CounterVec inside a non-nil
// interface — so `c == nil` is false. Registry.Register then calls
// Describe on it from a goroutine of its own that does not recover, which
// is why a deferred recover around Register is no protection either.
// Before RegisterAll described its inputs first, this test did not fail:
// it killed the whole test binary with an unrecoverable nil dereference
// inside prometheus.(*Registry).Register.func1, exactly as it would have
// killed the control plane before /metrics was ever served.
func TestRegisterAllRejectsAnUninitialisedCollector(t *testing.T) {
	var uninitialised *prometheus.CounterVec
	if prometheus.Collector(uninitialised) == nil {
		t.Fatal("a typed nil compared equal to nil; this test no longer covers the case it was written for")
	}

	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(), []prometheus.Collector{uninitialised})
	if err == nil {
		t.Fatal("RegisterAll accepted an uninitialised collector")
	}
	if !strings.Contains(err.Error(), "group 0") {
		t.Errorf("error does not say which slice the bad entry was in: %v", err)
	}
	// One bad entry must not take the metric surface with it.
	mustBeZero(t, reg, "farm_lease_renew_failures_total", map[string]string{"kind": string(KindFenced)})
}

// nilDescCollector describes a nil *prometheus.Desc. Desc.String
// dereferences its receiver and Register reads desc.err one field
// earlier, so both the error message and the registration would panic on
// it.
type nilDescCollector struct{}

func (nilDescCollector) Describe(ch chan<- *prometheus.Desc) { ch <- nil }
func (nilDescCollector) Collect(chan<- prometheus.Metric)    {}

func TestRegisterAllRejectsANilDescriptor(t *testing.T) {
	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(), []prometheus.Collector{nilDescCollector{}})
	if err == nil {
		t.Fatal("RegisterAll accepted a collector that describes a nil descriptor")
	}
	mustBeZero(t, reg, "farm_lease_reaped_total", map[string]string{"reason": string(ReasonHolderExpired)})
}

// TestRegisterAllDetectsAShadowedOwnCollector is the silent failure that
// AlreadyRegisteredError hides.
//
// AlreadyRegisteredError is descriptor identity, not instance identity.
// Two distinct vectors with the same name, help and labels produce it as
// well, and the registry keeps the first: registering a second identical
// CounterVec, incrementing it, and gathering yields zero metric families.
// Treated as plain success, that is a total blackout of this package's
// metrics behind a log line reading "farm metrics registered" — every
// LeaseReaped call incrementing a vector nobody scrapes, and the
// tombstone alert for destroyed work reading no data forever.
func TestRegisterAllDetectsAShadowedOwnCollector(t *testing.T) {
	shadow := cloneCounterVec(t, leaseReaped)
	if prometheus.Collector(shadow) == prometheus.Collector(leaseReaped) {
		t.Fatal("the clone is the same instance; this test proves nothing")
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(shadow); err != nil {
		t.Fatalf("registering the shadow: %v", err)
	}

	err := RegisterAll(reg, discardLog())
	if err == nil {
		t.Fatal("RegisterAll reported success while farm_lease_reaped_total was served by a different collector")
	}
	mustNameTheMetricFirst(t, err, "farm_lease_reaped_total")
	// Deliberately not wrapped as an AlreadyRegisteredError: that is the
	// one error class RegisterAll treats as success, and a caller handed a
	// shadow disguised as one is back to the silent blackout.
	var dup prometheus.AlreadyRegisteredError
	if errors.As(err, &dup) {
		t.Errorf("a shadowed collector was reported as AlreadyRegisteredError, which callers treat as success: %v", err)
	}
	// The harm the error is warning about, stated as an assertion: the
	// alert increase(farm_lease_reaped_total{reason="holder_expired"}[15m])
	// has nothing to read.
	if _, ok := seriesValue(t, reg, "farm_lease_reaped_total",
		map[string]string{"reason": string(ReasonHolderExpired)}); ok {
		t.Error("the shadowing collector unexpectedly serves the seeded series; this test no longer demonstrates the harm")
	}
	// Everything not shadowed still registered.
	mustBeZero(t, reg, "farm_lease_suspect", map[string]string{
		"pool": unknownLabel, "tenant": unknownLabel, "protected": "true",
	})
}

// TestRegisterAllDetectsAShadowedSuppliedCollector covers the same
// blackout in a supplied group, where it is the likelier mistake: a
// package re-declaring a metric another package already owns produces two
// instances with identical descriptors, and the one registered second
// counts into nothing. internal/recovery re-declaring
// farm_recovery_attempts_total is that mistake with the labels changed;
// with the labels left alone it would have been this one, and silent.
func TestRegisterAllDetectsAShadowedSuppliedCollector(t *testing.T) {
	opts := prometheus.CounterOpts{
		Name: "test_two_packages_total", Help: "Declared in two packages by mistake.",
	}
	first := prometheus.NewCounterVec(opts, []string{"k"})
	second := prometheus.NewCounterVec(opts, []string{"k"})

	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(),
		[]prometheus.Collector{first},
		[]prometheus.Collector{second},
	)
	if err == nil {
		t.Fatal("RegisterAll reported success while a supplied collector was shadowed; its increments would go nowhere")
	}
	mustNameTheMetricFirst(t, err, "test_two_packages_total")

	// The proof that it matters: the shadowed instance is the one the
	// second package would increment, and it reaches no scrape.
	second.WithLabelValues("x").Add(7)
	if v, _ := seriesValue(t, reg, "test_two_packages_total", map[string]string{"k": "x"}); v != 0 {
		t.Errorf("test_two_packages_total{k=\"x\"} = %v; the shadowed instance was expected to be invisible", v)
	}
}

// TestSameCollectorProvesOnlyWhatItCanAndNeverPanics pins both halves of
// the shadow check.
//
// The tempting one-liner — `ours == existing` — panics at runtime when
// both interfaces hold the same non-comparable dynamic type, which would
// turn a supplied collector into a crash on the startup path. The last
// case below is that collector.
func TestSameCollectorProvesOnlyWhatItCanAndNeverPanics(t *testing.T) {
	opts := prometheus.CounterOpts{Name: "test_same_total", Help: "Two instances, one descriptor."}
	a := prometheus.NewCounterVec(opts, []string{"k"})
	b := prometheus.NewCounterVec(opts, []string{"k"})
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_same", Help: "Other kind."}, []string{"k"})

	if !sameCollector(a, a) {
		t.Error("a collector is not itself")
	}
	if sameCollector(a, b) {
		t.Error("two distinct vectors with identical descriptors were reported as the same instance; a shadow would go undetected")
	}
	if sameCollector(a, g) {
		t.Error("collectors of different concrete types were reported as the same instance")
	}
	// Not one of this package's kinds: claim nothing rather than compare.
	if !sameCollector(uncomparableCollector{}, uncomparableCollector{}) {
		t.Error("sameCollector made a claim about a collector it cannot identify")
	}
}

// uncomparableCollector has a field that makes `==` on two of them a
// runtime panic.
type uncomparableCollector struct{ notes []string }

func (uncomparableCollector) Describe(chan<- *prometheus.Desc) {}
func (uncomparableCollector) Collect(chan<- prometheus.Metric) {}

// TestGroupOriginNamesTheOffendingSlice: "group 3 collector 2 is nil"
// makes an operator count arguments at the RegisterAll call site to find
// the package that produced the bad slice. Naming a metric from the same
// slice makes them grep for it instead.
func TestGroupOriginNamesTheOffendingSlice(t *testing.T) {
	marker := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_origin_marker_total", Help: "A healthy sibling of a broken entry.",
	})

	reg := prometheus.NewRegistry()
	err := RegisterAll(reg, discardLog(), nil, nil, []prometheus.Collector{marker, nil})
	if err == nil {
		t.Fatal("RegisterAll accepted a nil entry")
	}
	if !strings.Contains(err.Error(), "test_origin_marker_total") {
		t.Errorf("error names no metric from the offending slice, so it cannot be traced to a package: %v", err)
	}
	if familySize(t, reg, "test_origin_marker_total") != 1 {
		t.Error("the healthy sibling of a broken entry was not registered")
	}
}

// TestUnclassifiedRenewFailureIsTransientNeverFenced is the fold that
// keeps #663 out of the alerting layer.
//
// KindFenced means one thing: farm.lease_renew returned zero rows, the
// lease is gone, the job aborts. A renewal that failed because Postgres
// was unreachable proves nothing about the lease, and recording it as
// fenced would page a human — under the rule
// increase(farm_lease_renew_failures_total{kind="fenced"}[10m]) > 0 —
// claiming destroyed work on a job that is running perfectly well. The
// fold therefore has a direction, and this asserts the direction rather
// than merely that the value is one of the two.
func TestUnclassifiedRenewFailureIsTransientNeverFenced(t *testing.T) {
	restoreSeeded(t, leaseRenewFailures)

	reg := prometheus.NewRegistry()
	if err := RegisterAll(reg, discardLog()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	LeaseRenewFailure(RenewFailureKind("dial tcp 127.0.0.1:5432: connect: connection refused"))

	fenced, ok := seriesValue(t, reg, "farm_lease_renew_failures_total",
		map[string]string{"kind": string(KindFenced)})
	if !ok {
		t.Fatal("farm_lease_renew_failures_total{kind=\"fenced\"} is absent")
	}
	if fenced != 0 {
		t.Errorf("an unclassified renewal failure was recorded as fenced (%v); a Postgres dial error would page a human about work that is fine", fenced)
	}
	transient, ok := seriesValue(t, reg, "farm_lease_renew_failures_total",
		map[string]string{"kind": string(KindTransient)})
	if !ok || transient != 1 {
		t.Errorf("farm_lease_renew_failures_total{kind=\"transient\"} = %v (present=%v), want 1", transient, ok)
	}
}

// TestTransportBlipMovesNoLeaseSeries is the invariant read off /metrics.
//
// A socket died. Under #663 that is what ends a lease; here it must be
// visible in exactly one family and in no other. The assertion is over
// the whole lease surface rather than three hand-picked series, because
// the failure being guarded against is a transport error acquiring a
// lease-shaped consequence anywhere at all.
func TestTransportBlipMovesNoLeaseSeries(t *testing.T) {
	restoreSeeded(t, transportBlips)

	reg := prometheus.NewRegistry()
	if err := RegisterAll(reg, discardLog()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	slot := Slot{Host: "host-a", Hub: "3-1.4", RackSlot: "R1-U14-H2-P3"}
	// The kind most often mistaken for a device loss, and the exact
	// signature in #663.
	TransportBlip(slot, BlipTransportGone)
	TransportBlip(slot, BlipReset)

	for _, kind := range []BlipKind{BlipTransportGone, BlipReset} {
		v, ok := seriesValue(t, reg, "farm_adb_transport_blips_total", map[string]string{
			"host": slot.Host, "hub": slot.Hub, "rack_slot": slot.RackSlot, "kind": string(kind),
		})
		if !ok || v != 1 {
			t.Errorf("farm_adb_transport_blips_total{kind=%q} = %v (present=%v), want 1", kind, v, ok)
		}
	}

	for _, name := range []string{
		"farm_lease_reaped_total",
		"farm_lease_renew_failures_total",
		"farm_lease_held",
		"farm_lease_suspect",
	} {
		if sum := familySum(t, reg, name); sum != 0 {
			t.Errorf("a transport blip moved %s to %v; a socket error must have no lease-shaped consequence (#663)", name, sum)
		}
	}
}

// cloneCounterVec builds a DIFFERENT *prometheus.CounterVec whose
// descriptors are identical to c's, reading name, help and label names
// back off c rather than copying its help text into this file. A copied
// help string stops being identical the day someone rewords the original,
// and the shadow test would then silently stop testing a shadow.
func cloneCounterVec(t *testing.T, c prometheus.Collector) *prometheus.CounterVec {
	t.Helper()
	d := soleDesc(t, c)
	s := d.String()
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: fqName(d),
		Help: quotedField(t, s, "help: "),
	}, variableLabelNames(t, s))
}

func soleDesc(t *testing.T, c prometheus.Collector) *prometheus.Desc {
	t.Helper()
	ch := make(chan *prometheus.Desc, 8)
	go func() {
		defer close(ch)
		c.Describe(ch)
	}()
	var got []*prometheus.Desc
	for d := range ch {
		got = append(got, d)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly one descriptor, got %d", len(got))
	}
	return got[0]
}

// quotedField extracts one %q-formatted field of a Desc.String(). It
// fails the test rather than guessing if the format moves, which is the
// difference between this and fqName: fqName degrades an error message,
// this one decides whether a test is testing anything.
func quotedField(t *testing.T, s, prefix string) string {
	t.Helper()
	i := strings.Index(s, prefix)
	if i < 0 {
		t.Fatalf("Desc.String has no %q field: %s", prefix, s)
	}
	rest := s[i+len(prefix):]
	if len(rest) == 0 || rest[0] != '"' {
		t.Fatalf("field %q is not quoted: %s", prefix, s)
	}
	for j := 1; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++
		case '"':
			v, err := strconv.Unquote(rest[:j+1])
			if err != nil {
				t.Fatalf("unquoting %s: %v", rest[:j+1], err)
			}
			return v
		}
	}
	t.Fatalf("unterminated %q field: %s", prefix, s)
	return ""
}

func variableLabelNames(t *testing.T, s string) []string {
	t.Helper()
	const marker = "variableLabels: {"
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("Desc.String has no variableLabels: %s", s)
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatalf("unterminated variableLabels: %s", s)
	}
	if rest[:j] == "" {
		return nil
	}
	return strings.Split(rest[:j], ",")
}

// gathered returns the metric family names in r.
func gathered(t *testing.T, r *prometheus.Registry) []string {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make([]string, 0, len(mfs))
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	return names
}

// ---------------------------------------------------------------------
// Source-level guards.
//
// The two tests below read the repository instead of the running
// program, because both regressions they catch are invisible at runtime.
// A package whose collectors nobody registers looks exactly like a
// package whose loop never ran — the counters are declared, they are
// incremented, and /metrics is silent — and a duplicated metric name is
// only discovered by a process that registers both declarations, which,
// until the groups above were passed to RegisterAll, no process did.
//
// They live in this package because it is the one place that cannot
// import the packages in question: everything that contributes a group
// imports obs, so the check has to be made against the source rather
// than against the symbols.
// ---------------------------------------------------------------------

// TestEveryCollectorGroupIsRegistered fails when a package exports
// Collectors() and nothing passes the result to obs.RegisterAll.
//
// This is the regression the test exists for, not a hypothetical: nine of
// the ten sets — every scheduler, reaper, jobrunner, janitor, watchdog,
// node, topo, enroll and recovery counter in the project — were declared,
// documented and incremented while reaching no registry at all, and the
// gap was reported closed once before it actually was.
func TestEveryCollectorGroupIsRegistered(t *testing.T) {
	files := repoFiles(t)

	// Keyed by package name, which is also the identifier a call site
	// writes (`recovery.Collectors()`). An import alias would be read as
	// a different package and reported as unregistered — noisy, but in
	// the safe direction: this test can never pass a group that is
	// genuinely missing.
	declared := make(map[string]string) // package -> file declaring Collectors()
	registered := make(map[string]bool) // package -> passed to obs.RegisterAll
	for _, f := range files {
		if declaresCollectors(f.file) {
			declared[f.file.Name.Name] = f.path
		}
		for _, pkg := range registerAllArguments(f.file) {
			registered[pkg] = true
		}
	}

	if len(declared) == 0 {
		t.Fatal("no Collectors() declarations found at all: the scan is broken, not the code")
	}

	var missing []string
	for pkg, path := range declared {
		if !registered[pkg] {
			missing = append(missing, pkg+" ("+path+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these packages export Collectors() that nothing hands to obs.RegisterAll, so "+
			"their metrics never reach /metrics and every rule over them returns no data and "+
			"never fires:\n\t%s\nAdd the group to newRegistry in cmd/farmd/roles.go.",
			strings.Join(missing, "\n\t"))
	}
}

// TestNoDuplicateMetricNames fails when two declarations anywhere in the
// module render the same fully-qualified metric name.
//
// farm_recovery_attempts_total was declared twice: here with
// {tier, outcome, host, hub, rack_slot}, and in internal/recovery as
// Namespace "farm" + Subsystem "recovery" + Name "attempts_total" with
// {tier, outcome}. Registry.Register refuses the second of two
// descriptors that disagree, and it refuses whichever arrives second, so
// the collision did not cost one metric — it cost a whole package its
// registration, which is why it has to be caught at the declaration and
// not at the call site.
func TestNoDuplicateMetricNames(t *testing.T) {
	byName := make(map[string][]string)
	for dir, files := range repoFilesByDir(t) {
		// Constants are resolved per directory because this package
		// writes `Namespace: namespace`, not `Namespace: "farm"`. A scan
		// that only read string literals would have missed the very
		// collision that motivated it.
		consts := packageStringConsts(files)
		for _, f := range files {
			for _, name := range metricNamesDeclaredIn(f, consts) {
				byName[name] = append(byName[name], dir+"/"+baseName(f.path))
			}
		}
	}

	var dupes []string
	for name, sites := range byName {
		if len(sites) > 1 {
			sort.Strings(sites)
			dupes = append(dupes, name+": "+strings.Join(sites, ", "))
		}
	}
	sort.Strings(dupes)
	if len(dupes) > 0 {
		t.Errorf("these metric names are declared more than once. Prometheus refuses the second "+
			"registration whenever the descriptors differ, and the refusal costs the package that "+
			"loses the race every metric it owns:\n\t%s", strings.Join(dupes, "\n\t"))
	}
}

// ---------------------------------------------------------------------
// Source scanning.
// ---------------------------------------------------------------------

type parsedFile struct {
	path string
	file *ast.File
}

// moduleRoot is the module root relative to this package's directory,
// which is where `go test` runs.
const moduleRoot = "../.."

func repoFiles(t *testing.T) []parsedFile {
	t.Helper()
	var all []parsedFile
	for _, files := range repoFilesByDir(t) {
		all = append(all, files...)
	}
	return all
}

// repoFilesByDir parses every non-test Go file of THIS module, grouped by
// directory.
func repoFilesByDir(t *testing.T) map[string][]parsedFile {
	t.Helper()
	fset := token.NewFileSet()
	byDir := make(map[string][]parsedFile)

	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == moduleRoot {
				return nil
			}
			// Dot-directories are skipped wholesale, and .claude in
			// particular: it holds .claude/worktrees/<agent>/, a complete
			// checkout of this repository per branch in flight. Walking
			// into those reports every metric in the project as declared
			// a dozen times over and every package that is not on this
			// branch as unregistered. A test that fails on work sitting
			// in another checkout is a test that gets deleted.
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata" {
				return fs.SkipDir
			}
			// A nested module is somebody else's build; its declarations
			// are not in the binary this test makes claims about.
			if _, serr := os.Stat(filepath.Join(path, "go.mod")); serr == nil {
				return fs.SkipDir
			}
			return nil
		}
		// Test files are excluded deliberately. A test that registered a
		// group would satisfy TestEveryCollectorGroupIsRegistered while
		// the binary still served nothing.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		byDir[filepath.ToSlash(filepath.Dir(path))] = append(
			byDir[filepath.ToSlash(filepath.Dir(path))],
			parsedFile{path: filepath.ToSlash(path), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(byDir) == 0 {
		t.Fatal("no Go files found: the scan is looking in the wrong place")
	}
	return byDir
}

func declaresCollectors(f *ast.File) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "Collectors" {
			return true
		}
	}
	return false
}

// registerAllArguments returns the package identifiers whose Collectors()
// call is passed to obs.RegisterAll in f.
func registerAllArguments(f *ast.File) []string {
	var pkgs []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isQualified(call.Fun, "obs", "RegisterAll") {
			return true
		}
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := inner.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Collectors" {
				continue
			}
			if ident, ok := sel.X.(*ast.Ident); ok {
				pkgs = append(pkgs, ident.Name)
			}
		}
		return true
	})
	return pkgs
}

func isQualified(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// packageStringConsts maps package-level identifiers to their string
// value, so `Namespace: namespace` resolves to "farm".
func packageStringConsts(files []parsedFile) map[string]string {
	out := make(map[string]string)
	for _, f := range files {
		for _, decl := range f.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				if s, ok := literalString(vs.Values[0]); ok {
					out[vs.Names[0].Name] = s
				}
			}
		}
	}
	return out
}

// metricNamesDeclaredIn returns every fully-qualified metric name f
// declares through a prometheus *Opts literal. A declaration whose Name
// is computed rather than written down is skipped: there is nothing to
// compare it against, and guessing would produce a false collision.
func metricNamesDeclaredIn(f parsedFile, consts map[string]string) []string {
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "prometheus" {
			return true
		}
		switch sel.Sel.Name {
		case "Opts", "CounterOpts", "GaugeOpts", "HistogramOpts", "SummaryOpts", "UntypedOpts":
		default:
			return true
		}

		fields := make(map[string]string, 3)
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if v, ok := resolveString(kv.Value, consts); ok {
				fields[key.Name] = v
			}
		}
		if fields["Name"] == "" {
			return true
		}
		parts := make([]string, 0, 3)
		for _, p := range []string{fields["Namespace"], fields["Subsystem"], fields["Name"]} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		out = append(out, strings.Join(parts, "_"))
		return true
	})
	return out
}

func resolveString(e ast.Expr, consts map[string]string) (string, bool) {
	if s, ok := literalString(e); ok {
		return s, true
	}
	if ident, ok := e.(*ast.Ident); ok {
		s, ok := consts[ident.Name]
		return s, ok
	}
	return "", false
}

func literalString(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
