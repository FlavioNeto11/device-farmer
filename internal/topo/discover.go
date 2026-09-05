// Package topo discovers the USB topology of a farm host and writes it into
// the schema, so that slots and hubs are a description of the hardware rather
// than of a seed script somebody ran once.
//
// # What a slot is, and what it is not
//
// A slot is a POSITION: bus, hub, port. It is what a human can walk to, what a
// power switch can cut, and what survives the device in it being unplugged,
// replaced, reflashed or renamed. Devices come and go from slots; the schema
// is built that way on purpose (see migrations/00001_core.sql), and so is this
// package.
//
// The consequence that matters most: NOTHING here removes a slot because a
// device stopped answering. A phone that boots into fastboot, drops off the
// bus, or has USB debugging switched off is still in its socket, and its slot
// still exists. The only thing that retires a slot is the PORT itself no
// longer being reported by the kernel — the hub was unplugged or replaced —
// and even then the slot is marked 'maintenance', never deleted, because
// leases, recovery attempts and quarantines all reference it by id.
//
// # What discovery may and may not do
//
// It creates and updates farm.controllers, farm.hubs, farm.power_domains and
// farm.slots, all through farm.register_slot, which already knows the correct
// shape of those rows. It writes farm.events and farm.audit_log so a change to
// the physical rack has a timeline entry and a named actor.
//
// It does not touch farm.leases, farm.devices or farm.device_runtime. It
// cannot end a lease, and nothing in it accepts a value that could carry a
// transport error into an allocation decision. A hub vanishing from sysfs
// while a device on it holds a six-hour lease marks the slot unschedulable for
// the NEXT allocation and leaves the running job alone.
//
// # Idempotence
//
// Running discovery twice against unchanged hardware writes nothing the second
// time: every statement is either an upsert whose values are already present
// or is guarded by IS DISTINCT FROM. That is a correctness property, not an
// optimisation — discovery runs on a timer, and an operator must be able to
// tell a real topology change from routine noise by looking at farm.events.
package topo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Defaults for [Config].
const (
	// DefaultActor names discovery in farm.audit_log and farm.events. It is
	// deliberately not a human name: rows written by this package were not
	// somebody's decision.
	DefaultActor = "topo-discovery"

	DefaultInterval    = 5 * time.Minute
	DefaultCallTimeout = 30 * time.Second

	// DefaultMinHubPorts keeps two-port hubs — the one inside a monitor, the
	// one inside a keyboard — out of the fleet.
	DefaultMinHubPorts = 3

	// DefaultMaxRetireFraction is the share of a host's active slots that one
	// pass may mark 'maintenance' before it refuses and asks for a human. A
	// real change (somebody pulled a hub) moves a few percent. Half the host
	// vanishing at once is a bad read, and acting on it would take a rack out
	// of service on the strength of a udev hiccup.
	DefaultMaxRetireFraction = 0.25
)

// schemaMaxPorts mirrors the CHECK on farm.hubs.port_count and
// farm.slots.port_number. A hub reporting more than this is not clamped to
// fit: clamping would record a lie about the hardware, and the extra ports
// would silently never become slots.
const schemaMaxPorts = 32

// HubFilter decides which hubs become farm hubs.
//
// The default (zero value plus applyDefaults) is conservative: a hub joins the
// farm when it is carrying at least one Android device. A hub full of dongles
// on somebody's desk does not become fourteen slots because it happens to be
// plugged into the same machine.
type HubFilter struct {
	// Include is a list of hub USB paths that are always adopted, whatever the
	// heuristics say. This is how an empty hub gets its slots before the
	// phones arrive.
	Include []string

	// Exclude is a list of hub USB paths that are never adopted. Excludes win
	// over includes.
	Exclude []string

	// MinPorts ignores hubs smaller than this. Defaults to
	// DefaultMinHubPorts.
	MinPorts int

	// AdoptEmpty adopts any hub that is carrying nothing foreign — no
	// keyboard, no NIC, no dongle — even when it is carrying no phone either.
	// For a rack that is cabled before it is populated.
	AdoptEmpty bool

	// IncludeRootHubs allows slots on a controller's root hub, for phones
	// plugged straight into the motherboard.
	//
	// Off by default, and even when on, ONLY root-hub ports that currently
	// hold an Android device become slots. A root hub's other ports are the
	// machine's own keyboard, NIC and BMC, and turning those into schedulable
	// farm positions is how an operator ends up power-cycling the host's boot
	// drive to recover a phone.
	IncludeRootHubs bool
}

func (f *HubFilter) applyDefaults() {
	if f.MinPorts <= 0 {
		f.MinPorts = DefaultMinHubPorts
	}
}

func (f *HubFilter) excluded(usbPath string) bool { return slices.Contains(f.Exclude, usbPath) }
func (f *HubFilter) included(usbPath string) bool { return slices.Contains(f.Include, usbPath) }

// Config wires a Discoverer. Pool, HostID and Source are required.
type Config struct {
	// Pool must connect as a role that may write farm.slots, farm.hubs,
	// farm.controllers and farm.power_domains. It must NOT be farm_reaper,
	// farm_scheduler or farm_watchdog: those roles exist to keep allocation,
	// health and recovery apart, and discovery belongs to none of them.
	Pool *pgxpool.Pool

	// HostID is farm.hosts.id for the machine being scanned. The row must
	// already exist; discovery does not invent hosts, because it cannot know a
	// host's adb_endpoint.
	HostID string

	// Source reads the USB tree. Use [Sysfs] in production, [FromFS] in tests.
	Source Source

	// Overrides is the operator's naming map. See rackslot.go.
	Overrides Overrides

	Filter HubFilter

	// Actor is written to farm.audit_log.actor and farm.events.actor.
	Actor string

	// RetireVanished enables removal reconciliation: ports the kernel no
	// longer reports are marked 'maintenance'. Off by default, because the
	// first thing a new deployment does is scan a host whose slots were seeded
	// by hand, and the second thing it should not do is retire all of them.
	RetireVanished bool

	// MaxRetireFraction bounds one pass's retirements. See
	// DefaultMaxRetireFraction.
	MaxRetireFraction float64

	// DryRun computes the full plan and writes nothing. The returned Report
	// still lists every slot that would be created and every label it would
	// carry.
	DryRun bool

	Interval    time.Duration
	CallTimeout time.Duration

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Actor == "" {
		c.Actor = DefaultActor
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.MaxRetireFraction <= 0 || c.MaxRetireFraction > 1 {
		c.MaxRetireFraction = DefaultMaxRetireFraction
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.Filter.applyDefaults()
}

// Discoverer scans a host's USB tree and reconciles it into the schema.
type Discoverer struct {
	cfg Config
	log *slog.Logger
}

// New validates cfg and returns a Discoverer.
func New(cfg Config) (*Discoverer, error) {
	if cfg.Pool == nil {
		return nil, errors.New("topo: Config.Pool is required")
	}
	if cfg.HostID == "" {
		return nil, errors.New("topo: Config.HostID is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("topo: Config.Source is required; " +
			"a discoverer with nothing to read would report an empty host")
	}
	cfg.applyDefaults()
	return &Discoverer{
		cfg: cfg,
		log: cfg.Logger.With("component", "topo", "host", cfg.HostID),
	}, nil
}

// PlannedSlot is one farm.register_slot call.
type PlannedSlot struct {
	HubPath    string
	HubModel   string
	HubPorts   int
	Switchable bool
	Port       int
	USBPath    string
	RackSlot   string
	// Occupied records whether something was enumerated in the port at scan
	// time. It is informational: the slot is registered either way.
	Occupied bool
}

// Skip is a hub or port discovery declined to register, and why.
type Skip struct {
	Path   string
	Reason string
}

// SlotRef identifies a slot in a Report.
type SlotRef struct {
	ID       int64
	USBPath  string
	RackSlot string
}

// Report is what one pass did.
type Report struct {
	Host     string
	Source   string
	Started  time.Time
	Duration time.Duration

	// Planned is every slot the scan resolved, whether or not it was written.
	Planned []PlannedSlot
	// Hubs is the number of distinct hubs in Planned.
	Hubs int
	// Written is the number of farm.register_slot calls that succeeded. Zero
	// in a dry run.
	Written int

	Skipped  []Skip
	Retired  []SlotRef
	Restored []SlotRef

	// PowerCorrections counts power domains whose switching kind was brought
	// back in line with the hardware.
	PowerCorrections int
	// HubLinks counts parent_hub_id links established this pass.
	HubLinks int

	// Partial is true when the USB scan was incomplete. A partial pass
	// registers what it saw and retires nothing.
	Partial  bool
	Problems []string

	DryRun bool
}

// ErrMassRemoval means one pass would have retired more of the host than
// [Config.MaxRetireFraction] allows. Nothing was retired. It is a signal for a
// human, not a condition to retry through: either the rack really was
// rebuilt — in which case raise the bound deliberately for one run — or the
// scan is wrong.
var ErrMassRemoval = errors.New("topo: refusing to retire that share of the host's slots")

// ErrHostUnknown means farm.hosts has no row for Config.HostID.
var ErrHostUnknown = errors.New("topo: host is not registered")

// Once performs one discovery pass.
//
// The Report is filled in as far as the pass got, so a caller that receives an
// error still learns what was registered before it.
func (d *Discoverer) Once(ctx context.Context) (rep Report, err error) {
	rep = Report{
		Host:    d.cfg.HostID,
		Started: time.Now(),
		DryRun:  d.cfg.DryRun,
	}
	// Named returns, so that a caller who gets an error still gets the elapsed
	// time and everything the pass managed to do before it stopped.
	defer func() { rep.Duration = time.Since(rep.Started) }()

	tree, err := Scan(ctx, d.cfg.Source)
	if err != nil {
		scansTotal.WithLabelValues("failed").Inc()
		return rep, err
	}
	rep.Source = tree.Source
	rep.Partial = tree.Partial
	rep.Problems = slices.Clone(tree.Problems)

	rack, unit, err := d.hostRack(ctx)
	if err != nil {
		scansTotal.WithLabelValues("failed").Inc()
		return rep, err
	}

	labeler, err := NewLabeler(d.cfg.HostID, rack, unit, d.cfg.Overrides)
	if err != nil {
		scansTotal.WithLabelValues("failed").Inc()
		return rep, err
	}

	hubs := selectHubs(&d.cfg.Filter, tree, &rep)
	paths := make([]string, 0, len(hubs))
	for _, h := range hubs {
		paths = append(paths, h.Path)
	}
	if err = labeler.Check(paths); err != nil {
		scansTotal.WithLabelValues("failed").Inc()
		return rep, err
	}

	rep.Hubs = len(hubs)
	for _, h := range hubs {
		rep.Planned = append(rep.Planned, planHub(h, labeler, &rep)...)
	}

	// Checked on the finished plan, and before a dry run returns, because
	// Labeler.Check can only see the hub tokens. A SlotLabels override that
	// happens to equal a generated label collides here and nowhere else, and
	// farm.slots.rack_slot carries no uniqueness constraint to catch it — the
	// second write would simply succeed and leave two sockets answering to one
	// label.
	if err = checkPlannedLabels(rep.Planned, d.cfg.HostID); err != nil {
		scansTotal.WithLabelValues("failed").Inc()
		return rep, err
	}

	if d.cfg.DryRun {
		scansTotal.WithLabelValues("dry_run").Inc()
		d.logPass(rep)
		return rep, nil
	}

	// Registration happens hub by hub, each in its own transaction. A hub is
	// the natural unit: its controller, its power domain and its ports are one
	// consistent statement about one piece of hardware. A failure part-way
	// leaves whole hubs registered rather than half a hub.
	byHub := groupByHub(rep.Planned)
	for _, h := range hubs {
		plan := byHub[h.Path]
		if len(plan) == 0 {
			continue
		}
		n, rerr := d.registerHub(ctx, h, plan, &rep)
		rep.Written += n
		slotsRegistered.Add(float64(n))
		if err = rerr; err != nil {
			scansTotal.WithLabelValues("failed").Inc()
			return rep, err
		}
	}

	if err = d.linkHubs(ctx, hubs, byHub, &rep); err != nil {
		scansTotal.WithLabelValues("failed").Inc()
		return rep, err
	}

	live := make([]string, 0, len(rep.Planned))
	for _, p := range rep.Planned {
		live = append(live, p.USBPath)
	}

	switch {
	case !d.cfg.RetireVanished:
		// Reconciliation disabled. Slots that no longer exist stay active and
		// will simply never have a device in them.
	case rep.Partial:
		rep.Problems = append(rep.Problems,
			"removal reconciliation skipped: the USB scan was incomplete, "+
				"so an absent port is not evidence that the port is gone")
	default:
		if err = d.reconcileRemovals(ctx, live, &rep); err != nil {
			// A refusal and a database failure are different events: the first
			// is the guard doing its job and needs a human, the second is a
			// retry. They must not share an alert.
			outcome := "failed"
			if errors.Is(err, ErrMassRemoval) {
				outcome = "removal_refused"
			}
			scansTotal.WithLabelValues(outcome).Inc()
			d.logPass(rep)
			return rep, err
		}
	}

	if rep.Partial {
		scansTotal.WithLabelValues("partial").Inc()
	} else {
		scansTotal.WithLabelValues("ok").Inc()
		lastSuccess.SetToCurrentTime()
	}
	d.logPass(rep)
	return rep, nil
}

// Run performs a pass immediately and then every Config.Interval until ctx is
// cancelled.
//
// A failed pass is logged and the loop continues. Discovery failing is a
// degradation — new hardware is not picked up — and never an emergency: it
// takes nothing away from anybody, so there is no reason to bring the process
// down over it.
//
// Note what this loop deliberately does NOT do: it does not write
// farm.component_beat. That heartbeat feeds farm.control_plane_gap, which
// exists to refund leases when the control plane was absent. Discovery is not
// on the renewal path, and a discovery loop that stalled must never be
// mistaken for a control plane that stopped renewing.
func (d *Discoverer) Run(ctx context.Context) error {
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()

	for {
		if _, err := d.Once(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			d.log.Error("usb discovery pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (d *Discoverer) logPass(rep Report) {
	args := []any{
		"source", rep.Source, "hubs", rep.Hubs, "slots", len(rep.Planned),
		"written", rep.Written, "skipped", len(rep.Skipped),
		"retired", len(rep.Retired), "restored", len(rep.Restored),
		"power_corrections", rep.PowerCorrections,
		"took", time.Since(rep.Started).Round(time.Millisecond),
	}
	if rep.DryRun {
		args = append(args, "dry_run", true)
	}
	if rep.Partial {
		d.log.Warn("usb discovery pass was incomplete; nothing was retired",
			append(args, "problems", rep.Problems)...)
		return
	}
	d.log.Info("usb discovery pass", args...)
}

// ---------------------------------------------------------------------------
// Selecting hubs and ports
// ---------------------------------------------------------------------------

// selectHubs applies the filter and records why each rejected hub was
// rejected. The reasons end up in the Report because "discovery did not create
// my slots" is otherwise an unanswerable question.
//
// It is a function of the filter and the tree alone — no pool, no host — so
// the whole adoption policy can be exercised against a fixture tree.
func selectHubs(f *HubFilter, t *Tree, rep *Report) []*Hub {
	var out []*Hub
	for _, h := range t.Hubs() {
		switch {
		case f.excluded(h.Path):
			rep.Skipped = append(rep.Skipped, Skip{h.Path, "excluded by configuration"})
			continue
		case h.MaxChild < 1 || h.MaxChild > schemaMaxPorts:
			rep.Skipped = append(rep.Skipped, Skip{h.Path, fmt.Sprintf(
				"hub reports %d ports; the schema accepts 1..%d", h.MaxChild, schemaMaxPorts)})
			continue
		}

		if f.included(h.Path) {
			out = append(out, h)
			continue
		}

		if h.IsRoot {
			if !f.IncludeRootHubs {
				rep.Skipped = append(rep.Skipped, Skip{h.Path,
					"root hub; set IncludeRootHubs to use motherboard ports as slots"})
				continue
			}
			if h.AndroidPorts() == 0 {
				rep.Skipped = append(rep.Skipped, Skip{h.Path,
					"root hub with no Android device attached"})
				continue
			}
			out = append(out, h)
			continue
		}

		if h.MaxChild < f.MinPorts {
			rep.Skipped = append(rep.Skipped, Skip{h.Path, fmt.Sprintf(
				"%d ports, below the %d-port floor", h.MaxChild, f.MinPorts)})
			continue
		}
		switch {
		case h.AndroidPorts() > 0:
			out = append(out, h)
		case f.AdoptEmpty && h.ForeignPorts() == 0:
			out = append(out, h)
		default:
			rep.Skipped = append(rep.Skipped, Skip{h.Path, fmt.Sprintf(
				"no Android device attached (%d ports occupied by something else)",
				h.ForeignPorts())})
		}
	}
	return out
}

// planHub turns one hub into the register_slot calls it deserves.
//
// Every port of an adopted hub becomes a slot, occupied or not: the empty
// socket next to seven full ones is where the eighth phone goes, and the slot
// has to exist before the device does. The exceptions are recorded as skips:
//
//   - a port holding another hub is not a device position. Registering it
//     would create a slot whose usb_path is a hub's usb_path, and a recovery
//     action aimed at that "device" would be aimed at a hub.
//
//   - on a root hub, only ports that currently hold an Android device are
//     registered. See HubFilter.IncludeRootHubs.
func planHub(h *Hub, l *Labeler, rep *Report) []PlannedSlot {
	model := h.Model()
	out := make([]PlannedSlot, 0, len(h.Ports))
	for _, p := range h.Ports {
		if p.Downstream != nil {
			rep.Skipped = append(rep.Skipped, Skip{p.Path,
				"port carries a hub, not a device position"})
			continue
		}
		if h.IsRoot && (p.Attached == nil || !p.Attached.IsAndroid()) {
			continue
		}
		out = append(out, PlannedSlot{
			HubPath:    h.Path,
			HubModel:   model,
			HubPorts:   h.MaxChild,
			Switchable: h.Power.Switchable(),
			Port:       p.Number,
			USBPath:    p.Path,
			RackSlot:   l.Slot(h.Path, p.Path, p.Number),
			Occupied:   p.Attached != nil,
		})
	}
	return out
}

// checkPlannedLabels refuses a plan in which two slots would carry the same
// rack_slot.
//
// The whole value of a rack_slot is that it names one socket. Two slots under
// one label is worse than no label: it sends an operator confidently to a
// device that is not the one on the page, and farm.register_slot will overwrite
// rack_slot with whatever it is given, so the collision persists until somebody
// notices it in the rack. Nothing is written; the override map is the fix.
func checkPlannedLabels(plan []PlannedSlot, hostID string) error {
	seen := make(map[string]string, len(plan))
	for _, p := range plan {
		if other, dup := seen[p.RackSlot]; dup {
			return fmt.Errorf("topo: slots %s and %s on %s would both be labelled %q; "+
				"fix Overrides.SlotLabels or Overrides.HubTokens before these labels are written",
				other, p.USBPath, hostID, p.RackSlot)
		}
		seen[p.RackSlot] = p.USBPath
	}
	return nil
}

func groupByHub(plan []PlannedSlot) map[string][]PlannedSlot {
	out := make(map[string][]PlannedSlot)
	for _, p := range plan {
		out[p.HubPath] = append(out[p.HubPath], p)
	}
	return out
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

func (d *Discoverer) hostRack(ctx context.Context) (rack string, unit int, err error) {
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	defer cancel()

	var (
		rackID   *string
		rackUnit *int
	)
	err = d.cfg.Pool.QueryRow(cctx,
		`SELECT rack_id, rack_unit FROM farm.hosts WHERE id = $1`,
		d.cfg.HostID).Scan(&rackID, &rackUnit)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Creating the row here is not an option: farm.hosts.adb_endpoint is
		// NOT NULL and discovery has no idea what it is. A host is declared by
		// deployment; discovery only describes what is plugged into it.
		return "", 0, fmt.Errorf("%w: %q has no row in farm.hosts; "+
			"insert the host with its adb_endpoint (and rack_id/rack_unit if it is racked) "+
			"before pointing discovery at it", ErrHostUnknown, d.cfg.HostID)
	case err != nil:
		return "", 0, fmt.Errorf("topo: reading host %s: %w", d.cfg.HostID, err)
	}
	if rackID != nil {
		rack = *rackID
	}
	if rackUnit != nil {
		unit = *rackUnit
	}
	return rack, unit, nil
}

// registerHub writes one hub and its ports in a single transaction.
//
// It returns a non-zero count only after the transaction has committed. A
// partially written hub is rolled back in full, and reporting those rows as
// written would put a number in the Report and in slots_registered_total that
// nothing in the database backs — which is how an operator concludes that a
// rack was registered and stops looking.
func (d *Discoverer) registerHub(ctx context.Context, h *Hub, plan []PlannedSlot, rep *Report) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	defer cancel()

	tx, err := d.cfg.Pool.Begin(cctx)
	if err != nil {
		return 0, fmt.Errorf("topo: begin for hub %s: %w", h.Path, err)
	}
	defer tx.Rollback(cctx)

	written := 0
	for _, p := range plan {
		// farm.register_slot creates the controller, the hub and the power
		// domain, and gives a non-switchable hub ONE ganged domain. That last
		// part is the reason this is a database function and not Go: it is
		// what stops the recovery ladder from power-cycling seven devices to
		// fix one.
		var slotID int64
		if err := tx.QueryRow(cctx,
			`SELECT farm.register_slot($1, $2, $3, $4, $5, $6, $7, $8)`,
			d.cfg.HostID, p.USBPath, p.HubPath, p.Port,
			p.HubModel, p.HubPorts, p.Switchable, p.RackSlot,
		).Scan(&slotID); err != nil {
			return 0, fmt.Errorf("topo: register_slot %s/%s: %w", d.cfg.HostID, p.USBPath, err)
		}
		written++
	}

	corrected, err := d.reconcilePower(cctx, tx, h)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(cctx); err != nil {
		return 0, fmt.Errorf("topo: commit for hub %s: %w", h.Path, err)
	}
	rep.PowerCorrections += corrected
	if corrected > 0 {
		kind := GangedPower
		if h.Power.Switchable() {
			kind = PerPortPower
		}
		hubsRepowered.WithLabelValues(kind.String()).Inc()
	}
	return written, nil
}

// reconcilePower brings an existing power domain back in line with the
// hardware.
//
// farm.register_slot creates a hub's power domain on first sight and never
// revisits it, which is correct for a schema function and wrong for a rack
// where somebody swapped a switchable hub for a cheap one in the same socket.
// The domain would keep saying 'per_port' while VBUS is now ganged, and the
// recovery ladder — which reads farm.power_domains.kind to compute a blast
// radius — would cut power to a whole hub believing it had touched one port.
//
// So the downgrade to 'ganged' is the load-bearing direction here. The upgrade
// is a convenience and happens only on positive evidence. Both are restricted
// to the exact ('ganged','none')/('per_port','uhubctl') shapes register_slot
// itself writes, so a domain an operator pointed at a PDU or a smart hub is
// left alone: an external switch is authoritative over sysfs.
func (d *Discoverer) reconcilePower(ctx context.Context, tx pgx.Tx, h *Hub) (int, error) {
	const toGanged = `
UPDATE farm.power_domains
   SET kind = 'ganged', control = 'none'
 WHERE host_id = $1 AND control_addr = $2
   AND kind = 'per_port' AND control = 'uhubctl'`

	const toPerPort = `
UPDATE farm.power_domains
   SET kind = 'per_port', control = 'uhubctl'
 WHERE host_id = $1 AND control_addr = $2
   AND kind = 'ganged' AND control = 'none'`

	q := toGanged
	if h.Power.Switchable() {
		q = toPerPort
	}
	tag, err := tx.Exec(ctx, q, d.cfg.HostID, h.Path)
	if err != nil {
		return 0, fmt.Errorf("topo: reconciling the power domain of %s: %w", h.Path, err)
	}
	n := int(tag.RowsAffected())
	if n == 0 {
		return 0, nil
	}

	detail := map[string]any{
		"host":     d.cfg.HostID,
		"hub":      h.Path,
		"kind":     h.Power.String(),
		"evidence": h.PowerEvidence,
	}
	if err := insertEvent(ctx, tx, "power_domain_corrected", nil, d.cfg.Actor, detail); err != nil {
		return n, err
	}
	if err := insertAudit(ctx, tx, d.cfg.Actor, "power_domain.reconcile",
		"hub:"+d.cfg.HostID+":"+h.Path,
		"usb topology reports "+h.Power.String()+" power switching", detail); err != nil {
		return n, err
	}
	return n, nil
}

// linkHubs sets farm.hubs.parent_hub_id for hubs plugged into other hubs.
//
// farm.register_slot cannot do this: it registers one hub at a time and has no
// way to know the upstream one exists yet. The link matters because
// blast-radius correlation reads the hub tree — a five-port hub daisy-chained
// off another one fails as a unit with its parent, and without the link that
// correlation stops at the child.
func (d *Discoverer) linkHubs(ctx context.Context, hubs []*Hub, byHub map[string][]PlannedSlot, rep *Report) error {
	const q = `
UPDATE farm.hubs h
   SET parent_hub_id = p.id
  FROM farm.hubs p
 WHERE h.host_id = $1 AND h.usb_path = $2
   AND p.host_id = $1 AND p.usb_path = $3
   AND h.parent_hub_id IS DISTINCT FROM p.id`

	cctx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	defer cancel()

	for _, h := range hubs {
		if h.Upstream == nil || len(byHub[h.Path]) == 0 {
			continue
		}
		// Only link to a hub that discovery itself registered; an upstream hub
		// that was filtered out has no row to point at.
		if len(byHub[h.Upstream.Path]) == 0 {
			continue
		}
		tag, err := d.cfg.Pool.Exec(cctx, q, d.cfg.HostID, h.Path, h.Upstream.Path)
		if err != nil {
			return fmt.Errorf("topo: linking hub %s to %s: %w", h.Path, h.Upstream.Path, err)
		}
		rep.HubLinks += int(tag.RowsAffected())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Removals
// ---------------------------------------------------------------------------

// reconcileRemovals marks ports the kernel no longer reports as
// 'maintenance', and brings back the ones it reports again.
//
// Marked, never deleted. farm.leases, farm.recovery_attempts,
// farm.quarantines and farm.slot_occupancy all reference slots by id, and a
// slot id in a six-month-old lease row has to keep resolving to a position a
// human can be told about. 'maintenance' is also exactly the right semantics:
// farm.lease_acquire requires state='active', so a retired slot stops being
// scheduled and does nothing else. It does not, and cannot, end the lease of a
// device that is sitting in one — the hub being unplugged is not a reason to
// take a running job's device away.
func (d *Discoverer) reconcileRemovals(ctx context.Context, live []string, rep *Report) error {
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	defer cancel()

	var active int
	if err := d.cfg.Pool.QueryRow(cctx,
		`SELECT count(*) FROM farm.slots WHERE host_id = $1 AND state = 'active'`,
		d.cfg.HostID).Scan(&active); err != nil {
		return fmt.Errorf("topo: counting active slots on %s: %w", d.cfg.HostID, err)
	}

	if len(live) == 0 {
		if active == 0 {
			// A host with no farm hubs cabled yet. Nothing to reconcile and
			// nothing suspicious about it.
			return nil
		}
		// The host HAS slots and the scan found none. That is not a host that
		// lost its hardware; it is a scan that did not work.
		return d.refuseMassRemoval(active, active, rep)
	}

	vanished, err := d.vanishedSlots(cctx, live)
	if err != nil {
		return err
	}

	// The bound is checked BEFORE anything is written, and one vanished slot
	// is always allowed through: pulling a single dead hub out of a small host
	// can exceed any fraction, and that case is both common and obviously
	// benign.
	if len(vanished) > 1 && active > 0 &&
		float64(len(vanished))/float64(active) > d.cfg.MaxRetireFraction {
		return d.refuseMassRemoval(len(vanished), active, rep)
	}

	if len(vanished) > 0 {
		if err := d.retire(cctx, vanished, rep); err != nil {
			return err
		}
	}
	return d.restore(cctx, live, rep)
}

// refuseMassRemoval reports a pass that would have taken too much of the host
// out of service, and retires nothing.
//
// The bound is not a heuristic about hardware; it is a statement about
// evidence. Hubs fail one at a time. A pass claiming that a quarter of a host
// disappeared at once is far more likely to be a udev hiccup, a container that
// lost its /sys bind mount, or a scan against the wrong machine — and acting
// on it would make every device on that host unschedulable, which is a farm
// outage caused by a bad read.
func (d *Discoverer) refuseMassRemoval(missing, active int, rep *Report) error {
	rep.Problems = append(rep.Problems, fmt.Sprintf(
		"%d of %d active slots are missing from the scan, over the %.0f%% bound; nothing retired",
		missing, active, d.cfg.MaxRetireFraction*100))
	d.log.Error("usb scan says most of this host disappeared; retiring nothing",
		"missing", missing, "active", active,
		"bound", d.cfg.MaxRetireFraction, "source", rep.Source)
	return fmt.Errorf("%w: %d of %d active slots on %s; "+
		"confirm the host really lost that hardware (check that /sys is mounted and that "+
		"discovery is scanning the right machine), then raise MaxRetireFraction for one "+
		"deliberate run, or mark the slots maintenance by hand",
		ErrMassRemoval, missing, active, d.cfg.HostID)
}

func (d *Discoverer) vanishedSlots(ctx context.Context, live []string) ([]SlotRef, error) {
	const q = `
SELECT s.id, s.usb_path, COALESCE(s.rack_slot, '')
  FROM farm.slots s
 WHERE s.host_id = $1
   AND s.state = 'active'
   AND NOT (s.usb_path = ANY($2::text[]))
 ORDER BY s.usb_path`

	rows, err := d.cfg.Pool.Query(ctx, q, d.cfg.HostID, live)
	if err != nil {
		return nil, fmt.Errorf("topo: listing vanished slots on %s: %w", d.cfg.HostID, err)
	}
	defer rows.Close()

	var out []SlotRef
	for rows.Next() {
		var s SlotRef
		if err := rows.Scan(&s.ID, &s.USBPath, &s.RackSlot); err != nil {
			return nil, fmt.Errorf("topo: scanning vanished slots: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *Discoverer) retire(ctx context.Context, vanished []SlotRef, rep *Report) error {
	const q = `
UPDATE farm.slots
   SET state = 'maintenance'
 WHERE id = $1 AND state = 'active'`

	tx, err := d.cfg.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("topo: begin for retirement: %w", err)
	}
	defer tx.Rollback(ctx)

	const reason = "the USB scan no longer reports this port; " +
		"the slot is kept because leases and recovery history reference it"

	// Accumulated locally and published to the Report only after the commit:
	// a Report listing slots as retired that a failed commit rolled back would
	// tell an operator the fleet is smaller than it is.
	var retired []SlotRef
	for _, s := range vanished {
		tag, err := tx.Exec(ctx, q, s.ID)
		if err != nil {
			return fmt.Errorf("topo: retiring slot %d (%s): %w", s.ID, s.USBPath, err)
		}
		if tag.RowsAffected() == 0 {
			continue // somebody else moved it since the SELECT
		}
		detail := map[string]any{
			"host":      d.cfg.HostID,
			"usb_path":  s.USBPath,
			"rack_slot": s.RackSlot,
			"source":    rep.Source,
		}
		if err := insertEvent(ctx, tx, "slot_vanished", &s.ID, d.cfg.Actor, detail); err != nil {
			return err
		}
		// The audit subject matches the API's own slot subject format, so that
		// a human later re-enabling this slot lands on the same subject and
		// restore() can see that the last word was theirs.
		if err := insertAudit(ctx, tx, d.cfg.Actor, "slot.maintenance",
			fmt.Sprintf("slot:%d", s.ID), reason, detail); err != nil {
			return err
		}
		retired = append(retired, s)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("topo: commit for retirement: %w", err)
	}
	rep.Retired = retired
	slotsRetired.Add(float64(len(retired)))
	for _, s := range retired {
		d.log.Warn("slot retired: the USB scan no longer reports this port",
			"slot", s.ID, "usb_path", s.USBPath, "rack_slot", s.RackSlot)
	}
	return nil
}

// restore reactivates slots that discovery itself retired and that the kernel
// is reporting again — a hub that was unplugged and put back.
//
// It reactivates ONLY slots whose most recent audit row is discovery's own
// retirement. A human who put a slot into maintenance ("this port is flaky,
// leave it out until I replace the cable") has the last word, and a timer
// waking up and undoing their decision behind their back is precisely the
// class of automation this system refuses to contain.
func (d *Discoverer) restore(ctx context.Context, live []string, rep *Report) error {
	const q = `
UPDATE farm.slots s
   SET state = 'active'
 WHERE s.host_id = $1
   AND s.state = 'maintenance'
   AND s.usb_path = ANY($2::text[])
   AND (SELECT a.actor = $3 AND a.action = 'slot.maintenance'
          FROM farm.audit_log a
         WHERE a.subject = 'slot:' || s.id::text
         ORDER BY a.at DESC, a.id DESC
         LIMIT 1)
RETURNING s.id, s.usb_path, COALESCE(s.rack_slot, '')`

	tx, err := d.cfg.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("topo: begin for restoration: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, q, d.cfg.HostID, live, d.cfg.Actor)
	if err != nil {
		return fmt.Errorf("topo: restoring slots on %s: %w", d.cfg.HostID, err)
	}
	var restored []SlotRef
	for rows.Next() {
		var s SlotRef
		if err := rows.Scan(&s.ID, &s.USBPath, &s.RackSlot); err != nil {
			rows.Close()
			return fmt.Errorf("topo: scanning restored slots: %w", err)
		}
		restored = append(restored, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("topo: restoring slots on %s: %w", d.cfg.HostID, err)
	}

	for _, s := range restored {
		detail := map[string]any{
			"host":      d.cfg.HostID,
			"usb_path":  s.USBPath,
			"rack_slot": s.RackSlot,
			"source":    rep.Source,
		}
		if err := insertEvent(ctx, tx, "slot_returned", &s.ID, d.cfg.Actor, detail); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, d.cfg.Actor, "slot.active",
			fmt.Sprintf("slot:%d", s.ID),
			"the USB scan reports this port again", detail); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("topo: commit for restoration: %w", err)
	}
	rep.Restored = restored
	slotsRestored.Add(float64(len(restored)))
	for _, s := range restored {
		d.log.Info("slot restored: the USB scan reports this port again",
			"slot", s.ID, "usb_path", s.USBPath, "rack_slot", s.RackSlot)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bookkeeping helpers
// ---------------------------------------------------------------------------

func insertEvent(ctx context.Context, tx pgx.Tx, kind string, slotID *int64, actor string, detail map[string]any) error {
	body, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("topo: encoding event detail: %w", err)
	}
	// now() is the database's, never this process's: a host with a skewed
	// clock must not be able to write a timeline out of order.
	if _, err := tx.Exec(ctx,
		`INSERT INTO farm.events (kind, slot_id, actor, detail) VALUES ($1, $2, $3, $4::jsonb)`,
		kind, slotID, actor, string(body)); err != nil {
		return fmt.Errorf("topo: writing event %s: %w", kind, err)
	}
	return nil
}

func insertAudit(ctx context.Context, tx pgx.Tx, actor, action, subject, reason string, detail map[string]any) error {
	body, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("topo: encoding audit detail: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
		 VALUES ($1, $2, $3, nullif($4,''), $5::jsonb)`,
		actor, action, subject, reason, string(body)); err != nil {
		return fmt.Errorf("topo: writing audit row %s: %w", action, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	scansTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "topo", Name: "scans_total",
		Help: "USB discovery passes by outcome. removal_refused means one pass claimed " +
			"most of a host had disappeared and was stopped.",
	}, []string{"outcome"})

	slotsRegistered = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "topo", Name: "slots_registered_total",
		Help: "farm.register_slot calls that succeeded. It counts calls, not changes: an " +
			"unchanged rack still increments it once per port per pass.",
	})

	slotsRetired = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "topo", Name: "slots_retired_total",
		Help: "Slots marked maintenance because their port left the USB tree.",
	})

	slotsRestored = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "topo", Name: "slots_restored_total",
		Help: "Slots reactivated because their port came back.",
	})

	hubsRepowered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "topo", Name: "power_domain_corrections_total",
		Help: "Power domains realigned with the hardware. kind=ganged is the safety-critical " +
			"one: until it fired, the recovery ladder believed it could cut one port.",
	}, []string{"kind"})

	lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "topo", Name: "last_success_timestamp_seconds",
		Help: "When a complete discovery pass last finished. Staleness means new hardware " +
			"is not being picked up; it does not mean anything is at risk.",
	})
)

// Collectors returns this package's metrics for registration by the binary.
//
// The series that mean "a pass was refused" and "a hub turned out to be
// ganged" are created at zero, so an alert on them is armed from the first
// scrape rather than from the first incident.
func Collectors() []prometheus.Collector {
	scansTotal.WithLabelValues("ok")
	scansTotal.WithLabelValues("partial")
	scansTotal.WithLabelValues("removal_refused")
	scansTotal.WithLabelValues("failed")
	scansTotal.WithLabelValues("dry_run")
	hubsRepowered.WithLabelValues(GangedPower.String())
	hubsRepowered.WithLabelValues(PerPortPower.String())
	return []prometheus.Collector{
		scansTotal, slotsRegistered, slotsRetired, slotsRestored, hubsRepowered, lastSuccess,
	}
}
