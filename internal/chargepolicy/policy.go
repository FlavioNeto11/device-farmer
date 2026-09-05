// Package chargepolicy holds idle devices inside a band of state of charge.
//
// # Why this loop exists
//
// A rack of lithium cells sitting at 100% is the fire hazard REQUIREMENTS.md
// calls HW-03, and charge limiting is the one mitigation for it that software
// can reach: a cell at 80% stores materially less energy than one at 100%, and
// the only way to stop a phone charging over USB is to take VBUS away from its
// port. The agent-side half of that — a VBUS set-point the host agent holds
// with a dead-man's switch that fails towards ON — is internal/node's charge
// gate. The parked state in migrations/00008_parked.sql is what lets a device
// be held dark on purpose without the watchdog calling it missing and the
// recovery ladder power-cycling it. This package is the loop between them.
//
// # THE RULE THIS LOOP IS BUILT AROUND
//
// The policy acts ONLY on devices with no live lease, and it never touches
// farm.leases. A lease ends when the job says so, when a deadline the user
// wrote down elapses, or when a human takes it back — never because a battery
// was full. So a leased device is invisible here whatever its charge, and a
// device that acquires a lease while held is released and forgotten within one
// cycle. The scheduler never allocates a parked device, so a lease appearing on
// one means a human unparked it: the human's decision wins, the gate comes off.
//
// The barrier is structural rather than promised: this package does not
// import internal/lease, its heartbeat is one SQL statement, and farm.leases
// appears in this file only as `d.current_lease_id IS NOT NULL` — a read of the
// trigger-maintained mirror on farm.devices, never of the table itself.
//
// # What a hold is
//
// A hold is a park plus a gate. farm.device_park(auto => true) takes the device
// out of the allocator's and the ladder's sight under this loop's own actor,
// and then the agent is asked to drive the port's VBUS off for two intervals.
// Every cycle re-asserts that gate; the renewal IS the proof that this loop is
// alive, and if it stops, the agent restores power on its own. A park without a
// gate is the worst of both — an unschedulable phone that is charging — so the
// loop reconciles the two every cycle against what the agent says it actually
// holds, and closes any park of its own whose gate it cannot re-assert.
//
// Automation reverses only its own decision. farm.device_unpark(auto => true)
// refuses to close a park a human opened, and this loop never tries: a device
// under somebody else's park is left exactly as they left it, including a
// gate this loop may once have held there, which is released. A human who
// closes one of this loop's parks gets a cooldown in which the device is not
// parked again, long enough to run a job or carry the phone to a bench.
//
// # A held phone is a phone nobody can see
//
// Cutting VBUS takes the device off the USB bus entirely, so while a gate is
// held the watchdog cannot read its battery: battery_pct freezes at the value
// that earned the hold. "Release when it drops below the floor" is therefore
// not something this loop can observe from a dark port. What it does instead
// is peek. After PeekEvery of darkness the gate is released with the park kept
// in place, the device re-enumerates, the watchdog's battery reader takes a
// fresh sample, and PeekFor later the loop reads it: at or below the floor the
// hold ends and the device goes back into service; above it the ledger is
// closed and reopened — so the park carries the reading and the dark clock
// restarts — and the gate is asserted again. A peek charges a phone for a few
// minutes every several hours, which an idle handset more than pays back.
//
// # Ganged power domains
//
// On a hub without per-port switching, one gate darkens every port in the
// domain, so the unit of decision there is the domain and not the device.
// Every device in it must be idle, unparked by anyone else, and have a battery
// reading; every one of them is parked under this loop's actor before the gate
// is asserted, and the agent is handed every sibling position as acknowledged.
// A ganged domain is held while its HIGHEST device is over the ceiling and
// released when its LOWEST reaches the floor — the honest best a shared switch
// allows. docs/hub-validation.md is right that such hubs are the wrong hardware
// for this; the loop does what it can with them rather than nothing.
package chargepolicy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/node"
)

// Defaults. None of them can end a lease, so the worst a bad value here can do
// is hold a phone a little longer or release it a little sooner.
const (
	// DefaultComponent is the name written to farm.component_heartbeat.
	//
	// It must NOT be added to FARM_REAPER_COMPONENTS: this loop is on the
	// health side of the firewall, and its downtime must not move a lease
	// clock. The beat exists so an operator can see whether the policy is
	// running, and for nothing else.
	DefaultComponent = "chargepolicy"

	// DefaultActor is written to farm.device_parks.opened_by. It is a constant
	// and not the component name on purpose: farm.device_unpark lets
	// automation close only a park opened by the SAME actor, so a policy
	// renamed with FARM_COMPONENT, or a replica that took over leadership,
	// must still recognise the parks its predecessor opened.
	DefaultActor = "chargepolicy"

	DefaultMinPct   = 40
	DefaultMaxPct   = 80
	DefaultInterval = 2 * time.Minute

	// DefaultCallTimeout bounds every individual database call. A wedged
	// statement must not wedge the loop; a wedged loop stops renewing, and the
	// agent then restores power to every held port.
	DefaultCallTimeout = 15 * time.Second

	// DefaultPeekEvery is how long a port stays dark between two readings of
	// the battery behind it. Long on purpose: an idle handset loses about a
	// percent an hour with its screen off, and a peek charges it for a few
	// minutes, so a peek every few hours nets a slow discharge and a peek
	// every thirty minutes would net a slow CHARGE — the loop would then hold
	// a device at the ceiling forever while believing it was lowering it.
	DefaultPeekEvery = 6 * time.Hour

	// DefaultPeekFor is how long a peek waits before it trusts the battery
	// column again. The watchdog's battery reader polls once a minute
	// (watchdog.DefaultBatteryInterval); three minutes covers re-enumeration,
	// one poll, and a policy interval of slack.
	DefaultPeekFor = 3 * time.Minute

	// DefaultHumanCooldown is how long a device stays unparked after a human
	// closed one of this loop's parks. The reading that earned the park is
	// still on the row — the device was dark, nothing refreshed it — so without
	// a cooldown the next cycle would park it again two minutes later, and the
	// human's decision would be undone before the phone was even warm.
	DefaultHumanCooldown = 30 * time.Minute

	// DefaultLockKey is the pg_try_advisory_lock key that elects the single
	// active policy loop. It spells "farmChrg" in ASCII so an operator reading
	// pg_locks can tell whose lock it is.
	DefaultLockKey int64 = 0x6661726d43687267

	// reasonPrefix marks every gate this loop asserts. The agent records who
	// asked for nothing — only why — so the reason is how this loop tells its
	// own gates from one an operator placed by hand, which it never releases.
	reasonPrefix = "charge policy: "
)

// GateClient is the slice of node.Client this loop needs. It is an interface
// so a test can stand a stub agent behind it; the shipped binary always hands
// in the real client.
type GateClient interface {
	SetChargeGate(ctx context.Context, req node.ChargeGateRequest) (node.ChargeGate, error)
	ReleaseChargeGate(ctx context.Context, hostID, devpath, reason string) (node.ChargeGate, error)
	ChargeGates(ctx context.Context, hostID string) ([]node.ChargeGate, error)
}

var _ GateClient = (*node.Client)(nil)

// Config is the loop's wiring. Pool is required.
type Config struct {
	// Pool is used for the cycle AND for the dedicated leadership connection,
	// so it must allow at least two connections.
	Pool *pgxpool.Pool

	// Gates reaches the host agents. Nil is allowed and means the loop can
	// observe but not act: it beats, counts every device it would have held
	// under a skip reason of its own, and closes any park it cannot back with
	// a gate. That is the shape a farm takes with no FARM_NODE_TOKEN, and it
	// is deliberately a running loop rather than a refused one, so that `all`
	// and `demo` still start and the metric says what is missing.
	Gates GateClient

	// Component is the farm.component_heartbeat key. Defaults to
	// DefaultComponent. See that constant before renaming it.
	Component string
	// Actor is written to farm.device_parks.opened_by. Defaults to
	// DefaultActor; see that constant before changing it.
	Actor string

	MinPct      int
	MaxPct      int
	Interval    time.Duration
	CallTimeout time.Duration

	PeekEvery     time.Duration
	PeekFor       time.Duration
	HumanCooldown time.Duration

	// LockKey is the advisory lock key for leader election. Every replica
	// must use the same value or leader election elects everyone.
	LockKey int64

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.Actor == "" {
		c.Actor = DefaultActor
	}
	if c.MinPct == 0 && c.MaxPct == 0 {
		c.MinPct, c.MaxPct = DefaultMinPct, DefaultMaxPct
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.PeekEvery <= 0 {
		c.PeekEvery = DefaultPeekEvery
	}
	if c.PeekFor <= 0 {
		c.PeekFor = DefaultPeekFor
	}
	if c.HumanCooldown <= 0 {
		c.HumanCooldown = DefaultHumanCooldown
	}
	if c.LockKey == 0 {
		c.LockKey = DefaultLockKey
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Policy is the loop. Construct it with New and run it with Run.
type Policy struct {
	cfg Config
	log *slog.Logger

	// hold is what every off-gate is asserted for: two intervals, so one
	// missed cycle costs nothing, capped where the agent would refuse it.
	hold time.Duration

	lead leadership

	// peeks is when each unit's current peek began, by unit key. It is the
	// one piece of state that lives only in memory, and losing it costs at
	// most PeekFor of extra charging after a restart: a peek this process
	// does not remember starting is treated as starting now.
	peeks map[string]time.Time

	// warned keeps once-per-condition log lines from repeating every cycle.
	// A condition that clears is forgotten, so it is warned about again if
	// it returns.
	warned map[string]struct{}
}

// New validates cfg and returns a Policy.
func New(cfg Config) (*Policy, error) {
	if cfg.Pool == nil {
		return nil, errors.New("chargepolicy: Config.Pool is required")
	}
	if conns := cfg.Pool.Config().MaxConns; conns < 2 {
		return nil, fmt.Errorf("chargepolicy: Config.Pool allows %d connection(s); leader "+
			"election holds one for the life of the process, so the cycle needs at least "+
			"one more", conns)
	}
	cfg.applyDefaults()
	if cfg.MinPct <= 0 || cfg.MaxPct > 100 || cfg.MinPct >= cfg.MaxPct {
		return nil, fmt.Errorf("chargepolicy: the band must satisfy 0 < min < max <= 100, "+
			"got min %d max %d", cfg.MinPct, cfg.MaxPct)
	}
	if cfg.Interval*2 > node.MaxChargeGateHold {
		return nil, fmt.Errorf("chargepolicy: Config.Interval %s is above half the agent's "+
			"hold cap %s; a gate is asserted for two intervals so one missed cycle does "+
			"not hand the port back", cfg.Interval, node.MaxChargeGateHold)
	}
	return &Policy{
		cfg:    cfg,
		log:    cfg.Logger.With("component", cfg.Component),
		hold:   min(2*cfg.Interval, node.MaxChargeGateHold),
		lead:   leadership{pool: cfg.Pool, key: cfg.LockKey, log: cfg.Logger},
		peeks:  make(map[string]time.Time),
		warned: make(map[string]struct{}),
	}, nil
}

// Run drives the loop until ctx is cancelled.
//
// It returns nil on cancellation. Nothing is released on the way out: a held
// gate outlives this process by at most one hold, after which the agent
// restores power by itself — that is the dead-man's switch doing its job, and
// the replacement re-asserts whatever still deserves holding on its first
// cycle. Releasing here would turn every rolling deploy into a fleet-wide
// charging window.
func (p *Policy) Run(ctx context.Context) error {
	defer p.lead.release(ctx)
	defer leaderGauge.Set(0)

	p.log.Info("charge policy starting",
		"min_pct", p.cfg.MinPct, "max_pct", p.cfg.MaxPct,
		"interval", p.cfg.Interval, "hold", p.hold,
		"peek_every", p.cfg.PeekEvery, "gates", p.cfg.Gates != nil)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("charge policy stopping; held gates expire on the agent's clock")
			return nil
		case <-timer.C:
		}

		p.cycle(ctx)
		timer.Reset(jitter(p.cfg.Interval))
	}
}

// ---------------------------------------------------------------------------
// One cycle
// ---------------------------------------------------------------------------

// device is one row of the candidate scan: a device, where it sits, what the
// runtime row says about its charge, and whether anybody else has a claim on it.
type device struct {
	ID      string
	HostID  string
	Devpath string // empty when the device has no slot
	SlotID  int64

	DomainID      int64 // 0 when the slot has no power domain
	DomainKind    string
	DomainControl string
	HasAgent      bool

	Battery    int // -1 when never observed
	ChargeGate string
	Leased     bool
	AdminState string

	Parked   bool
	ParkAuto bool
	ParkedBy string
	// HoldDue is true when this loop's park on the device is old enough for a
	// peek. Computed against the server's clock, never this process's.
	HoldDue bool
	// HumanUnparked is true when a human closed one of this loop's parks on
	// this device inside the cooldown.
	HumanUnparked bool
}

// unit is what one decision is made about: a single device on a per-port
// domain, or every device on a ganged one.
type unit struct {
	Key      string
	HostID   string
	Ganged   bool
	Devices  []device
	Siblings []string // every other position in a ganged domain, for Acknowledged

	HasClient bool

	// Held is the gate this loop holds on one of the unit's positions, as the
	// AGENT reports it. Nil when it holds none.
	Held *node.ChargeGate
	// ForeignGate is true when somebody else's gate sits on one of the unit's
	// positions. This loop never asserts over another party's hold.
	ForeignGate bool

	// Peeking is true when this loop released the gate deliberately to read
	// the battery, and PeekSettled when that peek has waited long enough.
	Peeking     bool
	PeekSettled bool
}

func (u unit) devpaths() []string {
	out := make([]string, 0, len(u.Devices))
	for _, d := range u.Devices {
		if d.Devpath != "" {
			out = append(out, d.Devpath)
		}
	}
	return out
}

func (u unit) ids() []string {
	out := make([]string, 0, len(u.Devices))
	for _, d := range u.Devices {
		out = append(out, d.ID)
	}
	return out
}

// verdict is what a unit's decision came to.
type verdict int

const (
	verdictNone verdict = iota
	// verdictGate parks every device in the unit and asserts a new gate.
	verdictGate
	// verdictRegate re-asserts a gate under parks that are already open —
	// after an agent restart, an expiry, or a peek that read a battery still
	// above the floor.
	verdictRegate
	// verdictRenew re-asserts a gate the agent still holds.
	verdictRenew
	// verdictPeek releases the gate, keeps the parks, and starts the clock on
	// a fresh battery reading.
	verdictPeek
	// verdictRelease releases the gate if one is held and closes this loop's
	// parks in the unit.
	verdictRelease
	// verdictSkip records that a device earned a hold this loop could not
	// place, under a reason an operator can count.
	verdictSkip
)

// plan is a verdict with the addresses it needs.
type plan struct {
	verdict verdict
	// reason is the skip label, or the text carried into the agent's gate
	// reason and the park ledger for every other verdict.
	reason string
	anchor string
	ack    []string
	// park lists devices to park before a gate goes on; unpark lists this
	// loop's parks to close.
	park   []string
	unpark []string
	// reopen is set on a Regate that follows a peek: the parks are closed
	// and opened again so the ledger carries the reading and the dark clock
	// restarts from now.
	reopen bool
}

// thresholds is the band, plus the actor whose parks count as this loop's.
type thresholds struct {
	actor    string
	min, max int
}

// decide is the whole policy, as a pure function of one unit's state.
//
// The order of the clauses is the order of precedence, and it is the same
// order the package comment states: anybody else's claim on a device comes
// first and always ends in the gate coming off; then whether a gate can be
// held here at all; then the hold in progress; and only last the question of
// whether an idle, unclaimed device has earned one.
func decide(u unit, th thresholds) plan {
	var (
		ours        []string
		fresh       []string // enabled and unparked, to be parked with the unit
		leased      bool
		foreign     bool
		unknown     bool
		cooldown    bool
		holdDue     bool
		maxPct      = -1
		minPct      = 101
		hottest     string
		firstDev    string
		unswitch    bool
		anyOverCeil bool
	)
	for _, d := range u.Devices {
		if d.Leased {
			leased = true
		}
		switch {
		case d.Parked && d.ParkAuto && d.ParkedBy == th.actor:
			ours = append(ours, d.ID)
			holdDue = holdDue || d.HoldDue
		case d.Parked, d.AdminState != "enabled":
			foreign = true
		default:
			fresh = append(fresh, d.ID)
		}
		if d.Battery < 0 {
			unknown = true
		} else {
			if d.Battery > maxPct {
				maxPct, hottest = d.Battery, d.Devpath
			}
			minPct = min(minPct, d.Battery)
		}
		if d.HumanUnparked {
			cooldown = true
		}
		if firstDev == "" {
			firstDev = d.Devpath
		}
		if d.DomainKind == "none" || d.DomainControl == "none" {
			unswitch = true
		}
	}
	anyOverCeil = maxPct > th.max
	if hottest == "" {
		hottest = firstDev
	}
	heldAt := ""
	if u.Held != nil {
		heldAt = u.Held.Devpath
	}

	// standDown is the shape every "hands off" answer takes: whatever this
	// loop holds here comes off, and whatever it would have done is counted.
	standDown := func(reason string) plan {
		if u.Held != nil || len(ours) > 0 {
			return plan{verdict: verdictRelease, reason: reason, anchor: heldAt, unpark: ours}
		}
		if anyOverCeil {
			return plan{verdict: verdictSkip, reason: reason}
		}
		return plan{verdict: verdictNone}
	}

	// 1. Somebody else's claim.
	switch {
	case leased:
		return standDown("leased")
	case foreign:
		return standDown("foreign_park")
	case u.ForeignGate:
		return standDown("foreign_gate")
	}

	// 2. No way to hold a port here.
	switch {
	case !u.HasClient:
		return standDown("no_client")
	case firstDev == "":
		return standDown("no_slot")
	case !u.Devices[0].HasAgent:
		return standDown("no_agent")
	case unswitch:
		return standDown("unswitchable")
	}

	// 3. A hold in progress.
	if len(ours) > 0 {
		// The floor is checked before anything else about the hold, whatever
		// produced the reading: a peek that settled, a hub that keeps its
		// data lines up with VBUS off, or a human who plugged the phone into
		// a bench charger and back. A reading at or below the floor is the
		// one fact that ends a hold, and it is not made to wait for the next
		// peek to be believed. On a ganged domain the LOWEST device sets it.
		if !unknown && minPct <= th.min {
			return plan{verdict: verdictRelease, unpark: ours, anchor: heldAt,
				reason: fmt.Sprintf("%d%% is at or below %d%%", minPct, th.min)}
		}
		if u.Held != nil {
			if holdDue {
				return plan{verdict: verdictPeek, reason: "peek", anchor: heldAt}
			}
			return plan{verdict: verdictRenew, anchor: heldAt, ack: u.Siblings, park: fresh,
				reason: fmt.Sprintf("holding at %d%%", maxPct)}
		}
		if u.Peeking {
			if !u.PeekSettled {
				return plan{verdict: verdictNone}
			}
			// The settled peek read a battery still above the floor — the
			// release at the top of this branch would otherwise have taken
			// it. A device that answered nothing during the peek is held
			// again too: the reading that earned the hold is the only one
			// there is, and the safe direction for a reading nobody could
			// refresh is dark.
			why := fmt.Sprintf("peek read %d%%, above %d%%", minPct, th.min)
			if unknown {
				why = "peek read no battery; holding again"
			}
			return plan{verdict: verdictRegate, anchor: hottest, ack: u.Siblings,
				park: fresh, unpark: ours, reopen: true, reason: why}
		}
		// The agent no longer holds what the ledger says it should: it
		// restarted, or this loop missed enough cycles for the dead-man's
		// switch. Re-assert rather than churn the ledger; a re-assertion the
		// agent refuses closes the park in execute.
		return plan{verdict: verdictRegate, anchor: hottest, ack: u.Siblings, park: fresh,
			reason: fmt.Sprintf("re-asserting a dropped gate at %d%%", maxPct)}
	}

	// 4. Idle, enabled, and nobody's.
	if u.Held != nil {
		// A gate with no park under it: a human closed this loop's park and
		// the port is still dark. The park was the human's to close; the gate
		// follows it.
		return plan{verdict: verdictRelease, reason: "park_closed", anchor: heldAt}
	}
	if !anyOverCeil {
		return plan{verdict: verdictNone}
	}
	if u.Ganged && unknown {
		return plan{verdict: verdictSkip, reason: "unknown_battery"}
	}
	if cooldown {
		return plan{verdict: verdictSkip, reason: "human_unparked"}
	}
	return plan{verdict: verdictGate, anchor: hottest, ack: u.Siblings, park: fresh,
		reason: fmt.Sprintf("%d%% is above %d%%", maxPct, th.max)}
}

// cycle runs one pass of the policy over the whole fleet.
func (p *Policy) cycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	cyclesTotal.Inc()
	p.beat(ctx)

	leader, err := p.lead.ensure(ctx, p.cfg.CallTimeout)
	if err != nil {
		p.log.Warn("charge policy leadership check failed; not acting this cycle", "err", err)
		leaderGauge.Set(0)
		return
	}
	if !leader {
		leaderGauge.Set(0)
		return
	}
	leaderGauge.Set(1)

	devices, err := p.scan(ctx)
	if err != nil {
		p.failed(ctx, "scan", "could not list the fleet; acting on nothing this cycle", "err", err)
		return
	}
	siblings, err := p.gangedSiblings(ctx)
	if err != nil {
		p.failed(ctx, "siblings", "could not list ganged domains; acting on nothing this cycle", "err", err)
		return
	}

	units := group(devices, siblings)
	parked := 0
	for _, d := range devices {
		if d.Parked && d.ParkAuto && d.ParkedBy == p.cfg.Actor {
			parked++
		}
	}
	parkedGauge.Set(float64(parked))

	// One listing per host that can be asked. The agent's answer is the only
	// truth about which ports are dark right now; the database only knows
	// what was asked for.
	gates := p.listGates(ctx, units)

	touched := make(map[string]map[string]bool) // host -> devpath -> renewed or released this cycle
	th := thresholds{actor: p.cfg.Actor, min: p.cfg.MinPct, max: p.cfg.MaxPct}
	for _, u := range units {
		if ctx.Err() != nil {
			return
		}
		u.HasClient = p.cfg.Gates != nil
		hg, ok := gates[u.HostID]
		if u.HasClient && u.Devices[0].HasAgent && !ok {
			// The agent did not answer. Nothing is known about its ports, so
			// nothing is decided about them; its dead-man's switch is what
			// protects a phone this loop cannot renew.
			if unitNeedsAction(u, th) {
				skippedTotal.WithLabelValues("agent_unreachable").Inc()
			}
			continue
		}
		p.attachGates(&u, hg)
		p.attachPeek(&u)

		pl := decide(u, th)
		p.execute(ctx, u, pl, touched)
	}

	p.releaseStrays(ctx, gates, touched)
}

// unitNeedsAction reports whether a unit would have produced a plan other
// than None, so a skipped host is counted once per unit that mattered.
func unitNeedsAction(u unit, th thresholds) bool {
	for _, d := range u.Devices {
		if d.Battery > th.max || (d.Parked && d.ParkAuto && d.ParkedBy == th.actor) {
			return true
		}
	}
	return false
}

// attachGates finds the gate this loop holds on the unit, if any, and notes a
// gate that is somebody else's.
func (p *Policy) attachGates(u *unit, held []node.ChargeGate) {
	mine := make(map[string]bool)
	for _, d := range u.devpaths() {
		mine[d] = true
	}
	for i := range held {
		g := held[i]
		if !g.Held || !mine[g.Devpath] {
			continue
		}
		if !strings.HasPrefix(g.Reason, reasonPrefix) {
			u.ForeignGate = true
			continue
		}
		if u.Held == nil {
			u.Held = &g
		}
	}
}

// attachPeek reads the peek state for a unit whose parks are this loop's and
// whose gate the agent does not hold: charge_gate 'on' on every parked device
// means the release was deliberate.
func (p *Policy) attachPeek(u *unit) {
	if u.Held != nil {
		delete(p.peeks, u.Key)
		return
	}
	n := 0
	for _, d := range u.Devices {
		if !(d.Parked && d.ParkAuto && d.ParkedBy == p.cfg.Actor) {
			continue
		}
		if d.ChargeGate != node.ChargePowerOn {
			delete(p.peeks, u.Key)
			return
		}
		n++
	}
	if n == 0 {
		delete(p.peeks, u.Key)
		return
	}
	u.Peeking = true
	since, ok := p.peeks[u.Key]
	if !ok {
		since = time.Now()
		p.peeks[u.Key] = since
	}
	u.PeekSettled = time.Since(since) >= p.cfg.PeekFor
}

// execute carries out one plan and records what came of it.
func (p *Policy) execute(ctx context.Context, u unit, pl plan, touched map[string]map[string]bool) {
	mark := func(devpath string) {
		if devpath == "" {
			return
		}
		if touched[u.HostID] == nil {
			touched[u.HostID] = make(map[string]bool)
		}
		touched[u.HostID][devpath] = true
	}

	switch pl.verdict {
	case verdictNone:
		return

	case verdictSkip:
		skippedTotal.WithLabelValues(pl.reason).Inc()
		p.warnOnce("skip/"+u.Key,
			"a device is over the charge ceiling and cannot be held",
			"host", u.HostID, "devpaths", u.devpaths(), "reason", pl.reason)
		return

	case verdictGate, verdictRegate:
		if pl.reopen {
			// Close and reopen so the ledger records the reading and the
			// dark clock restarts. A device the scheduler takes in the
			// millisecond between is a leased device, and next cycle sees it.
			if !p.unparkAll(ctx, pl.unpark, pl.reason) {
				return
			}
			pl.park = append(pl.park, pl.unpark...)
		}
		if !p.parkAll(ctx, u, pl.park, pl.reason) {
			return
		}
		mark(pl.anchor)
		if err := p.assert(ctx, u, pl); err != nil {
			// A park with no gate under it is an unschedulable phone that is
			// charging. Undo the park rather than leave that shape behind.
			toClose := pl.park
			if pl.verdict == verdictRegate && !pl.reopen {
				toClose = append(toClose, ourParks(u, p.cfg.Actor)...)
			}
			p.unparkAll(ctx, toClose, "the charge gate could not be asserted: "+errText(err))
			p.noteFailure(ctx, u, err)
			return
		}
		p.forgetWarn("gate/" + u.Key)
		p.forgetWarn("skip/" + u.Key)
		delete(p.peeks, u.Key)
		p.writeGate(ctx, u.ids(), node.ChargePowerOff)

	case verdictRenew:
		if !p.parkAll(ctx, u, pl.park, pl.reason) {
			return
		}
		mark(pl.anchor)
		err := p.assert(ctx, u, pl)
		if err == nil {
			p.forgetWarn("gate/" + u.Key)
			p.writeGate(ctx, u.ids(), node.ChargePowerOff)
			return
		}
		if errors.Is(err, node.ErrUnreachable) {
			// The hold is still in force on the agent's clock. Say the port's
			// state is unknown and try again next cycle; the dead-man's
			// switch covers the gap if the agent stays away.
			p.writeGate(ctx, u.ids(), node.ChargePowerUnknown)
			return
		}
		// Refused: the agent leaves the old hold running until it expires,
		// and the reason will not change by asking again. End the hold now
		// rather than in four minutes with the park still open.
		p.releaseGate(ctx, u, pl.anchor, "renewal refused: "+errText(err))
		p.unparkAll(ctx, ourParks(u, p.cfg.Actor), "the charge gate was refused: "+errText(err))
		p.writeGate(ctx, u.ids(), node.ChargePowerOn)

	case verdictPeek:
		mark(pl.anchor)
		if err := p.releaseGate(ctx, u, pl.anchor, "peek: re-reading the battery"); err != nil {
			p.noteFailure(ctx, u, err)
			return
		}
		actionsTotal.WithLabelValues("peek", "ok").Inc()
		p.peeks[u.Key] = time.Now()
		p.writeGate(ctx, u.ids(), node.ChargePowerOn)

	case verdictRelease:
		if pl.anchor != "" {
			mark(pl.anchor)
			if err := p.releaseGate(ctx, u, pl.anchor, pl.reason); err != nil {
				// Keep the parks: a device still dark must not be handed to
				// the scheduler. The switch restores power inside one hold,
				// and next cycle's listing says so.
				p.noteFailure(ctx, u, err)
				return
			}
		}
		delete(p.peeks, u.Key)
		p.unparkAll(ctx, pl.unpark, pl.reason)
		p.writeGate(ctx, u.ids(), node.ChargePowerOn)
	}
}

// ourParks lists the devices in a unit under this loop's park.
func ourParks(u unit, actor string) []string {
	var out []string
	for _, d := range u.Devices {
		if d.Parked && d.ParkAuto && d.ParkedBy == actor {
			out = append(out, d.ID)
		}
	}
	return out
}

// assert drives the anchor port dark for one hold.
func (p *Policy) assert(ctx context.Context, u unit, pl plan) error {
	action := "gate"
	if pl.verdict == verdictRenew {
		action = "renew"
	}
	req := node.ChargeGateRequest{
		HostID:       u.HostID,
		Devpath:      pl.anchor,
		Power:        node.ChargePowerOff,
		HoldSeconds:  p.hold.Seconds(),
		Acknowledged: pl.ack,
		Reason:       reasonPrefix + pl.reason,
	}
	cctx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout+node.DefaultResolveTimeout)
	defer cancel()
	_, err := p.cfg.Gates.SetChargeGate(cctx, req)
	actionsTotal.WithLabelValues(action, outcomeOf(err)).Inc()
	if err != nil {
		if ctx.Err() == nil {
			p.warnOnce("gate/"+u.Key, "the charge gate was not applied",
				"host", u.HostID, "devpath", pl.anchor, "action", action, "err", err)
		}
		return err
	}
	if pl.verdict != verdictRenew {
		p.log.Info("charge gate asserted", "host", u.HostID, "devpath", pl.anchor,
			"hold", p.hold, "acknowledged", len(pl.ack), "reason", pl.reason)
	}
	return nil
}

// releaseGate returns a port to its powered state.
func (p *Policy) releaseGate(ctx context.Context, u unit, devpath, reason string) error {
	cctx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout+node.DefaultResolveTimeout)
	defer cancel()
	_, err := p.cfg.Gates.ReleaseChargeGate(cctx, u.HostID, devpath, reasonPrefix+reason)
	actionsTotal.WithLabelValues("release", outcomeOf(err)).Inc()
	if err != nil {
		if ctx.Err() == nil {
			p.warnOnce("release/"+u.Key, "the charge gate could not be released; the agent's "+
				"own deadline will restore power if it stays that way",
				"host", u.HostID, "devpath", devpath, "err", err)
		}
		return err
	}
	p.forgetWarn("release/" + u.Key)
	p.log.Info("charge gate released", "host", u.HostID, "devpath", devpath, "reason", reason)
	return nil
}

// parkAll opens this loop's park on each device. It stops at the first
// failure and closes what it opened, so a unit is either wholly parked or not
// parked at all — on a ganged domain a half-parked unit would leave a device
// schedulable on a port about to go dark.
func (p *Policy) parkAll(ctx context.Context, u unit, ids []string, reason string) bool {
	var opened []string
	for _, id := range ids {
		cctx, cancel := p.db(ctx)
		_, err := p.cfg.Pool.Exec(cctx,
			`SELECT farm.device_park($1::uuid, $2::text, $3::text, true)`,
			id, p.cfg.Actor, reasonPrefix+reason)
		cancel()
		actionsTotal.WithLabelValues("park", outcomeOf(err)).Inc()
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("could not park a device for charge limiting; the unit is left "+
					"unparked", "host", u.HostID, "device", id, "err", err)
			}
			p.unparkAll(ctx, opened, "a neighbour could not be parked")
			return false
		}
		opened = append(opened, id)
	}
	return true
}

// unparkAll closes this loop's park on each device. A refusal means the park
// is no longer this loop's — a human took it over between the scan and now —
// and that is logged, not retried: next cycle's scan shows the human's park
// and the loop keeps its hands off.
func (p *Policy) unparkAll(ctx context.Context, ids []string, reason string) bool {
	ok := true
	for _, id := range ids {
		cctx, cancel := p.db(ctx)
		_, err := p.cfg.Pool.Exec(cctx,
			`SELECT farm.device_unpark($1::uuid, $2::text, $3::text, true)`,
			id, p.cfg.Actor, reasonPrefix+reason)
		cancel()
		actionsTotal.WithLabelValues("unpark", outcomeOf(err)).Inc()
		if err != nil && ctx.Err() == nil {
			p.log.Warn("could not close a charge-policy park", "device", id, "err", err)
			ok = false
		}
	}
	return ok
}

// writeGate records what this loop believes about the ports' power on the
// runtime rows. This is the ONLY writer of farm.device_runtime.charge_gate.
//
// The loop connects as the DSN user, which owns the schema, so no grant is
// needed today. A deployment that gives this role a runtime user of its own
// needs `GRANT UPDATE (charge_gate, updated_at) ON farm.device_runtime` — the
// watchdog's role deliberately does not carry it, so that health can never
// claim a port state it did not set.
func (p *Policy) writeGate(ctx context.Context, ids []string, state string) {
	if len(ids) == 0 {
		return
	}
	cctx, cancel := p.db(ctx)
	defer cancel()
	if _, err := p.cfg.Pool.Exec(cctx, `
UPDATE farm.device_runtime
   SET charge_gate = $2::text, updated_at = now()
 WHERE device_id = ANY($1::uuid[])
   AND charge_gate IS DISTINCT FROM $2::text`, ids, state); err != nil {
		p.failed(ctx, "charge_gate", "could not record the charge gate state", "state", state, "err", err)
	}
}

// noteFailure records what a failed agent call leaves behind on the column:
// unknown when the agent could not be reached, since the port may be either;
// nothing at all otherwise, because a refusal switched nothing and the column
// already says what the port is doing.
func (p *Policy) noteFailure(ctx context.Context, u unit, err error) {
	if errors.Is(err, node.ErrUnreachable) {
		p.writeGate(ctx, u.ids(), node.ChargePowerUnknown)
	}
}

// releaseStrays hands back every gate of this loop's that no unit renewed or
// released this cycle: a device that was retired, a slot that moved, a park
// that vanished from under it. Gates that are not this loop's are never
// touched; they are somebody's deliberate hold, and it is theirs to end.
func (p *Policy) releaseStrays(ctx context.Context, gates map[string][]node.ChargeGate, touched map[string]map[string]bool) {
	for host, held := range gates {
		for _, g := range held {
			if !g.Held || !strings.HasPrefix(g.Reason, reasonPrefix) || touched[host][g.Devpath] {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout+node.DefaultResolveTimeout)
			_, err := p.cfg.Gates.ReleaseChargeGate(cctx, host, g.Devpath,
				reasonPrefix+"no device under this hold")
			cancel()
			actionsTotal.WithLabelValues("reconcile", outcomeOf(err)).Inc()
			if err != nil {
				if ctx.Err() == nil {
					p.log.Warn("a stray charge gate could not be released",
						"host", host, "devpath", g.Devpath, "err", err)
				}
				continue
			}
			p.log.Info("released a charge gate no device is held under",
				"host", host, "devpath", g.Devpath, "was", g.Reason)
			p.writeGateByPosition(ctx, host, g.Devpath, node.ChargePowerOn)
		}
	}
}

func (p *Policy) writeGateByPosition(ctx context.Context, host, devpath, state string) {
	cctx, cancel := p.db(ctx)
	defer cancel()
	if _, err := p.cfg.Pool.Exec(cctx, `
UPDATE farm.device_runtime r
   SET charge_gate = $3::text, updated_at = now()
  FROM farm.slots s
 WHERE s.id = r.slot_id AND s.host_id = $1::text AND s.adb_devpath = $2::text
   AND r.charge_gate IS DISTINCT FROM $3::text`, host, devpath, state); err != nil {
		p.failed(ctx, "charge_gate", "could not record the charge gate state", "state", state, "err", err)
	}
}

// ---------------------------------------------------------------------------
// The scan
// ---------------------------------------------------------------------------

// scan reads every non-retired device with everything a decision needs.
//
// Retired devices are out of the farm for good and cannot be parked, so they
// are neither candidates nor, on a ganged domain, obstacles. Everything else
// is read, including devices with no slot and hosts with no agent, because a
// park of this loop's may be open on any of them and has to be closed if it
// can no longer be backed by a gate.
//
// Every clock comparison is the server's: a park is "due" against now() in
// Postgres, and the cooldown window is measured there too.
const scanSQL = `
SELECT d.id::text,
       COALESCE(s.host_id, d.host_id, ''),
       COALESCE(s.adb_devpath, ''),
       COALESCE(s.id, 0),
       COALESCE(s.power_domain_id, 0),
       COALESCE(pd.kind, ''),
       COALESCE(pd.control, ''),
       h.node_endpoint IS NOT NULL,
       COALESCE(r.battery_pct, -1)::int,
       COALESCE(r.charge_gate, ''),
       d.current_lease_id IS NOT NULL,
       d.admin_state,
       p.id IS NOT NULL,
       COALESCE(p.auto, false),
       COALESCE(p.opened_by, ''),
       COALESCE(p.opened_at < now() - $2::interval, false),
       hc.device_id IS NOT NULL
  FROM farm.devices d
  LEFT JOIN farm.slots s          ON s.id = d.current_slot_id
  LEFT JOIN farm.hosts h          ON h.id = COALESCE(s.host_id, d.host_id)
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.device_parks p   ON p.device_id = d.id AND p.closed_at IS NULL
  LEFT JOIN (SELECT DISTINCT device_id
               FROM farm.device_parks
              WHERE auto AND opened_by = $1::text
                AND closed_by IS NOT NULL AND closed_by <> $1::text
                AND closed_at > now() - $3::interval) hc ON hc.device_id = d.id
 WHERE d.admin_state <> 'retired'
 ORDER BY COALESCE(s.host_id, d.host_id, ''), COALESCE(s.power_domain_id, 0), s.adb_devpath, d.id`

func (p *Policy) scan(ctx context.Context) ([]device, error) {
	cctx, cancel := p.db(ctx)
	defer cancel()

	rows, err := p.cfg.Pool.Query(cctx, scanSQL, p.cfg.Actor,
		pgInterval(p.cfg.PeekEvery), pgInterval(p.cfg.HumanCooldown))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []device
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.ID, &d.HostID, &d.Devpath, &d.SlotID, &d.DomainID,
			&d.DomainKind, &d.DomainControl, &d.HasAgent, &d.Battery, &d.ChargeGate,
			&d.Leased, &d.AdminState, &d.Parked, &d.ParkAuto, &d.ParkedBy,
			&d.HoldDue, &d.HumanUnparked); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// gangedSiblings lists every position of every ganged domain, keyed by
// domain. It selects slots and not devices, for the reason the recovery
// ladder's powerDomainSiblings gives: the agent checks the acknowledgement
// against what is physically plugged in, and an empty slot in the database
// can still have a phone in it.
func (p *Policy) gangedSiblings(ctx context.Context) (map[int64][]string, error) {
	cctx, cancel := p.db(ctx)
	defer cancel()

	rows, err := p.cfg.Pool.Query(cctx, `
SELECT s.power_domain_id, s.adb_devpath
  FROM farm.slots s
  JOIN farm.power_domains pd ON pd.id = s.power_domain_id
 WHERE pd.kind = 'ganged'
 ORDER BY s.power_domain_id, s.adb_devpath`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var devpath string
		if err := rows.Scan(&id, &devpath); err != nil {
			return nil, err
		}
		out[id] = append(out[id], devpath)
	}
	return out, rows.Err()
}

// group turns the scan into units: one per device, except on a ganged domain,
// where every device on the domain is decided together.
func group(devices []device, siblings map[int64][]string) []unit {
	byKey := make(map[string]*unit)
	var order []string
	for _, d := range devices {
		key := "device/" + d.ID
		ganged := d.DomainKind == "ganged" && d.DomainID != 0
		if ganged {
			key = "domain/" + d.HostID + "/" + strconv.FormatInt(d.DomainID, 10)
		}
		u, ok := byKey[key]
		if !ok {
			u = &unit{Key: key, HostID: d.HostID, Ganged: ganged}
			byKey[key] = u
			order = append(order, key)
		}
		u.Devices = append(u.Devices, d)
	}
	out := make([]unit, 0, len(order))
	for _, key := range order {
		u := byKey[key]
		if u.Ganged {
			// Every position but the ones this unit itself occupies — the
			// agent wants the OTHER ports named. On a ganged domain the anchor
			// changes with the hottest device, so the full sibling list minus
			// nothing is the safe form: an over-acknowledged port costs a
			// harmless duplicate, an under-acknowledged one costs a refusal.
			u.Siblings = siblings[u.Devices[0].DomainID]
		}
		out = append(out, *u)
	}
	return out
}

// listGates asks each host with an agent what it is holding. A host that does
// not answer is absent from the result, which the cycle reads as "decide
// nothing here".
//
// Every host with an agent is asked, not only the hosts that still have a
// device enrolled: a gate can outlive its device — retired while the port was
// dark — and the only way to hand that port back is to ask the agent that
// holds it. Without this, the last phone leaving a host would leave its port
// to the dead-man's switch, which is safe but is not what "reconciled every
// cycle" promises.
func (p *Policy) listGates(ctx context.Context, units []unit) map[string][]node.ChargeGate {
	out := make(map[string][]node.ChargeGate)
	if p.cfg.Gates == nil {
		p.warnOnce("no-client", "no node client is configured, so the charge policy can "+
			"observe but cannot hold a port; set FARM_NODE_TOKEN to the value the farmd "+
			"node agents use")
		return out
	}

	// The once-per-host warning is about hosts that HAVE devices and no way
	// to hold them; a host with an agent and no devices is not a problem.
	for _, u := range units {
		if u.HostID == "" {
			continue
		}
		if !u.Devices[0].HasAgent {
			p.warnOnce("no-agent/"+u.HostID, "host has no node agent endpoint recorded, so "+
				"no device on it can be charge limited", "host", u.HostID,
				"fix", "run farmd node on the host, or set farm.hosts.node_endpoint")
		} else {
			p.forgetWarn("no-agent/" + u.HostID)
		}
	}

	for _, host := range p.agentHosts(ctx, units) {
		cctx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout+node.DefaultResolveTimeout)
		gates, err := p.cfg.Gates.ChargeGates(cctx, host)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				p.warnOnce("unreachable/"+host, "the node agent did not list its charge "+
					"gates; no device on this host is decided this cycle",
					"host", host, "err", err)
			}
			p.markUnreachable(ctx, host)
			continue
		}
		p.forgetWarn("unreachable/" + host)
		out[host] = gates
		held := 0
		for _, g := range gates {
			if g.Held && strings.HasPrefix(g.Reason, reasonPrefix) {
				held++
			}
		}
		gatesHeld.WithLabelValues(host).Set(float64(held))
	}
	return out
}

// agentHosts lists every host with a node endpoint, in a stable order. If
// farm.hosts cannot be read the hosts the scan already named are used, so a
// transient database fault costs a stray-gate check and not the whole cycle.
func (p *Policy) agentHosts(ctx context.Context, units []unit) []string {
	cctx, cancel := p.db(ctx)
	defer cancel()
	rows, err := p.cfg.Pool.Query(cctx,
		`SELECT id FROM farm.hosts WHERE node_endpoint IS NOT NULL ORDER BY id`)
	if err == nil {
		defer rows.Close()
		var hosts []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			hosts = append(hosts, id)
		}
		if err == nil {
			err = rows.Err()
		}
		if err == nil {
			return hosts
		}
	}
	p.failed(ctx, "hosts", "could not list the hosts with an agent; asking only the hosts "+
		"the scan named this cycle", "err", err)
	var hosts []string
	seen := make(map[string]bool)
	for _, u := range units {
		if u.HostID != "" && u.Devices[0].HasAgent && !seen[u.HostID] {
			seen[u.HostID] = true
			hosts = append(hosts, u.HostID)
		}
	}
	return hosts
}

// markUnreachable records that every port this loop believed dark on a host
// is now in a state it cannot vouch for.
func (p *Policy) markUnreachable(ctx context.Context, host string) {
	cctx, cancel := p.db(ctx)
	defer cancel()
	if _, err := p.cfg.Pool.Exec(cctx, `
UPDATE farm.device_runtime
   SET charge_gate = $2::text, updated_at = now()
 WHERE host_id = $1::text AND charge_gate = $3::text`,
		host, node.ChargePowerUnknown, node.ChargePowerOff); err != nil {
		p.failed(ctx, "charge_gate", "could not mark a host's gates unknown", "host", host, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// beat records that this loop is alive. One statement, issued directly, for
// the reason every health-side loop issues it directly: the lease package is
// the door to ending a lease and this package must not have it in scope.
func (p *Policy) beat(ctx context.Context) {
	cctx, cancel := p.db(ctx)
	defer cancel()
	if _, err := p.cfg.Pool.Exec(cctx, `SELECT farm.component_beat($1::text)`, p.cfg.Component); err != nil {
		if ctx.Err() == nil {
			beatFailures.Inc()
			p.log.Warn("component heartbeat failed", "err", err)
		}
	}
}

// failed records a database step that could not run, silently when the cause
// is this process shutting down.
func (p *Policy) failed(ctx context.Context, stage, msg string, args ...any) {
	if ctx.Err() != nil {
		return
	}
	errorsTotal.WithLabelValues(stage).Inc()
	p.log.Warn(msg, args...)
}

func (p *Policy) db(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.cfg.CallTimeout)
}

func (p *Policy) warnOnce(key, msg string, args ...any) {
	if _, seen := p.warned[key]; seen {
		return
	}
	p.warned[key] = struct{}{}
	p.log.Warn(msg, args...)
}

func (p *Policy) forgetWarn(key string) { delete(p.warned, key) }

// outcomeOf folds an error into a metric label using the agent client's own
// vocabulary: a refusal is the hardware saying no, unreachable says nothing
// about the hardware, and anything else was attempted and failed.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, node.ErrUnreachable):
		return "unreachable"
	case errors.Is(err, node.ErrRefused):
		return "refused"
	default:
		return "failed"
	}
}

// errText bounds an error for a ledger column. The agent's sentences are
// long by design; a park's close_reason is not the place for all of one.
func errText(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// pgInterval renders a duration as a Postgres interval literal in exact
// microseconds — a DURATION, never an instant, so the server's now() is the
// only clock any deadline is measured against.
func pgInterval(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(int64(d/time.Microsecond), 10) + " microseconds"
}

// jitter spreads timers so replicas, and the other loops sharing this
// database, do not wake in lockstep after a restart.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(int64(d)/4+1))
}

// ---------------------------------------------------------------------------
// Leader election
// ---------------------------------------------------------------------------

// leadership is the same session-advisory-lock pattern as
// internal/scheduler.leadership; the rationale for the dedicated connection is
// documented there. Two policy loops would not corrupt anything — every park
// and unpark re-checks its own guard in SQL — but they would race each other
// to assert and release the same gates, and the agent would be told two
// stories about one port.
type leadership struct {
	pool *pgxpool.Pool
	key  int64
	log  *slog.Logger

	conn *pgxpool.Conn
	held bool
}

func (l *leadership) ensure(ctx context.Context, timeout time.Duration) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if l.conn != nil {
		if err := l.conn.Ping(cctx); err != nil {
			if ctx.Err() == nil {
				l.log.Warn("charge policy leadership connection died; standing down", "err", err)
			}
			l.drop()
			return false, nil
		}
		if l.held {
			return true, nil
		}
	}
	if l.conn == nil {
		c, err := l.pool.Acquire(cctx)
		if err != nil {
			return false, fmt.Errorf("chargepolicy: acquire leadership connection: %w", err)
		}
		l.conn = c
	}
	var ok bool
	if err := l.conn.QueryRow(cctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&ok); err != nil {
		l.drop()
		return false, fmt.Errorf("chargepolicy: try advisory lock: %w", err)
	}
	if !ok {
		l.drop()
		return false, nil
	}
	l.held = true
	l.log.Info("charge policy acquired leadership", "lock_key", l.key)
	return true, nil
}

func (l *leadership) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	if l.held {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := l.conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, l.key); err != nil {
			l.log.Debug("advisory unlock failed; the session close will release it", "err", err)
		}
	}
	l.drop()
}

func (l *leadership) drop() {
	if l.conn != nil {
		l.conn.Release()
		l.conn = nil
	}
	l.held = false
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	cyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "cycles_total",
		Help: "Charge policy cycles, including cycles run by a standby that decided nothing.",
	})

	parkedGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "parked_devices",
		Help: "Devices under this policy's own park as of the last scan: idle phones taken " +
			"out of allocation because their battery was over the ceiling. Every one is " +
			"capacity that comes back on its own; a human's park is not counted here.",
	})

	gatesHeld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "gates_held",
		Help: "Ports the policy is holding dark on each host, as the host's agent reports " +
			"them. Every one is a parked device that is neither charging nor reachable.",
	}, []string{"host"})

	actionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "actions_total",
		Help: "What the policy did, by action and outcome. refused means the agent or the " +
			"ledger declined and asking again changes nothing; unreachable means nothing " +
			"is known about the port.",
	}, []string{"action", "outcome"})

	skippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "skipped_total",
		Help: "Units over the charge ceiling the policy could not hold, by reason. A " +
			"sustained rate under no_agent or unswitchable is hardware the policy cannot " +
			"reach; under leased or foreign_park it is the policy keeping its hands off.",
	}, []string{"reason"})

	errorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "errors_total",
		Help: "Database steps that failed, by stage. A failed scan decides nothing, which " +
			"is the safe direction: held gates expire on the agent's clock.",
	}, []string{"stage"})

	beatFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "heartbeat_failures_total",
		Help: "Failed farm.component_beat calls. This component must NOT be listed in " +
			"FARM_REAPER_COMPONENTS: a charge limiter's downtime may not move lease clocks.",
	})

	leaderGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "chargepolicy", Name: "leader",
		Help: "1 when this replica holds the charge policy advisory lock. Sum across replicas must be 1.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
// Every known label value is pre-created so a rule over it has a series from
// the first scrape rather than from the first event.
func Collectors() []prometheus.Collector {
	for _, action := range []string{"gate", "renew", "peek", "release", "park", "unpark", "reconcile"} {
		for _, outcome := range []string{"ok", "refused", "unreachable", "failed"} {
			actionsTotal.WithLabelValues(action, outcome)
		}
	}
	for _, reason := range []string{
		"leased", "foreign_park", "foreign_gate", "no_client", "no_slot", "no_agent",
		"agent_unreachable", "unswitchable", "unknown_battery", "human_unparked",
	} {
		skippedTotal.WithLabelValues(reason)
	}
	for _, stage := range []string{"scan", "siblings", "hosts", "charge_gate"} {
		errorsTotal.WithLabelValues(stage)
	}
	return []prometheus.Collector{
		cyclesTotal, parkedGauge, gatesHeld, actionsTotal, skippedTotal, errorsTotal,
		beatFailures, leaderGauge,
	}
}
