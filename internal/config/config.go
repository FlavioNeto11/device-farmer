// Package config turns the process environment into a validated Config for
// every farmd role.
//
// Three rules shape this package.
//
//   - Values that also exist as CHECK constraints in migrations/00001_core.sql
//     are re-validated here. A lease TTL below the schema floor should fail at
//     startup with a sentence an operator can act on, not at acquire time with
//     SQLSTATE 23514 while a tenant is waiting for a device.
//
//   - Timing knobs are validated against each other, not just individually. A
//     renew interval and a TTL are each defensible in isolation and lethal in
//     combination.
//
//   - One cross-field assertion is load bearing rather than cosmetic: the slot
//     rearm interval MUST strictly exceed the node proxy's self-fence timeout.
//     See Config.Validate for the hazard it prevents.
//
// Nothing here may release, shorten, or otherwise touch a lease. This package
// only decides what the process is allowed to start with.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Environment variable names. They are constants because several of them are
// named in cross-field validation errors, and an error that names the wrong
// knob is worse than no error at all.
const (
	EnvDatabaseURL      = "DATABASE_URL"
	EnvDBMaxConns       = "FARM_DB_MAX_CONNS"
	EnvDBConnectTimeout = "FARM_DB_CONNECT_TIMEOUT"

	EnvComponent     = "FARM_COMPONENT"
	EnvLogLevel      = "FARM_LOG_LEVEL"
	EnvShutdownGrace = "FARM_SHUTDOWN_GRACE"

	EnvAPIAddr     = "FARM_API_ADDR"
	EnvMetricsAddr = "FARM_METRICS_ADDR"
	EnvNodeAddr    = "FARM_NODE_ADDR"
	EnvAPIBaseURL  = "FARM_API_URL"

	EnvLeaseTTL             = "FARM_LEASE_TTL"
	EnvLeaseGrace           = "FARM_LEASE_GRACE"
	EnvLeaseRenewInterval   = "FARM_LEASE_RENEW_INTERVAL"
	EnvLeaseWitnessInterval = "FARM_LEASE_WITNESS_INTERVAL"
	EnvLeaseWitnessMaxExt   = "FARM_LEASE_WITNESS_MAX_EXTENSIONS"
	EnvSlotRearm            = "FARM_SLOT_REARM"

	EnvReaperInterval  = "FARM_REAPER_INTERVAL"
	EnvReaperBatch     = "FARM_REAPER_BATCH"
	EnvReaperGapFloor  = "FARM_REAPER_GAP_FLOOR"
	EnvReaperComponent = "FARM_REAPER_COMPONENTS"
	EnvHeartbeatEvery  = "FARM_HEARTBEAT_INTERVAL"

	EnvNodeSelfFence   = "FARM_NODE_SELF_FENCE_TIMEOUT"
	EnvFenceMargin     = "FARM_FENCE_SAFETY_MARGIN"
	EnvNodeADBEndpoint = "FARM_ADB_ENDPOINT"
	EnvNodeHostID      = "FARM_HOST_ID"

	EnvWatchdogInterval = "FARM_WATCHDOG_INTERVAL"
	EnvMigrationsTable  = "FARM_MIGRATIONS_TABLE"
	EnvMigrationsDir    = "FARM_MIGRATIONS_DIR"

	// U8 — charge policy. The band an idle device is held inside, and the
	// cadence of the loop that holds it there.
	EnvChargeMinPct   = "FARM_CHARGE_MIN_PCT"
	EnvChargeMaxPct   = "FARM_CHARGE_MAX_PCT"
	EnvChargeInterval = "FARM_CHARGE_INTERVAL"
)

// U15 — artifact GC.
//
// The blob sweep behind POST /api/v1/artifacts/gc only considers a blob older
// than this. artifacts.Store.Put commits the bytes and THEN writes the row
// that names them, so a blob with no row is either garbage or an upload in
// flight, and age is the only thing that tells the two apart. The floor
// mirrors api.MinBlobGCGrace the way the lease floors mirror the schema's
// CHECK constraints: the api package refuses a shorter grace at construction,
// and a process that would inevitably be refused should not finish booting.
const (
	EnvArtifactGCGrace     = "FARM_ARTIFACT_GC_GRACE"
	DefaultArtifactGCGrace = time.Hour
	MinArtifactGCGrace     = time.Minute
)

// U12 — node agent and topology discovery.
//
// These reach `farmd node` and, for the token, the recovery ladder. They were
// previously either read raw from the environment in cmd/farmd (the token) or
// not read at all: topo.Config has thirteen fields and the node role filled
// five, which left removal reconciliation, the hub filter, the naming map and
// the dry-run switch unreachable from any manifest.
const (
	// EnvNodeToken is the shared secret between the recovery ladder and
	// every node agent's HTTP surface. It is a secret: Summary reports whether
	// it is set and never its value.
	EnvNodeToken = "FARM_NODE_TOKEN"

	// EnvSysfsRoot is the directory the USB tree is read from. It is the bus
	// device directory itself, not the top of sysfs: the reader lists it and
	// expects entries named "usb3", "3-1.4" and so on.
	EnvSysfsRoot = "FARM_SYSFS_ROOT"

	// EnvTopoOverrides is the path of a JSON file holding topo.Overrides: hub
	// USB paths to the token printed after "H", and slot USB paths to whole
	// labels. Empty means no overrides.
	EnvTopoOverrides = "FARM_TOPO_OVERRIDES"

	EnvTopoRetireVanished    = "FARM_TOPO_RETIRE_VANISHED"
	EnvTopoMaxRetireFraction = "FARM_TOPO_MAX_RETIRE_FRACTION"
	EnvTopoDryRun            = "FARM_TOPO_DRY_RUN"
	EnvTopoMinPorts          = "FARM_TOPO_MIN_PORTS"
	EnvTopoAdoptEmpty        = "FARM_TOPO_ADOPT_EMPTY"
	EnvTopoIncludeRootHubs   = "FARM_TOPO_INCLUDE_ROOT_HUBS"
	EnvTopoInclude           = "FARM_TOPO_INCLUDE"
	EnvTopoExclude           = "FARM_TOPO_EXCLUDE"
	EnvTopoInterval          = "FARM_TOPO_INTERVAL"
	EnvTopoCallTimeout       = "FARM_TOPO_CALL_TIMEOUT"
)

// Defaults for the U12 knobs. Each mirrors the constant the consuming package
// would apply on its own for a zero value; they are restated here so that
// Summary prints the number the node will actually run with, and a test in
// this package holds the two copies together.
const (
	// DefaultSysfsRoot mirrors topo.DefaultSysfsRoot. The old node role passed
	// "/sys", which lists block/, bus/, class/ … and finds no USB device in any
	// of them — an empty host, reported as a successful scan.
	DefaultSysfsRoot = "/sys/bus/usb/devices"

	// DefaultTopoMaxRetireFraction mirrors topo.DefaultMaxRetireFraction.
	DefaultTopoMaxRetireFraction = 0.25
	// DefaultTopoMinPorts mirrors topo.DefaultMinHubPorts.
	DefaultTopoMinPorts = 3
	// DefaultTopoInterval mirrors both topo.DefaultInterval and
	// node.DefaultDiscoverInterval, which must agree: the node agent paces
	// discovery and budgets one pass at this interval.
	DefaultTopoInterval = 5 * time.Minute
	// DefaultTopoCallTimeout mirrors topo.DefaultCallTimeout.
	DefaultTopoCallTimeout = 30 * time.Second

	// MaxHubPorts mirrors CHECK (port_count BETWEEN 1 AND 32) on farm.hubs. A
	// port floor above it would let no hub through, ever, and say nothing.
	MaxHubPorts = 32
)

// U7 — runtime database role.
//
// FARM_DB_ROLE names the Postgres role a loop process assumes on every pooled
// connection (SET ROLE once per physical connection; see openPool in
// cmd/farmd). Empty means the process runs as the login user, which is what
// every deployment did before the knob existed. The three values are the
// NOLOGIN roles migrations/00002_lease.sql creates; migration 00015 is what
// lets the login user assume them.
//
// Each role belongs to exactly one process, and dbRoleForProcess is the only
// place that says which. The firewall is meaningless the other way round: a
// reaper started as farm_scheduler could read health, a scheduler started as
// farm_reaper could not allocate.
//
// The map is an allowlist. A process absent from it — the api, the loops that
// read and write everything, a role added later such as the charge-policy
// mesh — has no role to assume: it runs as the login user, and refuses the
// variable by name rather than silently taking another process's role. Giving
// such a process a role of its own is one entry here plus the grants a
// migration makes; every message below derives its lists from the map so
// none goes stale when that happens.
const (
	EnvDBRole = "FARM_DB_ROLE"

	DBRoleReaper    = "farm_reaper"
	DBRoleScheduler = "farm_scheduler"
	DBRoleWatchdog  = "farm_watchdog"
)

var dbRoleForProcess = map[string]string{
	"reaper":    DBRoleReaper,
	"scheduler": DBRoleScheduler,
	"watchdog":  DBRoleWatchdog,
}

// U9 — battery health.
//
// The thresholds the watchdog's swell detector flags on, in the units the
// schema stores: decidegrees Celsius and percentage points. They are policy,
// which is why they are here and not in a CHECK (00010 says so in as many
// words): a lab that runs its phones hot on purpose retunes a manifest, not a
// migration.
const (
	EnvBatteryTempRise = "FARM_BATTERY_TEMP_RISE_DC_PER_MIN"
	EnvBatteryTempMax  = "FARM_BATTERY_TEMP_MAX_DC"
	EnvBatteryDrain    = "FARM_BATTERY_DRAIN_PCT_PER_HOUR"

	// 2.0 C/min. A phone under a heavy test warms at tenths of a degree a
	// minute; a cell whose case is climbing two full degrees a minute is
	// being heated from inside, and it does not matter what temperature it
	// is passing through on the way.
	DefaultBatteryTempRiseDCPerMin = 20
	// 45.0 C. The top of the charging band every major cell vendor
	// specifies, and the point at which Android's own thermal service starts
	// throttling; a case above it on a shelf in a ventilated room is not
	// being warmed by the room.
	DefaultBatteryTempMaxDC = 450
	// 15 points an hour, IDLE and on a powered port. A handset doing nothing
	// on VBUS should gain charge or hold it; one that loses a sixth of its
	// capacity an hour while plugged in cannot hold what it is given.
	DefaultBatteryDrainPctPerHour = 15
)

// U10 — fence proxy (host side).
//
// The three TLS paths turn the proxy on, together or not at all. The rest
// only mean something once it is on. They are read by every role and served
// by the node role alone; see Fence.
const (
	EnvFenceTLSCert      = "FARM_FENCE_TLS_CERT"
	EnvFenceTLSKey       = "FARM_FENCE_TLS_KEY"
	EnvFenceTLSCA        = "FARM_FENCE_TLS_CA"
	EnvFenceListen       = "FARM_FENCE_LISTEN"
	EnvFenceAdvertise    = "FARM_FENCE_ADVERTISE"
	EnvFencePollInterval = "FARM_FENCE_POLL_INTERVAL"

	// DefaultFenceListen is where the proxy binds. 5038 is the ADB server's
	// own port plus one: the proxy stands directly in front of 127.0.0.1:5037,
	// and an operator who reads "10.20.0.11:5038" in farm.hosts.adb_endpoint
	// can tell at a glance that it is adb, fronted. It sits outside 5554-5587,
	// which the Android emulator claims in console/adbd pairs on any machine
	// that runs one, and nothing else in the adb tool chain uses it.
	DefaultFenceListen = ":5038"

	// DefaultFencePollInterval is how often the proxy re-reads this host's
	// fence floors: one query per host per interval, never per connection.
	DefaultFencePollInterval = 2 * time.Second
)

// U11 — fence proxy (client side).
//
// The three files that make every ADB client in this process dial each host's
// fence proxy over mutual TLS instead of the ADB server's bare port. Unset,
// nothing changes on the wire: plain TCP, no admission preamble, the fence
// enforced in PostgreSQL and honoured by this client. Set, every connection
// presents the certificate and announces its class — and, for a job, its
// fence — so a host running the proxy (FARM_FENCE_TLS_*, the host half) can
// refuse a stale fence at the socket. All three or none: a certificate
// without a CA to verify the proxy, or a CA without a certificate to present,
// is a deployment that would fail its first handshake with a message an
// operator has to decode, so it is refused here with one that names the knob.
//
// The certificate's farm://<class>/<service> URI SAN is what the proxy takes
// the credential class from; the class word in the preamble is advisory. One
// certificate per process means one class per process, so a role that runs
// several components (`all`) presents the same class for all of them.
//
// Set it on the processes that reach a host's ADB server across the network
// — jobrunner, recovery, watchdog, api — and not on farmd node: the node's
// own clients dial the ADB server on loopback, behind the proxy rather than
// through it, and a certificate there would be presented to a server that
// does not speak TLS.
const (
	EnvFenceClientCert = "FARM_FENCE_CLIENT_CERT"
	EnvFenceClientKey  = "FARM_FENCE_CLIENT_KEY"
	EnvFenceClientCA   = "FARM_FENCE_CLIENT_CA"

	// A SECOND client certificate, for the control class, and it is a second
	// one rather than a replacement because the proxy reads the class from the
	// CERTIFICATE and this process needs two.
	//
	// The api dials maintenance for POST /devices/{id}/exec, which runs on a
	// device that may hold no lease at all. Control class must present a
	// devpath and a fence or it is malformed, so promoting the process's one
	// certificate to control would break exec on every free device. Two
	// certificates, two adbwire clients, two classes, one process.
	//
	// The CA is shared: it is the same proxy on the other end.
	EnvFenceControlCert = "FARM_FENCE_CONTROL_CERT"
	EnvFenceControlKey  = "FARM_FENCE_CONTROL_KEY"
)

// Mirrors of CHECK constraints in migrations/00001_core.sql. Duplicated on
// purpose: the database is the authority, but a process that would inevitably
// violate the authority should never finish booting.
const (
	// farm.jobs.ttl   CHECK (ttl   >= interval '10 minutes')
	MinLeaseTTL = 10 * time.Minute
	// farm.jobs.grace CHECK (grace >= interval '5 minutes')
	MinLeaseGrace = 5 * time.Minute
	// farm.device_runtime.battery_temp_dc CHECK (... BETWEEN -400 AND 1500),
	// migrations/00010; farm.battery_readings.temp_dc carries the same one.
	MaxBatteryTempDC = 1500
)

// MinRenewAttempts is how many renewal opportunities must fit inside one TTL.
// A holder has to survive losing two consecutive renewals — a rolling API
// deploy, a Postgres failover, a GC pause — without its lease going suspect.
const MinRenewAttempts = 3

// The witness cadence triple (U1, LEASE-09).
//
// Three cadences make the on-device witness worth anything, and they are
// derived from one another HERE — the one package every consumer can import —
// so that no role can set them apart:
//
//   - the witness interval (FARM_LEASE_WITNESS_INTERVAL) is how often a
//     placement presents its evidence through farm.lease_witness;
//   - the marker interval is how often the job rewrites the marker file on
//     the device: MarkersPerWitnessTick times per witness tick, so a single
//     lost write never leaves a tick without fresh evidence;
//   - the evidence window (lease.WitnessConfig.MaxEvidenceAge) is how old the
//     last acknowledged write may be and still be presented: EvidenceWindow
//     marker intervals, which tolerates two consecutive lost writes and not a
//     third.
//
// The window is measured in MARKER intervals, not witness intervals, because
// it answers "when did this process last touch the device?", and the marker
// is the thing that touches it. Measured in witness ticks — three of them, the
// lease package's own fallback — it would tolerate eleven consecutive failed
// writes at the default cadence and let a device nobody has reached for six
// minutes read as demonstrably alive.
const (
	MarkersPerWitnessTick = 4
	EvidenceWindow        = 3

	// MinLeaseWitnessInterval is the floor on FARM_LEASE_WITNESS_INTERVAL.
	//
	// The marker cadence follows the witness cadence at one quarter, so an
	// unbounded witness interval is an unbounded marker interval: at 1s the
	// farm would run an ADB shell round trip against every leased device every
	// 250ms — a host with fifty-six phones DoS-ing its own adb server — and
	// issue one UPDATE per leased device per second on farm.leases for
	// evidence no reaper would ever look at that closely. At 30s the marker
	// lands every 7.5s, which is about the rate a busy step already talks to
	// its device, and a witness lands ten times inside the smallest grace band
	// the schema allows.
	MinLeaseWitnessInterval = 30 * time.Second
)

// MarkerIntervalFor is the marker cadence for a witness cadence.
func MarkerIntervalFor(witness time.Duration) time.Duration {
	return witness / MarkersPerWitnessTick
}

// MaxEvidenceAgeFor is the evidence window for a marker cadence.
func MaxEvidenceAgeFor(marker time.Duration) time.Duration {
	return EvidenceWindow * marker
}

// U8 — charge policy.
//
// MaxChargeGateHold mirrors node.MaxChargeGateHold, the longest the host agent
// will keep a port dark on one assertion before restoring power by itself. It
// is duplicated here rather than imported because this package is a leaf and
// must stay one; internal/chargepolicy asserts the two are equal, so a change
// to either without the other fails a test rather than a rack.
//
// The policy loop asserts each gate for TWO of its intervals, so that a single
// missed cycle — a rolling deploy, a slow database — does not let the agent
// hand power back to a phone the policy still means to hold. That is why the
// interval is bounded at half the cap and not at the cap itself.
const MaxChargeGateHold = 30 * time.Minute

// Defaults. Chosen so that an operator who sets only DATABASE_URL gets a
// configuration that satisfies every assertion below.
const (
	DefaultLogLevel      = "info"
	DefaultShutdownGrace = 30 * time.Second

	DefaultAPIAddr     = ":8080"
	DefaultMetricsAddr = ":9090"
	DefaultNodeAddr    = ":8082"
	DefaultAPIBaseURL  = "http://127.0.0.1:8080"

	DefaultLeaseTTL             = 15 * time.Minute
	DefaultLeaseGrace           = 30 * time.Minute
	DefaultLeaseRenewInterval   = 90 * time.Second
	DefaultLeaseWitnessInterval = 2 * time.Minute
	DefaultLeaseWitnessMaxExt   = 12 // farm.lease_witness p_max_extensions default
	DefaultSlotRearm            = 35 * time.Second

	DefaultReaperInterval   = 10 * time.Second
	DefaultReaperBatch      = 100
	DefaultReaperGapFloor   = 60 * time.Second
	DefaultHeartbeatEvery   = 5 * time.Second
	DefaultWatchdogEvery    = 5 * time.Second
	DefaultNodeSelfFence    = 20 * time.Second
	DefaultFenceMargin      = 5 * time.Second
	DefaultADBEndpoint      = "127.0.0.1:5037"
	DefaultDBMaxConns       = 8
	DefaultDBConnectTimeout = 10 * time.Second

	DefaultMigrationsTable = "public.goose_db_version"

	// U8 — charge policy. 40–80% is the band the lithium research in
	// REQUIREMENTS.md (HW-03) points at: a cell at 80% stores materially less
	// energy than one at 100%, and 40% keeps an idle handset far from the flat
	// state that costs a device outright. Two minutes is well inside the
	// agent's hold cap and coarse enough that the loop is one listing per host
	// and a handful of renewals, not a load.
	DefaultChargeMinPct   = 40
	DefaultChargeMaxPct   = 80
	DefaultChargeInterval = 2 * time.Minute

	// MetricsOff is the value of FARM_METRICS_ADDR that means "serve no
	// metrics listener". An empty string cannot mean it: an empty environment
	// variable is indistinguishable from an unset one here (see loader.raw),
	// so opting out needs a word.
	MetricsOff = "off"
)

// DefaultReaperComponents lists every component whose downtime must be
// refunded to live leases. A component on the renewal path that is missing
// from this list is a blind spot: farm.reaper_arm takes min(beat_at) over the
// components it is given, so an outage in a component it is not given is
// invisible to gap accounting — the failure mode called out as BLOCKER 8 in
// migrations/00002_lease.sql.
//
// It deliberately carries one name more than farm.reaper_arm's own SQL default
// of ('reaper','api','scheduler'): the jobrunner holds the leases whose
// deadlines the reaper enforces, so a jobrunner outage is precisely the outage
// the refund exists for. The SQL default is only reachable by a hand-typed
// psql call; every farmd process passes this list explicitly.
//
// The janitor is NOT here, and that is a decision rather than an omission. It
// beats (see roleComponents), and it looks like one more control-plane loop
// worth watching — but it is not on the renewal path. It closes step rows
// whose lease is already gone; it cannot extend a lease, so its being down
// never stops a holder from renewing. Refunding its downtime would hand every
// live lease an extension for an outage in a plane that cannot touch a lease
// clock — the same fusion of clocks that keeps the watchdog and the recovery
// ladder off this list, arriving through housekeeping instead of health. The
// test for a name here is "can this component's silence prevent a renewal",
// and the janitor's cannot.
//
// The list is what a farm RUNS, not what it could run. Since migration 00012 a
// name here with no farm.component_heartbeat row makes farm.reaper_arm REFUSE
// to arm: the reaper reclaims nothing, says so at WARN, on GET /api/v1/reaper
// and on farm_reaper_unbeaten_components, and arms by itself once the
// component beats. The mirror case is quieter: a component that beat once and
// was then scaled to zero leaves a stale row that refunds every second since it
// left to every live lease, on every arm. Remove a component from this list
// when you remove it from the farm — and delete its heartbeat row.
var DefaultReaperComponents = []string{"reaper", "api", "scheduler", "jobrunner"}

// roleComponents is what each farmd role actually writes to
// farm.component_heartbeat. It is the difference between a knob that renames a
// process and a knob that renames one row, and it is what lets the BLOCKER 8
// assertion below cover a process that runs six components rather than one.
//
// An empty list means the role beats for nothing at all: migrate exits, and
// ctl talks to the API over HTTP and never touches Postgres.
//
// The watchdog's real key carries a ":<host id>" suffix, because health is
// per-host and one shared "watchdog" row would let a healthy host's beat hide
// a dead one. The suffix is not known until farm.hosts is read, so the base
// name stands for it here. internal/node documents the same intent for the
// node agent, though cmd/farmd does not yet pass a per-host name through.
var roleComponents = map[string][]string{
	"api":          {"api"},
	"scheduler":    {"scheduler"},
	"reaper":       {"reaper"},
	"recovery":     {"recovery"},
	"jobrunner":    {"jobrunner"},
	"janitor":      {"janitor"},
	"chargepolicy": {"chargepolicy"},
	"watchdog":     {"watchdog"},
	"node":         {"node", "enroll"},
	"all":          {"api", "scheduler", "reaper", "recovery", "jobrunner", "janitor", "chargepolicy", "watchdog"},
	"demo":         {"api", "scheduler", "reaper", "recovery", "jobrunner", "janitor", "chargepolicy", "watchdog"},
	"ctl":          {},
	"migrate":      {},
}

// Lease holds the only clock that may end a lease on a timer: lease liveness.
// Job progress and device health have their own clocks elsewhere and are never
// folded into these values.
type Lease struct {
	// TTL is how long a lease survives without a renewal before the suspect
	// sweep marks it. Marking a lease suspect releases nothing.
	TTL time.Duration
	// Grace is the extra band after TTL before a suspect lease becomes
	// reclaimable. TTL+Grace is the only automatic release deadline that is
	// not written down by the user.
	Grace time.Duration
	// RenewInterval is the wall-clock period of farm.lease_renew. It runs on
	// a different wire from the ADB data path; that separation is the whole
	// mechanism, so this value must never be derived from device traffic.
	RenewInterval time.Duration
	// WitnessInterval is the period of farm.lease_witness: how often a
	// placement presents its on-device proof that the work is running. The
	// jobrunner starts one witness loop per placement
	// (internal/jobrunner/witness.go), fed by the marker runner.Marker keeps
	// fresh on the device, and this value is that loop's cadence.
	//
	// It is validated against Grace because that relationship is what makes
	// a witness worth anything: farm.lease_reclaim honours a witness only
	// while it is younger than one grace period, so a witness written less
	// often than that proves nothing, and an operator who widened this
	// without widening Grace would be turning the witness off while
	// believing it on. It has a floor as well, because the marker cadence
	// follows it: see MinLeaseWitnessInterval.
	WitnessInterval time.Duration
	// MaxWitnessExtensions caps consecutive witness-only extensions so a
	// wedged holder cannot hold a device forever on device-side evidence.
	MaxWitnessExtensions int
	// SlotRearm is passed as p_rearm to farm.lease_release and
	// farm.lease_reclaim. See Config.Validate.
	SlotRearm time.Duration
}

// MarkerInterval is how often a placement rewrites its on-device marker under
// this witness cadence. Derived, never set: see MarkersPerWitnessTick.
func (l Lease) MarkerInterval() time.Duration { return MarkerIntervalFor(l.WitnessInterval) }

// MaxEvidenceAge is how old the last acknowledged marker write may be and
// still be presented as a witness. Derived, never set: see EvidenceWindow.
func (l Lease) MaxEvidenceAge() time.Duration { return MaxEvidenceAgeFor(l.MarkerInterval()) }

// Reaper holds the knobs of the only automatic release path in the system.
type Reaper struct {
	Interval          time.Duration
	Batch             int
	GapFloor          time.Duration
	Components        []string
	HeartbeatInterval time.Duration
}

// Charge holds the charge policy's knobs: the band an idle device is held
// inside, and how often the loop re-asserts the gates that hold it there.
//
// The policy acts ONLY on devices with no live lease. These values decide
// which idle phones are parked and held off VBUS; none of them can shorten,
// end, or otherwise touch a lease, and the loop that reads them does not
// import the lease package.
type Charge struct {
	// MinPct is the state of charge at or below which a held device is
	// released and returned to service.
	MinPct int
	// MaxPct is the state of charge above which an idle device is parked and
	// its port held dark.
	MaxPct int
	// Interval is the loop period. Every held gate is re-asserted once per
	// interval for a hold of two intervals, so one missed cycle costs nothing
	// and two hand the port back to the agent's dead-man's switch.
	Interval time.Duration
}

// Node holds the host agent's knobs, and the two numbers the fence proxy it
// serves is sized by.
type Node struct {
	// SelfFenceTimeout is the proxy's staleness budget: how old its last
	// successful read of this host's fence floors may be and still admit a
	// NEW connection. It reaches the proxy as fenceproxy.Policy.MaxStaleness.
	// It tears nothing down — a live connection ends only on a fencing fact,
	// never on blindness — and it is validated against Lease.SlotRearm whether
	// or not the proxy is on, because the rearm window is what makes serving
	// from a view this old safe.
	SelfFenceTimeout time.Duration
	// FenceSafetyMargin is the slack required between SelfFenceTimeout and
	// Lease.SlotRearm. Zero reduces the assertion to "strictly greater".
	FenceSafetyMargin time.Duration
	ADBEndpoint       string
	HostID            string
	// Token authenticates the recovery ladder to the node agent's HTTP
	// surface, which can cut power to a port holding a live lease. It is
	// never printed: RedactedDatabaseURL has a shape it can blank, this has
	// none, so Summary says only whether it is set.
	Token string
}

// Topo holds the node agent's USB discovery knobs, one per topo.Config field
// the role can set. Values here decide which hubs become slots and whether a
// port the kernel stopped reporting is marked 'maintenance'. None of them can
// reach a lease: discovery does not read farm.leases, and a retired slot only
// stops being scheduled for the NEXT allocation.
type Topo struct {
	// SysfsRoot is the directory listed for USB devices. See EnvSysfsRoot.
	SysfsRoot string
	// OverridesPath names a JSON file of topo.Overrides, or is empty.
	OverridesPath string

	// RetireVanished turns removal reconciliation on: a port the kernel no
	// longer reports is marked 'maintenance', and one it reports again is
	// restored.
	//
	// Off by default, and the default must stay off. The first pass a new
	// deployment runs is against a host whose slots were seeded by hand or by
	// an earlier tool, with USB paths that may not match what this scan sees;
	// with reconciliation on, that pass would find every seeded slot missing
	// and retire the lot. The mass-removal bound catches most of that, but a
	// host with one seeded slot is under it. Turn this on once one pass has
	// been read and the slots it registered are the slots the host has.
	RetireVanished bool
	// MaxRetireFraction is the share of a host's active slots one pass may
	// retire before it refuses and asks for a human. In (0, 1].
	MaxRetireFraction float64
	// DryRun plans every slot and label and writes nothing.
	DryRun bool

	// MinPorts ignores hubs with fewer ports: the two-port hub inside a
	// monitor is not a rack position.
	MinPorts int
	// AdoptEmpty adopts a hub that carries nothing at all, for a rack cabled
	// before it is populated. Without it a hub joins the farm only once a
	// phone is in it.
	AdoptEmpty bool
	// IncludeRootHubs lets motherboard ports that currently hold an Android
	// device become slots. Never the other root ports: those are the host's
	// own keyboard, NIC and BMC.
	IncludeRootHubs bool
	// Include and Exclude are hub USB paths ("3-1.4") always or never
	// adopted. A path may not be in both.
	Include []string
	Exclude []string

	// Interval paces discovery passes and is also the budget for one pass:
	// the node agent cancels a pass that has not finished by the time the
	// next is due.
	Interval time.Duration
	// CallTimeout bounds one database statement inside a pass.
	CallTimeout time.Duration
}

// Battery holds the early-detection thresholds of the watchdog's swell
// detector (U9). Units are the schema's: decidegrees and percentage points.
type Battery struct {
	// TempRiseDCPerMin flags a cell whose temperature is climbing faster than
	// this over the detector's short window, whatever its absolute value.
	TempRiseDCPerMin int
	// TempMaxDC flags any reading above this.
	TempMaxDC int
	// DrainPctPerHour flags an IDLE device on a port whose charge gate is not
	// off losing charge faster than this. A device mid-job is never judged
	// by it: a test can legitimately outrun a charger.
	DrainPctPerHour int
}

// Fence is the host-side fence proxy (U10): the mTLS listener a node agent
// puts in front of its ADB server so that a connection is admitted only while
// the fence it presents is at or above farm.devices.fence_floor.
//
// It is opt-in by TLS material. With CertFile, KeyFile and CAFile all unset
// nothing changes: the agent advertises its ADB server directly and no fence
// is enforced at the device. With all three set, the node role serves the
// proxy and advertises IT as farm.hosts.adb_endpoint, so every process that
// dials a host dials the proxy — and keeps dialling it while it is down,
// because the alternative is a farm that silently falls back to the unfenced
// server the moment the thing fencing it stops. One or two of the three is
// refused: a proxy with a certificate and no client CA would admit anyone,
// and one with a CA and no certificate cannot listen at all.
type Fence struct {
	CertFile string
	KeyFile  string
	CAFile   string
	// Listen is the proxy's bind address.
	Listen string
	// Advertise is what the agent writes to farm.hosts.adb_endpoint while the
	// proxy is on. Empty means the node agent derives it from Listen and the
	// address it already knows for this machine; see node.AdvertiseAddr.
	Advertise string
	// PollInterval is how often the proxy re-reads this host's floors. It
	// must be shorter than Node.SelfFenceTimeout, or the view is stale before
	// it is refreshed.
	PollInterval time.Duration
}

// Enabled reports whether the TLS material that turns the proxy on is
// present. Load has already refused a partial set by the time this is asked.
func (f Fence) Enabled() bool {
	return f.CertFile != "" && f.KeyFile != "" && f.CAFile != ""
}

// FenceClient is the client half of the fence proxy: what this process
// presents to a host's proxy, when it presents anything at all.
type FenceClient struct {
	CertFile string
	KeyFile  string
	CAFile   string

	// TLS is the client configuration built from the three files, and nil
	// when they are unset. Every adbwire client in the process takes it
	// through adbwire.WithTLS; nil leaves the client on plain TCP.
	TLS *tls.Config

	// Leaf is the parsed client certificate, for the startup summary. Nil
	// when TLS was supplied in code rather than loaded from files.
	Leaf *x509.Certificate

	// certVar and keyVar name the environment variables these files came from.
	// There are two of these in a Config now — the maintenance client and the
	// control client — and a refusal that named the wrong pair would send an
	// operator to edit a variable that is not the one they got wrong. Empty
	// means the maintenance names, which is what a FenceClient built in code
	// or in a test gets.
	certVar, keyVar string
}

// vars names the two variables this instance was loaded from. The CA is shared
// between both instances and is therefore always the one variable.
func (f FenceClient) vars() (certVar, keyVar string) {
	certVar, keyVar = f.certVar, f.keyVar
	if certVar == "" {
		certVar = EnvFenceClientCert
	}
	if keyVar == "" {
		keyVar = EnvFenceClientKey
	}
	return certVar, keyVar
}

// Enabled reports whether this process dials hosts through the fence proxy.
func (f FenceClient) Enabled() bool { return f.TLS != nil }

// problems is the all-or-none check plus a parse of whatever was set, so a
// certificate that will not load is refused at boot and not at the first dial
// an hour later, against a host that then reports an opaque handshake error.
func (f FenceClient) problems() []error {
	set := 0
	for _, v := range []string{f.CertFile, f.KeyFile, f.CAFile} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		return nil
	}
	if set != 3 {
		certVar, keyVar := f.vars()
		return []error{fmt.Errorf("%s, %s and %s must be set together or not at all; "+
			"a certificate without a CA cannot verify the proxy, and a CA without a "+
			"certificate has nothing to present to it",
			certVar, keyVar, EnvFenceClientCA)}
	}
	_, _, err := f.build()
	if err != nil {
		return []error{err}
	}
	return nil
}

// build loads the files into a TLS configuration. TLS 1.3 only: it is what
// the proxy serves, and the choice is not the client's to negotiate down.
func (f FenceClient) build() (*tls.Config, *x509.Certificate, error) {
	certVar, keyVar := f.vars()
	cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("%s / %s: %w", certVar, keyVar, err)
	}
	leaf := cert.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", certVar, err)
		}
	}
	caPEM, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvFenceClientCA, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("%s (%s) holds no PEM certificate", EnvFenceClientCA, f.CAFile)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
	}, leaf, nil
}

// describe is the Summary line's value.
func (f FenceClient) describe() string {
	switch {
	case f.TLS == nil:
		certVar, _ := f.vars()
		return fmt.Sprintf("off (%s/KEY/CA unset: ADB servers are dialed in the clear, no preamble is sent, "+
			"and a revoked fence is refused in PostgreSQL only)", certVar)
	case f.Leaf == nil:
		return "mTLS to each host's fence proxy (certificate supplied in code)"
	}
	uris := make([]string, 0, len(f.Leaf.URIs))
	for _, u := range f.Leaf.URIs {
		uris = append(uris, u.String())
	}
	san := "NONE — the proxy will refuse this certificate; it needs a farm://<class>/<service> URI SAN"
	if len(uris) > 0 {
		san = strings.Join(uris, ",")
	}
	return fmt.Sprintf("mTLS to each host's fence proxy (cert %s, subject %q, san %s, expires %s)",
		f.CertFile, f.Leaf.Subject.CommonName, san, f.Leaf.NotAfter.UTC().Format(time.RFC3339))
}

// controlFenceClient reads the control-class certificate.
//
// The CA is borrowed from the maintenance client, because it is the same proxy
// being verified — but only when a control certificate was actually asked for.
// Borrowing it unconditionally would give this instance one of its three files
// on every farm that configured the maintenance client alone, and the
// all-or-none rule would then refuse a configuration nobody wrote.
func controlFenceClient(l loader) FenceClient {
	f := FenceClient{
		CertFile: l.str(EnvFenceControlCert, ""),
		KeyFile:  l.str(EnvFenceControlKey, ""),
		certVar:  EnvFenceControlCert,
		keyVar:   EnvFenceControlKey,
	}
	if f.CertFile != "" || f.KeyFile != "" {
		f.CAFile = l.str(EnvFenceClientCA, "")
	}
	return f
}

// Config is the whole of farmd's environment-derived configuration.
type Config struct {
	// Component is the name written by farm.component_beat. It must match the
	// name the reaper looks for, or this process's downtime is not refunded.
	//
	// Wire it through ComponentFor rather than reading it directly: a process
	// that runs several components writes several rows, and this field names
	// none of them.
	Component string
	LogLevel  string
	// ShutdownGrace bounds in-flight request draining ONLY. It must never be
	// used as a deadline to release leases: see cmd/farmd/main.go.
	ShutdownGrace time.Duration

	DatabaseURL      string
	DBMaxConns       int32
	DBConnectTimeout time.Duration
	// DBRole is the Postgres role every pooled connection assumes, or "" for
	// the login user. Validated against the process role: see EnvDBRole.
	DBRole string

	APIAddr string
	// MetricsAddr is where a role that is not the API binds its own /metrics
	// listener. The API serves /metrics off its own listener, so for that role
	// this is a second address carrying the same registry — which is what lets
	// one scrape config point at one port for every role in the farm.
	// MetricsOff disables it.
	MetricsAddr string
	NodeAddr    string
	APIBaseURL  string

	Lease       Lease
	Reaper      Reaper
	Node        Node
	Topo        Topo
	Battery     Battery
	Charge      Charge
	Fence       Fence
	FenceClient FenceClient

	// FenceControl is the control-class client certificate, for the live screen
	// and input path. It reuses FenceClient's type — same three files, same
	// all-or-none rule — and shares FenceClient's CA, so setting it needs two
	// variables rather than three. Unset, the control routes report themselves
	// unavailable and name what is missing rather than dialling in the clear.
	FenceControl FenceClient

	WatchdogInterval time.Duration
	MigrationsTable  string
	MigrationsDir    string

	// ArtifactGCGrace is how old a blob must be before the artifact sweep
	// will consider it. See EnvArtifactGCGrace.
	ArtifactGCGrace time.Duration

	// role is the subcommand this process was started as. It is kept apart
	// from Component because Component is operator-overridable via
	// FARM_COMPONENT, and the BLOCKER 8 assertion in Validate has to key on
	// what the process actually *does*, not on what it was renamed to.
	role string
}

type options struct{ requireDB bool }

// Option adjusts what Load insists on.
type Option func(*options)

// WithoutDatabase relaxes the DATABASE_URL requirement, for roles such as ctl
// that talk to the API rather than to Postgres.
func WithoutDatabase() Option { return func(o *options) { o.requireDB = false } }

// Load reads the environment and validates the result. component is the
// default value for FARM_COMPONENT, normally the subcommand name.
func Load(component string, opts ...Option) (*Config, error) {
	o := options{requireDB: true}
	for _, fn := range opts {
		fn(&o)
	}

	var l loader
	cfg := &Config{
		role:          component,
		Component:     l.str(EnvComponent, component),
		LogLevel:      l.str(EnvLogLevel, DefaultLogLevel),
		ShutdownGrace: l.dur(EnvShutdownGrace, DefaultShutdownGrace),

		DatabaseURL:      l.str(EnvDatabaseURL, ""),
		DBMaxConns:       l.num32(EnvDBMaxConns, DefaultDBMaxConns),
		DBConnectTimeout: l.dur(EnvDBConnectTimeout, DefaultDBConnectTimeout),
		DBRole:           l.str(EnvDBRole, ""),

		APIAddr:     l.str(EnvAPIAddr, DefaultAPIAddr),
		MetricsAddr: l.str(EnvMetricsAddr, DefaultMetricsAddr),
		NodeAddr:    l.str(EnvNodeAddr, DefaultNodeAddr),
		APIBaseURL:  l.str(EnvAPIBaseURL, DefaultAPIBaseURL),

		Lease: Lease{
			TTL:                  l.dur(EnvLeaseTTL, DefaultLeaseTTL),
			Grace:                l.dur(EnvLeaseGrace, DefaultLeaseGrace),
			RenewInterval:        l.dur(EnvLeaseRenewInterval, DefaultLeaseRenewInterval),
			WitnessInterval:      l.dur(EnvLeaseWitnessInterval, DefaultLeaseWitnessInterval),
			MaxWitnessExtensions: l.num(EnvLeaseWitnessMaxExt, DefaultLeaseWitnessMaxExt),
			SlotRearm:            l.dur(EnvSlotRearm, DefaultSlotRearm),
		},
		Reaper: Reaper{
			Interval:          l.dur(EnvReaperInterval, DefaultReaperInterval),
			Batch:             l.num(EnvReaperBatch, DefaultReaperBatch),
			GapFloor:          l.dur(EnvReaperGapFloor, DefaultReaperGapFloor),
			Components:        l.list(EnvReaperComponent, DefaultReaperComponents),
			HeartbeatInterval: l.dur(EnvHeartbeatEvery, DefaultHeartbeatEvery),
		},
		Node: Node{
			SelfFenceTimeout:  l.dur(EnvNodeSelfFence, DefaultNodeSelfFence),
			FenceSafetyMargin: l.dur(EnvFenceMargin, DefaultFenceMargin),
			ADBEndpoint:       l.str(EnvNodeADBEndpoint, DefaultADBEndpoint),
			HostID:            l.str(EnvNodeHostID, ""),
			Token:             l.str(EnvNodeToken, ""),
		},
		Topo: Topo{
			SysfsRoot:         l.str(EnvSysfsRoot, DefaultSysfsRoot),
			OverridesPath:     l.str(EnvTopoOverrides, ""),
			RetireVanished:    l.boolean(EnvTopoRetireVanished, false),
			MaxRetireFraction: l.float(EnvTopoMaxRetireFraction, DefaultTopoMaxRetireFraction),
			DryRun:            l.boolean(EnvTopoDryRun, false),
			MinPorts:          l.num(EnvTopoMinPorts, DefaultTopoMinPorts),
			AdoptEmpty:        l.boolean(EnvTopoAdoptEmpty, false),
			IncludeRootHubs:   l.boolean(EnvTopoIncludeRootHubs, false),
			Include:           l.list(EnvTopoInclude, nil),
			Exclude:           l.list(EnvTopoExclude, nil),
			Interval:          l.dur(EnvTopoInterval, DefaultTopoInterval),
			CallTimeout:       l.dur(EnvTopoCallTimeout, DefaultTopoCallTimeout),
		},
		Charge: Charge{
			MinPct:   l.num(EnvChargeMinPct, DefaultChargeMinPct),
			MaxPct:   l.num(EnvChargeMaxPct, DefaultChargeMaxPct),
			Interval: l.dur(EnvChargeInterval, DefaultChargeInterval),
		},
		Fence: Fence{
			CertFile:     l.str(EnvFenceTLSCert, ""),
			KeyFile:      l.str(EnvFenceTLSKey, ""),
			CAFile:       l.str(EnvFenceTLSCA, ""),
			Listen:       l.str(EnvFenceListen, DefaultFenceListen),
			Advertise:    l.str(EnvFenceAdvertise, ""),
			PollInterval: l.dur(EnvFencePollInterval, DefaultFencePollInterval),
		},
		FenceClient: FenceClient{
			CertFile: l.str(EnvFenceClientCert, ""),
			KeyFile:  l.str(EnvFenceClientKey, ""),
			CAFile:   l.str(EnvFenceClientCA, ""),
		},
		FenceControl: controlFenceClient(l),

		Battery: Battery{
			TempRiseDCPerMin: l.num(EnvBatteryTempRise, DefaultBatteryTempRiseDCPerMin),
			TempMaxDC:        l.num(EnvBatteryTempMax, DefaultBatteryTempMaxDC),
			DrainPctPerHour:  l.num(EnvBatteryDrain, DefaultBatteryDrainPctPerHour),
		},
		WatchdogInterval: l.dur(EnvWatchdogInterval, DefaultWatchdogEvery),
		MigrationsTable:  l.str(EnvMigrationsTable, DefaultMigrationsTable),
		MigrationsDir:    l.str(EnvMigrationsDir, ""),
		ArtifactGCGrace:  l.dur(EnvArtifactGCGrace, DefaultArtifactGCGrace),
	}

	// Parse errors first: validating a value we failed to parse produces
	// noise that buries the real problem.
	if err := errors.Join(l.errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// The role-dependent DATABASE_URL requirement is folded into the same
	// report as everything else. Returning it separately would make an
	// operator with two problems do two deploys to find both, which is the
	// failure this package's "report everything at once" rule exists to
	// prevent.
	errs := cfg.problems()
	if o.requireDB && cfg.DatabaseURL == "" {
		errs = append(errs, fmt.Errorf("%s is required for the %q role and is empty",
			EnvDatabaseURL, cfg.Component))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n%w", errors.Join(errs...))
	}
	if cfg.FenceClient.CertFile != "" {
		// problems() already parsed the files; this is the same parse kept.
		tlsCfg, leaf, err := cfg.FenceClient.build()
		if err != nil {
			return nil, fmt.Errorf("invalid configuration:\n%w", err)
		}
		cfg.FenceClient.TLS, cfg.FenceClient.Leaf = tlsCfg, leaf
	}
	if cfg.FenceControl.CertFile != "" {
		tlsCfg, leaf, err := cfg.FenceControl.build()
		if err != nil {
			return nil, fmt.Errorf("invalid configuration:\n%w", err)
		}
		cfg.FenceControl.TLS, cfg.FenceControl.Leaf = tlsCfg, leaf
	}
	return cfg, nil
}

// Role returns the subcommand this configuration was loaded for. It is what
// the process does; Component is what it is called.
func (c *Config) Role() string { return c.role }

// multiplexed reports whether this process runs several components at once.
//
// Such a process cannot be renamed by FARM_COMPONENT: each component it runs
// writes its own farm.component_heartbeat row, and one name cannot stand for
// all of them without one component's beat hiding another's silence. The node
// role is deliberately not here — its enroll loop is subordinate to the agent
// that runs it, on one host, and the two are named together.
func (c *Config) multiplexed() bool {
	switch c.role {
	case "all", "demo":
		return true
	}
	return false
}

// beats returns the canonical names of the components this process runs.
func (c *Config) beats() []string {
	if names, ok := roleComponents[c.role]; ok {
		return names
	}
	// A role this package has not been told about is assumed to beat under its
	// own name. Guessing wrong here costs an unnecessary assertion; guessing
	// the other way would silently drop the BLOCKER 8 check for it.
	return []string{c.role}
}

// ComponentFor returns the farm.component_heartbeat key this process must use
// for the named component.
//
// FARM_COMPONENT renames the component a process IS: `farmd scheduler` with
// FARM_COMPONENT=scheduler-a beats as "scheduler-a", which is how two
// schedulers, or a canary and its fleet, are told apart in gap accounting. It
// does not rename the components a process merely CONTAINS: inside `all` and
// `demo` each component keeps its canonical name, and Load refuses an override
// there rather than accepting one and discarding it.
func (c *Config) ComponentFor(component string) string {
	if !c.multiplexed() && component == c.role {
		return c.Component
	}
	return component
}

// HeartbeatComponents returns every farm.component_heartbeat key this process
// writes, in the order the role runs them. The watchdog and node entries are
// base names; their real keys carry a ":<host id>" suffix.
func (c *Config) HeartbeatComponents() []string {
	canon := c.beats()
	out := make([]string, 0, len(canon))
	for _, name := range canon {
		out = append(out, c.ComponentFor(name))
	}
	return out
}

// MetricsDisabled reports whether the operator asked for no metrics listener.
func (c *Config) MetricsDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(c.MetricsAddr), MetricsOff)
}

// opensPool reports whether this role connects through cmd/farmd's pool, which
// is the only place FARM_DB_ROLE takes effect. ctl talks to the API and never
// to Postgres; migrate runs as the schema owner on its own database/sql handle,
// necessarily — the migration that GRANTs the runtime roles cannot itself run
// as one of them. For those two a stray FARM_DB_ROLE in a shared environment
// is read and applied to nothing, for the same reason bindsMetrics lets them
// ignore a metrics address.
func (c *Config) opensPool() bool {
	switch c.role {
	case "ctl", "migrate":
		return false
	}
	return true
}

// dbRoleOwner names the process a runtime role belongs to, or "" when the
// string is not a runtime role at all.
func dbRoleOwner(role string) string {
	for process, r := range dbRoleForProcess {
		if r == role {
			return process
		}
	}
	return ""
}

// dbRoleProcesses and dbRoles list the allowlist for refusals, in a fixed
// order, so a message names exactly what the map covers today.
func dbRoleProcesses() []string { return slices.Sorted(maps.Keys(dbRoleForProcess)) }
func dbRoles() []string         { return slices.Sorted(maps.Values(dbRoleForProcess)) }

// bindsMetrics reports whether this role has a process to hang a metrics
// listener off at all. ctl is a one-shot command against the API and migrate
// exits; neither has a scrape interval to be visible across, and validating an
// address they will never bind would make a stray variable in a shared
// environment break the one command an operator reaches for when the control
// plane is the thing being investigated.
func (c *Config) bindsMetrics() bool {
	switch c.role {
	case "ctl", "migrate":
		return false
	}
	return !c.MetricsDisabled()
}

// MetricsListenAddr returns the address this process must bind its own
// /metrics listener on, and whether it must bind one at all.
//
// It answers false when metrics are switched off, when the role is not a
// long-running one, and when this role's API server already publishes the same
// registry at the same address — for `api`, `all` and `demo` a second bind
// there would fail on a port that is already serving exactly what was asked
// for.
func (c *Config) MetricsListenAddr() (string, bool) {
	if !c.bindsMetrics() {
		return "", false
	}
	switch c.role {
	case "api", "all", "demo":
		if sameListenAddr(c.MetricsAddr, c.APIAddr) {
			return "", false
		}
	}
	return c.MetricsAddr, true
}

// Validate checks every invariant except the presence of DATABASE_URL, which
// depends on the role and is enforced by Load. All violations are reported at
// once so an operator fixes a manifest in one edit rather than five deploys.
func (c *Config) Validate() error {
	errs := c.problems()
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n%w", errors.Join(errs...))
}

// problems collects every invariant violation. Split out of Validate so Load
// can add the role-dependent DATABASE_URL check to the same report.
func (c *Config) problems() []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// -----------------------------------------------------------------
	// THE STARTUP ASSERTION THAT KEEPS TWO JOBS OFF ONE PHONE.
	//
	// farm.lease_reclaim sets slots.rearm_at = now() + p_rearm and
	// farm.lease_release does the same. That window is not decoration: it is
	// what makes it safe for the host's fence proxy to admit a connection
	// from a view of the floors that is up to SelfFenceTimeout old.
	// -----------------------------------------------------------------
	minRearm := c.Node.SelfFenceTimeout + c.Node.FenceSafetyMargin
	if c.Lease.SlotRearm <= c.Node.SelfFenceTimeout || c.Lease.SlotRearm < minRearm {
		fail(`%s (%s) must exceed %s (%s) by at least %s (%s), and is not.

When a lease is reclaimed the device gets a new fence floor and the slot is
parked for the rearm interval. The host's fence proxy learns the new floor on
its next poll and refuses the old holder from then on. While it cannot reach
this database it keeps admitting connections against the floors it last read,
for as long as that view is younger than the self-fence timeout, and refuses
new ones after that. For the length of the rearm window the device belongs to
nobody, so a blind proxy admitting the previous holder inside it costs nothing.

If the slot became schedulable again before that view is guaranteed to have
aged out, the scheduler could hand the same physical device to a new job while
a blind proxy still forwards the previous job's commands to it.

Two tenants then share one phone: shell commands interleave, installs land in
the wrong session, and neither side sees an error. Nothing downstream can
detect this, which is why it is refused here.

Fix by raising %s above %s + %s, or by lowering the proxy's self-fence timeout.`,
			EnvSlotRearm, c.Lease.SlotRearm,
			EnvNodeSelfFence, c.Node.SelfFenceTimeout,
			EnvFenceMargin, c.Node.FenceSafetyMargin,
			EnvSlotRearm, EnvNodeSelfFence, EnvFenceMargin)
	}
	if c.Node.SelfFenceTimeout <= 0 {
		fail("%s must be positive; a non-positive self-fence timeout means the node "+
			"proxy never checks whether its fence is still valid", EnvNodeSelfFence)
	}
	if c.Node.FenceSafetyMargin < 0 {
		fail("%s must not be negative", EnvFenceMargin)
	}

	// ---- fence proxy (U10) -------------------------------------------
	// The PEMs are opened here, at startup, rather than at the first
	// handshake: a proxy that starts, advertises itself as the host's
	// endpoint and then fails every handshake has made the host unreachable
	// while looking healthy. Refusing to boot is the visible failure.
	set := 0
	for _, p := range []string{c.Fence.CertFile, c.Fence.KeyFile, c.Fence.CAFile} {
		if p != "" {
			set++
		}
	}
	switch {
	case set == 0:
	case set < 3:
		fail("%s, %s and %s turn the host-side fence proxy on together, and only %d of the "+
			"three %s set. A proxy with a certificate and no client CA would admit anyone; "+
			"one with a CA and no certificate cannot listen. Set all three or none",
			EnvFenceTLSCert, EnvFenceTLSKey, EnvFenceTLSCA, set, plural(set, "is", "are"))
	default:
		if _, err := tls.LoadX509KeyPair(c.Fence.CertFile, c.Fence.KeyFile); err != nil {
			fail("%s (%q) and %s (%q) do not load as a certificate and its private key: %v",
				EnvFenceTLSCert, c.Fence.CertFile, EnvFenceTLSKey, c.Fence.KeyFile, err)
		}
		if pem, err := os.ReadFile(c.Fence.CAFile); err != nil {
			fail("%s: %v", EnvFenceTLSCA, err)
		} else if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			fail("%s (%q) holds no PEM-encoded certificate; the proxy would trust no client "+
				"and refuse every connection to this host", EnvFenceTLSCA, c.Fence.CAFile)
		}
	}
	if err := checkListenAddr(c.Fence.Listen); err != nil {
		fail("%s (%q): %v", EnvFenceListen, c.Fence.Listen, err)
	}
	if c.Fence.Advertise != "" {
		if err := checkAdvertiseAddr(c.Fence.Advertise); err != nil {
			fail("%s (%q): %v", EnvFenceAdvertise, c.Fence.Advertise, err)
		}
	}
	if c.Fence.PollInterval <= 0 {
		fail("%s must be positive", EnvFencePollInterval)
	} else if c.Node.SelfFenceTimeout > 0 && c.Fence.PollInterval >= c.Node.SelfFenceTimeout {
		fail("%s (%s) must be shorter than %s (%s). The proxy admits a new connection only "+
			"against a floor observation younger than the self-fence timeout, so a poll "+
			"that lands less often than that leaves the view stale before it is refreshed "+
			"and refuses every new connection on a perfectly healthy farm",
			EnvFencePollInterval, c.Fence.PollInterval, EnvNodeSelfFence, c.Node.SelfFenceTimeout)
	}

	// ---- lease liveness ---------------------------------------------
	if c.Lease.TTL < MinLeaseTTL {
		fail("%s is %s but farm.jobs.ttl is CHECK-constrained to >= %s; every acquire "+
			"would fail with SQLSTATE 23514", EnvLeaseTTL, c.Lease.TTL, MinLeaseTTL)
	}
	if c.Lease.Grace < MinLeaseGrace {
		fail("%s is %s but farm.jobs.grace is CHECK-constrained to >= %s; every acquire "+
			"would fail with SQLSTATE 23514", EnvLeaseGrace, c.Lease.Grace, MinLeaseGrace)
	}
	if c.Lease.RenewInterval <= 0 {
		fail("%s must be positive", EnvLeaseRenewInterval)
	} else if c.Lease.RenewInterval*time.Duration(MinRenewAttempts) > c.Lease.TTL {
		fail("%s (%s) leaves fewer than %d renewal attempts inside %s (%s). A holder "+
			"must be able to lose two consecutive renewals — a rolling deploy, a "+
			"Postgres failover, a long GC pause — without its lease going suspect",
			EnvLeaseRenewInterval, c.Lease.RenewInterval, MinRenewAttempts,
			EnvLeaseTTL, c.Lease.TTL)
	}
	if c.Lease.WitnessInterval <= 0 {
		fail("%s must be positive", EnvLeaseWitnessInterval)
	} else if c.Lease.WitnessInterval < MinLeaseWitnessInterval {
		fail("%s (%s) is below the %s floor. The on-device marker is rewritten %d times per "+
			"witness tick, so this would have every leased device answering a shell round "+
			"trip every %s and every lease taking an UPDATE per tick, for evidence the reaper "+
			"never reads that closely", EnvLeaseWitnessInterval, c.Lease.WitnessInterval,
			MinLeaseWitnessInterval, MarkersPerWitnessTick, c.Lease.MarkerInterval())
	} else if c.Lease.WitnessInterval*2 > c.Lease.Grace {
		fail("%s (%s) is too coarse for %s (%s). farm.lease_reclaim ignores any witness "+
			"older than one grace period, so a witness that lands less than twice per "+
			"grace band protects nothing", EnvLeaseWitnessInterval, c.Lease.WitnessInterval,
			EnvLeaseGrace, c.Lease.Grace)
	}
	if c.Lease.MaxWitnessExtensions < 1 {
		fail("%s must be at least 1", EnvLeaseWitnessMaxExt)
	}

	// ---- control-plane liveness --------------------------------------
	if c.Reaper.Interval <= 0 {
		fail("%s must be positive", EnvReaperInterval)
	}
	if c.Reaper.Batch < 1 {
		fail("%s must be at least 1", EnvReaperBatch)
	}
	// A non-positive gap floor makes farm.reaper_arm treat any positive
	// heartbeat delta as an outage, so every arm inserts a control_plane_gap
	// row and refunds lease time that was never lost. Left to the check below
	// it would be reported as a heartbeat problem, which sends the operator to
	// the wrong knob.
	if c.Reaper.GapFloor <= 0 {
		fail("%s must be positive; farm.reaper_arm compares now()-beat_at against "+
			"it, so a non-positive floor records ordinary scheduling jitter as a "+
			"control-plane outage", EnvReaperGapFloor)
	}
	if c.Reaper.HeartbeatInterval <= 0 {
		fail("%s must be positive", EnvHeartbeatEvery)
	} else if c.Reaper.GapFloor > 0 && c.Reaper.HeartbeatInterval*2 > c.Reaper.GapFloor {
		fail("%s (%s) must be less than half of %s (%s), or ordinary scheduling jitter "+
			"is recorded as a control-plane outage and refunds lease time that was "+
			"never lost", EnvHeartbeatEvery, c.Reaper.HeartbeatInterval,
			EnvReaperGapFloor, c.Reaper.GapFloor)
	}
	if len(c.Reaper.Components) == 0 {
		fail("%s must name at least one component; an empty list disables gap "+
			"accounting entirely", EnvReaperComponent)
	}
	// LEASE-13: a health-plane component in the watch list fuses the health
	// clock into the lease clock.
	//
	// farm.reaper_arm does not merely observe the names it is given. When the
	// oldest beat among them is older than the gap floor it ADDS that gap to
	// expires_at and reclaimable_at of every held and suspect lease
	// (migrations/00012_reaper_arm_unbeaten.sql). The arm runs once per gain of
	// reaper leadership, so a component that stays silent is refunded again on
	// every restart, deploy and failover for as long as it is gone. That refund
	// is right for a component whose silence stopped renewals from landing. For
	// a component on the health side of the firewall it is the forbidden fusion,
	// and it is reached by an edit that reads like MORE monitoring: adding
	// "watchdog" to the list an operator already trusts.
	//
	// Migration 00012 closed one half of this by accident. A health component
	// listed on a farm that does not run it now has no heartbeat row, so the
	// arm refuses loudly. The realistic misconfiguration is the other half — a
	// farm that DOES run the watchdog, the recovery ladder, the janitor or the
	// charge policy, all of which beat — where the arm succeeds and the refund
	// runs against every lease with nothing said.
	//
	// A deny list and not !onRenewalPath(name), because these are RESOLVED
	// names: "api-canary" and "scheduler-a" are exactly what the rename guard
	// below exists to keep working, and refusing every name that is not one of
	// the four canonical ones would refuse them too. Only the names this binary
	// itself beats under on the health side are refused.
	//
	// The base name before ":" is what is compared. The per-host components
	// beat under a suffixed key — "watchdog:h01", "node:h01" — and that key,
	// copied out of farm.component_heartbeat by an operator who went looking
	// for a name that exists, is the spelling that has a row and would
	// therefore take effect.
	//
	// A health component RENAMED by FARM_COMPONENT is not reachable from here:
	// this process cannot know what another process was renamed to. The guard
	// for that half lives in the loop below, on the process that owns the
	// rename and therefore does know.
	for _, name := range c.Reaper.Components {
		base, _, _ := strings.Cut(name, ":")
		if !contains(healthPlaneComponents(), base) {
			continue
		}
		fail("%s names %q, and %q is on the health side of the firewall rather than the "+
			"lease renewal path. farm.reaper_arm ADDS a watched component's downtime to "+
			"expires_at and reclaimable_at of EVERY held and suspect lease, on every gain "+
			"of reaper leadership for as long as it stays silent, so a %s outage longer "+
			"than %s would extend every live lease in the farm by the length of that "+
			"outage — device health deciding when a lease ends, which is the fusion this "+
			"control plane exists to prevent. Drop it and alert on its "+
			"farm.component_heartbeat row instead; %s is only for components whose downtime "+
			"stops renewals from landing (%s, or renames of those)",
			EnvReaperComponent, name, base, base, EnvReaperGapFloor, EnvReaperComponent,
			strings.Join(DefaultReaperComponents, ", "))
	}
	// BLOCKER 8: a component on the renewal path that is not watched is a
	// blind spot big enough to mass-reclaim the farm.
	//
	// The assertion walks every component this PROCESS runs, not just the one
	// it is named after. A role that multiplexes — `all`, `demo` — puts an API,
	// a scheduler, a reaper and a jobrunner on the renewal path under one
	// process name, and asking only about that name let all four through.
	//
	// The resolved key is what is checked, not the canonical one. Testing only
	// the canonical name would let FARM_COMPONENT=api-canary switch the guard
	// off for a process that is still very much on the renewal path:
	// farm.component_beat would write "api-canary", farm.reaper_arm would not
	// be watching it, and the canary's downtime would be invisible.
	for _, canon := range c.beats() {
		name := c.ComponentFor(canon)
		switch {
		case onRenewalPath(canon) && !contains(c.Reaper.Components, name):
			fail("component %q is on the lease renewal path but is missing from %s (%s). "+
				"If this component is down while the reaper is healthy, no gap is recorded "+
				"and every unprotected lease is reclaimed once TTL+grace elapses",
				name, EnvReaperComponent, strings.Join(c.Reaper.Components, ","))
		case !onRenewalPath(canon) && onRenewalPath(name):
			// The mirror-image blind spot. A watchdog renamed "api" writes the
			// API's heartbeat row, so farm.reaper_arm reads a fresh beat for a
			// component that may have been dead for an hour and refunds
			// nothing. The masquerade is worse than the silence it replaces.
			fail("%s renames the %q component of this process to %q, which is the heartbeat "+
				"key of a component ON the lease renewal path. Its beat would stand in for "+
				"that component's, so a real outage there would be refunded to no lease",
				EnvComponent, canon, name)
		case !onRenewalPath(canon) && name != canon && watchedAs(c.Reaper.Components, name):
			// LEASE-13, the half the deny list above cannot reach. That list
			// knows the canonical health-plane names; it cannot know that a
			// watchdog somewhere was renamed, so a farm that watches "wd:h01"
			// would sail past it while the arm found a real row and refunded a
			// real health outage to every lease.
			//
			// This process is the one that owns the rename, and every pod of a
			// Helm release reads FARM_REAPER_COMPONENTS from the same
			// ConfigMap, so the renamed component is normally handed the list
			// that names it and refuses here. It is a narrower net than the
			// deny list — a farm that sets the variable on the reaper alone
			// still boots — and it composes with the arm's own refusal: a
			// health component that crashloops on this message writes no
			// heartbeat, and a watched name with no row makes farm.reaper_arm
			// refuse to arm rather than reclaim.
			fail("%s renames the %q component of this process to %q, and %s watches that name "+
				"(%s). %q is on the health side of the firewall: farm.reaper_arm would ADD "+
				"this process's downtime to expires_at and reclaimable_at of EVERY held and "+
				"suspect lease, so an outage here would extend every live lease in the farm "+
				"by its own length — device health deciding when a lease ends. Remove %q "+
				"from %s; that list is only for components whose downtime stops renewals "+
				"from landing (%s, or renames of those)",
				EnvComponent, canon, name, EnvReaperComponent,
				strings.Join(c.Reaper.Components, ","), canon, name, EnvReaperComponent,
				strings.Join(DefaultReaperComponents, ", "))
		}
	}
	// FARM_COMPONENT on a role that runs several components at once was read,
	// validated, and then applied to nothing. Refusing is the only honest
	// answer: silently ignoring it is how an operator ends up believing a
	// canary is named apart when it is not.
	if c.multiplexed() && c.Component != c.role {
		fail("%s=%q cannot be honoured by the %q role: it runs %s in one process, each "+
			"writing its own farm.component_heartbeat row, so one name cannot stand for "+
			"all of them. Run the component as its own process to rename it, or unset %s",
			EnvComponent, c.Component, c.role,
			strings.Join(roleComponents[c.role], ", "), EnvComponent)
	}

	// ---- identity and addresses --------------------------------------
	if err := validComponentName(c.Component); err != nil {
		fail("%s: %v", EnvComponent, err)
	}
	if c.ShutdownGrace <= 0 {
		fail("%s must be positive", EnvShutdownGrace)
	}
	if c.DBMaxConns < 1 {
		fail("%s must be at least 1", EnvDBMaxConns)
	}
	if c.DBConnectTimeout <= 0 {
		fail("%s must be positive", EnvDBConnectTimeout)
	}
	if c.DatabaseURL != "" {
		if err := checkDSN(c.DatabaseURL); err != nil {
			fail("%s: %v", EnvDatabaseURL, err)
		}
	}
	// ---- the migration version table ---------------------------------
	// One name, written by goose in `farmd migrate` and read back by
	// /api/v1/capabilities. It is checked for every role rather than only for
	// the two that touch it, because a manifest is shared: a name the api
	// cannot read is a name the operator wants to hear about while they are
	// editing it, not at the first capability call — which, since a failed
	// probe is a 503, would tell them a fully migrated database has no schema.
	if _, _, err := MigrationsTableParts(c.MigrationsTable); err != nil {
		fail("%s (%q) %v", EnvMigrationsTable, c.MigrationsTable, err)
	}
	// ---- runtime database role -----------------------------------------
	// The role is assumed by SET ROLE on every connection of ONE pool, so it
	// can only be right for a process that is one component. Refusing the
	// other shapes here is what keeps the summary line honest: a process that
	// printed "database role = farm_reaper" and then ran a scheduler through
	// that pool would be blind to health, and one that printed it and ran as
	// the login user anyway would be a firewall that exists on paper.
	if c.DBRole != "" && c.opensPool() {
		switch owner := dbRoleOwner(c.DBRole); {
		case owner == "":
			fail("%s=%q is not a runtime role; the roles migrations/00002_lease.sql creates "+
				"are %s", EnvDBRole, c.DBRole, strings.Join(dbRoles(), ", "))
		case c.multiplexed():
			fail("%s=%q cannot be honoured by the %q role: it runs %s in one process on one "+
				"connection pool, and one pool cannot assume one role for all of them — as %s "+
				"the components that must read health could not, or the reaper could. Run the "+
				"%s as its own process to firewall it, or unset %s",
				EnvDBRole, c.DBRole, c.role, strings.Join(roleComponents[c.role], ", "),
				c.DBRole, owner, EnvDBRole)
		case owner != c.role:
			if want, ok := dbRoleForProcess[c.role]; ok {
				fail("%s=%q belongs to the %q process and this process is %q; set %s=%s or unset it",
					EnvDBRole, c.DBRole, owner, c.role, EnvDBRole, want)
			} else {
				fail("%s=%q belongs to the %q process and this process is %q, which has no "+
					"runtime role: the firewall covers %s. Unset %s",
					EnvDBRole, c.DBRole, owner, c.role, strings.Join(dbRoleProcesses(), ", "), EnvDBRole)
			}
		}
	}
	if c.WatchdogInterval <= 0 {
		fail("%s must be positive", EnvWatchdogInterval)
	}
	if c.ArtifactGCGrace < MinArtifactGCGrace {
		fail("%s (%s) is below %s. The artifact sweep treats a blob with no row as garbage "+
			"once it is this old, and artifacts.Store.Put commits the bytes before it writes "+
			"the row; a shorter grace deletes content whose row is being written right now",
			EnvArtifactGCGrace, c.ArtifactGCGrace, MinArtifactGCGrace)
	}
	// U9 — battery health. A rate of zero flags every phone that warms up
	// under a test; a ceiling outside what the column can hold can never
	// fire, which is the silent-disarm failure this package exists to refuse.
	if c.Battery.TempRiseDCPerMin < 1 {
		fail("%s must be at least 1 (decidegrees per minute)", EnvBatteryTempRise)
	}
	if c.Battery.TempMaxDC < 1 || c.Battery.TempMaxDC > MaxBatteryTempDC {
		fail("%s (%d) must be within 1..%d decidegrees: the column CHECK in "+
			"migrations/00010 cannot hold a reading above %d, so a ceiling there would "+
			"never fire", EnvBatteryTempMax, c.Battery.TempMaxDC, MaxBatteryTempDC, MaxBatteryTempDC)
	}
	if c.Battery.DrainPctPerHour < 1 || c.Battery.DrainPctPerHour > 100 {
		fail("%s (%d) must be within 1..100 percentage points per hour",
			EnvBatteryDrain, c.Battery.DrainPctPerHour)
	}

	// ---- U8 — charge policy ------------------------------------------
	// An inverted or degenerate band is refused rather than clamped: a loop
	// that parks at 40% and releases at 80% would hold every idle phone dark
	// the moment it was observed and never let one go.
	if c.Charge.MinPct <= 0 || c.Charge.MaxPct > 100 || c.Charge.MinPct >= c.Charge.MaxPct {
		fail("%s (%d) and %s (%d) must satisfy 0 < min < max <= 100; the policy parks an "+
			"idle device above max and releases it at or below min, and any other order "+
			"is a band nothing can be held inside",
			EnvChargeMinPct, c.Charge.MinPct, EnvChargeMaxPct, c.Charge.MaxPct)
	}
	if c.Charge.Interval <= 0 {
		fail("%s must be positive", EnvChargeInterval)
	} else if c.Charge.Interval*2 > MaxChargeGateHold {
		fail("%s (%s) must be at most half of the host agent's charge-gate cap (%s). Every "+
			"off-gate is asserted for two intervals so that one missed cycle — a rolling "+
			"deploy, a slow database — does not let the agent restore power to a phone the "+
			"policy still means to hold; above half the cap there is no room for that cycle",
			EnvChargeInterval, c.Charge.Interval, MaxChargeGateHold)
	}

	// The metrics address is a listener now, so a malformed one is a bind
	// failure at startup rather than a string nobody reads.
	if c.bindsMetrics() {
		if err := checkListenAddr(c.MetricsAddr); err != nil {
			fail("%s (%q): %v — or %q to serve no metrics endpoint at all",
				EnvMetricsAddr, c.MetricsAddr, err, MetricsOff)
		}
	}
	// ctl reaches the API through this, and a base URL without a scheme joins
	// into a request path instead of a host. Catch it at boot rather than as a
	// confusing 404 against the operator's own filesystem-rooted URL.
	if u, err := url.Parse(c.APIBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		fail("%s (%q) must be an absolute URL with a scheme and a host, e.g. %s",
			EnvAPIBaseURL, c.APIBaseURL, DefaultAPIBaseURL)
	} else if u.Scheme != "http" && u.Scheme != "https" {
		fail("%s (%q) must use http or https, got scheme %q",
			EnvAPIBaseURL, c.APIBaseURL, u.Scheme)
	}

	// ---- topology discovery ------------------------------------------
	//
	// Values are checked for every role: a fraction of 1.5 is a typo wherever
	// it is set, and the "report everything at once" rule wants it in the
	// same report as the rest. The overrides FILE is checked only where it is
	// read, because a shared manifest points every role at a path that exists
	// on the device host alone.
	if !strings.HasPrefix(c.Topo.SysfsRoot, "/") {
		fail("%s (%q) must be an absolute path on the Linux host, normally %s; "+
			"it is the USB bus device directory itself, the one holding entries "+
			"named like \"usb3\" and \"3-1.4\"", EnvSysfsRoot, c.Topo.SysfsRoot, DefaultSysfsRoot)
	}
	if c.Topo.OverridesPath != "" && c.scansUSB() {
		if st, err := os.Stat(c.Topo.OverridesPath); err != nil {
			fail("%s (%q): %v", EnvTopoOverrides, c.Topo.OverridesPath, err)
		} else if st.IsDir() {
			fail("%s (%q) is a directory, and must be a JSON file holding "+
				"{\"hub_tokens\": {...}, \"slot_labels\": {...}}", EnvTopoOverrides, c.Topo.OverridesPath)
		}
	}
	// Written as the negation of the accepted range so that NaN, which
	// strconv.ParseFloat accepts, fails rather than slipping past both bounds.
	if f := c.Topo.MaxRetireFraction; !(f > 0 && f <= 1) {
		fail("%s (%g) must be greater than 0 and at most 1; it is the share of a host's "+
			"active slots one discovery pass may mark 'maintenance' before refusing. "+
			"1 means \"retire everything the scan says is gone\", which is what a lost "+
			"/sys bind mount looks like", EnvTopoMaxRetireFraction, f)
	}
	if c.Topo.MinPorts < 1 || c.Topo.MinPorts > MaxHubPorts {
		fail("%s (%d) must be between 1 and %d: farm.hubs.port_count is CHECK-constrained "+
			"to that range, so a floor above it lets no hub become slots and says nothing",
			EnvTopoMinPorts, c.Topo.MinPorts, MaxHubPorts)
	}
	if c.Topo.Interval <= 0 {
		fail("%s must be positive", EnvTopoInterval)
	}
	if c.Topo.CallTimeout <= 0 {
		fail("%s must be positive", EnvTopoCallTimeout)
	} else if c.Topo.Interval > 0 && c.Topo.CallTimeout > c.Topo.Interval {
		fail("%s (%s) exceeds %s (%s). The node agent cancels a discovery pass that has "+
			"not finished by the time the next is due, so a single statement allowed to "+
			"outlast the whole pass is a timeout that can never fire",
			EnvTopoCallTimeout, c.Topo.CallTimeout, EnvTopoInterval, c.Topo.Interval)
	}
	for _, p := range c.Topo.Include {
		if !isUSBPath(p) {
			fail("%s entry %q is not a hub USB path; expected the kernel's bus-port chain "+
				"such as \"3-1.4\", or \"3-0\" for a root hub", EnvTopoInclude, p)
		}
	}
	for _, p := range c.Topo.Exclude {
		if !isUSBPath(p) {
			fail("%s entry %q is not a hub USB path; expected the kernel's bus-port chain "+
				"such as \"3-1.4\", or \"3-0\" for a root hub", EnvTopoExclude, p)
		}
		// Excludes win over includes in topo.HubFilter, so the include would
		// be read, kept and applied to nothing — the shape of mistake this
		// package refuses rather than resolves.
		if contains(c.Topo.Include, p) {
			fail("hub %q is in both %s and %s; the exclude would win and the include "+
				"would silently do nothing", p, EnvTopoInclude, EnvTopoExclude)
		}
	}

	// ---- fence proxy, client side ------------------------------------
	errs = append(errs, c.FenceClient.problems()...)
	errs = append(errs, c.FenceControl.problems()...)

	return errs
}

// scansUSB reports whether this role reads the USB tree, and so the files the
// topo knobs point at. Only the node agent does; every other role carries the
// values through a shared manifest and never opens them.
func (c *Config) scansUSB() bool { return c.role == "node" }

// isUSBPath accepts what topo.canonicalPath renders: "<bus>-<port>[.<port>…]",
// with "<bus>-0" for a root hub. A hub named any other way ("usb3", "3-1:1.0",
// a devpath with a leading slash) would never match a hub the scan reports,
// so an include or exclude written that way is a filter that filters nothing.
func isUSBPath(s string) bool {
	bus, ports, ok := strings.Cut(s, "-")
	if !ok || bus == "" || ports == "" {
		return false
	}
	if _, err := strconv.Atoi(bus); err != nil {
		return false
	}
	for _, p := range strings.Split(ports, ".") {
		if n, err := strconv.Atoi(p); err != nil || n < 0 || p == "" {
			return false
		}
	}
	return true
}

// RedactedDatabaseURL renders the DSN safely for logs. Nothing in this binary
// should ever print DatabaseURL directly.
func (c *Config) RedactedDatabaseURL() string {
	if c.DatabaseURL == "" {
		return "(unset)"
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u.Scheme == "" {
		// Includes the libpq keyword/value form, which may carry
		// "password=..." and has no structure we can selectively blank.
		return "(redacted)"
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	// url.Redacted only blanks the userinfo password. libpq URIs also accept
	// the password as a query parameter, and this string is printed in startup
	// summaries and in every connection error, so it must not survive there.
	if q := u.Query(); len(q) > 0 {
		var touched bool
		for _, key := range []string{"password", "sslpassword"} {
			if q.Has(key) {
				q.Set(key, "xxxxx")
				touched = true
			}
		}
		if touched {
			u.RawQuery = q.Encode()
		}
	}
	return u.Redacted()
}

// Summary is a one-line-per-field dump for startup logs.
//
// It is printed by every role that starts, because the alternative — an
// operator reading a manifest and inferring what the process decided — is how
// a farm ends up being debugged against values it is not running. Where a
// value has no destination in this build, the line says so rather than
// implying one.
func (c *Config) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "role             = %s\n", c.role)
	fmt.Fprintf(&b, "heartbeats as    = %s (every %s)\n",
		strings.Join(c.HeartbeatComponents(), ", "), c.Reaper.HeartbeatInterval)
	fmt.Fprintf(&b, "database         = %s (max %d conns, connect timeout %s)\n",
		c.RedactedDatabaseURL(), c.DBMaxConns, c.DBConnectTimeout)
	if c.DBRole != "" {
		fmt.Fprintf(&b, "database role    = %s (SET ROLE on every pooled connection; the login "+
			"user's own grants are not in effect)\n", c.DBRole)
	} else {
		fmt.Fprintf(&b, "database role    = login user (%s unset; the SQL role firewall is "+
			"NOT assumed by this process)\n", EnvDBRole)
	}
	fmt.Fprintf(&b, "api addr         = %s\n", c.APIAddr)
	if c.MetricsDisabled() {
		fmt.Fprintf(&b, "metrics addr     = %s (no /metrics listener)\n", MetricsOff)
	} else {
		fmt.Fprintf(&b, "metrics addr     = %s\n", c.MetricsAddr)
	}
	fmt.Fprintf(&b, "node addr        = %s\n", c.NodeAddr)
	fmt.Fprintf(&b, "lease ttl/grace  = %s / %s\n", c.Lease.TTL, c.Lease.Grace)
	fmt.Fprintf(&b, "renew interval   = %s (%d renewal attempts inside one ttl)\n",
		c.Lease.RenewInterval, c.renewAttempts())
	fmt.Fprintf(&b, "witness          = every %s, at most %d consecutive extensions; "+
		"one loop per placement, started by the jobrunner\n",
		c.Lease.WitnessInterval, c.Lease.MaxWitnessExtensions)
	fmt.Fprintf(&b, "marker           = rewritten on the device every %s; evidence presented "+
		"while younger than %s (derived: %d writes per witness tick, window of %d)\n",
		c.Lease.MarkerInterval(), c.Lease.MaxEvidenceAge(), MarkersPerWitnessTick, EvidenceWindow)
	fmt.Fprintf(&b, "slot rearm       = %s (must exceed node self-fence %s + margin %s)\n",
		c.Lease.SlotRearm, c.Node.SelfFenceTimeout, c.Node.FenceSafetyMargin)
	// The self-fence timeout and the margin are validated whether or not the
	// proxy is on. Printing them without saying which is the case would read
	// as a fence enforced at the device on a farm that has no proxy at all.
	if c.Fence.Enabled() {
		advertise := c.Fence.Advertise
		if advertise == "" {
			advertise = "(derived from the listener by the node agent)"
		}
		served := ""
		if c.role != "node" {
			served = " — served by the node role only"
		}
		fmt.Fprintf(&b, "fence proxy      = on: %s, advertised as %s, floors polled every %s, "+
			"new connections refused once the view is older than %s%s\n",
			c.Fence.Listen, advertise, c.Fence.PollInterval, c.Node.SelfFenceTimeout, served)
	} else {
		fmt.Fprintf(&b, "fence proxy      = off (%s/%s/%s unset) — the fence is NOT enforced at "+
			"the device; the self-fence timeout and margin above only bound the slot rearm\n",
			EnvFenceTLSCert, EnvFenceTLSKey, EnvFenceTLSCA)
	}
	// "watching" understates what the list does, and the understatement is how
	// a health component gets added to it. An outage of any name printed here
	// is refunded to every live lease, so the line says so where an operator
	// comparing two deployments will read it.
	fmt.Fprintf(&b, "reaper           = every %s, batch %d, gap floor %s, watching %s "+
		"(renewal path only: an outage of any of these extends every live lease by "+
		"its length)\n",
		c.Reaper.Interval, c.Reaper.Batch, c.Reaper.GapFloor,
		strings.Join(c.Reaper.Components, ","))
	// Set or unset is the whole of what may be said about the token. Its
	// absence is worth a line because the consequence is silent otherwise:
	// the ladder logs one warning at startup and then refuses tiers 3 and 4
	// for the life of the process.
	if c.Node.Token == "" {
		fmt.Fprintf(&b, "node token       = (unset: the node HTTP endpoint cannot start and "+
			"recovery tiers 3 and 4 stay refused)\n")
	} else {
		fmt.Fprintf(&b, "node token       = (set, never printed)\n")
	}
	overrides := "none"
	if c.Topo.OverridesPath != "" {
		overrides = c.Topo.OverridesPath
	}
	fmt.Fprintf(&b, "topo             = sysfs %s, every %s (call timeout %s), overrides %s\n",
		c.Topo.SysfsRoot, c.Topo.Interval, c.Topo.CallTimeout, overrides)
	fmt.Fprintf(&b, "topo hubs        = min %d ports, adopt empty %s, root hubs %s, include [%s], exclude [%s]\n",
		c.Topo.MinPorts, onOff(c.Topo.AdoptEmpty), onOff(c.Topo.IncludeRootHubs),
		strings.Join(c.Topo.Include, ","), strings.Join(c.Topo.Exclude, ","))
	// A retirement policy that is off reads as though vanished ports are
	// handled somehow. They are not: they stay active and never fill.
	if c.Topo.RetireVanished {
		fmt.Fprintf(&b, "topo removals    = retire vanished ports, at most %.0f%% of the host per pass",
			c.Topo.MaxRetireFraction*100)
	} else {
		fmt.Fprintf(&b, "topo removals    = off (a port the kernel stops reporting stays an active, "+
			"empty slot; bound %.0f%% if enabled)", c.Topo.MaxRetireFraction*100)
	}
	if c.Topo.DryRun {
		b.WriteString(" — DRY RUN, nothing is written")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "battery health   = temp rise > %.1f C/min, temp > %.1f C, idle drain > %d %%/h "+
		"(raises farm.events battery_anomaly; ends nothing)\n",
		float64(c.Battery.TempRiseDCPerMin)/10, float64(c.Battery.TempMaxDC)/10,
		c.Battery.DrainPctPerHour)
	fmt.Fprintf(&b, "charge policy    = park idle devices above %d%%, release at %d%%, every %s "+
		"(idle means no live lease; a lease is never touched)\n",
		c.Charge.MaxPct, c.Charge.MinPct, c.Charge.Interval)
	fmt.Fprintf(&b, "shutdown grace   = %s (drains requests; releases nothing)\n", c.ShutdownGrace)
	fmt.Fprintf(&b, "fence client     = %s\n", c.FenceClient.describe())
	fmt.Fprintf(&b, "artifact gc      = grace %s; a blob younger than this is never swept, "+
		"and nothing sweeps unless an operator asks\n", c.ArtifactGCGrace)
	return b.String()
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// renewAttempts is how many renewals fit inside one TTL, which is the number
// the MinRenewAttempts assertion is really about.
func (c *Config) renewAttempts() int {
	if c.Lease.RenewInterval <= 0 {
		return 0
	}
	return int(c.Lease.TTL / c.Lease.RenewInterval)
}

// ---------------------------------------------------------------------
// environment plumbing
// ---------------------------------------------------------------------

// loader reads env vars and accumulates parse failures so that one run
// reports every malformed value instead of the first.
type loader struct{ errs []error }

func (l *loader) raw(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a duration (want e.g. 90s, 15m, 2h): %w", key, v, err))
		return def
	}
	return d
}

func (l *loader) num(key string, def int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not an integer: %w", key, v, err))
		return def
	}
	return n
}

// num32 parses a value that is stored as an int32. Going through num and
// converting would wrap silently: FARM_DB_MAX_CONNS=4294967297 would become a
// pool of exactly one connection, which passes every check below.
func (l *loader) num32(key string, def int32) int32 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a 32-bit integer: %w", key, v, err))
		return def
	}
	return int32(n)
}

// boolean accepts what strconv.ParseBool does: 1, t, true, 0, f, false, in any
// case. "yes" is refused rather than read as false, because a switch that
// turns removal reconciliation on must not be silently left off by a spelling.
func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a boolean (want true or false): %w", key, v, err))
		return def
	}
	return b
}

func (l *loader) float(key string, def float64) float64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a number: %w", key, v, err))
		return def
	}
	return f
}

func (l *loader) list(key string, def []string) []string {
	v, ok := l.raw(key)
	if !ok {
		out := make([]string, len(def))
		copy(out, def)
		return out
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// onRenewalPath reports whether a component's downtime can stop leases from
// being renewed, and therefore must be covered by gap accounting.
//
// The jobrunner belongs here and was missing: it is the process that HOLDS the
// leases the reaper enforces deadlines against, so its outage is the one the
// refund exists for. Its own package says as much (jobrunner.DefaultComponent),
// and the guard was letting it be omitted from FARM_REAPER_COMPONENTS without
// a word.
//
// The list is deliberately short. The watchdog, the recovery ladder, the node
// agent and enrollment must NOT be on it: they are on the health side of the
// firewall, and letting their downtime move a lease deadline would fuse the
// hardware plane into lease liveness — the exact fusion this system exists to
// prevent.
func onRenewalPath(component string) bool {
	switch component {
	case "api", "scheduler", "reaper", "jobrunner":
		return true
	}
	return false
}

// healthPlaneComponents lists the farm.component_heartbeat keys this binary
// writes that are NOT on the renewal path: the ones whose downtime must never
// move a lease deadline. It is what FARM_REAPER_COMPONENTS refuses.
//
// Derived from roleComponents rather than written out, so a component added to
// a role is refused from the watch list on the day it is added rather than on
// the day somebody remembers this list. The names are per-role bases; the
// caller compares the part before ":", since the per-host components beat under
// a suffixed key.
func healthPlaneComponents() []string {
	off := make(map[string]struct{})
	for _, names := range roleComponents {
		for _, name := range names {
			if !onRenewalPath(name) {
				off[name] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(off))
}

// watchedAs reports whether the reaper's watch list names a component, under
// its own spelling or under the per-host key it really beats with. It is used
// only for the health-plane refusals, where a match must be found however the
// name was written; the renewal-path assertion deliberately keeps its exact
// comparison, because those components beat under an unsuffixed name and an
// entry like "api:x" would be a name farm.reaper_arm finds no row for.
func watchedAs(list []string, component string) bool {
	for _, entry := range list {
		if base, _, _ := strings.Cut(entry, ":"); base == component {
			return true
		}
	}
	return false
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// MigrationsTableParts splits a FARM_MIGRATIONS_TABLE value into the schema
// and table halves a reader has to quote separately, and refuses the values on
// which the migrator and its readers would not be naming the same table.
//
// It is exported because the name has to travel to whoever reads the table and
// must arrive already split and already checked. Validate calls it too, so
// there is one rule rather than a boot-time rule and a read-time one that can
// drift apart.
//
// A table name is an identifier and not a value, so it reaches SQL by quoting
// rather than by binding. That makes two properties load-bearing, and the
// second is the one that is easy to miss.
//
// The name must be one or two dot-separated parts, because quoting a two-part
// name as ONE part names a table nobody has:
// pgx.Identifier{"public.goose_db_version"}.Sanitize() renders
// "public.goose_db_version" — a single quoted identifier that happens to
// contain a dot.
//
// And every part must already be spelled the way Postgres folds it, because
// goose writes this name into its DDL unquoted while a reader quotes it. Those
// are the same table for a lowercase name and different tables for
// "Farm_Versions": goose would create farm_versions, the reader would ask for
// "Farm_Versions", and the operator would be told a fully migrated database
// has no schema — the exact failure this whole path exists to end.
//
// schema is empty for an unqualified name, leaving resolution to search_path,
// which is what goose does with the same value.
func MigrationsTableParts(name string) (schema, table string, err error) {
	schema, table, qualified := strings.Cut(name, ".")
	if !qualified {
		schema, table = "", name
	}
	if table == "" || (qualified && schema == "") || strings.Contains(table, ".") {
		return "", "", fmt.Errorf("must be a table name, optionally schema-qualified — %q, or a "+
			"bare %q resolved through search_path. goose creates this table and the "+
			"capability probe reads the applied version out of it, and a name that is "+
			"not one or two dot-separated parts quotes into an identifier neither can "+
			"resolve", DefaultMigrationsTable, "goose_db_version")
	}
	for _, part := range []string{schema, table} {
		if part == "" {
			continue // an unqualified name has no schema half
		}
		if e := foldedIdentifier(part); e != nil {
			// Wrong case is the common shape of this mistake and the fix is
			// mechanical, so name it. goose already folded the value when it
			// created the table, which makes the folded spelling the table
			// that exists — an operator should not have to derive that from a
			// character class while their fleet is refusing to boot.
			if lower := strings.ToLower(part); foldedIdentifier(lower) == nil {
				return "", "", fmt.Errorf("part %q %w. Set it to %q, the name goose folded "+
					"this to when it created the table", part, e, lower)
			}
			return "", "", fmt.Errorf("part %q %w", part, e)
		}
	}
	return schema, table, nil
}

// foldedIdentifier accepts only the identifiers Postgres leaves alone, so that
// quoting the name and interpolating it raw name the same table. Anything else
// is a value goose and its readers would spell differently.
func foldedIdentifier(s string) error {
	if len(s) > 63 {
		return fmt.Errorf("is %d characters; Postgres truncates an identifier at 63, so "+
			"goose would create a table under a name no reader can spell", len(s))
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return errors.New("must not start with a digit")
			}
		default:
			return errors.New("must match [a-z_][a-z0-9_]*: goose writes this name into its " +
				"DDL unquoted, so Postgres folds it, while a reader quotes it — anything " +
				"needing quotes would be created under one name and looked for under another")
		}
	}
	return nil
}

// validComponentName keeps the primary key of farm.component_heartbeat to
// something an alert can print and a human can type.
func validComponentName(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	if len(s) > 63 {
		return fmt.Errorf("must be at most 63 characters, got %d", len(s))
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9', r == '-', r == '_':
			if i == 0 {
				return fmt.Errorf("must start with a lowercase letter, got %q", s)
			}
		default:
			return fmt.Errorf("must match [a-z][a-z0-9_-]*, got %q", s)
		}
	}
	return nil
}

// checkListenAddr rejects anything net.Listen would reject for a reason we can
// name now rather than at bind time. A bare port ("9090") is the mistake this
// catches: it is not a listen address, and the process would otherwise die
// several seconds into startup with a message about a missing port.
func checkListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be a listen address such as \":9090\" or \"127.0.0.1:9090\": %w", err)
	}
	if port == "" {
		return errors.New("has no port")
	}
	if _, err := strconv.Atoi(port); err != nil {
		if _, lerr := net.LookupPort("tcp", port); lerr != nil {
			return fmt.Errorf("port %q is neither a number nor a known service name", port)
		}
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil && strings.ContainsAny(host, "/\\ ") {
			return fmt.Errorf("host %q is not an address", host)
		}
	}
	return nil
}

// checkAdvertiseAddr rejects an advertised address that names no host. It is
// dialled by other machines, so unlike a listen address it cannot leave the
// host part empty or unspecified: ":5038" written into farm.hosts is an
// endpoint nothing can reach, on a row that looks healthy.
func checkAdvertiseAddr(addr string) error {
	if err := checkListenAddr(addr); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(addr)
	if isWildcardHost(host) {
		return errors.New("names no host; an advertised address is what other machines " +
			"dial, so it needs the address they reach this one at")
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// sameListenAddr reports whether two listen addresses would contend for the
// same socket. A string comparison is not enough: ":9090", "0.0.0.0:9090" and
// "[::]:9090" are three spellings of the same bind, and treating them as
// different is how a role comes to refuse to start on a port conflict it
// created with itself.
func sameListenAddr(a, b string) bool {
	if a == b {
		return true
	}
	ah, ap, aerr := net.SplitHostPort(a)
	bh, bp, berr := net.SplitHostPort(b)
	if aerr != nil || berr != nil || ap != bp {
		return false
	}
	return ah == bh || isWildcardHost(ah) && isWildcardHost(bh)
}

func isWildcardHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// checkDSN accepts both libpq URL form and keyword/value form, which are both
// valid pgx inputs, and rejects anything else early rather than at first
// connect inside a Kubernetes CrashLoopBackOff.
func checkDSN(dsn string) error {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return fmt.Errorf("not a valid URL: %w", err)
		}
		if u.Host == "" {
			return errors.New("URL has no host")
		}
		return nil
	}
	if strings.Contains(dsn, "=") {
		return nil // keyword/value form, e.g. "host=db user=farm sslmode=require"
	}
	return errors.New(`must be a postgres:// URL or a libpq keyword/value string`)
}
