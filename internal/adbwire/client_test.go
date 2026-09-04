package adbwire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// testTimeout bounds every test context. It is a failure guard, not a
// synchronisation device: nothing in this file waits for it on a healthy path.
const testTimeout = 30 * time.Second

func testContext(tb testing.TB) context.Context {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	tb.Cleanup(cancel)
	return ctx
}

// quietLogger keeps the tracker's reconnect bookkeeping out of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// dialFake returns a client pointed at a fake server, with reconnect delays
// short enough that a reconnect test finishes in milliseconds.
func dialFake(tb testing.TB, srv *fakeadb.Server, opts ...Option) *Client {
	tb.Helper()
	base := []Option{
		WithLogger(quietLogger()),
		WithBackoff(Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}),
	}
	return New(srv.Addr(), append(base, opts...)...)
}

// ---------------------------------------------------------------------------
// Host protocol round trips
// ---------------------------------------------------------------------------

func TestVersionRoundTrip(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t)
	cli := dialFake(t, srv)
	ctx := testContext(t)

	// The wire carries four hex digits. A client parsing them base 10 reads
	// version 41 as 65 and silently negotiates against the wrong server.
	got, err := cli.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != fakeadb.DefaultHostVersion {
		t.Fatalf("Version = %d, want %d", got, fakeadb.DefaultHostVersion)
	}

	srv.SetHostVersion(0x1f)
	if got, err = cli.Version(ctx); err != nil || got != 0x1f {
		t.Fatalf("Version after SetHostVersion = %d, %v; want 31", got, err)
	}
	if ep := cli.Endpoint(); ep != srv.Addr() {
		t.Fatalf("Endpoint() = %q, want %q", ep, srv.Addr())
	}
}

func TestFeaturesRoundTrip(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{
		Serial: "SERFEAT", Devpath: devpath, Model: "Pixel 6a",
	}))
	cli := dialFake(t, srv)
	ctx := testContext(t)

	server, err := cli.ServerFeatures(ctx)
	if err != nil {
		t.Fatalf("ServerFeatures: %v", err)
	}
	if !slices.Equal(server, fakeadb.DefaultFeatures()) {
		t.Fatalf("ServerFeatures = %v, want %v", server, fakeadb.DefaultFeatures())
	}

	// A device's feature set is negotiated with the device and is addressed by
	// position; the server's is not the same answer.
	srv.Update(devpath, func(d *fakeadb.Device) { d.Features = []string{"shell_v2", "abb_exec"} })
	dev, err := cli.Features(ctx, devpath)
	if err != nil {
		t.Fatalf("Features: %v", err)
	}
	if !slices.Equal(dev, []string{"shell_v2", "abb_exec"}) {
		t.Fatalf("Features = %v, want the device's own set", dev)
	}

	srv.SetFeatures()
	if got, err := cli.ServerFeatures(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ServerFeatures with an empty set = %v, %v; want no features and no error", got, err)
	}
}

func TestDevicesListingRoundTrip(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	srv.Add(fakeadb.Device{Serial: "SEROFF", Devpath: "usb:2-1.1", State: fakeadb.StateOffline})
	// No serial: the server prints "(no serial number)" for a transport whose
	// serial it has not read, which is exactly the handset an operator is
	// looking for when they run this command.
	srv.Add(fakeadb.Device{Devpath: "usb:2-1.2", State: fakeadb.StateUnauthorized})
	srv.Add(fakeadb.Device{Devpath: "usb:2-1.3", State: fakeadb.StateUnauthorized})
	srv.Add(fakeadb.Device{
		Serial:  "SERPERM",
		Devpath: "usb:2-1.4",
		State:   fakeadb.State("no permissions; see [http://developer.android.com/tools/device.html]"),
	})

	cli := dialFake(t, srv)
	snap, err := cli.Devices(testContext(t))
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}

	if snap.Endpoint != srv.Addr() {
		t.Errorf("snapshot endpoint = %q, want %q", snap.Endpoint, srv.Addr())
	}
	// A one-shot listing belongs to no tracker, so it has no position in any
	// series and must not claim one.
	if snap.Sequence != 0 {
		t.Errorf("one-shot listing carries Sequence %d, want 0", snap.Sequence)
	}
	if snap.At.IsZero() {
		t.Error("snapshot has no timestamp")
	}
	if len(snap.Devices) != 6 {
		t.Fatalf("listed %d devices, want 6: %+v", len(snap.Devices), snap.Devices)
	}

	idx := snap.ByDevpath()
	if len(idx) != 6 {
		t.Fatalf("ByDevpath indexed %d of 6 devices: %v", len(idx), idx)
	}

	clone := idx[fakeadb.CloneDevpathA]
	if clone.Serial != fakeadb.CloneSerial || clone.State != StateDevice {
		t.Errorf("clone A parsed as %+v", clone)
	}
	// The server substitutes underscores for spaces so whitespace in a model
	// name cannot be mistaken for a field separator.
	if clone.Model != "Pixel_6a" || clone.Product != "bluejay" || clone.Codename != "bluejay" {
		t.Errorf("identity tail parsed as product=%q model=%q device=%q", clone.Product, clone.Model, clone.Codename)
	}
	if clone.TransportID == 0 {
		t.Error("transport id was not parsed")
	}

	if got := idx["usb:2-1.1"].State; got != StateOffline {
		t.Errorf("offline device parsed as %q", got)
	}
	unauth := idx["usb:2-1.2"]
	if unauth.State != StateUnauthorized {
		t.Errorf("(no serial number) line parsed as state %q, want unauthorized", unauth.State)
	}
	if unauth.Serial != "" {
		t.Errorf("(no serial number) recorded serial %q; the honest record is that the server has not read one", unauth.Serial)
	}
	perm := idx["usb:2-1.4"]
	if perm.State != StateNoPermissions {
		t.Errorf("udev sentence parsed as %q, want no_permissions", perm.State)
	}
	if !strings.Contains(perm.RawState, "http://developer.android.com") {
		t.Errorf("raw state %q lost the URL an operator needs", perm.RawState)
	}

	// Only the clones collide. The two serial-less handsets must not be
	// reported as sharing a serial, or every host with a pair of unauthorized
	// devices would look like it had duplicates.
	if got, want := snap.AmbiguousSerials(), []string{fakeadb.CloneSerial}; !slices.Equal(got, want) {
		t.Fatalf("AmbiguousSerials() = %v, want %v", got, want)
	}
}

func TestStateAndSerialAreAddressedByPosition(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	cli := dialFake(t, srv)
	ctx := testContext(t)

	st, err := cli.State(ctx, fakeadb.CloneDevpathB)
	if err != nil || st != StateDevice {
		t.Fatalf("State(%s) = %q, %v", fakeadb.CloneDevpathB, st, err)
	}
	// Position to serial is the safe direction, and the only way a serial
	// legitimately enters the database.
	serial, err := cli.SerialOf(ctx, fakeadb.CloneDevpathB)
	if err != nil || serial != fakeadb.CloneSerial {
		t.Fatalf("SerialOf(%s) = %q, %v", fakeadb.CloneDevpathB, serial, err)
	}
}

func TestKillTolerantOfAServerThatAnswersAndLeaves(t *testing.T) {
	t.Parallel()

	t.Run("the server answers", func(t *testing.T) {
		t.Parallel()

		srv := fakeadb.Start(t)
		if err := dialFake(t, srv).Kill(testContext(t)); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		reqs := srv.Requests()
		if len(reqs) != 1 || reqs[0].Service != "host:kill" {
			t.Fatalf("server saw %+v, want one host:kill", reqs)
		}
	})

	// The half Kill actually branches on. Some server builds drop the socket on
	// the way out instead of answering, and the request was to make the server
	// go away — so here, and only here, a peer close is the success case. Every
	// other call in this package treats one as a failure, which is exactly why
	// the exception has to be exercised rather than assumed.
	t.Run("the server severs the socket on its way out", func(t *testing.T) {
		t.Parallel()

		srv := fakeadb.Start(t)
		srv.ResetNext("host:kill", 0)

		before := counterValue(t, transportErrorsTotal, "kill", KindPeerClosed.String())
		if err := dialFake(t, srv).Kill(testContext(t)); err != nil {
			t.Fatalf("Kill against a server that hung up = %v; the request was to make it go away", err)
		}
		// Swallowing the error is not the same as pretending the socket was
		// fine. The blip is still counted, because incrementing that counter is
		// the only side effect any error in this package may have.
		if after := counterValue(t, transportErrorsTotal, "kill", KindPeerClosed.String()); after < before+1 {
			t.Fatalf("transport_errors_total went %v -> %v; the close was swallowed uncounted", before, after)
		}
		reqs := srv.Requests()
		if len(reqs) != 1 || reqs[0].Service != "host:kill" || reqs[0].Reply != "RESET" {
			t.Fatalf("server saw %+v, want one severed host:kill", reqs)
		}
	})
}

// ---------------------------------------------------------------------------
// The clone test
// ---------------------------------------------------------------------------

// TestDevpathAddressingReachesExactlyOneClone is the test the whole addressing
// rule exists for.
//
// Two physical devices share the serial "0123456789ABCDEF" — STF's own README
// documents a handset shipping with it, and OEMs ship batches. They are
// indistinguishable by serial and distinguishable only by USB position. A
// recovery action resolved by serial lands on whichever transport the server
// hands back, which may be a healthy device hours into somebody else's run.
func TestDevpathAddressingReachesExactlyOneClone(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	cli := dialFake(t, srv)
	ctx := testContext(t)

	target, ok := srv.Device(fakeadb.CloneDevpathB)
	if !ok {
		t.Fatalf("fixture did not install %s", fakeadb.CloneDevpathB)
	}

	if _, err := cli.State(ctx, fakeadb.CloneDevpathB); err != nil {
		t.Fatalf("State(%s): %v", fakeadb.CloneDevpathB, err)
	}
	if _, err := cli.SerialOf(ctx, fakeadb.CloneDevpathB); err != nil {
		t.Fatalf("SerialOf(%s): %v", fakeadb.CloneDevpathB, err)
	}

	// A device-side service, which is the shape a recovery action takes.
	st, err := cli.OpenService(ctx, fakeadb.CloneDevpathB, "shell:id")
	if err != nil {
		t.Fatalf("OpenService(%s): %v", fakeadb.CloneDevpathB, err)
	}
	out, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("reading the service stream: %v", err)
	}
	if cerr := st.Close(); cerr != nil {
		t.Fatalf("closing the stream: %v", cerr)
	}
	if want := fakeadb.Echo(target, "shell:id"); string(out) != want {
		t.Fatalf("service reached the wrong device:\n got %q\nwant %q", out, want)
	}
	if st.Devpath() != fakeadb.CloneDevpathB || st.Service() != "shell:id" {
		t.Fatalf("stream reports devpath=%q service=%q", st.Devpath(), st.Service())
	}

	// The whole point: the other clone was never touched.
	if hits := srv.RequestsTo(fakeadb.CloneDevpathA); len(hits) != 0 {
		t.Fatalf("%d requests reached the wrong clone at %s: %+v", len(hits), fakeadb.CloneDevpathA, hits)
	}
	hits := srv.RequestsTo(fakeadb.CloneDevpathB)
	if len(hits) != 4 { // get-state, get-serialno, the transport switch, the service
		t.Fatalf("%d requests reached %s, want 4: %+v", len(hits), fakeadb.CloneDevpathB, hits)
	}

	// Nothing was ever addressed by the shared serial, at any layer.
	for _, r := range srv.Requests() {
		if r.Target == fakeadb.CloneSerial || strings.Contains(r.Service, fakeadb.CloneSerial) {
			t.Fatalf("a request was addressed by the duplicated serial: %+v", r)
		}
	}

	// And the listing tells the watchdog the serial is ambiguous before any
	// recovery action has a chance to resolve it.
	snap, err := cli.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if got, want := snap.AmbiguousSerials(), []string{fakeadb.CloneSerial}; !slices.Equal(got, want) {
		t.Fatalf("AmbiguousSerials() = %v, want %v", got, want)
	}
	idx := snap.ByDevpath()
	a, aok := idx[fakeadb.CloneDevpathA]
	b, bok := idx[fakeadb.CloneDevpathB]
	if !aok || !bok {
		t.Fatalf("the devpath index lost a clone: %v", idx)
	}
	if a.Serial != b.Serial {
		t.Fatalf("the clones no longer share a serial: %q vs %q", a.Serial, b.Serial)
	}
	if a.TransportID == b.TransportID {
		t.Fatalf("both clones report transport id %d", a.TransportID)
	}
}

func TestUnsafeBySerialIsAmbiguousForClones(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	cli := dialFake(t, srv)
	ctx := testContext(t)

	tr, err := cli.UnsafeBySerial(ctx, fakeadb.CloneSerial)
	if err == nil {
		_ = tr.Close()
		t.Fatal("a serial shared by two devices resolved to a transport")
	}
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("UnsafeBySerial(%q) = %v, want ErrAmbiguousTarget", fakeadb.CloneSerial, err)
	}
	var ae *AmbiguousTargetError
	if !errors.As(err, &ae) || !ae.BySerial {
		t.Fatalf("the error does not record that the lookup was serial-addressed: %+v", ae)
	}
	// A unique serial still resolves, which is what makes this usable for
	// bootstrap enrolment of a device whose position is not yet known.
	srv.Add(fakeadb.Device{Serial: "UNIQUE0001", Devpath: "usb:4-1.1"})
	tr, err = cli.UnsafeBySerial(ctx, "UNIQUE0001")
	if err != nil {
		t.Fatalf("UnsafeBySerial on a unique serial: %v", err)
	}
	if tr.Devpath() != "" {
		t.Fatalf("a serial-addressed transport claims devpath %q; it does not know the position", tr.Devpath())
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("closing the transport: %v", err)
	}

	for _, bad := range []string{"", "   ", "has space", "has\nnewline"} {
		if _, err := cli.UnsafeBySerial(ctx, bad); err == nil {
			t.Fatalf("UnsafeBySerial(%q) was accepted", bad)
		}
	}
}

// TestInvalidDevpathNeverReachesTheWire is the other half of the addressing
// safety property: a devpath that could retarget the service string is refused
// before a socket is opened, so there is no window in which the server could
// act on it.
func TestInvalidDevpathNeverReachesTheWire(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	cli := dialFake(t, srv)
	ctx := testContext(t)

	for _, bad := range []string{
		"",
		fakeadb.CloneSerial,
		"usb:3-1.4.1:reboot",
		"usb:3-1.4.1 usb:3-1.4.2",
		"usb:3-1.4.1\nhost:kill",
	} {
		calls := map[string]func() error{
			"State":       func() error { _, err := cli.State(ctx, bad); return err },
			"SerialOf":    func() error { _, err := cli.SerialOf(ctx, bad); return err },
			"Features":    func() error { _, err := cli.Features(ctx, bad); return err },
			"Transport":   func() error { _, err := cli.Transport(ctx, bad); return err },
			"Shell":       func() error { _, err := cli.Shell(ctx, bad, "id"); return err },
			"OpenService": func() error { _, err := cli.OpenService(ctx, bad, "shell:id"); return err },
		}
		for name, call := range calls {
			err := call()
			if !errors.Is(err, ErrInvalidDevpath) {
				t.Fatalf("%s(%q) = %v, want ErrInvalidDevpath", name, bad, err)
			}
		}
	}

	if got := srv.Stats().Accepted; got != 0 {
		t.Fatalf("%d connections were opened for devpaths that never validated", got)
	}
	if got := srv.Requests(); len(got) != 0 {
		t.Fatalf("the server saw %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Device-side services
// ---------------------------------------------------------------------------

func TestShellV2OverTheWire(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-1.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERSHELL", Devpath: devpath}))

	var framed bytes.Buffer
	for _, p := range []struct {
		id      ShellPacketID
		payload []byte
	}{
		{ShellStdout, []byte("uid=0(root)\n")},
		{ShellStderr, []byte("warning\n")},
		{ShellExit, []byte{9}},
	} {
		if err := WriteShellPacket(&framed, p.id, p.payload); err != nil {
			t.Fatalf("building the scripted reply: %v", err)
		}
	}
	srv.Respond(devpath, "shell,v2,raw:", framed.String())

	cli := dialFake(t, srv)
	res, err := cli.Shell(testContext(t), devpath, "id")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if string(res.Stdout) != "uid=0(root)\n" || string(res.Stderr) != "warning\n" {
		t.Fatalf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if !res.Exited || res.ExitCode != 9 {
		t.Fatalf("exited=%t code=%d, want true/9", res.Exited, res.ExitCode)
	}

	var sawShell bool
	for _, r := range srv.RequestsTo(devpath) {
		if r.Service == ShellService("id") {
			sawShell = true
		}
	}
	if !sawShell {
		t.Fatalf("the device never received %q: %+v", ShellService("id"), srv.RequestsTo(devpath))
	}
}

func TestServiceMayBeStartedOncePerTransport(t *testing.T) {
	t.Parallel()

	const devpath = "usb:5-2.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERONCE", Devpath: devpath}))
	cli := dialFake(t, srv)
	ctx := testContext(t)

	tr, err := cli.Transport(ctx, devpath)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	defer tr.Close()
	if tr.Devpath() != devpath {
		t.Fatalf("Devpath() = %q, want %q", tr.Devpath(), devpath)
	}
	if _, err := tr.Service(ctx, "shell:id"); err != nil {
		t.Fatalf("Service: %v", err)
	}
	// A second service string on the same socket would be written into the
	// middle of the first, and the server would answer whatever it managed to
	// parse — against a device somebody else may be using.
	if _, err := tr.Service(ctx, "shell:reboot"); err == nil {
		t.Fatal("a second service was accepted on a transport that already carries one")
	}
}

// TestHealthQuestionsStayAnswerableForAnUnusableDevice keeps the third clock
// separate from the other two: a device the server refuses to switch a
// transport to still answers "what state are you in", and the refusal is a
// protocol answer rather than a wire failure.
func TestHealthQuestionsStayAnswerableForAnUnusableDevice(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-1.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERSICK", Devpath: devpath}))
	cli := dialFake(t, srv)
	ctx := testContext(t)

	for _, tc := range []struct {
		wire fakeadb.State
		want ConnState
	}{
		{fakeadb.StateOffline, StateOffline},
		{fakeadb.StateUnauthorized, StateUnauthorized},
		{fakeadb.StateBootloader, StateBootloader},
		{fakeadb.StateDevice, StateDevice},
	} {
		srv.SetState(devpath, tc.wire)
		got, err := cli.State(ctx, devpath)
		if err != nil {
			t.Fatalf("State with the device %q: %v", tc.wire, err)
		}
		if got != tc.want {
			t.Fatalf("State = %q, want %q", got, tc.want)
		}
		if got.Usable() != (tc.want == StateDevice) {
			t.Fatalf("%q.Usable() = %t", got, got.Usable())
		}
	}

	srv.SetState(devpath, fakeadb.StateOffline)
	_, err := cli.Transport(ctx, devpath)
	if !IsProtocol(err) {
		t.Fatalf("a refused transport switch = %v (%T), want a protocol failure", err, err)
	}
	if IsTransport(err) {
		t.Fatal("the server's refusal was classified as a socket failure")
	}
	if IsNotFound(err) {
		t.Fatal("an offline device was classified as absent; those are different alarms")
	}
	var pe *ProtocolError
	if errors.As(err, &pe) && pe.Reason != fakeadb.MsgDeviceOffline {
		t.Fatalf("refusal reason = %q, want the server's own words %q", pe.Reason, fakeadb.MsgDeviceOffline)
	}
}

func TestAbsentDevicePositionIsNotFound(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-1.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERGONE", Devpath: devpath}))
	cli := dialFake(t, srv)
	ctx := testContext(t)

	// The cable came out. The server stops reporting the transport; the
	// control plane still has a row, and may still have a live lease.
	srv.SetState(devpath, fakeadb.StateAbsent)

	_, err := cli.State(ctx, devpath)
	if !IsNotFound(err) {
		t.Fatalf("State on an absent position = %v, want a not-found", err)
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("%T is not *NotFoundError", err)
	}
	if nf.Target != devpath || nf.BySerial {
		t.Fatalf("not-found recorded as %+v; it must name the position it looked for", nf)
	}
	// One lookup is a single observation, not a state: it is not a transport
	// failure and it is not cancellation.
	if IsTransport(err) || IsCanceled(err) {
		t.Fatalf("absence was reported as a wire failure: %v", err)
	}

	snap, err := cli.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(snap.Devices) != 0 {
		t.Fatalf("an absent device is still listed: %+v", snap.Devices)
	}
}

// ---------------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------------

func TestFailReplySurfacesTheServersReason(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SERFAIL", Devpath: "usb:8-1.1"}))
	cli := dialFake(t, srv)
	ctx := testContext(t)

	const reason = "insufficient permissions to open usbfs node"
	srv.FailNext("host:devices-l", reason)

	_, err := cli.Devices(ctx)
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("a FAIL reply produced %v (%T), want *ProtocolError", err, err)
	}
	if pe.Reason != reason {
		t.Fatalf("Reason = %q, want the server's verbatim message %q", pe.Reason, reason)
	}
	if pe.Op != "devices" || pe.Service != "host:devices-l" {
		t.Fatalf("protocol error names op=%q service=%q", pe.Op, pe.Service)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("error %q drops the reason", err)
	}
	// The connection worked. Classifying a refusal as a wire failure would
	// make an operator chase a network problem that does not exist.
	if IsTransport(err) || IsCanceled(err) || IsNotFound(err) {
		t.Fatalf("a FAIL reply was misclassified: %v", err)
	}
	if got := srv.Stats().Faults; got != 1 {
		t.Fatalf("the server applied %d faults, want 1", got)
	}
	// FailNext fires once; the next call must succeed.
	if _, err := cli.Devices(ctx); err != nil {
		t.Fatalf("the call after a one-shot FAIL: %v", err)
	}
}

// TestMidStreamResetIsATransportBlipAndNothingMore is the #663 shape. A
// connection severed with a TCP RST while a reply is in flight must classify as
// a socket failure, bump exactly one counter, and carry no verdict about the
// device on the far end.
func TestMidStreamResetIsATransportBlipAndNothingMore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		afterBytes int
	}{
		{"severed before the status word", 0},
		{"severed inside the status word", 2},
		{"severed inside the length prefix", 6},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := fakeadb.Start(t, fakeadb.WithDevices(
				fakeadb.Device{Serial: "SERRST", Devpath: "usb:9-1.1"},
			))
			cli := dialFake(t, srv)

			before := counterValue(t, transportErrorsTotal, "devices", KindPeerClosed.String())
			srv.ResetNext("host:devices-l", tc.afterBytes)

			snap, err := cli.Devices(testContext(t))
			te, ok := AsTransport(err)
			if !ok {
				t.Fatalf("a severed connection produced %v (%T), want *TransportError", err, err)
			}
			// EOF and ECONNRESET both mean the far side went away; both are
			// this kind, on every platform the farm builds for.
			if te.Kind != KindPeerClosed || !te.PeerClosed() {
				t.Fatalf("kind = %v (peer_closed=%t), want peer_closed", te.Kind, te.PeerClosed())
			}
			if te.Op != "devices" || te.Endpoint != srv.Addr() {
				t.Fatalf("blip recorded as op=%q endpoint=%q", te.Op, te.Endpoint)
			}
			if te.At.IsZero() {
				t.Error("the blip has no timestamp, so a caller cannot measure how long the run has lasted")
			}
			// Everything a caller might read as "the device is free" must be
			// false. There is no such answer in this package, and a socket
			// error is the one input that must never produce one.
			if IsNotFound(err) || errors.Is(err, ErrAmbiguousTarget) || IsProtocol(err) || IsCanceled(err) {
				t.Fatalf("a socket failure was classified as something about the device: %v", err)
			}
			if len(snap.Devices) != 0 {
				t.Fatalf("a failed listing returned devices: %+v", snap.Devices)
			}
			if after := counterValue(t, transportErrorsTotal, "devices", KindPeerClosed.String()); after < before+1 {
				t.Fatalf("transport_errors_total went %v -> %v; the blip was not counted", before, after)
			}

			// The fake keeps listening: a reconnect works immediately, which is
			// the only response this failure justifies.
			if _, err := cli.Devices(testContext(t)); err != nil {
				t.Fatalf("reconnect after the reset: %v", err)
			}
		})
	}
}

// TestHangIsBoundedAndIsNeverPeerClosed covers the server that accepts the
// connection and then says nothing. The call must end on the caller's own
// clock, and the error must say "nobody answered" — never "the far side went
// away", which is the classification an operator reads as a dead device.
func TestHangIsBoundedAndIsNeverPeerClosed(t *testing.T) {
	t.Parallel()

	// Both cases below ask to be cut off after 200ms. This bound is generous
	// enough for a loaded machine and still far below DefaultCallTimeout, so a
	// regression that drops the caller's bound and falls back to the package
	// default — or to the test guard — fails here instead of passing slowly.
	const bound = 5 * time.Second

	assertSilence := func(t *testing.T, err error, elapsed time.Duration) {
		t.Helper()
		if err == nil {
			t.Fatal("a server that never answered produced no error")
		}
		if elapsed >= bound {
			t.Fatalf("the call took %v; it was not bounded by the caller's clock", elapsed)
		}
		if te, ok := AsTransport(err); ok {
			if te.Kind != KindTimeout || !te.Timeout() {
				t.Fatalf("kind = %v, want timeout", te.Kind)
			}
			if te.PeerClosed() {
				t.Fatal("silence was reported as the peer going away")
			}
		} else if !IsCanceled(err) {
			t.Fatalf("%v (%T) is neither a timeout nor a cancellation", err, err)
		}
		// Nothing here may look like a statement about the device.
		if IsNotFound(err) || errors.Is(err, ErrAmbiguousTarget) || IsProtocol(err) {
			t.Fatalf("silence was classified as something about the device: %v", err)
		}
	}

	t.Run("bounded by the caller's context deadline", func(t *testing.T) {
		t.Parallel()

		srv := fakeadb.Start(t)
		cli := dialFake(t, srv)
		srv.HangNext("host:version")

		// The socket deadline and the context fire together, so the call comes
		// back either as the caller's cancellation or as a plain timeout. Both
		// say the same thing; neither says the peer disappeared.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := cli.Version(ctx)
		assertSilence(t, err, time.Since(start))
	})

	t.Run("bounded by the per-call fallback when the context carries no deadline", func(t *testing.T) {
		t.Parallel()

		srv := fakeadb.Start(t)
		cli := dialFake(t, srv, WithCallTimeout(200*time.Millisecond))
		srv.HangNext("host:version")

		start := time.Now()
		_, err := cli.Version(context.Background())
		elapsed := time.Since(start)
		assertSilence(t, err, elapsed)

		// With no context in play the classification is unambiguous: a wedged
		// host, not a shutdown.
		te, ok := AsTransport(err)
		if !ok {
			t.Fatalf("%v (%T) is not a transport failure", err, err)
		}
		if te.Kind != KindTimeout {
			t.Fatalf("kind = %v, want timeout", te.Kind)
		}
		if IsCanceled(err) {
			t.Fatal("an unreachable host was reported as an orderly shutdown")
		}
	})
}

func TestDialFailureIsATransportFailure(t *testing.T) {
	t.Parallel()

	// Port 1 is below every platform's ephemeral range, so nothing in this
	// test binary can be listening there. This is the shape of a host whose
	// adb server has died: the phones are almost certainly fine, the host is
	// not, and the difference must survive into the classification.
	const addr = "127.0.0.1:1"

	// context.Background(), not testContext: a context carrying a deadline
	// bounds the dial itself, and the per-call fallback this client was built
	// with would never run. It also keeps a host that black-holes port 1
	// instead of refusing from parking here until the guard deadline and then
	// failing as a cancellation for the wrong reason.
	cli := New(addr, WithCallTimeout(2*time.Second), WithLogger(quietLogger()))
	start := time.Now()
	_, err := cli.Version(context.Background())
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Fatalf("the dial took %v; the per-call fallback did not bound it", elapsed)
	}
	te, ok := AsTransport(err)
	if !ok {
		t.Fatalf("dialling a dead endpoint produced %v (%T), want *TransportError", err, err)
	}
	switch te.Kind {
	case KindDial, KindPeerClosed, KindTimeout:
	default:
		t.Fatalf("kind = %v, want a dial-side failure", te.Kind)
	}
	// A dial cut off by the fallback comes back as a net error carrying a
	// deadline, and on some paths that chain reaches context.DeadlineExceeded.
	// A plain errors.Is here would therefore file an unreachable host as an
	// orderly shutdown and hide it from the check an operator alerts on;
	// TestIsCanceledDoesNotMistakeAnUnreachableHostForAShutdown pins the
	// wrapping case directly.
	if IsCanceled(err) {
		t.Fatal("an unreachable host was reported as cancellation")
	}
	if te.Endpoint != addr || te.Devpath != "" {
		t.Fatalf("blip recorded as endpoint=%q devpath=%q", te.Endpoint, te.Devpath)
	}
}

// ---------------------------------------------------------------------------
// track-devices
// ---------------------------------------------------------------------------

// nextSnapshot returns the first snapshot satisfying want, failing the test if
// the tracker stops or the guard elapses first. Every snapshot it steps over is
// checked as well, because the property under test is as much about what is
// never delivered as about what eventually is.
func nextSnapshot(tb testing.TB, tr *Tracker, what string, want func(Snapshot) bool, each func(Snapshot)) Snapshot {
	tb.Helper()
	guard := time.NewTimer(testTimeout)
	defer guard.Stop()
	for {
		select {
		case snap, open := <-tr.Snapshots():
			if !open {
				tb.Fatalf("the tracker stopped before %s; last error: %v", what, tr.LastError())
			}
			if each != nil {
				each(snap)
			}
			if want(snap) {
				return snap
			}
		case <-guard.C:
			tb.Fatalf("no snapshot showing %s arrived; last error: %v", what, tr.LastError())
		}
	}
}

func TestTrackDevicesDeliversASnapshotPerMutation(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t)
	srv.Add(fakeadb.Device{Serial: "SERTRK1", Devpath: "usb:1-1.1", Model: "Pixel 6a"})

	cli := dialFake(t, srv)
	tr := cli.TrackDevices(testContext(t))
	t.Cleanup(tr.Close)

	first := nextSnapshot(t, tr, "the initial listing", func(s Snapshot) bool {
		return len(s.Devices) == 1
	}, nil)
	if first.Sequence == 0 {
		t.Error("a tracker snapshot has no position in its series")
	}
	if first.Endpoint != srv.Addr() {
		t.Errorf("snapshot endpoint = %q, want %q", first.Endpoint, srv.Addr())
	}

	srv.Add(fakeadb.Device{Serial: "SERTRK2", Devpath: "usb:1-1.2"})
	second := nextSnapshot(t, tr, "both devices", func(s Snapshot) bool {
		return len(s.Devices) == 2
	}, nil)
	if second.Sequence <= first.Sequence {
		t.Errorf("sequence went %d -> %d", first.Sequence, second.Sequence)
	}
	idx := second.ByDevpath()
	if _, ok := idx["usb:1-1.1"]; !ok {
		t.Errorf("the first device fell out of the snapshot: %v", idx)
	}
	if _, ok := idx["usb:1-1.2"]; !ok {
		t.Errorf("the added device is missing: %v", idx)
	}

	// A state change is a mutation like any other, and the snapshot is whole
	// state rather than a delta.
	srv.SetState("usb:1-1.2", fakeadb.StateOffline)
	offline := nextSnapshot(t, tr, "the second device offline", func(s Snapshot) bool {
		return s.ByDevpath()["usb:1-1.2"].State == StateOffline
	}, nil)
	if len(offline.Devices) != 2 {
		t.Fatalf("a state change dropped a device: %+v", offline.Devices)
	}

	// The cable came out: the device leaves the listing.
	if !srv.Remove("usb:1-1.2") {
		t.Fatal("Remove reported no such device")
	}
	gone := nextSnapshot(t, tr, "the removal", func(s Snapshot) bool {
		return len(s.Devices) == 1
	}, nil)
	if gone.Devices[0].Devpath != "usb:1-1.1" {
		t.Fatalf("the wrong device survived: %+v", gone.Devices)
	}

	if tr.LastError() != nil {
		t.Fatalf("a healthy tracker reports %v", tr.LastError())
	}

	// Close is the only thing that stops a tracker; it waits for the reader
	// and its socket to be gone, and it is idempotent.
	tr.Close()
	tr.Close()
	guard := time.NewTimer(testTimeout)
	defer guard.Stop()
	for {
		select {
		case _, open := <-tr.Snapshots():
			if !open {
				return
			}
		case <-guard.C:
			t.Fatal("the snapshot channel was never closed")
		}
	}
}

// TestTrackerReconnectsWithoutSynthesisingAnEmptyList is the tracking-socket
// half of #663. The connection dies mid-stream; the reader must reconnect and
// keep reporting what the server actually says. It must NEVER emit an empty or
// shrunken list to paper over the gap, because that list is the input the
// watchdog reads.
func TestTrackerReconnectsWithoutSynthesisingAnEmptyList(t *testing.T) {
	t.Parallel()

	const hubSize = 3
	srv := fakeadb.Start(t, fakeadb.HubFixture(3, 1, hubSize))
	cli := dialFake(t, srv)

	// Let the first snapshot through, then sever the socket on the next one.
	srv.ResetAfter("host:track-devices", 1)

	tr := cli.TrackDevices(testContext(t))
	t.Cleanup(tr.Close)

	noShrinkage := func(s Snapshot) {
		if len(s.Devices) < hubSize {
			t.Errorf("a snapshot reported %d of %d devices; losing the socket is evidence about the socket, not about the phones",
				len(s.Devices), hubSize)
		}
	}

	nextSnapshot(t, tr, "the hub", func(s Snapshot) bool {
		return len(s.Devices) == hubSize
	}, noShrinkage)

	// This mutation is the write that gets severed. The reader reconnects on
	// its own and re-reads the whole list.
	srv.Add(fakeadb.Device{Serial: "HUBNEW", Devpath: "usb:3-1.99"})
	after := nextSnapshot(t, tr, "the fourth device after the reconnect", func(s Snapshot) bool {
		return len(s.Devices) == hubSize+1
	}, noShrinkage)

	if _, ok := after.ByDevpath()["usb:3-1.99"]; !ok {
		t.Fatalf("the new device is missing after the reconnect: %+v", after.Devices)
	}
	if srv.Stats().Faults != 1 {
		t.Fatalf("the fake applied %d faults, want the single scripted reset", srv.Stats().Faults)
	}
}

func TestTrackerSurvivesAServerRestart(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
	cli := dialFake(t, srv)

	tr := cli.TrackDevices(testContext(t))
	t.Cleanup(tr.Close)

	nextSnapshot(t, tr, "the clones", func(s Snapshot) bool { return len(s.Devices) == 2 }, nil)

	// Every socket in the farm dies at the same instant, which is what an adb
	// server restart looks like from here. It is a reconnect and nothing else.
	if n := srv.SeverAll(); n == 0 {
		t.Fatal("SeverAll found no connections to sever")
	}

	srv.Add(fakeadb.Device{Serial: "AFTERRESTART", Devpath: "usb:3-1.4.3"})
	snap := nextSnapshot(t, tr, "the device added after the restart", func(s Snapshot) bool {
		return len(s.Devices) == 3
	}, func(s Snapshot) {
		if len(s.Devices) == 0 {
			t.Error("an empty snapshot was synthesised from a dead socket")
		}
	})
	if got, want := snap.AmbiguousSerials(), []string{fakeadb.CloneSerial}; !slices.Equal(got, want) {
		t.Fatalf("AmbiguousSerials() = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// The import barrier
// ---------------------------------------------------------------------------

// TestPackageCannotReachAllocation enforces the barrier doc.go declares.
//
// doc.go states that this package must not import internal/lease and must
// carry no allocation vocabulary, and asks for a check that scans every file
// except doc.go. This is that check. It lives in a test rather than in a lint
// script because a test is the one thing here that a future change cannot
// merge past without seeing.
//
// It scans the production files only. doc.go is exempt because stating the
// barrier requires naming what is barred, and the _test.go files are exempt
// for the same reason — this one spells out every forbidden word in order to
// go looking for it. That exemption costs nothing: the breach the barrier
// exists to prevent is a socket error acquiring a code path to an allocation
// decision, and only shipped code can build that path.
func TestPackageCannotReachAllocation(t *testing.T) {
	t.Parallel()

	// A socket failure here may return a typed error and increment a counter.
	// Anything that could carry it further — the package that owns leases, the
	// packages permitted to end one, or a database handle of any kind — is
	// unreachable by construction as long as it cannot be imported.
	forbiddenImports := []string{
		"device-farmer/internal/lease",
		"device-farmer/internal/reaper",
		"device-farmer/internal/scheduler",
		"database/sql",
		"jackc/pgx",
	}
	// Word boundaries on purpose: "placeholder" in the Winsock comment is not a
	// lease holder, and a check that cried wolf would be deleted within a week.
	forbiddenWords := regexp.MustCompile(
		`(?i)\b(lease|leases|leased|fence|fenced|holder|reclaim|reclaimed|quarantine|quarantined|allocation|deallocate)\b`)

	// go test runs with the working directory set to the package source
	// directory, so this sees every file in the package — including one added
	// after this check was written, which is the whole point.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if name == "doc.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		scanned++
		text := string(src)
		for _, imp := range forbiddenImports {
			if strings.Contains(text, imp) {
				t.Errorf("%s references %q; a transport failure must not be able to reach an allocation decision, even transitively",
					name, imp)
			}
		}
		for i, line := range strings.Split(text, "\n") {
			if m := forbiddenWords.FindString(line); m != "" {
				t.Errorf("%s:%d uses allocation vocabulary %q: %s",
					name, i+1, m, strings.TrimSpace(line))
			}
		}
	}

	// A scan that silently examined nothing would pass forever. The package has
	// production files other than doc.go; if it stops having them, this check
	// has stopped meaning anything and must say so.
	if scanned == 0 {
		t.Fatal("the barrier scan read no production files; it is asserting nothing")
	}
}
