package obs

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "farm"

	// Several columns feeding these labels are nullable in the schema —
	// devices.host_id, device_runtime.slot_id, slots.rack_slot — and a
	// device can legitimately be observed before its slot is known. An
	// empty label value silently breaks `group by (hub)` and every join
	// built on it, so every label value passes through orUnknown.
	unknownLabel = "unknown"
)

// ---------------------------------------------------------------------
// Label value types.
//
// Every label value in this package is a named type with a closed set of
// constants. Callers never write a label string inline, so a typo cannot
// mint a new time series and a renamed state cannot drift out of sync
// with the CHECK constraint it mirrors.
//
// FOLDING POLICY. Each helper folds a value outside its set rather than
// passing it through (which would make cardinality unbounded) — but it
// never folds toward a claim it cannot prove:
//
//	BlipKind          -> BlipOther        (blips page no one; "other" is honest)
//	RenewFailureKind  -> KindTransient    (never assert a fence without proof)
//	RecoveryOutcome   -> OutcomeFailed    (never assert a recovery without proof)
//	RecoveryTier      -> TierReprobe      (the only rung that touches no hardware)
//	HealthState       -> HealthUnknown    (a real value in the health CHECK)
//	Component         -> "unknown"        (bounded; no member would be provable)
//	ReleaseReason     -> dropped          (see LeaseReaped)
// ---------------------------------------------------------------------

// Slot names a physical position in the rack. A 3am page needs somewhere
// to walk to: device_id identifies what failed, this identifies where.
// Hub is the correlation unit — devices that die together nearly always
// share a hub or a power domain (farm.power_domains), so grouping by it
// turns twelve device alerts into one hub alert.
type Slot struct {
	Host     string // farm.hosts.id
	Hub      string // farm.hubs.usb_path, e.g. "3-1.4"
	RackSlot string // farm.slots.rack_slot, the human-facing "R1-U14-H2-P3"
}

func (s Slot) labels() []string {
	return []string{orUnknown(s.Host), orUnknown(s.Hub), orUnknown(s.RackSlot)}
}

// BlipKind classifies a transport-level failure. These are observations
// about a socket, never about a device's fitness to keep a lease.
type BlipKind string

const (
	BlipDial     BlipKind = "dial"     // could not reach the adb server on the host
	BlipReset    BlipKind = "reset"    // ECONNRESET — the exact signature in #663
	BlipEOF      BlipKind = "eof"      // peer closed mid-stream
	BlipTimeout  BlipKind = "timeout"  // read/write deadline elapsed
	BlipProtocol BlipKind = "protocol" // malformed OKAY/FAIL framing or length header
	// BlipTransportGone is adb reporting that the transport vanished
	// ("device not found", "device offline"). It is the kind most likely
	// to be mistaken for a device loss. It is not one: the device may be
	// mid-reboot under its own test, and the lease is untouched.
	BlipTransportGone BlipKind = "transport_gone"
	BlipOther         BlipKind = "other"
)

var blipKinds = []BlipKind{
	BlipDial, BlipReset, BlipEOF, BlipTimeout,
	BlipProtocol, BlipTransportGone, BlipOther,
}

// Valid reports whether k is one of the closed set of blip kinds.
func (k BlipKind) Valid() bool { return containsKind(blipKinds, k) }

// ReleaseReason mirrors the CHECK constraint on farm.leases.release_reason
// exactly. There is deliberately no connectivity member: passing
// 'device_offline' to farm.lease_release raises check_violation (SQLSTATE
// 23514), and the same value has no representation here.
type ReleaseReason string

const (
	ReasonCompleted       ReleaseReason = "completed"
	ReasonFailed          ReleaseReason = "failed"
	ReasonJobCancelled    ReleaseReason = "job_cancelled"
	ReasonMaxRuntime      ReleaseReason = "max_runtime"
	ReasonOperatorRevoked ReleaseReason = "operator_revoked"
	// ReasonHolderExpired is written only by farm.lease_reclaim. It means
	// the holder stopped renewing for ttl+grace across every control-plane
	// component, and it is the only reason in this list that represents
	// work destroyed by us rather than work that ended. In a healthy farm
	// it is flat at zero; see doc.go.
	ReasonHolderExpired ReleaseReason = "holder_expired"
	ReasonDeviceRetired ReleaseReason = "device_retired"
)

var releaseReasons = []ReleaseReason{
	ReasonCompleted, ReasonFailed, ReasonJobCancelled, ReasonMaxRuntime,
	ReasonOperatorRevoked, ReasonHolderExpired, ReasonDeviceRetired,
}

// Valid reports whether r is one of the seven permitted release reasons.
func (r ReleaseReason) Valid() bool { return containsKind(releaseReasons, r) }

// ParseReleaseReason converts untrusted text (an API request body, a
// column read back from farm.leases) into a typed reason. It is the only
// text -> ReleaseReason conversion callers should use, and it accepts
// exactly the set the database accepts, so a value rejected here is a
// value that would have raised check_violation there.
func ParseReleaseReason(s string) (ReleaseReason, bool) {
	r := ReleaseReason(s)
	if !r.Valid() {
		return "", false
	}
	return r, true
}

// RenewFailureKind distinguishes the two outcomes of a failed renewal,
// which mean opposite things.
type RenewFailureKind string

const (
	// KindFenced means farm.lease_renew returned ZERO ROWS. That is the
	// single unambiguous fence signal in the system: the lease is gone,
	// the job must abort, every ADB socket must close, nothing may be
	// written. Work has been lost, so this is page-worthy.
	KindFenced RenewFailureKind = "fenced"
	// KindTransient means the renew call did not complete: a dial error to
	// Postgres, a statement timeout, a serialization failure, a pool
	// exhaustion. It proves nothing about the lease. The supervisor
	// retries; the lease keeps its deadline; nothing is aborted.
	KindTransient RenewFailureKind = "transient"
)

var renewFailureKinds = []RenewFailureKind{KindFenced, KindTransient}

// Valid reports whether k is a known renew failure kind.
func (k RenewFailureKind) Valid() bool { return containsKind(renewFailureKinds, k) }

// HealthState mirrors the CHECK constraint on farm.device_runtime.health.
type HealthState string

const (
	HealthUnknown      HealthState = "unknown"
	HealthBooting      HealthState = "booting"
	HealthHealthy      HealthState = "healthy"
	HealthDegraded     HealthState = "degraded"
	HealthOffline      HealthState = "offline"
	HealthUnauthorized HealthState = "unauthorized"
	HealthMissing      HealthState = "missing"
	HealthRecovering   HealthState = "recovering"
	HealthQuarantined  HealthState = "quarantined"
	HealthRetired      HealthState = "retired"
)

var healthStates = []HealthState{
	HealthUnknown, HealthBooting, HealthHealthy, HealthDegraded, HealthOffline,
	HealthUnauthorized, HealthMissing, HealthRecovering, HealthQuarantined, HealthRetired,
}

// Valid reports whether h is one of the ten health states.
func (h HealthState) Valid() bool { return containsKind(healthStates, h) }

// ParseHealthState converts a health column read back from
// farm.device_runtime into a typed state.
func ParseHealthState(s string) (HealthState, bool) {
	h := HealthState(s)
	if !h.Valid() {
		return "", false
	}
	return h, true
}

// RecoveryTier names a rung of the escalation ladder tracked in
// farm.device_runtime.ladder_tier. The names match the vocabulary of
// farm.jobs.disruption_policy so a refusal reads as an obvious pairing:
// a job with disruption_policy='allow_soft_reset' refuses
// TierPortPowerCycle.
type RecoveryTier string

const (
	TierReprobe        RecoveryTier = "reprobe"          // re-read state, touch nothing
	TierReconnect      RecoveryTier = "adb_reconnect"    // reset the transport only
	TierSoftReset      RecoveryTier = "soft_reset"       // adb reboot, device-side
	TierPortPowerCycle RecoveryTier = "port_power_cycle" // cut VBUS at the slot
	TierQuarantine     RecoveryTier = "quarantine"       // give up, take it out of the pool
)

var recoveryTiers = []RecoveryTier{
	TierReprobe, TierReconnect, TierSoftReset, TierPortPowerCycle, TierQuarantine,
}

// Valid reports whether t is a known recovery tier.
func (t RecoveryTier) Valid() bool { return containsKind(recoveryTiers, t) }

// RecoveryTierFromLadder maps device_runtime.ladder_tier onto the named
// tiers, clamping at both ends. Clamping rather than formatting the
// integer keeps the label set bounded even if the ladder grows a rung
// before this package learns its name.
func RecoveryTierFromLadder(n int) RecoveryTier {
	switch {
	case n <= 0:
		return TierReprobe
	case n >= len(recoveryTiers):
		return recoveryTiers[len(recoveryTiers)-1]
	default:
		return recoveryTiers[n]
	}
}

// RecoveryOutcome is what a recovery attempt achieved. The three refusal
// outcomes are as important as the failures: they are the mechanism
// protecting live work from a well-meaning watchdog.
type RecoveryOutcome string

const (
	OutcomeRecovered RecoveryOutcome = "recovered"
	OutcomeFailed    RecoveryOutcome = "failed"
	// OutcomeRefusedPolicy: the holder's farm.jobs.disruption_policy
	// forbids this tier. The device stays broken and the job keeps it.
	OutcomeRefusedPolicy RecoveryOutcome = "refused_policy"
	// OutcomeRefusedGanged: the slot's power domain is kind='ganged', so
	// cutting power here would also cut it for a neighbour holding a live
	// lease. Refusing is correct; a rising rate here means the rack needs
	// per-port switching, not that the watchdog is broken.
	OutcomeRefusedGanged RecoveryOutcome = "refused_ganged"
	// OutcomeSuppressed: device_runtime.suppress_until is still in the
	// future, i.e. an induced reset is already in flight.
	OutcomeSuppressed RecoveryOutcome = "suppressed"
)

var recoveryOutcomes = []RecoveryOutcome{
	OutcomeRecovered, OutcomeFailed, OutcomeRefusedPolicy,
	OutcomeRefusedGanged, OutcomeSuppressed,
}

// Valid reports whether o is a known recovery outcome.
func (o RecoveryOutcome) Valid() bool { return containsKind(recoveryOutcomes, o) }

// Component names a process on the LEASE RENEWAL path — exactly the set
// passed to farm.reaper_arm, because a gap in any one of them is a gap
// that must be refunded to tenants.
//
// There is deliberately no watchdog member. The watchdog writes health
// and, by the Postgres role firewall, may not touch farm.leases at all; a
// watchdog outage must therefore never extend a lease deadline. Adding it
// here would invite adding it to reaper_arm's component list, which would
// let the health plane move lease clocks — the fusion this system exists
// to prevent.
type Component string

const (
	ComponentReaper    Component = "reaper"
	ComponentAPI       Component = "api"
	ComponentScheduler Component = "scheduler"
)

var components = []Component{ComponentReaper, ComponentAPI, ComponentScheduler}

// Valid reports whether c is a renewal-path component.
func (c Component) Valid() bool { return containsKind(components, c) }

// ---------------------------------------------------------------------
// Snapshot inputs. Each of these is one row of a GROUP BY; the setters
// publish a whole result set at once so a gauge can never keep serving a
// value whose underlying group has disappeared.
// ---------------------------------------------------------------------

// LeaseCount is one row of `GROUP BY pool_id, tenant_id` over leases in
// state 'held'.
type LeaseCount struct {
	Pool   string // farm.pools.id
	Tenant string // farm.tenants.id
	Count  int64
}

// SuspectCount is one row of `GROUP BY pool_id, tenant_id, protected`
// over leases in state 'suspect'.
//
// Protected is a label because protected suspect leases are the ones
// farm.lease_reclaim will never touch: they are held indefinitely and
// resolved by a human. Their count is therefore an actionable page, while
// unprotected suspects are usually just a supervisor mid-restart.
type SuspectCount struct {
	Pool      string
	Tenant    string
	Protected bool
	Count     int64
}

// DeviceHealthCount is one row of `GROUP BY host_id, hub, health` over
// farm.device_runtime.
type DeviceHealthCount struct {
	Host  string
	Hub   string
	State HealthState
	Count int64
}

// SlotRearmCount is one row of `GROUP BY host_id, hub` over
// farm.slots WHERE rearm_at > now().
type SlotRearmCount struct {
	Host  string
	Hub   string
	Count int64
}

// ---------------------------------------------------------------------
// Collectors.
// ---------------------------------------------------------------------

var (
	// transportBlips is DELIBERATELY NON-ALERTING. Do not write a page on
	// it, and do not let a dashboard threshold on it become one by habit.
	//
	// Paging on this metric is how DeviceFarmer/STF issue #663 happens: it
	// trains operators — and then, inevitably, automation built to silence
	// the page — to treat a transport blip as a device loss. A ~90-minute
	// ECONNRESET on a healthy device then releases it mid-run and destroys
	// hours of work. A blip means a socket died. Sockets die. The device
	// is very probably still sitting there running the test.
	//
	// Its legitimate uses are correlation (which hub is shedding sockets)
	// and feeding a human decision to DRAIN a hub for new allocations.
	// Never to end an existing lease.
	transportBlips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "adb_transport_blips_total",
		Help: "ADB transport-level failures by physical position. NON-ALERTING BY DESIGN: " +
			"a transport blip is not a device loss and must never end a lease (see #663).",
	}, []string{"host", "hub", "rack_slot", "kind"})

	// leaseReaped carries only {reason}: the seven values are known ahead
	// of time, so every child can be created at zero on Register and an
	// alert works from process start instead of waiting for the first
	// occurrence to bring the series into existence. Forensics — which
	// job, which tenant, which device — belong in farm.events, whose rows
	// carry job_id, device_id, slot_id and actor for exactly that purpose.
	leaseReaped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "lease_reaped_total",
		Help: "Leases ended, by farm.leases.release_reason. reason=holder_expired is work " +
			"destroyed by the control plane and should be flat at zero.",
	}, []string{"reason"})

	leaseHeld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "lease_held",
		Help:      "Leases in state 'held'.",
	}, []string{"pool", "tenant"})

	leaseSuspect = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "lease_suspect",
		Help: "Leases in state 'suspect'. Suspect is an ALERTING state only: it reallocates " +
			"nothing, and a heartbeat inside the grace band self-heals it at the same fence.",
	}, []string{"pool", "tenant", "protected"})

	leaseRenewFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "lease_renew_failures_total",
		Help: "Failed lease renewals. kind=fenced means farm.lease_renew returned zero rows " +
			"and the job aborted; kind=transient means the call never completed and proves nothing.",
	}, []string{"kind"})

	// deviceHealth is labelled by host and hub, not by device, precisely so
	// a hub or power-domain failure reads as one correlated cliff instead
	// of N unrelated device alerts arriving at 3am.
	deviceHealth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "device_health",
		Help: "Devices by farm.device_runtime.health, aggregated per hub so correlated " +
			"physical failure is visible as a cliff rather than as N device alerts.",
	}, []string{"state", "host", "hub"})

	recoveryAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "recovery_attempts_total",
		Help: "Recovery ladder attempts by tier and outcome. The refused_* outcomes are the " +
			"watchdog correctly declining to disturb a device that still holds live work.",
	}, []string{"tier", "outcome", "host", "hub", "rack_slot"})

	// gapBuckets: farm.reaper_arm's default p_gap_floor is 60s, so nothing
	// shorter is normally recorded at all; the 30s bucket exists so a farm
	// that lowers the floor still gets resolution. The top boundary is 6h
	// to match lease_reclaim's `g.ended_at > now() - interval '6 hours'`
	// window — past that, a recorded gap no longer shields any lease from
	// reclamation, which is exactly the condition worth seeing on a graph.
	gapBuckets = []float64{30, 60, 120, 300, 600, 1800, 3600, 10800, 21600}

	controlPlaneGap = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "control_plane_gap_seconds",
		Help: "Duration of control-plane outages recorded by farm.reaper_arm. This is OUR " +
			"downtime: it is added back to every live lease's deadline, never charged to the tenant.",
		Buckets: gapBuckets,
	}, []string{"component"})

	slotRearmPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "slot_rearm_pending",
		Help: "Slots with farm.slots.rearm_at in the future, i.e. unschedulable while the " +
			"previous holder's sockets are guaranteed severed.",
	}, []string{"host", "hub"})
)

// Snapshot publishers, one per gauge, so each gauge's series set exactly
// tracks the last result set handed to it.
var (
	leaseHeldSnap        = newGaugeSnapshot(leaseHeld)
	leaseSuspectSnap     = newGaugeSnapshot(leaseSuspect)
	deviceHealthSnap     = newGaugeSnapshot(deviceHealth)
	slotRearmPendingSnap = newGaugeSnapshot(slotRearmPending)
)

func collectors() []prometheus.Collector {
	return []prometheus.Collector{
		transportBlips,
		leaseReaped,
		leaseHeld,
		leaseSuspect,
		leaseRenewFailures,
		deviceHealth,
		recoveryAttempts,
		controlPlaneGap,
		slotRearmPending,
	}
}

// Register adds every farm metric to r and materialises the label
// combinations that are knowable up front.
//
// Pre-creating those children matters for alerting, not for tidiness: a
// counter with no observations has no series, and `increase(x[15m]) > 0`
// over a series that does not exist yields no result and therefore never
// fires. The two alerts that mean "work was destroyed" —
// lease_reaped_total{reason="holder_expired"} and
// lease_renew_failures_total{kind="fenced"} — must be armed from the
// first scrape of a fresh process, not from the first casualty.
//
// The process and Go runtime collectors are deliberately not registered
// here; that is the binary's decision, not this package's.
func Register(r *prometheus.Registry) error {
	for _, c := range collectors() {
		if err := r.Register(c); err != nil {
			return fmt.Errorf("obs: register collector: %w", err)
		}
	}
	for _, reason := range releaseReasons {
		leaseReaped.WithLabelValues(string(reason))
	}
	for _, kind := range renewFailureKinds {
		leaseRenewFailures.WithLabelValues(string(kind))
	}
	return nil
}

// ---------------------------------------------------------------------
// Counter helpers.
// ---------------------------------------------------------------------

// TransportBlip records one transport-level failure at a physical slot.
//
// THIS IS THE ONLY SYMBOL IN THIS PACKAGE THAT internal/adbwire MAY CALL.
// It accepts no lease, job, fence or holder identifier, and it returns
// nothing — there is no value for transport code to branch on. So even
// though this package also defines the lease metrics, it cannot serve as
// a side channel by which a socket error influences a lease decision. The
// import ban on internal/lease closes the front door; this signature
// closes the back one.
func TransportBlip(s Slot, kind BlipKind) {
	if !kind.Valid() {
		kind = BlipOther
	}
	transportBlips.WithLabelValues(append(s.labels(), string(kind))...).Inc()
}

// LeaseReaped records one lease that actually ended. Call it once per row
// the SQL function returned, never once per attempt: lease_reclaim and
// lease_expire_max_runtime are LIMIT-bounded and use SKIP LOCKED, so an
// attempt that lost a race ended nothing and must not be counted as work
// lost. Likewise call it only when farm.lease_release returned true.
//
//	farm.lease_release(..., p_reason)   -> that reason, if it returned true
//	farm.lease_reclaim(...)            -> ReasonHolderExpired, per row
//	farm.lease_expire_max_runtime(...) -> ReasonMaxRuntime, per row
//
// A reason outside the seven is not recorded. This mirrors the database
// rather than inventing an eighth bucket: a value the CHECK constraint
// would reject with SQLSTATE 23514 releases nothing, so there is nothing
// to count. ParseReleaseReason is the boundary for untrusted text.
func LeaseReaped(reason ReleaseReason) {
	if !reason.Valid() {
		return
	}
	leaseReaped.WithLabelValues(string(reason)).Inc()
}

// LeaseRenewFailure records a renewal that did not succeed.
//
// Pass KindFenced if and only if farm.lease_renew returned ZERO ROWS —
// the one unambiguous proof that the lease is gone. Every other failure
// (dial error, statement timeout, serialization failure, pool exhaustion)
// is KindTransient and must be retried, never treated as a fence. An
// unrecognised kind folds to KindTransient for that reason: claiming a
// fence without proof would page a human about work that is fine.
func LeaseRenewFailure(kind RenewFailureKind) {
	if !kind.Valid() {
		kind = KindTransient
	}
	leaseRenewFailures.WithLabelValues(string(kind)).Inc()
}

// RecoveryAttempt records one rung of the recovery ladder being tried at
// a physical slot. An unrecognised outcome folds to OutcomeFailed: never
// record a recovery that cannot be proven, because a false "recovered"
// suppresses the page that should have followed.
func RecoveryAttempt(s Slot, tier RecoveryTier, outcome RecoveryOutcome) {
	if !tier.Valid() {
		tier = TierReprobe
	}
	if !outcome.Valid() {
		outcome = OutcomeFailed
	}
	labels := append([]string{string(tier), string(outcome)}, s.labels()...)
	recoveryAttempts.WithLabelValues(labels...).Inc()
}

// ControlPlaneGap records one outage of a renewal-path component, i.e.
// one row inserted into farm.control_plane_gap by farm.reaper_arm. Pass
// the interval that function returned. That same interval was added to
// expires_at and reclaimable_at on every live lease, so this histogram is
// a direct measure of how much lease budget we refunded.
//
// A non-positive duration is not recorded. reaper_arm returns interval '0'
// whenever the observed gap did not exceed p_gap_floor — that is, on every
// healthy arm — while inserting a control_plane_gap row only for a positive
// gap. Observing those zeros would make _count an outage count that
// includes every non-outage. A negative duration (clock skew on a caller
// that derives the gap itself rather than passing reaper_arm's return
// value) is worse: it makes _sum decrease, PromQL reads the decrease as a
// counter reset, and the increase(..._sum) rule in doc.go then reports an
// outage budget that never happened. One observation per row in
// farm.control_plane_gap, and none otherwise.
func ControlPlaneGap(c Component, d time.Duration) {
	if d <= 0 {
		return
	}
	name := unknownLabel
	if c.Valid() {
		name = string(c)
	}
	controlPlaneGap.WithLabelValues(name).Observe(d.Seconds())
}

// ---------------------------------------------------------------------
// Gauge snapshot helpers.
// ---------------------------------------------------------------------

// SetLeaseHeld publishes the complete set of held-lease counts. Groups
// absent from counts are removed rather than left at a stale value.
func SetLeaseHeld(counts []LeaseCount) {
	samples := make([]gaugeSample, 0, len(counts))
	for _, c := range counts {
		samples = append(samples, gaugeSample{
			labels: []string{orUnknown(c.Pool), orUnknown(c.Tenant)},
			value:  float64(c.Count),
		})
	}
	leaseHeldSnap.publish(samples)
}

// SetLeaseSuspect publishes the complete set of suspect-lease counts.
func SetLeaseSuspect(counts []SuspectCount) {
	samples := make([]gaugeSample, 0, len(counts))
	for _, c := range counts {
		samples = append(samples, gaugeSample{
			labels: []string{orUnknown(c.Pool), orUnknown(c.Tenant), strconv.FormatBool(c.Protected)},
			value:  float64(c.Count),
		})
	}
	leaseSuspectSnap.publish(samples)
}

// SetDeviceHealth publishes the complete device health census.
//
// Every hub that appears in counts is published for ALL ten health
// states, zero-filling the ones with no devices. That zero-fill is the
// whole point of the metric: when a hub dies, its healthy count must fall
// to a visible 0 rather than have the series simply vanish, because
// `sum by (hub) (farm_device_health{state="healthy"}) == 0` matches a
// series at zero and matches nothing at all when the series is absent.
// One hub alert instead of twelve device alerts depends on it.
func SetDeviceHealth(counts []DeviceHealthCount) {
	byHub := make(map[hubKey]map[HealthState]int64)
	for _, c := range counts {
		state := c.State
		if !state.Valid() {
			state = HealthUnknown
		}
		k := hubKey{host: orUnknown(c.Host), hub: orUnknown(c.Hub)}
		m, ok := byHub[k]
		if !ok {
			m = make(map[HealthState]int64, len(healthStates))
			byHub[k] = m
		}
		m[state] += c.Count
	}

	samples := make([]gaugeSample, 0, len(byHub)*len(healthStates))
	for k, m := range byHub {
		for _, state := range healthStates {
			samples = append(samples, gaugeSample{
				labels: []string{string(state), k.host, k.hub},
				value:  float64(m[state]),
			})
		}
	}
	deviceHealthSnap.publish(samples)
}

// SetSlotRearmPending publishes the complete set of slots still inside
// their post-reclaim rearm quarantine. Hubs with none may be omitted:
// unlike device health this is summed, never compared against zero, so an
// absent series and a zero contribute identically.
func SetSlotRearmPending(counts []SlotRearmCount) {
	samples := make([]gaugeSample, 0, len(counts))
	for _, c := range counts {
		samples = append(samples, gaugeSample{
			labels: []string{orUnknown(c.Host), orUnknown(c.Hub)},
			value:  float64(c.Count),
		})
	}
	slotRearmPendingSnap.publish(samples)
}

// ---------------------------------------------------------------------
// Internals.
// ---------------------------------------------------------------------

type hubKey struct{ host, hub string }

type gaugeSample struct {
	labels []string
	value  float64
}

// gaugeSnapshot makes a GaugeVec track a periodically recomputed result
// set: it updates what is present and deletes what has gone away.
type gaugeSnapshot struct {
	vec  *prometheus.GaugeVec
	mu   sync.Mutex
	live map[string][]string
}

func newGaugeSnapshot(vec *prometheus.GaugeVec) *gaugeSnapshot {
	return &gaugeSnapshot{vec: vec, live: make(map[string][]string)}
}

// publish makes the vector match next exactly, summing any rows of next
// that share a label tuple.
//
// The summing is not defensive tidiness. orUnknown folds NULL host_id,
// slot_id and rack_slot to "unknown", so two distinct GROUP BY rows can
// arrive as one label tuple; setting each in turn would publish the last
// row's count instead of their total. That is a silent undercount in
// series an alert reads directly — farm_lease_suspect{protected="true"}
// is a page about work waiting for a human, and a suspect lease hidden by
// a colliding row is a page that never fires.
//
// It updates surviving series in place and only then deletes the ones
// that vanished, rather than calling Reset first. A Reset would leave a
// window in which every series is absent, and a scrape landing in that
// window manufactures a cliff in farm_device_health identical to the one
// a dead hub produces — a synthetic 3am page on a healthy farm.
func (g *gaugeSnapshot) publish(next []gaugeSample) {
	g.mu.Lock()
	defer g.mu.Unlock()

	seen := make(map[string][]string, len(next))
	total := make(map[string]float64, len(next))
	for _, s := range next {
		key := strings.Join(s.labels, "\x1f")
		if _, ok := seen[key]; !ok {
			seen[key] = s.labels
		}
		total[key] += s.value
	}
	for key, labels := range seen {
		g.vec.WithLabelValues(labels...).Set(total[key])
	}
	for key, labels := range g.live {
		if _, ok := seen[key]; !ok {
			g.vec.DeleteLabelValues(labels...)
		}
	}
	g.live = seen
}

func orUnknown(s string) string {
	if s == "" {
		return unknownLabel
	}
	return s
}

func containsKind[T comparable](set []T, v T) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
