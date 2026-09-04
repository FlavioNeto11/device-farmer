// Package recovery drives the escalation ladder that repairs devices.
//
// # Recovery never ends a lease
//
// This is the load-bearing sentence of the package. Recovery acts ON BEHALF OF
// the holder while the holder KEEPS its device: the lease clock keeps ticking,
// the fence never moves, and no code path in this file writes farm.leases. It
// reads them — it must, to know what it is allowed to disturb — but the only
// direction that information flows is toward refusing to act.
//
// The enforcement, as in internal/watchdog, is the import list: this package
// does not import internal/lease, so lease_release, lease_reclaim and every
// other ending are out of scope in every function here. A change that needs one
// of them belongs in another package and almost certainly should not exist.
//
// # Refusal is a first-class outcome
//
// Every tier in farm.recovery_tiers declares a blast_radius (what else it
// disturbs) and a requires_policy (the minimum farm.jobs.disruption_policy a
// live lease in that radius must carry). A tier whose blast radius reaches a
// lease that forbids it is REFUSED — not downgraded, not deferred silently, and
// certainly not performed anyway. The refusal is written to
// farm.recovery_attempts.refusal so the UI can say
//
//	tier 4 refused: lease 9f3… on device … (job …) is no_disruption
//
// instead of showing a device that mysteriously never got fixed. A broken
// device that still holds live work stays broken until the work ends. That is
// the correct trade: a phone is worth less than six hours of somebody's test
// run, and DeviceFarmer/STF issue #663 is what the other trade looks like.
//
// # Correlation before escalation
//
// Devices that fail together share hardware. When a majority of one hub's
// devices go bad inside a short window, the evidence is about the hub — a power
// brick, a controller, a cable to the host — and opening N device quarantines
// would produce N pages, N ladder climbs, and N power cycles that cannot fix a
// hub. So the ladder opens ONE hub-scoped quarantine instead, and the members
// are left alone.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// Defaults.
const (
	// DefaultComponent is this loop's farm.component_heartbeat key. Like the
	// watchdog's, it exists so a stalled loop is visible, and like the
	// watchdog's it must NOT be added to FARM_REAPER_COMPONENTS: recovery is on
	// the health side of the firewall and its downtime may not move a lease
	// deadline.
	DefaultComponent = "recovery"

	DefaultInterval    = 15 * time.Second
	DefaultBatch       = 25
	DefaultCallTimeout = 15 * time.Second

	// DefaultDebounce is how long a device must have been unhealthy before the
	// ladder considers it. Most blips are shorter than this and self-heal; tier
	// 0 exists for the ones that are not.
	DefaultDebounce = 45 * time.Second

	// DefaultActionTimeout bounds one actuator call. A wedged uhubctl must not
	// stall the whole ladder.
	DefaultActionTimeout = 90 * time.Second

	// DefaultMaxSuppress caps how long an attempt may hold the watchdog's
	// health writes off a device. The suppression exists so an INDUCED reset is
	// not mistaken for a fault; letting a 15-minute tier cooldown suppress
	// health for 15 minutes would instead hide a real one.
	DefaultMaxSuppress = 3 * time.Minute

	// DefaultStaleAttempt is when an attempt that never recorded a finish is
	// assumed dead (the process was killed mid-action) and stops blocking new
	// attempts on that device.
	DefaultStaleAttempt = 15 * time.Minute

	// Hub correlation defaults: a strict majority of the hub's devices, at
	// least two of them, all going bad inside one window.
	DefaultHubQuorum = 0.5
	DefaultMinHubBad = 2
	DefaultHubWindow = 3 * time.Minute
	DefaultLockClass = 26981 // 'r','e' — the advisory-lock class for this loop

	// unhealthyPredicate is the numerator of the hub-fault quorum, and it
	// counts ONLY states that are positive evidence of a device or hardware
	// fault. Everything else is excluded because it is not evidence:
	//
	//	'unknown'      nobody has looked. reconcileQuarantines writes it to
	//	               EVERY device on a hub, in one statement, the moment an
	//	               operator closes that hub's quarantine — same health_since
	//	               for all of them, so the spread is zero and the quorum is
	//	               unanimous. Counting it re-opens the quarantine the
	//	               operator just closed, about a debounce window later,
	//	               forever.
	//	'recovering'   this loop wrote it in begin. Reading our own induced
	//	               state back as evidence of a hub fault is the same mistake
	//	               suppress_until exists to prevent, one level up: two
	//	               devices under active recovery on a three-device hub reach
	//	               quorum and quarantine the healthy third.
	//	'quarantined'  our own bookkeeping, not a new observation.
	//	'booting'      authorizing/connecting is transient and normal; a
	//	               legitimate mass reboot would otherwise read as a hub fault.
	//
	// The denominator stays count(*) over the hub's devices: including
	// retired and healthy ones only makes quorum harder, which is the safe
	// direction for an action whose blast radius is a whole hub.
	unhealthyPredicate = `r.health IN ('offline','unauthorized','missing','degraded')`

	// coveredByQuarantine is the one definition of "an open quarantine covers
	// this device", shared by the two places that ask. It expects a device `d`
	// and its slot `s` in scope, and the whole of farm.quarantines as `q`.
	//
	// Scope decides which subject column counts, and that is the load-bearing
	// part. A scope='device' row carries host_id as well — quarantineDevice
	// fills it in so the row can be reported without a slot lookup — and a
	// predicate that reads host_id without checking scope therefore reads one
	// broken phone as a quarantine over its whole host. In the candidate query
	// that silently makes every device on the host ineligible for recovery: a
	// single stuck phone stops the ladder for sixty of its neighbours, and the
	// symptom is devices that stay broken with no refusal recorded, because no
	// attempt was ever considered. In reconcileQuarantines it means the same
	// host's devices are never released.
	//
	// 'power_domain' is in the table's CHECK constraint but has no column here,
	// so it cannot be expressed; the last arm falls back to whatever subject
	// columns such a row does carry. That can only over-cover, which for a test
	// that decides whether the ladder may touch a device is the safe direction.
	coveredByQuarantine = `
     SELECT 1 FROM farm.quarantines q
      WHERE q.closed_at IS NULL
        AND ( (q.scope = 'device' AND q.device_id = d.id)
           OR (q.scope = 'slot'   AND q.slot_id   = s.id)
           OR (q.scope = 'hub'    AND q.hub_id    = s.hub_id)
           OR (q.scope = 'host'   AND q.host_id   = s.host_id)
           OR (q.scope NOT IN ('device','slot','hub','host')
               AND (q.device_id = d.id OR q.slot_id = s.id
                    OR q.hub_id = s.hub_id OR q.host_id = s.host_id)) )`
)

// Outcome mirrors the CHECK constraint on farm.recovery_attempts.outcome.
// There is no value here that means "gave up and took the device back": the
// ladder has no such power.
type Outcome string

const (
	// OutcomeRecovered — the actuator reports the device came back. The final
	// word still belongs to the watchdog's next observation.
	OutcomeRecovered Outcome = "recovered"
	// OutcomeNoChange — the action ran and changed nothing.
	OutcomeNoChange Outcome = "no_change"
	// OutcomeFailed — the action itself errored.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused — a live lease in the blast radius forbids this tier.
	OutcomeRefused Outcome = "refused"
	// OutcomeAborted — the loop was cancelled mid-action.
	OutcomeAborted Outcome = "aborted"
)

// Action is one rung of the ladder applied to one physical position.
//
// Every field that addresses hardware is a POSITION, never a serial. Devpath
// comes from farm.slots.adb_devpath, and duplicate OEM serials are real: a
// recovery addressed by serial can land on a healthy device that is holding a
// live six-hour lease.
type Action struct {
	Tier     int
	TierName string

	DeviceID string
	SlotID   int64
	// Devpath is "usb:3-1.4" form, ready for adbwire's position-addressed
	// services. It is the only safe address for a destructive action.
	Devpath  string
	RackSlot string

	HubID   int64
	HubPath string
	HostID  string
	// ADBEndpoint is farm.hosts.adb_endpoint for the device's host.
	ADBEndpoint string
	// PowerDomainID is nil when the slot's power topology is unknown, which the
	// ladder treats as "assume the worst" rather than "assume per-port".
	PowerDomainID *int64

	// Acknowledged are the OTHER positions in this power domain that the ladder
	// has checked and is willing to see go dark: every slot the blast-radius
	// check just covered, minus the target itself. It is populated only for a
	// power_domain rung on a slot whose power topology is actually known, and
	// only after checkBlastRadius has returned no refusal for that radius —
	// which is precisely the statement "no live lease in this domain forbids
	// this tier".
	//
	// Empty means "this call authorises the target and nothing else", which is
	// the default and the safe reading. It is never a way to widen a radius the
	// check did not clear: the list is built from the same domain the check ran
	// against, and it is written into farm.recovery_attempts.detail, so what was
	// authorised is a record rather than an inference.
	//
	// There is a window between the check and the cycle in which somebody could
	// acquire a lease on a neighbour. It is the same window the single-device
	// path has always had — the check has always preceded the action — and it is
	// why the agent re-checks the hub itself before switching anything.
	Acknowledged []string

	// Timeout is the budget for this action.
	Timeout time.Duration
}

// Result is what an Actuator reports.
type Result struct {
	// Outcome must be one of OutcomeRecovered, OutcomeNoChange or
	// OutcomeFailed. Anything else is recorded as failed: never claim a
	// recovery that cannot be proven, because a false "recovered" suppresses
	// the page that should have followed.
	Outcome Outcome
	// Detail is merged into farm.recovery_attempts.detail.
	Detail map[string]any
}

// Actuator performs the tiers that touch hardware or the host's ADB server —
// tiers 1 to 5 and 7 in the shipped ladder.
//
// It is an interface because those actions belong to the host-side proxy (the
// node role), which owns the ADB endpoint, the USB device nodes and uhubctl.
// This loop owns the decision: which rung, whether the blast radius is
// permitted, whether the budget allows it, and the record of what happened.
//
// The control-plane rungs — observe, quarantine, host_drain — never reach an
// Actuator; they are database actions and are performed here.
type Actuator interface {
	Recover(ctx context.Context, a Action) (Result, error)
}

// Config is the ladder's wiring. Pool and Actuator are required.
type Config struct {
	// Pool reads farm.recovery_tiers, farm.device_runtime, the topology and
	// (read-only) farm.leases, and writes farm.recovery_attempts,
	// farm.quarantines, farm.device_runtime and farm.events.
	Pool *pgxpool.Pool

	// Actuator performs the hardware rungs. Required: a ladder that cannot act
	// would climb tiers, record successes and repair nothing.
	Actuator Actuator

	Component string
	Interval  time.Duration
	Batch     int

	Debounce      time.Duration
	ActionTimeout time.Duration
	MaxSuppress   time.Duration
	StaleAttempt  time.Duration
	CallTimeout   time.Duration

	// HubQuorum is the fraction of a hub's devices that must be unhealthy
	// before the failure is attributed to the hub. MinHubBad is the floor in
	// absolute devices, and HubWindow is how close together they must have
	// gone bad to count as one event.
	HubQuorum float64
	MinHubBad int
	HubWindow time.Duration

	// LockClass is the pg_advisory_xact_lock class used to serialise attempts
	// on one device across replicas.
	LockClass int32

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.Debounce <= 0 {
		c.Debounce = DefaultDebounce
	}
	if c.ActionTimeout <= 0 {
		c.ActionTimeout = DefaultActionTimeout
	}
	if c.MaxSuppress <= 0 {
		c.MaxSuppress = DefaultMaxSuppress
	}
	if c.StaleAttempt <= 0 {
		c.StaleAttempt = DefaultStaleAttempt
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.HubQuorum <= 0 || c.HubQuorum > 1 {
		c.HubQuorum = DefaultHubQuorum
	}
	if c.MinHubBad < 1 {
		c.MinHubBad = DefaultMinHubBad
	}
	if c.HubWindow <= 0 {
		c.HubWindow = DefaultHubWindow
	}
	if c.LockClass == 0 {
		c.LockClass = DefaultLockClass
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Ladder is the recovery driver.
type Ladder struct {
	cfg Config
	log *slog.Logger
}

// New validates cfg and returns a Ladder.
func New(cfg Config) (*Ladder, error) {
	if cfg.Pool == nil {
		return nil, errors.New("recovery: Config.Pool is required")
	}
	if cfg.Actuator == nil {
		return nil, errors.New("recovery: Config.Actuator is required; " +
			"without one the ladder would climb tiers and repair nothing")
	}
	cfg.applyDefaults()
	return &Ladder{cfg: cfg, log: cfg.Logger.With("component", cfg.Component)}, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation. A stopped ladder leaves broken devices
// broken, which is inconvenient and harmless; it takes nothing from anybody.
func (l *Ladder) Run(ctx context.Context) error {
	l.log.Info("recovery ladder starting",
		"interval", l.cfg.Interval, "batch", l.cfg.Batch, "debounce", l.cfg.Debounce,
		"hub_quorum", l.cfg.HubQuorum, "hub_window", l.cfg.HubWindow)

	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()

	for {
		l.cycle(ctx)

		select {
		case <-ctx.Done():
			l.log.Info("recovery ladder stopping; no lease was ever at risk from it")
			return nil
		case <-ticker.C:
		}
	}
}

func (l *Ladder) cycle(ctx context.Context) {
	cyclesTotal.Inc()
	l.beat(ctx)

	// Quarantine bookkeeping first, so a quarantine an operator closed through
	// the API releases its devices back to the watchdog in the same cycle.
	l.reconcileQuarantines(ctx)

	tiers, err := l.tiers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("could not read the recovery tier table", "err", err)
		}
		return
	}

	cands, err := l.candidates(ctx)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("could not read recovery candidates", "err", err)
		}
		return
	}
	candidatesGauge.Set(float64(len(cands)))
	if len(cands) == 0 {
		return
	}

	// Correlate before escalating: one hub quarantine beats six device ladders.
	handled := l.correlate(ctx, cands, tiers)

	for _, c := range cands {
		if ctx.Err() != nil {
			return
		}
		if _, done := handled[c.DeviceID]; done {
			continue
		}
		l.attempt(ctx, c, tiers)
	}
}

func (l *Ladder) beat(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	if _, err := l.cfg.Pool.Exec(cctx, `SELECT farm.component_beat($1::text)`, l.cfg.Component); err != nil {
		if ctx.Err() == nil {
			beatFailures.Inc()
			l.log.Warn("component heartbeat failed", "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// The tier table
// ---------------------------------------------------------------------------

// tier is one row of farm.recovery_tiers. The table is read every cycle rather
// than cached, because it is stored precisely so an operator can retune budgets
// without a redeploy.
type tier struct {
	Tier           int
	Name           string
	BlastRadius    string
	RequiresPolicy string
	Cooldown       time.Duration
	MaxPerHour     int
}

func (l *Ladder) tiers(ctx context.Context) ([]tier, error) {
	const q = `
SELECT t.tier, t.name, t.blast_radius, t.requires_policy,
       EXTRACT(EPOCH FROM t.cooldown)::float8, t.max_per_hour
  FROM farm.recovery_tiers t
 WHERE t.enabled
 ORDER BY t.tier`

	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	rows, err := l.cfg.Pool.Query(cctx, q)
	if err != nil {
		return nil, fmt.Errorf("recovery: read tiers: %w", err)
	}
	defer rows.Close()

	var out []tier
	for rows.Next() {
		var t tier
		var secs float64
		if err := rows.Scan(&t.Tier, &t.Name, &t.BlastRadius, &t.RequiresPolicy, &secs, &t.MaxPerHour); err != nil {
			return nil, fmt.Errorf("recovery: read tiers scan: %w", err)
		}
		t.Cooldown = time.Duration(secs * float64(time.Second))
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recovery: read tiers: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("recovery: no enabled tiers in farm.recovery_tiers")
	}
	return out, nil
}

// next returns the rung to try for a device whose ladder_tier is cur.
//
// # farm.device_runtime.ladder_tier is the lowest rung NOT YET SPENT
//
// It is not "the rung last attempted", and the difference is the whole reason
// tier 0 is reachable at all. The column DEFAULTs to 0 for a fresh device_runtime
// row and is only ever RESET to 0 — by reconcileQuarantines, when a quarantine
// closes or a device goes healthy. So a predicate of `t.Tier > cur` can never be
// satisfied by tier 0, and "observe: do nothing for one debounce window; most
// blips self-heal" — the cheapest, least disruptive rung, the one the tier table
// puts first precisely so the expensive ones are not reached by accident — would
// never run. Every incident would open with an adb reconnect against a blip that
// was going to clear itself.
//
// Hence `>=` here, and hence rungAfter in begin: spending a rung moves the column
// PAST it rather than onto it. The two must stay in step; ladderIsMonotonic in
// ladder_fidelity_test.go is what says so.
//
// A device that has exhausted the ladder repeats its top rung, bounded by that
// rung's own cooldown and max_per_hour. Repeating a six-hourly host_drain is
// the ladder's way of saying "still broken, still needs a human".
//
// Upgrade note: rows written by the previous reading carry the rung last spent,
// so on the first cycle after deploying this each mid-climb device repeats one
// rung before resuming. That costs one rung under its own cooldown, which is the
// safe direction to be wrong in; the alternative — a migration that guesses which
// rung a device was on — could skip one instead.
func next(tiers []tier, cur int) tier {
	for _, t := range tiers {
		if t.Tier >= cur {
			return t
		}
	}
	return tiers[len(tiers)-1]
}

// rungAfter is the ladder_tier a device carries once rung t has been spent.
//
// It is t.Tier+1 and not "the next row in tiers" on purpose: the tier table is
// an operator-editable table with an enabled flag, so the rung after 2 may be 4
// today and 3 tomorrow. Storing the first integer ABOVE the rung just spent lets
// next() resolve the gap against whatever the table says on the cycle that reads
// it, instead of freezing yesterday's ordering into a device's row.
func rungAfter(t tier) int { return t.Tier + 1 }

// ---------------------------------------------------------------------------
// Candidates
// ---------------------------------------------------------------------------

// candidate is one unhealthy device the ladder may act on.
type candidate struct {
	DeviceID    string
	Health      string
	LadderTier  int
	SlotID      int64
	Devpath     string
	RackSlot    string
	HubID       int64
	HubPath     string
	HostID      string
	ADBEndpoint string
	PowerDomain *int64
}

func (c candidate) slot() obs.Slot {
	return obs.Slot{Host: c.HostID, Hub: c.HubPath, RackSlot: c.RackSlot}
}

// candidates lists devices that are unhealthy, settled, unsuppressed, and not
// already covered by an open quarantine at any scope.
//
// farm.leases is absent from this query on purpose. Whether a device is leased
// changes what the ladder may DO to it, never whether the ladder looks at it;
// mixing the two here would make the candidate set depend on allocation state.
//
// # Only a health value the watchdog is standing behind
//
// Three predicates below exist for one reason: this loop may act only on
// evidence the health plane currently vouches for, never on a value that was
// written by somebody else — including by this loop — and left unconfirmed.
//
//   - health 'unknown' is excluded. It means nobody has looked: it is what a
//     fresh farm.device_runtime row carries, and what reconcileQuarantines
//     writes to every device of a quarantine an operator has just closed.
//     Climbing a ladder on it is escalating against the ABSENCE of evidence,
//     which for a tier that power-cycles a port is the exact shape of mistake
//     this package refuses to make about leases.
//
//   - adb_state 'device' is excluded. That value means the ADB server is
//     talking to this device RIGHT NOW; there is no transport left to repair.
//     A device can sit at health 'recovering' with a perfectly good transport
//     for minutes, because promotion back to 'healthy' costs the watchdog's
//     consec_good hysteresis plus a flap token — deliberately slower than
//     this ladder's cooldowns. Without this predicate the ladder outruns the
//     damper and climbs from a reconnect that WORKED to a port power cycle and
//     then a quarantine, against a device that is already back. Keeping the
//     device out of the schedulable pool meanwhile is the damper's job, and it
//     still does it: the allocator reads health, not adb_state.
//
//   - updated_at must be past any lapsed suppression window. While
//     suppress_until is open the watchdog deliberately FREEZES health at
//     whatever it was, so waiting only for the window to lapse spends the next
//     rung on the frozen value. Requiring a write after the window is what
//     makes the settle window actually settle something.
//
// With no watchdog running there is no confirmation and the ladder stalls. A
// stalled ladder leaves broken devices broken; a ladder driven by a dead
// health plane power-cycles a working rack.
func (l *Ladder) candidates(ctx context.Context) ([]candidate, error) {
	const q = `
SELECT r.device_id::text, r.health, r.ladder_tier,
       s.id, s.adb_devpath, COALESCE(s.rack_slot, ''), s.power_domain_id,
       hb.id, COALESCE(hb.usb_path, ''), ho.id, ho.adb_endpoint
  FROM farm.device_runtime r
  JOIN farm.devices d ON d.id = r.device_id
  JOIN farm.slots   s ON s.id = d.current_slot_id
  JOIN farm.hubs   hb ON hb.id = s.hub_id
  JOIN farm.hosts  ho ON ho.id = s.host_id
 WHERE r.health NOT IN ('healthy','retired','quarantined','unknown')
   AND r.adb_state <> 'device'
   AND r.health_since < now() - $1::interval
   AND (r.suppress_until IS NULL
        OR (r.suppress_until <= now() AND r.updated_at > r.suppress_until))
   AND d.admin_state = 'enabled'
   AND s.state = 'active'
   AND ho.admin_state <> 'disabled'
   AND NOT EXISTS (` + coveredByQuarantine + `)
 ORDER BY r.health_since
 LIMIT $2`

	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	rows, err := l.cfg.Pool.Query(cctx, q, pgInterval(l.cfg.Debounce), l.cfg.Batch)
	if err != nil {
		return nil, fmt.Errorf("recovery: read candidates: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.DeviceID, &c.Health, &c.LadderTier,
			&c.SlotID, &c.Devpath, &c.RackSlot, &c.PowerDomain,
			&c.HubID, &c.HubPath, &c.HostID, &c.ADBEndpoint); err != nil {
			return nil, fmt.Errorf("recovery: read candidates scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recovery: read candidates: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Correlation
// ---------------------------------------------------------------------------

// hubStat is one hub's failure shape.
type hubStat struct {
	HubID     int64
	HostID    string
	HubPath   string
	Devices   int
	Unhealthy int
	Spread    time.Duration // last_bad - first_bad
}

// correlate opens hub-scoped quarantines where the evidence is about the hub,
// and returns the devices it has therefore taken care of.
func (l *Ladder) correlate(ctx context.Context, cands []candidate, tiers []tier) map[string]struct{} {
	handled := make(map[string]struct{})

	ids := make([]int64, 0, len(cands))
	seen := make(map[int64]struct{}, len(cands))
	for _, c := range cands {
		if _, ok := seen[c.HubID]; ok {
			continue
		}
		seen[c.HubID] = struct{}{}
		ids = append(ids, c.HubID)
	}

	stats, err := l.hubStats(ctx, ids)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("hub correlation query failed; falling back to per-device recovery", "err", err)
		}
		return handled
	}

	qTier, ok := tierNamed(tiers, "quarantine")
	if !ok {
		l.log.Warn("no enabled 'quarantine' tier; cannot open a hub quarantine")
		return handled
	}

	for _, st := range stats {
		if !l.hubIsTheFault(st) {
			continue
		}
		reason := fmt.Sprintf(
			"%d of %d devices on hub %s went unhealthy within %s — the evidence is about the hub, not the devices",
			st.Unhealthy, st.Devices, st.HubPath, st.Spread.Round(time.Second))

		opened, err := l.openHubQuarantine(ctx, st, qTier, reason)
		if err != nil {
			if ctx.Err() == nil {
				l.log.Warn("could not open hub quarantine", "hub", st.HubID, "err", err)
			}
			continue
		}
		// Whether or not this cycle opened it, the hub is quarantined now, so
		// its devices are not the ladder's business.
		for _, c := range cands {
			if c.HubID == st.HubID {
				handled[c.DeviceID] = struct{}{}
			}
		}
		if opened {
			hubQuarantines.Inc()
			l.log.Error("HUB QUARANTINED: correlated failure, one alert instead of many",
				"hub", st.HubID, "hub_path", st.HubPath, "host", st.HostID,
				"unhealthy", st.Unhealthy, "devices", st.Devices, "window", st.Spread)
		}
	}
	return handled
}

// hubIsTheFault applies the correlation rule: a strict majority of the hub's
// devices, at least MinHubBad of them, all having gone bad inside one window.
//
// The window matters as much as the count. Six devices that failed over three
// days are six independent faults; six that failed inside three minutes are one
// fault upstream of all of them.
func (l *Ladder) hubIsTheFault(st hubStat) bool {
	if st.Devices < 2 || st.Unhealthy < l.cfg.MinHubBad {
		return false
	}
	if float64(st.Unhealthy) <= l.cfg.HubQuorum*float64(st.Devices) {
		return false
	}
	return st.Spread <= l.cfg.HubWindow
}

func (l *Ladder) hubStats(ctx context.Context, hubIDs []int64) ([]hubStat, error) {
	if len(hubIDs) == 0 {
		return nil, nil
	}
	const q = `
SELECT s.hub_id, hb.host_id, COALESCE(hb.usb_path, ''),
       count(*),
       count(*) FILTER (WHERE ` + unhealthyPredicate + `),
       COALESCE(EXTRACT(EPOCH FROM (
         max(r.health_since) FILTER (WHERE ` + unhealthyPredicate + `) -
         min(r.health_since) FILTER (WHERE ` + unhealthyPredicate + `)))::float8, 0)
  FROM farm.slots s
  JOIN farm.hubs hb ON hb.id = s.hub_id
  JOIN farm.devices d ON d.current_slot_id = s.id
  JOIN farm.device_runtime r ON r.device_id = d.id
 WHERE s.hub_id = ANY($1::bigint[])
 GROUP BY s.hub_id, hb.host_id, hb.usb_path`

	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	rows, err := l.cfg.Pool.Query(cctx, q, hubIDs)
	if err != nil {
		return nil, fmt.Errorf("recovery: hub stats: %w", err)
	}
	defer rows.Close()

	var out []hubStat
	for rows.Next() {
		var st hubStat
		var spread float64
		if err := rows.Scan(&st.HubID, &st.HostID, &st.HubPath, &st.Devices, &st.Unhealthy, &spread); err != nil {
			return nil, fmt.Errorf("recovery: hub stats scan: %w", err)
		}
		st.Spread = time.Duration(spread * float64(time.Second))
		out = append(out, st)
	}
	return out, rows.Err()
}

// openHubQuarantine opens the quarantine, marks the hub's devices so the
// allocator stops choosing them, records the attempt and writes an event.
//
// Marking is done in farm.device_runtime.health, because that is what
// farm.lease_acquire actually consults — a row in farm.quarantines alone stops
// nothing. Live leases are untouched: quarantine blocks NEW allocations, and a
// job already on one of these devices keeps it.
func (l *Ladder) openHubQuarantine(ctx context.Context, st hubStat, t tier, reason string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	tx, err := l.cfg.Pool.Begin(cctx)
	if err != nil {
		return false, fmt.Errorf("recovery: begin hub quarantine: %w", err)
	}
	defer tx.Rollback(cctx)

	var qid int64
	err = tx.QueryRow(cctx, `
INSERT INTO farm.quarantines (scope, hub_id, host_id, reason, auto)
SELECT 'hub', $1::bigint, $2::text, $3::text, true
 WHERE NOT EXISTS (
   SELECT 1 FROM farm.quarantines q
    WHERE q.scope = 'hub' AND q.hub_id = $1::bigint AND q.closed_at IS NULL)
RETURNING id`, st.HubID, st.HostID, reason).Scan(&qid)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Already open — another replica or an earlier cycle. Nothing to do and
		// nothing to report as new.
		return false, nil
	case isSQLState(err, sqlStateUniqueViolation):
		// Lost the race against another replica between the EXISTS check and
		// the insert. The partial unique index did its job.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("recovery: open hub quarantine: %w", err)
	}

	if _, err := tx.Exec(cctx, `
UPDATE farm.device_runtime r
   SET health = 'quarantined', health_since = now(), updated_at = now()
  FROM farm.devices d, farm.slots s
 WHERE r.device_id = d.id AND d.current_slot_id = s.id
   AND s.hub_id = $1::bigint
   AND r.health NOT IN ('quarantined','retired')`, st.HubID); err != nil {
		return false, fmt.Errorf("recovery: quarantine hub devices: %w", err)
	}

	detail := jsonDetail(map[string]any{
		"scope":         "hub",
		"quarantine_id": qid,
		"devices":       st.Devices,
		"unhealthy":     st.Unhealthy,
		"window_s":      st.Spread.Seconds(),
	})
	if _, err := tx.Exec(cctx, `
INSERT INTO farm.recovery_attempts
  (hub_id, host_id, tier, finished_at, outcome, detail)
VALUES ($1::bigint, $2::text, $3::int, now(), 'no_change', $4::jsonb)`,
		st.HubID, st.HostID, t.Tier, detail); err != nil {
		return false, fmt.Errorf("recovery: record hub quarantine attempt: %w", err)
	}

	if _, err := tx.Exec(cctx, `
INSERT INTO farm.events (kind, actor, detail)
VALUES ('hub_quarantined', $1::text, $2::jsonb)`,
		l.cfg.Component, detail); err != nil {
		return false, fmt.Errorf("recovery: record hub quarantine event: %w", err)
	}

	if err := tx.Commit(cctx); err != nil {
		return false, fmt.Errorf("recovery: commit hub quarantine: %w", err)
	}

	// The outcome is 'no_change' and not 'recovered' because nothing was
	// repaired: scheduling stopped and a human was told.
	attemptsTotal.WithLabelValues(t.Name, string(OutcomeNoChange)).Inc()
	obs.RecoveryAttempt(obs.Slot{Host: st.HostID, Hub: st.HubPath},
		obsTier(t), obs.OutcomeFailed)
	return true, nil
}

// reconcileQuarantines keeps farm.device_runtime.health in step with
// farm.quarantines.
//
// It is what makes POST /api/v1/quarantines/{id}/close work: closing the row is
// all an operator does, and the next cycle drops the affected devices back to
// 'unknown' so the watchdog re-observes them from scratch. Going back to
// 'unknown' rather than to 'healthy' is deliberate — nobody has looked at the
// device since it was released, and 'healthy' would be an assumption the
// allocator would act on.
//
// The ladder is reset to rung zero at the same time. An operator who closes a
// quarantine has usually just fixed something, and resuming at rung 7 would
// answer their repair with an adb server restart.
//
// It also resets the ladder for devices that recovered: a healthy device starts
// its next ladder from rung zero.
func (l *Ladder) reconcileQuarantines(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	tag, err := l.cfg.Pool.Exec(cctx, `
UPDATE farm.device_runtime r
   SET health = 'unknown', health_since = now(), ladder_tier = 0, updated_at = now()
  FROM farm.devices d
  LEFT JOIN farm.slots s ON s.id = d.current_slot_id
 WHERE r.device_id = d.id
   AND r.health = 'quarantined'
   AND NOT EXISTS (`+coveredByQuarantine+`)`)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Debug("quarantine reconciliation failed", "err", err)
		}
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		quarantinesCleared.Add(float64(n))
		l.log.Info("released devices whose quarantine was closed; the watchdog re-observes them", "devices", n)
	}

	if _, err := l.cfg.Pool.Exec(cctx, `
UPDATE farm.device_runtime
   SET ladder_tier = 0, updated_at = now()
 WHERE health = 'healthy' AND ladder_tier > 0`); err != nil && ctx.Err() == nil {
		l.log.Debug("ladder reset failed", "err", err)
	}
}

// ---------------------------------------------------------------------------
// One attempt
// ---------------------------------------------------------------------------

func (l *Ladder) attempt(ctx context.Context, c candidate, tiers []tier) {
	t := next(tiers, c.LadderTier)
	log := l.log.With("device", c.DeviceID, "rack_slot", c.RackSlot, "hub", c.HubPath,
		"host", c.HostID, "tier", t.Tier, "tier_name", t.Name)

	radius := t.BlastRadius
	if radius == "power_domain" && c.PowerDomain == nil {
		// An unknown power topology is not a per-port one. Widening to the hub
		// is the conservative reading, and it can only ever cause a refusal.
		radius = "hub"
	}

	// Blast radius against live leases FIRST. A refusal is cheap, and it must
	// not be preceded by anything that changes state.
	refusal, refusalKind, err := l.checkBlastRadius(ctx, c, t, radius)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("could not evaluate the blast radius; not acting", "err", err)
		}
		return
	}
	if refusal != "" {
		l.recordRefusal(ctx, c, t, refusal, refusalKind)
		log.Info("recovery tier refused", "refusal", refusal)
		return
	}

	// The check has just said that every live lease in `radius` permits this
	// tier. For a power_domain rung that is exactly the sentence the agent needs
	// to hear before it will let the target's neighbours go dark, so name them —
	// once, here, after the check and nowhere else. A list gathered before the
	// check would be an assumption; a list gathered for a widened radius would
	// cover a domain nobody enumerated.
	var acknowledged []string
	if radius == "power_domain" && c.PowerDomain != nil {
		acknowledged, err = l.powerDomainSiblings(ctx, c)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("could not enumerate the power domain; "+
					"proceeding without an acknowledgement, which the agent may refuse", "err", err)
			}
			acknowledged = nil
		}
	}

	// Budgets, and the per-device serialisation, in one short transaction.
	attemptID, ok := l.begin(ctx, c, t, radius, acknowledged, log)
	if !ok {
		return
	}

	out, detail := l.perform(ctx, c, t, acknowledged, log)
	l.finish(ctx, attemptID, out, detail, log)

	attemptsTotal.WithLabelValues(t.Name, string(out)).Inc()
	obs.RecoveryAttempt(c.slot(), obsTier(t), obsOutcome(out))
}

// checkBlastRadius asks what the tier would disturb and whether every live
// lease in that scope permits it.
//
// This is the one place the ladder reads farm.leases, and it reads exactly two
// things: that a lease exists, and what disruption_policy it carries. The
// answer can only ever be "act" or "refuse" — there is no branch here that
// ends, shortens, or otherwise touches a lease.
func (l *Ladder) checkBlastRadius(ctx context.Context, c candidate, t tier, radius string) (refusal string, kind obs.RecoveryOutcome, err error) {
	const q = `
SELECT l.id::text AS lease_id, l.job_id::text, d.id::text AS device_id,
       COALESCE(s.rack_slot, '') AS rack_slot,
       l.disruption_policy, l.protected, COALESCE(pd.kind, 'none') AS power_kind
  FROM farm.slots s
  JOIN farm.devices d ON d.current_slot_id = s.id
  JOIN farm.leases  l ON l.id = d.current_lease_id AND l.state IN ('held','suspect')
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
 WHERE ($1::text = 'device'       AND d.id = $2::uuid)
    OR ($1::text = 'power_domain' AND s.power_domain_id = $3::bigint)
    OR ($1::text = 'hub'          AND s.hub_id = $4::bigint)
    OR ($1::text = 'host'         AND s.host_id = $5::text)`

	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	rows, qerr := l.cfg.Pool.Query(cctx, q, radius, c.DeviceID, c.PowerDomain, c.HubID, c.HostID)
	if qerr != nil {
		return "", "", fmt.Errorf("recovery: blast radius query: %w", qerr)
	}
	defer rows.Close()

	required := policyRank(t.RequiresPolicy)
	for rows.Next() {
		var leaseID, jobID, devID, rackSlot, policy, powerKind string
		var protected bool
		if serr := rows.Scan(&leaseID, &jobID, &devID, &rackSlot, &policy, &protected, &powerKind); serr != nil {
			return "", "", fmt.Errorf("recovery: blast radius scan: %w", serr)
		}
		if policyRank(policy) >= required {
			continue
		}

		neighbour := devID != c.DeviceID
		outcome := obs.OutcomeRefusedPolicy
		if neighbour && powerKind == "ganged" {
			// A ganged power domain is the classic case: cutting power to the
			// broken phone also cuts it for the one next to it that is running
			// somebody's six-hour test. A rising rate here means the rack needs
			// per-port switching, not that the ladder is broken.
			outcome = obs.OutcomeRefusedGanged
		}

		who := "this device's own lease"
		if neighbour {
			who = fmt.Sprintf("neighbour %s in the same %s", rackSlot, radius)
		}
		msg := fmt.Sprintf(
			"tier %d (%s) has blast radius %s and requires disruption_policy >= %s, "+
				"but %s (lease %s, job %s) is %s%s",
			t.Tier, t.Name, radius, t.RequiresPolicy, who, leaseID, jobID, policy,
			map[bool]string{true: " and protected", false: ""}[protected])
		return msg, outcome, nil
	}
	if rerr := rows.Err(); rerr != nil {
		return "", "", fmt.Errorf("recovery: blast radius query: %w", rerr)
	}
	return "", "", nil
}

// powerDomainSiblings lists the other positions in c's power domain, as
// devpaths, for Action.Acknowledged.
//
// It selects the SAME set checkBlastRadius just cleared — every slot with this
// power_domain_id — and only drops the target. That correspondence is the whole
// warrant for the acknowledgement: a slot this query returned but the check did
// not cover would be a position going dark on nobody's authority, and a slot the
// check covered but this query missed would be one the agent then refuses over,
// after the ladder has already spent the rung.
//
// It deliberately does not filter on farm.slots.state or on whether the slot
// holds a device. The agent compares against what is PHYSICALLY plugged in right
// now, and a slot the control plane calls inactive can still have a phone in it;
// checkBlastRadius does not filter on state either, so an inactive slot holding a
// live lease is checked like any other. Filtering here would produce a list that
// looks narrower and is not.
func (l *Ladder) powerDomainSiblings(ctx context.Context, c candidate) ([]string, error) {
	// adb_devpath is GENERATED ALWAYS AS ('usb:' || usb_path) over a NOT NULL
	// column, so every row has one and there is nothing to filter out.
	const q = `
SELECT s.adb_devpath
  FROM farm.slots s
 WHERE s.power_domain_id = $1::bigint
   AND s.id <> $2::bigint
 ORDER BY s.adb_devpath`

	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	rows, err := l.cfg.Pool.Query(cctx, q, c.PowerDomain, c.SlotID)
	if err != nil {
		return nil, fmt.Errorf("recovery: power domain siblings: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var devpath string
		if err := rows.Scan(&devpath); err != nil {
			return nil, fmt.Errorf("recovery: power domain siblings scan: %w", err)
		}
		out = append(out, devpath)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recovery: power domain siblings: %w", err)
	}
	return out, nil
}

// recordRefusal writes the refusal to farm.recovery_attempts.refusal.
//
// It is recorded rather than skipped so the UI can explain a gap instead of
// showing one, and so `sum by (outcome) (farm_recovery_attempts_total)` tells an
// operator that their fleet's disruption policies — not the ladder — are why
// devices are staying broken.
func (l *Ladder) recordRefusal(ctx context.Context, c candidate, t tier, refusal string, kind obs.RecoveryOutcome) {
	detail := jsonDetail(map[string]any{
		"blast_radius":    t.BlastRadius,
		"requires_policy": t.RequiresPolicy,
		"health":          c.Health,
	})

	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	if _, err := l.cfg.Pool.Exec(cctx, `
INSERT INTO farm.recovery_attempts
  (device_id, slot_id, hub_id, host_id, tier, finished_at, outcome, refusal, detail)
VALUES ($1::uuid, $2::bigint, $3::bigint, $4::text, $5::int, now(), 'refused', $6::text, $7::jsonb)`,
		c.DeviceID, c.SlotID, c.HubID, c.HostID, t.Tier, refusal, detail); err != nil {
		if ctx.Err() == nil {
			l.log.Warn("could not record a refusal", "device", c.DeviceID, "err", err)
		}
		return
	}
	attemptsTotal.WithLabelValues(t.Name, string(OutcomeRefused)).Inc()
	refusalsTotal.WithLabelValues(t.Name, string(kind)).Inc()
	obs.RecoveryAttempt(c.slot(), obsTier(t), kind)
}

// begin takes the per-device lock, re-checks the budgets under it, opens the
// attempt row and suppresses health writes for the settle window.
//
// The transaction deliberately does NOT wrap the action itself: a uhubctl power
// cycle can take a minute, and holding a database transaction open across it
// would pin a connection and a lock for the duration.
func (l *Ladder) begin(ctx context.Context, c candidate, t tier, radius string, acknowledged []string, log *slog.Logger) (int64, bool) {
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	tx, err := l.cfg.Pool.Begin(cctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("could not begin a recovery attempt", "err", err)
		}
		return 0, false
	}
	defer tx.Rollback(cctx)

	// Serialise attempts on this device across every replica. A transaction
	// lock is used rather than a session lock so it is released by COMMIT even
	// if this process dies holding it.
	if _, err := tx.Exec(cctx, `SELECT pg_advisory_xact_lock($1::int, hashtext($2::text)::int)`,
		l.cfg.LockClass, c.DeviceID); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not take the per-device recovery lock", "err", err)
		}
		return 0, false
	}

	// Cooldown and hourly budget are evaluated in the SCOPE the tier disturbs.
	// A per-device count would let one hub eat eight adb_restarts an hour by
	// spreading them over eight devices.
	const budget = `
WITH att AS (
  SELECT a.started_at
    FROM farm.recovery_attempts a
    LEFT JOIN farm.slots s ON s.id = a.slot_id
   WHERE a.tier = $1::int
     AND a.outcome IS DISTINCT FROM 'refused'
     AND a.started_at > now() - GREATEST($2::interval, interval '1 hour')
     AND ( ($3::text = 'device'       AND a.device_id = $4::uuid)
        OR ($3::text = 'power_domain' AND s.power_domain_id = $5::bigint)
        OR ($3::text = 'hub'          AND a.hub_id = $6::bigint)
        OR ($3::text = 'host'         AND a.host_id = $7::text) )
)
SELECT EXISTS (SELECT 1 FROM att WHERE started_at > now() - $2::interval),
       (SELECT count(*) FROM att WHERE started_at > now() - interval '1 hour'),
       EXISTS (SELECT 1 FROM farm.recovery_attempts a
                WHERE a.device_id = $4::uuid AND a.finished_at IS NULL
                  AND a.started_at > now() - $8::interval)`

	var inCooldown, busy bool
	var inHour int64
	if err := tx.QueryRow(cctx, budget,
		t.Tier, pgInterval(t.Cooldown), radius, c.DeviceID, c.PowerDomain, c.HubID, c.HostID,
		pgInterval(l.cfg.StaleAttempt)).Scan(&inCooldown, &inHour, &busy); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not evaluate the recovery budget", "err", err)
		}
		return 0, false
	}

	switch {
	case busy:
		// An attempt on this device is still open. Two ladders on one phone at
		// once is how a reset lands in the middle of the next reset.
		budgetSkips.WithLabelValues(t.Name, "in_flight").Inc()
		return 0, false
	case inCooldown:
		budgetSkips.WithLabelValues(t.Name, "cooldown").Inc()
		return 0, false
	case int(inHour) >= t.MaxPerHour:
		// Out of budget. Repeating a tier that is not working is how a farm
		// power-cycles a hub forty times an hour.
		budgetSkips.WithLabelValues(t.Name, "max_per_hour").Inc()
		log.Info("recovery tier is out of hourly budget",
			"in_hour", inHour, "max_per_hour", t.MaxPerHour, "scope", radius)
		return 0, false
	}

	// from_tier is the lowest rung this device had NOT yet spent when the
	// attempt opened; the attempt's own tier column says which rung it then
	// spent. acknowledged records exactly which neighbouring positions this
	// attempt was willing to darken, so a cycle that took a rack row down has a
	// list with somebody's reasoning attached rather than a shrug.
	entry := map[string]any{
		"blast_radius": radius,
		"health":       c.Health,
		"from_tier":    c.LadderTier,
		"devpath":      c.Devpath,
	}
	if len(acknowledged) > 0 {
		entry["acknowledged"] = acknowledged
	}
	detail := jsonDetail(entry)

	var id int64
	if err := tx.QueryRow(cctx, `
INSERT INTO farm.recovery_attempts (device_id, slot_id, hub_id, host_id, tier, detail)
VALUES ($1::uuid, $2::bigint, $3::bigint, $4::text, $5::int, $6::jsonb)
RETURNING id`,
		c.DeviceID, c.SlotID, c.HubID, c.HostID, t.Tier, detail).Scan(&id); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not open a recovery attempt", "err", err)
		}
		return 0, false
	}

	// Climb the ladder and hold health steady for the settle window. The
	// suppression is what stops the watchdog from recording an INDUCED
	// transport drop as a fresh fault and re-arming the whole ladder.
	//
	// ladder_tier moves PAST the rung just spent, not onto it: the column names
	// the lowest rung still unspent, which is what makes 0 — the column default,
	// and the value every reset writes — mean "observe first" rather than
	// "observe already happened". See next and rungAfter.
	if _, err := tx.Exec(cctx, `
UPDATE farm.device_runtime
   SET ladder_tier    = $2::int,
       suppress_until = now() + LEAST($3::interval, $4::interval),
       health         = CASE WHEN health IN ('quarantined','retired') THEN health ELSE 'recovering' END,
       health_since   = CASE WHEN health IN ('quarantined','retired','recovering') THEN health_since ELSE now() END,
       updated_at     = now()
 WHERE device_id = $1::uuid`,
		c.DeviceID, rungAfter(t), pgInterval(t.Cooldown), pgInterval(l.cfg.MaxSuppress)); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not mark the device recovering", "err", err)
		}
		return 0, false
	}

	if err := tx.Commit(cctx); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not commit the start of a recovery attempt", "err", err)
		}
		return 0, false
	}
	return id, true
}

// perform runs the rung. The control-plane rungs are done here; everything that
// touches hardware or the host's ADB server goes to the Actuator.
func (l *Ladder) perform(ctx context.Context, c candidate, t tier, acknowledged []string, log *slog.Logger) (Outcome, map[string]any) {
	switch t.Name {
	case "observe":
		// Not a no-op with extra steps: the attempt row and the suppression
		// window written by begin ARE the action. Most blips self-heal inside
		// it, and the ones that do not have now paid a rung for the evidence.
		log.Info("observing for one debounce window before touching anything")
		return OutcomeNoChange, map[string]any{"action": "observe"}

	case "quarantine":
		if err := l.quarantineDevice(ctx, c, t); err != nil {
			if ctx.Err() != nil {
				return OutcomeAborted, map[string]any{"error": err.Error()}
			}
			log.Warn("could not quarantine the device", "err", err)
			return OutcomeFailed, map[string]any{"error": err.Error()}
		}
		log.Error("DEVICE QUARANTINED: the ladder is exhausted and a human is needed",
			"health", c.Health)
		return OutcomeNoChange, map[string]any{"action": "quarantine", "scope": "device"}

	case "host_drain":
		if err := l.drainHost(ctx, c, t); err != nil {
			if ctx.Err() != nil {
				return OutcomeAborted, map[string]any{"error": err.Error()}
			}
			log.Warn("could not open a host quarantine", "err", err)
			return OutcomeFailed, map[string]any{"error": err.Error()}
		}
		log.Error("HOST QUARANTINED: no new leases anywhere on this host; live leases run out untouched",
			"host", c.HostID)
		return OutcomeNoChange, map[string]any{"action": "host_drain", "scope": "host"}
	}

	actx, cancel := context.WithTimeout(ctx, l.cfg.ActionTimeout)
	defer cancel()

	res, err := l.cfg.Actuator.Recover(actx, Action{
		Tier: t.Tier, TierName: t.Name,
		DeviceID: c.DeviceID, SlotID: c.SlotID, Devpath: c.Devpath, RackSlot: c.RackSlot,
		HubID: c.HubID, HubPath: c.HubPath, HostID: c.HostID, ADBEndpoint: c.ADBEndpoint,
		PowerDomainID: c.PowerDomain, Acknowledged: acknowledged,
		Timeout: l.cfg.ActionTimeout,
	})
	switch {
	case err != nil && ctx.Err() != nil:
		return OutcomeAborted, map[string]any{"error": err.Error()}
	case err != nil:
		log.Warn("recovery action failed", "err", err)
		return OutcomeFailed, map[string]any{"error": err.Error()}
	}

	detail := res.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	switch res.Outcome {
	case OutcomeRecovered, OutcomeNoChange, OutcomeFailed:
		// The actuator's verdict is provisional either way: the authority on
		// whether a device is healthy is the watchdog's next observation, which
		// arrives on the track-devices stream after the suppression lapses.
		return res.Outcome, detail
	default:
		detail["actuator_outcome"] = string(res.Outcome)
		return OutcomeFailed, detail
	}
}

// quarantineDevice opens a device-scoped quarantine and marks the device so the
// allocator stops choosing it. It does not touch any lease: a job already on
// this device keeps it and finishes on it.
func (l *Ladder) quarantineDevice(ctx context.Context, c candidate, t tier) error {
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	tx, err := l.cfg.Pool.Begin(cctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(cctx)

	reason := fmt.Sprintf("recovery ladder exhausted at tier %d (%s); device health %s",
		t.Tier, t.Name, c.Health)

	var qid int64
	err = tx.QueryRow(cctx, `
INSERT INTO farm.quarantines (scope, device_id, slot_id, host_id, reason, auto)
SELECT 'device', $1::uuid, $2::bigint, $3::text, $4::text, true
 WHERE NOT EXISTS (
   SELECT 1 FROM farm.quarantines q
    WHERE q.scope = 'device' AND q.device_id = $1::uuid AND q.closed_at IS NULL)
RETURNING id`, c.DeviceID, c.SlotID, c.HostID, reason).Scan(&qid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) && !isSQLState(err, sqlStateUniqueViolation) {
		return err
	}

	if _, err := tx.Exec(cctx, `
UPDATE farm.device_runtime
   SET health = 'quarantined', health_since = now(), suppress_until = NULL, updated_at = now()
 WHERE device_id = $1::uuid AND health <> 'retired'`, c.DeviceID); err != nil {
		return err
	}

	detail := jsonDetail(map[string]any{
		"scope": "device", "quarantine_id": qid, "tier": t.Tier, "health": c.Health,
	})
	if _, err := tx.Exec(cctx, `
INSERT INTO farm.events (kind, device_id, slot_id, actor, detail)
VALUES ('device_quarantined', $1::uuid, $2::bigint, $3::text, $4::jsonb)`,
		c.DeviceID, c.SlotID, l.cfg.Component, detail); err != nil {
		return err
	}
	return tx.Commit(cctx)
}

// drainHost opens a host-scoped quarantine: no new leases anywhere on the host,
// while every live lease on it runs to its own end untouched.
//
// It deliberately does not write farm.hosts.admin_state. Draining a host is an
// audited operator action with its own endpoint (POST /api/v1/hosts/{id}/drain);
// the ladder stops the bleeding and names a human rather than quietly taking a
// whole host out of the fleet under an operator's feet.
func (l *Ladder) drainHost(ctx context.Context, c candidate, t tier) error {
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()

	tx, err := l.cfg.Pool.Begin(cctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(cctx)

	reason := fmt.Sprintf("recovery ladder reached tier %d (%s) on host %s", t.Tier, t.Name, c.HostID)

	var qid int64
	err = tx.QueryRow(cctx, `
INSERT INTO farm.quarantines (scope, host_id, reason, auto)
SELECT 'host', $1::text, $2::text, true
 WHERE NOT EXISTS (
   SELECT 1 FROM farm.quarantines q
    WHERE q.scope = 'host' AND q.host_id = $1::text AND q.closed_at IS NULL)
RETURNING id`, c.HostID, reason).Scan(&qid)
	switch {
	case errors.Is(err, pgx.ErrNoRows), isSQLState(err, sqlStateUniqueViolation):
		return nil // already drained by an earlier cycle
	case err != nil:
		return err
	}

	if _, err := tx.Exec(cctx, `
UPDATE farm.device_runtime r
   SET health = 'quarantined', health_since = now(), updated_at = now()
  FROM farm.devices d, farm.slots s
 WHERE r.device_id = d.id AND d.current_slot_id = s.id
   AND s.host_id = $1::text
   AND r.health NOT IN ('quarantined','retired')`, c.HostID); err != nil {
		return err
	}

	detail := jsonDetail(map[string]any{
		"scope": "host", "quarantine_id": qid, "tier": t.Tier, "host": c.HostID,
	})
	if _, err := tx.Exec(cctx, `
INSERT INTO farm.events (kind, actor, detail)
VALUES ('host_quarantined', $1::text, $2::jsonb)`, l.cfg.Component, detail); err != nil {
		return err
	}
	return tx.Commit(cctx)
}

// finish closes the attempt row. started_at/finished_at bracket exactly the
// action, so the UI can show how long a rung actually took.
func (l *Ladder) finish(ctx context.Context, id int64, out Outcome, detail map[string]any, log *slog.Logger) {
	blob := jsonDetail(detail)

	// Detached from ctx: an attempt that ran must be recorded even when the
	// loop is shutting down, or the next process sees an attempt that never
	// finished and blocks the device for StaleAttempt.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CallTimeout)
	defer cancel()

	if _, err := l.cfg.Pool.Exec(cctx, `
UPDATE farm.recovery_attempts
   SET finished_at = now(), outcome = $2::text, detail = detail || $3::jsonb
 WHERE id = $1::bigint AND finished_at IS NULL`, id, string(out), blob); err != nil {
		l.log.Error("could not record the end of a recovery attempt",
			"attempt", id, "outcome", string(out), "err", err)
		return
	}
	log.Info("recovery attempt finished", "attempt", id, "outcome", string(out))
}

// ---------------------------------------------------------------------------
// Vocabulary helpers
// ---------------------------------------------------------------------------

// policyRank orders farm.jobs.disruption_policy from least to most permissive.
// A tier is allowed when every live lease in its blast radius ranks at or above
// the tier's requires_policy.
func policyRank(p string) int {
	switch p {
	case "no_disruption":
		return 0
	case "allow_soft_reset":
		return 1
	case "allow_port_power_cycle":
		return 2
	default:
		// An unknown policy is treated as the most restrictive one. A future
		// policy value must not become permission by default.
		return 0
	}
}

func tierNamed(tiers []tier, name string) (tier, bool) {
	for _, t := range tiers {
		if t.Name == name {
			return t, true
		}
	}
	return tier{}, false
}

// obsTier maps a ladder rung onto obs's deliberately coarser five-value tier
// label. The exact rung lives in farm.recovery_attempts.tier; the metric label
// stays bounded so a retuned ladder cannot multiply time series.
func obsTier(t tier) obs.RecoveryTier {
	switch t.Name {
	case "observe":
		return obs.TierReprobe
	case "adb_reconnect", "transport_reset":
		return obs.TierReconnect
	case "usb_reset", "device_reboot":
		return obs.TierSoftReset
	case "port_power", "adb_restart":
		return obs.TierPortPowerCycle
	case "quarantine", "host_drain":
		return obs.TierQuarantine
	default:
		return obs.RecoveryTierFromLadder(t.Tier)
	}
}

// obsOutcome folds this package's five outcomes onto obs's set.
//
// no_change folds to failed on purpose: obs's rule is never to record a
// recovery that cannot be proven, and "the action ran and nothing changed" is
// not a recovery. A false "recovered" would suppress the page that should
// follow.
func obsOutcome(o Outcome) obs.RecoveryOutcome {
	switch o {
	case OutcomeRecovered:
		return obs.OutcomeRecovered
	default:
		return obs.OutcomeFailed
	}
}

// jsonDetail renders a detail blob for a ::jsonb parameter.
//
// It is total, and it always returns a jsonb OBJECT. Both properties matter at
// the two call sites that concatenate:
//
//   - A discarded marshal error yields "", which a ::jsonb cast rejects, so the
//     INSERT fails and the forensic record is lost along with the reason it
//     could not be written.
//   - A nil map marshals to "null", and `detail || 'null'::jsonb` does not
//     error — it silently rewrites the row's object into a two-element ARRAY,
//     after which every detail->>'key' read in the API and the UI returns NULL.
//     A record that quietly stops answering questions is worse than one that
//     was never written.
func jsonDetail(m map[string]any) string {
	if m == nil {
		return `{}`
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"detail_marshal_error": ` + strconv.Quote(err.Error()) + `}`
	}
	return string(b)
}

// pgInterval renders a duration as a Postgres interval literal in exact
// microseconds. It is a DURATION and never an instant: nothing here tells
// Postgres what time this pod thinks it is.
func pgInterval(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(int64(d/time.Microsecond), 10) + " microseconds"
}

const sqlStateUniqueViolation = "23505"

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	cyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "cycles_total",
		Help: "Recovery ladder cycles.",
	})

	// attemptsTotal keeps the database's exact vocabulary — nine tiers, five
	// outcomes — without the physical-position labels obs carries, so the
	// cardinality stays at tiers x outcomes.
	attemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "attempts_total",
		Help: "Recovery attempts by tier name and farm.recovery_attempts.outcome. " +
			"outcome=refused means a live lease's disruption_policy forbade the tier.",
	}, []string{"tier", "outcome"})

	refusalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "refusals_total",
		Help: "Tiers refused because a live lease in the blast radius forbade them. " +
			"kind=refused_ganged means the rack needs per-port power switching.",
	}, []string{"tier", "kind"})

	budgetSkips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "budget_skips_total",
		Help: "Attempts not made because of a cooldown, an hourly cap, or an attempt already " +
			"in flight on that device.",
	}, []string{"tier", "reason"})

	hubQuarantines = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "hub_quarantines_total",
		Help: "Hub-scoped quarantines opened from correlated failure — one alert instead of N.",
	})

	quarantinesCleared = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "quarantines_cleared_total",
		Help: "Devices returned to 'unknown' health after their quarantine was closed.",
	})

	candidatesGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "candidates",
		Help: "Unhealthy, settled, unquarantined devices the ladder considered last cycle.",
	})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "recovery", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. Visible to operators; deliberately NOT part of " +
			"farm.reaper_arm's gap accounting, which would let health move lease clocks.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		cyclesTotal, attemptsTotal, refusalsTotal, budgetSkips,
		hubQuarantines, quarantinesCleared, candidatesGauge, beatFailures,
	}
}
