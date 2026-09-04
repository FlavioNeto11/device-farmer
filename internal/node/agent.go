// Package node is farmd-node: the agent that runs ON a bare-metal device host,
// next to the phones and the hubs, and does the things the control plane
// physically cannot.
//
// # Why this process exists at all
//
// The recovery ladder has eight rungs. Tiers 1, 2, 5 and 7 are ADB verbs and a
// process anywhere in the cluster can perform them through the host's ADB
// server. Tiers 3 and 4 are not:
//
//	tier 3  USBDEVFS_RESET  an ioctl on /dev/bus/usb/BBB/DDD
//	tier 4  VBUS power      toggling the port's power through the hub
//
// Both need a process with the host's own filesystem and hub. Without one,
// internal/recovery/adbactuator.go refuses those rungs with a reason naming
// what is missing — deliberately, because reporting "no change" for a rung
// nobody attempted teaches the ladder that the hardware is unrecoverable and
// gets a healthy device quarantined. This package is what turns those
// refusals into real actions: an [Agent] implements [recovery.HostRunner].
//
// # What this agent may never do
//
// It may never end a lease. It has no lease vocabulary, it does not import
// internal/lease, and there is no code path from a USB event to an allocation
// decision anywhere in it. A device that this agent power-cycles is a device
// that is very probably in the middle of somebody's six-hour job: the whole
// point of tiers 3 and 4 is to repair the USB link UNDER a live lease, with
// the lease clock still ticking and the fence unmoved. DeviceFarmer/STF issue
// #663 is what the other arrangement looks like.
//
// Consequently, nothing here treats an error as a verdict about ownership. A
// Postgres error, an ADB error, a uhubctl that is not installed: each is
// logged, retried, and survived. The agent never exits because the world was
// briefly unreachable.
//
// # The heartbeat is NOT on the renewal path
//
// [Config.Component] is written to farm.component_heartbeat so an operator can
// see that a host agent is alive, and it must NOT be listed in
// FARM_REAPER_COMPONENTS. farm.reaper_arm ADDS each named component's downtime
// to every live lease's deadline; letting a host agent's outage move lease
// clocks would fuse the hardware plane into lease liveness, which is the exact
// fusion this system exists to prevent. The component name defaults to
// "node:<host id>" because farm.component_heartbeat is keyed by component: one
// shared "node" key would let a healthy host's beat hide a dead host.
//
// # host_epoch
//
// An ADB transport id is a small integer the server hands out and REUSES after
// it restarts, so (host_epoch, transport_id) is the only stable pair. This
// agent owns farm.hosts.host_epoch for its host and bumps it whenever it can
// see that the local ADB server restarted — see [Agent.probeADB] for what
// counts as seeing it, and for the cases it cannot prove either way, where it
// bumps rather than risk a stale transport id passing for a live one.
//
// # Platform split
//
// The Linux half of the agent lives in hostops.go and uhubctl.go behind
// `//go:build linux`. On any other GOOS the package still builds and still
// runs — it registers the host, discovers topology, enrolls devices and
// heartbeats — but the two hardware rungs answer with [ErrNotSupported] and a
// message naming the platform, because a developer's laptop should be able to
// build and test the control plane without pretending it can toggle VBUS.
//
// # The node endpoint
//
// When [Config.Addr] is set, the agent serves the two hardware rungs over
// HTTP. ALL THREE routes — including GET /node/v1/health — require the bearer
// token, so a liveness probe must send an Authorization header. Health reports
// which epoch is in force and whether the local ADB server is answering, which
// is exactly the reconnaissance an attacker would want before choosing a port
// to cut power to, and there is nothing on that endpoint a prober cannot carry
// one header to reach.
//
// Once an operation has been accepted its deadline comes from
// [Agent.opBudget], never from the request socket. A client that hangs up
// mid-cycle does not stop the cycle, for the same reason a job survives an ADB
// socket error: a transport event is not a verdict about hardware, and a rung
// reported as failed because nobody waited for it pushes the recovery ladder
// onto a more destructive rung.
package node

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// The agent is exactly the seam internal/recovery describes. Asserting it here
// means a signature drift is a compile error in this package rather than a
// tier-3 refusal discovered at 3am.
var _ recovery.HostRunner = (*Agent)(nil)

// Defaults.
const (
	// DefaultComponentPrefix is joined with the host id to form the
	// farm.component_heartbeat key. See the package doc: per-host, and never
	// in FARM_REAPER_COMPONENTS.
	DefaultComponentPrefix = "node:"

	// DefaultHostRefresh is how often farm.hosts.last_seen_at is refreshed and
	// the local ADB server is probed for a restart. Short, because the epoch
	// is what makes every recorded transport id meaningful.
	DefaultHostRefresh = 15 * time.Second

	// DefaultHeartbeat matches the control plane's default beat.
	DefaultHeartbeat = 5 * time.Second

	// DefaultDiscoverInterval is how often USB topology is re-discovered.
	// Racks are re-cabled by humans on a human timescale; an epoch bump also
	// triggers an immediate pass, which covers the fast case.
	DefaultDiscoverInterval = 5 * time.Minute

	// DefaultCallTimeout bounds one database statement or one ADB probe.
	DefaultCallTimeout = 10 * time.Second

	// DefaultPowerOffSettle is how long VBUS stays off during a cycle. Long
	// enough that a phone's USB PHY and the hub's port controller both see a
	// real disconnect rather than a glitch.
	DefaultPowerOffSettle = 3 * time.Second

	// DefaultPortReturnTimeout is how long the agent waits for the device to
	// re-enumerate in sysfs after power returns before reporting that it did
	// not come back.
	DefaultPortReturnTimeout = 30 * time.Second

	// DefaultEnrollBackoff is the initial delay before an enrollment loop that
	// returned is restarted. It doubles up to DefaultEnrollBackoffMax.
	DefaultEnrollBackoff    = 2 * time.Second
	DefaultEnrollBackoffMax = 30 * time.Second

	// enrollHealthyRun is how long an enrollment run has to last before the
	// restart that follows it is treated as a new fault rather than the
	// continuation of the previous one. Without a reset the backoff ratchets to
	// its maximum on the first bad afternoon and stays there for the life of
	// the process, so a phone plugged in after six healthy hours waits the
	// worst-case delay to be adopted.
	enrollHealthyRun = time.Minute

	// DefaultOpGrace is the slack the agent adds to its own settle and return
	// timeouts when it budgets one hardware operation end to end: the uhubctl
	// invocations either side of the settle, and the retries that give a dark
	// port its power back. See [Agent.opBudget].
	DefaultOpGrace = 30 * time.Second

	// DefaultAgentVersion is written to farm.hosts.agent_version when the
	// binary does not pass its own build stamp in.
	DefaultAgentVersion = "farmd-node"
)

// ErrNotSupported is the platform's answer when a hardware rung has no
// implementation here — a Windows or macOS build, or a Linux kernel too old
// for the operation to mean anything. It is wrapped, never returned bare, so
// the message always names what was attempted and on what.
var ErrNotSupported = errors.New("not supported on this host")

// ErrRefused marks the agent's own refusals: a request for another host, a
// power cycle whose blast radius nobody authorised, a kernel that will undo
// the operation silently. A refusal is not a failure of the hardware and must
// not be recorded as one — it means the agent declined to act, and the reason
// is in the wrapped message.
var ErrRefused = errors.New("refused by the host agent")

// DiscoverFunc runs ONE pass of USB topology discovery for this host: read the
// USB tree, call farm.register_slot for every position found. It is supplied
// by internal/topo; this package holds it as a function so the agent depends
// on the behaviour rather than on a constructor signature.
type DiscoverFunc func(ctx context.Context) error

// EnrollFunc runs enrollment until ctx ends: observe what is attached, record
// farm.identity_observations, call farm.resolve_device, brand new devices. It
// is supplied by internal/enroll. Returning early is treated as a fault and
// the agent restarts it with backoff, because an agent that stops enrolling
// silently stops adopting every device plugged in from then on.
type EnrollFunc func(ctx context.Context) error

// Config is the agent's wiring. Pool and HostID are required.
type Config struct {
	// Pool writes farm.hosts and farm.component_heartbeat. It never reads or
	// writes farm.leases: see the package doc.
	Pool *pgxpool.Pool

	// HostID is the farm.hosts row this agent owns. Required: an agent that
	// does not know which host it is cannot register topology and must not
	// power-cycle anything, since every devpath it might be handed would be
	// interpreted against the wrong rack.
	HostID string

	// ADBEndpoint is the LOCAL ADB server, normally 127.0.0.1:5037. It is
	// recorded in farm.hosts.adb_endpoint and probed for server restarts.
	ADBEndpoint string

	// Component is the farm.component_heartbeat key. Defaults to
	// "node:<HostID>".
	Component string

	// AgentVersion is written to farm.hosts.agent_version.
	AgentVersion string

	// Discover and Enroll are the topology and identity halves of the agent.
	// Both are optional; a nil one is reported at startup with the
	// consequence spelled out, because an agent that quietly never enrolls
	// looks identical to a farm where nobody plugs devices in.
	Discover         DiscoverFunc
	DiscoverInterval time.Duration
	Enroll           EnrollFunc

	HeartbeatInterval   time.Duration
	HostRefreshInterval time.Duration
	CallTimeout         time.Duration

	// UhubctlPath overrides the search for the uhubctl binary.
	UhubctlPath string
	// PowerOffSettle is how long VBUS stays off during a cycle.
	PowerOffSettle time.Duration
	// PortReturnTimeout bounds the wait for the device to come back.
	PortReturnTimeout time.Duration

	// Addr, when set, serves the two HostRunner operations over HTTP for a
	// control plane that runs elsewhere. Token is then required: this
	// endpoint can cut power to a rack of phones running other people's jobs.
	Addr  string
	Token string

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.ADBEndpoint == "" {
		c.ADBEndpoint = "127.0.0.1:5037"
	}
	if c.Component == "" {
		c.Component = DefaultComponentPrefix + c.HostID
	}
	if c.AgentVersion == "" {
		c.AgentVersion = DefaultAgentVersion
	}
	if c.DiscoverInterval <= 0 {
		c.DiscoverInterval = DefaultDiscoverInterval
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeat
	}
	if c.HostRefreshInterval <= 0 {
		c.HostRefreshInterval = DefaultHostRefresh
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.PowerOffSettle <= 0 {
		c.PowerOffSettle = DefaultPowerOffSettle
	}
	if c.PortReturnTimeout <= 0 {
		c.PortReturnTimeout = DefaultPortReturnTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Agent is farmd-node.
type Agent struct {
	cfg Config
	log *slog.Logger
	adb *adbwire.Client
	ops opsConfig

	// registered closes after the first successful farm.hosts upsert. Slots,
	// devices and observations all reference the host row, so discovery and
	// enrollment wait for it rather than racing it and logging foreign key
	// violations for the first minute of every deployment.
	registered     chan struct{}
	registeredOnce sync.Once

	// discoverNow asks the discovery loop for an out-of-band pass. It is
	// buffered by one: two coalesced requests are one pass, and a full buffer
	// means a pass is already pending.
	discoverNow chan struct{}

	mu sync.Mutex
	// adbSeen is false until the first successful probe of the local server.
	adbSeen bool
	// adbUp is whether the last probe reached the server. A down-then-up
	// transition on a LOCAL server is a restart: nothing else stops listening
	// on 127.0.0.1 and then starts again.
	adbUp bool
	// lastTransports maps devpath -> transport id from the last probe. A
	// device that kept its position while its transport id DECREASED proves a
	// server restart; see probeADB.
	lastTransports map[string]int64
	// epoch is the last host_epoch the database returned, for logging.
	epoch int64
}

// New validates cfg and returns an agent.
func New(cfg Config) (*Agent, error) {
	if cfg.Pool == nil {
		return nil, errors.New("node: Config.Pool is required")
	}
	if strings.TrimSpace(cfg.HostID) == "" {
		return nil, errors.New("node: Config.HostID is required; an agent that does " +
			"not know which host it runs on would interpret every devpath against " +
			"the wrong rack")
	}
	if cfg.Addr != "" && cfg.Token == "" {
		return nil, errors.New("node: Config.Token is required whenever Config.Addr " +
			"is set; the node endpoint can cut power to ports holding live leases " +
			"and must never be reachable unauthenticated")
	}
	cfg.applyDefaults()

	log := cfg.Logger.With("component", cfg.Component, "host", cfg.HostID)
	return &Agent{
		cfg: cfg,
		log: log,
		adb: adbwire.New(cfg.ADBEndpoint, adbwire.WithLogger(log)),
		ops: opsConfig{
			uhubctl:       cfg.UhubctlPath,
			offSettle:     cfg.PowerOffSettle,
			returnTimeout: cfg.PortReturnTimeout,
			log:           log,
		},
		registered:     make(chan struct{}),
		discoverNow:    make(chan struct{}, 1),
		lastTransports: make(map[string]int64),
	}, nil
}

// ---------------------------------------------------------------------------
// The process
// ---------------------------------------------------------------------------

// Run drives the agent until ctx is cancelled.
//
// Cancellation means "stop doing new work". It does not mean "put the hardware
// back": a port this agent powered on stays powered on, a device it reset
// stays reset, and no lease is affected either way. Run returns nil on a clean
// cancellation, and an error only for a fault that makes the agent unable to
// do its job at all — today, failing to bind the node listener.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	a.log.Info("farmd-node starting",
		"adb_endpoint", a.cfg.ADBEndpoint,
		"platform", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"discover_every", a.cfg.DiscoverInterval,
		"serving", a.cfg.Addr != "")

	if a.cfg.Discover == nil {
		a.log.Warn("no topology discovery is wired into this agent; farm.slots will " +
			"not gain positions from this host, and farm.resolve_device raises " +
			"no_data_found for any device plugged into an unregistered slot")
	}
	if a.cfg.Enroll == nil {
		a.log.Warn("no enrollment is wired into this agent; devices plugged into this " +
			"host will never be observed, resolved or branded")
	}
	if _, err := platform.kernelRelease(); err != nil {
		a.log.Warn("this build cannot perform the host-local recovery rungs; "+
			"tiers 3 and 4 will be refused with a reason rather than faked",
			"err", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				a.log.Error("agent loop stopped", "loop", name, "err", err)
				errs <- fmt.Errorf("node: %s: %w", name, err)
				// One fatal loop takes the process down rather than leaving a
				// half-alive agent whose heartbeat still claims a healthy host.
				cancel()
			}
		}()
	}

	start("host", a.hostLoop)
	start("heartbeat", a.heartbeatLoop)
	start("discover", a.discoverLoop)
	start("enroll", a.enrollLoop)
	if a.cfg.Addr != "" {
		start("serve", a.serve)
	}

	wg.Wait()
	close(errs)

	var joined []error
	for err := range errs {
		joined = append(joined, err)
	}
	if len(joined) > 0 {
		return errors.Join(joined...)
	}
	a.log.Info("farmd-node stopping; no lease was ever at risk from it")
	return nil
}

// hostLoop keeps farm.hosts current: the endpoint, the kernel release, the
// agent version, last_seen_at, and the host epoch.
func (a *Agent) hostLoop(ctx context.Context) error {
	// The first registration bumps the epoch unconditionally. The agent was
	// not running a moment ago, so it cannot prove the local ADB server it is
	// about to talk to is the same process that minted the transport ids
	// already recorded against this host. Bumping costs one integer and one
	// re-observation per device; NOT bumping leaves a stale transport id
	// indistinguishable from a live one, which is a wrong device away from a
	// reset landing on somebody's running job.
	bump, reason := true, "agent started and cannot prove the local adb server survived its absence"

	t := time.NewTicker(a.cfg.HostRefreshInterval)
	defer t.Stop()

	for {
		if restart, why := a.probeADB(ctx); restart {
			bump, reason = true, why
		}
		if err := a.registerHost(ctx, bump, reason); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Postgres being briefly unreachable is not a reason to stop being
			// a host agent: the hardware rungs still work without it.
			a.log.Warn("could not refresh this host's row", "err", err)
			hostRegistrations.WithLabelValues("error").Inc()
		} else {
			hostRegistrations.WithLabelValues("ok").Inc()
			if bump {
				epochBumps.Inc()
			}
			bump, reason = false, ""
		}

		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// registerHost upserts this host's row and returns the epoch in force.
//
// Every timestamp is now() evaluated by Postgres. The agent never tells the
// database what time it thinks it is: a host with a skewed clock would
// otherwise write a last_seen_at that outlives its own liveness.
func (a *Agent) registerHost(ctx context.Context, bumpEpoch bool, reason string) error {
	const q = `
INSERT INTO farm.hosts (id, adb_endpoint, kernel_release, agent_version, last_seen_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (id) DO UPDATE
   SET adb_endpoint   = EXCLUDED.adb_endpoint,
       kernel_release = COALESCE(EXCLUDED.kernel_release, farm.hosts.kernel_release),
       agent_version  = COALESCE(EXCLUDED.agent_version, farm.hosts.agent_version),
       last_seen_at   = now(),
       host_epoch     = farm.hosts.host_epoch + CASE WHEN $5 THEN 1 ELSE 0 END
RETURNING host_epoch, admin_state`

	cctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
	defer cancel()

	// A kernel release we cannot read is left NULL rather than guessed at; the
	// COALESCE above keeps whatever a previous, better-informed agent wrote.
	var kernel *string
	if k, err := platform.kernelRelease(); err == nil && k != "" {
		kernel = &k
	}

	var epoch int64
	var adminState string
	if err := a.cfg.Pool.QueryRow(cctx, q,
		a.cfg.HostID, a.cfg.ADBEndpoint, kernel, a.cfg.AgentVersion, bumpEpoch,
	).Scan(&epoch, &adminState); err != nil {
		return fmt.Errorf("node: register host %s: %w", a.cfg.HostID, err)
	}

	a.mu.Lock()
	prev := a.epoch
	a.epoch = epoch
	a.mu.Unlock()

	if bumpEpoch {
		// prev is zero on the first upsert of this process, where the agent has
		// no earlier reading of its own — reporting that as "previous 0" would
		// invent a comparison it never made.
		previous := "unknown to this agent"
		if prev != 0 {
			previous = strconv.FormatInt(prev, 10)
		}
		a.log.Info("host epoch bumped; transport ids minted under the previous epoch "+
			"are no longer comparable",
			"epoch", epoch, "previous", previous, "reason", reason)
		// Everything downstream re-enumerates after a server restart, so the
		// topology is worth re-reading now rather than at the next tick.
		a.kickDiscovery()
	}

	a.registeredOnce.Do(func() {
		a.log.Info("host registered", "epoch", epoch, "admin_state", adminState,
			"kernel_release", derefOr(kernel, "unknown"))
		close(a.registered)
	})
	return nil
}

// probeADB asks the local ADB server what it has, and reports whether that
// server appears to have restarted since the previous probe.
//
// Three things count as evidence, and none of them is a guess:
//
//   - The server stopped answering and is answering again. On 127.0.0.1 a
//     refused connection means nothing is listening; a later success means a
//     process is. That is a restart.
//   - A device that stayed in the same USB position came back with a LOWER
//     transport id. Within one server lifetime the id counter only rises, and
//     a device that re-plugs gets a HIGHER id, so a decrease can only mean the
//     counter started over.
//   - Two or more devices that stayed in their positions were all issued
//     DIFFERENT transport ids inside one probe interval. A restart re-mints
//     every id at once; nothing else makes several phones change id together
//     while none of them moved.
//
// What it deliberately does NOT do is compare the maximum id across probes:
// unplugging the device that happened to hold the highest id lowers that
// maximum without any restart, and a false epoch bump on every unplug would
// make the epoch meaningless. For the same reason ONE changed id is not
// evidence — that is what an ordinary unplug-and-replug looks like.
//
// The remaining blind spot is a restart that fits between two probes on a host
// with fewer than two devices attached. At agent start the unconditional bump
// covers it; after that, a host with one phone on it has one transport id to
// get wrong, and the cost is bounded by that.
func (a *Agent) probeADB(ctx context.Context) (bool, string) {
	cctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
	defer cancel()

	snap, err := a.adb.Devices(cctx)

	a.mu.Lock()
	defer a.mu.Unlock()

	if err != nil {
		if ctx.Err() != nil {
			return false, ""
		}
		if a.adbUp || !a.adbSeen {
			a.log.Warn("the local adb server is not answering; this agent keeps running "+
				"and every lease on this host keeps its device", "err", err)
		}
		a.adbSeen = true
		a.adbUp = false
		adbServerUp.Set(0)
		return false, ""
	}

	current := make(map[string]int64, len(snap.Devices))
	for _, d := range snap.Devices {
		if d.Devpath != "" {
			current[d.Devpath] = d.TransportID
		}
	}
	adbServerUp.Set(1)

	wasUp, seen := a.adbUp, a.adbSeen
	prev := a.lastTransports
	a.adbUp, a.adbSeen, a.lastTransports = true, true, current

	if !seen {
		// First successful probe of this agent's life. The startup bump has
		// already been decided; there is nothing to compare against.
		return false, ""
	}
	if !wasUp {
		return true, "the local adb server stopped answering and is answering again"
	}

	// A restart that fits entirely between two probes leaves no down window to
	// see, and its fresh counter may hand a device the same id or a higher one,
	// so a decrease is not the only evidence worth reading. What a restart
	// always does is re-mint EVERY id at once. Two or more devices that kept
	// their positions but were issued different transport ids inside one probe
	// interval is therefore a restart, not a coincidence: phones do not re-plug
	// themselves in pairs. A single changed id is left alone, because that is
	// exactly what one ordinary unplug-and-replug looks like and bumping on
	// every one of those would make the epoch meaningless.
	var changed []string
	for devpath, id := range current {
		old, ok := prev[devpath]
		if !ok || id == old {
			continue
		}
		if id < old {
			return true, fmt.Sprintf(
				"device at %s kept its position but its transport id fell from %d to %d, "+
					"which only happens when the adb server restarted its counter",
				devpath, old, id)
		}
		changed = append(changed, devpath)
	}
	if len(changed) >= 2 {
		slices.Sort(changed)
		return true, fmt.Sprintf(
			"%d devices kept their positions but were all issued new transport ids within "+
				"one probe interval (%s), which only happens when the adb server re-minted "+
				"its counter", len(changed), strings.Join(changed, ", "))
	}
	return false, ""
}

// heartbeatLoop writes farm.component_beat.
//
// The statement is issued directly rather than through internal/lease: that
// package's Store is the only door to acquiring, renewing and releasing
// leases, and a host agent holding one would put lease-ending calls in scope
// on the very process that also has root on the USB bus. One SQL statement is
// a smaller thing to own than that API.
func (a *Agent) heartbeatLoop(ctx context.Context) error {
	t := time.NewTicker(a.cfg.HeartbeatInterval)
	defer t.Stop()

	failing := false
	for {
		cctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
		_, err := a.cfg.Pool.Exec(cctx, `SELECT farm.component_beat($1::text)`, a.cfg.Component)
		cancel()

		switch {
		case err != nil && ctx.Err() == nil:
			beatFailures.Inc()
			if !failing {
				a.log.Warn("component heartbeat failed; this host agent is now invisible "+
					"to operators, though no lease depends on it", "err", err)
				failing = true
			}
		case err == nil && failing:
			a.log.Info("component heartbeat recovered")
			failing = false
		}

		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// kickDiscovery asks for an out-of-band discovery pass without blocking.
func (a *Agent) kickDiscovery() {
	select {
	case a.discoverNow <- struct{}{}:
	default:
	}
}

// discoverLoop runs topology discovery on a schedule, and immediately after an
// epoch bump.
func (a *Agent) discoverLoop(ctx context.Context) error {
	if a.cfg.Discover == nil {
		<-ctx.Done()
		return nil
	}
	// farm.slots references farm.hosts, so there is nothing to insert into
	// until the host row exists.
	select {
	case <-ctx.Done():
		return nil
	case <-a.registered:
	}

	t := time.NewTicker(a.cfg.DiscoverInterval)
	defer t.Stop()

	for {
		// One pass is bounded by the interval between passes: a pass that has
		// not finished by the time the next one is due is stuck, and the
		// alternative is a discovery loop that blocks forever inside a foreign
		// callback and never says so. The message names the knob, because a
		// farm with a genuinely slow USB tree wants a longer interval rather
		// than a truncated pass.
		dctx, dcancel := context.WithTimeout(ctx, a.cfg.DiscoverInterval)
		err := a.cfg.Discover(dctx)
		dcancel()

		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			discoveries.WithLabelValues("error").Inc()
			a.log.Warn("topology discovery pass failed; the last known topology stands. "+
				"If this reports a deadline, one pass is taking longer than "+
				"Config.DiscoverInterval and that interval needs raising",
				"err", err, "budget", a.cfg.DiscoverInterval)
		default:
			discoveries.WithLabelValues("ok").Inc()
		}

		select {
		case <-ctx.Done():
			return nil
		case <-a.discoverNow:
		case <-t.C:
		}
	}
}

// enrollLoop keeps enrollment running.
//
// Enrollment is a continuous job, not a pass: a device plugged in at 02:00 must
// be observed at 02:00. If it returns — with an error or without one — that is
// a fault, and the agent restarts it with backoff instead of quietly becoming
// an agent that no longer adopts anything.
func (a *Agent) enrollLoop(ctx context.Context) error {
	if a.cfg.Enroll == nil {
		<-ctx.Done()
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-a.registered:
	}

	backoff := DefaultEnrollBackoff
	for {
		started := time.Now()
		err := a.cfg.Enroll(ctx)
		if ctx.Err() != nil {
			return nil
		}
		enrollRestarts.Inc()
		// A run that lasted is not part of the same failure as the one before
		// it; see enrollHealthyRun.
		if time.Since(started) >= enrollHealthyRun {
			backoff = DefaultEnrollBackoff
		}
		if err != nil {
			a.log.Warn("enrollment stopped with an error; restarting it",
				"err", err, "in", backoff)
		} else {
			a.log.Warn("enrollment returned without an error before the agent was asked "+
				"to stop; restarting it", "in", backoff)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > DefaultEnrollBackoffMax {
			backoff = DefaultEnrollBackoffMax
		}
	}
}

// ---------------------------------------------------------------------------
// HostRunner
// ---------------------------------------------------------------------------

// USBReset performs recovery tier 3: USBDEVFS_RESET on the device node that
// backs devpath. The device keeps its lease, its fence and its job throughout;
// this re-enumerates a USB link, nothing more.
func (a *Agent) USBReset(ctx context.Context, hostID, devpath string) error {
	if err := a.checkHost(hostID, "USBDEVFS_RESET", devpath); err != nil {
		usbResets.WithLabelValues("refused").Inc()
		return err
	}
	a.log.Info("performing USBDEVFS_RESET", "devpath", devpath)

	err := platform.usbReset(ctx, devpath, a.ops)
	usbResets.WithLabelValues(outcomeLabel(err)).Inc()
	if err != nil {
		a.log.Warn("USBDEVFS_RESET did not repair the link", "devpath", devpath, "err", err)
	}
	return err
}

// PortPower performs recovery tier 4: cut VBUS to the port behind devpath,
// wait, and restore it.
//
// The caller has already checked lease policy for the device it named. This
// call authorises the disturbance of THAT device only: on a hub without
// per-port switching, where cutting one port cuts the whole power domain, the
// agent refuses rather than taking the rest of the domain down as a side
// effect. Use [Agent.PortPowerWithDomain] when the caller has genuinely
// checked policy for every device in the domain.
func (a *Agent) PortPower(ctx context.Context, hostID, devpath string) error {
	return a.PortPowerWithDomain(ctx, hostID, devpath, nil)
}

// PortPowerWithDomain is PortPower with an explicit list of other devpaths the
// caller has checked and is willing to disturb.
//
// The agent still checks: anything the power domain contains that is not the
// target and not in acknowledged is a device nobody authorised, and the cycle
// is refused. Being the last line is the point — the agent is the only party
// that can see what is actually plugged into the hub right now, and a blind
// executor would turn one operator's tier-4 decision into six broken jobs.
func (a *Agent) PortPowerWithDomain(ctx context.Context, hostID, devpath string, acknowledged []string) error {
	if err := a.checkHost(hostID, "VBUS power cycle", devpath); err != nil {
		portPowers.WithLabelValues("refused").Inc()
		return err
	}
	a.log.Info("cycling VBUS", "devpath", devpath,
		"acknowledged", acknowledged, "off_settle", a.cfg.PowerOffSettle)

	err := platform.portPower(ctx, devpath, acknowledged, a.ops)
	portPowers.WithLabelValues(outcomeLabel(err)).Inc()
	switch {
	case errors.Is(err, ErrRefused):
		a.log.Warn("VBUS cycle refused", "devpath", devpath, "err", err)
	case err != nil:
		a.log.Warn("VBUS cycle did not bring the device back", "devpath", devpath, "err", err)
	}
	return err
}

// opBudget is how long the agent will spend on ONE hardware operation once it
// has started, measured from its own configuration rather than from whoever
// asked. A VBUS cycle is the long one: a uhubctl status call, the off command,
// the settle, the on command with its retries, and the wait for the device to
// enumerate again.
//
// It exists so the node endpoint has a deadline that is a decision rather than
// a socket. See [Agent.opHandler].
func (a *Agent) opBudget() time.Duration {
	return a.cfg.CallTimeout + a.cfg.PowerOffSettle + a.cfg.PortReturnTimeout + DefaultOpGrace
}

// checkHost refuses a request addressed to a different host.
//
// A misrouted tier-4 request is not a harmless mistake: the devpath "usb:3-1.4"
// exists on every host in the fleet, and honouring it here would cut power to
// this rack's port 4 because some other rack's port 4 was sick.
//
// An empty hostID is accepted on the in-process seam, where there is no routing
// to get wrong — a [recovery.HostRunner] call reaches the one agent it was
// handed to. The HTTP surface does not accept it: a request that crossed a
// network could have been routed anywhere, so opHandler requires the caller to
// name the host it means before this check is even reached.
func (a *Agent) checkHost(hostID, what, devpath string) error {
	if hostID != "" && hostID != a.cfg.HostID {
		return fmt.Errorf("node: %w: %s for %s was addressed to host %q but this agent "+
			"runs on host %q, and that devpath names a different physical port here",
			ErrRefused, what, devpath, hostID, a.cfg.HostID)
	}
	return nil
}

// outcomeLabel folds an error into a metric label. An error that is neither a
// refusal nor an unsupported platform is a failure: we never claim success we
// cannot prove.
func outcomeLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrNotSupported):
		return "unsupported"
	default:
		return "failed"
	}
}

// ---------------------------------------------------------------------------
// The node endpoint
// ---------------------------------------------------------------------------

// opRequest is the body of both node operations.
type opRequest struct {
	HostID  string `json:"host_id"`
	Devpath string `json:"devpath"`
	// Acknowledged lists the other devpaths in the power domain the caller has
	// checked lease policy for. Port power only.
	Acknowledged []string `json:"acknowledged,omitempty"`
}

// Handler returns the node's HTTP surface: the two HostRunner operations,
// behind a bearer token.
func (a *Agent) Handler() (http.Handler, error) {
	if a.cfg.Token == "" {
		return nil, errors.New("node: a token is required before the node endpoint may " +
			"be served; these routes cut power to ports that hold live leases")
	}
	want := sha256.Sum256([]byte(a.cfg.Token))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /node/v1/usb-reset", a.opHandler(want[:],
		func(ctx context.Context, req opRequest) error {
			return a.USBReset(ctx, req.HostID, req.Devpath)
		}))
	mux.HandleFunc("POST /node/v1/port-power", a.opHandler(want[:],
		func(ctx context.Context, req opRequest) error {
			return a.PortPowerWithDomain(ctx, req.HostID, req.Devpath, req.Acknowledged)
		}))
	// Health carries the same token as the two operations. It reports whether
	// this host's adb server is answering and which epoch is in force, which is
	// precisely the reconnaissance an attacker wants before deciding which port
	// is worth cutting power to; a probe that can send a header can send this
	// one. See the deployment note in the package doc.
	mux.HandleFunc("GET /node/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r, want[:]) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorised"})
			return
		}
		a.mu.Lock()
		epoch, up := a.epoch, a.adbUp
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"host_id": a.cfg.HostID, "host_epoch": epoch,
			"adb_server_up": up, "platform": runtime.GOOS + "/" + runtime.GOARCH,
		})
	})
	return mux, nil
}

func (a *Agent) opHandler(want []byte, run func(context.Context, opRequest) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r, want) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorised"})
			return
		}
		var req opRequest
		// A node request is a few hundred bytes; anything larger is either a
		// mistake or an attempt to make this process the memory problem.
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if req.Devpath == "" {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": "devpath is required; positions, never serials"})
			return
		}
		// A request that crossed a network must name the host it means. The
		// in-process seam may leave this empty because it reaches exactly one
		// agent; an HTTP caller may not, because "usb:3-1.4" is a real port on
		// every host in the fleet and a misrouted request would cut power to
		// this rack because another rack was sick. See [Agent.checkHost].
		if req.HostID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "host_id is required on the node endpoint; send the farm.hosts.id " +
					"this request is meant for, because the same devpath names a different " +
					"physical port on every host"})
			return
		}

		// The hardware runs on a context DETACHED from the request.
		//
		// r.Context() dies when the client hangs up or its HTTP timeout fires,
		// and that is a socket event. Letting it abort a VBUS cycle mid-settle
		// would hand the recovery ladder an OutcomeFailed for a rung that was
		// never allowed to finish, and the ladder answers a failed rung by
		// escalating to a more destructive one — on a device that is very
		// probably three hours into somebody's job. The deadline that governs
		// this work is the agent's own budget, written down in opBudget, for
		// the same reason this server has no WriteTimeout.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), a.opBudget())
		defer cancel()

		if err := run(ctx, req); err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrRefused):
				// 409: the request was understood and deliberately not done.
				// It is not a 5xx, and a caller must not retry it unchanged.
				status = http.StatusConflict
			case errors.Is(err, ErrNotSupported):
				status = http.StatusNotImplemented
			}
			writeJSON(w, status, map[string]any{
				"error": err.Error(), "refused": errors.Is(err, ErrRefused)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// authorised compares the bearer token in constant time, over digests so that
// the comparison time does not depend on the token's length either.
func authorised(r *http.Request, want []byte) bool {
	raw := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(strings.TrimSpace(raw), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// serve runs the node endpoint until ctx ends.
func (a *Agent) serve(ctx context.Context) error {
	h, err := a.Handler()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return fmt.Errorf("node: listen on %s: %w", a.cfg.Addr, err)
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Deliberately no WriteTimeout. A VBUS cycle legitimately takes tens
		// of seconds, and a deadline that aborted the response while the
		// hardware work continued would put a socket back in charge of what is
		// true — the mistake this whole system is built against.
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	a.log.Info("node endpoint listening", "addr", ln.Addr().String())

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("node: serve: %w", err)
	case <-ctx.Done():
		// The grace period is the operation budget, not a small constant.
		// Shutdown returns as soon as nothing is in flight, so on an idle agent
		// this costs nothing; when a VBUS cycle IS in flight it is the
		// difference between the port getting its power back and a rack of
		// phones going dark until somebody walks to it. The detached restore in
		// portPower survives a cancelled context but not a dead process.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.opBudget())
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			a.log.Error("the node endpoint still had work in flight when its shutdown grace "+
				"ran out; if a VBUS cycle was in progress, check that no port was left "+
				"dark with `uhubctl -l <hub>` on this host",
				"grace", a.opBudget(), "err", err)
		}
		return nil
	}
}

func derefOr(p *string, alt string) string {
	if p == nil {
		return alt
	}
	return *p
}

// ---------------------------------------------------------------------------
// The platform seam
// ---------------------------------------------------------------------------

// opsConfig carries the tunables the platform half needs. It is a value, not a
// pointer to Config, so the Linux files cannot reach the database handle.
type opsConfig struct {
	uhubctl       string
	offSettle     time.Duration
	returnTimeout time.Duration
	log           *slog.Logger
}

// platformOps is the half of this agent that only exists on a real Linux
// device host.
type platformOps interface {
	// kernelRelease is uname -r. It goes into farm.hosts.kernel_release and
	// it decides whether a VBUS cycle can work at all.
	kernelRelease() (string, error)
	usbReset(ctx context.Context, devpath string, o opsConfig) error
	portPower(ctx context.Context, devpath string, acknowledged []string, o opsConfig) error
}

// platform is replaced by hostops.go's init on Linux. Everywhere else it stays
// this value and every hardware call answers with a refusal that names the
// platform, so the package builds and tests on a laptop without ever
// pretending it toggled anything.
var platform platformOps = unsupportedPlatform{}

type unsupportedPlatform struct{}

func (unsupportedPlatform) kernelRelease() (string, error) {
	return "", fmt.Errorf("node: %w: reading the kernel release is a Linux operation "+
		"and this agent is built for %s/%s", ErrNotSupported, runtime.GOOS, runtime.GOARCH)
}

func (unsupportedPlatform) usbReset(context.Context, string, opsConfig) error {
	return fmt.Errorf("node: %w: USBDEVFS_RESET is an ioctl on Linux's /dev/bus/usb "+
		"and this agent is built for %s/%s; recovery tier 3 cannot be performed here",
		ErrNotSupported, runtime.GOOS, runtime.GOARCH)
}

func (unsupportedPlatform) portPower(context.Context, string, []string, opsConfig) error {
	return fmt.Errorf("node: %w: VBUS control needs uhubctl and Linux sysfs, and this "+
		"agent is built for %s/%s; recovery tier 4 cannot be performed here",
		ErrNotSupported, runtime.GOOS, runtime.GOARCH)
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	hostRegistrations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "host_registrations_total",
		Help: "farm.hosts upserts by this agent, by outcome.",
	}, []string{"outcome"})

	epochBumps = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "host_epoch_bumps_total",
		Help: "Times this agent bumped farm.hosts.host_epoch because the local adb " +
			"server restarted, or because it could not prove otherwise at startup.",
	})

	adbServerUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "node", Name: "adb_server_up",
		Help: "1 when the last probe reached the local adb server. Observational: no " +
			"lease on this host depends on it.",
	})

	usbResets = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "usb_resets_total",
		Help: "Recovery tier 3 attempts on this host, by outcome. refused means the " +
			"agent declined; unsupported means this build cannot do it.",
	}, []string{"outcome"})

	portPowers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "port_power_cycles_total",
		Help: "Recovery tier 4 attempts on this host, by outcome. A sustained refused " +
			"rate means ganged hubs are being asked to cycle one port at a time.",
	}, []string{"outcome"})

	discoveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "discovery_passes_total",
		Help: "Topology discovery passes, by outcome.",
	}, []string{"outcome"})

	enrollRestarts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "enroll_restarts_total",
		Help: "Times the enrollment loop returned and was restarted. A rising count " +
			"means devices are being adopted late or not at all.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. This component must NOT be listed in " +
			"FARM_REAPER_COMPONENTS: a host agent's downtime may not move lease clocks.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
func Collectors() []prometheus.Collector {
	for _, outcome := range []string{"ok", "failed", "refused", "unsupported"} {
		usbResets.WithLabelValues(outcome)
		portPowers.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "error"} {
		hostRegistrations.WithLabelValues(outcome)
		discoveries.WithLabelValues(outcome)
	}
	return []prometheus.Collector{
		hostRegistrations, epochBumps, adbServerUp, usbResets, portPowers,
		discoveries, enrollRestarts, beatFailures,
	}
}
