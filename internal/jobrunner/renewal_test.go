package jobrunner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// The renewal loop, observed: the hooks must report what the holder decided
// and must never turn a failed renewal into a lease that ended.
//
// None of these tests calls t.Parallel. The counters under test are package
// globals and every assertion below is a delta across one call, so two of
// these running at once would read each other's increments. Nothing else in
// this package touches them — blindHolder deliberately wires no hooks — so
// serialising these three costs nothing.

// The four outcomes land on four series, and a failure lands on none of the
// meters whose subject is a lease that stopped.
//
// Falsify: increment fencedTotal from OnFenced, or drop the WasSuspect branch
// so a self-heal counts as an ordinary renewal; either fails here.
func TestRenewalOutcomesAreCountedApartAndEndNothing(t *testing.T) {
	hooks := renewalHooks()
	l := lease.Lease{ID: "11111111-1111-4111-8111-111111111111", DeviceID: "dev-1", JobID: "job-1", Fence: 5}

	// farm_jobrunner_fenced_total and farm_jobrunner_releases_total are the
	// two meters in this package whose subject is a lease that stopped. A
	// renewal outcome must move neither: at the instant these hooks run the
	// lease still has its deadline and the device is still ours.
	endings := scratchGatherer(t, fencedTotal, releasesTotal)

	for _, tc := range []struct {
		name string
		call func()
		want string
	}{
		{"a renewal that landed", func() { hooks.OnRenewed(l, lease.RenewResult{}) }, renewalOK},
		{
			"a renewal that healed a suspect lease",
			func() { hooks.OnRenewed(l, lease.RenewResult{WasSuspect: true}) },
			renewalSelfHealed,
		},
		{
			"a call that did not complete",
			func() { hooks.OnTransientError(l, 3, context.DeadlineExceeded, time.Second) },
			renewalTransient,
		},
		{"zero rows from farm.lease_renew", func() { hooks.OnFenced(l) }, renewalFenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := counterFamily(t, scratchGatherer(t, renewalsTotal), "farm_jobrunner_renewals_total")
			endedBefore := counterFamily(t, endings, "")

			tc.call()

			after := counterFamily(t, scratchGatherer(t, renewalsTotal), "farm_jobrunner_renewals_total")
			for _, outcome := range renewalOutcomes {
				want := before[outcome]
				if outcome == tc.want {
					want++
				}
				if after[outcome] != want {
					t.Errorf("farm_jobrunner_renewals_total{outcome=%q} moved %v -> %v, want %v: "+
						"one renewal outcome was recorded as another",
						outcome, before[outcome], after[outcome], want)
				}
			}
			endedAfter := counterFamily(t, endings, "")
			for key, was := range endedBefore {
				if now := endedAfter[key]; now != was {
					t.Errorf("a lease-ending meter (%q) moved %v -> %v for %s; a renewal outcome is "+
						"an attempt, never an ending", key, was, now, tc.name)
				}
			}
		})
	}
}

// The fleet-wide counter DeviceFarmerLeaseFenced is written over gets the same
// verdict under the same word, and a transient failure never reaches the
// fenced series.
//
// This is the assertion that matters most in this file. internal/lease decides
// between the two branches, internal/obs names them, and the wiring in between
// is the only place the two could be swapped — which would page a human about
// work that is fine, or say nothing about work that is gone.
//
// Falsify: pass obs.KindFenced from OnTransientError.
func TestTheFleetCounterGetsTheSameVerdictUnderTheSameWord(t *testing.T) {
	hooks := renewalHooks()
	l := lease.Lease{ID: "11111111-1111-4111-8111-111111111111", DeviceID: "dev-1", JobID: "job-1", Fence: 5}

	// obs's collectors are unexported, so they are read the way the binary
	// installs them: through RegisterAll on a registry of this test's own. A
	// nil logger is accepted there on purpose.
	reg := prometheus.NewRegistry()
	if err := obs.RegisterAll(reg, nil); err != nil {
		// Reported, not fatal, for the reason RegisterAll's own documentation
		// gives: a metrics fault must never stop a control plane, and it must
		// not stop this read either. The two series are checked for below.
		t.Logf("obs.RegisterAll reported gaps: %v", err)
	}
	read := func() map[string]float64 {
		return counterFamily(t, reg, "farm_lease_renew_failures_total")
	}

	// Both series exist before either hook has fired. That is obs.zeroFill's
	// doing and it is the precondition for the page: a rule over a series that
	// does not exist returns no data and never fires.
	before := read()
	for _, kind := range []string{"transient", "fenced"} {
		if _, ok := before[kind]; !ok {
			t.Fatalf("farm_lease_renew_failures_total{kind=%q} does not exist before the first "+
				"failure, so DeviceFarmerLeaseFenced is armed by the incident it should warn about", kind)
		}
	}

	hooks.OnTransientError(l, 1, context.DeadlineExceeded, time.Second)
	afterTransient := read()
	if afterTransient["transient"] != before["transient"]+1 {
		t.Errorf("kind=\"transient\" moved %v -> %v, want +1", before["transient"], afterTransient["transient"])
	}
	if afterTransient["fenced"] != before["fenced"] {
		t.Fatalf("kind=\"fenced\" moved %v -> %v on a call that did not complete: a transient failure "+
			"was reported as proof the lease is gone, which pages a human about work that is fine",
			before["fenced"], afterTransient["fenced"])
	}

	hooks.OnFenced(l)
	afterFenced := read()
	if afterFenced["fenced"] != afterTransient["fenced"]+1 {
		t.Errorf("kind=\"fenced\" moved %v -> %v on zero rows from farm.lease_renew, want +1: the one "+
			"unambiguous fence signal in the system reached no page",
			afterTransient["fenced"], afterFenced["fenced"])
	}
	if afterFenced["transient"] != afterTransient["transient"] {
		t.Errorf("kind=\"transient\" moved %v -> %v on a fencing verdict",
			afterTransient["transient"], afterFenced["transient"])
	}
}

// A real renewal loop, failing for real, reports itself — and reports the
// failure as transient, because a database it cannot dial proves nothing about
// who holds the lease.
//
// The holder here runs against a pool that will never connect, which is the
// shape of every control-plane outage this system is built to absorb. It is
// the branch that must never be mistaken for fencing: a dial error read as a
// lost lease is DeviceFarmer/STF #663 rebuilt on the control plane.
//
// Falsify: drop Hooks from the config and the wait times out with the counter
// still at its seeded value, which is exactly what /metrics showed before this
// wiring existed.
func TestAFailingHolderReportsItselfAsTransientAndNeverAsFenced(t *testing.T) {
	before := counterFamily(t, scratchGatherer(t, renewalsTotal), "farm_jobrunner_renewals_total")

	l := lease.Lease{
		ID: "11111111-1111-4111-8111-111111111111", DeviceID: "dev-1", JobID: "job-1",
		Fence: 5, Holder: "farmd-0", HolderInstance: "22222222-2222-4222-8222-222222222222",
	}
	h := lease.NewHolder(context.Background(), lease.NewStore(unreachablePool(t)), l, lease.HolderConfig{
		Interval:     2 * time.Millisecond,
		RenewTimeout: time.Second,
		RetryBase:    time.Millisecond,
		RetryMax:     2 * time.Millisecond,
		Logger:       quietLogger(),
		Hooks:        renewalHooks(),
	})
	t.Cleanup(h.Stop)

	waitFor(t, "the holder's refused renewals to reach its own meter", func() bool {
		now := counterFamily(t, scratchGatherer(t, renewalsTotal), "farm_jobrunner_renewals_total")
		return now[renewalTransient] >= before[renewalTransient]+3
	})

	after := counterFamily(t, scratchGatherer(t, renewalsTotal), "farm_jobrunner_renewals_total")
	if after[renewalFenced] != before[renewalFenced] {
		t.Errorf("outcome=%q moved %v -> %v while the database was merely unreachable: the holder is "+
			"still holding this lease and the job is still running",
			renewalFenced, before[renewalFenced], after[renewalFenced])
	}
	if after[renewalOK] != before[renewalOK] || after[renewalSelfHealed] != before[renewalSelfHealed] {
		t.Errorf("a renewal was recorded as landing against a pool that cannot connect (ok %v -> %v, "+
			"self_healed %v -> %v)", before[renewalOK], after[renewalOK],
			before[renewalSelfHealed], after[renewalSelfHealed])
	}
	// And the loop said none of this was fencing, which is what keeps the job
	// on its device for the length of the outage.
	if h.Fenced() {
		t.Error("a holder that could not dial the database reported itself fenced")
	}
}

// ---------------------------------------------------------------------------
// Reading counters
// ---------------------------------------------------------------------------

// scratchGatherer registers cs into a registry of the caller's own, so a
// package-global collector can be read without touching the process registry.
func scratchGatherer(tb testing.TB, cs ...prometheus.Collector) prometheus.Gatherer {
	tb.Helper()
	reg := prometheus.NewRegistry()
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			tb.Fatalf("registering a collector for a read: %v", err)
		}
	}
	return reg
}

// counterFamily reads one metric family out of g, keyed by its label values
// joined with "/" — "" for a metric that has none. An empty name reads every
// family, which is how the two lease-ending meters are watched together.
//
// Not Counter.Write and not prometheus/testutil: the first puts *dto.Metric in
// a signature and the second adds a diff library to a module graph this
// repository keeps to three dependencies.
func counterFamily(tb testing.TB, g prometheus.Gatherer, name string) map[string]float64 {
	tb.Helper()
	families, err := g.Gather()
	if err != nil {
		tb.Fatalf("gathering %q: %v", name, err)
	}
	out := make(map[string]float64)
	for _, f := range families {
		if name != "" && f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			values := make([]string, 0, len(m.GetLabel())+1)
			if name == "" {
				values = append(values, f.GetName())
			}
			for _, lp := range m.GetLabel() {
				values = append(values, lp.GetValue())
			}
			out[strings.Join(values, "/")] = m.GetCounter().GetValue()
		}
	}
	return out
}
