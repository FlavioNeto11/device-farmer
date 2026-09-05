package enroll

// What these tests protect: a sighting is EVIDENCE, and the only thing that
// turns evidence into identity is farm.resolve_device. So the Go side must
// (a) read a device without ever addressing it by serial, (b) refuse to draw a
// fingerprint from an answer that was not the one asked for, and (c) hash the
// properties that survive a factory reset — and nothing that changes on an
// OTA. Each of those is a way a phone gets handed to the wrong job.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// ---------------------------------------------------------------------------
// Fixtures shared by this package's tests
// ---------------------------------------------------------------------------

// pixelSerial is what a Pixel 6a burns into ro.boot.serialno. It is a fixture
// value, never an address: every call in these tests goes to a devpath.
const pixelSerial = "1B091FDF6001XX"

// pixelProps is what a Pixel 6a answers, keyed the way the probe asks.
func pixelProps(serial string) map[string]string {
	return map[string]string{
		propManufacturer: "Google",
		propModel:        "Pixel 6a",
		propName:         "bluejay",
		propDevice:       "bluejay",
		propRelease:      "14",
		propSDK:          "34",
		propABIList:      "arm64-v8a,armeabi-v7a,armeabi",
		propBuildFP:      "google/bluejay/bluejay:14/AP2A.240805.005/12025142:user/release-keys",
		propHardware:     "bluejay",
		propBootSerial:   serial,
		propSerial:       serial,
	}
}

// probeAnswer renders what the probe command prints on a device: the brand
// line first, then one line per property in the order the device was asked.
func probeAnswer(uid string, props map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s=%s\n", uidKey, uid)
	for _, k := range probeProps {
		fmt.Fprintf(&b, "%s=%s\n", k, props[k])
	}
	return b.String()
}

// shellV2 frames a device's answer the way adbd does: one stdout packet and
// an exit packet. fakeadb scripts raw bytes, so the framing is the test's job.
func shellV2(t *testing.T, stdout string, exit byte) string {
	t.Helper()
	return shellV2Stderr(t, stdout, "", exit)
}

func shellV2Stderr(t *testing.T, stdout, stderr string, exit byte) string {
	t.Helper()
	var b bytes.Buffer
	if stdout != "" {
		if err := adbwire.WriteShellPacket(&b, adbwire.ShellStdout, []byte(stdout)); err != nil {
			t.Fatalf("framing stdout: %v", err)
		}
	}
	if stderr != "" {
		if err := adbwire.WriteShellPacket(&b, adbwire.ShellStderr, []byte(stderr)); err != nil {
			t.Fatalf("framing stderr: %v", err)
		}
	}
	if err := adbwire.WriteShellPacket(&b, adbwire.ShellExit, []byte{exit}); err != nil {
		t.Fatalf("framing exit: %v", err)
	}
	return b.String()
}

// dial opens the same kind of client the shipping enroller opens, with the
// per-call timeout short enough for a hang to be observed in a test.
func dial(srv *fakeadb.Server, opts ...adbwire.Option) *adbwire.Client {
	base := []adbwire.Option{
		adbwire.WithCallTimeout(2 * time.Second),
		adbwire.WithMaxOutput(maxProbeOutput),
		adbwire.WithLogger(quietLogger()),
	}
	return adbwire.New(srv.Addr(), append(base, opts...)...)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// listed returns the listing entry for one position, the way enrollHost sees
// it, so a probe in a test starts from what the ADB server said rather than
// from a struct the test invented.
func listed(t *testing.T, cli *adbwire.Client, devpath string) adbwire.Device {
	t.Helper()
	snap, err := cli.Devices(t.Context())
	if err != nil {
		t.Fatalf("listing devices: %v", err)
	}
	dev, ok := snap.ByDevpath()[devpath]
	if !ok {
		t.Fatalf("the listing has no device at %s: %+v", devpath, snap.Devices)
	}
	return dev
}

// shellsTo counts the device-side shell requests one position received.
func shellsTo(srv *fakeadb.Server, devpath string) int {
	n := 0
	for _, r := range srv.RequestsTo(devpath) {
		if strings.HasPrefix(r.Service, "shell") {
			n++
		}
	}
	return n
}

// assertNoSerialAddressing fails if any request the fake saw was aimed at a
// serial. The listing is host-level and carries no target; everything else
// must carry a devpath.
func assertNoSerialAddressing(t *testing.T, srv *fakeadb.Server, serials ...string) {
	t.Helper()
	for _, r := range srv.Requests() {
		for _, s := range serials {
			if r.Target == s || strings.Contains(r.Service, "host-serial:"+s) {
				t.Errorf("a request was addressed by serial %q: %+v", s, r)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Parsing what the device printed
// ---------------------------------------------------------------------------

// TestParseProps: the probe's key=value lines become a bounded, storable map,
// and an answer with more keys than the probe could have asked for is reported
// as not being the answer to the question.
//
// Falsify: change `len(props) >= maxPropKeys` to `>` in parseProps — the
// sixty-fifth key is then admitted and the noisy case reports complete.
func TestParseProps(t *testing.T) {
	t.Parallel()

	t.Run("lines, CRLF and padding", func(t *testing.T) {
		props, ok := parseProps([]byte("a=1\r\n b = 2 \nnoequals\n=orphan\nk=v=w\n"))
		if !ok {
			t.Fatal("a twelve-line answer was reported as noisy")
		}
		want := map[string]string{"a": "1", "b": "2", "k": "v=w"}
		if len(props) != len(want) {
			t.Fatalf("props = %v, want %v", props, want)
		}
		for k, v := range want {
			if props[k] != v {
				t.Errorf("props[%q] = %q, want %q", k, props[k], v)
			}
		}
	})

	t.Run("the last value of a repeated key wins and counts once", func(t *testing.T) {
		props, ok := parseProps([]byte("a=1\na=2\n"))
		if !ok || len(props) != 1 || props["a"] != "2" {
			t.Fatalf("props = %v ok=%v, want {a:2} true", props, ok)
		}
	})

	t.Run("control bytes and invalid UTF-8 never reach the database", func(t *testing.T) {
		props, _ := parseProps([]byte("k=a\x00b\x7fc\nbad=\xff\xfe\n"))
		if props["k"] != "abc" {
			t.Errorf("props[k] = %q, want the NUL and DEL mapped out", props["k"])
		}
		// An invalid byte is REPLACED, not dropped: U+FFFD is what PostgreSQL
		// accepts where 0xff would fail the whole INSERT with 22021, and a
		// visible replacement character tells an operator the device said
		// something unrepresentable rather than nothing.
		if got := props["bad"]; !utf8.ValidString(got) || strings.ContainsRune(got, 0) || got != "��" {
			t.Errorf("props[bad] = %q, want each invalid byte replaced by U+FFFD so the value is storable", got)
		}
	})

	t.Run("an oversized value is cut on a rune boundary and marked", func(t *testing.T) {
		// One ASCII byte then two-byte runes, so the byte at maxPropValue is
		// the second half of a rune and a naive cut would leave half of it.
		v := "a" + strings.Repeat("é", maxPropValue)
		props, _ := parseProps([]byte("k=" + v + "\n"))
		got := props["k"]
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("a %d-byte value was stored whole (%d bytes)", len(v), len(got))
		}
		if !strings.HasPrefix(got, "aé") || strings.ContainsRune(got, '�') {
			t.Fatalf("the cut broke a rune: %q", got[:8])
		}
		if len(got) > maxPropValue+len("…") {
			t.Fatalf("truncated value is %d bytes, over the %d-byte bound", len(got), maxPropValue)
		}
	})

	t.Run("more keys than the probe asked for is not an answer", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < maxPropKeys+1; i++ {
			fmt.Fprintf(&b, "junk.%02d=x\n", i)
		}
		props, ok := parseProps([]byte(b.String()))
		if ok {
			t.Fatal("an answer with more keys than the probe could ask for was reported complete")
		}
		// What arrived is still evidence and is still returned.
		if len(props) != maxPropKeys {
			t.Fatalf("kept %d keys, want the first %d", len(props), maxPropKeys)
		}
	})
}

func TestSplitABIs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"arm64-v8a,armeabi-v7a,armeabi", []string{"arm64-v8a", "armeabi-v7a", "armeabi"}},
		{" arm64-v8a , armeabi-v7a ", []string{"arm64-v8a", "armeabi-v7a"}},
		{"", nil},
		{" , ", []string{}},
	}
	for _, c := range cases {
		got := splitABIs(c.in)
		if len(got) != len(c.want) || (c.want == nil) != (got == nil) {
			t.Errorf("splitABIs(%q) = %#v, want %#v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitABIs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestUSBPath: the ADB server says "usb:3-1.4", the schema says "3-1.4", and
// every lookup against farm.slots speaks the second.
func TestUSBPath(t *testing.T) {
	t.Parallel()
	if got := USBPath("usb:3-1.4"); got != "3-1.4" {
		t.Errorf("USBPath(usb:3-1.4) = %q", got)
	}
	if got := USBPath("3-1.4"); got != "3-1.4" {
		t.Errorf("USBPath(3-1.4) = %q, want it unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// The hardware fingerprint
// ---------------------------------------------------------------------------

// TestHardwareFingerprintIsStableAndNeedsMoreThanASerial pins the contract
// farm.resolve_device's second rung depends on: the same phone hashes the same
// way on every sighting, two units of one model hash differently, a serial on
// its own hashes to nothing, and the build properties are NOT inputs — an OTA
// must not re-fingerprint the whole rack overnight.
//
// Falsify: add {"build", props[propBuildFP]} to the components in
// HardwareFingerprint — the OTA case then produces a different digest.
func TestHardwareFingerprintIsStableAndNeedsMoreThanASerial(t *testing.T) {
	t.Parallel()

	props := pixelProps(pixelSerial)
	fp1, keys := HardwareFingerprint(props, pixelSerial)
	fp2, _ := HardwareFingerprint(pixelProps(pixelSerial), pixelSerial)
	if fp1 == nil || !bytes.Equal(fp1, fp2) {
		t.Fatalf("the same properties hashed differently: %x vs %x", fp1, fp2)
	}
	if want := []string{"manufacturer", "model", "device", "hardware", "serial"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("components = %v, want %v", keys, want)
	}

	// An OTA changes the build fingerprint and the release; the digest must
	// not move, or every phone falls off the fingerprint rung at once.
	ota := pixelProps(pixelSerial)
	ota[propBuildFP] = "google/bluejay/bluejay:15/AP3A.250105.008/12701944:user/release-keys"
	ota[propRelease] = "15"
	ota[propSDK] = "35"
	if fp, _ := HardwareFingerprint(ota, pixelSerial); !bytes.Equal(fp, fp1) {
		t.Fatal("an OTA changed the hardware fingerprint")
	}

	// A second unit of the same model is a different phone.
	other := pixelProps("2C081FDF6002YY")
	if fp, _ := HardwareFingerprint(other, "2C081FDF6002YY"); bytes.Equal(fp, fp1) {
		t.Fatal("two units of one model hashed alike")
	}

	// The serial the DEVICE reports wins over what the ADB server reported,
	// so a bogus USB serial descriptor does not fuse two phones.
	if fp, _ := HardwareFingerprint(props, "0123456789ABCDEF"); !bytes.Equal(fp, fp1) {
		t.Fatal("the ADB serial changed a digest that had ro.boot.serialno")
	}
	// ...and is the fallback when the device reports none.
	bare := pixelProps("")
	fpADB, keysADB := HardwareFingerprint(bare, "0123456789ABCDEF")
	if fpADB == nil || keysADB[len(keysADB)-1] != "serial" {
		t.Fatalf("a device with only an ADB serial got fp=%x keys=%v", fpADB, keysADB)
	}

	// Missing components are left out and the names say so.
	thin := map[string]string{propManufacturer: "Google", propModel: "Pixel 6a", propSerial: pixelSerial}
	fpThin, keysThin := HardwareFingerprint(thin, "")
	if fpThin == nil || strings.Join(keysThin, ",") != "manufacturer,model,serial" {
		t.Fatalf("three components: fp=%x keys=%v", fpThin, keysThin)
	}
	if bytes.Equal(fpThin, fp1) {
		t.Fatal("dropping two components did not change the digest")
	}

	// Below the floor there is no fingerprint: a digest shared by every unit
	// of a model would make farm.resolve_device answer 'ambiguous' for all
	// of them.
	if fp, keys := HardwareFingerprint(map[string]string{propSerial: pixelSerial}, ""); fp != nil || keys != nil {
		t.Fatalf("a serial alone produced a fingerprint: %x %v", fp, keys)
	}
	noSerial := pixelProps("")
	if fp, _ := HardwareFingerprint(noSerial, ""); fp != nil {
		t.Fatalf("a device with no serial anywhere produced a fingerprint: %x", fp)
	}
}

// ---------------------------------------------------------------------------
// The probe command
// ---------------------------------------------------------------------------

// TestBuildProbeCommandReadsTheBrandFirstAndRefusesAnUnsafeKey: the brand is
// the first line printed because it is the strongest evidence, and a key that
// would need shell quoting fails at package initialisation rather than
// silently changing the question.
//
// Falsify: delete the propKeyRe check in buildProbeCommand.
func TestBuildProbeCommandReadsTheBrandFirstAndRefusesAnUnsafeKey(t *testing.T) {
	t.Parallel()

	cmd := buildProbeCommand([]string{"ro.product.model", "ro.serialno"})
	if !strings.HasPrefix(cmd, `printf '%s=%s\n' `+uidKey+` "$(cat `+BrandPath) {
		t.Fatalf("the probe does not print the brand first: %s", cmd)
	}
	if !strings.Contains(cmd, " ro.product.model ro.serialno;") {
		t.Fatalf("the probe does not ask for its keys in order: %s", cmd)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("a key carrying shell syntax was accepted")
		}
	}()
	buildProbeCommand([]string{"ro.x; reboot"})
}

// TestResolvable: what counts as enough evidence to hand to
// farm.resolve_device. Adoption on nothing mints a phantom, so the bar is a
// brand, or a serial together with something that names the model.
func TestResolvable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   Identity
		ok   bool
		why  string
	}{
		{"unreadable", Identity{Unreadable: "unauthorized"}, false, "unauthorized"},
		{"branded, nothing else", Identity{FarmUID: "df-" + strings.Repeat("a", 32)}, true, ""},
		{"no serial anywhere", Identity{Manufacturer: "Google", Model: "Pixel 6a", Props: map[string]string{}}, false, "identity_incomplete"},
		{"serial but no model", Identity{Serial: pixelSerial, Props: map[string]string{}}, false, "identity_incomplete"},
		{"serial from getprop and a codename", Identity{Codename: "bluejay", Props: map[string]string{propSerial: pixelSerial}}, true, ""},
		{"ADB serial and a model", Identity{Serial: pixelSerial, Model: "Pixel 6a", Props: map[string]string{}}, true, ""},
	}
	for _, c := range cases {
		ok, why := c.id.Resolvable()
		if ok != c.ok || why != c.why {
			t.Errorf("%s: Resolvable() = (%v, %q), want (%v, %q)", c.name, ok, why, c.ok, c.why)
		}
	}
}

// ---------------------------------------------------------------------------
// Probe over the wire
// ---------------------------------------------------------------------------

// TestProbeReadsIdentityInOneRoundTrip: one shell call, addressed by devpath,
// yields the whole identity, and what the device says about itself wins over
// the listing's underscored tail.
//
// Falsify: swap the arguments of firstNonEmpty for id.Model in Probe — the
// listing's "Pixel_6a" then beats the device's "Pixel 6a".
func TestProbeReadsIdentityInOneRoundTrip(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{
		Serial: pixelSerial, Devpath: devpath, Model: "Pixel 6a", Product: "bluejay", Codename: "bluejay",
	}))
	srv.Respond(devpath, adbwire.ShellService(probeCommand),
		shellV2(t, probeAnswer("", pixelProps(pixelSerial)), 0))

	cli := dial(srv)
	dev := listed(t, cli, devpath)
	if dev.Model != "Pixel_6a" {
		t.Fatalf("the listing reports model %q; this test relies on it being underscored", dev.Model)
	}

	id := Probe(t.Context(), cli, dev, 2*time.Second)

	if !id.Readable() {
		t.Fatalf("probe unreadable: %s (%v)", id.Unreadable, id.Err)
	}
	if id.Devpath != devpath || id.USBPath != "3-1.4.1" {
		t.Errorf("position = %q/%q", id.Devpath, id.USBPath)
	}
	if id.Serial != pixelSerial || id.State != adbwire.StateDevice || id.TransportID != dev.TransportID {
		t.Errorf("listing facts not carried: serial=%q state=%q tid=%d", id.Serial, id.State, id.TransportID)
	}
	if id.FarmUID != "" || id.MalformedUID != "" {
		t.Errorf("an unbranded device read as uid=%q malformed=%q", id.FarmUID, id.MalformedUID)
	}
	if id.Manufacturer != "Google" || id.Model != "Pixel 6a" || id.Product != "bluejay" || id.Codename != "bluejay" {
		t.Errorf("identity = %q %q %q %q", id.Manufacturer, id.Model, id.Product, id.Codename)
	}
	if id.AndroidRelease != "14" || id.SDKInt != 34 {
		t.Errorf("release/sdk = %q/%d", id.AndroidRelease, id.SDKInt)
	}
	if strings.Join(id.ABIs, ",") != "arm64-v8a,armeabi-v7a,armeabi" {
		t.Errorf("abis = %v", id.ABIs)
	}
	if id.BuildFingerprint != pixelProps("")[propBuildFP] {
		t.Errorf("build fingerprint = %q", id.BuildFingerprint)
	}
	wantFP, wantKeys := HardwareFingerprint(pixelProps(pixelSerial), pixelSerial)
	if !bytes.Equal(id.HWFingerprint, wantFP) || strings.Join(id.FingerprintKeys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("fingerprint over the wire differs from the pure function: %x %v", id.HWFingerprint, id.FingerprintKeys)
	}
	if ok, why := id.Resolvable(); !ok {
		t.Errorf("a complete sighting is not resolvable: %s", why)
	}
	if id.ProbeDuration <= 0 {
		t.Errorf("probe duration = %v", id.ProbeDuration)
	}

	if n := shellsTo(srv, devpath); n != 1 {
		t.Errorf("the device saw %d shell requests, want exactly 1: %+v", n, srv.RequestsTo(devpath))
	}
	assertNoSerialAddressing(t, srv, pixelSerial)
}

// TestProbeReadsTheBrandAndRefusesAStrangers: a farm uid in the brand file is
// the strongest evidence; anything else in that file is recorded and never
// treated as identity.
//
// Falsify: drop the uidRe check in Probe and assign props[uidKey] to FarmUID
// unconditionally.
func TestProbeReadsTheBrandAndRefusesAStrangers(t *testing.T) {
	t.Parallel()

	const branded, tampered = "usb:3-1.4.1", "usb:3-1.4.2"
	const uid = "df-0123456789abcdef0123456789abcdef"
	srv := fakeadb.Start(t, fakeadb.WithDevices(
		fakeadb.Device{Serial: pixelSerial, Devpath: branded},
		fakeadb.Device{Serial: "2C081FDF6002YY", Devpath: tampered},
	))
	srv.Respond(branded, adbwire.ShellService(probeCommand),
		shellV2(t, probeAnswer(uid, pixelProps(pixelSerial)), 0))
	srv.Respond(tampered, adbwire.ShellService(probeCommand),
		shellV2(t, probeAnswer("hello from another farm", pixelProps("2C081FDF6002YY")), 0))
	cli := dial(srv)

	id := Probe(t.Context(), cli, listed(t, cli, branded), 2*time.Second)
	if id.FarmUID != uid || id.MalformedUID != "" {
		t.Errorf("branded device read as uid=%q malformed=%q", id.FarmUID, id.MalformedUID)
	}

	id = Probe(t.Context(), cli, listed(t, cli, tampered), 2*time.Second)
	if id.FarmUID != "" || id.MalformedUID != "hello from another farm" {
		t.Errorf("tampered device read as uid=%q malformed=%q", id.FarmUID, id.MalformedUID)
	}
	if ok, _ := id.Resolvable(); !ok {
		t.Error("a device with a stranger's brand file still carries a serial and a model, and must resolve on those")
	}
}

// TestProbeOnAHungDeviceIsUnreadableNotGuessed: a phone that never answers is
// a sighting with a reason, not an error and not an identity. Nothing about
// the outcome names the serial.
//
// Falsify: in Probe, on err != nil set only id.Err and fall through to parsing
// — the identity then reads as complete and resolvable.
func TestProbeOnAHungDeviceIsUnreadableNotGuessed(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: pixelSerial, Devpath: devpath}))
	srv.Inject(fakeadb.Fault{Match: "shell", Devpath: devpath, Kind: fakeadb.FaultHang})
	cli := dial(srv)

	id := Probe(t.Context(), cli, listed(t, cli, devpath), 300*time.Millisecond)

	if id.Readable() || id.Unreadable != "probe_timeout" {
		t.Fatalf("unreadable = %q, want probe_timeout", id.Unreadable)
	}
	if id.Err == nil {
		t.Fatal("a timed-out probe carries no error for the log")
	}
	if strings.Contains(id.Err.Error(), pixelSerial) {
		t.Errorf("the error names the serial: %v", id.Err)
	}
	if id.Devpath != devpath {
		t.Errorf("the sighting lost its position: %q", id.Devpath)
	}
	if id.HWFingerprint != nil || id.Manufacturer != "" {
		t.Errorf("a silent device was given an identity: fp=%x manufacturer=%q", id.HWFingerprint, id.Manufacturer)
	}
	if ok, why := id.Resolvable(); ok || why != "probe_timeout" {
		t.Errorf("Resolvable() = (%v, %q)", ok, why)
	}
	assertNoSerialAddressing(t, srv, pixelSerial)
}

// TestProbeSkipsAnUnauthorizedTransport: the ADB dialog still on screen is the
// most common thing in a fresh rack. It is recorded as a sighting and costs no
// connection, because the server would refuse the service anyway.
//
// Falsify: remove the `!dev.State.Usable()` gate in Probe — the fake then
// refuses the transport and the reason becomes probe_failed after a request.
func TestProbeSkipsAnUnauthorizedTransport(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{
		Serial: pixelSerial, Devpath: devpath, State: fakeadb.StateUnauthorized,
	}))
	cli := dial(srv)

	id := Probe(t.Context(), cli, listed(t, cli, devpath), 2*time.Second)

	if id.Unreadable != "unauthorized" || id.Err != nil {
		t.Fatalf("unreadable = %q err = %v, want unauthorized and no error", id.Unreadable, id.Err)
	}
	if id.RawState != "unauthorized" || id.Serial != pixelSerial {
		t.Errorf("the listing's facts were not kept: raw=%q serial=%q", id.RawState, id.Serial)
	}
	if n := shellsTo(srv, devpath); n != 0 {
		t.Errorf("an unauthorized device was sent %d shell requests", n)
	}
}

// TestProbeRefusesANoisyOrTruncatedAnswer: an answer that had to be cut short,
// or that carries far more keys than were asked for, is indistinguishable
// from another device's answer. What arrived is kept as evidence; no
// fingerprint is drawn from it.
//
// Falsify: delete the `case !complete` arm of the switch in Probe.
func TestProbeRefusesANoisyOrTruncatedAnswer(t *testing.T) {
	t.Parallel()

	const noisy, chatty = "usb:3-1.4.1", "usb:3-1.4.2"
	srv := fakeadb.Start(t, fakeadb.WithDevices(
		fakeadb.Device{Serial: pixelSerial, Devpath: noisy},
		fakeadb.Device{Serial: "2C081FDF6002YY", Devpath: chatty},
	))

	var junk strings.Builder
	junk.WriteString(probeAnswer("", pixelProps(pixelSerial)))
	for i := 0; i < maxPropKeys; i++ {
		fmt.Fprintf(&junk, "persist.junk.%02d=x\n", i)
	}
	srv.Respond(noisy, adbwire.ShellService(probeCommand), shellV2(t, junk.String(), 0))
	srv.Respond(chatty, adbwire.ShellService(probeCommand),
		shellV2(t, probeAnswer("", pixelProps("2C081FDF6002YY")), 0))

	cli := dial(srv)
	id := Probe(t.Context(), cli, listed(t, cli, noisy), 2*time.Second)
	if id.Unreadable != "probe_noisy" {
		t.Fatalf("unreadable = %q, want probe_noisy", id.Unreadable)
	}
	if id.Model != "Pixel 6a" || len(id.Props) != maxPropKeys {
		t.Errorf("the evidence that did arrive was not kept: model=%q props=%d", id.Model, len(id.Props))
	}
	if id.HWFingerprint != nil {
		t.Error("a fingerprint was drawn from a noisy answer")
	}

	// A client whose output cap is smaller than the answer sees Truncated.
	small := dial(srv, adbwire.WithMaxOutput(64))
	id = Probe(t.Context(), small, listed(t, small, chatty), 2*time.Second)
	if id.Unreadable != "probe_truncated" {
		t.Fatalf("unreadable = %q, want probe_truncated", id.Unreadable)
	}
	if id.HWFingerprint != nil {
		t.Error("a fingerprint was drawn from a truncated answer")
	}
	if ok, _ := id.Resolvable(); ok {
		t.Error("a truncated sighting was reported resolvable")
	}
}

// TestProbeDropsPartialOutputOnAWireFailure: half a property list is
// indistinguishable from a different device's property list. When the wire
// breaks mid-answer, everything that arrived is discarded.
//
// Falsify: in Probe, parse res.Stdout before checking err.
func TestProbeDropsPartialOutputOnAWireFailure(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: pixelSerial, Devpath: devpath}))

	// Two stdout packets, so that the first one is complete and parseable
	// when the wire is cut in the middle of the second.
	full := probeAnswer("", pixelProps(pixelSerial))
	half := len(full) / 2
	var body bytes.Buffer
	if err := adbwire.WriteShellPacket(&body, adbwire.ShellStdout, []byte(full[:half])); err != nil {
		t.Fatal(err)
	}
	firstPacket := body.Len()
	if err := adbwire.WriteShellPacket(&body, adbwire.ShellStdout, []byte(full[half:])); err != nil {
		t.Fatal(err)
	}
	if err := adbwire.WriteShellPacket(&body, adbwire.ShellExit, []byte{0}); err != nil {
		t.Fatal(err)
	}
	srv.Respond(devpath, adbwire.ShellService(probeCommand), body.String())
	// The reply on the wire is "OKAY" followed by the scripted bytes; sever
	// after the first packet plus a few bytes of the second's header.
	srv.Inject(fakeadb.Fault{Match: "shell", Devpath: devpath, Kind: fakeadb.FaultReset,
		AfterBytes: len("OKAY") + firstPacket + 2})

	cli := dial(srv)
	id := Probe(t.Context(), cli, listed(t, cli, devpath), 2*time.Second)

	if id.Readable() || id.Err == nil {
		t.Fatalf("a severed probe read as readable: unreadable=%q err=%v", id.Unreadable, id.Err)
	}
	if id.Unreadable != "probe_failed" {
		t.Errorf("unreadable = %q, want probe_failed (a reset is not a timeout)", id.Unreadable)
	}
	if len(id.Props) != 0 || id.Manufacturer != "" || id.HWFingerprint != nil {
		t.Errorf("partial output was kept: props=%v manufacturer=%q", id.Props, id.Manufacturer)
	}
}

// TestUnreadableReasonTellsShutdownFromSilence: a probe this process abandoned
// on its way out is not evidence against the phone.
func TestUnreadableReasonTellsShutdownFromSilence(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := unreadableReason(cancelled, context.Canceled); got != "cancelled" {
		t.Errorf("a cancelled parent classified as %q", got)
	}
	if got := unreadableReason(context.Background(), context.DeadlineExceeded); got != "probe_timeout" {
		t.Errorf("a deadline classified as %q", got)
	}
	if got := unreadableReason(context.Background(), adbwire.ErrNotFound); got != "detached" {
		t.Errorf("a vanished transport classified as %q", got)
	}
	if got := unreadableReason(context.Background(), io.ErrUnexpectedEOF); got != "probe_failed" {
		t.Errorf("a wire failure classified as %q", got)
	}
}
