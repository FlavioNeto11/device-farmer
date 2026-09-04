// Package watchdog owns device HEALTH and nothing else.
//
// # The one rule
//
// This package writes exactly one table of fleet state: farm.device_runtime.
// (It also writes its own row in farm.component_heartbeat, via
// farm.component_beat, so that a stalled watchdog is visible.) It never reads
// and never writes farm.leases, farm.jobs or farm.devices. That is not a
// convention to be weighed against convenience later — it is the third clock
// kept separate from the first two:
//
//	lease liveness   farm.leases.heartbeat_at   "does the holder still exist?"
//	job liveness     device-side progress       alerting only
//	device health    device_runtime.adb_state   THIS LOOP, and it touches no lease
//
// DeviceFarmer/STF issue #663 is what happens when the third clock is allowed
// to reach the first: a device that went offline — or merely looked offline
// through a broken socket — is taken from the job that owns it, and hours of
// work die. Here that path does not exist. The database enforces it too: the
// farm_watchdog role has ALL privileges on farm.leases revoked
// (migrations/00002_lease.sql).
//
// The Go-level enforcement is the import list. This package does not import
// internal/lease, so no function that can end, renew, or even read a lease is
// in scope anywhere in this file. Keep it that way: needing a lease value here
// is proof that the change belongs in another package.
//
// # What an observation is worth
//
// A snapshot from adbwire is whole-state: the ADB server re-sends the complete
// device list on every change and then stays silent. Silence is therefore not
// evidence. A device is marked missing only because a REAL snapshot arrived
// that did not contain it, never because no snapshot arrived — a dropped
// socket to one host must not turn every device behind it into an alert.
//
// Because the wire is silent while nothing changes, the loop also re-applies
// the last real observation every Config.Resync. That is what makes it a
// reconciler rather than an event handler: a row another actor changed - the
// recovery ladder marking a device 'recovering', an operator closing a
// quarantine - converges within one resync instead of waiting for some device
// to happen to move.
//
// # The flap damper
//
// A USB device on a marginal cable can enumerate and drop every few seconds.
// Reflecting that faithfully produces a health column that oscillates
// healthy <-> degraded, an allocator that hands the device out between blips,
// and an alert nobody can act on. device_runtime.flap_credits is a token
// bucket, refilled server-side, that prices the oscillation:
//
//   - Going DOWN (to any non-healthy state) is free, but debounced by
//     consec_bad. Safety first: a device that is failing must leave the
//     schedulable set promptly, and never at the mercy of an empty bucket.
//   - Coming back UP to healthy costs one token, on top of a consec_good
//     hysteresis. A device that keeps bouncing runs its bucket dry and then
//     STAYS out of the pool until it has been quiet long enough to refill.
//
// The asymmetry is the whole mechanism: the expensive direction is the one that
// would put a bad device back in front of a tenant.
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// Defaults.
const (
	// DefaultComponent is this loop's farm.component_heartbeat key.
	//
	// It is written so an operator can see that the health plane is alive, and
	// it must NOT be added to FARM_REAPER_COMPONENTS. farm.reaper_arm ADDS a
	// component's downtime to every live lease's deadline; letting a watchdog
	// outage move lease clocks would fuse the health plane into lease liveness,
	// which is the fusion this system exists to prevent.
	DefaultComponent = "watchdog"

	DefaultInterval    = 5 * time.Second
	DefaultCallTimeout = 10 * time.Second

	// DefaultResync is how often each host's LAST observation is re-applied
	// even though nothing changed on the wire. See Config.Resync: without it
	// this loop is edge-triggered, and anything that changes health from
	// outside it - the recovery ladder, an operator closing a quarantine -
	// would sit uncorrected until the next time a device happened to move.
	DefaultResync = 60 * time.Second

	// DefaultFlapCap matches the schema default of farm.device_runtime.flap_credits.
	DefaultFlapCap = 10.0
	// DefaultFlapRefill is tokens per minute. At 1/min a device may return to
	// healthy roughly once a minute in the long run, and a burst of ten is
	// absorbed without damping anything.
	DefaultFlapRefill = 1.0

	// DefaultMinBad debounces a fall: two consecutive bad observations before
	// health leaves its current value. One blip on a healthy device does not
	// move it.
	DefaultMinBad = 2
	// DefaultMinGood is the hysteresis on the way back up. Higher than MinBad
	// on purpose: claiming healthy is a promise to the allocator.
	DefaultMinGood = 3
)

// Config is the watchdog's wiring. Pool is required.
type Config struct {
	// Pool is used for farm.device_runtime writes, the host and slot lookups,
	// and farm.component_beat. It is never used to read or write a lease.
	Pool *pgxpool.Pool

	Component string

	// HostID limits this process to one host, as in a per-node DaemonSet. Empty
	// means every host in farm.hosts that is not administratively disabled,
	// which is how a single central watchdog runs.
	HostID string

	// ADBEndpoint overrides farm.hosts.adb_endpoint for every host this process
	// watches. A node-local deployment reaches its ADB server at 127.0.0.1:5037
	// whatever address the rest of the fleet uses to reach it.
	ADBEndpoint string

	// Interval is the heartbeat, host-list refresh and health-census period.
	// Device state itself is event-driven off track-devices, plus the Resync
	// safety net below.
	Interval time.Duration

	// Resync is the level-triggered safety net. The ADB server only speaks when
	// something changes, so a purely edge-triggered reconciler writes nothing
	// for a quiet rack - and a row changed underneath it (the recovery ladder
	// marking a device 'recovering', an operator closing a quarantine and
	// leaving it 'unknown') would then never converge. Every Resync the last
	// observation is applied again, which costs one UPDATE per attached device
	// per period and makes this loop reconcile toward a state instead of
	// reacting to an event.
	Resync time.Duration

	CallTimeout time.Duration

	// Flap damper knobs; see the package comment.
	FlapCap    float64
	FlapRefill float64
	MinBad     int
	MinGood    int

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Resync <= 0 {
		c.Resync = DefaultResync
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.FlapCap <= 0 {
		c.FlapCap = DefaultFlapCap
	}
	if c.FlapRefill <= 0 {
		c.FlapRefill = DefaultFlapRefill
	}
	if c.MinBad < 1 {
		c.MinBad = DefaultMinBad
	}
	if c.MinGood < 1 {
		c.MinGood = DefaultMinGood
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Watchdog reconciles what the ADB servers report into farm.device_runtime.
type Watchdog struct {
	cfg Config
	log *slog.Logger

	// workers is one goroutine per host, keyed by host id.
	workers map[string]*worker
	wg      sync.WaitGroup
}

type worker struct {
	host     string
	endpoint string
	epoch    int64
	cancel   context.CancelFunc
}

// New validates cfg and returns a Watchdog.
func New(cfg Config) (*Watchdog, error) {
	if cfg.Pool == nil {
		return nil, errors.New("watchdog: Config.Pool is required")
	}
	cfg.applyDefaults()
	return &Watchdog{
		cfg:     cfg,
		log:     cfg.Logger.With("component", cfg.Component),
		workers: make(map[string]*worker),
	}, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation. Stopping the watchdog freezes health at its
// last observed value; it does not mark anything missing and it does not touch
// a single lease, so a watchdog outage costs nobody a device.
func (w *Watchdog) Run(ctx context.Context) error {
	w.log.Info("watchdog loop starting",
		"host", orAll(w.cfg.HostID), "interval", w.cfg.Interval,
		"flap_cap", w.cfg.FlapCap, "flap_refill_per_min", w.cfg.FlapRefill,
		"min_bad", w.cfg.MinBad, "min_good", w.cfg.MinGood)

	defer func() {
		for _, wk := range w.workers {
			wk.cancel()
		}
		w.wg.Wait()
	}()

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		w.cycle(ctx)

		select {
		case <-ctx.Done():
			w.log.Info("watchdog loop stopping; health freezes at its last observed value")
			return nil
		case <-ticker.C:
		}
	}
}

// cycle is the timed half of the loop: heartbeat, host membership, census. The
// device state itself arrives asynchronously on each host's track-devices
// stream.
func (w *Watchdog) cycle(ctx context.Context) {
	cyclesTotal.Inc()
	w.beat(ctx)

	hosts, err := w.hosts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not read the host list; keeping the workers that are running", "err", err)
		}
	} else {
		w.reconcileWorkers(ctx, hosts)
	}

	w.census(ctx)
}

// beat records that the health plane is alive. See DefaultComponent for why
// this heartbeat must never be added to farm.reaper_arm's component list.
func (w *Watchdog) beat(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	if _, err := w.cfg.Pool.Exec(cctx, `SELECT farm.component_beat($1::text)`, w.cfg.Component); err != nil {
		if ctx.Err() == nil {
			beatFailures.Inc()
			w.log.Warn("component heartbeat failed", "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Host membership
// ---------------------------------------------------------------------------

type hostRow struct {
	ID       string
	Endpoint string
	// Epoch is farm.hosts.host_epoch. A transport_id is a small integer the ADB
	// server reuses after a restart, so it is meaningless without the epoch it
	// was minted in; the pair is recorded together or not at all. This loop only
	// COPIES the epoch — bumping it is the node role's job, and farm.hosts is
	// not a table the health plane writes.
	Epoch int64
}

func (w *Watchdog) hosts(ctx context.Context) ([]hostRow, error) {
	// A draining host is included deliberately: it is still full of devices
	// whose health an operator needs to watch while the last leases run out.
	// Only 'disabled' hosts drop out.
	const q = `
SELECT h.id, h.adb_endpoint, h.host_epoch
  FROM farm.hosts h
 WHERE h.admin_state <> 'disabled'
   AND ($1::text = '' OR h.id = $1::text)
 ORDER BY h.id`

	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	rows, err := w.cfg.Pool.Query(cctx, q, w.cfg.HostID)
	if err != nil {
		return nil, fmt.Errorf("watchdog: read hosts: %w", err)
	}
	defer rows.Close()

	var out []hostRow
	for rows.Next() {
		var h hostRow
		if err := rows.Scan(&h.ID, &h.Endpoint, &h.Epoch); err != nil {
			return nil, fmt.Errorf("watchdog: read hosts scan: %w", err)
		}
		if w.cfg.ADBEndpoint != "" {
			h.Endpoint = w.cfg.ADBEndpoint
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("watchdog: read hosts: %w", err)
	}
	return out, nil
}

// reconcileWorkers starts a reader for every host that has none and stops the
// readers of hosts that have gone away, changed endpoint, or bumped their
// host_epoch.
//
// The epoch is part of the comparison because a worker CAPTURES it at start
// and writes it alongside every transport_id it observes. farm.hosts.host_epoch
// increments on every adb server restart, and adb reuses small transport
// integers across a restart — so (epoch, transport_id) is the only stable pair,
// and a worker still stamping the previous epoch is recording a pair that
// names a transport nobody has. Restarting the reader is also the right
// response on its own terms: the epoch moved because the server it is reading
// from was replaced.
func (w *Watchdog) reconcileWorkers(ctx context.Context, hosts []hostRow) {
	want := make(map[string]hostRow, len(hosts))
	for _, h := range hosts {
		want[h.ID] = h
	}

	for id, wk := range w.workers {
		h, ok := want[id]
		if ok && h.Endpoint == wk.endpoint && h.Epoch == wk.epoch {
			continue
		}
		w.log.Info("stopping host reader", "host", id, "endpoint", wk.endpoint,
			"epoch", wk.epoch, "still_listed", ok)
		wk.cancel()
		delete(w.workers, id)
	}

	for id, h := range want {
		if _, running := w.workers[id]; running {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		wk := &worker{host: h.ID, endpoint: h.Endpoint, epoch: h.Epoch, cancel: cancel}
		w.workers[id] = wk
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.watchHost(wctx, wk)
		}()
		w.log.Info("started host reader", "host", h.ID, "endpoint", h.Endpoint, "epoch", h.Epoch)
	}
	hostsGauge.Set(float64(len(w.workers)))
}

// watchHost consumes one host's track-devices stream until its context ends.
//
// The tracker reconnects on its own with jittered backoff and emits nothing
// while it is disconnected, which is exactly the behaviour this loop needs: a
// lost socket produces no observations at all rather than a fabricated empty
// device list.
func (w *Watchdog) watchHost(ctx context.Context, wk *worker) {
	client := adbwire.New(wk.endpoint, adbwire.WithLogger(w.log.With("host", wk.host)))
	tracker := client.TrackDevices(ctx)
	defer tracker.Close()

	// lastState remembers what we last wrote per devpath so an unchanged device
	// in a snapshot about a different device costs no write. The database
	// remains the truth: a write that touches zero rows clears the entry, and
	// the resync below clears the whole cache periodically.
	lastState := make(map[string]adbwire.ConnState)

	var last adbwire.Snapshot
	var have bool

	resync := time.NewTicker(w.cfg.Resync)
	defer resync.Stop()

	snaps := tracker.Snapshots()
	for {
		select {
		case <-ctx.Done():
			return

		case snap, ok := <-snaps:
			if !ok {
				return
			}
			last, have = snap, true
			snapshotsTotal.WithLabelValues(wk.host).Inc()
			w.apply(ctx, wk, snap, lastState)

		case <-resync.C:
			if !have {
				// Nothing has ever been observed on this host, and silence is
				// not evidence. Inventing an empty listing here is exactly the
				// mistake adbwire refuses to make on a dropped socket.
				continue
			}
			// Re-apply the last real observation. Clearing the cache is what
			// makes this a reconciliation rather than a repeat: whatever the
			// row says now, it is brought back in line with what the ADB server
			// actually reported.
			clear(lastState)
			resyncsTotal.WithLabelValues(wk.host).Inc()
			w.apply(ctx, wk, last, lastState)
		}
	}
}

// apply reconciles one snapshot and reports failure without stopping the reader.
func (w *Watchdog) apply(ctx context.Context, wk *worker, snap adbwire.Snapshot,
	lastState map[string]adbwire.ConnState) {

	if err := w.reconcile(ctx, wk, snap, lastState); err != nil {
		if ctx.Err() != nil {
			return
		}
		reconcileErrors.WithLabelValues(wk.host).Inc()
		w.log.Warn("reconcile failed", "host", wk.host, "seq", snap.Sequence, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// slotRow is one physical position on a host, with the device currently sitting
// in it.
type slotRow struct {
	DeviceID string
	SlotID   int64
	Devpath  string
	RackSlot string
	HubPath  string
}

// slots reads the host's occupied positions.
//
// It joins slots to devices and stops there. farm.leases is deliberately absent
// from this query and from every other query in this file; farm.v_fleet is not
// used for the same reason, since it carries lease columns.
func (w *Watchdog) slots(ctx context.Context, host string) (map[string]slotRow, error) {
	const q = `
SELECT d.id::text, s.id, s.adb_devpath,
       COALESCE(s.rack_slot, ''), COALESCE(hb.usb_path, '')
  FROM farm.slots s
  JOIN farm.devices d ON d.current_slot_id = s.id
  LEFT JOIN farm.hubs hb ON hb.id = s.hub_id
 WHERE s.host_id = $1::text
   AND d.admin_state <> 'retired'`

	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	rows, err := w.cfg.Pool.Query(cctx, q, host)
	if err != nil {
		return nil, fmt.Errorf("watchdog: read slots of %s: %w", host, err)
	}
	defer rows.Close()

	out := make(map[string]slotRow)
	for rows.Next() {
		var s slotRow
		if err := rows.Scan(&s.DeviceID, &s.SlotID, &s.Devpath, &s.RackSlot, &s.HubPath); err != nil {
			return nil, fmt.Errorf("watchdog: read slots scan: %w", err)
		}
		out[s.Devpath] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("watchdog: read slots of %s: %w", host, err)
	}
	return out, nil
}

// reconcile folds one whole-state snapshot into farm.device_runtime.
func (w *Watchdog) reconcile(ctx context.Context, wk *worker, snap adbwire.Snapshot,
	lastState map[string]adbwire.ConnState) error {

	host := wk.host
	positions, err := w.slots(ctx, host)
	if err != nil {
		return err
	}

	observed := snap.ByDevpath()
	devicesGauge.WithLabelValues(host).Set(float64(len(snap.Devices)))

	// Duplicate OEM serials are real (STF documents a device shipping with
	// serial "0123456789ABCDEF"), which is why every address in this system is
	// a devpath. Report the collision; do not write it. farm.devices is not
	// ours: the farm_watchdog role has SELECT on it and nothing more, and
	// widening that grant to set one boolean would put the health plane inside
	// the identity table.
	if amb := snap.AmbiguousSerials(); len(amb) > 0 {
		ambiguousGauge.WithLabelValues(host).Set(float64(len(amb)))
		w.log.Warn("more than one attached device is reporting the same serial; "+
			"addressing stays by devpath", "host", host, "serials", amb)
	} else {
		ambiguousGauge.WithLabelValues(host).Set(0)
	}

	unmapped := 0
	for devpath := range observed {
		if _, ok := positions[devpath]; !ok {
			unmapped++
		}
	}
	if unmapped > 0 {
		// A device the ADB server can see that the topology does not know about
		// is an unprovisioned slot, not a health problem. Enrolling it is
		// provisioning's job, not this loop's.
		unmappedGauge.WithLabelValues(host).Set(float64(unmapped))
		w.log.Debug("attached devices with no slot in the topology", "host", host, "count", unmapped)
	} else {
		unmappedGauge.WithLabelValues(host).Set(0)
	}

	seen := make([]string, 0, len(positions))
	for devpath, pos := range positions {
		dev, present := observed[devpath]
		state := adbwire.StateAbsent
		if present {
			state = dev.State
			seen = append(seen, pos.DeviceID)
		}

		if prev, ok := lastState[devpath]; ok && prev == state {
			continue
		}

		var transport, epoch *int64
		if present && dev.TransportID != 0 {
			t := dev.TransportID
			e := wk.epoch
			transport, epoch = &t, &e
		}

		res, err := w.write(ctx, pos, host, state, transport, epoch)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			// Drop the cache entry so the next snapshot retries this device
			// rather than believing a write that never landed.
			delete(lastState, devpath)
			w.log.Warn("could not write device runtime",
				"host", host, "device", pos.DeviceID, "rack_slot", pos.RackSlot, "err", err)
			continue
		}
		lastState[devpath] = state
		w.report(host, pos, state, res)
	}

	// last_seen_at is a freshness fact, not a health transition, so it is
	// refreshed for every present device in one statement instead of forcing a
	// full reconcile write for devices whose state did not change.
	if len(seen) > 0 {
		if err := w.touch(ctx, seen); err != nil && ctx.Err() == nil {
			w.log.Debug("could not refresh last_seen_at", "host", host, "err", err)
		}
	}
	return nil
}

// writeResult is what one reconcile write reports back about its decision.
type writeResult struct {
	Previous   string
	Next       string
	Candidate  string
	Credits    float64
	ConsecBad  int
	ConsecGood int
	Suppressed bool
}

// damped reports whether the damper withheld the state the observation implied.
func (r writeResult) damped() bool { return r.Next != r.Candidate }

// write reconciles one device.
//
// The whole decision — credit refill, hysteresis, transition — is one statement
// evaluated against the server's now(). No clock on this pod is consulted and
// none is sent: a watchdog with a skewed clock must not be able to hand a
// device a bucket full of tokens it did not earn.
func (w *Watchdog) write(ctx context.Context, pos slotRow, host string,
	state adbwire.ConnState, transport, epoch *int64) (writeResult, error) {

	candidate := healthFor(state)
	bad := candidate != obs.HealthHealthy

	const q = `
WITH o AS (
  SELECT $1::uuid    AS device_id,
         $2::text    AS host_id,
         $3::bigint  AS slot_id,
         $4::text    AS adb_state,
         $5::text    AS candidate,
         $6::boolean AS bad,
         $7::bigint  AS transport_id,
         $8::bigint  AS host_epoch,
         $9::numeric AS cap,
         $10::numeric AS refill,
         $11::int    AS min_bad,
         $12::int    AS min_good
), c AS (
  SELECT o.*,
         r.health AS cur_health,
         (r.suppress_until IS NOT NULL AND r.suppress_until > now()) AS suppressed,
         -- The token bucket refills against the SERVER clock, from the instant
         -- recorded on the row itself.
         LEAST(o.cap, r.flap_credits
               + EXTRACT(EPOCH FROM (now() - r.flap_updated_at)) / 60.0 * o.refill) AS credits,
         CASE WHEN o.bad THEN r.consec_bad + 1 ELSE 0 END AS consec_bad,
         CASE WHEN o.bad THEN 0 ELSE r.consec_good + 1 END AS consec_good
    FROM farm.device_runtime r
    JOIN o ON o.device_id = r.device_id
), n AS (
  SELECT c.*,
         CASE
           -- Retirement is an administrative fact, quarantine belongs to the
           -- recovery ladder, and 'parked' is a human (or a charge limiter)
           -- saying "out of service ON PURPOSE". None of the three is an
           -- observation this loop may overwrite.
           --
           -- 'parked' is the one that changes what this loop MEANS. A parked
           -- device usually has no VBUS, so the ADB tracker reports it absent
           -- and healthFor() calls that 'missing' — which is true about the
           -- wire and false about the device. Writing it would put a perfectly
           -- good handset in front of the recovery ladder, which would climb
           -- to a port power cycle and then quarantine it. The authority for
           -- the state is farm.devices.admin_state='parked', which this role
           -- can read and cannot write; the value here is its mirror, and
           -- migration 00008 carries a trigger that holds it even if this CASE
           -- is ever edited away.
           WHEN c.cur_health IN ('retired','quarantined','parked') THEN c.cur_health
           -- An induced reset is in flight: the transport is EXPECTED to drop,
           -- so a drop proves nothing.
           WHEN c.suppressed THEN c.cur_health
           WHEN c.candidate = c.cur_health THEN c.cur_health
           -- 'unknown' is the absence of history, not a history of instability:
           -- nobody has looked at this device since it was enrolled, or since a
           -- quarantine was closed. Hysteresis exists to damp oscillation and
           -- there is nothing here to oscillate against, so one good look is
           -- enough. The token is still charged.
           WHEN NOT c.bad AND c.cur_health = 'unknown' AND c.credits >= 1 THEN c.candidate
           -- Falling is free but debounced: a device that is failing must leave
           -- the schedulable set even with an empty bucket.
           WHEN c.bad AND c.consec_bad >= c.min_bad THEN c.candidate
           -- Rising costs a token on top of the hysteresis. This is the
           -- expensive direction because it is the one that puts a device back
           -- in front of a tenant.
           WHEN NOT c.bad AND c.consec_good >= c.min_good AND c.credits >= 1 THEN c.candidate
           ELSE c.cur_health
         END AS next_health
    FROM c
)
UPDATE farm.device_runtime r
   SET adb_state       = n.adb_state,
       host_id         = COALESCE(n.host_id, r.host_id),
       slot_id         = COALESCE(n.slot_id, r.slot_id),
       transport_id    = COALESCE(n.transport_id, r.transport_id),
       host_epoch      = COALESCE(n.host_epoch, r.host_epoch),
       consec_bad      = n.consec_bad,
       consec_good     = n.consec_good,
       health          = n.next_health,
       health_since    = CASE WHEN n.next_health <> r.health THEN now() ELSE r.health_since END,
       flap_credits    = CASE WHEN n.next_health <> r.health AND NOT n.bad
                              THEN GREATEST(0::numeric, n.credits - 1)
                              ELSE n.credits END,
       flap_updated_at = now(),
       last_seen_at    = CASE WHEN n.adb_state = 'absent' THEN r.last_seen_at ELSE now() END,
       updated_at      = now()
  FROM n
 WHERE r.device_id = n.device_id
RETURNING n.cur_health, r.health, n.candidate, n.credits::float8, n.consec_bad, n.consec_good, n.suppressed`

	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	var out writeResult
	err := w.cfg.Pool.QueryRow(cctx, q,
		pos.DeviceID, host, pos.SlotID, string(state), string(candidate), bad,
		transport, epoch, w.cfg.FlapCap, w.cfg.FlapRefill, w.cfg.MinBad, w.cfg.MinGood,
	).Scan(&out.Previous, &out.Next, &out.Candidate, &out.Credits,
		&out.ConsecBad, &out.ConsecGood, &out.Suppressed)

	if err == nil {
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return writeResult{}, fmt.Errorf("watchdog: reconcile device %s: %w", pos.DeviceID, err)
	}

	// No runtime row yet: this is the first time anything has looked at this
	// device. Create it in its declared-unknown state and let the next snapshot
	// (or the retry below) move it.
	if err := w.ensure(ctx, pos, host); err != nil {
		return writeResult{}, err
	}

	// A FRESH budget for the retry. cctx has already spent up to CallTimeout on
	// the first attempt and ensure may have spent as much again, so reusing it
	// turns "this device had no runtime row" into a deadline error on the very
	// statement that was meant to fix it — a spurious failure on the one path
	// that only ever runs for a device nobody has looked at yet.
	rctx, rcancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer rcancel()

	err = w.cfg.Pool.QueryRow(rctx, q,
		pos.DeviceID, host, pos.SlotID, string(state), string(candidate), bad,
		transport, epoch, w.cfg.FlapCap, w.cfg.FlapRefill, w.cfg.MinBad, w.cfg.MinGood,
	).Scan(&out.Previous, &out.Next, &out.Candidate, &out.Credits,
		&out.ConsecBad, &out.ConsecGood, &out.Suppressed)
	if err != nil {
		return writeResult{}, fmt.Errorf("watchdog: reconcile new device %s: %w", pos.DeviceID, err)
	}
	return out, nil
}

// ensure creates the farm.device_runtime row for a device that has never been
// observed. It starts at health 'unknown', which is a real state in the CHECK
// constraint and means precisely "nobody has looked yet" — not 'healthy', which
// would let the allocator hand out a device on the strength of an assumption.
func (w *Watchdog) ensure(ctx context.Context, pos slotRow, host string) error {
	const q = `
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
VALUES ($1::uuid, $2::text, $3::bigint, 'unknown', 'unknown')
ON CONFLICT (device_id) DO NOTHING`

	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	if _, err := w.cfg.Pool.Exec(cctx, q, pos.DeviceID, host, pos.SlotID); err != nil {
		return fmt.Errorf("watchdog: create runtime row for %s: %w", pos.DeviceID, err)
	}
	return nil
}

// touch refreshes last_seen_at for devices present in the latest snapshot.
func (w *Watchdog) touch(ctx context.Context, deviceIDs []string) error {
	const q = `
UPDATE farm.device_runtime
   SET last_seen_at = now(), updated_at = now()
-- The ids arrive as text and are cast to uuid[] as an array rather than sent
-- as a uuid[] parameter: it is one less encoding assumption on the hot path,
-- and the cast still leaves the primary key usable.
 WHERE device_id = ANY($1::text[]::uuid[])`

	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	if _, err := w.cfg.Pool.Exec(cctx, q, deviceIDs); err != nil {
		return fmt.Errorf("watchdog: refresh last_seen_at: %w", err)
	}
	return nil
}

// report turns one write into metrics and, when something actually moved, a log
// line an operator can walk to a rack with.
func (w *Watchdog) report(host string, pos slotRow, state adbwire.ConnState, res writeResult) {
	if res.Next != res.Previous {
		transitionsTotal.WithLabelValues(res.Next).Inc()
		w.log.Info("device health changed",
			"host", host, "rack_slot", pos.RackSlot, "hub", pos.HubPath,
			"device", pos.DeviceID, "adb_state", string(state),
			"from", res.Previous, "to", res.Next,
			"flap_credits", res.Credits)
		return
	}
	if !res.damped() {
		return
	}

	reason := "hysteresis"
	switch {
	case res.Suppressed:
		reason = "suppressed"
	// 'parked' belongs with the other two, and leaving it out was not merely
	// a mislabel: a parked device is damped on EVERY tick for as long as the
	// hold lasts (the observation says 'missing', the row says 'parked'), so
	// hours of a routine charge hold would land in the "hysteresis" bucket and
	// drown the signal that bucket exists to carry.
	case res.Previous == "quarantined" || res.Previous == "retired" ||
		res.Previous == string(obs.HealthParked):
		reason = "sticky"
	case res.Candidate == string(obs.HealthHealthy) && res.ConsecGood >= w.cfg.MinGood && res.Credits < 1:
		reason = "no_credits"
	}
	dampedTotal.WithLabelValues(reason).Inc()
	w.log.Debug("health transition damped",
		"host", host, "rack_slot", pos.RackSlot, "device", pos.DeviceID,
		"observed", res.Candidate, "held_at", res.Next, "reason", reason,
		"flap_credits", res.Credits, "consec_bad", res.ConsecBad, "consec_good", res.ConsecGood)
}

// ---------------------------------------------------------------------------
// Census
// ---------------------------------------------------------------------------

// census publishes the per-hub health gauge.
//
// Aggregating by hub rather than by device is what turns a dead hub into one
// alert instead of twelve, and obs.SetDeviceHealth zero-fills every state of
// every hub it is given so a hub whose healthy count falls to zero produces a
// series AT zero rather than no series at all.
func (w *Watchdog) census(ctx context.Context) {
	const q = `
SELECT COALESCE(s.host_id, r.host_id, ''), COALESCE(hb.usb_path, ''), r.health, count(*)
  FROM farm.device_runtime r
  LEFT JOIN farm.devices d ON d.id = r.device_id
  LEFT JOIN farm.slots   s ON s.id = COALESCE(d.current_slot_id, r.slot_id)
  LEFT JOIN farm.hubs   hb ON hb.id = s.hub_id
 WHERE ($1::text = '' OR COALESCE(s.host_id, r.host_id) = $1::text)
 GROUP BY 1, 2, 3`

	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()

	rows, err := w.cfg.Pool.Query(cctx, q, w.cfg.HostID)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Debug("health census failed", "err", err)
		}
		return
	}
	defer rows.Close()

	var counts []obs.DeviceHealthCount
	for rows.Next() {
		var c obs.DeviceHealthCount
		var state string
		if err := rows.Scan(&c.Host, &c.Hub, &state, &c.Count); err != nil {
			w.log.Debug("health census scan failed", "err", err)
			return
		}
		// A value read back out of the health column is already in the closed
		// set; ParseHealthState is the boundary that keeps it that way.
		if parsed, ok := obs.ParseHealthState(state); ok {
			c.State = parsed
		} else {
			c.State = obs.HealthUnknown
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		w.log.Debug("health census failed", "err", err)
		return
	}
	obs.SetDeviceHealth(counts)
}

// ---------------------------------------------------------------------------
// State mapping
// ---------------------------------------------------------------------------

// healthFor maps an ADB connection state onto the health vocabulary of
// farm.device_runtime.health.
//
// Every arm is a statement about the WIRE, never about the lease on the device.
// "offline" here means the ADB server cannot talk to it; it does not mean the
// job running on it has stopped, and it may never be read as a reason to take
// the device away from that job.
func healthFor(s adbwire.ConnState) obs.HealthState {
	switch s {
	case adbwire.StateDevice:
		return obs.HealthHealthy
	case adbwire.StateOffline:
		return obs.HealthOffline
	case adbwire.StateUnauthorized:
		return obs.HealthUnauthorized
	case adbwire.StateAuthorizing, adbwire.StateConnecting:
		return obs.HealthBooting
	case adbwire.StateAbsent, adbwire.StateDetached:
		return obs.HealthMissing
	case adbwire.StateBootloader, adbwire.StateRecovery,
		adbwire.StateSideload, adbwire.StateRescue:
		// The device is alive and addressable, just not in a state that can run
		// a test. Distinguished from 'offline' because the operator response is
		// different: one is a cable, the other is a mode.
		return obs.HealthRecovering
	case adbwire.StateNoPermissions:
		// udev rules on the host, not a fault of the device.
		return obs.HealthDegraded
	default:
		return obs.HealthUnknown
	}
}

func orAll(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	cyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "cycles_total",
		Help: "Watchdog timer cycles: heartbeat, host-list refresh and health census.",
	})

	snapshotsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "snapshots_total",
		Help: "track-devices snapshots consumed per host. A silent host is normal: the ADB " +
			"server only speaks when something changes.",
	}, []string{"host"})

	devicesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "devices_attached",
		Help: "Devices in the last snapshot from each host's ADB server.",
	}, []string{"host"})

	unmappedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "devices_unmapped",
		Help: "Attached devices whose devpath matches no occupied slot. A provisioning gap, " +
			"not a health problem.",
	}, []string{"host"})

	ambiguousGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "ambiguous_serials",
		Help: "Serials currently reported by more than one attached device. Duplicate OEM " +
			"serials are why every address in this system is a devpath.",
	}, []string{"host"})

	transitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "health_transitions_total",
		Help: "Health transitions actually written, by the state entered.",
	}, []string{"to"})

	dampedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "flap_damped_total",
		Help: "Health transitions withheld by the damper. reason=no_credits is a device that " +
			"has spent its flap budget and is deliberately kept out of the pool.",
	}, []string{"reason"})

	resyncsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "resyncs_total",
		Help: "Level-triggered re-applications of the last observation, which is how health " +
			"changed by another loop or an operator converges on a quiet rack.",
	}, []string{"host"})

	reconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "reconcile_errors_total",
		Help: "Snapshots that could not be reconciled into farm.device_runtime.",
	}, []string{"host"})

	hostsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "hosts_watched",
		Help: "Hosts with a live track-devices reader in this process.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. Visible to operators; deliberately NOT part of " +
			"farm.reaper_arm's gap accounting, which would let health move lease clocks.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
func Collectors() []prometheus.Collector {
	for _, reason := range []string{"hysteresis", "no_credits", "suppressed", "sticky"} {
		dampedTotal.WithLabelValues(reason)
	}
	return []prometheus.Collector{
		cyclesTotal, snapshotsTotal, devicesGauge, unmappedGauge, ambiguousGauge,
		transitionsTotal, dampedTotal, resyncsTotal, reconcileErrors, hostsGauge, beatFailures,
	}
}
