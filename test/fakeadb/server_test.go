package fakeadb

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake is a test harness, so its own tests speak the protocol by hand
// rather than through internal/adbwire. If both sides shared a framing
// implementation, a matching pair of bugs would look like a passing test.

// wireDeadline bounds every hand-rolled exchange. It is a failure guard: a
// protocol mistake fails the test instead of parking it until the package
// timeout.
const wireDeadline = 20 * time.Second

type wire struct {
	tb testing.TB
	c  net.Conn
	br *bufio.Reader
}

func dialFor(tb testing.TB, s *Server, timeout time.Duration) *wire {
	tb.Helper()
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		tb.Fatalf("dial %s: %v", s.Addr(), err)
	}
	tb.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		tb.Fatalf("set deadline: %v", err)
	}
	return &wire{tb: tb, c: c, br: bufio.NewReader(c)}
}

func dial(tb testing.TB, s *Server) *wire { return dialFor(tb, s, wireDeadline) }

func (w *wire) send(service string) {
	w.tb.Helper()
	if _, err := fmt.Fprintf(w.c, "%04x%s", len(service), service); err != nil {
		w.tb.Fatalf("write %q: %v", service, err)
	}
}

func (w *wire) status() string {
	w.tb.Helper()
	var st [4]byte
	if _, err := io.ReadFull(w.br, st[:]); err != nil {
		w.tb.Fatalf("read status: %v", err)
	}
	return string(st[:])
}

// message reads one length-prefixed payload.
func (w *wire) message() string {
	w.tb.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
		w.tb.Fatalf("read length prefix: %v", err)
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	if err != nil {
		w.tb.Fatalf("length prefix %q is not four hex digits: %v", hdr, err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.br, buf); err != nil {
		w.tb.Fatalf("read %d-byte payload: %v", n, err)
	}
	return string(buf)
}

// ok sends service, requires OKAY and returns the framed payload.
func (w *wire) ok(service string) string {
	w.tb.Helper()
	w.send(service)
	switch st := w.status(); st {
	case "OKAY":
		return w.message()
	case "FAIL":
		w.tb.Fatalf("service %q: FAIL %q, want OKAY", service, w.message())
	default:
		w.tb.Fatalf("service %q: status %q, want OKAY", service, st)
	}
	return ""
}

// okBare requires a bare OKAY with no payload, which is what a transport
// switch and a wait-for answer with.
func (w *wire) okBare(service string) {
	w.tb.Helper()
	w.send(service)
	if st := w.status(); st != "OKAY" {
		w.tb.Fatalf("service %q: status %q, want a bare OKAY", service, st)
	}
}

// fail sends service, requires FAIL and returns the server's reason.
func (w *wire) fail(service string) string {
	w.tb.Helper()
	w.send(service)
	if st := w.status(); st != "FAIL" {
		w.tb.Fatalf("service %q: status %q, want FAIL", service, st)
	}
	return w.message()
}

// hostOK and hostFail each run one host service on its own connection,
// because the real adb server closes the socket after a completed host
// request and this fake keeps that shape.
func hostOK(tb testing.TB, s *Server, service string) string {
	tb.Helper()
	return dial(tb, s).ok(service)
}

func hostFail(tb testing.TB, s *Server, service string) string {
	tb.Helper()
	return dial(tb, s).fail(service)
}

// ---------------------------------------------------------------------
// Host services
// ---------------------------------------------------------------------

func TestVersionIsFourHexDigits(t *testing.T) {
	t.Parallel()

	s := Start(t)
	// A decimal-formatted fake would be read base 16 by every real client, so
	// version 41 would silently become 65.
	if got, want := hostOK(t, s, "host:version"), "0029"; got != want {
		t.Fatalf("host:version = %q, want %q", got, want)
	}
	s.SetHostVersion(0x100)
	if got, want := hostOK(t, s, "host:version"), "0100"; got != want {
		t.Fatalf("host:version after SetHostVersion = %q, want %q", got, want)
	}
}

func TestHostFeaturesAreTheServersNotADevices(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.1"
	s := Start(t, WithDevices(Device{Serial: "SER1", Devpath: devpath}))

	if got, want := hostOK(t, s, "host:host-features"), strings.Join(DefaultFeatures(), ","); got != want {
		t.Fatalf("host:host-features = %q, want the server set", got)
	}
	// The device's own set is a different question and needs an address.
	s.Update(devpath, func(d *Device) { d.Features = []string{"shell_v2", "abb"} })
	if got, want := hostOK(t, s, "host-usb:"+devpath+":features"), "shell_v2,abb"; got != want {
		t.Fatalf("device features = %q, want %q", got, want)
	}
	s.SetFeatures("cmd", "stat_v2")
	s.Update(devpath, func(d *Device) { d.Features = nil })
	if got, want := hostOK(t, s, "host-usb:"+devpath+":features"), "cmd,stat_v2"; got != want {
		t.Fatalf("device features fell back to %q, want the server default %q", got, want)
	}
	// adb has not read a non-device transport's properties, so it cannot
	// answer for it.
	s.SetState(devpath, StateOffline)
	if got, want := hostFail(t, s, "host-usb:"+devpath+":features"), MsgDeviceOffline; got != want {
		t.Fatalf("features on an offline device = %q, want %q", got, want)
	}
}

func TestKillIsAnsweredButNotObeyed(t *testing.T) {
	t.Parallel()

	s := Start(t)
	dial(t, s).okBare("host:kill")
	// A test that killed the fake by accident would take its own listener
	// down mid-run, so the request is recorded and ignored.
	if got := hostOK(t, s, "host:version"); got == "" {
		t.Fatal("the server stopped answering after host:kill")
	}
	reqs := s.Requests()
	if len(reqs) == 0 || reqs[0].Service != "host:kill" {
		t.Fatalf("host:kill was not recorded: %+v", reqs)
	}
}

func TestUnknownServicesFail(t *testing.T) {
	t.Parallel()

	s := Start(t, WithDevices(Device{Serial: "SER1", Devpath: "usb:3-1.1"}))
	for _, svc := range []string{"host:frobnicate", "garbage", "host-usb:usb:3-1.1:frobnicate"} {
		if got := hostFail(t, s, svc); !strings.Contains(got, "unknown host service") {
			t.Fatalf("%q = %q, want an unknown-service refusal", svc, got)
		}
	}
}

// ---------------------------------------------------------------------
// Listings
// ---------------------------------------------------------------------

func TestLongListingShape(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Add(Device{Serial: "SER1", Devpath: "usb:3-1.1", Product: "bluejay", Model: "Pixel 6a", Codename: "bluejay"})
	s.Add(Device{Serial: "SER2", Devpath: "usb:3-1.2", Product: "bluejay", Model: "Pixel 6a", State: StateOffline})
	s.Add(Device{Devpath: "usb:3-1.3", State: StateUnauthorized})

	lines := strings.Split(strings.TrimSuffix(hostOK(t, s, "host:devices-l"), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("listing has %d lines, want 3:\n%s", len(lines), strings.Join(lines, "\n"))
	}

	// The exact rendering, so a change to padding or to the model sanitiser
	// shows up here rather than as a mis-parse three layers away. adb pads the
	// serial column to 22 and replaces whitespace in the model, so that
	// "Pixel 6a" cannot be mistaken for two fields.
	want := "SER1" + strings.Repeat(" ", 19) +
		"device usb:3-1.1 product:bluejay model:Pixel_6a device:bluejay transport_id:1"
	if lines[0] != want {
		t.Fatalf("line 0:\n got %q\nwant %q", lines[0], want)
	}

	// Identity fields are omitted for a transport adb has not read properties
	// from. A parser that assumes they are present is broken, and this fake
	// exists to catch that.
	if strings.Contains(lines[1], "model:") || strings.Contains(lines[1], "product:") {
		t.Fatalf("an offline device carries identity fields: %q", lines[1])
	}
	if !strings.Contains(lines[1], " usb:3-1.2 ") {
		t.Fatalf("an offline device lost its position: %q", lines[1])
	}

	// The serial column of a transport whose serial adb has not read is a
	// three-word sentinel, not an empty column: an empty one would leave the
	// state in field one and the handset would not appear as unhealthy, it
	// would not appear at all.
	if !strings.HasPrefix(lines[2], noSerial) {
		t.Fatalf("line 2 = %q, want the %q sentinel", lines[2], noSerial)
	}
	if !strings.Contains(lines[2], "unauthorized") {
		t.Fatalf("line 2 lost the state: %q", lines[2])
	}

	for i, line := range lines {
		fields := strings.Fields(line)
		if last := fields[len(fields)-1]; !strings.HasPrefix(last, "transport_id:") {
			// transport_id last is what lets a parser scanning backwards from
			// the newline always find it.
			t.Errorf("line %d ends with %q, want transport_id", i, last)
		}
	}
}

func TestModelSanitisation(t *testing.T) {
	t.Parallel()

	s := Start(t, WithDevices(Device{Serial: "SER1", Devpath: "usb:3-1.1", Model: "Pixel 6a (BETA)"}))
	line := strings.TrimSuffix(hostOK(t, s, "host:devices-l"), "\n")
	if !strings.Contains(line, "model:Pixel_6a__BETA_") {
		t.Fatalf("model was not sanitised: %q", line)
	}
	if n := len(strings.Fields(line)); n != 5 {
		t.Fatalf("line split into %d fields, want 5; whitespace leaked out of the model: %q", n, line)
	}
}

func TestShortListingCarriesNoPosition(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Add(Device{Serial: "SER1", Devpath: "usb:3-1.1"})
	s.Add(Device{Devpath: "usb:3-1.2", State: StateUnauthorized})

	got := hostOK(t, s, "host:devices")
	want := "SER1\tdevice\n" + noSerial + "\tunauthorized\n"
	if got != want {
		t.Fatalf("host:devices = %q, want %q", got, want)
	}
	// Which is exactly why the control plane asks for the long form: this
	// listing cannot be addressed safely on a farm where serials collide.
	if strings.Contains(got, "usb:") {
		t.Fatalf("the short listing leaked a devpath: %q", got)
	}
}

func TestAbsentDeviceVanishesFromTheWireButKeepsItsRow(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.1"
	s := Start(t, WithDevices(Device{Serial: "SERGONE", Devpath: devpath}))
	if !s.SetState(devpath, StateAbsent) {
		t.Fatal("SetState reported no such device")
	}

	if got := hostOK(t, s, "host:devices-l"); got != "" {
		t.Fatalf("an absent device is still listed: %q", got)
	}
	if got, want := hostFail(t, s, "host-usb:"+devpath+":get-state"), MsgNotFound(devpath); got != want {
		t.Fatalf("get-state on an absent device = %q, want %q", got, want)
	}
	// The row survives: this models a handset that fell off the bus while the
	// control plane still has a row — and a live lease — for it.
	d, ok := s.Device(devpath)
	if !ok || d.State != StateAbsent {
		t.Fatalf("Device(%q) = %+v, %t; the scripted row must survive", devpath, d, ok)
	}
	if n := len(s.Devices()); n != 1 {
		t.Fatalf("the table holds %d devices, want 1", n)
	}
}

func TestClearAndRemove(t *testing.T) {
	t.Parallel()

	s := Start(t, HubFixture(2, 1, 3))
	if got := len(strings.Fields(hostOK(t, s, "host:devices"))); got != 6 {
		t.Fatalf("hub listing has %d tokens, want 6 (three serial/state pairs)", got)
	}
	if !s.Remove("usb:2-1.2") {
		t.Fatal("Remove reported no such device")
	}
	if s.Remove("usb:2-1.2") {
		t.Fatal("Remove reported success twice for one device")
	}
	if n := len(s.Devices()); n != 2 {
		t.Fatalf("%d devices after Remove, want 2", n)
	}
	s.Clear()
	if got := hostOK(t, s, "host:devices-l"); got != "" {
		t.Fatalf("listing after Clear = %q", got)
	}
	if got, want := hostFail(t, s, "host:get-state"), MsgNoDevices; got != want {
		t.Fatalf("get-state with nothing attached = %q, want %q", got, want)
	}
}

func TestAddRejectsAmbiguousPositions(t *testing.T) {
	t.Parallel()

	s := Start(t)
	assertPanics(t, "a device with no position", func() { s.Add(Device{Serial: "SER1"}) })
	s.Add(Device{Serial: "SER1", Devpath: "usb:3-1.1"})
	// Two rows claiming one physical position is a bug in the test, not a
	// scenario: two devices sharing a serial is the scenario, and that one is
	// allowed.
	assertPanics(t, "two devices in one position", func() { s.Add(Device{Serial: "SER2", Devpath: "usb:3-1.1"}) })
	s.Add(Device{Serial: "SER1", Devpath: "usb:3-1.2"})

	if n := len(s.Devices()); n != 2 {
		t.Fatalf("%d devices survived, want 2", n)
	}
	// The panic must not have left the table lock held.
	if got := hostFail(t, s, "host-serial:SER1:get-state"); got != MsgAmbiguousTarget {
		t.Fatalf("the server stopped working after a panic: %q", got)
	}
}

func assertPanics(tb testing.TB, what string, fn func()) {
	tb.Helper()
	defer func() {
		if recover() == nil {
			tb.Errorf("%s did not panic", what)
		}
	}()
	fn()
}

// eventually polls until want reports true, failing the test if wireDeadline
// elapses first.
//
// It is for the handful of facts a connection teardown settles asynchronously —
// a goroutine noticing its socket is gone — and for nothing else. Everything
// this file asserts about a reply is settled by the time the reply is in hand
// and is asserted directly.
func eventually(tb testing.TB, what string, want func() bool) {
	tb.Helper()
	deadline := time.Now().Add(wireDeadline)
	for !want() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// Addressing
// ---------------------------------------------------------------------

// mustDevice returns a scripted device or fails the test.
//
// The bare two-value form drops the ok, and a fixture that stopped installing
// the devpath would hand back the zero Device — turning a clear "the fixture
// changed" into a mismatched echo or a transport-id of 0 asserted three lines
// later.
func mustDevice(tb testing.TB, s *Server, devpath string) Device {
	tb.Helper()
	d, ok := s.Device(devpath)
	if !ok {
		tb.Fatalf("the fixture did not install %s", devpath)
	}
	return d
}

// TestCloneAddressing is the duplicate-serial trap the fixture exists for.
func TestCloneAddressing(t *testing.T) {
	t.Parallel()

	s := Start(t, TwoClonesFixture())

	// Position addressing resolves to exactly one transport.
	if got := hostOK(t, s, "host-usb:"+CloneDevpathB+":get-serialno"); got != CloneSerial {
		t.Fatalf("get-serialno at %s = %q", CloneDevpathB, got)
	}
	if n := len(s.RequestsTo(CloneDevpathA)); n != 0 {
		t.Fatalf("%d requests reached the other clone", n)
	}
	if n := len(s.RequestsTo(CloneDevpathB)); n != 1 {
		t.Fatalf("%d requests reached %s, want 1", n, CloneDevpathB)
	}

	// Serial addressing does not, and the server says so rather than picking.
	// A recovery action resolved this way lands on whichever transport adb
	// hands back, which may be a device hours into somebody else's run.
	if got := hostFail(t, s, "host-serial:"+CloneSerial+":get-state"); got != MsgAmbiguousTarget {
		t.Fatalf("serial-addressed get-state = %q, want %q", got, MsgAmbiguousTarget)
	}
	if got := hostFail(t, s, "host:transport:"+CloneSerial); got != MsgAmbiguousTarget {
		t.Fatalf("serial-addressed transport = %q, want %q", got, MsgAmbiguousTarget)
	}
	// Matching mirrors atransport::MatchesTarget: a target matches by serial
	// OR by devpath, so host-serial with a devpath in the target field is
	// still position addressing.
	if got := hostOK(t, s, "host-serial:"+CloneDevpathA+":get-state"); got != string(StateDevice) {
		t.Fatalf("devpath in the target-name field = %q", got)
	}
	// One transport is one transport however it is addressed.
	dev := mustDevice(t, s, CloneDevpathA)
	if got := hostOK(t, s, fmt.Sprintf("host-transport-id:%d:get-devpath", dev.TransportID)); got != CloneDevpathA {
		t.Fatalf("transport-id addressing reached %q", got)
	}
	if got := hostFail(t, s, "host-usb:usb:9-9.9:get-state"); got != MsgNotFound("usb:9-9.9") {
		t.Fatalf("an empty position = %q", got)
	}
	if got := hostFail(t, s, "host-transport-id:9999:get-state"); !strings.Contains(got, "9999") {
		t.Fatalf("an unknown transport id = %q", got)
	}
}

func TestSplitService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service string
		want    target
		cmd     string
		ok      bool
	}{
		{"no address", "host:version", target{}, "version", true},
		// Splitting on the first colon tears "usb:3-1.4.1" in half and
		// addresses a device that does not exist.
		{"devpath keeps its colon", "host-usb:usb:3-1.4.1:get-state", target{text: "usb:3-1.4.1", usbOnly: true}, "get-state", true},
		{"devpath in the target-name field", "host-serial:usb:3-1.4.1:get-state", target{text: "usb:3-1.4.1"}, "get-state", true},
		{"serial", "host-serial:0123456789ABCDEF:get-state", target{text: "0123456789ABCDEF"}, "get-state", true},
		{"tcp host and port", "host-serial:192.168.1.5:5555:shell:ls", target{text: "192.168.1.5:5555"}, "shell:ls", true},
		{"transport id", "host-transport-id:7:get-state", target{tid: 7}, "get-state", true},
		{"non-numeric transport id", "host-transport-id:x:get-state", target{}, "", false},
		{"transport id with no command", "host-transport-id:7", target{}, "", false},
		{"scoped by transport type", "host-usb:transport-any", target{usbOnly: true}, "transport-any", true},
		{"local", "host-local:emulator-5554:get-state", target{text: "emulator-5554"}, "get-state", true},
		{"not a host service", "shell:ls", target{}, "", false},
		{"empty", "", target{}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotTarget, gotCmd, gotOK := splitService(tc.service)
			if gotOK != tc.ok {
				t.Fatalf("splitService(%q) ok = %t, want %t", tc.service, gotOK, tc.ok)
			}
			if !tc.ok {
				return
			}
			if !reflect.DeepEqual(gotTarget, tc.want) || gotCmd != tc.cmd {
				t.Fatalf("splitService(%q) = %+v, %q; want %+v, %q", tc.service, gotTarget, gotCmd, tc.want, tc.cmd)
			}
		})
	}
}

func TestTargetString(t *testing.T) {
	t.Parallel()

	if got := (target{text: "usb:3-1.1"}).String(); got != "usb:3-1.1" {
		t.Fatalf("target text rendered as %q", got)
	}
	if got := (target{tid: 4}).String(); got != "transport-id:4" {
		t.Fatalf("transport id rendered as %q", got)
	}
}

// ---------------------------------------------------------------------
// Transports and device streams
// ---------------------------------------------------------------------

func TestTransportSwitchThenDeviceStream(t *testing.T) {
	t.Parallel()

	s := Start(t, TwoClonesFixture())
	dev := mustDevice(t, s, CloneDevpathB)

	w := dial(t, s)
	w.okBare("host:transport:" + CloneDevpathB)
	w.okBare("shell:id")
	// After the OKAY the bytes are raw: no framing, no length, just the stream
	// until the server closes.
	rest, err := io.ReadAll(w.br)
	if err != nil {
		t.Fatalf("reading the device stream: %v", err)
	}
	if got, want := string(rest), Echo(dev, "shell:id"); got != want {
		t.Fatalf("device stream = %q, want %q", got, want)
	}

	// The request log records which device the bytes actually reached, which
	// is how a test proves a command landed on one clone and not the other.
	hits := s.RequestsTo(CloneDevpathB)
	if len(hits) != 2 {
		t.Fatalf("%d requests reached %s, want the switch and the service: %+v", len(hits), CloneDevpathB, hits)
	}
	if hits[1].Service != "shell:id" {
		t.Fatalf("the service was recorded as %q", hits[1].Service)
	}
	if n := len(s.RequestsTo(CloneDevpathA)); n != 0 {
		t.Fatalf("%d requests reached the other clone", n)
	}
}

func TestRespondScriptsDeviceOutput(t *testing.T) {
	t.Parallel()

	s := Start(t, TwoClonesFixture())
	devA := mustDevice(t, s, CloneDevpathA)

	s.Respond("", "shell:getprop", "generic\n")
	s.Respond(CloneDevpathB, "shell:getprop", "clone-b\n")

	// The most recently registered match wins, and a devpath-scoped script
	// only fires for that position.
	if got := deviceService(t, s, CloneDevpathB, "shell:getprop ro.serialno"); got != "clone-b\n" {
		t.Fatalf("clone B answered %q", got)
	}
	if got := deviceService(t, s, CloneDevpathA, "shell:getprop ro.serialno"); got != "generic\n" {
		t.Fatalf("clone A answered %q", got)
	}
	// An unscripted service falls back to the echo, which names the device it
	// reached.
	if got, want := deviceService(t, s, CloneDevpathA, "shell:id"), Echo(devA, "shell:id"); got != want {
		t.Fatalf("unscripted service = %q, want %q", got, want)
	}
}

// deviceService runs one device-side service on a fresh connection and returns
// the raw stream.
func deviceService(tb testing.TB, s *Server, devpath, service string) string {
	tb.Helper()
	w := dial(tb, s)
	w.okBare("host:transport:" + devpath)
	w.okBare(service)
	out, err := io.ReadAll(w.br)
	if err != nil {
		tb.Fatalf("reading %q from %s: %v", service, devpath, err)
	}
	return string(out)
}

func TestTransportRefusedForNonDeviceStates(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-1.1"
	s := Start(t, WithDevices(Device{Serial: "SERSICK", Devpath: devpath}))

	for _, tc := range []struct {
		state State
		want  string
	}{
		{StateOffline, MsgDeviceOffline},
		{StateUnauthorized, MsgUnauthorized},
		{StateBootloader, MsgDeviceOffline},
	} {
		s.SetState(devpath, tc.state)
		if got := hostFail(t, s, "host:transport:"+devpath); got != tc.want {
			t.Errorf("transport to a %q device = %q, want %q", tc.state, got, tc.want)
		}
		// "What state is it in" is a health question, and health questions are
		// always answerable — including for the device that cannot be used.
		if got := hostOK(t, s, "host-usb:"+devpath+":get-state"); got != string(tc.state) {
			t.Errorf("get-state on a %q device = %q", tc.state, got)
		}
	}
}

func TestGetSerialnoIsUnknownWhenTheServerHasNotReadOne(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-2.1"
	s := Start(t, WithDevices(Device{Devpath: devpath, State: StateUnauthorized}))
	if got := hostFail(t, s, "host-usb:"+devpath+":get-serialno"); got != "unknown" {
		t.Fatalf("get-serialno without a serial = %q, want %q", got, "unknown")
	}
	if got := hostOK(t, s, "host-usb:"+devpath+":get-devpath"); got != devpath {
		t.Fatalf("get-devpath = %q, want %q", got, devpath)
	}
}

// ---------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------

func TestFaultRuleSelection(t *testing.T) {
	t.Parallel()

	s := Start(t, TwoClonesFixture())

	// Match is a substring of the service string; Times caps the firings.
	s.FailNext("host:version", "boom")
	if got := hostFail(t, s, "host:version"); got != "boom" {
		t.Fatalf("first host:version = %q, want the scripted failure", got)
	}
	if got := hostOK(t, s, "host:version"); got != "0029" {
		t.Fatalf("second host:version = %q, want a normal reply", got)
	}
	if got := s.Stats().Faults; got != 1 {
		t.Fatalf("Stats().Faults = %d, want 1", got)
	}

	// Skip lets matching opportunities pass first.
	s.Inject(Fault{Match: "host:version", Kind: FaultFail, Message: "third", Skip: 2, Times: 1})
	hostOK(t, s, "host:version")
	hostOK(t, s, "host:version")
	if got := hostFail(t, s, "host:version"); got != "third" {
		t.Fatalf("the skipped rule fired at the wrong time: %q", got)
	}

	// Devpath restricts a rule to the device a request resolved to, which is
	// how a fault is aimed at exactly one of two clones.
	s.ClearFaults()
	s.Inject(Fault{Match: "get-state", Devpath: CloneDevpathA, Kind: FaultFail, Message: "only A"})
	if got := hostFail(t, s, "host-usb:"+CloneDevpathA+":get-state"); got != "only A" {
		t.Fatalf("clone A = %q, want the scripted failure", got)
	}
	if got := hostOK(t, s, "host-usb:"+CloneDevpathB+":get-state"); got != string(StateDevice) {
		t.Fatalf("clone B = %q, want an untouched reply", got)
	}

	s.ClearFaults()
	if got := hostOK(t, s, "host-usb:"+CloneDevpathA+":get-state"); got != string(StateDevice) {
		t.Fatalf("clone A after ClearFaults = %q", got)
	}
}

func TestFaultFailUsesADefaultMessage(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Inject(Fault{Match: "host:version", Kind: FaultFail})
	if got := hostFail(t, s, "host:version"); got == "" {
		t.Fatal("a FAIL with no scripted message carried no reason at all")
	}
}

func TestFaultResetLeavesTheClientHoldingHalfAFrame(t *testing.T) {
	t.Parallel()

	s := Start(t, WithDevices(Device{Serial: "SER1", Devpath: "usb:3-1.1"}))

	for _, afterBytes := range []int{0, 2, 6} {
		s.ClearFaults()
		s.ResetNext("host:devices-l", afterBytes)

		w := dial(t, s)
		w.send("host:devices-l")
		// Whether a reset discards bytes already sitting in the peer's receive
		// queue is up to the stack, so the stable claim is not how many bytes
		// arrive but that a complete reply never does: the shortest one is
		// four status bytes plus a four-digit length.
		got, _ := io.ReadAll(w.br)
		if len(got) > afterBytes {
			t.Fatalf("afterBytes=%d: %d bytes arrived", afterBytes, len(got))
		}
		if len(got) >= 8 {
			t.Fatalf("afterBytes=%d: a complete frame arrived: %q", afterBytes, got)
		}
	}

	var reset int
	for _, r := range s.Requests() {
		if r.Fault == FaultReset && r.Reply == "RESET" {
			reset++
		}
	}
	if reset != 3 {
		t.Fatalf("%d requests recorded as reset, want 3: %+v", reset, s.Requests())
	}
	// The listener survives: a severed connection is a reconnect and nothing
	// more.
	if got := hostOK(t, s, "host:version"); got != "0029" {
		t.Fatalf("the server stopped answering after severing: %q", got)
	}
}

func TestFaultHangAnswersNothingAtAll(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.HangNext("host:version")

	// The only thing that ends this call is the caller's own deadline, which
	// is the point: a fake that eventually replied would test nothing.
	w := dialFor(t, s, 250*time.Millisecond)
	w.send("host:version")
	var buf [4]byte
	_, err := io.ReadFull(w.br, buf[:])
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read after a hang = %v, want a timeout on the caller's clock", err)
	}

	var hung bool
	for _, r := range s.Requests() {
		if r.Fault == FaultHang && r.Reply == "HANG" {
			hung = true
		}
	}
	if !hung {
		t.Fatalf("the hang was not recorded: %+v", s.Requests())
	}
	if got := hostOK(t, s, "host:version"); got != "0029" {
		t.Fatalf("a hung connection blocked the next one: %q", got)
	}
}

func TestFaultDelayStillDeliversACorrectReply(t *testing.T) {
	t.Parallel()

	s := Start(t)
	// FaultNone with a Delay is a reply that is slow but perfectly correct —
	// the case a naive timeout mistakes for a dead host.
	s.Inject(Fault{Match: "host:version", Kind: FaultNone, Delay: 20 * time.Millisecond, Times: 1})

	start := time.Now()
	if got := hostOK(t, s, "host:version"); got != "0029" {
		t.Fatalf("delayed reply = %q", got)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("the reply arrived in %v, before the scripted delay", elapsed)
	}
}

// ---------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------

func TestTrackDevicesPushesASnapshotPerMutation(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Add(Device{Serial: "SER1", Devpath: "usb:1-1.1"})

	w := dial(t, s)
	w.send("host:track-devices-l")
	// The stream is OKAY once, then bare length-prefixed payloads forever. A
	// client that waits for a second status hangs against real adb too.
	if st := w.status(); st != "OKAY" {
		t.Fatalf("track-devices status = %q", st)
	}
	first := w.message()
	if n := len(strings.Split(strings.TrimSuffix(first, "\n"), "\n")); n != 1 {
		t.Fatalf("first snapshot has %d lines, want 1:\n%s", n, first)
	}
	// The counter is bumped before the first write, so this is settled by the
	// time the snapshot is in hand.
	if got := s.Stats().Trackers; got != 1 {
		t.Fatalf("Stats().Trackers = %d, want 1", got)
	}

	s.Add(Device{Serial: "SER2", Devpath: "usb:1-1.2"})
	if got := w.message(); !strings.Contains(got, "usb:1-1.2") {
		t.Fatalf("the second snapshot is missing the added device:\n%s", got)
	}

	s.SetState("usb:1-1.2", StateOffline)
	third := w.message()
	if !strings.Contains(third, "offline") {
		t.Fatalf("the third snapshot is missing the state change:\n%s", third)
	}
	// Snapshots are whole state, never deltas: a consumer that missed one is
	// not left with a corrupted model.
	if !strings.Contains(third, "usb:1-1.1") {
		t.Fatalf("the third snapshot dropped the unchanged device:\n%s", third)
	}

	s.SetState("usb:1-1.2", StateAbsent)
	if got := w.message(); strings.Contains(got, "usb:1-1.2") {
		t.Fatalf("an absent device is still streamed:\n%s", got)
	}

	// Only the increments that provably precede a payload we have already read
	// are asserted; the last one races with our own read by construction.
	if got := s.Stats().Snapshots; got < 3 {
		t.Fatalf("Stats().Snapshots = %d, want at least 3", got)
	}
}

func TestTrackDevicesSeveredAfterNSnapshots(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Add(Device{Serial: "SER1", Devpath: "usb:1-1.1"})
	// "Die after one device-list update" — an opportunity on a stream is one
	// snapshot.
	s.ResetAfter("host:track-devices", 1)

	w := dial(t, s)
	w.send("host:track-devices-l")
	if st := w.status(); st != "OKAY" {
		t.Fatalf("track-devices status = %q", st)
	}
	if got := w.message(); !strings.Contains(got, "usb:1-1.1") {
		t.Fatalf("first snapshot:\n%s", got)
	}

	s.Add(Device{Serial: "SER2", Devpath: "usb:1-1.2"})
	var hdr [4]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err == nil {
		t.Fatal("the second snapshot arrived; the scripted reset did not fire")
	}
	if got := s.Stats().Faults; got != 1 {
		t.Fatalf("Stats().Faults = %d, want 1", got)
	}
	// The listener keeps working, so a reader can come straight back.
	if got := hostOK(t, s, "host:devices"); !strings.Contains(got, "SER2") {
		t.Fatalf("listing after the severed stream:\n%s", got)
	}
}

func TestSeverAllModelsAServerRestart(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Add(Device{Serial: "SER1", Devpath: "usb:1-1.1"})

	w := dial(t, s)
	w.send("host:track-devices-l")
	w.status()
	w.message()
	// Pinned before the sever so the teardown assertion below cannot pass by
	// having had nothing to tear down.
	if got := s.Stats().Trackers; got != 1 {
		t.Fatalf("Stats().Trackers = %d before SeverAll, want 1", got)
	}

	if n := s.SeverAll(); n != 1 {
		t.Fatalf("SeverAll severed %d connections, want 1", n)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err == nil {
		t.Fatal("the tracking socket survived SeverAll")
	}
	// The server side has to notice too, and noticing is the entire reason the
	// stream watches its peer. Nothing mutates the table after this point, so a
	// tracker that only discovered a dead socket on its next write would sit in
	// its select forever — still counted here, and leaking one goroutine per
	// iteration in any test that severs and reconnects in a loop.
	eventually(t, "the severed tracker to stop being counted", func() bool {
		return s.Stats().Trackers == 0
	})
	// Every socket in the farm died at once and the server is still there,
	// which is the whole shape of an adb server restart.
	if got := hostOK(t, s, "host:version"); got != "0029" {
		t.Fatalf("the listener did not survive SeverAll: %q", got)
	}
}

func TestWaitForIsAnsweredTwice(t *testing.T) {
	t.Parallel()

	const devpath = "usb:1-2.1"
	s := Start(t, WithDevices(Device{Serial: "SER1", Devpath: devpath, State: StateOffline}))

	w := dial(t, s)
	// First OKAY: the request was accepted. Nothing here is on a timer.
	w.okBare("host:wait-for-usb-device")
	s.SetState(devpath, StateDevice)
	// Second OKAY: the condition holds.
	if st := w.status(); st != "OKAY" {
		t.Fatalf("second wait-for status = %q", st)
	}

	dis := dial(t, s)
	dis.okBare("host-usb:" + devpath + ":wait-for-usb-disconnect")
	s.SetState(devpath, StateAbsent)
	if st := dis.status(); st != "OKAY" {
		t.Fatalf("wait-for-disconnect status = %q", st)
	}
}

func TestFlappingDeviceKeepsChangingState(t *testing.T) {
	t.Parallel()

	// A flapping handset is the most common thing in a real farm and the input
	// STF #663 turns into data loss. Every transition pushes a snapshot.
	s := Start(t, FlappingFixture(Device{}, 5*time.Millisecond))

	w := dial(t, s)
	w.send("host:track-devices-l")
	if st := w.status(); st != "OKAY" {
		t.Fatalf("track-devices status = %q", st)
	}

	seen := map[string]bool{}
	for !seen[string(StateDevice)] || !seen[string(StateOffline)] {
		snapshot := strings.TrimSuffix(w.message(), "\n")
		fields := strings.Fields(snapshot)
		if len(fields) < 2 {
			t.Fatalf("snapshot %q has no state field", snapshot)
		}
		if fields[0] != FlapSerial {
			t.Fatalf("snapshot names %q, want %q", fields[0], FlapSerial)
		}
		seen[fields[1]] = true
	}
	if d, ok := s.Device(FlapDevpath); !ok {
		t.Fatalf("the flapping device left the table: %+v", d)
	}
}

// ---------------------------------------------------------------------
// Observation
// ---------------------------------------------------------------------

func TestRequestLog(t *testing.T) {
	t.Parallel()

	s := Start(t, TwoClonesFixture())
	dev := mustDevice(t, s, CloneDevpathA)

	hostOK(t, s, "host:version")
	hostOK(t, s, "host-usb:"+CloneDevpathA+":get-serialno")
	hostFail(t, s, "host-serial:"+CloneSerial+":get-state")

	reqs := s.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests recorded, want 3: %+v", len(reqs), reqs)
	}
	if reqs[0].Service != "host:version" || reqs[0].Reply != "OKAY" || reqs[0].Devpath != "" {
		t.Errorf("host:version recorded as %+v", reqs[0])
	}
	if reqs[1].Target != CloneDevpathA || reqs[1].Devpath != CloneDevpathA {
		t.Errorf("a position-addressed request recorded as %+v", reqs[1])
	}
	// A refusal reached no device, so it names no position — which is exactly
	// what makes RequestsTo trustworthy.
	if reqs[2].Devpath != "" {
		t.Errorf("an ambiguous request was credited to %q", reqs[2].Devpath)
	}
	if !strings.HasPrefix(reqs[2].Reply, "FAIL: ") {
		t.Errorf("a refusal recorded as %q", reqs[2].Reply)
	}
	if reqs[0].At.IsZero() || reqs[1].At.Before(reqs[0].At) {
		t.Errorf("requests are not in arrival order: %v then %v", reqs[0].At, reqs[1].At)
	}
	if got := s.Stats().Requests; got != 3 {
		t.Errorf("Stats().Requests = %d, want 3", got)
	}
	if got := s.Stats().Accepted; got != 3 {
		t.Errorf("Stats().Accepted = %d, want 3", got)
	}
	if n := len(s.RequestsTo(dev.Devpath)); n != 1 {
		t.Errorf("RequestsTo(%s) = %d, want 1", dev.Devpath, n)
	}
}

func TestCloseIsIdempotentAndStopsTheListener(t *testing.T) {
	t.Parallel()

	s := Start(t)
	s.Add(Device{Serial: "SER1", Devpath: "usb:1-1.1"})
	w := dial(t, s)
	w.send("host:track-devices-l")
	w.status()
	w.message()

	// Close severs every open connection and waits for every goroutine the
	// server started, so a leaked tracker shows up as a hang here rather than
	// as a mystery in whichever test runs next.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err == nil {
		t.Fatal("the tracking socket survived Close")
	}
	if got := s.Stats().Trackers; got != 0 {
		t.Fatalf("Stats().Trackers = %d after Close, want 0", got)
	}
}

// ---------------------------------------------------------------------
// Framing helpers
// ---------------------------------------------------------------------

func TestFraming(t *testing.T) {
	t.Parallel()

	if got, want := string(frame("")), "0000"; got != want {
		t.Errorf("frame(\"\") = %q, want %q", got, want)
	}
	if got, want := string(frame("abc")), "0003abc"; got != want {
		t.Errorf("frame(\"abc\") = %q, want %q", got, want)
	}
	if got, want := string(okayFrame("hi")), "OKAY0002hi"; got != want {
		t.Errorf("okayFrame = %q, want %q", got, want)
	}
	if got, want := string(failBytes("no")), "FAIL0002no"; got != want {
		t.Errorf("failBytes = %q, want %q", got, want)
	}

	// The real protocol cannot describe more than the header can express
	// either, so an oversize payload is truncated rather than mis-framed.
	long := frame(strings.Repeat("x", maxPayload+10))
	if got, want := len(long), 4+maxPayload; got != want {
		t.Errorf("oversize frame is %d bytes, want %d", got, want)
	}
	if got := string(long[:4]); got != "ffff" {
		t.Errorf("oversize frame header = %q, want %q", got, "ffff")
	}

	if got := summarize(failBytes("why")); got != "FAIL: why" {
		t.Errorf("summarize(FAIL) = %q", got)
	}
	if got := summarize(okayFrame("payload")); got != "OKAY" {
		t.Errorf("summarize(OKAY) = %q", got)
	}
}

func TestReadFrame(t *testing.T) {
	t.Parallel()

	got, err := readFrame(strings.NewReader("000chost:version"))
	if err != nil || got != "host:version" {
		t.Fatalf("readFrame = %q, %v", got, err)
	}
	if got, err := readFrame(strings.NewReader("0000")); err != nil || got != "" {
		t.Fatalf("readFrame of an empty payload = %q, %v", got, err)
	}
	if _, err := readFrame(strings.NewReader("zzzzpayload")); err == nil {
		t.Fatal("a malformed length header was accepted")
	}
	if _, err := readFrame(strings.NewReader("0010short")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a truncated payload = %v, want io.ErrUnexpectedEOF", err)
	}
}
