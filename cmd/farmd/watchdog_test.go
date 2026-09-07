package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// The health plane has two deployment shapes and ONE variable chooses between
// them, so the two things that can go wrong are choosing the wrong shape and
// carrying a value across the boundary. Both are silent at runtime — a watchdog
// that supervises the wrong set of hosts still starts, still beats, and still
// writes health rows — so they are pinned here rather than left to be noticed
// from a graph that is missing a rack row.
//
// Why this test exists at all: the fleet-wide shape used to be a SECOND
// implementation of host membership in roles.go (watchdogsForHosts), which
// SELECTed farm.hosts once at startup and returned
// "no hosts registered; set FARM_HOST_ID or seed farm.hosts" when the table was
// empty. A farm whose schema has just been migrated has an empty farm.hosts by
// definition, so the farm profile in docker-compose.yml shipped a role that
// exited non-zero on every start of a new installation and restarted forever.
// Both shapes are now one watchdog.Watchdog, which re-reads the host list every
// cycle, and what is left to get wrong is exactly the two fields below.
//
// It sets the environment rather than reading it, including the variables it is
// asserting the ABSENCE of: config.Load reads the process environment, so a
// developer with FARM_HOST_ID exported would otherwise see a pass or a failure
// that nobody else can reproduce.
func TestWatchdogConfigTakesItsShapeFromTheHostID(t *testing.T) {
	const dsn = "postgres://farm@127.0.0.1:5432/farm?sslmode=disable"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	load := func(t *testing.T, hostID, endpoint string) *config.Config {
		t.Helper()
		t.Setenv(config.EnvDatabaseURL, dsn)
		t.Setenv(config.EnvNodeHostID, hostID)
		t.Setenv(config.EnvNodeADBEndpoint, endpoint)
		// A rename would change Component below and say nothing about the
		// shape, which is what this test is about.
		t.Setenv(config.EnvComponent, "")
		cfg, err := config.Load("watchdog")
		if err != nil {
			t.Fatalf("config.Load(watchdog): %v", err)
		}
		return cfg
	}

	// The fleet-wide shape: no FARM_HOST_ID, so every host in farm.hosts, and
	// each host's OWN farm.hosts.adb_endpoint.
	//
	// The empty ADBEndpoint is the load-bearing half. watchdog.Config.ADBEndpoint
	// overrides that column for every host the process watches, and
	// config.DefaultADBEndpoint means cfg.Node.ADBEndpoint is NEVER empty — so a
	// watchdogConfig that passed it through unconditionally would point every
	// host in the farm at 127.0.0.1:5037, which on a control-plane container is
	// nothing at all. The farm's health would then read as one host's, or as
	// silence, with no error anywhere.
	t.Run("no host id means every host, at its own endpoint", func(t *testing.T) {
		cfg := load(t, "", "")
		got := watchdogConfig(cfg, log, nil, cfg.Node.HostID)

		if got.HostID != "" {
			t.Errorf("HostID = %q with %s unset, want \"\" (every host in farm.hosts)",
				got.HostID, config.EnvNodeHostID)
		}
		if got.ADBEndpoint != "" {
			t.Errorf("ADBEndpoint = %q with %s unset, want \"\": a fleet-wide watchdog must "+
				"dial each host's own farm.hosts.adb_endpoint, and this value overrides that "+
				"column for EVERY host it watches", got.ADBEndpoint, config.EnvNodeHostID)
		}
		if got.Component != "watchdog" {
			t.Errorf("Component = %q, want %q: one process, one "+
				"farm.component_heartbeat row", got.Component, "watchdog")
		}
	})

	// Explicitly set and still overridden fleet-wide would be the same bug with
	// a manifest to blame it on: FARM_ADB_ENDPOINT is a node-local address, and
	// a process watching every host is by definition not node-local.
	t.Run("a host endpoint does not leak into the fleet-wide shape", func(t *testing.T) {
		cfg := load(t, "", "10.20.0.11:5037")
		if got := watchdogConfig(cfg, log, nil, cfg.Node.HostID); got.ADBEndpoint != "" {
			t.Errorf("ADBEndpoint = %q with %s set and %s unset, want \"\"",
				got.ADBEndpoint, config.EnvNodeADBEndpoint, config.EnvNodeHostID)
		}
	})

	// The pinned production shape: one pod per machine. The endpoint override is
	// the whole point here — a node-local pod reaches its own ADB server at
	// 127.0.0.1:5037 whatever address the rest of the fleet uses to reach it —
	// and the component carries the host id because farm.component_heartbeat is
	// keyed by component: a constant "watchdog" would have every pod in the farm
	// overwriting one row and keeping it fresh for the hosts that are dead.
	t.Run("a host id pins the host, the endpoint and the heartbeat key", func(t *testing.T) {
		cfg := load(t, "h01", "10.20.0.11:5037")
		got := watchdogConfig(cfg, log, nil, cfg.Node.HostID)

		if got.HostID != "h01" {
			t.Errorf("HostID = %q, want %q", got.HostID, "h01")
		}
		if got.ADBEndpoint != "10.20.0.11:5037" {
			t.Errorf("ADBEndpoint = %q, want the pinned %q", got.ADBEndpoint, "10.20.0.11:5037")
		}
		if got.Component != "watchdog:h01" {
			t.Errorf("Component = %q, want %q: one row per host, or a dead host's row "+
				"is kept fresh by its neighbours", got.Component, "watchdog:h01")
		}
	})

	// The DaemonSet shape that sets no endpoint at all, which is the one the
	// default exists for.
	t.Run("a pinned host with no endpoint takes the node-local default", func(t *testing.T) {
		cfg := load(t, "h01", "")
		if got := watchdogConfig(cfg, log, nil, cfg.Node.HostID); got.ADBEndpoint != config.DefaultADBEndpoint {
			t.Errorf("ADBEndpoint = %q, want config.DefaultADBEndpoint %q",
				got.ADBEndpoint, config.DefaultADBEndpoint)
		}
	})

	// The shape comes from the ARGUMENT, never from the environment behind it.
	//
	// `all` and `demo` multiplex every loop into one process and call
	// startWatchdog with "" unconditionally, and they have to: on a
	// single-machine farm the same shell exports FARM_HOST_ID for the node role
	// — docker-compose.yml and deploy/helm/README.md both instruct exactly that
	// — so a watchdogConfig that reached for cfg.Node.HostID would reduce the
	// whole control plane's health plane to one host AND force its endpoint to
	// config.DefaultADBEndpoint. In the demo, whose ADB servers are in-process
	// fakes on ports assigned at runtime, that address is nothing at all, and
	// every simulated device would read as dead health with no error logged.
	t.Run("an empty host id stays fleet-wide however the environment is set", func(t *testing.T) {
		cfg := load(t, "h01", "10.20.0.11:5037")
		got := watchdogConfig(cfg, log, nil, "")

		if got.HostID != "" {
			t.Errorf("HostID = %q for a caller that asked for every host", got.HostID)
		}
		if got.ADBEndpoint != "" {
			t.Errorf("ADBEndpoint = %q for a caller that asked for every host; %s reached a "+
				"process that is not node-local", got.ADBEndpoint, config.EnvNodeADBEndpoint)
		}
		if got.Component != "watchdog" {
			t.Errorf("Component = %q, want %q", got.Component, "watchdog")
		}
	})

	// Not the shape decision, but the two fields a reader of watchdogConfig
	// would assume are wired and that nothing else checks: the probe cadence,
	// and the admission class every connection this loop opens announces. A
	// watchdog that announced a lease class would be a health plane presenting
	// a fence at a device, which is the fusion the fence proxy exists to refuse.
	t.Run("the cadence and the maintenance class are carried", func(t *testing.T) {
		cfg := load(t, "", "")
		got := watchdogConfig(cfg, log, nil, cfg.Node.HostID)
		if got.Interval != cfg.WatchdogInterval {
			t.Errorf("Interval = %v, want the resolved %v", got.Interval, cfg.WatchdogInterval)
		}
		if len(got.ADBOptions) != len(maintenanceADBOptions(cfg)) || len(got.ADBOptions) == 0 {
			t.Errorf("ADBOptions has %d entries, want maintenanceADBOptions' %d",
				len(got.ADBOptions), len(maintenanceADBOptions(cfg)))
		}
	})
}
