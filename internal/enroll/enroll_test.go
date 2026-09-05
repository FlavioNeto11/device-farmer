package enroll

// What these tests protect: one pass of the loop, end to end, against the real
// farm.resolve_device. A phone that was plugged in is adopted, branded, and
// recognised by that brand next time; a tray of clone-serial handsets is
// adopted AND flagged on every colliding row, with the sighting recorded as
// 'ambiguous' rather than as a clean adoption; what cannot be identified is
// recorded and adopted as nothing; and a slot another device row still points
// at is never cleared to make room — that row may be mid-job, and enrollment
// has no standing over a lease.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// farm is one test's slice of the schema: a rack, a host whose adb_endpoint
// is the fake server, and the default pool.
type farm struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context

	hostID string
	poolID string
}

func newFarm(t *testing.T, endpoint string) *farm {
	t.Helper()
	pool := requireDB(t)
	resetSchema(t, pool)
	f := &farm{t: t, pool: pool, ctx: context.Background(), hostID: "h1", poolID: DefaultPoolID}
	f.exec(`INSERT INTO farm.racks (id) VALUES ('r1')`)
	f.exec(`INSERT INTO farm.hosts (id, rack_id, rack_unit, adb_endpoint) VALUES ($1, 'r1', 14, $2)`,
		f.hostID, endpoint)
	f.exec(`INSERT INTO farm.pools (id) VALUES ($1)`, f.poolID)
	return f
}

// slot registers one position through farm.register_slot, the way topology
// discovery does, so the slot carries a real hub and power domain.
func (f *farm) slot(usbPath string) int64 {
	f.t.Helper()
	dot := strings.LastIndex(usbPath, ".")
	if dot < 0 {
		f.t.Fatalf("slot %q has no hub component", usbPath)
	}
	var id int64
	f.scan(&id, `SELECT farm.register_slot($1, $2, $3, $4::int, 'Fixture Hub', 7, false, NULL)`,
		f.hostID, usbPath, usbPath[:dot], usbPath[dot+1:])
	return id
}

// enroller wires the loop to this farm and to a rack, and checks that the
// endpoint it connects to is the one farm.hosts holds.
func (f *farm) enroller(r *rack, probeTimeout time.Duration) *Enroller {
	f.t.Helper()
	e, err := New(Config{
		Pool:         f.pool,
		HostID:       f.hostID,
		PoolID:       f.poolID,
		ProbeTimeout: probeTimeout,
		Connect: func(endpoint string) (Host, error) {
			if endpoint != r.srv.Addr() {
				f.t.Errorf("the enroller connected to %q, farm.hosts says %q", endpoint, r.srv.Addr())
			}
			return r, nil
		},
		Logger: quietLogger(),
	})
	if err != nil {
		f.t.Fatalf("new enroller: %v", err)
	}
	return e
}

func (f *farm) once(e *Enroller) Summary {
	f.t.Helper()
	sum, err := e.EnrollOnce(f.ctx)
	if err != nil {
		f.t.Fatalf("EnrollOnce: %v", err)
	}
	return sum
}

func (f *farm) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func (f *farm) scan(dest any, sql string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(dest); err != nil {
		f.t.Fatalf("query %.60q: %v", sql, err)
	}
}

func (f *farm) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	f.scan(&n, sql, args...)
	return n
}

// observation is what one sighting row says.
type observation struct {
	Resolution string
	DeviceID   string // "" when NULL
	FarmUID    string
	Detail     map[string]any
}

// lastObservation reads the newest sighting of one position.
func (f *farm) lastObservation(usbPath string) observation {
	f.t.Helper()
	var o observation
	var dev, uid *string
	if err := f.pool.QueryRow(f.ctx, `
SELECT resolution, device_id::text, farm_uid, detail
  FROM farm.identity_observations
 WHERE host_id = $1 AND usb_path = $2
 ORDER BY id DESC LIMIT 1`, f.hostID, usbPath).Scan(&o.Resolution, &dev, &uid, &o.Detail); err != nil {
		f.t.Fatalf("read the observation at %s: %v", usbPath, err)
	}
	if dev != nil {
		o.DeviceID = *dev
	}
	if uid != nil {
		o.FarmUID = *uid
	}
	return o
}

func (f *farm) detailString(o observation, key string) string {
	v, _ := o.Detail[key].(string)
	return v
}

// ---------------------------------------------------------------------------

// TestEnrollOnceAdoptsBrandsAndThenRecognisesByBrand is the whole loop for
// one phone: sighting recorded, adopted with its properties, branded over the
// wire with the uid farm.resolve_device minted, and on the next pass matched
// by that brand with no second row and no second event.
//
// Falsify: in probeAnswer's caller — the rack — stop re-scripting the probe
// after a write (delete the probeService Respond in setBrand); the second pass
// then resolves by hw_fingerprint instead of branded_uid.
func TestEnrollOnceAdoptsBrandsAndThenRecognisesByBrand(t *testing.T) {
	const devpath, usbPath = "usb:3-1.4.1", "3-1.4.1"
	r := newRack(t)
	f := newFarm(t, r.srv.Addr())
	slotID := f.slot(usbPath)
	r.phone(devpath, pixelSerial, pixelProps(pixelSerial))
	e := f.enroller(r, 2*time.Second)

	sum := f.once(e)
	if sum.Hosts != 1 || sum.Errors != 0 || sum.ByResolution[ResolutionAdoptedNew] != 1 || sum.Branded != 1 {
		t.Fatalf("first pass: %s (%+v)", sum.String(), sum)
	}

	var devID, uid, manufacturer, model string
	var sdk int
	var abis []string
	var slot *int64
	var fpLen int
	if err := f.pool.QueryRow(f.ctx, `
SELECT id::text, farm_uid, manufacturer, model, sdk_int, abis, current_slot_id, length(hw_fingerprint)
  FROM farm.devices WHERE host_id = $1`, f.hostID).
		Scan(&devID, &uid, &manufacturer, &model, &sdk, &abis, &slot, &fpLen); err != nil {
		t.Fatalf("exactly one device row was expected: %v", err)
	}
	if manufacturer != "Google" || model != "Pixel 6a" || sdk != 34 || fpLen == 0 {
		t.Errorf("device row = %s %s sdk=%d fp=%d bytes", manufacturer, model, sdk, fpLen)
	}
	if strings.Join(abis, ",") != "arm64-v8a,armeabi-v7a,armeabi" {
		t.Errorf("abis = %v; farm.resolve_device does not store them, the loop must", abis)
	}
	if slot == nil || *slot != slotID {
		t.Errorf("current_slot_id = %v, want %d", slot, slotID)
	}
	if got := r.brandOf(devpath); got != uid {
		t.Fatalf("the phone carries %q, the row says %q", got, uid)
	}

	obs := f.lastObservation(usbPath)
	if obs.Resolution != ResolutionAdoptedNew || obs.DeviceID != devID {
		t.Errorf("observation = %+v, want adopted_new for %s", obs, devID)
	}
	if f.detailString(obs, "brand") != string(BrandWritten) || f.detailString(obs, "uid") != uid {
		t.Errorf("the observation does not record the brand written: %v", obs.Detail)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_adopted' AND device_id = $1::uuid
	                  AND slot_id = $2 AND actor = $3 AND (detail->>'ambiguous')::boolean = false`,
		devID, slotID, DefaultComponent); n != 1 {
		t.Errorf("%d device_adopted events, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.slot_occupancy WHERE slot_id = $1 AND device_id = $2::uuid AND until IS NULL`,
		slotID, devID); n != 1 {
		t.Errorf("%d open occupancy rows, want 1", n)
	}

	// Second sighting: the brand is the whole answer.
	sum = f.once(e)
	if sum.ByResolution[ResolutionBrandedUID] != 1 || sum.Branded != 0 || sum.Errors != 0 || sum.Seen() != 1 {
		t.Fatalf("second pass: %s (%+v)", sum.String(), sum)
	}
	obs = f.lastObservation(usbPath)
	if obs.Resolution != ResolutionBrandedUID || obs.DeviceID != devID || obs.FarmUID != uid {
		t.Errorf("second observation = %+v, want branded_uid for %s carrying %s", obs, devID, uid)
	}
	if n := f.count(`SELECT count(*) FROM farm.devices`); n != 1 {
		t.Errorf("%d device rows after two sightings of one phone", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_adopted'`); n != 1 {
		t.Errorf("%d device_adopted events after a recognised sighting", n)
	}
	if got := strings.Join(shellServices(r.srv, devpath), ","); got != "probe,read,write,read,probe" {
		t.Errorf("commands = %s: the second pass should be one probe and no brand traffic", got)
	}
	assertNoSerialAddressing(t, r.srv, pixelSerial)
}

// TestEnrollOnceFlagsACloneSerialOnBothRows is the case
// migrations/00011_resolve_ambiguous.sql was written for. Two handsets wear
// the serial STF's README documents; the second one to be seen is adopted
// with its sighting recorded as 'ambiguous', its timeline event says so, and
// serial_ambiguous is set on BOTH rows. Next pass, both resolve by brand.
//
// Falsify: in resolve, pass "" instead of id.Serial to farm.resolve_device —
// the collision is then never visible to the ladder and no row is flagged.
func TestEnrollOnceFlagsACloneSerialOnBothRows(t *testing.T) {
	r := newRack(t, fakeadb.TwoClonesFixture())
	f := newFarm(t, r.srv.Addr())
	slotA := f.slot(USBPath(fakeadb.CloneDevpathA))
	slotB := f.slot(USBPath(fakeadb.CloneDevpathB))
	// The USB descriptor serial is the bogus shared one; each handset still
	// knows its own ro.boot.serialno, so the two fingerprints differ and the
	// only thing they share is what the ADB server reports.
	r.script(fakeadb.CloneDevpathA, pixelProps("A1111FDF6001XX"))
	r.script(fakeadb.CloneDevpathB, pixelProps("B2222FDF6002YY"))
	e := f.enroller(r, 2*time.Second)

	sum := f.once(e)
	if sum.Errors != 0 || sum.ByResolution[ResolutionAdoptedNew] != 1 ||
		sum.ByResolution[ResolutionAmbiguous] != 1 || sum.Branded != 2 {
		t.Fatalf("first pass: %s (%+v)", sum.String(), sum)
	}

	if n := f.count(`SELECT count(*) FROM farm.devices WHERE adb_serial = $1`, fakeadb.CloneSerial); n != 2 {
		t.Fatalf("%d device rows carry the clone serial, want 2", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.devices WHERE adb_serial = $1 AND serial_ambiguous`,
		fakeadb.CloneSerial); n != 2 {
		t.Fatalf("serial_ambiguous is set on %d of the 2 clones", n)
	}

	obsB := f.lastObservation(USBPath(fakeadb.CloneDevpathB))
	if obsB.Resolution != ResolutionAmbiguous || obsB.DeviceID == "" {
		t.Errorf("the second clone's sighting = %+v, want ambiguous WITH a device", obsB)
	}
	obsA := f.lastObservation(USBPath(fakeadb.CloneDevpathA))
	if obsA.Resolution != ResolutionAdoptedNew || obsA.DeviceID == obsB.DeviceID {
		t.Errorf("the first clone's sighting = %+v, want a clean adoption of a different row", obsA)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_adopted' AND slot_id = $1
	                  AND (detail->>'ambiguous')::boolean`, slotB); n != 1 {
		t.Errorf("%d contested device_adopted events for the second clone, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_adopted' AND slot_id = $1
	                  AND NOT (detail->>'ambiguous')::boolean`, slotA); n != 1 {
		t.Errorf("%d clean device_adopted events for the first clone, want 1", n)
	}
	if a, b := r.brandOf(fakeadb.CloneDevpathA), r.brandOf(fakeadb.CloneDevpathB); a == "" || b == "" || a == b {
		t.Errorf("clones branded %q and %q, want two distinct uids", a, b)
	}

	// Branded, the two are told apart by the only evidence that cannot
	// collide: the uid this farm wrote.
	sum = f.once(e)
	if sum.ByResolution[ResolutionBrandedUID] != 2 || sum.Seen() != 2 || sum.Errors != 0 {
		t.Fatalf("second pass: %s (%+v)", sum.String(), sum)
	}
	if n := f.count(`SELECT count(*) FROM farm.devices`); n != 2 {
		t.Errorf("%d device rows after the clones were recognised, want 2", n)
	}
	assertNoSerialAddressing(t, r.srv, fakeadb.CloneSerial)
}

// TestEnrollOnceRecordsWhatItCannotResolve: an unauthorized phone, a phone at
// a position topology has not registered, and a phone that will not answer
// are three sightings and zero identities. Each is recorded with its reason,
// none is adopted, none is branded, and none is an error.
//
// Falsify: delete the `case !hasSlot` arm in record — the unregistered
// position is then handed to farm.resolve_device, which raises, and the pass
// reports an error.
func TestEnrollOnceRecordsWhatItCannotResolve(t *testing.T) {
	const (
		unauthorized = "usb:3-1.4.1"
		unregistered = "usb:3-1.4.2"
		silent       = "usb:3-1.4.3"
	)
	r := newRack(t)
	f := newFarm(t, r.srv.Addr())
	f.slot(USBPath(unauthorized))
	f.slot(USBPath(silent))

	r.phone(unauthorized, "SERUNAUTH", pixelProps("SERUNAUTH"))
	r.srv.SetState(unauthorized, fakeadb.StateUnauthorized)
	r.phone(unregistered, "SERNOSLOT", pixelProps("SERNOSLOT"))
	r.phone(silent, "SERSILENT", pixelProps("SERSILENT"))
	r.srv.Inject(fakeadb.Fault{Match: "shell", Devpath: silent, Kind: fakeadb.FaultHang})

	sum := f.once(f.enroller(r, 400*time.Millisecond))
	if sum.Errors != 0 || sum.Seen() != 3 ||
		sum.ByResolution[ResolutionUnreadable] != 2 || sum.ByResolution[ResolutionPending] != 1 {
		t.Fatalf("pass: %s (%+v)", sum.String(), sum)
	}

	want := map[string][2]string{
		USBPath(unauthorized): {ResolutionUnreadable, "unauthorized"},
		USBPath(unregistered): {ResolutionPending, "no_slot_registered"},
		USBPath(silent):       {ResolutionUnreadable, "probe_timeout"},
	}
	for usbPath, w := range want {
		obs := f.lastObservation(usbPath)
		if obs.Resolution != w[0] || f.detailString(obs, "reason") != w[1] || obs.DeviceID != "" {
			t.Errorf("%s: observation = %+v, want %s/%s and no device", usbPath, obs, w[0], w[1])
		}
	}
	if n := f.count(`SELECT count(*) FROM farm.devices`); n != 0 {
		t.Errorf("%d devices were adopted from sightings that identified nothing", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.events`); n != 0 {
		t.Errorf("%d events were written for sightings that changed nothing", n)
	}
	for _, dp := range []string{unauthorized, unregistered, silent} {
		if got := strings.Join(shellServices(r.srv, dp), ","); strings.Contains(got, "write") {
			t.Errorf("%s was branded: %s", dp, got)
		}
	}
	if n := shellsTo(r.srv, unauthorized); n != 0 {
		t.Errorf("an unauthorized phone was sent %d shell commands", n)
	}
}

// TestEnrollOnceWillNotEvictAnotherDeviceFromItsSlot: a different phone shows
// up in a slot whose previous occupant's row still points at it. Adoption
// would need that pointer cleared, and the departing device may be holding a
// live lease this second. The sighting is recorded as a conflict for a human;
// the existing row is untouched and the newcomer is neither adopted nor
// branded.
//
// Falsify: in classifyResolveError, return ResolutionPending from the
// unique-violation arm — the conflict is then indistinguishable from a
// missing slot.
func TestEnrollOnceWillNotEvictAnotherDeviceFromItsSlot(t *testing.T) {
	const devpath, usbPath = "usb:3-1.4.1", "3-1.4.1"
	r := newRack(t)
	f := newFarm(t, r.srv.Addr())
	slotID := f.slot(usbPath)

	// The row that was enrolled here last week and is, for all this loop
	// knows, mid-job right now.
	var incumbent string
	f.scan(&incumbent, `
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
VALUES ($1, 'OLDSERIAL0001', $2, $3, $4, 'Old Phone') RETURNING id::text`,
		uidA, f.poolID, f.hostID, slotID)

	r.phone(devpath, pixelSerial, pixelProps(pixelSerial))
	sum := f.once(f.enroller(r, 2*time.Second))

	if sum.ByResolution[ResolutionConflict] != 1 || sum.Errors != 1 || sum.Branded != 0 {
		t.Fatalf("pass: %s (%+v)", sum.String(), sum)
	}
	obs := f.lastObservation(usbPath)
	if obs.Resolution != ResolutionConflict || f.detailString(obs, "reason") != "slot_occupied_by_another_device" || obs.DeviceID != "" {
		t.Errorf("observation = %+v, want a conflict naming no device", obs)
	}
	var stillThere *int64
	f.scan(&stillThere, `SELECT current_slot_id FROM farm.devices WHERE id = $1::uuid`, incumbent)
	if stillThere == nil || *stillThere != slotID {
		t.Errorf("the incumbent's slot pointer was changed to %v", stillThere)
	}
	if n := f.count(`SELECT count(*) FROM farm.devices`); n != 1 {
		t.Errorf("%d device rows, want only the incumbent", n)
	}
	if n := f.count(`SELECT count(*) FROM farm.events WHERE kind = 'device_adopted'`); n != 0 {
		t.Errorf("%d device_adopted events for a phone that was refused", n)
	}
	if got := strings.Join(shellServices(r.srv, devpath), ","); got != "probe" {
		t.Errorf("commands = %s, want only the probe: a refused phone is not branded", got)
	}
}
