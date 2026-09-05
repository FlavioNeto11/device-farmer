package obs

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterAll installs every collector this process publishes into r and
// materialises the label combinations that are knowable at startup.
//
// # Why the caller supplies the other packages' collectors
//
// Every package that measures something exports Collectors(). This
// function cannot call them itself. internal/jobrunner, internal/node,
// internal/reaper, internal/recovery, internal/scheduler and
// internal/watchdog all import this package to record lease and recovery
// events, so importing them back is an import cycle — `go list -deps`
// confirms it for all six.
//
// internal/adbwire, internal/enroll and internal/topo do not import this
// package today, so importing those three would compile. They are still
// passed in, for one reason that matters more than the convenience:
// adbwire's whole design is that it may call obs.TransportBlip and
// nothing else (see doc.go). It has not made that call yet. Importing
// adbwire here would compile today and permanently revoke the one
// dependency this package's documentation explicitly grants it, and the
// person who later tries to record a blip from the transport layer would
// find a cycle and no explanation. A metrics surface every other package
// calls into has to stay a leaf. So the direction is uniform: packages
// call in, and the binary that owns the registry hands their collectors
// here.
//
// # The three ways a collector fails to reach the registry
//
// All three are silent in the obvious implementation, and all three end
// the same way: a metric name that is absent from /metrics, an alerting
// rule that reads an absent series, and a rule that reads an absent
// series returns no data and NEVER FIRES. A disarmed lost-work alert
// looks exactly like a farm that has lost no work. So each is turned
// into a named error here rather than into a scrape nobody re-reads.
//
//  1. The entry cannot be described at all — see safeDescribe. This one
//     does not merely go missing; it takes the process with it.
//
//  2. The name is taken by a collector with different labels or help.
//     Registry.Register rejects the newcomer and keeps the incumbent.
//     Reported as an error naming the metric, and registration continues,
//     because a process that starts with 95% of its metrics and a loud
//     error is worth more than one that refuses to start.
//
//  3. The name is taken by a DIFFERENT collector with identical
//     descriptors. Prometheus calls that AlreadyRegisteredError, which is
//     ordinarily success here — the 'all' and 'demo' roles run several
//     roles against one registry, and internal/api registers the shared
//     collectors when it owns its registry, so the same collector arrives
//     more than once by design. But AlreadyRegisteredError is descriptor
//     identity, not instance identity: two distinct vectors with the same
//     name, help and labels also produce it, and the registry keeps the
//     first. Every increment made through the second one then lands on a
//     vector nobody scrapes. Measured, not assumed: registering a second
//     identical CounterVec, incrementing it, and gathering yields zero
//     metric families. So the returned ExistingCollector is
//     checked for instance identity, and a shadow is an error. The check
//     covers supplied groups as well as this package's own collectors,
//     because a supplied collector goes dark in exactly the same way and
//     re-declaring an existing metric is a far likelier mistake in a
//     package that does not own the name.
//
// # What the caller must do with the returned error
//
// Log it. Do not exit on it. Every error above reports a metric that is
// missing, never a farm that is unsafe to run, and the error is non-nil
// in this repository as it stands: internal/recovery declares
// farm_recovery_attempts_total with {tier,outcome} while this package
// declares the same name with {tier,outcome,host,hub,rack_slot}, so one
// of the two is refused on every start until one is renamed. A binary
// that treats this as fatal turns a naming collision into a control plane
// that will not start, and a control plane that will not start is a
// control-plane gap — the one outage class this system is built to
// absorb rather than to cause.
//
// The process and Go runtime collectors are still not registered here.
// That remains the binary's decision, as Register documents.
func RegisterAll(r *prometheus.Registry, log *slog.Logger, groups ...[]prometheus.Collector) error {
	if r == nil {
		return errors.New("obs: RegisterAll: nil registry; pass the *prometheus.Registry that the /metrics handler serves")
	}
	if log == nil {
		// A nil logger is a caller that does not want the running
		// commentary, not a caller that wants a panic on the startup path.
		log = slog.New(slog.DiscardHandler)
	}

	var (
		errs    []error
		added   int
		already int
	)
	register := func(origin string, i int, d describedCollector) {
		if d.err != nil {
			log.Error("metric collector cannot be registered",
				"origin", origin, "slot", i, "err", d.err)
			errs = append(errs, fmt.Errorf("obs: %s collector %d: %w", origin, i, d.err))
			return
		}
		err := r.Register(d.c)
		if err == nil {
			added++
			return
		}
		var dup prometheus.AlreadyRegisteredError
		if errors.As(err, &dup) {
			already++
			if !sameCollector(d.c, dup.ExistingCollector) {
				log.Error("collector shadowed by a different one with identical descriptors",
					"origin", origin, "metrics", d.metrics())
				errs = append(errs, fmt.Errorf(
					"obs: %s is served by a different collector with identical descriptors, so every "+
						"increment made through this one lands on a vector nobody scrapes; delete "+
						"whichever of the two duplicate declarations is redundant (origin %s)",
					d.metrics(), origin))
				return
			}
			log.Debug("metric collector already registered",
				"origin", origin, "metrics", d.metrics())
			return
		}
		log.Error("metric refused by the registry; it will be absent from /metrics",
			"origin", origin, "metrics", d.metrics(), "err", err)
		errs = append(errs, fmt.Errorf(
			"obs: metric %s was refused by the registry, so it is absent from /metrics and every "+
				"alerting rule over it returns no data and never fires; rename whichever of the two "+
				"colliding collectors is wrong, or fix the descriptor named here: %w",
			d.metrics(), err))
	}

	for i, d := range describeAll(collectors()) {
		register("obs", i, d)
	}
	for gi, g := range groups {
		ds := describeAll(g)
		origin := groupOrigin(gi, ds)
		for i, d := range ds {
			register(origin, i, d)
		}
	}

	zeroFill()

	if len(errs) > 0 {
		log.Error("farm metrics registered with gaps; alerting rules over the missing series return no data and never fire",
			"collectors", added, "already_present", already, "failed", len(errs))
	} else {
		log.Info("farm metrics registered",
			"collectors", added, "already_present", already)
	}

	return errors.Join(errs...)
}

// zeroFill creates every child of this package's vectors whose label
// values are knowable before anything has happened.
//
// An alerting rule reads a time series. `farm_lease_suspect{protected="true"} > 0`
// over a series that does not exist yields an empty vector, which is not
// "0" — it is "no data", and no data fires nothing. So a gauge that
// springs into existence at the moment it first goes bad is a gauge whose
// alert was armed by the incident it was supposed to warn about. Register
// pre-created two counters; every other series here was missing from
// /metrics until its first occurrence.
//
// The open dimensions — pool, tenant, host, hub, rack_slot — cannot be
// enumerated at startup, so each is filled with unknownLabel. That is not
// a placeholder value invented for this: orUnknown already folds a NULL
// hosts.id, slots.rack_slot or device_runtime.slot_id to exactly this
// string, so "unknown" is a value these labels genuinely take, and the
// filled child is a real member of the label space rather than a decoy.
// Filling them is what makes `sum(farm_slot_rearm_pending)` and
// `farm_lease_suspect{protected="true"}` evaluate to 0 on a fresh process
// instead of to nothing at all.
//
// The closed dimensions are filled from the enums themselves rather than
// from a second list, so a tier or health state added to the CHECK
// constraint and mirrored here is zero-filled without anyone remembering
// to come back to this function. Checked against the live schema rather
// than against the Go source: device_runtime_health_check permits exactly
// the ten members of healthStates, leases_release_reason_check exactly
// the seven of releaseReasons, and farm.reaper_arm's p_components default
// is exactly ARRAY['reaper','api','scheduler'].
//
// One known hole, stated rather than hidden: the four gauges are
// published through gaugeSnapshot, which deletes the series it published
// last cycle and no longer sees. A child seeded here is not in that
// bookkeeping, so it survives every publish — unless a real result row
// arrives carrying the same all-"unknown" labels and later disappears,
// which deletes the seed with it. That needs a NULL pool_id or host_id on
// a real row; closing it properly belongs in gaugeSnapshot, not here.
func zeroFill() {
	// Non-alerting by design (see doc.go) but seeded all the same: the blip
	// counter's job is hub-level correlation on a dashboard, and a panel
	// that reads "No data" until the first socket dies is a panel nobody
	// trusts when it finally shows something. Existing at zero arms no rule
	// that the first real blip would not have armed anyway.
	for _, kind := range blipKinds {
		transportBlips.WithLabelValues(unknownLabel, unknownLabel, unknownLabel, string(kind))
	}

	// The two tombstones. Both are alerted on with increase() > 0, both
	// mean work the control plane destroyed, and both must therefore be
	// armed from the first scrape rather than from the first casualty.
	for _, reason := range releaseReasons {
		leaseReaped.WithLabelValues(string(reason))
	}
	for _, kind := range renewFailureKinds {
		leaseRenewFailures.WithLabelValues(string(kind))
	}

	leaseHeld.WithLabelValues(unknownLabel, unknownLabel)

	// The warning-ahead-of-time series. A protected suspect lease is never
	// auto-reclaimed: it waits for a human indefinitely, which is correct,
	// and is exactly why nobody notices it without a rule that can fire.
	for _, protected := range []bool{false, true} {
		leaseSuspect.WithLabelValues(unknownLabel, unknownLabel, strconv.FormatBool(protected))
	}

	// SetDeviceHealth already zero-fills every health state for every hub it
	// has seen. Before the first census it has seen none, so the correlated
	// hub-failure rule has nothing to read at all; this covers that window.
	for _, state := range healthStates {
		deviceHealth.WithLabelValues(string(state), unknownLabel, unknownLabel)
	}

	for _, tier := range recoveryTiers {
		for _, outcome := range recoveryOutcomes {
			recoveryAttempts.WithLabelValues(
				string(tier), string(outcome), unknownLabel, unknownLabel, unknownLabel)
		}
	}

	// Only the three named components. ControlPlaneGap folds an unrecognised
	// Component to unknownLabel, but that fold is our own bug rather than a
	// state of the farm, and seeding a series for it would arm a rule on a
	// typo.
	for _, c := range components {
		controlPlaneGap.WithLabelValues(string(c))
	}

	slotRearmPending.WithLabelValues(unknownLabel, unknownLabel)
}

// describedCollector is one entry of a Collectors() slice paired with the
// metric names it describes — or with the reason it could not be
// described, which is a fault that must never reach Registry.Register.
type describedCollector struct {
	c     prometheus.Collector
	names []string
	err   error
}

// metrics names the metrics an operator has to go and look at, so an
// error can point at a name they can grep for rather than at an opaque
// collector.
func (d describedCollector) metrics() string {
	if len(d.names) == 0 {
		// Legitimate: a collector that describes nothing is registered
		// unchecked, and prometheus supports that on purpose.
		return "(unchecked collector, describes no metrics)"
	}
	return strings.Join(d.names, ",")
}

func describeAll(cs []prometheus.Collector) []describedCollector {
	out := make([]describedCollector, len(cs))
	for i, c := range cs {
		names, err := safeDescribe(c)
		out[i] = describedCollector{c: c, names: names, err: err}
	}
	return out
}

// safeDescribe returns the fully-qualified metric names c describes, or
// an error if describing it is not survivable.
//
// A nil check is not enough and a deferred recover around Register is
// useless. The realistic fault is an entry of a Collectors() slice that
// is an uninitialised field — a nil *prometheus.CounterVec — which is a
// NON-nil interface holding a nil pointer, so `c == nil` is false.
// Registry.Register then calls Describe on it, and Register runs Describe
// on a goroutine of its own that does not recover: the nil dereference is
// an unrecovered panic on a goroutine no caller can defer against, and
// the process dies before /metrics has ever been served. Verified against
// client_golang v1.20.5, whose stack names Registry.Register.func1.
//
// So the describe happens here first, on a goroutine this function owns,
// which is the last point at which the fault is still an error rather
// than a crash.
func safeDescribe(c prometheus.Collector) ([]string, error) {
	if c == nil {
		return nil, errors.New("entry is nil; fix the Collectors() slice that produced it")
	}

	ch := make(chan *prometheus.Desc, 16)
	var panicked any
	go func() {
		// Deferred calls run last-registered-first, so the recover runs
		// before the close: a Describe that panics part way through still
		// releases the range below. The close is also the happens-before
		// edge that publishes panicked to the reader.
		defer close(ch)
		defer func() { panicked = recover() }()
		c.Describe(ch)
	}()

	var names []string
	nilDesc := false
	for d := range ch {
		if d == nil {
			// Desc.String dereferences its receiver, so naming this one
			// would panic on the goroutine the recover above does not
			// cover. Register dereferences it too, one field earlier.
			nilDesc = true
			continue
		}
		names = append(names, fqName(d))
	}
	// A panic is reported ahead of a nil descriptor: it is the more severe
	// fault and it explains the truncated descriptor stream.
	if panicked != nil {
		return nil, fmt.Errorf(
			"Describe panicked (%v); the entry is almost certainly an uninitialised vector, which "+
				"is a non-nil Collector holding a nil pointer and would crash the process inside "+
				"Registry.Register — initialise it in the package that exports Collectors()",
			panicked)
	}
	if nilDesc {
		return nil, errors.New(
			"Describe emitted a nil descriptor, which crashes Registry.Register; " +
				"the collector must send only descriptors built by prometheus.NewDesc")
	}
	return names, nil
}

// groupOrigin names a supplied group by a metric one of its collectors
// describes. "group 4" alone makes the reader count arguments at the
// RegisterAll call site to work out which package produced the bad slice;
// "group 4 (farm_recovery_cycles_total)" makes them grep for it. A group
// in which nothing could be described has nothing to name it with.
func groupOrigin(i int, ds []describedCollector) string {
	for _, d := range ds {
		if d.err == nil && len(d.names) > 0 {
			return fmt.Sprintf("group %d (%s)", i, d.names[0])
		}
	}
	return fmt.Sprintf("group %d", i)
}

// sameCollector reports whether existing is the very same instance as
// ours.
//
// The obvious spelling, `ours == existing`, is a latent panic: comparing
// two interface values whose identical dynamic type is non-comparable —
// any collector implemented on a struct value with a slice, map or func
// field — is a runtime error, and it would fire on the startup path, on a
// collector supplied by another package, in the branch meant to make
// startup safer. The type switch keeps every comparison a pointer
// comparison instead.
//
// A collector that is not one of the three vector kinds answers true,
// which claims nothing: this check exists to prove a shadow, and an
// unrecognised type is not evidence of one. Under-reporting a shadow
// costs a metric; a false one would reject a legitimate startup.
func sameCollector(ours, existing prometheus.Collector) bool {
	switch o := ours.(type) {
	case *prometheus.CounterVec:
		e, ok := existing.(*prometheus.CounterVec)
		return ok && e == o
	case *prometheus.GaugeVec:
		e, ok := existing.(*prometheus.GaugeVec)
		return ok && e == o
	case *prometheus.HistogramVec:
		e, ok := existing.(*prometheus.HistogramVec)
		return ok && e == o
	default:
		return true
	}
}

// fqName digs the metric name out of a Desc.
//
// client_golang exposes no accessor for it — Desc's fields are all
// unexported and String() is the only way in. Parsing a String() is
// unpleasant, and the alternative is worse: the raw form is 300 characters
// of help text and label lists, and an error that buries the one word the
// reader needs is an error they skim. The whole Desc is returned unchanged
// if the format ever moves, so a client_golang upgrade degrades the
// message and breaks nothing.
func fqName(d *prometheus.Desc) string {
	s := d.String()
	const marker = `fqName: "`
	i := strings.Index(s, marker)
	if i < 0 {
		return s
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return s
	}
	return rest[:j]
}
