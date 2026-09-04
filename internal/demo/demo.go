// Package demo runs the whole of device-farmer against simulated hardware, so
// that the system can be looked at, argued with and demonstrated on a laptop
// with zero phones attached.
//
// # What is fake, and what is emphatically not
//
// Exactly one thing is fake: the handsets and the ADB servers they are plugged
// into. Those are [github.com/flaviopadilha/device-farmer/test/fakeadb]
// servers, in-process, listening on real loopback sockets and speaking the
// real ADB host protocol — length-prefixed frames, OKAY/FAIL status, transport
// switches, host:track-devices-l streams, and TCP resets with SO_LINGER 0 so a
// severed connection arrives as ECONNRESET rather than a tidy EOF.
//
// Everything above the socket is the shipping implementation:
//
//   - the real [github.com/flaviopadilha/device-farmer/internal/adbwire]
//     client, dialing those sockets and classifying their failures;
//   - the real [github.com/flaviopadilha/device-farmer/internal/lease] store
//     and holder, calling the real farm.lease_* SQL functions in Postgres;
//   - the real allocation path (farm.lease_acquire), the real renewal loop,
//     the real suspect sweep, the real reclaim, the real max-runtime expiry,
//     the real control-plane gap refund.
//
// A demo that stubbed the logic would prove nothing. The whole point of the
// exercise is that the interesting behaviour is emergent: nothing in this
// package tells the lease to survive a transport failure. It survives because
// the lease clock and the device clock are separate all the way down.
//
// # What the simulation is trying to show you
//
// The centrepiece is one event, logged in block capitals when it happens: a
// device drops off the USB bus in the middle of a live lease and comes back.
// The ADB calls fail — real ECONNRESET, real "device offline" refusals, real
// typed transport errors, counted as blips. The lease does not move. Same
// lease id, same fence, no release_reason, heartbeats landing throughout.
//
// DeviceFarmer/STF issue #663, open and unanswered since 2023, is precisely
// this scenario ending the other way: a ~90-minute ECONNRESET releases the
// device mid-run and destroys multi-hour work. The runner checks the outcome
// against the database rather than asserting it in a log line, and shouts if
// the invariant is ever broken.
//
// Around that, the simulation also exercises:
//
//   - jobs arriving on a queue and being allocated by the real scheduler;
//   - a pod eviction: the holder is stopped WITHOUT releasing, and the
//     replacement re-acquires on the same job id and gets the same lease at
//     the same fence back;
//   - jobs.max_runtime elapsing — one of the only two automatic endings the
//     system permits, and the only one a user asked for;
//   - a flapping handset burning its flap credits until the damper marks it
//     degraded, and refilling them once it settles;
//   - a whole hub failing at once, which becomes ONE hub-scoped quarantine
//     rather than seven device alerts, and is then closed by a simulated
//     operator with an audit row.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// Baseline cadences, all at Speed 1. Every one of them is a LOCAL TIMER that
// decides when to ask a question; not one of them computes a deadline. Lease
// arithmetic is server-side against Postgres' now(), always.
//
// The ratios are what matter and they mirror production. The renewal interval
// is a small fraction of the TTL (5s against the schema's 10-minute floor, the
// same shape as 60s against 15 minutes), so roughly a hundred consecutive
// renewals would have to fail before a lease even became suspect — and suspect
// still releases nothing.
const (
	baseRenewInterval  = 5 * time.Second
	baseSchedulerTick  = 2 * time.Second
	baseFeederTick     = 4 * time.Second
	baseReaperTick     = 10 * time.Second
	baseStatsTick      = 5 * time.Second
	baseWorkStep       = 2 * time.Second
	baseOutage         = 25 * time.Second
	baseFlapHalf       = 3 * time.Second
	baseQuarantineHold = 45 * time.Second

	// healthLogSettle is how long a device's health chatter is folded into one
	// line, and blipLogEvery how many failed ADB calls on one job share a
	// line. Both bound the NARRATION only: every transition still reaches
	// farm.device_runtime and every blip still reaches the metric. They are
	// wall-clock and are deliberately not scaled by Speed — they exist for
	// the reader, who does not run faster when the simulation does.
	healthLogSettle = 10 * time.Second
	blipLogEvery    = 10

	// baseRearm is the post-release quarantine on a slot. In production this
	// MUST exceed the node proxy's self-fence timeout — that is what
	// guarantees the previous holder's sockets are severed before the device
	// is handed on. Here it is compressed so devices recirculate while someone
	// is watching.
	baseRearm = 4 * time.Second

	// demoTTL and demoGrace are the schema's minimums (jobs.ttl >= 10 min,
	// jobs.grace >= 5 min). They are floors on purpose: nothing a demo wants
	// to show is worth making a lease easier to lose.
	demoTTL   = 10 * time.Minute
	demoGrace = 5 * time.Minute

	// flapCreditCap and flapRefillPerSecond drive the damper. A device pays
	// one credit for every healthy -> not-healthy transition and earns them
	// back with quiet time, so a handset that blinks once is still healthy
	// while one that blinks constantly is degraded and stops being scheduled.
	// A raw counter would flip it between healthy and quarantined forever.
	flapCreditCap       = 10.0
	flapRefillPerSecond = 1.0 / 30.0
)

// Options configures the simulation.
type Options struct {
	// Hosts is how many simulated hosts to run, each with its own in-process
	// fake ADB server on its own loopback port. Default 2.
	Hosts int

	// Devices is how many handsets to place across those hosts. Default: one
	// per slot, which with the default topology is 56.
	Devices int

	// Speed multiplies the pace of every timer: 2 runs twice as fast, 0.5 at
	// half speed. It does not change any deadline the database owns — TTL and
	// grace stay at the schema's minimums — only how often this process acts.
	// Default 1.
	Speed float64

	// Logger receives the narration. Default slog.Default().
	Logger *slog.Logger

	// SkipSeed leaves the database exactly as it is. Use it to run the
	// simulation against a farm seeded earlier, or hand-edited.
	SkipSeed bool
}

func (o *Options) applyDefaults() {
	if o.Hosts <= 0 {
		o.Hosts = DefaultHosts
	}
	if o.Speed <= 0 {
		o.Speed = 1
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Run seeds a farm, stands up one fake ADB server per host, and drives the
// simulation until ctx is cancelled.
//
// Cancelling ctx stops the simulation the way SIGTERM stops farmd: in-flight
// work unwinds and LIVE LEASES ARE DELIBERATELY NOT RELEASED. A pod eviction
// is not evidence that the job on the phone died, so the lease, the device and
// the fence survive the process. Start the demo again and the scheduler
// re-attaches to those leases by job id, at the same fence — which is itself
// worth watching.
func Run(ctx context.Context, pool *pgxpool.Pool, opts Options) error {
	opts.applyDefaults()

	r := &Runner{
		pool:     pool,
		store:    lease.NewStore(pool),
		log:      opts.Logger,
		opts:     opts,
		rng:      rand.New(rand.NewPCG(0x5f3759df, uint64(time.Now().UnixNano()))),
		servers:  make(map[string]*fakeadb.Server),
		clients:  make(map[string]*adbwire.Client),
		byID:     make(map[string]*simDevice),
		byHost:   make(map[string][]*simDevice),
		live:     make(map[string]*jobRun),
		reserved: make(map[string]struct{}),
		health:   make(map[string]string),
		healthAt: make(map[string]time.Time),
	}
	return r.run(ctx)
}

// Runner owns the simulated farm for the lifetime of one Run.
type Runner struct {
	pool  *pgxpool.Pool
	store *lease.Store
	log   *slog.Logger
	opts  Options
	seed  SeedResult

	mu      sync.Mutex
	rng     *rand.Rand
	servers map[string]*fakeadb.Server
	clients map[string]*adbwire.Client
	devices []*simDevice
	byID    map[string]*simDevice
	byHost  map[string][]*simDevice
	live    map[string]*jobRun // by job id: the runs this process is holding
	// reserved holds job ids that are mid-handover. Without it the scheduler
	// sees a RUNNING job with a live lease and no local holder during an
	// eviction, re-acquires it, and the two holders in this one process then
	// fence each other — a race invented by the demo, not by the system.
	reserved map[string]struct{}
	health   map[string]string    // device id -> last health we logged
	healthAt map[string]time.Time // device id -> when we last logged it
	stopFlap []func()

	wg sync.WaitGroup

	// Counters for the closing summary. leasesLostToTransport is the one that
	// matters: it must be zero, and the runner proves it against the database
	// rather than asserting it.
	jobsStarted           atomic.Int64
	jobsCompleted         atomic.Int64
	renewals              atomic.Int64
	adbBlips              atomic.Int64
	outages               atomic.Int64
	reattaches            atomic.Int64
	maxRuntimeEndings     atomic.Int64
	reclaimed             atomic.Int64
	leasesLostToTransport atomic.Int64
}

// simDevice is one simulated handset: the database row and the fake hardware
// standing in for it, joined by the one thing that addresses a physical
// position — the devpath.
type simDevice struct {
	deviceID string
	hostID   string
	slotID   int64
	hubID    int64
	hubPath  string
	rackSlot string
	devpath  string
	serial   string
	model    string
	product  string
	codename string
	initial  string // seeded adb_state
}

func (d *simDevice) slot() obs.Slot {
	return obs.Slot{Host: d.hostID, Hub: d.hubPath, RackSlot: d.rackSlot}
}

// jobRun is one job this process is currently holding a lease for.
type jobRun struct {
	job    jobRow
	dev    *simDevice
	holder *lease.Holder
	lease  lease.Lease

	renewals atomic.Int64
	blips    atomic.Int64
	step     atomic.Int64
}

type jobRow struct {
	id    string
	state string
	steps int
}

// ---------------------------------------------------------------------
// Bring-up
// ---------------------------------------------------------------------

func (r *Runner) run(ctx context.Context) error {
	if !r.opts.SkipSeed {
		res, err := Seed(ctx, r.pool, SeedOptions{Hosts: r.opts.Hosts, Devices: r.opts.Devices})
		if err != nil {
			return err
		}
		r.seed = res
	} else if err := r.loadSeedShape(ctx); err != nil {
		return err
	}

	r.banner()

	if err := r.loadFleet(ctx); err != nil {
		return err
	}
	if len(r.devices) == 0 {
		return errors.New("demo: no devices in the fleet; seed the database first")
	}
	if err := r.startHosts(ctx); err != nil {
		r.closeServers()
		return err
	}
	defer r.closeServers()

	// Arm the reaper BEFORE beating, never after. reaper_arm computes the
	// control-plane gap from the OLDEST heartbeat across every component on
	// the renewal path and refunds it to every live lease; a beat written
	// first would erase the evidence of our own downtime and charge the
	// tenant for it. On a second run of the demo this is where the previous
	// run's downtime is given back.
	gap, err := r.store.ReaperArm(ctx, lease.ReaperComponents, 0)
	if err != nil {
		return fmt.Errorf("demo: arm reaper: %w", err)
	}
	if gap > 0 {
		obs.ControlPlaneGap(obs.ComponentReaper, gap)
		r.log.Warn("control-plane gap refunded to every live lease",
			"gap", gap.Round(time.Second),
			"note", "our downtime costs the tenant zero lease budget")
	}

	loops := []struct {
		name string
		fn   func(context.Context)
	}{
		{"scheduler", r.schedulerLoop},
		{"feeder", r.feederLoop},
		{"reaper", r.reaperLoop},
		{"stats", r.statsLoop},
		{"chaos", r.chaosLoop},
	}
	for _, l := range loops {
		r.wg.Add(1)
		go func(fn func(context.Context)) {
			defer r.wg.Done()
			fn(ctx)
		}(l.fn)
	}
	for hostID, cli := range r.clients {
		r.wg.Add(1)
		go func(hostID string, cli *adbwire.Client) {
			defer r.wg.Done()
			r.watchHost(ctx, hostID, cli)
		}(hostID, cli)
	}

	<-ctx.Done()
	r.log.Info("demo: shutting down; live leases are deliberately NOT released — " +
		"the replacement re-attaches at the same fence")
	r.stopFlapping()
	r.wg.Wait()
	r.summary()
	return nil
}

// loadSeedShape fills in the banner details when the caller asked us not to
// seed, so the summary still names real hosts.
func (r *Runner) loadSeedShape(ctx context.Context) error {
	const q = `
SELECT COALESCE(array_agg(DISTINCT h.id ORDER BY h.id), '{}'::text[]), count(DISTINCT d.id)
  FROM farm.hosts h LEFT JOIN farm.devices d ON d.host_id = h.id`
	var hosts []string
	var devices int
	if err := r.pool.QueryRow(ctx, q).Scan(&hosts, &devices); err != nil {
		return fmt.Errorf("demo: read existing farm: %w", err)
	}
	r.seed = SeedResult{Hosts: hosts, Devices: devices, Pool: DefaultPool,
		Tenant: DefaultTenant, Queue: DefaultQueue}
	return nil
}

func (r *Runner) banner() {
	bar := strings.Repeat("=", 74)
	r.log.Info(bar)
	r.log.Info("device-farmer DEMO — THE HARDWARE IS SIMULATED, NOTHING ELSE IS")
	r.log.Info(bar)
	r.log.Info("no phones are attached: each host below is an in-process fake ADB server " +
		"on a real loopback socket, speaking the real ADB host protocol")
	r.log.Info("everything above the socket is the shipping code: the real adbwire client, " +
		"the real lease store, the real farm.lease_* functions, the real loops")
	r.log.Info("watch for: DEVICE DROPPED OFFLINE MID-LEASE. The ADB calls fail; the lease " +
		"does not move. STF #663 is that same event ending the other way")
	r.log.Info("farm seeded",
		"hosts", r.seed.Hosts, "devices", r.seed.Devices, "slots", r.seed.Slots,
		"pool", r.seed.Pool, "tenant", r.seed.Tenant, "queue", r.seed.Queue,
		"speed", r.opts.Speed)
	if len(r.seed.ClonePositions) == 2 {
		r.log.Info("planted a duplicate-serial pair — every addressed call carries a devpath",
			"serial", CloneSerial, "positions", r.seed.ClonePositions)
	}
	if r.seed.FaultyHub != "" {
		r.log.Info("planted a hub with correlated failures for the blast-radius banner",
			"host", r.seed.FaultyHost, "hub", r.seed.FaultyHub,
			"unhealthy", r.seed.FaultyHubUnhealthy, "of", r.seed.FaultyHubDevices)
	}
	r.log.Info(bar)
}

// loadFleet reads the seeded farm back out of the database. The database is
// the source of truth for what hardware exists, even for hardware that does
// not exist.
func (r *Runner) loadFleet(ctx context.Context) error {
	const q = `
SELECT d.id::text, d.host_id, s.id, hb.id, hb.usb_path, COALESCE(s.rack_slot,''),
       s.adb_devpath, COALESCE(d.adb_serial,''), COALESCE(d.model,''),
       COALESCE(d.product,''), COALESCE(d.device_codename,''), rt.adb_state
  FROM farm.devices d
  JOIN farm.slots s          ON s.id = d.current_slot_id
  JOIN farm.hubs hb          ON hb.id = s.hub_id
  JOIN farm.device_runtime rt ON rt.device_id = d.id
 WHERE d.host_id = ANY($1::text[])
 ORDER BY d.host_id, s.usb_path`

	rows, err := r.pool.Query(ctx, q, r.seed.Hosts)
	if err != nil {
		return fmt.Errorf("demo: load fleet: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		d := &simDevice{}
		if err := rows.Scan(&d.deviceID, &d.hostID, &d.slotID, &d.hubID, &d.hubPath,
			&d.rackSlot, &d.devpath, &d.serial, &d.model, &d.product, &d.codename,
			&d.initial); err != nil {
			return fmt.Errorf("demo: scan fleet: %w", err)
		}
		r.devices = append(r.devices, d)
		r.byID[d.deviceID] = d
		r.byHost[d.hostID] = append(r.byHost[d.hostID], d)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("demo: load fleet: %w", err)
	}
	return nil
}

// startHosts brings up one fake ADB server per host, points the host row at
// its real listen address, and proves the wire works with a real host:version
// round trip before anything depends on it.
func (r *Runner) startHosts(ctx context.Context) error {
	for _, hostID := range r.seed.Hosts {
		devs := r.byHost[hostID]
		if len(devs) == 0 {
			continue
		}
		srv, err := fakeadb.New()
		if err != nil {
			return fmt.Errorf("demo: start fake adb for %s: %w", hostID, err)
		}
		installShellScripts(srv)
		for _, d := range devs {
			srv.Add(fakeadb.Device{
				Serial:   d.serial,
				Devpath:  d.devpath,
				Model:    d.model,
				Product:  d.product,
				Codename: d.codename,
				State:    wireState(d.initial),
			})
		}
		r.servers[hostID] = srv

		// host_epoch increments on every adb server restart: a transport_id is
		// meaningless without the epoch that minted it, because adb reuses
		// small integers.
		const q = `
UPDATE farm.hosts
   SET adb_endpoint = $2, host_epoch = host_epoch + 1, last_seen_at = now(),
       admin_state = CASE WHEN admin_state = 'disabled' THEN admin_state ELSE 'enabled' END
 WHERE id = $1
RETURNING host_epoch`
		var epoch int64
		if err := r.pool.QueryRow(ctx, q, hostID, srv.Addr()).Scan(&epoch); err != nil {
			return fmt.Errorf("demo: register host %s: %w", hostID, err)
		}

		cli := adbwire.New(srv.Addr(),
			adbwire.WithLogger(r.log.With("component", "adbwire", "host", hostID)),
			adbwire.WithCallTimeout(5*time.Second))
		version, err := cli.Version(ctx)
		if err != nil {
			return fmt.Errorf("demo: handshake with fake adb for %s: %w", hostID, err)
		}
		r.clients[hostID] = cli

		r.log.Info("simulated host up",
			"host", hostID, "endpoint", srv.Addr(), "devices", len(devs),
			"adb_protocol_version", version, "host_epoch", epoch)
	}

	// Devices seeded as flaky are made flaky in the hardware too, so the
	// watchdog reaches "degraded" on its own evidence instead of being told.
	r.startSeededFlappers()
	return nil
}

func (r *Runner) startSeededFlappers() {
	for _, pos := range r.seed.DegradedPositions {
		d := r.deviceByRackSlot(pos)
		if d == nil {
			continue
		}
		if srv := r.servers[d.hostID]; srv != nil {
			stop := srv.Flap(d.devpath, r.scale(baseFlapHalf))
			r.mu.Lock()
			r.stopFlap = append(r.stopFlap, stop)
			r.mu.Unlock()
			r.log.Info("seeded flaky handset is flapping in the simulated hardware",
				"rack_slot", d.rackSlot, "devpath", d.devpath,
				"note", "the damper will settle it at degraded; no lease is involved")
		}
	}
}

func (r *Runner) closeServers() {
	r.stopFlapping()
	for id, srv := range r.servers {
		if err := srv.Close(); err != nil {
			r.log.Warn("demo: closing fake adb server", "host", id, "err", err)
		}
	}
}

func (r *Runner) stopFlapping() {
	r.mu.Lock()
	stops := r.stopFlap
	r.stopFlap = nil
	r.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

// ---------------------------------------------------------------------
// Device health: the third clock. It touches leases exactly never.
// ---------------------------------------------------------------------

// watchHost consumes the real host:track-devices-l stream and writes what it
// sees into farm.device_runtime.
//
// Note what a dropped stream does here: nothing. adbwire's tracker reconnects
// with jittered backoff and emits no snapshot for a failed session, so a dead
// socket never becomes an empty device list. Losing the connection is evidence
// about the connection.
func (r *Runner) watchHost(ctx context.Context, hostID string, cli *adbwire.Client) {
	tracker := cli.TrackDevices(ctx)
	defer tracker.Close()

	// A slow periodic re-listing backstops the stream: fakeadb pushes on every
	// mutation, but a real adb server can go silent for hours and a demo that
	// only ever reacted to pushes would look frozen after its last event.
	tick := time.NewTicker(r.scale(baseStatsTick))
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case snap, ok := <-tracker.Snapshots():
			if !ok {
				return
			}
			r.applySnapshot(ctx, hostID, snap)
		case <-tick.C:
			snap, err := cli.Devices(ctx)
			if err != nil {
				// The host is unreachable. That is a fact about the host, and
				// it is recorded as one: no device is marked absent on the
				// strength of our own socket dying.
				if !errors.Is(err, context.Canceled) {
					r.log.Debug("demo: periodic listing failed; device state left as last observed",
						"host", hostID, "err", err)
				}
				continue
			}
			r.applySnapshot(ctx, hostID, snap)
		}
	}
}

func (r *Runner) applySnapshot(ctx context.Context, hostID string, snap adbwire.Snapshot) {
	devpaths := make([]string, 0, len(snap.Devices))
	states := make([]string, 0, len(snap.Devices))
	tids := make([]int64, 0, len(snap.Devices))
	for _, d := range snap.Devices {
		if d.Devpath == "" {
			continue // an emulator or a TCP device: no position, nothing to reconcile
		}
		devpaths = append(devpaths, d.Devpath)
		states = append(states, string(d.State))
		tids = append(tids, d.TransportID)
	}

	// The observation half. flap_credits is a token bucket recomputed
	// server-side from flap_updated_at, so no client timestamp is involved.
	const observed = `
WITH obs AS (
  SELECT * FROM unnest($2::text[], $3::text[], $4::bigint[]) AS t(devpath, adb_state, transport_id)
), tgt AS (
  SELECT d.id AS device_id, o.adb_state, o.transport_id
    FROM obs o
    JOIN farm.slots s   ON s.host_id = $1 AND s.adb_devpath = o.devpath
    JOIN farm.devices d ON d.current_slot_id = s.id
), calc AS (
  SELECT t.device_id, t.adb_state, t.transport_id, r.health AS old_health,
         GREATEST(0::float8, LEAST($5::float8,
           r.flap_credits::float8
             + EXTRACT(EPOCH FROM (now() - r.flap_updated_at))::float8 * $6::float8
             - CASE WHEN t.adb_state <> 'device' AND r.adb_state = 'device' THEN 1 ELSE 0 END
         )) AS credits,
         CASE WHEN t.adb_state = 'device' THEN r.consec_good + 1 ELSE 0 END AS good,
         CASE WHEN t.adb_state = 'device' THEN 0 ELSE r.consec_bad + 1 END AS bad
    FROM tgt t
    JOIN farm.device_runtime r ON r.device_id = t.device_id
   -- Both of these are HUMAN verdicts and a probe may not overturn either: a
   -- quarantine is closed by a person, and a retired device stays retired
   -- however cheerfully it answers.
   WHERE r.health NOT IN ('quarantined','retired')
), verdict AS (
  SELECT c.*, CASE
           WHEN c.adb_state = 'device' AND c.credits < 1 THEN 'degraded'
           WHEN c.adb_state = 'device' THEN 'healthy'
           WHEN c.adb_state = 'offline' THEN 'offline'
           WHEN c.adb_state = 'unauthorized' THEN 'unauthorized'
           WHEN c.adb_state = 'absent' THEN 'missing'
           WHEN c.adb_state IN ('bootloader','recovery','sideload','rescue') THEN 'recovering'
           ELSE 'degraded'
         END AS health
    FROM calc c
)
UPDATE farm.device_runtime r
   SET adb_state       = v.adb_state,
       transport_id    = v.transport_id,
       host_epoch      = (SELECT h.host_epoch FROM farm.hosts h WHERE h.id = $1),
       consec_good     = v.good,
       consec_bad      = v.bad,
       flap_credits    = v.credits,
       flap_updated_at = now(),
       health          = v.health,
       health_since    = CASE WHEN v.health <> r.health THEN now() ELSE r.health_since END,
       last_seen_at    = now(),
       updated_at      = now()
  FROM verdict v
 WHERE r.device_id = v.device_id
   AND (r.adb_state <> v.adb_state
        OR r.health <> v.health
        OR r.last_seen_at IS NULL
        OR r.last_seen_at < now() - interval '10 seconds')
RETURNING r.device_id::text, r.adb_state, r.health, v.old_health, r.flap_credits::float8`

	rows, err := r.pool.Query(ctx, observed, hostID, devpaths, states, tids,
		flapCreditCap, flapRefillPerSecond*r.opts.Speed)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: writing device health failed", "host", hostID, "err", err)
		}
		return
	}
	type change struct {
		id, state, health, old string
		credits                float64
	}
	var changes []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.id, &c.state, &c.health, &c.old, &c.credits); err != nil {
			rows.Close()
			r.log.Warn("demo: scanning health update", "host", hostID, "err", err)
			return
		}
		changes = append(changes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		r.log.Warn("demo: writing device health failed", "host", hostID, "err", err)
	}
	for _, c := range changes {
		r.noteHealth(c.id, c.old, c.health, c.state, c.credits)
	}

	// The complement: rows for this host that the server did not list at all.
	// Safe only because an ADB snapshot is whole-state — the server re-sends
	// the entire list on every change — and because a failed session produced
	// no snapshot to get here.
	const missing = `
UPDATE farm.device_runtime r
   SET adb_state    = 'absent',
       health       = 'missing',
       health_since = CASE WHEN r.health <> 'missing' THEN now() ELSE r.health_since END,
       consec_good  = 0,
       consec_bad   = r.consec_bad + 1,
       updated_at   = now()
  FROM farm.devices d
  LEFT JOIN farm.slots s ON s.id = d.current_slot_id
 WHERE r.device_id = d.id
   AND d.host_id = $1
   AND r.health NOT IN ('quarantined','retired')
   AND r.adb_state <> 'absent'
   AND (s.adb_devpath IS NULL OR NOT (s.adb_devpath = ANY($2::text[])))
RETURNING r.device_id::text, r.health`

	mrows, err := r.pool.Query(ctx, missing, hostID, devpaths)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: marking absent devices", "host", hostID, "err", err)
		}
		return
	}
	type absence struct{ id, health string }
	var absences []absence
	for mrows.Next() {
		var a absence
		if err := mrows.Scan(&a.id, &a.health); err != nil {
			mrows.Close()
			r.log.Warn("demo: scanning absent devices", "host", hostID, "err", err)
			return
		}
		absences = append(absences, a)
	}
	mrows.Close()
	// Checked, not assumed. An UPDATE ... RETURNING that dies halfway through
	// streaming reports it here and nowhere else, and swallowing it would mean
	// the fleet grid quietly disagreed with the hardware.
	if err := mrows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		r.log.Warn("demo: marking absent devices", "host", hostID, "err", err)
	}
	for _, a := range absences {
		// Negative credits mean "not asked": this statement reconciles absence
		// and has no opinion about the flap bucket.
		r.noteHealth(a.id, "", a.health, "absent", -1)
	}
}

// noteHealth logs a health transition, and says out loud what such a line does
// NOT mean.
//
// It is deliberately damped. A flapping handset changes state several times a
// second, and a line per transition would bury the one event this demo exists
// to show under the exact noise the flap damper was built to absorb. A return
// to healthy is always worth a line; anything else repeats at most once per
// settle window per device.
func (r *Runner) noteHealth(deviceID, old, health, state string, credits float64) {
	now := time.Now()

	// While a device's flap bucket is empty, every transition it makes is the
	// same story — "this handset is not trustworthy" — so they collapse into
	// one line until it either settles or the bucket refills. Deduping on the
	// raw health would never collapse anything: a flapping device alternates
	// degraded and offline forever.
	key := health
	if credits >= 0 && credits < 1 {
		key = "damped"
	}

	r.mu.Lock()
	prev, seen := r.health[deviceID]
	last := r.healthAt[deviceID]
	dev := r.byID[deviceID]
	quiet := seen && prev == key
	if !quiet && health != "healthy" && !last.IsZero() && now.Sub(last) < healthLogSettle {
		quiet = true
	}
	if !quiet {
		// Recorded only when it is actually narrated. Marking a suppressed
		// state as seen would swallow it permanently: a device drains its
		// bucket inside one settle window, and the "damper engaged" line —
		// the one worth reading — would never be printed at all.
		r.healthAt[deviceID] = now
		r.health[deviceID] = key
	}
	r.mu.Unlock()
	if quiet || dev == nil {
		return
	}
	bucket := "n/a"
	if credits >= 0 {
		bucket = fmt.Sprintf("%.1f", credits)
	}
	attrs := []any{
		"rack_slot", dev.rackSlot, "devpath", dev.devpath,
		"adb_state", state, "health", health, "flap_credits", bucket,
	}
	switch {
	case key == "damped":
		r.log.Warn("flap damper engaged: handset keeps blinking, so it is marked degraded "+
			"and stops being scheduled — no lease is touched by this", attrs...)
	case health == "healthy" && (old == "degraded" || prev == "damped"):
		r.log.Info("flap damper released: credits refilled after quiet time", attrs...)
	default:
		r.log.Info("device health changed (health clock only; leases are unaffected)", attrs...)
	}
}

// ---------------------------------------------------------------------
// Work: jobs arriving, being allocated, and running on real ADB sockets
// ---------------------------------------------------------------------

// shellStep is one command the simulated test harness runs on a device. They
// are real service strings on the real shell v2 protocol; the fake answers
// with properly framed stdout and an exit packet.
type shellStep struct {
	cmd string
	out string
}

var shellSteps = []shellStep{
	{"getprop ro.build.version.release", "14\n"},
	{"dumpsys battery | grep level", "  level: 87\n"},
	{"pm list packages -3", "package:com.acme.app\npackage:com.acme.app.test\n"},
	{"am instrument -w -e class com.acme.SmokeTest com.acme.app.test/androidx.test.runner.AndroidJUnitRunner",
		"INSTRUMENTATION_STATUS_CODE: 0\nTests run: 24, Failures: 0\nOK (24 tests)\n"},
	{"logcat -d -t 5", "01-01 00:00:00.000  1000  1000 I acme: step complete\n"},
}

// installShellScripts teaches one fake server to answer the harness. The
// payloads are genuine shell v2 frames built with the shipping encoder, so
// adbwire's demultiplexer is exercised rather than bypassed.
func installShellScripts(srv *fakeadb.Server) {
	// Registered first so the specific commands below win: fakeadb consults
	// scripts newest-first.
	srv.Respond("", "shell", shellV2("\n", 0))
	for _, s := range shellSteps {
		srv.Respond("", adbwire.ShellService(s.cmd), shellV2(s.out, 0))
	}
}

func shellV2(stdout string, exit byte) string {
	var b bytes.Buffer
	// Errors are impossible: bytes.Buffer never fails a write and the payloads
	// are far below MaxShellPacket.
	_ = adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout))
	_ = adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{exit})
	return b.String()
}

// feederLoop submits work. It keeps a shallow queue rather than a deep one, so
// that "no capacity" stays an occasional, visible outcome instead of the
// permanent state of the farm.
func (r *Runner) feederLoop(ctx context.Context) {
	tick := time.NewTicker(r.scale(baseFeederTick))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		var queued int
		if err := r.pool.QueryRow(ctx,
			`SELECT count(*) FROM farm.jobs WHERE state = 'queued' AND pool_id = $1`,
			r.poolID()).Scan(&queued); err != nil {
			if !errors.Is(err, context.Canceled) {
				r.log.Warn("demo: counting queued jobs", "err", err)
			}
			continue
		}
		if queued >= 5 || r.liveCount() >= r.maxConcurrentJobs() {
			continue
		}
		if err := r.submitJob(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: submitting job", "err", err)
		}
	}
}

func (r *Runner) submitJob(ctx context.Context) error {
	steps := 6 + r.randN(9)
	kind := "smoke"
	expected := time.Duration(steps) * r.scale(baseWorkStep)
	policy := []string{"allow_port_power_cycle", "allow_soft_reset", "no_disruption"}[r.randN(3)]
	var maxRuntime any

	switch n := r.randN(8); {
	case n == 0:
		// A soak test: six expected hours means farm.lease_acquire marks the
		// lease protected, and a protected lease is never auto-reclaimed —
		// the reaper holds it and pages a human instead.
		//
		// The step count is large but finite. A soak that never finished
		// would hold its device across every future run of the demo — the
		// leases outlive the process on purpose — and the farm would slowly
		// fill with immortal jobs.
		kind, steps, expected = "soak", 2000, 6*time.Hour
		policy = "no_disruption"
	case n == 1:
		// The one automatic ending a USER asked for. It is set to half the
		// work this job intends to do, so it actually elapses mid-run: a
		// max_runtime nobody ever reaches demonstrates nothing. When it
		// fires, farm.lease_expire_max_runtime ends the lease and the holder
		// learns it the honest way — its next renewal returns zero rows.
		limit := expected / 2
		if floor := r.scale(3 * time.Second); limit < floor {
			limit = floor
		}
		maxRuntime = iv(limit)
	}

	// A REAL jobspec document, not a hand-rolled shape. internal/runner
	// executes exactly this, so a spec the demo cannot express is a spec the
	// product cannot run — which is the point of generating it here rather
	// than stubbing the execution.
	//
	// The steps are deliberately cheap shell probes: the simulated hardware
	// answers them, and the demo is about the LEASE surviving a partition, not
	// about what the phone computed.
	js := jobspec.New()
	js.DefaultTimeout = jobspec.Duration(30 * time.Second)

	// A soak is hours of work, and expressing it as thousands of ADB round
	// trips is the wrong shape: every one of them is a socket that can die.
	// The right shape — the one that makes a six-hour job survive a ten-minute
	// partition — is to start the work DETACHED on the device and poll a
	// result file, so no socket is the source of truth for anything.
	if kind == "soak" {
		const result = "/data/local/tmp/.farm/soak.result"
		js.Steps = []jobspec.Step{
			{ID: "soak/start", Payload: jobspec.ShellDetached{
				Command:    fmt.Sprintf("for i in $(seq 1 %d); do getprop sys.boot_completed >/dev/null; sleep 1; done", steps),
				ResultPath: result,
				Handle:     "soak",
			}},
			{ID: "soak/await", Timeout: jobspec.Duration(6 * time.Hour),
				Payload: jobspec.WaitFor{
					Probe:    "test -f " + result,
					Interval: jobspec.Duration(r.scale(5 * time.Second)),
					Timeout:  jobspec.Duration(6 * time.Hour),
				}},
			{ID: "soak/collect", Payload: jobspec.Shell{Command: "cat " + result}},
		}
		return r.insertJob(ctx, js, kind, steps, expected, maxRuntime, policy)
	}

	for i := 0; i < steps; i++ {
		id := fmt.Sprintf("%s/%03d", kind, i)
		switch i % 4 {
		case 0:
			js.Steps = append(js.Steps, jobspec.Step{
				ID:      id,
				Payload: jobspec.Shell{Command: "getprop sys.boot_completed"},
			})
		case 1:
			js.Steps = append(js.Steps, jobspec.Step{
				ID:      id,
				Payload: jobspec.Shell{Command: "dumpsys battery | head -20"},
			})
		case 2:
			js.Steps = append(js.Steps, jobspec.Step{
				ID: id,
				Payload: jobspec.WaitFor{
					Probe:    "getprop sys.boot_completed",
					Interval: jobspec.Duration(time.Second),
					Timeout:  jobspec.Duration(15 * time.Second),
				},
			})
		default:
			js.Steps = append(js.Steps, jobspec.Step{
				ID:      id,
				Payload: jobspec.Sleep{Duration: jobspec.Duration(r.scale(2 * time.Second))},
			})
		}
	}

	return r.insertJob(ctx, js, kind, steps, expected, maxRuntime, policy)
}

// insertJob validates and queues one generated spec.
//
// Validation happens here rather than at the call sites so that no path can
// queue a job the runner will refuse: a demo that does that teaches the reader
// the runner is broken when the fault is in the document.
func (r *Runner) insertJob(ctx context.Context, js jobspec.Spec, kind string, steps int,
	expected time.Duration, maxRuntime any, policy string) error {

	if verr := jobspec.Validate(js); verr != nil {
		return fmt.Errorf("demo: generated an invalid job spec: %w", verr)
	}

	spec, err := json.Marshal(js)
	if err != nil {
		return fmt.Errorf("demo: encode job spec: %w", err)
	}

	const q = `
INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, spec, expected_duration, max_runtime,
                       ttl, grace, disruption_policy, created_by)
VALUES ($1,$2,$3,$4::jsonb,$5::interval,$6::interval,$7::interval,$8::interval,$9,$10)
RETURNING id::text`
	var id string
	if err := r.pool.QueryRow(ctx, q,
		DefaultTenant, DefaultQueue, r.poolID(), string(spec),
		iv(expected), maxRuntime, iv(demoTTL), iv(demoGrace), policy, "demo-feeder",
	).Scan(&id); err != nil {
		return fmt.Errorf("demo: insert job: %w", err)
	}
	r.log.Info("job queued", "job_id", short(id), "suite", kind, "steps", len(js.Steps),
		"work_units", steps, "disruption_policy", policy, "max_runtime", maxRuntime)
	return nil
}

// schedulerLoop is the allocation path. It calls farm.lease_acquire and
// nothing else decides who gets a device.
//
// It also picks up jobs left RUNNING with a live lease by a previous process —
// a demo that was Ctrl-C'd, or a pod that was evicted. Those re-attach at the
// same fence, which is Phase 1 of lease_acquire doing its job.
func (r *Runner) schedulerLoop(ctx context.Context) {
	tick := time.NewTicker(r.scale(baseSchedulerTick))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		jobs, err := r.schedulableJobs(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				r.log.Warn("demo: reading schedulable jobs", "err", err)
			}
			continue
		}
		for _, job := range jobs {
			if ctx.Err() != nil {
				return
			}
			if r.liveCount() >= r.maxConcurrentJobs() {
				break
			}
			r.tryAcquire(ctx, job)
		}
	}
}

func (r *Runner) schedulableJobs(ctx context.Context) ([]jobRow, error) {
	const q = `
-- steps is a jobspec array, not a count. It was an integer in the demo's
-- own ad-hoc shape before internal/jobspec existed; reading it as one
-- now fails every scheduling poll with SQLSTATE 22P02.
SELECT j.id::text, j.state,
       COALESCE(jsonb_array_length(j.spec->'steps'), 8)
  FROM farm.jobs j
 WHERE j.pool_id = $1
   AND (j.state = 'queued'
        OR (j.state = 'running'
            AND EXISTS (SELECT 1 FROM farm.leases l
                         WHERE l.job_id = j.id AND l.state IN ('held','suspect'))))
 ORDER BY j.state DESC, j.created_at
 LIMIT 20`
	rows, err := r.pool.Query(ctx, q, r.poolID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.id, &j.state, &j.steps); err != nil {
			return nil, err
		}
		if r.isLive(j.id) {
			continue // this process is already holding it
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *Runner) tryAcquire(ctx context.Context, job jobRow) {
	instance, err := lease.NewHolderInstance()
	if err != nil {
		r.log.Error("demo: mint holder instance", "err", err)
		return
	}
	holderName := "runner-pod-" + short(job.id)

	res, err := r.store.Acquire(ctx, job.id, holderName, instance)
	switch {
	case errors.Is(err, lease.ErrNoCapacity):
		// An ordinary scheduling outcome, not a failure: every healthy device
		// in the pool is busy, degraded, or inside a slot's rearm window.
		return
	case err != nil:
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: acquire failed", "job_id", short(job.id), "err", err)
		}
		return
	}

	dev := r.device(res.Lease.DeviceID)
	if dev == nil {
		r.log.Warn("demo: acquired a device this runner does not simulate",
			"job_id", short(job.id), "device_id", short(res.Lease.DeviceID))
		return
	}
	if res.Reattached {
		r.reattaches.Add(1)
		r.log.Info("RE-ATTACHED to an existing lease at the SAME FENCE — a previous "+
			"holder went away and cost this job nothing",
			"job_id", short(job.id), "lease_id", short(res.Lease.ID),
			"fence", res.Lease.Fence, "rack_slot", dev.rackSlot)
	} else {
		r.jobsStarted.Add(1)
		r.log.Info("lease acquired",
			"job_id", short(job.id), "lease_id", short(res.Lease.ID),
			"fence", res.Lease.Fence, "rack_slot", dev.rackSlot,
			"model", dev.model, "devpath", dev.devpath)
	}

	if _, err := r.pool.Exec(ctx,
		`UPDATE farm.jobs SET state='running', started_at=COALESCE(started_at, now())
          WHERE id = $1 AND state IN ('queued','allocating')`, job.id); err != nil {
		r.log.Warn("demo: marking job running", "job_id", short(job.id), "err", err)
	}
	r.event(ctx, "lease_acquired", dev, &res.Lease, job.id,
		map[string]any{"holder": holderName, "reattached": res.Reattached})

	r.startRun(ctx, job, res.Lease, dev, 0)
}

// startRun begins renewing a lease and working the job. fromStep is where the
// work resumes: zero for a fresh allocation, and the previous holder's
// position when a replacement re-attaches — which is what jobs.checkpoint
// carries in a real runner.
func (r *Runner) startRun(ctx context.Context, job jobRow, l lease.Lease, dev *simDevice, fromStep int64) {
	run := &jobRun{job: job, dev: dev, lease: l}
	run.step.Store(fromStep)
	run.holder = lease.NewHolder(ctx, r.store, l, lease.HolderConfig{
		Interval: r.scale(baseRenewInterval),
		Logger:   r.log.With("component", "holder", "job", short(job.id)),
		Hooks: lease.HolderHooks{
			OnRenewed: func(_ lease.Lease, _ lease.RenewResult) {
				run.renewals.Add(1)
				r.renewals.Add(1)
			},
			OnTransientError: func(_ lease.Lease, _ int, _ error, _ time.Duration) {
				// The database was briefly unreachable. The lease is untouched
				// and the job keeps its device.
				obs.LeaseRenewFailure(obs.KindTransient)
			},
			OnFenced: func(_ lease.Lease) {
				// Zero rows from farm.lease_renew: the only unambiguous proof
				// that the lease is gone.
				obs.LeaseRenewFailure(obs.KindFenced)
			},
		},
	})

	r.mu.Lock()
	r.live[job.id] = run
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.work(ctx, run)
	}()
}

// work is the job itself: real ADB commands over real sockets, on a context
// derived from the holder so that losing the lease severs every one of them at
// once rather than through a cleanup checklist someone has to remember.
func (r *Runner) work(ctx context.Context, run *jobRun) {
	hctx := run.holder.Context()
	tick := time.NewTicker(r.scale(baseWorkStep))
	defer tick.Stop()

	for {
		select {
		case <-hctx.Done():
			r.endRun(ctx, run, context.Cause(hctx))
			return
		case <-tick.C:
		}
		if int(run.step.Load()) >= run.job.steps {
			r.complete(ctx, run)
			return
		}
		cli := r.client(run.dev.hostID)
		if cli == nil {
			// startHosts registers a server and a client for a host together,
			// before any loop starts, and loadFleet only yields devices on
			// those hosts — so this cannot happen. If it ever did, the job can
			// never run a single command, and the two tempting responses are
			// both lies: skipping the step marches a no-op job to "succeeded",
			// and holding the device forever costs it to a reclaim that would
			// be blamed on the lease machinery. Fail it, out loud, and give
			// the device back the way a job is supposed to.
			r.log.Error("demo: no adb client for this host; failing the job and "+
				"releasing its device",
				"job_id", short(run.job.id), "host", run.dev.hostID,
				"rack_slot", run.dev.rackSlot)
			r.abandon(ctx, run)
			return
		}
		r.execStep(hctx, run, cli)
	}
}

func (r *Runner) execStep(hctx context.Context, run *jobRun, cli *adbwire.Client) {
	step := int(run.step.Add(1))
	cmd := shellSteps[(step-1)%len(shellSteps)].cmd

	res, err := cli.Shell(hctx, run.dev.devpath, cmd)
	if err != nil {
		if adbwire.IsCanceled(err) || hctx.Err() != nil {
			return // we are being torn down; the socket dying is us, not the world
		}
		n := run.blips.Add(1)
		r.adbBlips.Add(1)
		kind := blipKind(err)
		obs.TransportBlip(run.dev.slot(), kind)
		// Every blip is counted; only some are narrated. A device that is
		// away for a while fails every step it is asked for, and a line each
		// would bury the events that matter under the noise this system is
		// designed to ignore.
		if n == 1 || n%blipLogEvery == 0 {
			// The loudest thing this line may ever do is complain about a socket.
			r.log.Warn("adb command failed — TRANSPORT ONLY: the lease is untouched, "+
				"the fence has not moved, the device is still this job's",
				"job_id", short(run.job.id), "rack_slot", run.dev.rackSlot,
				"blip", string(kind), "step", step, "blips_so_far", n, "err", err)
		}
		return
	}
	r.log.Debug("adb step ok", "job_id", short(run.job.id), "rack_slot", run.dev.rackSlot,
		"step", step, "cmd", cmd, "exit", res.ExitCode, "bytes", len(res.Stdout))
}

// complete is the normal end of a job: the work finished, so the job releases
// its own lease with a reason the schema recognises.
func (r *Runner) complete(ctx context.Context, run *jobRun) {
	released, err := run.holder.Release(ctx, lease.ReasonCompleted, r.scale(baseRearm))
	if err != nil {
		r.log.Warn("demo: release failed", "job_id", short(run.job.id), "err", err)
	}
	if released {
		obs.LeaseReaped(obs.ReasonCompleted)
		r.jobsCompleted.Add(1)
	}
	// The bookkeeping must outlive the cancellation, for the same reason
	// Holder.Release does: the device has already been handed back, and a job
	// left RUNNING with no live lease is a row nothing will ever finish. The
	// timeout is what stops a wedged database holding shutdown open.
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := r.pool.Exec(jctx,
		`UPDATE farm.jobs SET state='succeeded', finished_at=now()
          WHERE id=$1 AND state='running'`, run.job.id); err != nil {
		r.log.Warn("demo: marking job succeeded", "job_id", short(run.job.id), "err", err)
	}
	r.event(jctx, "job_succeeded", run.dev, &run.lease, run.job.id,
		map[string]any{"steps": run.step.Load(), "renewals": run.renewals.Load(),
			"adb_blips": run.blips.Load()})
	r.log.Info("job complete, lease released",
		"job_id", short(run.job.id), "rack_slot", run.dev.rackSlot,
		"steps", run.step.Load(), "renewals", run.renewals.Load(),
		"adb_blips_survived", run.blips.Load())
	r.forget(run)
}

// abandon ends a job that cannot proceed. It is still the JOB releasing its own
// lease — the same path complete() takes, with the reason the schema keeps for
// work that did not succeed. Nothing here is a new way for a lease to end.
func (r *Runner) abandon(ctx context.Context, run *jobRun) {
	released, err := run.holder.Release(ctx, lease.ReasonFailed, r.scale(baseRearm))
	if err != nil {
		r.log.Warn("demo: release after an unrunnable job failed",
			"job_id", short(run.job.id), "err", err)
	}
	// Counted only when this call is the one that ended it, exactly as
	// complete() does: a release that matched nothing ended nothing.
	if released {
		obs.LeaseReaped(obs.ReasonFailed)
	}
	r.markJob(ctx, run.job.id, "failed")
	r.forget(run)
}

// endRun handles every ending that is not the job finishing its own work. The
// three causes mean entirely different things and are never collapsed.
func (r *Runner) endRun(ctx context.Context, run *jobRun, cause error) {
	defer r.forget(run)

	switch {
	case errors.Is(cause, lease.ErrFenced):
		// We lost the device. The honest question is WHY, and the database
		// knows: a lease ended by max_runtime is the user's own deadline
		// firing, while holder_expired would mean we stopped renewing.
		snap, err := r.leaseSnapshot(ctx, run.lease.ID)
		if err != nil {
			// We could not ask. That is a fact about our connection to the
			// database, not about the lease, and it must not be folded into
			// the verdict below: counting an unreadable row as a lease lost to
			// a transport failure would make the one number this demo exists
			// to report a lie in exactly the direction that flatters nobody.
			// The commonest way to arrive here is shutdown cancelling the read.
			r.log.Warn("lease fenced, but its ending could not be read back — "+
				"recorded as undetermined, NOT as a broken invariant",
				"job_id", short(run.job.id), "lease_id", short(run.lease.ID),
				"rack_slot", run.dev.rackSlot, "adb_blips", run.blips.Load(), "err", err)
			r.markJob(ctx, run.job.id, "failed")
			return
		}
		reason := ""
		if snap.reason != nil {
			reason = *snap.reason
		}
		// A live lease we can no longer renew was RE-ATTACHED by somebody
		// else — another farmd, or a replacement for a pod that was declared
		// gone. The job kept its device; only this process stopped being its
		// holder. That is a handover, not a loss, and it is the reason renew
		// matches on holder_instance as well as fence.
		if snap.reason == nil && (snap.state == "held" || snap.state == "suspect") {
			r.log.Warn("fenced by a re-attach: another holder now owns this lease, and the "+
				"job kept its device",
				"job_id", short(run.job.id), "lease_id", short(run.lease.ID),
				"fence", run.lease.Fence, "rack_slot", run.dev.rackSlot)
			return
		}
		switch reason {
		case "max_runtime":
			r.maxRuntimeEndings.Add(1)
			r.log.Warn("lease ended by jobs.max_runtime — the ONE automatic ending the "+
				"user asked for, fired on a number they wrote down",
				"job_id", short(run.job.id), "rack_slot", run.dev.rackSlot,
				"fence", run.lease.Fence, "steps", run.step.Load())
			r.markJob(ctx, run.job.id, "failed")
		case "holder_expired":
			// This is work destroyed by us. In a healthy farm it never happens.
			r.log.Error("LEASE RECLAIMED: this holder stopped heartbeating for ttl+grace "+
				"across no control-plane gap",
				"job_id", short(run.job.id), "rack_slot", run.dev.rackSlot,
				"fence", run.lease.Fence)
			r.markJob(ctx, run.job.id, "failed")
		case "operator_revoked", "job_cancelled", "completed", "failed", "device_retired":
			r.log.Warn("lease fenced by a deliberate ending",
				"job_id", short(run.job.id), "rack_slot", run.dev.rackSlot, "reason", reason)
			r.markJob(ctx, run.job.id, "failed")
		default:
			// The row was read and it is terminal, yet names no reason: the
			// seven in leases_release_reason_check are all handled above, so
			// reaching here means release_reason is NULL. A fence nobody can
			// account for, on a job that saw transport trouble, is the exact
			// failure mode this project exists to prevent. It is counted and
			// shouted about rather than logged once and forgotten.
			r.leasesLostToTransport.Add(1)
			r.log.Error("LEASE LOST WITH NO RECORDED REASON — if this job saw transport "+
				"errors, the invariant has been broken",
				"job_id", short(run.job.id), "rack_slot", run.dev.rackSlot,
				"lease_state", snap.state, "adb_blips", run.blips.Load())
			r.markJob(ctx, run.job.id, "failed")
		}

	case errors.Is(cause, lease.ErrHolderStopped):
		// A pod eviction. The lease, the device and the fence survive it.
		r.log.Info("holder stopped WITHOUT releasing — the lease survives the process",
			"job_id", short(run.job.id), "lease_id", short(run.lease.ID),
			"fence", run.lease.Fence, "rack_slot", run.dev.rackSlot)

	case errors.Is(cause, lease.ErrHolderReleased):
		// complete() already narrated this.

	default:
		// The demo is shutting down. Same policy as farmd on SIGTERM.
		r.log.Info("shutdown with the lease still held (deliberately): a replacement "+
			"re-attaches by job id at the same fence",
			"job_id", short(run.job.id), "lease_id", short(run.lease.ID),
			"fence", run.lease.Fence, "rack_slot", run.dev.rackSlot)
	}
}

// ---------------------------------------------------------------------
// The reaper: alerts, the two permitted automatic endings, and heartbeats
// ---------------------------------------------------------------------

func (r *Runner) reaperLoop(ctx context.Context) {
	tick := time.NewTicker(r.scale(baseReaperTick))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		// Every component on the renewal path beats. The gap is computed from
		// the OLDEST of them, so an API that dies next to a healthy reaper
		// still buys every live lease a refund instead of a mass reclaim.
		for _, c := range lease.ReaperComponents {
			if err := r.store.ComponentBeat(ctx, c); err != nil {
				if !errors.Is(err, context.Canceled) {
					r.log.Warn("demo: component beat", "component", c, "err", err)
				}
				break
			}
		}

		suspects, err := r.store.MarkSuspect(ctx, 0)
		if err != nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: suspect sweep", "err", err)
		}
		for _, s := range suspects {
			// An alert, never an action. The device stays unschedulable and
			// stays with its holder; a heartbeat inside the grace band heals
			// it at the same fence with zero work lost.
			r.log.Warn("lease is SUSPECT (an alert, not an action — nothing was released)",
				"lease_id", short(s.LeaseID), "job_id", short(s.JobID),
				"protected", s.Protected)
		}

		reclaimed, err := r.store.Reclaim(ctx, 0, r.scale(baseRearm))
		if err != nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: reclaim", "err", err)
		}
		for _, rc := range reclaimed {
			r.reclaimed.Add(1)
			obs.LeaseReaped(obs.ReasonHolderExpired)
			r.log.Error("lease RECLAIMED — the only automatic release path, and it fired "+
				"on holder silence alone; device health was not consulted and could not be",
				"lease_id", short(rc.LeaseID), "job_id", short(rc.JobID),
				"old_fence", rc.OldFence, "new_fence_floor", rc.NewFloor)
			r.markJob(ctx, rc.JobID, "failed")
		}

		expired, err := r.store.ExpireMaxRuntime(ctx, 0)
		if err != nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: max runtime sweep", "err", err)
		}
		for _, e := range expired {
			obs.LeaseReaped(obs.ReasonMaxRuntime)
			r.log.Warn("lease ended: jobs.max_runtime elapsed",
				"lease_id", short(e.LeaseID), "job_id", short(e.JobID))
			r.markJob(ctx, e.JobID, "failed")
		}
	}
}

// ---------------------------------------------------------------------
// Metrics and the one-line fleet summary
// ---------------------------------------------------------------------

func (r *Runner) statsLoop(ctx context.Context) {
	tick := time.NewTicker(r.scale(baseStatsTick))
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n++
		if err := r.publishGauges(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: publishing gauges", "err", err)
		}
		if n%4 == 0 {
			r.fleetLine(ctx)
		}
	}
}

func (r *Runner) publishGauges(ctx context.Context) error {
	held := []obs.LeaseCount{}
	rows, err := r.pool.Query(ctx, `
SELECT d.pool_id, l.tenant_id, count(*)
  FROM farm.leases l JOIN farm.devices d ON d.id = l.device_id
 WHERE l.state = 'held' GROUP BY 1,2`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c obs.LeaseCount
		if err := rows.Scan(&c.Pool, &c.Tenant, &c.Count); err != nil {
			rows.Close()
			return err
		}
		held = append(held, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	obs.SetLeaseHeld(held)

	suspect := []obs.SuspectCount{}
	rows, err = r.pool.Query(ctx, `
SELECT d.pool_id, l.tenant_id, l.protected, count(*)
  FROM farm.leases l JOIN farm.devices d ON d.id = l.device_id
 WHERE l.state = 'suspect' GROUP BY 1,2,3`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c obs.SuspectCount
		if err := rows.Scan(&c.Pool, &c.Tenant, &c.Protected, &c.Count); err != nil {
			rows.Close()
			return err
		}
		suspect = append(suspect, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	obs.SetLeaseSuspect(suspect)

	// farm.v_fleet already folds identity, position, health and allocation
	// into one row, so the census is a group-by rather than a client-side
	// stitch across four tables.
	healthCounts := []obs.DeviceHealthCount{}
	rows, err = r.pool.Query(ctx, `
SELECT COALESCE(host_id,''), COALESCE(hub_path,''), health, count(*)
  FROM farm.v_fleet GROUP BY 1,2,3`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c obs.DeviceHealthCount
		var state string
		if err := rows.Scan(&c.Host, &c.Hub, &state, &c.Count); err != nil {
			rows.Close()
			return err
		}
		c.State = obs.HealthState(state)
		healthCounts = append(healthCounts, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	obs.SetDeviceHealth(healthCounts)

	rearm := []obs.SlotRearmCount{}
	rows, err = r.pool.Query(ctx, `
SELECT s.host_id, hb.usb_path, count(*)
  FROM farm.slots s JOIN farm.hubs hb ON hb.id = s.hub_id
 WHERE s.rearm_at > now() GROUP BY 1,2`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c obs.SlotRearmCount
		if err := rows.Scan(&c.Host, &c.Hub, &c.Count); err != nil {
			rows.Close()
			return err
		}
		rearm = append(rearm, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	obs.SetSlotRearmPending(rearm)
	return nil
}

func (r *Runner) fleetLine(ctx context.Context) {
	const q = `
SELECT count(*),
       count(*) FILTER (WHERE health = 'healthy'),
       count(*) FILTER (WHERE health NOT IN ('healthy','retired')),
       count(*) FILTER (WHERE lease_state IN ('held','suspect')),
       (SELECT count(*) FROM farm.jobs WHERE state = 'queued'),
       (SELECT count(*) FROM farm.quarantines WHERE closed_at IS NULL)
  FROM farm.v_fleet`
	var total, healthy, unhealthy, leased, queued, quarantines int64
	if err := r.pool.QueryRow(ctx, q).Scan(&total, &healthy, &unhealthy, &leased,
		&queued, &quarantines); err != nil {
		// A census that cannot be taken is worth saying so. Returning silently
		// would leave the fleet line simply absent from the log, which reads
		// as a quiet farm rather than as an unreachable database.
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: fleet census failed; no fleet line this tick", "err", err)
		}
		return
	}
	r.log.Info("fleet",
		"devices", total, "healthy", healthy, "unhealthy", unhealthy, "leased", leased,
		"queued_jobs", queued, "open_quarantines", quarantines,
		"renewals", r.renewals.Load(), "adb_blips", r.adbBlips.Load(),
		"leases_lost_to_transport_failure", r.leasesLostToTransport.Load())
}

func (r *Runner) summary() {
	bar := strings.Repeat("=", 74)
	r.log.Info(bar)
	r.log.Info("demo finished",
		"jobs_started", r.jobsStarted.Load(),
		"jobs_completed", r.jobsCompleted.Load(),
		"lease_renewals", r.renewals.Load(),
		"adb_transport_blips", r.adbBlips.Load(),
		"simulated_outages", r.outages.Load(),
		"reattaches_at_same_fence", r.reattaches.Load(),
		"ended_by_max_runtime", r.maxRuntimeEndings.Load(),
		"reclaimed_after_holder_silence", r.reclaimed.Load(),
		"LEASES_LOST_TO_TRANSPORT_FAILURE", r.leasesLostToTransport.Load())
	r.log.Info("that last number is the point: every socket error, offline device and " +
		"hub failure above cost exactly zero leases")
	r.log.Info(bar)
}

// ---------------------------------------------------------------------
// The scripted incidents
// ---------------------------------------------------------------------

func (r *Runner) chaosLoop(ctx context.Context) {
	script := []struct {
		wait time.Duration
		fn   func(context.Context)
	}{
		{15 * time.Second, r.offlineIncident}, // the centrepiece
		{20 * time.Second, r.flapIncident},
		{25 * time.Second, r.evictionIncident},
		{20 * time.Second, r.hubIncident},
	}
	for _, step := range script {
		if !r.sleep(ctx, r.scale(step.wait)) {
			return
		}
		step.fn(ctx)
	}
	// Then keep the farm interesting for as long as anyone is watching.
	for {
		if !r.sleep(ctx, r.scale(60*time.Second)) {
			return
		}
		r.offlineIncident(ctx)
	}
}

// offlineIncident is the demonstration this whole project exists for.
//
// A device holding a live lease falls off the USB bus, its in-flight ADB call
// dies with ECONNRESET, and it comes back a while later. The verdict is taken
// from the database, not from a log line: same lease, same fence, no release
// reason, and heartbeats that never stopped landing.
func (r *Runner) offlineIncident(ctx context.Context) {
	run := r.pickLiveRun()
	if run == nil {
		r.log.Debug("demo: no live lease to interrupt yet")
		return
	}
	srv := r.server(run.dev.hostID)
	cli := r.client(run.dev.hostID)
	if srv == nil || cli == nil {
		return
	}
	before, err := r.leaseSnapshot(ctx, run.lease.ID)
	if err != nil {
		r.log.Warn("demo: reading lease before outage", "err", err)
		return
	}
	r.outages.Add(1)
	renewalsBefore := run.renewals.Load()

	bar := strings.Repeat("-", 74)
	r.log.Warn(bar)
	r.log.Warn("DEVICE DROPPING OFFLINE MID-LEASE — this is DeviceFarmer/STF #663's scenario",
		"rack_slot", run.dev.rackSlot, "devpath", run.dev.devpath,
		"job_id", short(run.job.id), "lease_id", short(run.lease.ID),
		"fence", before.fence, "lease_state", before.state,
		"heartbeat_seq", before.heartbeatSeq)
	r.log.Warn(bar)

	// An ECONNRESET on the next call to this device, then the transport goes
	// away entirely. SO_LINGER 0 in the fake makes it a reset, not a tidy EOF:
	// a clean close is easy to handle, and #663 is not about the easy case.
	srv.Inject(fakeadb.Fault{Devpath: run.dev.devpath, Kind: fakeadb.FaultReset, Times: 1})
	srv.SetState(run.dev.devpath, fakeadb.StateOffline)
	r.event(ctx, "device_offline", run.dev, &run.lease, run.job.id,
		map[string]any{"simulated": true, "kind": "usb_dropout"})

	// The recovery ladder acts ON BEHALF of the holder, which keeps its
	// device throughout. Tier 0 is "do nothing for one debounce window";
	// most blips self-heal, and this one will.
	attempt := r.openRecovery(ctx, run.dev, 0, map[string]any{
		"reason": "adb_state left 'device' during a live lease",
		"holder": run.lease.ID,
	})
	// Every path out of this function closes the attempt. An attempt row left
	// with finished_at NULL is not a harmless orphan: farm.recovery_attempts
	// has a partial index on exactly that predicate and the recovery view
	// reads it as a ladder still climbing, so a demo cut short at the wrong
	// moment would show a permanent phantom recovery on a healthy device.
	// Marked 'aborted', which is what abandoning it actually is.
	settled := false
	defer func() {
		if settled {
			return
		}
		// The commonest way to get here is shutdown, so the write has to
		// survive the cancellation that caused it — bounded, because a wedged
		// database must not hold the process open.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		r.finishRecovery(cctx, attempt, "aborted",
			"the demo stopped before this recovery reached a verdict", nil)
	}()

	if state, err := cli.State(ctx, run.dev.devpath); err != nil {
		r.log.Warn("tier 0 probe: the device is not answering",
			"rack_slot", run.dev.rackSlot, "err", err)
	} else {
		r.log.Warn("tier 0 probe", "rack_slot", run.dev.rackSlot, "adb_state", string(state))
	}

	// Anything harsher than a probe has to pass the holder's own policy
	// first. This refusal is computed from the live lease and the slot's
	// power domain, and it is what the recovery view shows instead of a gap.
	if refusal := r.escalationRefusal(ctx, run.dev); refusal != "" {
		refused := r.openRecovery(ctx, run.dev, 4, map[string]any{"considered": "port_power"})
		r.finishRecovery(ctx, refused, "refused", refusal, nil)
		obs.RecoveryAttempt(run.dev.slot(), obs.TierPortPowerCycle, obs.OutcomeRefusedPolicy)
		r.log.Warn("tier 4 (port power) REFUSED", "rack_slot", run.dev.rackSlot, "refusal", refusal)
	}

	if !r.sleep(ctx, r.scale(baseOutage)) {
		return
	}

	srv.SetState(run.dev.devpath, fakeadb.StateDevice)
	srv.ClearFaults()
	r.log.Info("device is back on the bus", "rack_slot", run.dev.rackSlot)

	// Give the holder a renewal or two to prove the point, then read the
	// lease back out of the database.
	if !r.sleep(ctx, 2*r.scale(baseRenewInterval)) {
		return
	}
	after, err := r.leaseSnapshot(ctx, run.lease.ID)
	if err != nil {
		r.log.Warn("demo: reading lease after outage", "err", err)
		return
	}
	r.finishRecovery(ctx, attempt, "recovered", "",
		map[string]any{"self_healed": true, "tier": 0})
	settled = true
	obs.RecoveryAttempt(run.dev.slot(), obs.TierReprobe, obs.OutcomeRecovered)

	// Three outcomes, and only one of them is a broken invariant.
	//
	// The lease may legitimately have ENDED during the outage — the job's own
	// work finished, or the user's max_runtime elapsed. What may never happen
	// is the lease ending because the transport did: a moved fence, or an
	// ending nobody deliberate can be named for.
	stillOurs := after.fence == before.fence && after.reason == nil &&
		(after.state == "held" || after.state == "suspect")
	deliberate := after.fence == before.fence && after.reason != nil &&
		*after.reason != "holder_expired"

	r.log.Warn(bar)
	switch {
	case deliberate:
		r.log.Warn("the job ended on its own terms while the device was offline — "+
			"a deliberate ending, which is the only kind there is",
			"rack_slot", run.dev.rackSlot, "job_id", short(run.job.id),
			"lease_id", short(run.lease.ID), "release_reason", *after.reason,
			"fence", after.fence, "adb_blips", run.blips.Load())
	case stillOurs:
		r.log.Warn("LEASE SURVIVED THE OUTAGE — same lease, same fence, no release reason",
			"rack_slot", run.dev.rackSlot, "job_id", short(run.job.id),
			"lease_id", short(run.lease.ID), "fence", after.fence,
			"lease_state", after.state,
			"heartbeats_during_outage", after.heartbeatSeq-before.heartbeatSeq,
			"renewals_during_outage", run.renewals.Load()-renewalsBefore,
			"adb_blips", run.blips.Load())
		r.log.Warn("STF #663 releases the device here and destroys the run. Nothing in " +
			"this system can: there is no release reason that names connectivity, and " +
			"the reaper cannot read device health at all")
		r.event(ctx, "lease_survived_transport_failure", run.dev, &run.lease, run.job.id,
			map[string]any{"blips": run.blips.Load(),
				"heartbeats": after.heartbeatSeq - before.heartbeatSeq})
	default:
		// The demo is a check, not a claim. If this ever fires, the invariant
		// is broken and the log says so in the loudest voice available.
		r.leasesLostToTransport.Add(1)
		r.log.Error("INVARIANT BROKEN: a transport failure changed lease state",
			"rack_slot", run.dev.rackSlot, "lease_id", short(run.lease.ID),
			"fence_before", before.fence, "fence_after", after.fence,
			"state_before", before.state, "state_after", after.state,
			"release_reason", derefOr(after.reason, "<null>"))
	}
	r.log.Warn(bar)
}

// flapIncident makes a healthy, idle handset flap so the damper can be watched
// working: credits drain, the device goes degraded and stops being scheduled,
// then quiet time refills them.
func (r *Runner) flapIncident(ctx context.Context) {
	dev := r.pickIdleDevice()
	if dev == nil {
		return
	}
	srv := r.server(dev.hostID)
	if srv == nil {
		return
	}
	r.log.Warn("handset started flapping (device <-> offline) — watch the flap damper",
		"rack_slot", dev.rackSlot, "devpath", dev.devpath)
	stop := srv.Flap(dev.devpath, r.scale(baseFlapHalf))
	r.event(ctx, "device_flapping", dev, nil, "", map[string]any{"simulated": true})

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if !r.sleep(ctx, r.scale(60*time.Second)) {
			stop()
			return
		}
		stop()
		srv.SetState(dev.devpath, fakeadb.StateDevice)
		r.log.Info("handset settled; the damper's credits refill with quiet time",
			"rack_slot", dev.rackSlot)
	}()
}

// evictionIncident is a Kubernetes pod eviction: the most ordinary event in a
// cluster, and one that must cost a device exactly nothing.
func (r *Runner) evictionIncident(ctx context.Context) {
	run := r.pickLiveRun()
	if run == nil {
		return
	}
	// The job is ours for the whole handover, so the scheduler does not treat
	// the gap between the two holders as an unowned running job.
	r.reserve(run.job.id)
	defer r.unreserve(run.job.id)

	bar := strings.Repeat("-", 74)
	r.log.Warn(bar)
	r.log.Warn("POD EVICTION: stopping the holder WITHOUT releasing the lease",
		"job_id", short(run.job.id), "lease_id", short(run.lease.ID),
		"fence", run.lease.Fence, "rack_slot", run.dev.rackSlot)

	// Stop blocks until the renewal loop has exited, so nothing is in flight
	// when the replacement arrives.
	run.holder.Stop()
	r.forget(run)

	if !r.sleep(ctx, r.scale(baseWorkStep)) {
		return
	}

	instance, err := lease.NewHolderInstance()
	if err != nil {
		r.log.Error("demo: mint holder instance", "err", err)
		return
	}
	res, err := r.store.Acquire(ctx, run.job.id, "runner-pod-"+short(run.job.id)+"-b", instance)
	if err != nil {
		r.log.Error("demo: replacement pod could not re-acquire",
			"job_id", short(run.job.id), "err", err)
		return
	}
	if res.Reattached && res.Lease.ID == run.lease.ID && res.Lease.Fence == run.lease.Fence {
		r.reattaches.Add(1)
		r.log.Warn("REPLACEMENT RE-ATTACHED: same lease, same device, SAME FENCE — the "+
			"fence is deliberately not bumped, because the job's own work may still be "+
			"running on the device",
			"job_id", short(run.job.id), "lease_id", short(res.Lease.ID),
			"fence", res.Lease.Fence, "rack_slot", run.dev.rackSlot)
	} else {
		r.log.Error("INVARIANT BROKEN: a pod eviction cost this job its lease",
			"job_id", short(run.job.id), "reattached", res.Reattached,
			"fence_before", run.lease.Fence, "fence_after", res.Lease.Fence)
	}
	r.log.Warn(bar)

	// The replacement resumes from where the previous holder left off, and it
	// must assume the device is dirty with that holder's own state — the
	// fence was not bumped precisely because that work may still be attached.
	resumed := jobRow{id: run.job.id, state: "running", steps: run.job.steps}
	r.startRun(ctx, resumed, res.Lease, run.dev, run.step.Load())
}

// hubIncident kills a whole hub at once. Devices that fail together nearly
// always share a hub or a power domain, so the correct response is ONE
// hub-scoped quarantine and one page, not seven device alerts.
func (r *Runner) hubIncident(ctx context.Context) {
	dev := r.pickHubVictim()
	if dev == nil {
		return
	}
	var victims []*simDevice
	inBlast := 0
	r.mu.Lock()
	for _, d := range r.devices {
		if d.hubID == dev.hubID {
			victims = append(victims, d)
		}
	}
	for _, run := range r.live {
		if run.dev.hubID == dev.hubID {
			inBlast++
		}
	}
	r.mu.Unlock()
	srv := r.server(dev.hostID)
	if srv == nil || len(victims) == 0 {
		return
	}

	bar := strings.Repeat("-", 74)
	r.log.Warn(bar)
	r.log.Warn("HUB FAILURE: every device on one hub going offline at once",
		"host", dev.hostID, "hub", dev.hubPath, "devices", len(victims),
		"live_leases_in_the_blast_radius", inBlast)
	for _, v := range victims {
		srv.SetState(v.devpath, fakeadb.StateOffline)
	}

	// Let the watchdog observe it, then ask the correlation view what it sees.
	if !r.sleep(ctx, 2*r.scale(baseStatsTick)) {
		return
	}
	devices, healthy, unhealthy, err := r.hubHealth(ctx, dev.hubID)
	if err != nil {
		r.log.Warn("demo: reading hub health", "err", err)
		return
	}
	r.log.Warn("farm.v_hub_health agrees this is a hub problem, not seven device problems",
		"host", dev.hostID, "hub", dev.hubPath,
		"devices", devices, "healthy", healthy, "unhealthy", unhealthy)

	quarantineID, err := r.openHubQuarantine(ctx, dev,
		fmt.Sprintf("%d of %d devices on hub %s unhealthy within one observation window",
			unhealthy, devices, dev.hubPath))
	if err != nil {
		r.log.Warn("demo: opening hub quarantine", "err", err)
		return
	}
	attempt := r.openRecovery(ctx, dev, 6, map[string]any{
		"scope": "hub", "unhealthy": unhealthy, "devices": devices})
	r.finishRecovery(ctx, attempt, "no_change",
		"quarantine stops scheduling and pages a human; it does not touch any live lease", nil)
	obs.RecoveryAttempt(dev.slot(), obs.TierQuarantine, obs.OutcomeFailed)

	held := r.quarantineHub(ctx, victims)
	r.log.Warn("hub quarantined: no new leases here until a human closes it",
		"quarantine_id", quarantineID, "hub", dev.hubPath,
		"live_leases_still_holding_their_devices", held,
		"note", "a device holding a live lease KEEPS it — quarantine stops allocation, not work")
	r.log.Warn(bar)

	if !r.sleep(ctx, r.scale(baseQuarantineHold)) {
		return
	}

	// A human reseated the hub. Every step of the recovery is audited and
	// names the operator.
	for _, v := range victims {
		srv.SetState(v.devpath, fakeadb.StateDevice)
	}
	if err := r.closeQuarantine(ctx, quarantineID, victims); err != nil {
		r.log.Warn("demo: closing quarantine", "err", err)
		return
	}
	r.log.Info("operator closed the hub quarantine; the devices are schedulable again",
		"quarantine_id", quarantineID, "hub", dev.hubPath, "actor", "demo-operator")
}

// escalationRefusal answers whether a port power cycle at this position would
// be refused, and why. Both halves are read from the database: the live
// lease's disruption_policy, and whether the power domain is ganged (in which
// case cutting power for one device cuts it for every device on the hub).
func (r *Runner) escalationRefusal(ctx context.Context, dev *simDevice) string {
	const q = `
SELECT COALESCE(pd.kind,'none'),
       COALESCE((SELECT l.disruption_policy FROM farm.leases l
                  WHERE l.device_id = $1::uuid AND l.state IN ('held','suspect')), ''),
       (SELECT count(*) FROM farm.slots s2
          JOIN farm.devices d2 ON d2.current_slot_id = s2.id
          JOIN farm.leases l2  ON l2.device_id = d2.id AND l2.state IN ('held','suspect')
         WHERE s2.power_domain_id = s.power_domain_id)
  FROM farm.slots s
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
 WHERE s.id = $2`
	var kind, policy string
	var leasesInDomain int
	if err := r.pool.QueryRow(ctx, q, dev.deviceID, dev.slotID).Scan(&kind, &policy, &leasesInDomain); err != nil {
		// Fail CLOSED. An empty string here means "nothing forbids cutting the
		// power", and answering that because a SELECT failed would let an
		// unanswered question authorise the most destructive tier in the
		// ladder. Not knowing whose work is on this power domain is itself a
		// refusal.
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: could not read the escalation policy for this position",
				"rack_slot", dev.rackSlot, "err", err)
		}
		return "the disruption policy and power domain for this position could not be " +
			"read, and an unanswered question is not consent"
	}
	switch {
	case policy == "no_disruption" || policy == "allow_soft_reset":
		return fmt.Sprintf("the live lease on this device carries disruption_policy=%q, "+
			"which forbids a tier-4 power cycle", policy)
	case kind == "none":
		return "this hub has no switchable VBUS: there is nothing to power cycle"
	case kind == "ganged" && leasesInDomain > 0:
		return fmt.Sprintf("the power domain is ganged and %d live lease(s) share it: "+
			"cutting power here would disturb work that is not ours", leasesInDomain)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------
// Database helpers
// ---------------------------------------------------------------------

type leaseState struct {
	state        string
	fence        int64
	heartbeatSeq int64
	reason       *string
}

func (r *Runner) leaseSnapshot(ctx context.Context, leaseID string) (leaseState, error) {
	const q = `SELECT state, fence, heartbeat_seq, release_reason FROM farm.leases WHERE id = $1::uuid`
	var out leaseState
	err := r.pool.QueryRow(ctx, q, leaseID).Scan(&out.state, &out.fence, &out.heartbeatSeq, &out.reason)
	if err != nil {
		return leaseState{}, fmt.Errorf("demo: read lease %s: %w", leaseID, err)
	}
	return out, nil
}

func (r *Runner) markJob(ctx context.Context, jobID, state string) {
	if _, err := r.pool.Exec(ctx,
		`UPDATE farm.jobs SET state=$2, finished_at=now()
          WHERE id=$1::uuid AND state IN ('queued','allocating','running')`,
		jobID, state); err != nil && !errors.Is(err, context.Canceled) {
		r.log.Warn("demo: marking job", "job_id", short(jobID), "state", state, "err", err)
	}
}

func (r *Runner) event(ctx context.Context, kind string, dev *simDevice, l *lease.Lease,
	jobID string, detail map[string]any) {

	body, err := json.Marshal(detail)
	if err != nil {
		body = []byte("{}")
	}
	var deviceID, leaseID, job any
	var slotID any
	if dev != nil {
		deviceID, slotID = dev.deviceID, dev.slotID
	}
	if l != nil {
		leaseID = l.ID
	}
	if jobID != "" {
		job = jobID
	}
	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
VALUES ($1, $2::uuid, $3::bigint, $4::uuid, $5::uuid, $6, $7::jsonb)`
	if _, err := r.pool.Exec(ctx, q, kind, deviceID, slotID, leaseID, job, "demo", string(body)); err != nil {
		// Warn, not Debug. farm.events is what /api/v1/events and the SSE
		// stream read; an event that silently failed to land is a hole in the
		// narrative at exactly the moment somebody is watching it.
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: writing event", "kind", kind, "err", err)
		}
	}
}

func (r *Runner) openRecovery(ctx context.Context, dev *simDevice, tier int, detail map[string]any) int64 {
	body, err := json.Marshal(detail)
	if err != nil {
		body = []byte("{}")
	}
	const q = `
INSERT INTO farm.recovery_attempts (device_id, slot_id, hub_id, host_id, tier, detail)
VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb)
RETURNING id`
	var id int64
	if err := r.pool.QueryRow(ctx, q, dev.deviceID, dev.slotID, dev.hubID, dev.hostID,
		tier, string(body)).Scan(&id); err != nil {
		// Zero disables finishRecovery, so a failure here silently removes the
		// whole attempt from the recovery view. Say so rather than leaving the
		// reader to wonder why a tier never appeared.
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: opening recovery attempt", "tier", tier,
				"rack_slot", dev.rackSlot, "err", err)
		}
		return 0
	}
	return id
}

func (r *Runner) finishRecovery(ctx context.Context, id int64, outcome, refusal string, detail map[string]any) {
	if id == 0 {
		return
	}
	body := []byte("{}")
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			body = b
		}
	}
	const q = `
UPDATE farm.recovery_attempts
   SET finished_at = now(), outcome = $2, refusal = NULLIF($3,''), detail = detail || $4::jsonb
 WHERE id = $1`
	if _, err := r.pool.Exec(ctx, q, id, outcome, refusal, string(body)); err != nil {
		// An unfinished attempt keeps showing as a ladder still climbing, so
		// this failure is worth a line even though nothing above it can act.
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: finishing recovery attempt",
				"attempt", id, "outcome", outcome, "err", err)
		}
	}
}

func (r *Runner) hubHealth(ctx context.Context, hubID int64) (devices, healthy, unhealthy int, err error) {
	const q = `SELECT devices, healthy, unhealthy FROM farm.v_hub_health WHERE hub_id = $1`
	err = r.pool.QueryRow(ctx, q, hubID).Scan(&devices, &healthy, &unhealthy)
	return devices, healthy, unhealthy, err
}

func (r *Runner) openHubQuarantine(ctx context.Context, dev *simDevice, reason string) (int64, error) {
	// One open quarantine per subject per scope is a partial unique index in
	// the schema; the WHERE NOT EXISTS keeps a repeat incident from raising a
	// constraint error instead of being recognised as the same problem.
	const q = `
INSERT INTO farm.quarantines (scope, hub_id, host_id, reason, auto)
SELECT 'hub', $1, $2, $3, true
 WHERE NOT EXISTS (SELECT 1 FROM farm.quarantines q
                    WHERE q.scope = 'hub' AND q.hub_id = $1 AND q.closed_at IS NULL)
RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q, dev.hubID, dev.hostID, reason).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already quarantined: return the open one.
		err = r.pool.QueryRow(ctx,
			`SELECT id FROM farm.quarantines
              WHERE scope='hub' AND hub_id=$1 AND closed_at IS NULL`, dev.hubID).Scan(&id)
	}
	return id, err
}

// quarantineHub stops the allocator scheduling onto these devices and reports
// how many of them are inside a live lease. A leased device keeps its lease:
// admin_state governs ALLOCATION, and nothing here may end work in progress.
func (r *Runner) quarantineHub(ctx context.Context, victims []*simDevice) int {
	ids := make([]string, 0, len(victims))
	for _, v := range victims {
		ids = append(ids, v.deviceID)
	}
	// The array crosses the wire as text[] and is cast server-side, so the
	// parameter type never depends on how a uuid slice happens to be encoded.
	//
	// Note which rows the health update deliberately skips: a device inside a
	// live lease keeps reporting real health to its holder. Quarantine stops
	// ALLOCATION; it has no opinion about work already in progress.
	const q = `
WITH ids AS (SELECT $1::text[]::uuid[] AS v), held AS (
  SELECT count(*) AS n FROM farm.leases l, ids
   WHERE l.device_id = ANY(ids.v) AND l.state IN ('held','suspect')
), upd AS (
  UPDATE farm.devices d SET admin_state = 'quarantined', updated_at = now()
    FROM ids
   WHERE d.id = ANY(ids.v) AND d.admin_state = 'enabled'
  RETURNING 1
), rt AS (
  UPDATE farm.device_runtime r
     SET health = 'quarantined', health_since = now(), updated_at = now()
    FROM ids
   WHERE r.device_id = ANY(ids.v)
     AND r.health NOT IN ('quarantined','retired')
     AND NOT EXISTS (SELECT 1 FROM farm.leases l
                      WHERE l.device_id = r.device_id AND l.state IN ('held','suspect'))
  RETURNING 1
)
SELECT (SELECT n FROM held), (SELECT count(*) FROM upd), (SELECT count(*) FROM rt)`
	var live, changed, frozen int
	if err := r.pool.QueryRow(ctx, q, ids).Scan(&live, &changed, &frozen); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.log.Warn("demo: quarantining hub devices", "err", err)
		}
		return 0
	}
	r.log.Info("quarantine applied",
		"devices_taken_out_of_allocation", changed,
		"health_frozen_until_a_human_looks", frozen,
		"devices_left_reporting_because_they_hold_a_lease", live)
	return live
}

func (r *Runner) closeQuarantine(ctx context.Context, id int64, victims []*simDevice) error {
	ids := make([]string, 0, len(victims))
	for _, v := range victims {
		ids = append(ids, v.deviceID)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE farm.quarantines SET closed_at = now(), closed_by = $2
          WHERE id = $1 AND closed_at IS NULL`, id, "demo-operator"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE farm.devices SET admin_state='enabled', updated_at=now()
          WHERE id = ANY($1::text[]::uuid[]) AND admin_state='quarantined'`, ids); err != nil {
		return err
	}
	// Health goes back to unknown rather than to healthy: closing a
	// quarantine is a human saying "look again", not a probe.
	if _, err := tx.Exec(ctx,
		`UPDATE farm.device_runtime
            SET health='unknown', health_since=now(), consec_bad=0, updated_at=now()
          WHERE device_id = ANY($1::text[]::uuid[]) AND health='quarantined'`, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
         VALUES ($1,$2,$3,$4,$5::jsonb)`,
		"demo-operator", "quarantine.close", fmt.Sprintf("quarantine:%d", id),
		"hub reseated; devices re-enumerated", `{"simulated":true}`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------

func (r *Runner) scale(d time.Duration) time.Duration {
	out := time.Duration(float64(d) / r.opts.Speed)
	if out < 50*time.Millisecond {
		out = 50 * time.Millisecond
	}
	return out
}

// sleep waits, and reports false if the context ended first.
func (r *Runner) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (r *Runner) poolID() string {
	if r.seed.Pool != "" {
		return r.seed.Pool
	}
	return DefaultPool
}

func (r *Runner) maxConcurrentJobs() int {
	n := len(r.devices) / 4
	if n < 2 {
		n = 2
	}
	return n
}

func (r *Runner) randN(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.IntN(n)
}

func (r *Runner) liveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// isLive reports whether this process already owns the job, or is in the
// middle of handing it from one holder to another.
func (r *Runner) isLive(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.live[jobID]; ok {
		return true
	}
	_, reserved := r.reserved[jobID]
	return reserved
}

func (r *Runner) reserve(jobID string) {
	r.mu.Lock()
	r.reserved[jobID] = struct{}{}
	r.mu.Unlock()
}

func (r *Runner) unreserve(jobID string) {
	r.mu.Lock()
	delete(r.reserved, jobID)
	r.mu.Unlock()
}

func (r *Runner) runFor(jobID string) *jobRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[jobID]
}

// forget drops a run, but only if it is still the current one for that job: an
// eviction replaces the entry, and the departing goroutine must not delete its
// own successor.
func (r *Runner) forget(run *jobRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.live[run.job.id]; ok && cur == run {
		delete(r.live, run.job.id)
	}
}

func (r *Runner) pickLiveRun() *jobRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	var candidates []*jobRun
	for _, run := range r.live {
		if run.holder.Context().Err() == nil {
			candidates = append(candidates, run)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[r.rng.IntN(len(candidates))]
}

// pickIdleDevice finds a device with no live lease, so the flap demonstration
// disturbs the allocator rather than a job.
func (r *Runner) pickIdleDevice() *simDevice {
	r.mu.Lock()
	defer r.mu.Unlock()
	leased := make(map[string]struct{}, len(r.live))
	for _, run := range r.live {
		leased[run.dev.deviceID] = struct{}{}
	}
	var candidates []*simDevice
	for _, d := range r.devices {
		if _, busy := leased[d.deviceID]; busy || d.initial != "device" {
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[r.rng.IntN(len(candidates))]
}

// pickHubVictim prefers a hub that currently holds a live lease, because the
// interesting half of a hub failure is what happens to the work on it.
func (r *Runner) pickHubVictim() *simDevice {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.live {
		// Never pick the hub the seed already broke: it is the cold-start
		// exhibit and overwriting it would confuse the two stories.
		if run.dev.hubPath != r.seed.FaultyHub || run.dev.hostID != r.seed.FaultyHost {
			return run.dev
		}
	}
	for _, d := range r.devices {
		if d.hubPath != r.seed.FaultyHub || d.hostID != r.seed.FaultyHost {
			return d
		}
	}
	return nil
}

func (r *Runner) device(id string) *simDevice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *Runner) deviceByRackSlot(rackSlot string) *simDevice {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.devices {
		if d.rackSlot == rackSlot {
			return d
		}
	}
	return nil
}

func (r *Runner) server(hostID string) *fakeadb.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.servers[hostID]
}

func (r *Runner) client(hostID string) *adbwire.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients[hostID]
}

// wireState maps a seeded adb_state onto the fake's wire vocabulary. The two
// vocabularies are the same by construction — fakeadb.State's values are a
// subset of the farm.device_runtime.adb_state CHECK list — so this is a
// narrowing, not a translation.
func wireState(adbState string) fakeadb.State {
	switch adbState {
	case "", "device":
		return fakeadb.StateDevice
	case "absent", "detached", "unknown", "host":
		// Nothing the server would list. StateAbsent keeps the row in the
		// fake's table while hiding it from every listing, which is exactly a
		// handset that fell off the bus while the control plane still has a
		// row — and possibly a live lease — for it.
		return fakeadb.StateAbsent
	default:
		return fakeadb.State(adbState)
	}
}

// blipKind classifies an adbwire failure for the transport-blip counter. Every
// branch here is a fact about a socket; none of them is a fact about a lease.
func blipKind(err error) obs.BlipKind {
	if te, ok := adbwire.AsTransport(err); ok {
		switch te.Kind {
		case adbwire.KindDial:
			return obs.BlipDial
		case adbwire.KindPeerClosed:
			// ECONNRESET and a clean EOF arrive as the same kind. The reset is
			// the one worth naming: it is the signature in #663.
			if strings.Contains(strings.ToLower(err.Error()), "reset") {
				return obs.BlipReset
			}
			return obs.BlipEOF
		case adbwire.KindTimeout:
			return obs.BlipTimeout
		case adbwire.KindFrame:
			return obs.BlipProtocol
		case adbwire.KindRead, adbwire.KindWrite:
			return obs.BlipOther
		default:
			return obs.BlipOther
		}
	}
	if adbwire.IsNotFound(err) {
		return obs.BlipTransportGone
	}
	if adbwire.IsProtocol(err) && strings.Contains(strings.ToLower(err.Error()), "offline") {
		return obs.BlipTransportGone
	}
	return obs.BlipOther
}

// iv renders a duration as a Postgres interval literal in exact microseconds.
// It is a DURATION, never an instant: nothing here tells Postgres what time
// this process thinks it is.
func iv(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%d microseconds", d/time.Microsecond)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
