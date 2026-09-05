package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/node"
	"github.com/flaviopadilha/device-farmer/internal/topo"
)

// A note on why every test here sets the whole environment.
//
// This package reads process-wide state, so a test that sets only the variable
// it cares about is really asserting about the machine it runs on: a developer
// with FARM_LEASE_TTL exported in a shell would see failures nobody else can
// reproduce, and — much worse — a CI runner with FARM_REAPER_COMPONENTS set
// would see the BLOCKER 8 assertions below pass for the wrong reason. Every
// test therefore starts from a known-empty environment.
//
// An empty value is how "unset" is spelled: loader.raw treats an empty or
// whitespace-only variable as absent, so t.Setenv(key, "") is the only way to
// unset one and still have the test framework restore it afterwards.
var allEnv = []string{
	EnvDatabaseURL, EnvDBMaxConns, EnvDBConnectTimeout, EnvDBRole,
	EnvComponent, EnvLogLevel, EnvShutdownGrace,
	EnvAPIAddr, EnvMetricsAddr, EnvNodeAddr, EnvAPIBaseURL,
	EnvLeaseTTL, EnvLeaseGrace, EnvLeaseRenewInterval,
	EnvLeaseWitnessInterval, EnvLeaseWitnessMaxExt, EnvSlotRearm,
	EnvReaperInterval, EnvReaperBatch, EnvReaperGapFloor, EnvReaperComponent,
	EnvHeartbeatEvery,
	EnvNodeSelfFence, EnvFenceMargin, EnvNodeADBEndpoint, EnvNodeHostID,
	EnvWatchdogInterval, EnvMigrationsTable, EnvMigrationsDir,
	EnvArtifactGCGrace,
	EnvNodeToken, EnvSysfsRoot, EnvTopoOverrides,
	EnvTopoRetireVanished, EnvTopoMaxRetireFraction, EnvTopoDryRun,
	EnvTopoMinPorts, EnvTopoAdoptEmpty, EnvTopoIncludeRootHubs,
	EnvTopoInclude, EnvTopoExclude, EnvTopoInterval, EnvTopoCallTimeout,
	EnvBatteryTempRise, EnvBatteryTempMax, EnvBatteryDrain,
	EnvChargeMinPct, EnvChargeMaxPct, EnvChargeInterval,
	EnvFenceTLSCert, EnvFenceTLSKey, EnvFenceTLSCA, EnvFenceListen, EnvFenceAdvertise,
	EnvFencePollInterval,
	EnvFenceClientCert, EnvFenceClientKey, EnvFenceClientCA,
}

const testDSN = "postgres://farm@127.0.0.1:5432/farm?sslmode=disable"

// env clears every variable this package reads and then applies the given
// ones. It never runs in parallel: t.Setenv forbids it, for exactly the reason
// above.
func env(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range allEnv {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		if !slices.Contains(allEnv, k) {
			t.Fatalf("test sets %s, which is not in allEnv and so is not cleared between tests", k)
		}
		t.Setenv(k, v)
	}
}

// withDSN adds a valid DATABASE_URL to a set of overrides, since almost every
// role requires one and no test here is about that fact.
func withDSN(kv map[string]string) map[string]string {
	out := map[string]string{EnvDatabaseURL: testDSN}
	for k, v := range kv {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// TestDefaultsLoadForEveryRole is the assertion behind the claim in the
// package doc that "an operator who sets only DATABASE_URL gets a
// configuration that satisfies every assertion below".
//
// It runs over every role because the assertions are role-dependent: the
// BLOCKER 8 check asks what components the process runs, and a default that
// satisfies it for `api` and refuses `all` would be a default that cannot
// start a laptop farm.
func TestDefaultsLoadForEveryRole(t *testing.T) {
	for role := range roleComponents {
		t.Run(role, func(t *testing.T) {
			env(t, withDSN(nil))
			cfg, err := Load(role)
			if err != nil {
				t.Fatalf("defaults refused for role %q: %v", role, err)
			}
			if cfg.Component != role {
				t.Errorf("Component = %q, want the role name %q", cfg.Component, role)
			}
			if cfg.Role() != role {
				t.Errorf("Role() = %q, want %q", cfg.Role(), role)
			}
		})
	}
}

// TestDefaultValues pins the resolved defaults. They are load bearing: the
// cross-field assertions are stated in terms of them, and a silent change to
// one is a silent change to what a farm does with no manifest at all.
func TestDefaultValues(t *testing.T) {
	env(t, withDSN(nil))
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{EnvLogLevel, cfg.LogLevel, DefaultLogLevel},
		{EnvShutdownGrace, cfg.ShutdownGrace, DefaultShutdownGrace},
		{EnvAPIAddr, cfg.APIAddr, DefaultAPIAddr},
		{EnvMetricsAddr, cfg.MetricsAddr, DefaultMetricsAddr},
		{EnvNodeAddr, cfg.NodeAddr, DefaultNodeAddr},
		{EnvAPIBaseURL, cfg.APIBaseURL, DefaultAPIBaseURL},
		{EnvLeaseTTL, cfg.Lease.TTL, DefaultLeaseTTL},
		{EnvLeaseGrace, cfg.Lease.Grace, DefaultLeaseGrace},
		{EnvLeaseRenewInterval, cfg.Lease.RenewInterval, DefaultLeaseRenewInterval},
		{EnvLeaseWitnessInterval, cfg.Lease.WitnessInterval, DefaultLeaseWitnessInterval},
		{EnvLeaseWitnessMaxExt, cfg.Lease.MaxWitnessExtensions, DefaultLeaseWitnessMaxExt},
		{EnvSlotRearm, cfg.Lease.SlotRearm, DefaultSlotRearm},
		{EnvReaperInterval, cfg.Reaper.Interval, DefaultReaperInterval},
		{EnvReaperBatch, cfg.Reaper.Batch, DefaultReaperBatch},
		{EnvReaperGapFloor, cfg.Reaper.GapFloor, DefaultReaperGapFloor},
		{EnvHeartbeatEvery, cfg.Reaper.HeartbeatInterval, DefaultHeartbeatEvery},
		{EnvNodeSelfFence, cfg.Node.SelfFenceTimeout, DefaultNodeSelfFence},
		{EnvFenceMargin, cfg.Node.FenceSafetyMargin, DefaultFenceMargin},
		{EnvNodeADBEndpoint, cfg.Node.ADBEndpoint, DefaultADBEndpoint},
		{EnvDBMaxConns, cfg.DBMaxConns, int32(DefaultDBMaxConns)},
		{EnvDBConnectTimeout, cfg.DBConnectTimeout, DefaultDBConnectTimeout},
		{EnvWatchdogInterval, cfg.WatchdogInterval, DefaultWatchdogEvery},
		{EnvMigrationsTable, cfg.MigrationsTable, DefaultMigrationsTable},
		{EnvArtifactGCGrace, cfg.ArtifactGCGrace, DefaultArtifactGCGrace},
		{EnvNodeToken, cfg.Node.Token, ""},
		{EnvSysfsRoot, cfg.Topo.SysfsRoot, DefaultSysfsRoot},
		{EnvTopoOverrides, cfg.Topo.OverridesPath, ""},
		// Off, and it must stay off: see Topo.RetireVanished.
		{EnvTopoRetireVanished, cfg.Topo.RetireVanished, false},
		{EnvTopoMaxRetireFraction, cfg.Topo.MaxRetireFraction, DefaultTopoMaxRetireFraction},
		{EnvTopoDryRun, cfg.Topo.DryRun, false},
		{EnvTopoMinPorts, cfg.Topo.MinPorts, DefaultTopoMinPorts},
		{EnvTopoAdoptEmpty, cfg.Topo.AdoptEmpty, false},
		{EnvTopoIncludeRootHubs, cfg.Topo.IncludeRootHubs, false},
		{EnvTopoInterval, cfg.Topo.Interval, DefaultTopoInterval},
		{EnvTopoCallTimeout, cfg.Topo.CallTimeout, DefaultTopoCallTimeout},
		{EnvBatteryTempRise, cfg.Battery.TempRiseDCPerMin, DefaultBatteryTempRiseDCPerMin},
		{EnvBatteryTempMax, cfg.Battery.TempMaxDC, DefaultBatteryTempMaxDC},
		{EnvBatteryDrain, cfg.Battery.DrainPctPerHour, DefaultBatteryDrainPctPerHour},
		{EnvFenceListen, cfg.Fence.Listen, DefaultFenceListen},
		{EnvFencePollInterval, cfg.Fence.PollInterval, DefaultFencePollInterval},
		{"fence proxy off by default", cfg.Fence.Enabled(), false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.Topo.Include) != 0 || len(cfg.Topo.Exclude) != 0 {
		t.Errorf("default hub filter lists are not empty: include %v, exclude %v",
			cfg.Topo.Include, cfg.Topo.Exclude)
	}
	if !slices.Equal(cfg.Reaper.Components, DefaultReaperComponents) {
		t.Errorf("%s: got %v, want %v", EnvReaperComponent, cfg.Reaper.Components, DefaultReaperComponents)
	}
	// The list must be a copy: a caller that appends to it would otherwise
	// mutate the package default for every later process in the same binary.
	cfg.Reaper.Components[0] = "clobbered"
	if DefaultReaperComponents[0] == "clobbered" {
		t.Fatal("Reaper.Components aliases DefaultReaperComponents")
	}
}

// TestTopoDefaultsMatchTheirPackages holds this package's restated defaults
// to the constants the consuming packages apply for a zero value. If the two
// drift, Summary prints a number the node is not running with — which is the
// exact failure the summary exists to prevent.
//
// The sysfs root is the one that has already been wrong once: the node role
// used to open "/sys", a directory with no USB device in it, and would have
// reported every host as empty.
func TestTopoDefaultsMatchTheirPackages(t *testing.T) {
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"sysfs root", DefaultSysfsRoot, topo.DefaultSysfsRoot},
		{"max retire fraction", DefaultTopoMaxRetireFraction, topo.DefaultMaxRetireFraction},
		{"min hub ports", DefaultTopoMinPorts, topo.DefaultMinHubPorts},
		{"interval (topo)", DefaultTopoInterval, topo.DefaultInterval},
		{"interval (node)", DefaultTopoInterval, node.DefaultDiscoverInterval},
		{"call timeout", DefaultTopoCallTimeout, topo.DefaultCallTimeout},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: config says %v, the package applies %v", c.name, c.got, c.want)
		}
	}
}

// TestDefaultReaperComponentsCoversTheRenewalPath is the guard that keeps the
// default from being the thing that fails preflight.
//
// Every component this package considers to be on the renewal path must be
// watched by default, or the default configuration for the role that runs it
// is refused — and an operator's first response to a refusal they did not
// cause is to widen the list until it stops complaining, which is how the
// blind spot gets configured back in by hand.
func TestDefaultReaperComponentsCoversTheRenewalPath(t *testing.T) {
	seen := map[string]bool{}
	for _, names := range roleComponents {
		for _, name := range names {
			if onRenewalPath(name) {
				seen[name] = true
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no role runs a renewal-path component; the assertion below proves nothing")
	}
	for name := range seen {
		if !slices.Contains(DefaultReaperComponents, name) {
			t.Errorf("%q is on the renewal path but is not in DefaultReaperComponents (%v); "+
				"farm.reaper_arm would not watch it and its outage would refund nothing",
				name, DefaultReaperComponents)
		}
	}
	// And nothing on the health side of the firewall may be there: a watchdog
	// or recovery outage that moved lease deadlines would fuse device health
	// into lease liveness.
	for _, name := range DefaultReaperComponents {
		if !onRenewalPath(name) {
			t.Errorf("%q is watched by default but is not on the renewal path; its downtime "+
				"would extend every live lease's deadline", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Preflight refusals
// ---------------------------------------------------------------------------

// TestPreflightRefusals drives one bad manifest per case and requires the
// refusal to name the variable that caused it. The message matters as much as
// the refusal: an operator reading a CrashLoopBackOff has the error text and
// nothing else.
func TestPreflightRefusals(t *testing.T) {
	cases := []struct {
		name string
		role string
		envs map[string]string
		want []string // substrings that must all appear
	}{{
		// The assertion this whole package exists for: a slot that becomes
		// schedulable before the previous holder's sockets are certainly gone
		// hands one phone to two jobs.
		name: "slot rearm below the node self-fence timeout",
		role: "scheduler",
		envs: map[string]string{EnvSlotRearm: "10s", EnvNodeSelfFence: "20s"},
		want: []string{EnvSlotRearm, EnvNodeSelfFence, "Two tenants then share one phone"},
	}, {
		name: "slot rearm inside the safety margin",
		role: "scheduler",
		envs: map[string]string{EnvSlotRearm: "21s", EnvNodeSelfFence: "20s", EnvFenceMargin: "5s"},
		want: []string{EnvSlotRearm, EnvFenceMargin},
	}, {
		name: "lease ttl below the schema floor",
		role: "api",
		envs: map[string]string{EnvLeaseTTL: "5m", EnvLeaseRenewInterval: "30s"},
		want: []string{EnvLeaseTTL, "23514"},
	}, {
		name: "lease grace below the schema floor",
		role: "api",
		envs: map[string]string{EnvLeaseGrace: "1m", EnvLeaseWitnessInterval: "30s"},
		want: []string{EnvLeaseGrace, "23514"},
	}, {
		// Two renewals must be losable — a rolling deploy, a failover, a GC
		// pause — without the lease going suspect.
		name: "renew interval leaves fewer than three attempts",
		role: "jobrunner",
		envs: map[string]string{EnvLeaseRenewInterval: "6m"},
		want: []string{EnvLeaseRenewInterval, EnvLeaseTTL, "two consecutive renewals"},
	}, {
		name: "renew interval not positive",
		role: "jobrunner",
		envs: map[string]string{EnvLeaseRenewInterval: "0s"},
		want: []string{EnvLeaseRenewInterval, "must be positive"},
	}, {
		name: "witness interval too coarse for the grace band",
		role: "api",
		envs: map[string]string{EnvLeaseWitnessInterval: "20m"},
		want: []string{EnvLeaseWitnessInterval, EnvLeaseGrace},
	}, {
		// The marker cadence follows the witness cadence at a quarter, so a
		// witness interval with no floor is a marker written every few hundred
		// milliseconds on every leased device on a host.
		name: "witness interval below the floor",
		role: "jobrunner",
		envs: map[string]string{EnvLeaseWitnessInterval: "1s"},
		want: []string{EnvLeaseWitnessInterval, MinLeaseWitnessInterval.String(), "shell round trip", "250ms"},
	}, {
		name: "witness extensions below one",
		role: "api",
		envs: map[string]string{EnvLeaseWitnessMaxExt: "0"},
		want: []string{EnvLeaseWitnessMaxExt},
	}, {
		name: "gap floor not positive",
		role: "reaper",
		envs: map[string]string{EnvReaperGapFloor: "0s"},
		want: []string{EnvReaperGapFloor, "control-plane outage"},
	}, {
		name: "heartbeat too slow for the gap floor",
		role: "reaper",
		envs: map[string]string{EnvHeartbeatEvery: "45s", EnvReaperGapFloor: "60s"},
		want: []string{EnvHeartbeatEvery, EnvReaperGapFloor},
	}, {
		// U9 — battery health. A ceiling the column cannot hold is a rule
		// that never fires, which is worse than no rule: somebody believes
		// they are covered.
		name: "battery ceiling above the column CHECK",
		role: "watchdog",
		envs: map[string]string{EnvBatteryTempMax: "1501"},
		want: []string{EnvBatteryTempMax, "never fire"},
	}, {
		name: "battery rise rate of zero",
		role: "watchdog",
		envs: map[string]string{EnvBatteryTempRise: "0"},
		want: []string{EnvBatteryTempRise, "at least 1"},
	}, {
		name: "battery drain above one hundred points an hour",
		role: "watchdog",
		envs: map[string]string{EnvBatteryDrain: "101"},
		want: []string{EnvBatteryDrain, "1..100"},
	}, {
		name: "empty component watch list",
		role: "reaper",
		envs: map[string]string{EnvReaperComponent: " , ,"},
		want: []string{EnvReaperComponent, "disables gap accounting"},
	}, {
		// The sweep's grace is the fence around Put's commit-then-insert
		// window; a grace of seconds is a race against every upload.
		name: "artifact gc grace below the floor the sweep refuses",
		role: "api",
		envs: map[string]string{EnvArtifactGCGrace: "30s"},
		want: []string{EnvArtifactGCGrace, "row is being written"},
	}, {
		// BLOCKER 8, the original shape: rename the process and the reaper is
		// watching a name nothing writes.
		name: "renamed api is not watched",
		role: "api",
		envs: map[string]string{EnvComponent: "api-canary"},
		want: []string{"api-canary", EnvReaperComponent, "renewal path"},
	}, {
		// BLOCKER 8, the shape the old check missed: the jobrunner holds the
		// leases whose deadlines the reaper enforces, and could be left out of
		// the watch list without a word.
		name: "jobrunner omitted from the watch list",
		role: "jobrunner",
		envs: map[string]string{EnvReaperComponent: "reaper,api,scheduler"},
		want: []string{"jobrunner", EnvReaperComponent, "renewal path"},
	}, {
		// The other shape the old check missed: `all` and `demo` put four
		// components on the renewal path under a process name that is on it
		// under neither name.
		name: "all runs an unwatched renewal-path component",
		role: "all",
		envs: map[string]string{EnvReaperComponent: "reaper,scheduler,jobrunner"},
		want: []string{`"api"`, EnvReaperComponent, "renewal path"},
	}, {
		name: "demo runs an unwatched renewal-path component",
		role: "demo",
		envs: map[string]string{EnvReaperComponent: "reaper,api,scheduler"},
		want: []string{`"jobrunner"`, EnvReaperComponent},
	}, {
		// A watchdog beating under the API's key writes a fresh heartbeat for
		// a component that may be an hour dead, so the reaper refunds nothing.
		name: "an off-path component named after an on-path one",
		role: "watchdog",
		envs: map[string]string{EnvComponent: "api"},
		want: []string{EnvComponent, "stand in for"},
	}, {
		// U8 — an inverted band would park every idle phone on sight and
		// release none of them.
		name: "charge band inverted",
		role: "chargepolicy",
		envs: map[string]string{EnvChargeMinPct: "80", EnvChargeMaxPct: "40"},
		want: []string{EnvChargeMinPct, EnvChargeMaxPct, "0 < min < max <= 100"},
	}, {
		name: "charge max above one hundred",
		role: "chargepolicy",
		envs: map[string]string{EnvChargeMaxPct: "101"},
		want: []string{EnvChargeMaxPct, "0 < min < max <= 100"},
	}, {
		// A hold is two intervals, and the agent restores power at its cap:
		// an interval above half the cap leaves no missed cycle to lose.
		name: "charge interval above half the agent's hold cap",
		role: "chargepolicy",
		envs: map[string]string{EnvChargeInterval: "16m"},
		want: []string{EnvChargeInterval, "half", "restore power"},
	}, {
		name: "charge interval not positive",
		role: "chargepolicy",
		envs: map[string]string{EnvChargeInterval: "0s"},
		want: []string{EnvChargeInterval, "must be positive"},
	}, {
		// The knob that was read, validated and applied to nothing.
		name: "FARM_COMPONENT on a multiplexed role",
		role: "demo",
		envs: map[string]string{EnvComponent: "scheduler-a"},
		want: []string{EnvComponent, "cannot be honoured", "own process"},
	}, {
		// One pool serving six components cannot assume one component's
		// role: as farm_reaper the scheduler inside would be blind to health.
		name: "FARM_DB_ROLE on a multiplexed role",
		role: "demo",
		envs: map[string]string{EnvDBRole: DBRoleReaper},
		want: []string{EnvDBRole, "cannot be honoured", "one connection pool", "own process"},
	}, {
		name: "FARM_DB_ROLE on all",
		role: "all",
		envs: map[string]string{EnvDBRole: DBRoleScheduler},
		want: []string{EnvDBRole, "one connection pool"},
	}, {
		// The firewall is directional. A reaper running as the watchdog's
		// role would have no path to a lease, and the process exists to end
		// them.
		name: "FARM_DB_ROLE that belongs to another process",
		role: "reaper",
		envs: map[string]string{EnvDBRole: DBRoleWatchdog},
		want: []string{EnvDBRole, `"watchdog"`, `"reaper"`, EnvDBRole + "=" + DBRoleReaper},
	}, {
		name: "FARM_DB_ROLE on a process the firewall does not cover",
		role: "api",
		envs: map[string]string{EnvDBRole: DBRoleScheduler},
		want: []string{EnvDBRole, `"api"`, "no runtime role"},
	}, {
		name: "FARM_DB_ROLE that is not a runtime role",
		role: "reaper",
		envs: map[string]string{EnvDBRole: "postgres"},
		want: []string{EnvDBRole, "not a runtime role", DBRoleReaper},
	}, {
		name: "metrics address without a port",
		role: "scheduler",
		envs: map[string]string{EnvMetricsAddr: "9090"},
		want: []string{EnvMetricsAddr, MetricsOff},
	}, {
		name: "metrics address with an empty port",
		role: "scheduler",
		envs: map[string]string{EnvMetricsAddr: "127.0.0.1:"},
		want: []string{EnvMetricsAddr, "port"},
	}, {
		name: "component name the heartbeat key cannot hold",
		role: "api",
		envs: map[string]string{EnvComponent: "API_Canary"},
		want: []string{EnvComponent, "[a-z][a-z0-9_-]*"},
	}, {
		name: "missing database url",
		role: "scheduler",
		envs: map[string]string{EnvDatabaseURL: ""},
		want: []string{EnvDatabaseURL, "required"},
	}, {
		name: "database url that is neither form",
		role: "scheduler",
		envs: map[string]string{EnvDatabaseURL: "/var/run/postgres"},
		want: []string{EnvDatabaseURL, "keyword/value"},
	}, {
		name: "api base url without a scheme",
		role: "ctl",
		envs: map[string]string{EnvAPIBaseURL: "127.0.0.1:8080"},
		want: []string{EnvAPIBaseURL, "absolute URL"},
	}, {
		name: "api base url with the wrong scheme",
		role: "ctl",
		envs: map[string]string{EnvAPIBaseURL: "ftp://farm/api"},
		want: []string{EnvAPIBaseURL, "http or https"},
	}, {
		name: "duration that is not a duration",
		role: "api",
		envs: map[string]string{EnvLeaseTTL: "15 minutes"},
		want: []string{EnvLeaseTTL, "is not a duration"},
	}, {
		name: "connection count that is not a number",
		role: "api",
		envs: map[string]string{EnvDBMaxConns: "lots"},
		want: []string{EnvDBMaxConns, "not a 32-bit integer"},
	}, {
		// Parsing this through int and narrowing would silently yield a pool
		// of exactly one connection, which passes every check below it.
		name: "connection count that overflows int32",
		role: "api",
		envs: map[string]string{EnvDBMaxConns: "4294967297"},
		want: []string{EnvDBMaxConns, "not a 32-bit integer"},
	}, {
		name: "shutdown grace not positive",
		role: "api",
		envs: map[string]string{EnvShutdownGrace: "0s"},
		want: []string{EnvShutdownGrace},
	}, {
		name: "watchdog interval not positive",
		role: "watchdog",
		envs: map[string]string{EnvWatchdogInterval: "-1s"},
		want: []string{EnvWatchdogInterval},
	}, {
		// U12. Above 1 is "retire everything the scan says is gone", which is
		// what a lost /sys bind mount produces.
		name: "retire fraction above one",
		role: "node",
		envs: map[string]string{EnvTopoMaxRetireFraction: "1.5"},
		want: []string{EnvTopoMaxRetireFraction, "at most 1"},
	}, {
		name: "retire fraction of zero",
		role: "node",
		envs: map[string]string{EnvTopoMaxRetireFraction: "0"},
		want: []string{EnvTopoMaxRetireFraction, "greater than 0"},
	}, {
		// strconv.ParseFloat accepts NaN, and NaN passes both a <= 0 and a
		// > 1 test.
		name: "retire fraction that is not a number at all",
		role: "node",
		envs: map[string]string{EnvTopoMaxRetireFraction: "NaN"},
		want: []string{EnvTopoMaxRetireFraction},
	}, {
		name: "retire fraction that does not parse",
		role: "node",
		envs: map[string]string{EnvTopoMaxRetireFraction: "a quarter"},
		want: []string{EnvTopoMaxRetireFraction, "is not a number"},
	}, {
		name: "hub port floor of zero",
		role: "node",
		envs: map[string]string{EnvTopoMinPorts: "0"},
		want: []string{EnvTopoMinPorts, "between 1 and 32"},
	}, {
		name: "hub port floor above the schema ceiling",
		role: "node",
		envs: map[string]string{EnvTopoMinPorts: "33"},
		want: []string{EnvTopoMinPorts, "no hub"},
	}, {
		name: "discovery interval not positive",
		role: "node",
		envs: map[string]string{EnvTopoInterval: "0s", EnvTopoCallTimeout: "1s"},
		want: []string{EnvTopoInterval, "must be positive"},
	}, {
		name: "discovery call timeout not positive",
		role: "node",
		envs: map[string]string{EnvTopoCallTimeout: "0s"},
		want: []string{EnvTopoCallTimeout, "must be positive"},
	}, {
		name: "discovery call timeout outlasts the pass",
		role: "node",
		envs: map[string]string{EnvTopoInterval: "1m", EnvTopoCallTimeout: "2m"},
		want: []string{EnvTopoCallTimeout, EnvTopoInterval, "never fire"},
	}, {
		name: "relative sysfs root",
		role: "node",
		envs: map[string]string{EnvSysfsRoot: "sys/bus/usb/devices"},
		want: []string{EnvSysfsRoot, "absolute path"},
	}, {
		// "usb3" is the directory name; the scan reports the hub as "3-0", so
		// an include spelled this way matches nothing and says nothing.
		name: "include that is not a usb path",
		role: "node",
		envs: map[string]string{EnvTopoInclude: "3-1.4,usb3"},
		want: []string{EnvTopoInclude, `"usb3"`, "3-1.4"},
	}, {
		name: "exclude that is not a usb path",
		role: "node",
		envs: map[string]string{EnvTopoExclude: "/sys/bus/usb/devices/3-1"},
		want: []string{EnvTopoExclude, "not a hub USB path"},
	}, {
		name: "hub both included and excluded",
		role: "node",
		envs: map[string]string{EnvTopoInclude: "3-1.4", EnvTopoExclude: "3-1.4"},
		want: []string{EnvTopoInclude, EnvTopoExclude, `"3-1.4"`, "silently do nothing"},
	}, {
		// "yes" must not be read as false: a switch that turns removal
		// reconciliation on cannot be left off by a spelling.
		name: "boolean that is not a boolean",
		role: "node",
		envs: map[string]string{EnvTopoRetireVanished: "yes"},
		want: []string{EnvTopoRetireVanished, "not a boolean"},
	}, {
		name: "overrides file that does not exist",
		role: "node",
		envs: map[string]string{EnvTopoOverrides: "/nonexistent/overrides.json"},
		want: []string{EnvTopoOverrides, "overrides.json"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := withDSN(tc.envs)
			if v, ok := tc.envs[EnvDatabaseURL]; ok {
				e[EnvDatabaseURL] = v
			}
			env(t, e)

			cfg, err := Load(tc.role)
			if err == nil {
				t.Fatalf("Load(%q) accepted a manifest it must refuse; got %+v", tc.role, cfg)
			}
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal does not mention %q.\ngot:\n%s", want, msg)
				}
			}
		})
	}
}

// TestEveryViolationIsReportedAtOnce is the "one edit, not five deploys" rule.
// A manifest with four independent problems must name all four.
func TestEveryViolationIsReportedAtOnce(t *testing.T) {
	env(t, map[string]string{
		EnvDatabaseURL: "", // required and absent
		EnvLeaseTTL:    "1m",
		EnvSlotRearm:   "1s",
		EnvReaperBatch: "0",
	})
	_, err := Load("scheduler")
	if err == nil {
		t.Fatal("Load accepted four simultaneous violations")
	}
	msg := err.Error()
	for _, want := range []string{EnvDatabaseURL, EnvLeaseTTL, EnvSlotRearm, EnvReaperBatch} {
		if !strings.Contains(msg, want) {
			t.Errorf("report omits %s.\ngot:\n%s", want, msg)
		}
	}
}

// TestParseFailuresSuppressTheValidationNoise. A value that failed to parse
// falls back to its default, and validating the default would print an
// assertion about a number the operator never wrote.
func TestParseFailuresSuppressTheValidationNoise(t *testing.T) {
	env(t, withDSN(map[string]string{EnvLeaseTTL: "fifteen"}))
	_, err := Load("api")
	if err == nil {
		t.Fatal("Load accepted an unparseable duration")
	}
	if strings.Contains(err.Error(), "23514") {
		t.Errorf("a parse failure also reported a CHECK-constraint violation:\n%s", err)
	}
}

// TestWithoutDatabase covers the one role that must keep working while the
// control plane is the thing being investigated.
func TestWithoutDatabase(t *testing.T) {
	env(t, nil)
	if _, err := Load("ctl", WithoutDatabase()); err != nil {
		t.Fatalf("ctl refused to load without a DSN: %v", err)
	}
	if _, err := Load("ctl"); err == nil {
		t.Fatal("a role that requires a DSN loaded without one")
	}
}

// ---------------------------------------------------------------------------
// Every variable reaches something
// ---------------------------------------------------------------------------

// TestEveryVariableIsRead sets each variable to a distinctive value and finds
// it again on the Config. It is the direct guard against this package's
// characteristic bug: a knob that is named, parsed, validated, and then not
// carried anywhere.
func TestEveryVariableIsRead(t *testing.T) {
	pems := testPEMs(t)
	env(t, map[string]string{
		EnvDatabaseURL:           "postgres://farm:hunter2@db.internal:6432/farm",
		EnvDBMaxConns:            "17",
		EnvDBConnectTimeout:      "11s",
		EnvComponent:             "scheduler-a",
		EnvLogLevel:              "debug",
		EnvShutdownGrace:         "45s",
		EnvAPIAddr:               "127.0.0.1:8517",
		EnvMetricsAddr:           "127.0.0.1:9517",
		EnvNodeAddr:              "127.0.0.1:8518",
		EnvAPIBaseURL:            "https://farm.example:8443",
		EnvLeaseTTL:              "30m",
		EnvLeaseGrace:            "40m",
		EnvLeaseRenewInterval:    "45s",
		EnvLeaseWitnessInterval:  "3m",
		EnvLeaseWitnessMaxExt:    "7",
		EnvSlotRearm:             "40s",
		EnvReaperInterval:        "7s",
		EnvReaperBatch:           "42",
		EnvReaperGapFloor:        "90s",
		EnvReaperComponent:       " reaper , api ,scheduler-a, jobrunner ",
		EnvHeartbeatEvery:        "4s",
		EnvNodeSelfFence:         "30s",
		EnvFenceMargin:           "9s",
		EnvNodeADBEndpoint:       "10.0.0.9:5037",
		EnvNodeHostID:            "rack1-host7",
		EnvWatchdogInterval:      "6s",
		EnvMigrationsTable:       "farm.schema_version",
		EnvMigrationsDir:         "/srv/migrations",
		EnvArtifactGCGrace:       "2h",
		EnvNodeToken:             "s3cret-node-token",
		EnvSysfsRoot:             "/host/sys/bus/usb/devices",
		EnvTopoOverrides:         "/etc/farm/overrides.json",
		EnvTopoRetireVanished:    "true",
		EnvTopoMaxRetireFraction: "0.5",
		EnvTopoDryRun:            "1",
		EnvTopoMinPorts:          "4",
		EnvTopoAdoptEmpty:        "TRUE",
		EnvTopoIncludeRootHubs:   "t",
		EnvTopoInclude:           " 3-1.4 , 3-1.5 ",
		EnvTopoExclude:           "1-0",
		EnvTopoInterval:          "2m",
		EnvTopoCallTimeout:       "20s",
		EnvDBRole:                "farm_scheduler",
		EnvBatteryTempRise:       "35",
		EnvBatteryTempMax:        "500",
		EnvBatteryDrain:          "25",
		EnvChargeMinPct:          "35",
		EnvChargeMaxPct:          "75",
		EnvChargeInterval:        "3m",
		EnvFenceTLSCert:          pems.cert,
		EnvFenceTLSKey:           pems.key,
		EnvFenceTLSCA:            pems.ca,
		EnvFenceListen:           "0.0.0.0:5138",
		EnvFenceAdvertise:        "h07.lab.example:5138",
		EnvFencePollInterval:     "3s",
	})
	cfg, err := Load("scheduler")
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{EnvComponent, cfg.Component, "scheduler-a"},
		{EnvLogLevel, cfg.LogLevel, "debug"},
		{EnvShutdownGrace, cfg.ShutdownGrace, 45 * time.Second},
		{EnvDBMaxConns, cfg.DBMaxConns, int32(17)},
		{EnvDBConnectTimeout, cfg.DBConnectTimeout, 11 * time.Second},
		{EnvDBRole, cfg.DBRole, "farm_scheduler"},
		{EnvAPIAddr, cfg.APIAddr, "127.0.0.1:8517"},
		{EnvMetricsAddr, cfg.MetricsAddr, "127.0.0.1:9517"},
		{EnvNodeAddr, cfg.NodeAddr, "127.0.0.1:8518"},
		{EnvAPIBaseURL, cfg.APIBaseURL, "https://farm.example:8443"},
		{EnvLeaseTTL, cfg.Lease.TTL, 30 * time.Minute},
		{EnvLeaseGrace, cfg.Lease.Grace, 40 * time.Minute},
		{EnvLeaseRenewInterval, cfg.Lease.RenewInterval, 45 * time.Second},
		{EnvLeaseWitnessInterval, cfg.Lease.WitnessInterval, 3 * time.Minute},
		{EnvLeaseWitnessMaxExt, cfg.Lease.MaxWitnessExtensions, 7},
		{EnvSlotRearm, cfg.Lease.SlotRearm, 40 * time.Second},
		{EnvReaperInterval, cfg.Reaper.Interval, 7 * time.Second},
		{EnvReaperBatch, cfg.Reaper.Batch, 42},
		{EnvReaperGapFloor, cfg.Reaper.GapFloor, 90 * time.Second},
		{EnvHeartbeatEvery, cfg.Reaper.HeartbeatInterval, 4 * time.Second},
		{EnvNodeSelfFence, cfg.Node.SelfFenceTimeout, 30 * time.Second},
		{EnvFenceMargin, cfg.Node.FenceSafetyMargin, 9 * time.Second},
		{EnvNodeADBEndpoint, cfg.Node.ADBEndpoint, "10.0.0.9:5037"},
		{EnvNodeHostID, cfg.Node.HostID, "rack1-host7"},
		{EnvWatchdogInterval, cfg.WatchdogInterval, 6 * time.Second},
		{EnvMigrationsTable, cfg.MigrationsTable, "farm.schema_version"},
		{EnvMigrationsDir, cfg.MigrationsDir, "/srv/migrations"},
		{EnvArtifactGCGrace, cfg.ArtifactGCGrace, 2 * time.Hour},
		{EnvNodeToken, cfg.Node.Token, "s3cret-node-token"},
		{EnvSysfsRoot, cfg.Topo.SysfsRoot, "/host/sys/bus/usb/devices"},
		{EnvTopoOverrides, cfg.Topo.OverridesPath, "/etc/farm/overrides.json"},
		{EnvTopoRetireVanished, cfg.Topo.RetireVanished, true},
		{EnvTopoMaxRetireFraction, cfg.Topo.MaxRetireFraction, 0.5},
		{EnvTopoDryRun, cfg.Topo.DryRun, true},
		{EnvTopoMinPorts, cfg.Topo.MinPorts, 4},
		{EnvTopoAdoptEmpty, cfg.Topo.AdoptEmpty, true},
		{EnvTopoIncludeRootHubs, cfg.Topo.IncludeRootHubs, true},
		{EnvTopoInterval, cfg.Topo.Interval, 2 * time.Minute},
		{EnvTopoCallTimeout, cfg.Topo.CallTimeout, 20 * time.Second},
		{EnvBatteryTempRise, cfg.Battery.TempRiseDCPerMin, 35},
		{EnvBatteryTempMax, cfg.Battery.TempMaxDC, 500},
		{EnvBatteryDrain, cfg.Battery.DrainPctPerHour, 25},
		{EnvChargeMinPct, cfg.Charge.MinPct, 35},
		{EnvChargeMaxPct, cfg.Charge.MaxPct, 75},
		{EnvChargeInterval, cfg.Charge.Interval, 3 * time.Minute},
		{EnvFenceTLSCert, cfg.Fence.CertFile, pems.cert},
		{EnvFenceTLSKey, cfg.Fence.KeyFile, pems.key},
		{EnvFenceTLSCA, cfg.Fence.CAFile, pems.ca},
		{EnvFenceListen, cfg.Fence.Listen, "0.0.0.0:5138"},
		{EnvFenceAdvertise, cfg.Fence.Advertise, "h07.lab.example:5138"},
		{EnvFencePollInterval, cfg.Fence.PollInterval, 3 * time.Second},
		{"fence proxy on", cfg.Fence.Enabled(), true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	// The list is split, trimmed and emptied of blanks.
	if want := []string{"reaper", "api", "scheduler-a", "jobrunner"}; !slices.Equal(cfg.Reaper.Components, want) {
		t.Errorf("%s: got %v, want %v", EnvReaperComponent, cfg.Reaper.Components, want)
	}
	if want := []string{"3-1.4", "3-1.5"}; !slices.Equal(cfg.Topo.Include, want) {
		t.Errorf("%s: got %v, want %v", EnvTopoInclude, cfg.Topo.Include, want)
	}
	if want := []string{"1-0"}; !slices.Equal(cfg.Topo.Exclude, want) {
		t.Errorf("%s: got %v, want %v", EnvTopoExclude, cfg.Topo.Exclude, want)
	}
	// FARM_COMPONENT reached the component this process IS.
	if got := cfg.ComponentFor("scheduler"); got != "scheduler-a" {
		t.Errorf("ComponentFor(scheduler) = %q, want the renamed %q", got, "scheduler-a")
	}
	// A password must not survive into a log line or a connection error.
	if red := cfg.RedactedDatabaseURL(); strings.Contains(red, "hunter2") {
		t.Errorf("RedactedDatabaseURL leaked the password: %s", red)
	}
}

// TestRedactedDatabaseURL covers the forms a DSN actually arrives in.
func TestRedactedDatabaseURL(t *testing.T) {
	cases := []struct{ name, dsn, wantAbsent, wantPresent string }{
		{"unset", "", "", "(unset)"},
		{"userinfo password", "postgres://farm:hunter2@db/farm", "hunter2", "farm:xxxxx@db"},
		{"query password", "postgres://farm@db/farm?password=hunter2", "hunter2", "xxxxx"},
		{"sslpassword", "postgres://farm@db/farm?sslpassword=hunter2", "hunter2", "xxxxx"},
		{"keyword form", "host=db user=farm password=hunter2", "hunter2", "(redacted)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{DatabaseURL: tc.dsn}
			got := c.RedactedDatabaseURL()
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("%q leaked %q", got, tc.wantAbsent)
			}
			if !strings.Contains(got, tc.wantPresent) {
				t.Errorf("got %q, want it to contain %q", got, tc.wantPresent)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Component naming
// ---------------------------------------------------------------------------

// TestComponentForNamesWhatTheProcessIs. FARM_COMPONENT renames the component
// a process IS and never one it merely CONTAINS, because each component a
// multiplexed process runs writes its own heartbeat row.
func TestComponentForNamesWhatTheProcessIs(t *testing.T) {
	cases := []struct {
		role      string
		component string
		want      map[string]string // canonical -> heartbeat key
	}{
		{"scheduler", "scheduler-a", map[string]string{"scheduler": "scheduler-a"}},
		{"api", "api-canary", map[string]string{"api": "api-canary"}},
		{"watchdog", "watchdog-rack1", map[string]string{"watchdog": "watchdog-rack1"}},
		{"node", "node-h7", map[string]string{"node": "node-h7", "enroll": "enroll"}},
		{"all", "all", map[string]string{"api": "api", "scheduler": "scheduler", "jobrunner": "jobrunner"}},
		{"demo", "demo", map[string]string{"api": "api", "reaper": "reaper"}},
	}
	for _, tc := range cases {
		t.Run(tc.role+"/"+tc.component, func(t *testing.T) {
			cfg := &Config{role: tc.role, Component: tc.component}
			for canon, want := range tc.want {
				if got := cfg.ComponentFor(canon); got != want {
					t.Errorf("ComponentFor(%q) = %q, want %q", canon, got, want)
				}
			}
		})
	}
}

// TestHeartbeatComponentsMatchTheRole. The list is what the BLOCKER 8
// assertion walks and what the startup summary prints, so it has to be the set
// of rows the process will really write.
func TestHeartbeatComponentsMatchTheRole(t *testing.T) {
	env(t, withDSN(map[string]string{
		EnvComponent:       "scheduler-a",
		EnvReaperComponent: "reaper,api,scheduler-a,jobrunner",
	}))
	cfg, err := Load("scheduler")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.HeartbeatComponents(); !slices.Equal(got, []string{"scheduler-a"}) {
		t.Errorf("HeartbeatComponents() = %v, want [scheduler-a]", got)
	}

	env(t, withDSN(nil))
	all, err := Load("all")
	if err != nil {
		t.Fatal(err)
	}
	if got := all.HeartbeatComponents(); !slices.Equal(got, roleComponents["all"]) {
		t.Errorf("HeartbeatComponents() = %v, want %v", got, roleComponents["all"])
	}
	// An unknown role still gets an assertion rather than a free pass.
	unknown := &Config{role: "invented", Component: "api"}
	if got := unknown.HeartbeatComponents(); !slices.Equal(got, []string{"api"}) {
		t.Errorf("HeartbeatComponents() for an unknown role = %v, want [api]", got)
	}
}

// TestRoleComponentsCoversEveryRole reads the dispatch table out of
// cmd/farmd/main.go and requires this package to know about every role in it.
//
// The two lists are the same fact stated twice: what farmd can be started as,
// and what each of those writes to farm.component_heartbeat. When they drift,
// a new role gets the fall-back assumption that it beats under its own name —
// which is right for a single-component role and silently wrong for one that
// runs several, and being silently wrong there is BLOCKER 8.
func TestRoleComponentsCoversEveryRole(t *testing.T) {
	src, err := os.ReadFile("../../cmd/farmd/main.go")
	if err != nil {
		t.Skipf("cannot read the dispatch table: %v", err)
	}
	// The multi-role case in run()'s switch, e.g.
	//   case "api", "scheduler", ... "demo":
	//       err = runRole(...)
	block := regexp.MustCompile(`(?s)case ("[a-z]+",\s*)+"[a-z]+":\s*\n\s*err = runRole`)
	m := block.Find(src)
	if m == nil {
		t.Skip("the dispatch table in cmd/farmd/main.go no longer has the shape this test reads")
	}
	roles := regexp.MustCompile(`"([a-z]+)"`).FindAllStringSubmatch(string(m), -1)
	if len(roles) < 5 {
		t.Fatalf("read only %d roles out of the dispatch table; the regexp is wrong", len(roles))
	}
	for _, r := range roles {
		if _, ok := roleComponents[r[1]]; !ok {
			t.Errorf("farmd can be started as %q but roleComponents does not say what it beats as; "+
				"add it, or the BLOCKER 8 assertion assumes it writes exactly one row named %q",
				r[1], r[1])
		}
	}
	// ctl and migrate are dispatched separately and beat for nothing.
	for _, r := range []string{"ctl", "migrate"} {
		if names, ok := roleComponents[r]; !ok || len(names) != 0 {
			t.Errorf("roleComponents[%q] = %v, want an empty list: it writes no heartbeat", r, names)
		}
	}
}

// ---------------------------------------------------------------------------
// Runtime database role
// ---------------------------------------------------------------------------

// TestDBRoleFollowsTheProcess. FARM_DB_ROLE is accepted exactly when it names
// the role of the process being started, and the summary then says the role
// is in force; unset, the summary says out loud that the firewall is not
// assumed, so a manifest is never read as more protected than it is.
func TestDBRoleFollowsTheProcess(t *testing.T) {
	for process, role := range dbRoleForProcess {
		t.Run(process, func(t *testing.T) {
			if _, ok := roleComponents[process]; !ok {
				t.Fatalf("dbRoleForProcess names %q, which farmd cannot be started as", process)
			}
			env(t, withDSN(map[string]string{EnvDBRole: role}))
			cfg, err := Load(process)
			if err != nil {
				t.Fatalf("Load(%q) refused its own role %s: %v", process, role, err)
			}
			if cfg.DBRole != role {
				t.Errorf("DBRole = %q, want %q", cfg.DBRole, role)
			}
			s := cfg.Summary()
			if !strings.Contains(s, role) || !strings.Contains(s, "SET ROLE") {
				t.Errorf("the summary does not say the process runs as %s:\n%s", role, s)
			}
			if strings.Contains(s, "NOT assumed") {
				t.Errorf("the summary calls the firewall unassumed while %s is set:\n%s", EnvDBRole, s)
			}
		})
	}

	env(t, withDSN(nil))
	cfg, err := Load("reaper")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBRole != "" {
		t.Errorf("DBRole = %q with %s unset, want empty", cfg.DBRole, EnvDBRole)
	}
	if s := cfg.Summary(); !strings.Contains(s, "NOT assumed") || !strings.Contains(s, EnvDBRole) {
		t.Errorf("with %s unset the summary must say the firewall is not assumed:\n%s", EnvDBRole, s)
	}

	// ctl never opens the pool and migrate must run as the schema owner, so a
	// stray FARM_DB_ROLE in a shared environment must not stop either — they
	// are the two commands an operator reaches for when the loops are the
	// thing being investigated.
	for _, process := range []string{"ctl", "migrate"} {
		env(t, map[string]string{EnvDBRole: DBRoleReaper})
		if _, err := Load(process, WithoutDatabase()); err != nil {
			t.Errorf("Load(%q) refused a %s it does not use: %v", process, EnvDBRole, err)
		}
	}
}

// TestDBRoleRefusedWhereNoRoleExists. Every process farmd can be started as
// that opens the pool, is one component, and has no entry in the allowlist
// refuses FARM_DB_ROLE by name, listing the processes the firewall covers.
// The loop is over roleComponents rather than a literal list so a process
// added later — the charge-policy mesh, say — is covered the day it appears:
// it runs as the login user until it is given a role of its own, and never
// silently as another process's.
func TestDBRoleRefusedWhereNoRoleExists(t *testing.T) {
	covered := 0
	for process := range roleComponents {
		if _, mapped := dbRoleForProcess[process]; mapped {
			continue
		}
		if c := (&Config{role: process}); !c.opensPool() || c.multiplexed() {
			continue
		}
		covered++
		t.Run(process, func(t *testing.T) {
			env(t, withDSN(map[string]string{EnvDBRole: DBRoleReaper}))
			_, err := Load(process)
			if err == nil {
				t.Fatalf("Load(%q) accepted %s=%s, which belongs to the reaper", process, EnvDBRole, DBRoleReaper)
			}
			for _, want := range []string{
				EnvDBRole, "no runtime role", `"` + process + `"`,
				strings.Join(dbRoleProcesses(), ", "),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal for %q does not say %q:\n%s", process, want, err)
				}
			}
		})
	}
	if covered == 0 {
		t.Fatal("no process outside the allowlist opens the pool, so this test covers nothing")
	}
}

// ---------------------------------------------------------------------------
// Metrics address
// ---------------------------------------------------------------------------

func TestMetricsAddr(t *testing.T) {
	accept := []string{":9090", "127.0.0.1:9090", "[::1]:9090", "0.0.0.0:9090", "localhost:9090"}
	for _, addr := range accept {
		t.Run("accept "+addr, func(t *testing.T) {
			env(t, withDSN(map[string]string{EnvMetricsAddr: addr}))
			cfg, err := Load("scheduler")
			if err != nil {
				t.Fatalf("%s=%s refused: %v", EnvMetricsAddr, addr, err)
			}
			if cfg.MetricsDisabled() {
				t.Errorf("%s=%s read as disabled", EnvMetricsAddr, addr)
			}
		})
	}
	for _, addr := range []string{"off", "OFF", " off "} {
		t.Run("disable "+addr, func(t *testing.T) {
			env(t, withDSN(map[string]string{EnvMetricsAddr: addr}))
			cfg, err := Load("scheduler")
			if err != nil {
				t.Fatalf("%s=%q refused: %v", EnvMetricsAddr, addr, err)
			}
			if !cfg.MetricsDisabled() {
				t.Errorf("%s=%q did not disable the listener", EnvMetricsAddr, addr)
			}
		})
	}
}

// TestMetricsListenAddr covers who binds a second listener and who does not.
//
// The question is not "are the two strings equal". A role that never serves
// HTTP has nothing on FARM_API_ADDR, so pointing both variables at one port
// from a shared ConfigMap must still get that role a /metrics endpoint —
// otherwise the roles this listener exists for are exactly the ones that
// silently keep exporting nothing.
func TestMetricsListenAddr(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		metrics string
		api     string
		want    string
		bind    bool
	}{
		{"scheduler binds its own", "scheduler", ":9090", ":8080", ":9090", true},
		{"api binds a second port", "api", ":9090", ":8080", ":9090", true},
		{"api already serves that address", "api", ":8080", ":8080", "", false},
		{"api, wildcard spelled differently", "api", "0.0.0.0:8080", ":8080", "", false},
		{"demo already serves that address", "demo", "[::]:8080", "0.0.0.0:8080", "", false},
		{"all binds a second port", "all", ":9090", ":8080", ":9090", true},
		// Nothing else in a scheduler binds FARM_API_ADDR, so one shared
		// address is still one listener that has to exist.
		{"scheduler with both variables equal", "scheduler", ":8080", ":8080", ":8080", true},
		{"switched off", "reaper", MetricsOff, ":8080", "", false},
		{"ctl never listens", "ctl", ":9090", ":8080", "", false},
		{"migrate never listens", "migrate", ":9090", ":8080", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{role: tc.role, MetricsAddr: tc.metrics, APIAddr: tc.api}
			got, bind := cfg.MetricsListenAddr()
			if bind != tc.bind || got != tc.want {
				t.Errorf("MetricsListenAddr() = (%q, %v), want (%q, %v)", got, bind, tc.want, tc.bind)
			}
		})
	}
}

// TestCtlSurvivesAMalformedMetricsAddr. ctl is the command an operator reaches
// for when the control plane is the thing being investigated, and it binds
// nothing. A stray variable in a shared environment must not be what stops it
// from answering.
func TestCtlSurvivesAMalformedMetricsAddr(t *testing.T) {
	env(t, map[string]string{EnvMetricsAddr: "9090"})
	if _, err := Load("ctl", WithoutDatabase()); err != nil {
		t.Errorf("ctl refused to start over a listener it never binds: %v", err)
	}
	// The roles that do bind it are still refused.
	env(t, withDSN(map[string]string{EnvMetricsAddr: "9090"}))
	if _, err := Load("reaper"); err == nil {
		t.Error("a role that binds the metrics listener accepted an address it cannot bind")
	}
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

// TestSummaryShowsWhatTheProcessDecided. The summary is printed by every role
// at startup, and it is the only place an operator sees the resolved values.
// A field missing from it is a value that has to be guessed at from a
// manifest.
func TestSummaryShowsWhatTheProcessDecided(t *testing.T) {
	env(t, withDSN(map[string]string{
		EnvDatabaseURL:        "postgres://farm:hunter2@db.internal:6432/farm",
		EnvLeaseRenewInterval: "45s",
		EnvLeaseTTL:           "30m",
	}))
	cfg, err := Load("all")
	if err != nil {
		t.Fatal(err)
	}

	s := cfg.Summary()
	for _, want := range []string{
		// the role, and every heartbeat key it will write
		"all", "api, scheduler",
		// the DSN, redacted, and the values the assertions were made about
		"db.internal:6432", "30m0s", "45s",
		"35s",   // slot rearm
		"9090",  // the metrics listener that now exists
		"40",    // 30m/45s renewal attempts
		"xxxxx", // the password, gone
		// the artifact sweep's fence, at its default
		"artifact gc      = grace 1h0m0s",
		// U9: the battery thresholds, in the degrees a human reads, and the
		// statement that they end nothing.
		"2.0 C/min", "45.0 C", "15 %/h", "ends nothing",
		// U8: the charge band, and the one sentence that keeps it honest.
		"above 80%", "release at 40%", "2m0s", "a lease is never touched",
		// BOTH halves of the fence proxy, which are two different things
		// under two sets of variables: the mTLS listener a host SERVES
		// (Config.Fence, FARM_FENCE_TLS_*) and what this process PRESENTS to
		// it (Config.FenceClient, FARM_FENCE_CLIENT_*).
		//
		// Asserted here because the compiler cannot: both groups were named
		// Fence when their branches merged, so one silently shadowed the
		// other and the struct still compiled. A summary missing a line is
		// the cheapest observable form of that mistake, and an operator who
		// reads only one of these lines concludes the fence is enforced on a
		// wire where it is not.
		"fence proxy      =", "fence client     =",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary omits %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "hunter2") {
		t.Fatalf("the startup summary printed the database password:\n%s", s)
	}
	// The witness line reads as a value in force because it is one: the
	// jobrunner starts a witness loop for every placement. The summary said
	// NOT STARTED for as long as that was true, and must not go on saying it
	// now that an operator reading it would go looking for wiring that
	// exists.
	if strings.Contains(s, "NOT STARTED") {
		t.Errorf("the summary still says no role starts a witness loop; the jobrunner does:\n%s", s)
	}
	if !strings.Contains(s, "started by the jobrunner") {
		t.Errorf("the summary does not say which role starts the witness loop:\n%s", s)
	}
	// The two cadences the operator did NOT set are printed beside the one
	// they did, because they are what the witness actually runs on: an
	// operator reading "witness every 2m" and nothing else would have no way
	// to know the evidence behind it goes stale after 90s.
	for _, want := range []string{
		"marker           = rewritten on the device every " + cfg.Lease.MarkerInterval().String(),
		"younger than " + cfg.Lease.MaxEvidenceAge().String(),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary does not print %q, the derived marker cadence:\n%s", want, s)
		}
	}

	env(t, withDSN(map[string]string{EnvMetricsAddr: MetricsOff}))
	off, err := Load("scheduler")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(off.Summary(), "no /metrics listener") {
		t.Errorf("a disabled metrics listener is not stated:\n%s", off.Summary())
	}
}

// TestSummaryNeverPrintsTheNodeToken. The summary goes to stderr of every
// role at info level, which is to say into every log aggregator the farm has.
// The token authenticates a call that cuts power to a port holding somebody's
// lease, so the line may say that it exists and nothing else.
func TestSummaryNeverPrintsTheNodeToken(t *testing.T) {
	const token = "tok-9f2a1c-never-in-a-log"
	env(t, withDSN(map[string]string{EnvNodeToken: token}))
	cfg, err := Load("recovery")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Summary()
	if strings.Contains(s, token) {
		t.Fatalf("the startup summary printed the node token:\n%s", s)
	}
	if !strings.Contains(s, "node token") || !strings.Contains(s, "set") {
		t.Errorf("the summary does not say that a node token is set:\n%s", s)
	}

	// And its absence is stated with its consequence, since nothing else in
	// the process will say so after the first warning.
	env(t, withDSN(nil))
	none, err := Load("recovery")
	if err != nil {
		t.Fatal(err)
	}
	if s := none.Summary(); !strings.Contains(s, "unset") || !strings.Contains(s, "tiers 3 and 4") {
		t.Errorf("an unset node token is not stated with its consequence:\n%s", s)
	}
}

// TestSummarySaysWhatDiscoveryWillDo. Every topo knob has to be visible in the
// summary, and the retirement policy in particular has to read as OFF when it
// is off: an operator who sees no line about vanished ports assumes they are
// handled, and they are not.
func TestSummarySaysWhatDiscoveryWillDo(t *testing.T) {
	env(t, withDSN(map[string]string{
		EnvSysfsRoot:       "/host/sys/bus/usb/devices",
		EnvTopoInclude:     "3-1.4",
		EnvTopoExclude:     "1-0",
		EnvTopoMinPorts:    "7",
		EnvTopoInterval:    "3m",
		EnvTopoCallTimeout: "9s",
	}))
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Summary()
	for _, want := range []string{
		"/host/sys/bus/usb/devices", "3-1.4", "1-0", "min 7 ports", "3m0s", "9s",
		"overrides none", "adopt empty off", "root hubs off",
		"topo removals    = off", "25%",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary omits %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "DRY RUN") {
		t.Errorf("summary claims a dry run that was not asked for:\n%s", s)
	}

	env(t, withDSN(map[string]string{
		EnvTopoRetireVanished:    "true",
		EnvTopoMaxRetireFraction: "0.4",
		EnvTopoDryRun:            "true",
		EnvTopoAdoptEmpty:        "true",
	}))
	on, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	s = on.Summary()
	for _, want := range []string{"retire vanished ports", "40%", "DRY RUN", "adopt empty on"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary omits %q:\n%s", want, s)
		}
	}
}

// TestOverridesFileIsCheckedOnlyWhereItIsRead. A shared manifest hands every
// role the same FARM_TOPO_OVERRIDES, and the file exists on the device host
// alone. The node must refuse a path it cannot open; the API must not refuse
// to start over a file it will never read.
func TestOverridesFileIsCheckedOnlyWhereItIsRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	env(t, withDSN(map[string]string{EnvTopoOverrides: missing}))
	if _, err := Load("api"); err != nil {
		t.Errorf("api refused to start over an overrides file it never reads: %v", err)
	}
	_, err := Load("node")
	if err == nil {
		t.Fatal("node accepted an overrides path that does not exist")
	}
	if !strings.Contains(err.Error(), EnvTopoOverrides) {
		t.Errorf("refusal does not name %s:\n%s", EnvTopoOverrides, err)
	}

	// A directory is not a file that can hold a JSON object.
	env(t, withDSN(map[string]string{EnvTopoOverrides: t.TempDir()}))
	if _, err := Load("node"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("node accepted a directory as an overrides file: %v", err)
	}

	// A file that exists is accepted here; its content is topo's to judge.
	present := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(present, []byte(`{"hub_tokens":{"3-1.4":"3"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env(t, withDSN(map[string]string{EnvTopoOverrides: present}))
	cfg, err := Load("node")
	if err != nil {
		t.Fatalf("node refused an overrides file that exists: %v", err)
	}
	if cfg.Topo.OverridesPath != present {
		t.Errorf("OverridesPath = %q, want %q", cfg.Topo.OverridesPath, present)
	}
}

// TestValidateIsIdempotentAndSeparate. Validate must report the same
// violations Load does, minus the DSN requirement, so a caller that builds a
// Config by hand gets the same assertions.
func TestValidateIsIdempotentAndSeparate(t *testing.T) {
	env(t, withDSN(nil))
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config Load accepted was refused by Validate: %v", err)
	}

	cfg.DatabaseURL = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate enforced the role-dependent DSN requirement, which is Load's: %v", err)
	}

	cfg.Lease.SlotRearm = time.Second
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted a slot rearm below the node self-fence timeout")
	}
}
