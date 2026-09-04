package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Default farm shape: two hosts, four hubs each, seven ports per hub — 56
// positions, which is about one 42U rack's worth of phones and big enough that
// the correlation view has something to correlate.
const (
	DefaultHosts       = 2
	DefaultHubsPerHost = 4
	DefaultSlotsPerHub = 7

	// DefaultRack, DefaultPool, DefaultTenant and DefaultQueue are the tenancy
	// rows the runner submits work against. They are ordinary rows, not magic:
	// an operator can add more by hand and the demo keeps working.
	DefaultRack   = "r1"
	DefaultPool   = "default"
	DefaultTenant = "acme"
	DefaultQueue  = "ci"

	// CloneSerial is the duplicate OEM serial planted on two distinct
	// positions. STF's own README documents a handset shipping this exact
	// serial, and OEMs ship batches of them. Two of them in one rack is the
	// reason every addressed call in this system carries a devpath: a
	// serial-addressed reset lands on whichever transport adb feels like
	// handing back, which may be a device three hours into someone else's
	// six-hour lease.
	CloneSerial = "0123456789ABCDEF"
)

// SeedOptions describes the farm to write. The zero value is valid and yields
// the default shape above.
type SeedOptions struct {
	// Hosts, HubsPerHost and SlotsPerHub describe the physical tree. Every
	// position gets a slot row whether or not a device is plugged into it,
	// because the slot is the primary physical object: it is what a human can
	// find in a rack and what a power switch acts on.
	Hosts       int
	HubsPerHost int
	SlotsPerHub int

	// Devices caps how many of those positions are occupied. Zero means every
	// slot has a device; a smaller number leaves the trailing positions empty,
	// which is what a real rack looks like halfway through a build-out.
	Devices int

	Rack   string
	Pool   string
	Tenant string
	Queue  string
}

func (o *SeedOptions) applyDefaults() {
	if o.Hosts <= 0 {
		o.Hosts = DefaultHosts
	}
	if o.HubsPerHost <= 0 {
		o.HubsPerHost = DefaultHubsPerHost
	}
	if o.SlotsPerHub <= 0 {
		o.SlotsPerHub = DefaultSlotsPerHub
	}
	// port_number and hubs.port_count both CHECK 1..32, and the bus/port
	// derivation below gives every hub its own (bus, port) pair only while
	// there are at most eight of them per host.
	if o.SlotsPerHub > 32 {
		o.SlotsPerHub = 32
	}
	if o.HubsPerHost > 8 {
		o.HubsPerHost = 8
	}
	total := o.Hosts * o.HubsPerHost * o.SlotsPerHub
	if o.Devices <= 0 || o.Devices > total {
		o.Devices = total
	}
	if o.Rack == "" {
		o.Rack = DefaultRack
	}
	if o.Pool == "" {
		o.Pool = DefaultPool
	}
	if o.Tenant == "" {
		o.Tenant = DefaultTenant
	}
	if o.Queue == "" {
		o.Queue = DefaultQueue
	}
}

// SeedResult reports what the seed put in the database, so the caller can print
// a banner that names the things worth looking at.
type SeedResult struct {
	Rack       string
	Hosts      []string
	Hubs       int
	Slots      int
	Devices    int
	EmptySlots int

	Pool   string
	Tenant string
	Queue  string

	// ClonePositions are the two rack_slot labels sharing CloneSerial.
	ClonePositions []string

	// DegradedPositions are devices seeded as reachable but flaky.
	DegradedPositions []string

	// FaultyHost and FaultyHub name the hub seeded with several unhealthy
	// devices, so the fleet grid's correlation banner has something TRUE to
	// show on a cold start. FaultyHubUnhealthy is how many of its devices are
	// unhealthy.
	FaultyHost         string
	FaultyHub          string
	FaultyHubDevices   int
	FaultyHubUnhealthy int
}

// Seed writes a realistic farm into Postgres and is idempotent.
//
// Idempotency here is structural rather than defensive: every attribute of
// every row is a pure function of the position it belongs to (host, hub,
// port), and every insert names the natural key the schema already enforces —
// hosts.id, (host_id, root_bus), (host_id, usb_path) for hubs and slots,
// devices.farm_uid. Re-running it against a live farm therefore converges to
// the same rows instead of accumulating duplicates or shuffling identities.
//
// Two columns are deliberately left alone once they exist:
//
//   - hosts.adb_endpoint, because [Run] repoints it at a live fake ADB server
//     and a re-seed must not send the watchdog back to a dead address;
//   - farm.device_runtime as a whole (ON CONFLICT DO NOTHING), because health
//     belongs to the watchdog from the moment it first runs. The seeded health
//     distribution is a starting position, not a fact to be restored.
func Seed(ctx context.Context, pool *pgxpool.Pool, opts SeedOptions) (SeedResult, error) {
	opts.applyDefaults()
	plan := planFarm(opts)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return SeedResult{}, fmt.Errorf("demo: seed begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := seedTenancy(ctx, tx, opts); err != nil {
		return SeedResult{}, err
	}
	for i := range plan.Hosts {
		if err := seedHost(ctx, tx, opts, &plan.Hosts[i]); err != nil {
			return SeedResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SeedResult{}, fmt.Errorf("demo: seed commit: %w", err)
	}
	return plan.result(opts), nil
}

// ---------------------------------------------------------------------
// Tenancy
// ---------------------------------------------------------------------

func seedTenancy(ctx context.Context, tx pgx.Tx, opts SeedOptions) error {
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO farm.racks (id, location, notes) VALUES ($1,$2,$3)
		    ON CONFLICT (id) DO NOTHING`,
			[]any{opts.Rack, "lab-1 / row A", "simulated rack — see internal/demo"}},
		{`INSERT INTO farm.pools (id, description) VALUES ($1,$2)
		    ON CONFLICT (id) DO NOTHING`,
			[]any{opts.Pool, "general purpose Android devices"}},
		{`INSERT INTO farm.tenants (id, name, max_devices) VALUES ($1,$2,$3)
		    ON CONFLICT (id) DO NOTHING`,
			[]any{opts.Tenant, "Acme Mobile", 0}},
		{`INSERT INTO farm.queues (id, tenant_id, priority, max_devices) VALUES ($1,$2,$3,$4)
		    ON CONFLICT (id) DO NOTHING`,
			[]any{opts.Queue, opts.Tenant, 100, 0}},
		// A profile, because a reset tier has no meaning without one: 'medium'
		// is defined as "uninstall everything this profile does NOT own", so
		// with no profile there is nothing to reset against and
		// GET /api/v1/specs/resets can only answer 404.
		{`INSERT INTO farm.profiles (id, description, packages) VALUES ($1,$2,$3)
		    ON CONFLICT (id) DO NOTHING`,
			[]any{opts.Pool, "baseline packages every device in this pool keeps",
				[]string{"com.android.chrome", "com.acme.harness"}}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("demo: seed tenancy: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Topology and devices
// ---------------------------------------------------------------------

func seedHost(ctx context.Context, tx pgx.Tx, opts SeedOptions, h *hostPlan) error {
	const hostSQL = `
INSERT INTO farm.hosts (id, rack_id, rack_unit, adb_endpoint, kernel_release, agent_version, admin_state)
VALUES ($1,$2,$3,$4,$5,$6,'enabled')
ON CONFLICT (id) DO UPDATE
   SET rack_id        = EXCLUDED.rack_id,
       rack_unit      = EXCLUDED.rack_unit,
       kernel_release = EXCLUDED.kernel_release,
       agent_version  = EXCLUDED.agent_version`
	// adb_endpoint is absent from the DO UPDATE list on purpose: the runner
	// owns it once a fake ADB server is listening, and a re-seed that reset it
	// to this placeholder would point the watchdog at nothing.
	if _, err := tx.Exec(ctx, hostSQL,
		h.ID, opts.Rack, h.RackUnit, h.Endpoint, h.Kernel, h.Agent); err != nil {
		return fmt.Errorf("demo: seed host %s: %w", h.ID, err)
	}

	for _, bus := range h.Buses {
		const ctlSQL = `
INSERT INTO farm.controllers (host_id, pci_addr, kind, root_bus)
VALUES ($1,$2,'xhci',$3)
ON CONFLICT (host_id, root_bus) DO NOTHING`
		pci := fmt.Sprintf("0000:00:%02x.0", 0x14+bus-3)
		if _, err := tx.Exec(ctx, ctlSQL, h.ID, pci, bus); err != nil {
			return fmt.Errorf("demo: seed controller %s bus %d: %w", h.ID, bus, err)
		}
	}

	for i := range h.Hubs {
		if err := seedHub(ctx, tx, opts, h, &h.Hubs[i]); err != nil {
			return err
		}
	}
	return nil
}

func seedHub(ctx context.Context, tx pgx.Tx, opts SeedOptions, h *hostPlan, hb *hubPlan) error {
	// farm.power_domains carries no natural key, so notes holds a stable one.
	// A power domain models what a single switch actually controls: on the
	// ganged hub below, "power-cycle this device" really means "power-cycle
	// these seven", which is why the API must refuse it while any of the seven
	// holds a live lease.
	const pdSQL = `
INSERT INTO farm.power_domains (host_id, kind, control, control_addr, notes)
SELECT $1,$2,$3,$4,$5
 WHERE NOT EXISTS (SELECT 1 FROM farm.power_domains p WHERE p.notes = $5)`
	if _, err := tx.Exec(ctx, pdSQL,
		h.ID, hb.Power.Kind, hb.Power.Control, nullable(hb.Power.Addr), hb.Power.Notes); err != nil {
		return fmt.Errorf("demo: seed power domain %s: %w", hb.Power.Notes, err)
	}
	var powerID int64
	if err := tx.QueryRow(ctx,
		`SELECT id FROM farm.power_domains WHERE notes = $1`, hb.Power.Notes).Scan(&powerID); err != nil {
		return fmt.Errorf("demo: read power domain %s: %w", hb.Power.Notes, err)
	}

	const hubSQL = `
INSERT INTO farm.hubs (host_id, controller_id, usb_path, model, port_count, vbus_switchable)
SELECT $1, c.id, $2, $3, $4, $5
  FROM farm.controllers c
 WHERE c.host_id = $1 AND c.root_bus = $6
ON CONFLICT (host_id, usb_path) DO UPDATE
   SET controller_id   = EXCLUDED.controller_id,
       model           = EXCLUDED.model,
       port_count      = EXCLUDED.port_count,
       vbus_switchable = EXCLUDED.vbus_switchable
RETURNING id`
	var hubID int64
	if err := tx.QueryRow(ctx, hubSQL,
		h.ID, hb.USB, hb.Model, hb.Ports, hb.Vbus, hb.Bus).Scan(&hubID); err != nil {
		return fmt.Errorf("demo: seed hub %s/%s: %w", h.ID, hb.USB, err)
	}

	for i := range hb.Slots {
		if err := seedSlot(ctx, tx, opts, h, hb, hubID, powerID, &hb.Slots[i]); err != nil {
			return err
		}
	}
	return nil
}

func seedSlot(ctx context.Context, tx pgx.Tx, opts SeedOptions, h *hostPlan, hb *hubPlan,
	hubID, powerID int64, sl *slotPlan) error {

	const slotSQL = `
INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path, topo_path, rack_slot, state)
VALUES ($1,$2,$3,$4,$5,$6::ltree,$7,'active')
ON CONFLICT (host_id, usb_path) DO UPDATE
   SET hub_id          = EXCLUDED.hub_id,
       power_domain_id = EXCLUDED.power_domain_id,
       port_number     = EXCLUDED.port_number,
       topo_path       = EXCLUDED.topo_path,
       rack_slot       = EXCLUDED.rack_slot
RETURNING id`
	var slotID int64
	if err := tx.QueryRow(ctx, slotSQL,
		h.ID, hubID, powerID, sl.Port, sl.USB, sl.Topo, sl.RackSlot).Scan(&slotID); err != nil {
		return fmt.Errorf("demo: seed slot %s/%s: %w", h.ID, sl.USB, err)
	}
	sl.SlotID = slotID

	d := sl.Device
	if d == nil {
		return nil // an empty position; the slot still exists and is still schedulable-looking
	}

	// admin_state is absent from the DO UPDATE list: an operator who
	// quarantined or retired a device must not have that decision undone by a
	// re-seed. Likewise current_lease_id and fence_floor, which belong to the
	// lease machinery alone.
	const devSQL = `
INSERT INTO farm.devices (farm_uid, adb_serial, serial_ambiguous, manufacturer, model, product,
                          device_codename, android_release, sdk_int, abis, build_fingerprint,
                          current_slot_id, host_id, pool_id, labels)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb)
ON CONFLICT (farm_uid) DO UPDATE
   SET adb_serial        = EXCLUDED.adb_serial,
       serial_ambiguous  = EXCLUDED.serial_ambiguous,
       manufacturer      = EXCLUDED.manufacturer,
       model             = EXCLUDED.model,
       product           = EXCLUDED.product,
       device_codename   = EXCLUDED.device_codename,
       android_release   = EXCLUDED.android_release,
       sdk_int           = EXCLUDED.sdk_int,
       abis              = EXCLUDED.abis,
       build_fingerprint = EXCLUDED.build_fingerprint,
       current_slot_id   = EXCLUDED.current_slot_id,
       host_id           = EXCLUDED.host_id,
       pool_id           = EXCLUDED.pool_id,
       labels            = EXCLUDED.labels,
       updated_at        = now()
RETURNING id::text`
	var deviceID string
	if err := tx.QueryRow(ctx, devSQL,
		d.FarmUID, d.Serial, d.Ambiguous, d.Manufacturer, d.Model, d.Product,
		d.Codename, d.Release, d.SDK, d.ABIs, d.Fingerprint,
		slotID, h.ID, opts.Pool, d.Labels).Scan(&deviceID); err != nil {
		return fmt.Errorf("demo: seed device %s: %w", d.FarmUID, err)
	}
	d.DeviceID = deviceID

	// DO NOTHING, not DO UPDATE: health is the watchdog's column. The seeded
	// distribution is only a cold-start position, and stomping observed health
	// on every re-seed would make the fleet grid lie.
	const rtSQL = `
INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health, health_since,
                                 battery_pct, battery_temp_dc, charge_gate, boot_completed,
                                 consec_good, consec_bad, flap_credits, flap_updated_at, last_seen_at)
VALUES ($1,$2,$3,$4,$5, now(), $6,$7,'on',$8,$9,$10,$11::float8, now(), now())
ON CONFLICT (device_id) DO NOTHING`
	// The token bucket starts where the seeded verdict says it should: a
	// degraded device is degraded precisely because it has spent its credits,
	// and handing it a full bucket would make the watchdog call it healthy on
	// its first good probe.
	good, bad, credits := 12, 0, 10.0
	switch d.Health {
	case "healthy":
	case "degraded":
		good, bad, credits = 0, 2, 0.5
	default:
		good, bad, credits = 0, 3, 3.0
	}
	if _, err := tx.Exec(ctx, rtSQL,
		deviceID, h.ID, slotID, d.ADBState, d.Health,
		d.Battery, d.TempDC, d.ADBState == "device", good, bad, credits); err != nil {
		return fmt.Errorf("demo: seed runtime %s: %w", d.FarmUID, err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------
// The plan: every attribute is a pure function of the physical position
// ---------------------------------------------------------------------

type farmPlan struct {
	Hosts []hostPlan

	clonePositions    []string
	degradedPositions []string
	faultyHost        string
	faultyHub         string
	faultyDevices     int
	faultyUnhealthy   int
	slots             int
	hubs              int
	devices           int
}

type hostPlan struct {
	ID       string
	RackUnit int
	Endpoint string
	Kernel   string
	Agent    string
	Buses    []int
	Hubs     []hubPlan
}

type hubPlan struct {
	Index int
	Bus   int
	Port  int
	USB   string
	Model string
	Ports int
	Vbus  bool
	Power powerPlan
	Slots []slotPlan
}

type powerPlan struct {
	Kind    string
	Control string
	Addr    string
	Notes   string
}

type slotPlan struct {
	Port     int
	USB      string
	Topo     string
	RackSlot string
	SlotID   int64
	Device   *devicePlan
}

type devicePlan struct {
	DeviceID     string
	FarmUID      string
	Serial       string
	Ambiguous    bool
	Manufacturer string
	Model        string
	Product      string
	Codename     string
	Release      string
	SDK          int
	ABIs         []string
	Fingerprint  string
	Battery      int
	TempDC       int
	ADBState     string
	Health       string
	Labels       string
}

// deviceModel is one entry of the handset catalogue. Real model names,
// codenames and SDK levels, because a fleet grid full of "Test Device 17"
// teaches an operator nothing about what the grid will look like in
// production.
type deviceModel struct {
	Manufacturer string
	Model        string
	Product      string
	Codename     string
	Release      string
	SDK          int
	BuildID      string
	ABIs         []string
}

var catalogue = []deviceModel{
	{"Google", "Pixel 6a", "bluejay", "bluejay", "14", 34, "UP1A.231105.001", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Google", "Pixel 7", "panther", "panther", "15", 35, "AP3A.241005.015", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Google", "Pixel 4a", "sunfish", "sunfish", "13", 33, "TQ3A.230901.001", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Samsung", "Galaxy S22", "r0qxxx", "r0q", "14", 34, "UP1A.231005.007", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Samsung", "Galaxy A54", "a54xnaxx", "a54x", "14", 34, "UP1A.231005.007", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Xiaomi", "Redmi Note 12", "tapas", "tapas", "13", 33, "TKQ1.221114.001", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Motorola", "moto g82 5G", "rhodep", "rhode", "13", 33, "T1SRS33.33-16-8", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
	{"Sony", "Xperia 10 IV", "XQ-CC54", "pdx225", "14", 34, "68.1.A.3.119", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
}

var hubModels = []string{
	"StarTech ST7300USBM 7-port",
	"Plugable USB3-HUB7BC",
	"Anker A7515 7-port",
	"AmazonBasics HU3770V1 7-port",
}

// serialPrefix keeps the generated serials shaped like the manufacturer's own.
var serialPrefix = map[string]string{
	"Google":   "3",
	"Samsung":  "R5",
	"Xiaomi":   "X1",
	"Motorola": "ZY",
	"Sony":     "CQ",
}

func planFarm(opts SeedOptions) farmPlan {
	var p farmPlan
	placed := 0

	// The deliberately broken hub is the LAST hub of the LAST host, and the
	// deliberately flaky pair sits on the FIRST hub of the FIRST host, so the
	// two stories stay separable in the UI.
	faultyHostIdx, faultyHubIdx := opts.Hosts-1, opts.HubsPerHost-1

	for hi := 0; hi < opts.Hosts; hi++ {
		h := hostPlan{
			ID:       fmt.Sprintf("h%02d", hi+1),
			RackUnit: 4 + hi*6,
			// Overwritten by the runner with a live fake ADB server's address.
			// The default adb port is the honest placeholder for a host nobody
			// has contacted yet.
			Endpoint: "127.0.0.1:5037",
			Kernel:   "6.8.0-45-generic",
			Agent:    "farmd-node/0.1.0",
		}
		for hub := 0; hub < opts.HubsPerHost; hub++ {
			// Two hubs per root controller, which is what a two-card host
			// looks like: usb_paths 3-1, 3-2, 4-1, 4-2.
			bus, hubPort := 3+hub/2, 1+hub%2
			if !containsInt(h.Buses, bus) {
				h.Buses = append(h.Buses, bus)
			}
			hb := hubPlan{
				Index: hub,
				Bus:   bus,
				Port:  hubPort,
				USB:   fmt.Sprintf("%d-%d", bus, hubPort),
				Model: hubModels[hub%len(hubModels)],
				Ports: opts.SlotsPerHub,
			}
			hb.Power = planPower(h.ID, hub, hb.USB)
			hb.Vbus = hb.Power.Kind != "none"

			for port := 1; port <= opts.SlotsPerHub; port++ {
				usb := fmt.Sprintf("%s.%d", hb.USB, port)
				sl := slotPlan{
					Port:     port,
					USB:      usb,
					Topo:     topoPath(h.ID, bus, hb.USB, usb),
					RackSlot: fmt.Sprintf("R%s-U%02d-H%d-P%d", trimRack(opts.Rack), h.RackUnit, hub+1, port),
				}
				if placed < opts.Devices {
					sl.Device = planDevice(&p, opts, h.ID, hi, hub, port, usb, sl.RackSlot,
						hi == faultyHostIdx && hub == faultyHubIdx,
						hi == 0 && hub == 0)
					placed++
					p.devices++
					if hi == faultyHostIdx && hub == faultyHubIdx {
						p.faultyDevices++
						if sl.Device.Health != "healthy" {
							p.faultyUnhealthy++
						}
					}
				}
				hb.Slots = append(hb.Slots, sl)
				p.slots++
			}
			if hi == faultyHostIdx && hub == faultyHubIdx {
				p.faultyHost, p.faultyHub = h.ID, hb.USB
			}
			h.Hubs = append(h.Hubs, hb)
			p.hubs++
		}
		p.Hosts = append(p.Hosts, h)
	}
	return p
}

// planPower gives each hub a different switching story, because the API's
// power endpoint has to refuse the ganged case and an all-per_port farm would
// never exercise that refusal.
func planPower(hostID string, hubIdx int, hubUSB string) powerPlan {
	notes := fmt.Sprintf("demo:%s:%s", hostID, hubUSB)
	switch hubIdx % 4 {
	case 2:
		// One switch for the whole hub: cutting VBUS for one device cuts it
		// for all seven.
		return powerPlan{Kind: "ganged", Control: "smarthub",
			Addr: fmt.Sprintf("smarthub://%s/%s", hostID, hubUSB), Notes: notes}
	case 3:
		// A dumb hub. Nothing to switch; recovery stops at the soft tiers.
		return powerPlan{Kind: "none", Control: "none", Notes: notes}
	default:
		return powerPlan{Kind: "per_port", Control: "uhubctl",
			Addr: fmt.Sprintf("uhubctl://%s/%s", hostID, hubUSB), Notes: notes}
	}
}

// planDevice derives every attribute from the position, so the seed is a pure
// function and re-running it converges instead of drifting.
func planDevice(p *farmPlan, opts SeedOptions, hostID string, hostIdx, hubIdx, port int,
	usb, rackSlot string, onFaultyHub, onFirstHub bool) *devicePlan {

	key := hostID + "/" + usb
	h := hash64(key)
	m := catalogue[int(h%uint64(len(catalogue)))]

	d := &devicePlan{
		// farm_uid is branded onto the device itself in production. Here it is
		// the md5-shaped hash of the position, which satisfies the schema's
		// '^df-[0-9a-f]{32}$' and stays stable across re-seeds.
		FarmUID:      "df-" + hex32(key),
		Serial:       plausibleSerial(m.Manufacturer, h),
		Manufacturer: m.Manufacturer,
		Model:        m.Model,
		Product:      m.Product,
		Codename:     m.Codename,
		Release:      m.Release,
		SDK:          m.SDK,
		ABIs:         m.ABIs,
		Fingerprint: fmt.Sprintf("%s/%s/%s:%s/%s/%d:user/release-keys",
			strings.ToLower(m.Manufacturer), m.Product, m.Codename, m.Release, m.BuildID, 10000000+h%9000000),
		Battery:  40 + int(h%61),
		TempDC:   250 + int(h%120), // decidegrees: 25.0 C .. 37.0 C
		ADBState: "device",
		Health:   "healthy",
	}
	d.Labels = deviceLabels(m, hostIdx, hubIdx)

	// The duplicate-serial trap: two devices, two positions, one serial.
	if onFirstHub && (port == 1 || port == 2) {
		d.Serial = CloneSerial
		d.Ambiguous = true
		p.clonePositions = append(p.clonePositions, rackSlot)
	}

	// One hub with several unhealthy devices, so the correlation banner has
	// something true to show the moment the UI opens — before the runner has
	// broken anything itself.
	if onFaultyHub {
		switch port {
		case 1, 2, 4:
			d.ADBState, d.Health = "offline", "offline"
		case 3:
			d.ADBState, d.Health = "unauthorized", "unauthorized"
		case 5:
			d.ADBState, d.Health = "absent", "missing"
		}
	}

	// A couple of devices that answer but have been misbehaving: reachable,
	// not trusted, and not schedulable. This is what the flap damper leaves
	// behind.
	if onFirstHub && !onFaultyHub && port >= opts.SlotsPerHub-1 && opts.SlotsPerHub >= 4 {
		d.Health = "degraded"
		p.degradedPositions = append(p.degradedPositions, rackSlot)
	}
	return d
}

func deviceLabels(m deviceModel, hostIdx, hubIdx int) string {
	labels := map[string]any{
		"form_factor": "phone",
		"sdk":         strconv.Itoa(m.SDK),
		"vendor":      strings.ToLower(m.Manufacturer),
		// A label an operator would actually select on: the devices wired for
		// battery measurement sit on the hubs with per-port switching.
		"power_metered": hubIdx%4 == 0 || hubIdx%4 == 1,
		"rack_side":     map[bool]string{true: "front", false: "rear"}[hostIdx%2 == 0],
	}
	b, err := json.Marshal(labels)
	if err != nil {
		// The map is a literal of JSON-safe types; a failure here would be a
		// bug in this function, not bad input, so the empty object keeps the
		// NOT NULL column valid rather than aborting a seed over it.
		return "{}"
	}
	return string(b)
}

func (p farmPlan) result(opts SeedOptions) SeedResult {
	res := SeedResult{
		Rack:               opts.Rack,
		Hubs:               p.hubs,
		Slots:              p.slots,
		Devices:            p.devices,
		EmptySlots:         p.slots - p.devices,
		Pool:               opts.Pool,
		Tenant:             opts.Tenant,
		Queue:              opts.Queue,
		ClonePositions:     p.clonePositions,
		DegradedPositions:  p.degradedPositions,
		FaultyHost:         p.faultyHost,
		FaultyHub:          p.faultyHub,
		FaultyHubDevices:   p.faultyDevices,
		FaultyHubUnhealthy: p.faultyUnhealthy,
	}
	for _, h := range p.Hosts {
		res.Hosts = append(res.Hosts, h.ID)
	}
	return res
}

// ---------------------------------------------------------------------
// Small deterministic helpers
// ---------------------------------------------------------------------

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// hex32 renders 32 lowercase hex digits, the shape farm_uid's CHECK demands.
func hex32(s string) string {
	a := hash64("a/" + s)
	b := hash64("b/" + s)
	return fmt.Sprintf("%016x%016x", a, b)
}

func plausibleSerial(manufacturer string, h uint64) string {
	prefix := serialPrefix[manufacturer]
	if prefix == "" {
		prefix = "SN"
	}
	body := strings.ToUpper(strconv.FormatUint(h, 36))
	for len(body) < 12 {
		body = "0" + body
	}
	return prefix + body[:12]
}

// topoPath builds the ltree ancestry, e.g. h01.c3.p3_1.p3_1_4. ltree labels
// admit only alphanumerics and underscore, so both separators in a usb_path
// have to be folded.
func topoPath(hostID string, bus int, hubUSB, slotUSB string) string {
	label := func(s string) string {
		return "p" + strings.NewReplacer("-", "_", ".", "_").Replace(s)
	}
	return fmt.Sprintf("%s.c%d.%s.%s",
		strings.NewReplacer("-", "_", ".", "_").Replace(hostID), bus, label(hubUSB), label(slotUSB))
}

// trimRack turns "r1" into "1" so a rack_slot reads "R1-U04-H2-P3".
func trimRack(rack string) string {
	return strings.ToUpper(strings.TrimPrefix(strings.ToLower(rack), "r"))
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
