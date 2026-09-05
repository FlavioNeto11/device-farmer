package topo

// What these tests protect: discovery creates slots on EVIDENCE and retires
// them on nothing short of the kernel no longer reporting the port. A hub
// becomes slots because a phone is in it or an operator said so — never
// because it happens to be plugged into the same machine. A dry run plans
// every one of those slots and writes nothing. A port that vanished is marked
// 'maintenance', never deleted, and never in bulk: a pass claiming that a
// quarter of a host disappeared is a bad read, not a rebuilt rack, and acting
// on it would take every device on the host out of scheduling. A human who
// parked a slot has the last word, and a partial scan retires nothing at all.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Helpers shared by the pure and the SQL-backed tests
// ---------------------------------------------------------------------------

// filterOf applies the defaults New would apply, so a test exercises the
// policy a deployment gets rather than a zero MinPorts.
func filterOf(f HubFilter) *HubFilter {
	f.applyDefaults()
	return &f
}

// skipReason reports why rep declined a path, or "" when it did not.
func skipReason(rep *Report, path string) string {
	for _, s := range rep.Skipped {
		if s.Path == path {
			return s.Reason
		}
	}
	return ""
}

// refPaths renders a slot list as its sorted USB paths, so an assertion does
// not depend on the order a RETURNING clause happened to use.
func refPaths(refs []SlotRef) string {
	paths := make([]string, 0, len(refs))
	for _, r := range refs {
		paths = append(paths, r.USBPath)
	}
	sort.Strings(paths)
	return strings.Join(paths, ",")
}

// plannedSlot is what the planner must produce for port n of the seven-port
// fixture hub at 3-1, given the label prefix the host's rack coordinates
// produce.
func plannedSlot(n int, labelPrefix string, occupied bool) PlannedSlot {
	return PlannedSlot{
		HubPath:    "3-1",
		HubModel:   "GenesysLogic USB2.1 Hub",
		HubPorts:   7,
		Switchable: true,
		Port:       n,
		USBPath:    "3-1." + strconv.Itoa(n),
		RackSlot:   labelPrefix + "-H3.1-P" + strconv.Itoa(n),
		Occupied:   occupied,
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---------------------------------------------------------------------------
// Which hubs join the farm
// ---------------------------------------------------------------------------

// deskFixture is a host with one of everything selectHubs has an opinion
// about: a root hub with a phone straight into port 5, a hub carrying a phone
// and a keyboard, a hub holding only a keyboard, an empty hub, a two-port hub
// with a phone in it, and a hub whose descriptor claims more ports than the
// schema can hold.
func deskFixture() fstest.MapFS {
	return sysfsFixture(
		fxHub{Path: "3-0", Ports: 8, Attached: map[int]fxKind{5: fxPhone}},
		fxHub{Path: "3-1", Ports: 7, Power: PerPortPower, Attached: map[int]fxKind{1: fxPhone, 4: fxKeyboard}},
		fxHub{Path: "3-2", Ports: 7, Power: PerPortPower, Attached: map[int]fxKind{3: fxKeyboard}},
		fxHub{Path: "3-3", Ports: 7, Power: PerPortPower},
		fxHub{Path: "3-4", Ports: 2, Power: NoPower, Attached: map[int]fxKind{1: fxPhone}},
		fxHub{Path: "3-6", Ports: 33, Attached: map[int]fxKind{1: fxPhone}},
	)
}

// TestSelectHubsAdoptsOnEvidence: by default a hub joins because a phone is in
// it; an operator's Include joins it regardless and Exclude wins over that;
// AdoptEmpty accepts a hub carrying nothing at all but not one carrying a
// keyboard; root hubs are opt-in and even then only with a phone straight
// into the board; and every refusal is recorded with its reason, because
// "discovery did not create my slots" must be answerable from the Report.
//
// Falsify: in selectHubs, change `case f.AdoptEmpty && h.ForeignPorts() == 0`
// to `case f.AdoptEmpty` — the keyboard hub 3-2 is then adopted and becomes
// seven schedulable slots on somebody's desk.
func TestSelectHubsAdoptsOnEvidence(t *testing.T) {
	t.Parallel()
	tree := scanFixture(t, deskFixture())

	adopted := func(f HubFilter) ([]string, Report) {
		var rep Report
		return hubPaths(selectHubs(filterOf(f), tree, &rep)), rep
	}

	// Defaults: a phone is the only evidence.
	got, rep := adopted(HubFilter{})
	if fmt.Sprint(got) != "[3-1]" {
		t.Fatalf("default filter adopted %v, want only the hub with a phone in it", got)
	}
	for path, want := range map[string]string{
		"3-0": "root hub; set IncludeRootHubs",
		"3-2": "no Android device attached (1 ports occupied by something else)",
		"3-3": "no Android device attached (0 ports occupied by something else)",
		"3-4": "2 ports, below the 3-port floor",
		"3-6": "hub reports 33 ports; the schema accepts 1..32",
	} {
		if r := skipReason(&rep, path); !strings.Contains(r, want) {
			t.Errorf("skip reason for %s = %q, want it to say %q", path, r, want)
		}
	}
	if len(rep.Skipped) != 5 {
		t.Errorf("%d skips, want one per refused hub: %+v", len(rep.Skipped), rep.Skipped)
	}

	// AdoptEmpty: nothing foreign is enough; a keyboard is foreign.
	if got, _ = adopted(HubFilter{AdoptEmpty: true}); fmt.Sprint(got) != "[3-1 3-3]" {
		t.Errorf("AdoptEmpty adopted %v, want the phone hub and the empty one, not the keyboard's", got)
	}

	// Include beats the heuristics and the floor; Exclude beats Include.
	got, rep = adopted(HubFilter{Include: []string{"3-2", "3-4"}, Exclude: []string{"3-1"}})
	if fmt.Sprint(got) != "[3-2 3-4]" {
		t.Errorf("Include/Exclude adopted %v, want [3-2 3-4]", got)
	}
	if r := skipReason(&rep, "3-1"); r != "excluded by configuration" {
		t.Errorf("an excluded hub was skipped for %q", r)
	}
	if got, _ = adopted(HubFilter{Include: []string{"3-1"}, Exclude: []string{"3-1"}}); len(got) != 0 {
		t.Errorf("a hub both included and excluded was adopted: %v", got)
	}
	// An include cannot make the schema hold thirty-three ports: the phone hub
	// is still adopted on its own evidence, the oversized one is still refused.
	if got, rep = adopted(HubFilter{Include: []string{"3-6"}}); fmt.Sprint(got) != "[3-1]" ||
		!strings.Contains(skipReason(&rep, "3-6"), "1..32") {
		t.Errorf("an included hub beyond the schema's port ceiling: adopted %v, skipped for %q",
			got, skipReason(&rep, "3-6"))
	}

	// Root hubs: opt-in, and only with a phone straight into the board.
	if got, _ = adopted(HubFilter{IncludeRootHubs: true}); fmt.Sprint(got) != "[3-0 3-1]" {
		t.Errorf("IncludeRootHubs adopted %v, want the root hub alongside the phone hub", got)
	}
	bare := deskFixture()
	for k := range bare {
		if strings.HasPrefix(k, "3-5/") {
			delete(bare, k)
		}
	}
	rep = Report{}
	if got := hubPaths(selectHubs(filterOf(HubFilter{IncludeRootHubs: true}), scanFixture(t, bare), &rep)); fmt.Sprint(got) != "[3-1]" {
		t.Errorf("a root hub with only hubs and nothing Android in its ports was adopted: %v", got)
	}
	if r := skipReason(&rep, "3-0"); r != "root hub with no Android device attached" {
		t.Errorf("the empty root hub was skipped for %q", r)
	}

	// The floor is configuration.
	if got, _ = adopted(HubFilter{MinPorts: 2}); fmt.Sprint(got) != "[3-1 3-4]" {
		t.Errorf("MinPorts=2 adopted %v, want the two-port hub too", got)
	}
}

// ---------------------------------------------------------------------------
// What an adopted hub becomes
// ---------------------------------------------------------------------------

// TestPlanHubRegistersEveryPortExceptHubPorts: every socket of an adopted hub
// is a slot whether or not anything is in it — the empty socket is where the
// next phone goes — except a socket holding another hub, which is not a device
// position. On a root hub only the sockets with a phone in them qualify. Each
// slot carries the hub's model, its switching capability and the label the
// Labeler derives for it.
//
// Falsify: in planHub, delete the `p.Downstream != nil` skip — the socket
// holding hub 3-1.7 then becomes a slot whose usb_path is a hub's, and a
// recovery action aimed at that "device" lands on the hub.
func TestPlanHubRegistersEveryPortExceptHubPorts(t *testing.T) {
	t.Parallel()

	tree := scanFixture(t, sysfsFixture(
		fxHub{Path: "3-0", Ports: 4, Attached: map[int]fxKind{2: fxPhone}},
		fxHub{Path: "3-1", Ports: 7, Power: PerPortPower, Attached: map[int]fxKind{1: fxPhone, 2: fxPhone, 5: fxKeyboard}},
		fxHub{Path: "3-1.7", Ports: 4, Power: NoPower, Attached: map[int]fxKind{2: fxPhone}},
	))
	l, err := NewLabeler("h1", "r2", 14, Overrides{})
	if err != nil {
		t.Fatal(err)
	}

	var rep Report
	plan := planHub(tree.Hub("3-1"), l, &rep)
	if len(plan) != 6 {
		t.Fatalf("planned %d slots for a seven-port hub with a hub in port 7, want 6: %+v", len(plan), plan)
	}
	for i, p := range plan {
		n := i + 1
		if want := plannedSlot(n, "R2-U14", n == 1 || n == 2 || n == 5); p != want {
			t.Errorf("port %d planned as %+v, want %+v", n, p, want)
		}
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Path != "3-1.7" || !strings.Contains(rep.Skipped[0].Reason, "carries a hub") {
		t.Errorf("skips = %+v, want exactly the socket holding the downstream hub", rep.Skipped)
	}

	// The downstream hub gets its own slots, and they are not switchable.
	child := planHub(tree.Hub("3-1.7"), l, &rep)
	if len(child) != 4 || child[0].Switchable || child[0].HubPorts != 4 ||
		child[0].USBPath != "3-1.7.1" || child[0].RackSlot != "R2-U14-H3.1.7-P1" ||
		child[0].Occupied || !child[1].Occupied {
		t.Errorf("downstream hub planned as %+v", child)
	}

	// A root hub: only the socket with the phone, and the socket holding hub
	// 3-1 is a recorded skip like any other hub port.
	rep = Report{}
	root := planHub(tree.Controllers[0].RootHub, l, &rep)
	if len(root) != 1 || root[0].USBPath != "3-2" || root[0].Port != 2 || root[0].RackSlot != "R2-U14-H3.0-P2" ||
		root[0].Switchable || root[0].HubPath != "3-0" || root[0].HubPorts != 4 || !root[0].Occupied {
		t.Errorf("root hub planned as %+v, want only root port 2", root)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Path != "3-1" {
		t.Errorf("root hub skips = %+v, want only the socket holding hub 3-1", rep.Skipped)
	}

	// The plan is written hub by hub, each in its own transaction.
	all := append(append(plan, child...), root...)
	if g := groupByHub(all); len(g) != 3 || len(g["3-1"]) != 6 || len(g["3-1.7"]) != 4 || len(g["3-0"]) != 1 {
		t.Errorf("groupByHub = %d hubs (%d/%d/%d)", len(g), len(g["3-1"]), len(g["3-1.7"]), len(g["3-0"]))
	}
}

// TestCheckPlannedLabelsRefusesTwoSocketsUnderOneLabel: a SlotLabels override
// equal to a label the scheme derives for the socket next to it passes
// Labeler.Check, which can only see hub tokens, and is caught on the finished
// plan. farm.slots.rack_slot has no uniqueness constraint, so nothing
// downstream would.
//
// Falsify: make checkPlannedLabels return nil.
func TestCheckPlannedLabelsRefusesTwoSocketsUnderOneLabel(t *testing.T) {
	t.Parallel()
	tree := scanFixture(t, rackFixture())

	l, err := NewLabeler("h1", "r1", 14, Overrides{SlotLabels: map[string]string{"3-1.3": "R1-U14-H3.1-P4"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Check([]string{"3-1"}); err != nil {
		t.Fatalf("Labeler.Check cannot see this collision and must not fail here: %v", err)
	}
	var rep Report
	err = checkPlannedLabels(planHub(tree.Hub("3-1"), l, &rep), "h1")
	if err == nil || !strings.Contains(err.Error(), "3-1.3") || !strings.Contains(err.Error(), "3-1.4") ||
		!strings.Contains(err.Error(), `"R1-U14-H3.1-P4"`) {
		t.Errorf("two sockets under one label: %v", err)
	}

	plain, err := NewLabeler("h1", "r1", 14, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPlannedLabels(planHub(tree.Hub("3-1"), plain, &rep), "h1"); err != nil {
		t.Errorf("distinct derived labels were refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// One pass against the real schema
// ---------------------------------------------------------------------------

// farmHost is one test's host row in the scratch database.
type farmHost struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context
	id   string
}

// newFarmHost empties the schema and declares one racked host, the way a
// deployment does before pointing discovery at it.
func newFarmHost(t *testing.T, id, rack string, unit int) *farmHost {
	t.Helper()
	pool := requireDB(t)
	resetSchema(t, pool)
	h := &farmHost{t: t, pool: pool, ctx: context.Background(), id: id}
	h.exec(`INSERT INTO farm.racks (id) VALUES ($1)`, rack)
	h.exec(`INSERT INTO farm.hosts (id, rack_id, rack_unit, adb_endpoint) VALUES ($1, $2, $3, '127.0.0.1:5037')`,
		id, rack, unit)
	return h
}

// discoverer wires cfg to this host and the scratch pool.
func (h *farmHost) discoverer(cfg Config) *Discoverer {
	h.t.Helper()
	cfg.Pool = h.pool
	if cfg.HostID == "" {
		cfg.HostID = h.id
	}
	cfg.Logger = quiet()
	d, err := New(cfg)
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	return d
}

// once runs one pass that must succeed.
func (h *farmHost) once(cfg Config) Report {
	h.t.Helper()
	rep, err := h.discoverer(cfg).Once(h.ctx)
	if err != nil {
		h.t.Fatalf("Once: %v (report %+v)", err, rep)
	}
	return rep
}

func (h *farmHost) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func (h *farmHost) scan(dest any, sql string, args ...any) {
	h.t.Helper()
	if err := h.pool.QueryRow(h.ctx, sql, args...).Scan(dest); err != nil {
		h.t.Fatalf("query %.60q: %v", sql, err)
	}
}

func (h *farmHost) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	h.scan(&n, sql, args...)
	return n
}

// powerDomain reads the kind and control of the domain register_slot created
// for a hub, "per_port/uhubctl" or "ganged/none".
func (h *farmHost) powerDomain(hubPath string) string {
	h.t.Helper()
	var s string
	h.scan(&s, `SELECT kind || '/' || control FROM farm.power_domains WHERE host_id = $1 AND control_addr = $2`,
		h.id, hubPath)
	return s
}

// slotIDs renders every slot of the host as "usb_path=id", so a test can prove
// a second pass reused the rows rather than replacing them.
func (h *farmHost) slotIDs() string {
	h.t.Helper()
	var s string
	h.scan(&s, `SELECT COALESCE(string_agg(usb_path || '=' || id, ',' ORDER BY usb_path), '')
	              FROM farm.slots WHERE host_id = $1`, h.id)
	return s
}

// assertStates checks the state of named slots.
func (h *farmHost) assertStates(want map[string]string) {
	h.t.Helper()
	for usbPath, state := range want {
		var got string
		h.scan(&got, `SELECT state FROM farm.slots WHERE host_id = $1 AND usb_path = $2`, h.id, usbPath)
		if got != state {
			h.t.Errorf("slot %s is %q, want %q", usbPath, got, state)
		}
	}
}

// assertNothingWritten checks that the topology tables and the timeline are
// as empty as before the pass.
func (h *farmHost) assertNothingWritten() {
	h.t.Helper()
	for _, table := range []string{"slots", "hubs", "controllers", "power_domains", "events", "audit_log"} {
		if n := h.count(`SELECT count(*) FROM farm.` + table); n != 0 {
			h.t.Errorf("farm.%s holds %d rows after a pass that must write nothing", table, n)
		}
	}
}

// hubOf is the removal fixture: a root hub and, in root port 1, a per-port
// switchable hub of the given size carrying two phones.
func hubOf(ports int) fstest.MapFS {
	return sysfsFixture(
		fxHub{Path: "3-0", Ports: 4},
		fxHub{Path: "3-1", Ports: ports, Power: PerPortPower, Attached: map[int]fxKind{1: fxPhone, 2: fxPhone}},
	)
}

// TestOnceDryRunPlansEverythingAndWritesNothing: with DryRun the Report
// carries every slot the pass would register, with the labels the host's rack
// coordinates and the operator's overrides produce, and no table changes. A
// map that would put two sockets under one label, and a host farm.hosts does
// not know, are refused before anything is written, dry run or not.
//
// Falsify: in Once, move the `if d.cfg.DryRun` return below the registration
// loop — the dry run then writes seven slots.
func TestOnceDryRunPlansEverythingAndWritesNothing(t *testing.T) {
	h := newFarmHost(t, "h1", "r1", 14)
	src := FromFS(rackFixture(), "fixture")

	rep := h.once(Config{Source: src, DryRun: true})
	if !rep.DryRun || rep.Written != 0 || rep.Hubs != 1 || len(rep.Planned) != 7 || rep.Partial ||
		rep.Source != "fixture" || rep.Host != "h1" || rep.Duration <= 0 {
		t.Fatalf("dry run report = %+v", rep)
	}
	for i, p := range rep.Planned {
		n := i + 1
		if want := plannedSlot(n, "R1-U14", n <= 3 || n == 5); p != want {
			t.Errorf("port %d planned as %+v, want %+v", n, p, want)
		}
	}
	if r := skipReason(&rep, "3-0"); !strings.Contains(r, "IncludeRootHubs") {
		t.Errorf("the root hub's skip reads %q; it must tell the operator which knob adopts it", r)
	}
	h.assertNothingWritten()

	// The operator's map is honoured in the plan, which is what makes a dry
	// run the way to proofread one.
	rep = h.once(Config{Source: src, DryRun: true, Overrides: Overrides{
		HubTokens:  map[string]string{"3-1": "3"},
		SlotLabels: map[string]string{"3-1.7": "spare"},
	}})
	if len(rep.Planned) != 7 || rep.Planned[0].RackSlot != "R1-U14-H3-P1" || rep.Planned[6].RackSlot != "spare" {
		t.Errorf("overrides in a dry run: %+v", rep.Planned)
	}
	h.assertNothingWritten()

	// A colliding map is refused on the finished plan, before any write.
	_, err := h.discoverer(Config{Source: src, Overrides: Overrides{
		SlotLabels: map[string]string{"3-1.7": "R1-U14-H3.1-P1"},
	}}).Once(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "3-1.7") || !strings.Contains(err.Error(), "3-1.1") {
		t.Errorf("a label collision was not refused: %v", err)
	}
	h.assertNothingWritten()

	// A host discovery was never told about is an error naming it, never an
	// empty rack: discovery cannot know a host's adb_endpoint.
	_, err = h.discoverer(Config{HostID: "ghost", Source: src, DryRun: true}).Once(h.ctx)
	if !errors.Is(err, ErrHostUnknown) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("an unknown host: %v", err)
	}
	h.assertNothingWritten()
}

// TestOnceRegistersHubsIdempotentlyAndCorrectsPower: a pass writes one slot
// per socket through farm.register_slot, links a daisy-chained hub to its
// parent, and gives a non-switchable hub one ganged power domain. A second
// pass over unchanged hardware reuses every row and writes no event. When the
// hub in a socket is swapped for one that cannot switch ports, the domain is
// downgraded — the direction that keeps the recovery ladder from cutting seven
// devices to fix one — and a domain an operator pointed at a PDU is left
// alone.
//
// Falsify: in reconcilePower, swap the toGanged and toPerPort statements —
// the swapped-in ganged hub then keeps its per_port domain.
func TestOnceRegistersHubsIdempotentlyAndCorrectsPower(t *testing.T) {
	h := newFarmHost(t, "h1", "r1", 14)
	rack := func(power PowerSwitching) Source {
		return FromFS(sysfsFixture(
			fxHub{Path: "3-0", Ports: 4},
			fxHub{Path: "3-1", Ports: 7, Power: power, Attached: map[int]fxKind{1: fxPhone, 2: fxPhone, 5: fxKeyboard}},
			fxHub{Path: "3-1.7", Ports: 4, Power: NoPower, Attached: map[int]fxKind{2: fxPhone}},
		), "fixture")
	}

	rep := h.once(Config{Source: rack(PerPortPower)})
	if rep.Written != 10 || rep.Hubs != 2 || rep.HubLinks != 1 || rep.PowerCorrections != 0 ||
		len(rep.Retired)+len(rep.Restored) != 0 || rep.Partial {
		t.Fatalf("first pass = %+v", rep)
	}
	if n := h.count(`SELECT count(*) FROM farm.slots WHERE host_id = $1 AND state = 'active'`, h.id); n != 10 {
		t.Errorf("%d active slots, want 6 on the hub plus 4 on the hub below it", n)
	}
	if n := h.count(`SELECT count(*) FROM farm.controllers WHERE host_id = $1 AND root_bus = 3`, h.id); n != 1 {
		t.Errorf("%d controllers for bus 3, want 1", n)
	}
	var hubRow string
	h.scan(&hubRow, `SELECT model || '/' || port_count || '/' || vbus_switchable
	                   FROM farm.hubs WHERE host_id = $1 AND usb_path = '3-1'`, h.id)
	if hubRow != "GenesysLogic USB2.1 Hub/7/true" {
		t.Errorf("hub row = %q", hubRow)
	}
	var parent string
	h.scan(&parent, `SELECT p.usb_path FROM farm.hubs c JOIN farm.hubs p ON p.id = c.parent_hub_id
	                  WHERE c.host_id = $1 AND c.usb_path = '3-1.7'`, h.id)
	if parent != "3-1" {
		t.Errorf("the downstream hub's parent is %q, want 3-1", parent)
	}
	if got := h.powerDomain("3-1"); got != "per_port/uhubctl" {
		t.Errorf("switchable hub's domain = %s", got)
	}
	if got := h.powerDomain("3-1.7"); got != "ganged/none" {
		t.Errorf("non-switchable hub's domain = %s, want one ganged domain", got)
	}
	if n := h.count(`SELECT count(DISTINCT power_domain_id) FROM farm.slots WHERE host_id = $1`, h.id); n != 2 {
		t.Errorf("slots span %d power domains, want one per hub", n)
	}
	var labels string
	h.scan(&labels, `SELECT string_agg(rack_slot, ',' ORDER BY usb_path) FROM farm.slots
	                  WHERE host_id = $1 AND usb_path LIKE '3-1.7.%'`, h.id)
	if labels != "R1-U14-H3.1.7-P1,R1-U14-H3.1.7-P2,R1-U14-H3.1.7-P3,R1-U14-H3.1.7-P4" {
		t.Errorf("downstream hub labels = %s", labels)
	}
	if n := h.count(`SELECT count(*) FROM farm.events`) + h.count(`SELECT count(*) FROM farm.audit_log`); n != 0 {
		t.Errorf("%d timeline rows for a routine registration, want none", n)
	}
	ids := h.slotIDs()

	// Unchanged hardware: the same rows, and still no timeline entry — this is
	// what lets an operator tell a real topology change from noise.
	rep = h.once(Config{Source: rack(PerPortPower)})
	if rep.Written != 10 || rep.HubLinks != 0 || rep.PowerCorrections != 0 {
		t.Errorf("second pass = %+v, want the same calls and no new links", rep)
	}
	if got := h.slotIDs(); got != ids {
		t.Errorf("slot rows changed across an idempotent pass:\n%s\n%s", ids, got)
	}
	if n := h.count(`SELECT count(*) FROM farm.events`) + h.count(`SELECT count(*) FROM farm.audit_log`); n != 0 {
		t.Errorf("%d timeline rows after a no-op pass", n)
	}

	// The hub in root port 1 was swapped for one that cannot switch ports.
	rep = h.once(Config{Source: rack(NoPower)})
	if rep.PowerCorrections != 1 {
		t.Fatalf("third pass corrected %d domains, want 1: %+v", rep.PowerCorrections, rep)
	}
	if got := h.powerDomain("3-1"); got != "ganged/none" {
		t.Errorf("domain after the swap = %s, want ganged/none", got)
	}
	if n := h.count(`SELECT count(*) FROM farm.events WHERE kind = 'power_domain_corrected' AND actor = $1
	                  AND detail->>'hub' = '3-1' AND detail->>'kind' = 'none' AND detail->>'evidence' <> ''`,
		DefaultActor); n != 1 {
		t.Errorf("%d power_domain_corrected events naming the hub and its evidence, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'power_domain.reconcile'
	                  AND subject = 'hub:h1:3-1' AND actor = $1`, DefaultActor); n != 1 {
		t.Errorf("%d audit rows for the correction, want 1", n)
	}
	h.scan(&hubRow, `SELECT model || '/' || port_count || '/' || vbus_switchable
	                   FROM farm.hubs WHERE host_id = $1 AND usb_path = '3-1'`, h.id)
	if hubRow != "GenesysLogic USB2.1 Hub/7/false" {
		t.Errorf("hub row after the swap = %q", hubRow)
	}
	if got := h.slotIDs(); got != ids {
		t.Errorf("a power correction replaced slot rows:\n%s\n%s", ids, got)
	}

	// ...and back, on positive evidence.
	rep = h.once(Config{Source: rack(PerPortPower)})
	if rep.PowerCorrections != 1 || h.powerDomain("3-1") != "per_port/uhubctl" {
		t.Errorf("upgrade on positive evidence: corrections=%d domain=%s", rep.PowerCorrections, h.powerDomain("3-1"))
	}

	// A domain an operator pointed at a PDU is theirs, whatever sysfs says.
	h.exec(`UPDATE farm.power_domains SET kind = 'per_port', control = 'pdu' WHERE host_id = $1 AND control_addr = '3-1'`, h.id)
	rep = h.once(Config{Source: rack(NoPower)})
	if rep.PowerCorrections != 0 || h.powerDomain("3-1") != "per_port/pdu" {
		t.Errorf("an external switch was overridden: corrections=%d domain=%s", rep.PowerCorrections, h.powerDomain("3-1"))
	}
}

// TestOnceRefusesMassRemovalAndRetiresOnlyWhatVanished: ports the kernel no
// longer reports are marked 'maintenance' with a timeline event and an audit
// row, never deleted, and only within MaxRetireFraction of the host — a scan
// that says most of the host is gone is refused as a bad read. One vanished
// slot is always allowed through. A port that comes back is restored, unless a
// human parked it, and a partial scan retires nothing at all.
//
// Falsify: in reconcileRemovals, delete the `len(vanished) > 1 &&` guard's
// fraction check (make the condition `false`) — the 5-port pass at the default
// bound then retires two of seven and the refusal never happens.
func TestOnceRefusesMassRemovalAndRetiresOnlyWhatVanished(t *testing.T) {
	h := newFarmHost(t, "h1", "r1", 14)
	src := func(fsys fstest.MapFS) Source { return FromFS(fsys, "fixture") }

	if rep := h.once(Config{Source: src(hubOf(7))}); rep.Written != 7 {
		t.Fatalf("seed pass = %+v", rep)
	}
	allActive := map[string]string{}
	for n := 1; n <= 7; n++ {
		allActive["3-1."+strconv.Itoa(n)] = "active"
	}

	// The whole hub gone from the scan: the host has slots and the scan has
	// none. That is not a host that lost its hardware; it is a scan that did
	// not work.
	rep, err := h.discoverer(Config{Source: src(sysfsFixture(fxHub{Path: "3-0", Ports: 4})), RetireVanished: true}).Once(h.ctx)
	if !errors.Is(err, ErrMassRemoval) || !strings.Contains(err.Error(), "7 of 7 active slots on h1") {
		t.Fatalf("an empty scan of a populated host: %v", err)
	}
	if !strings.Contains(strings.Join(rep.Problems, ";"), "7 of 7 active slots are missing") || len(rep.Retired) != 0 {
		t.Errorf("report = %+v, want the refusal explained and nothing retired", rep)
	}
	h.assertStates(allActive)
	if n := h.count(`SELECT count(*) FROM farm.events WHERE kind = 'slot_vanished'`); n != 0 {
		t.Errorf("%d slot_vanished events after a refused pass", n)
	}

	// Two of seven is over the default quarter. What IS there is still
	// registered; the retirement is not.
	rep, err = h.discoverer(Config{Source: src(hubOf(5)), RetireVanished: true}).Once(h.ctx)
	if !errors.Is(err, ErrMassRemoval) || rep.Written != 5 || len(rep.Retired) != 0 {
		t.Fatalf("2 of 7 at the default bound: err=%v report=%+v", err, rep)
	}
	if !strings.Contains(strings.Join(rep.Problems, ";"), "2 of 7 active slots are missing from the scan, over the 25% bound") {
		t.Errorf("problems = %v", rep.Problems)
	}
	h.assertStates(allActive)

	// Raised deliberately for one run: the two are marked, with their labels,
	// and nothing is deleted.
	rep = h.once(Config{Source: src(hubOf(5)), RetireVanished: true, MaxRetireFraction: 0.5})
	if got := refPaths(rep.Retired); got != "3-1.6,3-1.7" {
		t.Fatalf("retired %q, want 3-1.6,3-1.7 (report %+v)", got, rep)
	}
	if rep.Retired[0].RackSlot != "R1-U14-H3.1-P6" || rep.Retired[0].ID == 0 {
		t.Errorf("retired slot ref = %+v, want its id and label", rep.Retired[0])
	}
	if n := h.count(`SELECT count(*) FROM farm.slots WHERE host_id = $1`, h.id); n != 7 {
		t.Errorf("%d slot rows after a retirement, want all 7 kept", n)
	}
	h.assertStates(map[string]string{"3-1.5": "active", "3-1.6": "maintenance", "3-1.7": "maintenance"})
	if n := h.count(`SELECT count(*) FROM farm.events e JOIN farm.slots s ON s.id = e.slot_id
	                  WHERE e.kind = 'slot_vanished' AND e.actor = $1 AND s.usb_path IN ('3-1.6', '3-1.7')
	                    AND e.detail->>'rack_slot' = s.rack_slot AND e.detail->>'source' = 'fixture'`,
		DefaultActor); n != 2 {
		t.Errorf("%d slot_vanished events for the two retired slots, want 2", n)
	}
	if n := h.count(`SELECT count(*) FROM farm.audit_log a JOIN farm.slots s ON a.subject = 'slot:' || s.id
	                  WHERE a.action = 'slot.maintenance' AND a.actor = $1 AND a.reason <> ''
	                    AND s.usb_path IN ('3-1.6', '3-1.7')`, DefaultActor); n != 2 {
		t.Errorf("%d audit rows for the two retirements, want 2", n)
	}

	// One vanished slot is always allowed through, whatever the bound:
	// pulling one dead hub out of a small host is the common case.
	rep = h.once(Config{Source: src(hubOf(4)), RetireVanished: true, MaxRetireFraction: 0.01})
	if got := refPaths(rep.Retired); got != "3-1.5" {
		t.Errorf("a single vanished slot under a 1%% bound: retired %q, want 3-1.5", got)
	}

	// The hub is back: discovery undoes its own retirements, with a timeline
	// entry each.
	rep = h.once(Config{Source: src(hubOf(7)), RetireVanished: true})
	if got := refPaths(rep.Restored); got != "3-1.5,3-1.6,3-1.7" || len(rep.Retired) != 0 {
		t.Fatalf("restored %q (report %+v), want the three that vanished", got, rep)
	}
	h.assertStates(allActive)
	if n := h.count(`SELECT count(*) FROM farm.events WHERE kind = 'slot_returned' AND actor = $1`, DefaultActor); n != 3 {
		t.Errorf("%d slot_returned events, want 3", n)
	}
	if n := h.count(`SELECT count(*) FROM farm.audit_log WHERE action = 'slot.active' AND actor = $1`, DefaultActor); n != 3 {
		t.Errorf("%d slot.active audit rows, want 3", n)
	}

	// A human parked a slot. Their word is the last one, and a timer waking
	// up does not undo it.
	var parked int64
	h.scan(&parked, `SELECT id FROM farm.slots WHERE host_id = $1 AND usb_path = '3-1.4'`, h.id)
	h.exec(`UPDATE farm.slots SET state = 'maintenance' WHERE id = $1`, parked)
	h.exec(`INSERT INTO farm.audit_log (actor, action, subject, reason)
	        VALUES ('alice', 'slot.maintenance', $1, 'flaky cable; leave it out until it is replaced')`,
		fmt.Sprintf("slot:%d", parked))
	rep = h.once(Config{Source: src(hubOf(7)), RetireVanished: true})
	if len(rep.Restored) != 0 {
		t.Errorf("discovery restored a slot a human parked: %+v", rep.Restored)
	}
	h.assertStates(map[string]string{"3-1.4": "maintenance", "3-1.5": "active"})

	// A partial scan retires nothing, however many ports it failed to see: an
	// absent port on an incomplete read is not evidence that the port is gone.
	broken := hubOf(5)
	delete(broken, "3-1.2/idVendor")
	rep = h.once(Config{Source: src(broken), RetireVanished: true, MaxRetireFraction: 0.5})
	if !rep.Partial || len(rep.Retired) != 0 || !strings.Contains(strings.Join(rep.Problems, ";"), "reconciliation skipped") {
		t.Errorf("partial scan: partial=%v retired=%v problems=%v", rep.Partial, rep.Retired, rep.Problems)
	}
	h.assertStates(map[string]string{"3-1.6": "active", "3-1.7": "active"})
}
