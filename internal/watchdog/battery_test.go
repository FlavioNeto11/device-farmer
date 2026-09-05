package watchdog

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// ---------------------------------------------------------------------------
// Parsing what a handset says
// ---------------------------------------------------------------------------

// realDump is the shape adbd actually prints, blank first line and all.
const realDump = `Current Battery Service state:
  AC powered: false
  USB powered: true
  Wireless powered: false
  Max charging current: 500000
  Max charging voltage: 5000000
  Charge counter: 2814000
  status: 2
  health: 2
  present: true
  level: 87
  scale: 100
  voltage: 4123
  temperature: 293
  technology: Li-ion
`

func TestParseBatteryDump(t *testing.T) {
	t.Parallel()

	pct := func(n int32) *int32 { return &n }

	cases := []struct {
		name     string
		in       string
		wantPct  *int32
		wantTemp *int32
	}{
		{"a real dump", realDump, pct(87), pct(293)},

		// The demo's fake handsets answer in this shape; a divergence between
		// the two would make the demo prove nothing about the parser.
		{"the demo's shape", "Current Battery Service state:\n  level: 41\n  scale: 100\n" +
			"  voltage: 3912\n  temperature: 305\n  technology: Li-ion\n", pct(41), pct(305)},

		// The whole reason scale is read at all. A device that scales to 255
		// and reports 100 is at 39%, not full.
		{"a scale that is not 100", "level: 100\nscale: 255\n", pct(39), nil},
		{"rounding is to nearest", "level: 128\nscale: 255\n", pct(50), nil},

		// Only one of the two facts arriving is normal and must not be turned
		// into a zero for the other.
		{"temperature only", "temperature: 312\n", nil, pct(312)},
		{"level only", "level: 5\nscale: 100\n", pct(5), nil},

		// No scale line at all: the framework default is 100 and assuming it
		// is better than discarding a level everybody else can read.
		{"no scale line", "level: 64\n", pct(64), nil},

		// Nonsense is dropped, never clamped: a clamped value is an invented
		// observation, and a value outside the column's CHECK would take the
		// whole cycle's write down with it.
		{"level above scale", "level: 140\nscale: 100\ntemperature: 300\n", nil, pct(300)},
		{"negative level", "level: -1\nscale: 100\n", nil, nil},
		{"a sensor that is not there", "level: 50\nscale: 100\ntemperature: -32000\n", pct(50), nil},
		{"millidegrees, the likely wrong unit", "level: 50\nscale: 100\ntemperature: 29300\n", pct(50), nil},

		// Zero is a real reading at both ends and must survive.
		{"a flat battery", "level: 0\nscale: 100\ntemperature: 0\n", pct(0), pct(0)},

		// A device that answered something else entirely.
		{"empty", "", nil, nil},
		{"not a dump at all", "/system/bin/sh: dumpsys: not found\n", nil, nil},
		{"no colons", "level 87\ntemperature 293\n", nil, nil},

		// Unknown keys are ignored rather than fatal: the set of things
		// dumpsys prints grows with every Android release.
		{"unfamiliar keys", "battery_cycle_count: 41\nlevel: 12\nscale: 100\n", pct(12), nil},

		// Some transports deliver CRLF.
		{"crlf", "  level: 77\r\n  scale: 100\r\n  temperature: 288\r\n", pct(77), pct(288)},

		// A scale of zero would be a division by zero; the default is used.
		{"zero scale", "level: 9\nscale: 0\n", pct(9), nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gotPct, gotTemp := parseBatteryDump([]byte(c.in))
			if !eqPtr(gotPct, c.wantPct) {
				t.Errorf("pct = %s, want %s", showPtr(gotPct), showPtr(c.wantPct))
			}
			if !eqPtr(gotTemp, c.wantTemp) {
				t.Errorf("temp_dc = %s, want %s", showPtr(gotTemp), showPtr(c.wantTemp))
			}
		})
	}
}

// TestParsedTemperatureAlwaysFitsTheColumnCheck pins the agreement between
// this reader and migrations/00010_battery_temp_check.sql. The batch UPDATE
// carries a whole cycle in one statement, so one row that violates the CHECK
// would discard every other device's reading in that cycle.
func TestParsedTemperatureAlwaysFitsTheColumnCheck(t *testing.T) {
	t.Parallel()

	for _, v := range []int{-2000000, -401, -400, -1, 0, 1, 1500, 1501, 32000} {
		dump := "level: 50\nscale: 100\ntemperature: " + strconv.Itoa(v) + "\n"
		_, temp := parseBatteryDump([]byte(dump))
		if temp == nil {
			continue
		}
		if int(*temp) < minBatteryTempDC || int(*temp) > maxBatteryTempDC {
			t.Errorf("temperature %d was accepted as %d, outside the CHECK bounds %d..%d",
				v, *temp, minBatteryTempDC, maxBatteryTempDC)
		}
	}
}

// ---------------------------------------------------------------------------
// Reading a handset over the real wire
// ---------------------------------------------------------------------------

func TestReadOneOverTheWire(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERBATT", Devpath: devpath}))
	srv.Respond(devpath, adbwire.ShellService(BatteryCommand), shellV2(t, realDump, 0))

	p := testPoller(t)
	got, reason := p.readOne(t.Context(), p.dial(srv.Addr()),
		batteryTarget{DeviceID: "dev-1", Devpath: devpath, Host: "h1", Endpoint: srv.Addr()})

	if reason != "" {
		t.Fatalf("readOne refused the reading: %q", reason)
	}
	if got.Pct == nil || *got.Pct != 87 || got.TempDC == nil || *got.TempDC != 293 {
		t.Fatalf("pct=%s temp=%s, want 87/293", showPtr(got.Pct), showPtr(got.TempDC))
	}
	if got.DeviceID != "dev-1" {
		t.Fatalf("device id = %q", got.DeviceID)
	}

	// One command, three facts. Asking for level, scale and temperature
	// separately would triple the transports opened against handsets that are
	// mostly mid-job.
	shells := 0
	for _, r := range srv.RequestsTo(devpath) {
		if strings.HasPrefix(r.Service, "shell") {
			shells++
		}
	}
	if shells != 1 {
		t.Fatalf("the device saw %d shell requests, want exactly 1: %+v", shells, srv.RequestsTo(devpath))
	}
}

// TestReadingIsAddressedByPositionNotSerial is the clone trap. Two handsets
// wearing one OEM serial sit in two positions; a serial-addressed probe would
// read one battery and write it onto the other one's row.
func TestReadingIsAddressedByPositionNotSerial(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	srv.Respond(fakeadb.CloneDevpathA, adbwire.ShellService(BatteryCommand),
		shellV2(t, "level: 11\nscale: 100\ntemperature: 111\n", 0))
	srv.Respond(fakeadb.CloneDevpathB, adbwire.ShellService(BatteryCommand),
		shellV2(t, "level: 92\nscale: 100\ntemperature: 222\n", 0))

	p := testPoller(t)
	readings := p.poll(t.Context(), map[string][]batteryTarget{"h1": {
		{DeviceID: "clone-a", Devpath: fakeadb.CloneDevpathA, Host: "h1", Endpoint: srv.Addr()},
		{DeviceID: "clone-b", Devpath: fakeadb.CloneDevpathB, Host: "h1", Endpoint: srv.Addr()},
	}})

	byID := map[string]batteryReading{}
	for _, r := range readings {
		byID[r.DeviceID] = r
	}
	if len(byID) != 2 {
		t.Fatalf("got %d readings, want 2: %+v", len(byID), readings)
	}
	if a := byID["clone-a"]; a.Pct == nil || *a.Pct != 11 || a.TempDC == nil || *a.TempDC != 111 {
		t.Errorf("clone A read as %s/%s, want 11/111", showPtr(a.Pct), showPtr(a.TempDC))
	}
	if b := byID["clone-b"]; b.Pct == nil || *b.Pct != 92 || b.TempDC == nil || *b.TempDC != 222 {
		t.Errorf("clone B read as %s/%s, want 92/222", showPtr(b.Pct), showPtr(b.TempDC))
	}
}

// TestADeviceThatCannotAnswerProducesNothing is the central claim of this
// reader: absence of a reading is not a reading of zero. Every failure below
// must yield no batteryReading at all, so the row is never touched and the
// column keeps meaning "never observed".
func TestADeviceThatCannotAnswerProducesNothing(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-4.2"

	cases := []struct {
		name       string
		arrange    func(t *testing.T, srv *fakeadb.Server, p *batteryPoller)
		target     string
		wantReason string
	}{
		{
			name: "the server refuses the transport",
			arrange: func(_ *testing.T, srv *fakeadb.Server, _ *batteryPoller) {
				srv.FailNext("shell", "device offline")
			},
			wantReason: "failed",
		},
		{
			name: "the cable dies mid-answer",
			arrange: func(_ *testing.T, srv *fakeadb.Server, _ *batteryPoller) {
				srv.ResetNext("shell", 0)
			},
			wantReason: "failed",
		},
		{
			name: "the handset accepts and never answers",
			arrange: func(_ *testing.T, srv *fakeadb.Server, p *batteryPoller) {
				srv.HangNext("shell")
				p.probeTimeout = 150 * time.Millisecond
			},
			wantReason: "timeout",
		},
		{
			name:       "the transport is gone",
			target:     "usb:9-9.9",
			wantReason: "detached",
		},
		{
			name: "dumpsys itself failed",
			arrange: func(t *testing.T, srv *fakeadb.Server, _ *batteryPoller) {
				srv.Respond(devpath, adbwire.ShellService(BatteryCommand),
					shellV2(t, "level: 87\nscale: 100\ntemperature: 293\n", 1))
			},
			wantReason: "nonzero_exit",
		},
		{
			name: "the answer was cut off",
			arrange: func(t *testing.T, srv *fakeadb.Server, p *batteryPoller) {
				// A truncated tail can land inside a number: "temperature: 29"
				// would read as 2.9 C. Half an answer is not an answer.
				srv.Respond(devpath, adbwire.ShellService(BatteryCommand),
					shellV2(t, realDump+strings.Repeat("x", 4096), 0))
				p.dial = clientDialer(p, 64)
			},
			wantReason: "truncated",
		},
		{
			name: "the framework is not up yet",
			arrange: func(t *testing.T, srv *fakeadb.Server, _ *batteryPoller) {
				srv.Respond(devpath, adbwire.ShellService(BatteryCommand), shellV2(t, "", 0))
			},
			wantReason: "no_value",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			srv := fakeadb.Start(t, fakeadb.WithDevices(
				fakeadb.Device{Serial: "SERFAIL", Devpath: devpath}))
			srv.Respond(devpath, adbwire.ShellService(BatteryCommand), shellV2(t, realDump, 0))

			p := testPoller(t)
			if c.arrange != nil {
				c.arrange(t, srv, p)
			}
			target := batteryTarget{DeviceID: "dev-x", Devpath: devpath, Host: "h1", Endpoint: srv.Addr()}
			if c.target != "" {
				target.Devpath = c.target
			}

			got, reason := p.readOne(t.Context(), p.dial(srv.Addr()), target)
			if reason != c.wantReason {
				t.Fatalf("reason = %q, want %q (reading %+v)", reason, c.wantReason, got)
			}
			if got.Pct != nil || got.TempDC != nil || got.DeviceID != "" {
				t.Fatalf("a device that could not answer produced %+v; nothing may be written for it", got)
			}
		})
	}
}

// TestAShutdownIsNotEvidenceAgainstAHandset separates "this device did not
// answer" from "we stopped asking". Counting a redeploy as a device problem
// would put an operator's own deploy into the record as evidence against a
// phone.
func TestAShutdownIsNotEvidenceAgainstAHandset(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-4.3"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERGONE", Devpath: devpath}))
	srv.HangNext("shell")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	p := testPoller(t)
	p.probeTimeout = 5 * time.Second

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, reason := p.readOne(ctx, p.dial(srv.Addr()),
		batteryTarget{DeviceID: "dev-1", Devpath: devpath, Host: "h1", Endpoint: srv.Addr()})
	if reason != "cancelled" {
		t.Fatalf("reason = %q, want %q", reason, "cancelled")
	}
}

// TestPollKeepsGoingAroundASilentHandset: one wedged device in a rack must not
// cost the rack its readings.
func TestPollKeepsGoingAroundASilentHandset(t *testing.T) {
	t.Parallel()

	const good, bad = "usb:2-1.1", "usb:2-1.2"
	srv := fakeadb.Start(t, fakeadb.WithDevices(
		fakeadb.Device{Serial: "SERGOOD", Devpath: good},
		fakeadb.Device{Serial: "SERBAD", Devpath: bad},
	))
	srv.Respond(good, adbwire.ShellService(BatteryCommand), shellV2(t, realDump, 0))
	srv.Inject(fakeadb.Fault{Match: "shell", Devpath: bad, Kind: fakeadb.FaultHang})

	p := testPoller(t)
	p.probeTimeout = 200 * time.Millisecond

	readings := p.poll(t.Context(), map[string][]batteryTarget{"h1": {
		{DeviceID: "dev-good", Devpath: good, Host: "h1", Endpoint: srv.Addr()},
		{DeviceID: "dev-bad", Devpath: bad, Host: "h1", Endpoint: srv.Addr()},
	}})

	if len(readings) != 1 || readings[0].DeviceID != "dev-good" {
		t.Fatalf("got %+v, want exactly the healthy device", readings)
	}
	if readings[0].Pct == nil || *readings[0].Pct != 87 {
		t.Fatalf("healthy device read as %s, want 87", showPtr(readings[0].Pct))
	}
}

// TestAVanishedHostIsZeroedNotFrozen: the target gauge is per host, because one
// process can supervise many, and a host whose devices have all gone must
// produce a series AT zero rather than one frozen at the last count anybody
// saw — which would read as a full rack that is actually empty.
func TestAVanishedHostIsZeroedNotFrozen(t *testing.T) {
	t.Parallel()

	// Label values unique to this test, so the package-level vec shared with
	// every other test does not make this one order-dependent.
	const a, b = "gauge-host-a", "gauge-host-b"

	p := testPoller(t)
	p.publishTargets(map[string][]batteryTarget{
		a: {{DeviceID: "d1", Host: a}, {DeviceID: "d2", Host: a}},
		b: {{DeviceID: "d3", Host: b}},
	})
	if _, ok := p.lastHosts[a]; !ok {
		t.Fatalf("after publishing, lastHosts = %v, want it to carry %s", p.lastHosts, a)
	}

	// a's rack is gone; b is unchanged, so only a is zeroed.
	now := map[string][]batteryTarget{b: {{DeviceID: "d3", Host: b}}}
	gone := vanishedHosts(p.lastHosts, now)
	if len(gone) != 1 || gone[0] != a {
		t.Fatalf("vanished = %v, want exactly [%s]", gone, a)
	}

	p.publishTargets(now)
	if _, ok := p.lastHosts[a]; ok {
		t.Fatalf("lastHosts still carries %s after its devices went away: %v", a, p.lastHosts)
	}
	if gone := vanishedHosts(p.lastHosts, now); len(gone) != 0 {
		t.Fatalf("a host that is still there was reported vanished: %v", gone)
	}
}

// ---------------------------------------------------------------------------
// Invariants of the design itself
// ---------------------------------------------------------------------------

// TestOneCycleCannotOutlastItsInterval guards the arithmetic in the comments
// on batteryProbeTimeout: a rack whose every handset accepts a transport and
// then goes silent must still finish a cycle before the next tick, or cycles
// pile up on each other.
func TestOneCycleCannotOutlastItsInterval(t *testing.T) {
	t.Parallel()

	// A generously large host: 4 hubs of 16.
	const devices = 64
	waves := (devices + batteryConcurrency - 1) / batteryConcurrency
	worst := time.Duration(waves) * batteryProbeTimeout
	if worst >= DefaultBatteryInterval {
		t.Fatalf("a host of %d silent devices needs %s, which does not fit in the %s interval",
			devices, worst, DefaultBatteryInterval)
	}
}

// TestBatteryReadsSlowerThanTheReconciler states the cadence choice as an
// assertion. A battery drifts continuously and slowly; polling it at the
// reconciler's rate would open sixty transports every five seconds to re-read
// a number that had not moved.
func TestBatteryReadsSlowerThanTheReconciler(t *testing.T) {
	t.Parallel()

	if DefaultBatteryInterval < DefaultResync {
		t.Fatalf("battery interval %s is faster than the resync %s; a level-triggered fact that "+
			"moves in minutes has no business being polled faster than the state machine",
			DefaultBatteryInterval, DefaultResync)
	}
	if batteryProbeTimeout >= DefaultBatteryInterval {
		t.Fatalf("one probe may take %s out of a %s cycle", batteryProbeTimeout, DefaultBatteryInterval)
	}
}

// TestBatterySQLNeverNamesALease is the package's one rule, checked in the one
// file that was added to it. The watchdog role has ALL privileges on
// farm.leases revoked, so a query naming it would fail in production rather
// than here; this is the cheaper place to find out.
//
// Only string literals are examined. The prose in this package discusses
// farm.leases constantly and must be free to keep doing so — it is the
// explanation of why the table is absent.
func TestBatterySQLNeverNamesALease(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "battery.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing battery.go: %v", err)
	}

	// Vocabulary that only appears in code that can reach allocation. "lease"
	// covers farm.leases, lease_acquire and lease_release alike.
	banned := []string{"lease", "fence", "holder", "quarantin"}

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		lower := strings.ToLower(lit.Value)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Errorf("%s: a string in battery.go contains %q: %s",
					fset.Position(lit.Pos()), word, lit.Value)
			}
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testPoller builds a poller with no database. Everything exercised here is
// the device-side half; the two statements that touch Postgres are covered
// end to end against a real server.
func testPoller(t *testing.T) *batteryPoller {
	t.Helper()
	p := &batteryPoller{
		interval:     DefaultBatteryInterval,
		callTimeout:  2 * time.Second,
		probeTimeout: 2 * time.Second,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	p.dial = clientDialer(p, batteryMaxOutput)
	return p
}

// clientDialer builds the same client the shipping poller builds, with the
// output cap made an argument so truncation is reachable from a test.
func clientDialer(p *batteryPoller, maxOutput int) func(string) batteryShell {
	return func(endpoint string) batteryShell {
		return adbwire.New(endpoint,
			adbwire.WithLogger(p.log),
			adbwire.WithCallTimeout(p.probeTimeout),
			adbwire.WithMaxOutput(maxOutput))
	}
}

// shellV2 frames a scripted reply with the shipping encoder, so the fake
// exercises adbwire's demultiplexer rather than bypassing it.
func shellV2(t *testing.T, stdout string, exit byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout)); err != nil {
		t.Fatalf("framing stdout: %v", err)
	}
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{exit}); err != nil {
		t.Fatalf("framing exit: %v", err)
	}
	return b.String()
}

func eqPtr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func showPtr(p *int32) string {
	if p == nil {
		return "<none>"
	}
	return strconv.Itoa(int(*p))
}
