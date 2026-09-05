package e2e

// The only thing this harness fakes: the hardware.
//
// One test/fakeadb server per seeded host, in this process, on a real loopback
// socket, speaking the real ADB host protocol — and farm.hosts.adb_endpoint
// pointed at the address it actually got. The roles are separate processes and
// have no idea any of this is simulated; they dial a TCP address out of the
// database and get frames back.
//
// The device table is read back OUT of the database rather than generated
// beside the seeder. That is deliberate: the fake must agree with farm.slots
// about every devpath, and the only way to be sure of that is to ask the same
// rows the scheduler and the jobrunner will ask. A fixture built from the same
// arithmetic as the seeder would agree with the seeder's INTENT, which is not
// the same thing and drifts the first time either side changes.

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/watchdog"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// simDevice is one seeded handset, as the fake hardware needs to know it.
type simDevice struct {
	hostID   string
	devpath  string
	serial   string
	model    string
	product  string
	codename string
	adbState string
	rackSlot string
}

// startHardware brings up the fake hosts and repoints farm.hosts at them.
func (f *farm) startHardware(t *testing.T) {
	t.Helper()

	byHost := make(map[string][]simDevice)
	for _, d := range f.fleet(t) {
		byHost[d.hostID] = append(byHost[d.hostID], d)
	}

	for _, host := range f.seed.Hosts {
		// EVERY seeded host gets a server, including one the seeder left
		// empty. Skipping it used to leave farm.hosts.adb_endpoint at the
		// seeder's placeholder 127.0.0.1:5037 — which is the default port of
		// a REAL adb daemon. A developer with a phone plugged in, or with
		// Android Studio running, would have had the watchdog and the
		// jobrunner dial their actual hardware from a test, and the failure
		// would have read as a farm bug.
		//
		// An empty host is not a hypothetical: Devices caps how many slots are
		// occupied, so any scenario that seeds fewer devices than positions
		// gets one, and that is what a half-built rack looks like.
		devs := byHost[host]
		srv, err := fakeadb.New()
		if err != nil {
			t.Fatalf("starting the fake adb server for %s: %v", host, err)
		}
		f.adb[host] = srv

		for _, d := range devs {
			srv.Add(fakeadb.Device{
				Serial:   d.serial,
				Devpath:  d.devpath,
				Model:    d.model,
				Product:  d.product,
				Codename: d.codename,
				State:    wireState(d.adbState),
			})
		}
		installShellScripts(srv, devs)

		// host_epoch increments on every adb server restart, because a
		// transport id is meaningless without the epoch that minted it: adb
		// reuses small integers across restarts.
		const q = `
UPDATE farm.hosts
   SET adb_endpoint = $2, host_epoch = host_epoch + 1, last_seen_at = now()
 WHERE id = $1
RETURNING host_epoch`
		var epoch int64
		if err := f.pool.QueryRow(t.Context(), q, host, srv.Addr()).Scan(&epoch); err != nil {
			t.Fatalf("pointing host %s at its fake adb server: %v", host, err)
		}

		// One real round trip before any role depends on the socket, so a
		// scenario that fails later cannot be failing because the hardware
		// never came up.
		version, err := adbwire.New(srv.Addr(), adbwire.WithCallTimeout(5*time.Second)).Version(t.Context())
		if err != nil {
			t.Fatalf("handshaking with the fake adb server for %s at %s: %v", host, srv.Addr(), err)
		}
		t.Logf("host %s: %d simulated devices at %s (adb protocol %d, host_epoch %d)",
			host, len(devs), srv.Addr(), version, epoch)
	}
}

// fleet reads the seeded handsets back out of the database.
func (f *farm) fleet(t *testing.T) []simDevice {
	t.Helper()
	const q = `
SELECT d.host_id, s.adb_devpath, COALESCE(d.adb_serial,''), COALESCE(d.model,''),
       COALESCE(d.product,''), COALESCE(d.device_codename,''), rt.adb_state,
       COALESCE(s.rack_slot,'')
  FROM farm.devices d
  JOIN farm.slots s           ON s.id = d.current_slot_id
  JOIN farm.device_runtime rt ON rt.device_id = d.id
 WHERE d.host_id = ANY($1::text[])
 ORDER BY d.host_id, s.usb_path`

	rows, err := f.pool.Query(t.Context(), q, f.seed.Hosts)
	if err != nil {
		t.Fatalf("reading the seeded fleet: %v", err)
	}
	defer rows.Close()

	var out []simDevice
	for rows.Next() {
		var d simDevice
		if err := rows.Scan(&d.hostID, &d.devpath, &d.serial, &d.model,
			&d.product, &d.codename, &d.adbState, &d.rackSlot); err != nil {
			t.Fatalf("scanning the seeded fleet: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the seeded fleet: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("the seed produced no devices; a farm with no hardware can prove nothing")
	}
	return out
}

// DevicePosition is where a device is plugged in: its host and its devpath.
//
// It exists because everything a scenario wants to do to the hardware is
// addressed by devpath — that is the point of farm.slots.adb_devpath, and the
// seeded clone pair is there to prove a serial cannot do the job.
func (f *farm) DevicePosition(t *testing.T, deviceID string) (host, devpath string) {
	t.Helper()
	const q = `
SELECT d.host_id, s.adb_devpath
  FROM farm.devices d
  JOIN farm.slots s ON s.id = d.current_slot_id
 WHERE d.id = $1::uuid`
	if err := f.pool.QueryRow(t.Context(), q, deviceID).Scan(&host, &devpath); err != nil {
		t.Fatalf("looking up the position of device %s: %v", deviceID, err)
	}
	return host, devpath
}

// installShellScripts gives the fake hardware something to say.
//
// Order matters and is the opposite of what it looks like: fakeadb consults
// scripts newest-first and matches by service PREFIX, so the broadest answer
// is registered first and the narrowest last. Registering the battery dump
// before the per-device echo would let "dumpsys battery" match the echo.
func installShellScripts(srv *fakeadb.Server, devs []simDevice) {
	// A device that answers every command with a clean exit. Without this the
	// fake returns its own diagnostic echo, which is not a shell_v2 stream and
	// which the runner's very first round trip — the mkdir that creates the
	// work directory — would fail to parse.
	srv.Respond("", "shell", shellV2("\n", 0))

	for _, d := range devs {
		// Every shell command answers with the POSITION that ran it, so a
		// step's stored output is evidence about which physical device the
		// work reached. On a rack holding two handsets with the same OEM
		// serial — the seeder plants exactly that — nothing else is evidence.
		devpath := d.devpath
		srv.RespondWith(devpath, "shell", func(service string) string {
			return shellV2(fmt.Sprintf("%s ran %s\n", devpath, shellCommand(service)), 0)
		})
		srv.Respond(devpath, adbwire.ShellService(watchdog.BatteryCommand),
			shellV2(batteryDump(devpath), 0))
	}
}

// shellCommand strips the ADB service prefix off a shell request, leaving the
// command the job actually asked for.
func shellCommand(service string) string {
	const prefix = "shell,v2,raw:"
	if len(service) > len(prefix) && service[:len(prefix)] == prefix {
		return service[len(prefix):]
	}
	return service
}

// shellV2 frames a shell_v2 reply: some stdout, then an exit status. The
// packets are written with the SHIPPING encoder, so adbwire's demultiplexer is
// exercised rather than bypassed.
func shellV2(stdout string, exit byte) string {
	var b bytes.Buffer
	// bytes.Buffer never fails a write and these payloads are far below the
	// maximum packet size, so neither error can occur.
	_ = adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout))
	_ = adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{exit})
	return b.String()
}

// batteryDump is one handset's answer to the watchdog's battery probe, shaped
// like the real thing — header and all — so the shipping parser is exercised
// rather than a convenient subset of it. The numbers are a pure function of
// the position, so a re-run reads the same fleet.
func batteryDump(devpath string) string {
	h := uint64(0)
	for _, c := range devpath {
		h = h*31 + uint64(c)
	}
	return fmt.Sprintf(`Current Battery Service state:
  AC powered: false
  USB powered: true
  status: 2
  health: 2
  present: true
  level: %d
  scale: 100
  voltage: %d
  temperature: %d
  technology: Li-ion
`, 35+int(h%62), 3700+int(h%500), 240+int(h%140))
}

// wireState narrows a seeded farm.device_runtime.adb_state onto the fake's
// wire vocabulary, so the simulated hardware agrees with the database it was
// seeded from. A farm whose fake hardware said every handset was healthy would
// quietly heal the faulty hub the seeder plants.
func wireState(adbState string) fakeadb.State {
	switch adbState {
	case "", "device":
		return fakeadb.StateDevice
	case "absent", "detached", "unknown", "host":
		// Nothing the server would list. StateAbsent keeps the row in the
		// fake's table while hiding it from every listing: a handset that fell
		// off the bus while the control plane still has a row for it.
		return fakeadb.StateAbsent
	default:
		return fakeadb.State(adbState)
	}
}
