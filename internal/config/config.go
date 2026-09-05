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
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
)

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

	// MetricsOff is the value of FARM_METRICS_ADDR that means "serve no
	// metrics listener". An empty string cannot mean it: an empty environment
	// variable is indistinguishable from an unset one here (see loader.raw),
	// so opting out needs a word.
	MetricsOff = "off"
)

// DefaultReaperComponents lists every component whose downtime must be
// refunded to live leases. A component on the renewal path that is missing
// from this list is a blind spot: farm.reaper_arm takes min(beat_at) over the
// components it is given, a component with no row is simply absent from that
// minimum, and so its outage is invisible to gap accounting — the failure mode
// called out as BLOCKER 8 in migrations/00002_lease.sql.
//
// It deliberately carries one name more than farm.reaper_arm's own SQL default
// of ('reaper','api','scheduler'): the jobrunner holds the leases whose
// deadlines the reaper enforces, so a jobrunner outage is precisely the outage
// the refund exists for. The SQL default is only reachable by a hand-typed
// psql call; every farmd process passes this list explicitly.
//
// The list is what a farm RUNS, not what it could run. farm.reaper_arm refunds
// now()-min(beat_at) across the rows that exist, so a component that beat once
// and was then scaled to zero leaves a stale row that refunds every second
// since it left to every live lease, on every arm. Remove a component from
// this list when you remove it from the farm — and delete its heartbeat row.
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
	"api":       {"api"},
	"scheduler": {"scheduler"},
	"reaper":    {"reaper"},
	"recovery":  {"recovery"},
	"jobrunner": {"jobrunner"},
	"janitor":   {"janitor"},
	"watchdog":  {"watchdog"},
	"node":      {"node", "enroll"},
	"all":       {"api", "scheduler", "reaper", "recovery", "jobrunner", "janitor", "watchdog"},
	"demo":      {"api", "scheduler", "reaper", "recovery", "jobrunner", "janitor", "watchdog"},
	"ctl":       {},
	"migrate":   {},
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
	// WitnessInterval is the period of farm.lease_witness, the on-device
	// proof that the holder is alive.
	//
	// The loop that would use it exists — lease.Holder.StartWitness, fed by
	// runner.Marker — and NO ROLE STARTS ONE. Every StartWitness call site in
	// the tree is a test, so this value paces nothing in a running farm.
	//
	// It is still validated against Grace, because that check is what makes
	// wiring the loop up safe: a witness written less often than the lease can
	// go reclaimable proves nothing, and an operator who widened this without
	// widening Grace would be turning the witness off while believing it on.
	// Summary says the same out loud, so that setting this is not mistaken for
	// enabling it.
	WitnessInterval time.Duration
	// MaxWitnessExtensions caps consecutive witness-only extensions so a
	// wedged holder cannot hold a device forever on device-side evidence.
	MaxWitnessExtensions int
	// SlotRearm is passed as p_rearm to farm.lease_release and
	// farm.lease_reclaim. See Config.Validate.
	SlotRearm time.Duration
}

// Reaper holds the knobs of the only automatic release path in the system.
type Reaper struct {
	Interval          time.Duration
	Batch             int
	GapFloor          time.Duration
	Components        []string
	HeartbeatInterval time.Duration
}

// Node holds the host-side proxy's knobs.
type Node struct {
	// SelfFenceTimeout is how long the node proxy may go without a
	// successful fence check before it tears down every ADB socket it holds.
	SelfFenceTimeout time.Duration
	// FenceSafetyMargin is the slack required between SelfFenceTimeout and
	// Lease.SlotRearm. Zero reduces the assertion to "strictly greater".
	FenceSafetyMargin time.Duration
	ADBEndpoint       string
	HostID            string
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

	APIAddr string
	// MetricsAddr is where a role that is not the API binds its own /metrics
	// listener. The API serves /metrics off its own listener, so for that role
	// this is a second address carrying the same registry — which is what lets
	// one scrape config point at one port for every role in the farm.
	// MetricsOff disables it.
	MetricsAddr string
	NodeAddr    string
	APIBaseURL  string

	Lease   Lease
	Reaper  Reaper
	Node    Node
	Battery Battery

	WatchdogInterval time.Duration
	MigrationsTable  string
	MigrationsDir    string

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
		},

		Battery: Battery{
			TempRiseDCPerMin: l.num(EnvBatteryTempRise, DefaultBatteryTempRiseDCPerMin),
			TempMaxDC:        l.num(EnvBatteryTempMax, DefaultBatteryTempMaxDC),
			DrainPctPerHour:  l.num(EnvBatteryDrain, DefaultBatteryDrainPctPerHour),
		},
		WatchdogInterval: l.dur(EnvWatchdogInterval, DefaultWatchdogEvery),
		MigrationsTable:  l.str(EnvMigrationsTable, DefaultMigrationsTable),
		MigrationsDir:    l.str(EnvMigrationsDir, ""),
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
	// the only thing standing between a reclaimed slot and the previous
	// holder's still-open ADB sockets.
	// -----------------------------------------------------------------
	minRearm := c.Node.SelfFenceTimeout + c.Node.FenceSafetyMargin
	if c.Lease.SlotRearm <= c.Node.SelfFenceTimeout || c.Lease.SlotRearm < minRearm {
		fail(`%s (%s) must exceed %s (%s) by at least %s (%s), and is not.

When a lease is reclaimed the device gets a new fence floor and the slot is
parked for the rearm interval. The node proxy only discovers that its fence is
stale after its self-fence timeout elapses; until then it is still holding open
ADB sockets and still forwarding the old job's commands to the phone.

If the slot becomes schedulable again before that teardown finishes, the
scheduler hands the same physical device to a new job while the previous job is
still driving it. Two tenants then share one phone: shell commands interleave,
installs land in the wrong session, and neither side sees an error. Nothing
downstream can detect this, which is why it is refused here.

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
	if c.WatchdogInterval <= 0 {
		fail("%s must be positive", EnvWatchdogInterval)
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

	return errs
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
	fmt.Fprintf(&b, "witness          = every %s, at most %d extensions — NOT STARTED: "+
		"lease.Holder.StartWitness exists and no role calls it\n",
		c.Lease.WitnessInterval, c.Lease.MaxWitnessExtensions)
	fmt.Fprintf(&b, "slot rearm       = %s (node self-fence %s + margin %s)\n",
		c.Lease.SlotRearm, c.Node.SelfFenceTimeout, c.Node.FenceSafetyMargin)
	fmt.Fprintf(&b, "reaper           = every %s, batch %d, gap floor %s, watching %s\n",
		c.Reaper.Interval, c.Reaper.Batch, c.Reaper.GapFloor,
		strings.Join(c.Reaper.Components, ","))
	fmt.Fprintf(&b, "battery health   = temp rise > %.1f C/min, temp > %.1f C, idle drain > %d %%/h "+
		"(raises farm.events battery_anomaly; ends nothing)\n",
		float64(c.Battery.TempRiseDCPerMin)/10, float64(c.Battery.TempMaxDC)/10,
		c.Battery.DrainPctPerHour)
	fmt.Fprintf(&b, "shutdown grace   = %s (drains requests; releases nothing)\n", c.ShutdownGrace)
	return b.String()
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

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
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
