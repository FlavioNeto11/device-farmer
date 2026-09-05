package topo

// What these tests protect: the parser reads the USB tree the way the kernel
// actually lays it out — device directories named by position, interface
// directories named after the canonical path, port objects hanging off the
// hub's interface — and the only thing that makes a device "a phone" is the
// interface triple adb itself matches on. A hub adopted on the wrong evidence
// becomes seven schedulable slots on somebody's desk; a phone not recognised
// as one un-adopts its hub; and a hub read as per-port switchable when it is
// ganged lets the recovery ladder cut power to six devices mid-job.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// ---------------------------------------------------------------------------
// A synthetic /sys/bus/usb/devices
// ---------------------------------------------------------------------------

// fxKind is what sits in one port of a fixture hub.
type fxKind int

const (
	fxPhone    fxKind = iota + 1 // exposes the ADB interface
	fxFastboot                   // exposes the fastboot interface
	fxKeyboard                   // HID: present, foreign
)

// fxHub describes one hub of a fixture tree. A path ending in "-0" is the
// root hub of that bus and is written as the kernel names it, "usbN". Hubs
// plugged into other hubs are simply listed with their own path; Build links
// them through the path arithmetic the same way it links real ones.
type fxHub struct {
	Path     string
	Ports    int
	Power    PowerSwitching
	Attached map[int]fxKind
}

// sysfsFixture renders hubs as an fstest.MapFS shaped like the bus directory:
// one directory per device, attribute files inside it, the interface
// directory under the device, and port objects under the hub's interface.
func sysfsFixture(hubs ...fxHub) fstest.MapFS {
	fsys := fstest.MapFS{}
	put := func(p, v string) { fsys[p] = &fstest.MapFile{Data: []byte(v + "\n"), Mode: 0o444} }

	for _, h := range hubs {
		bus, _, _ := strings.Cut(h.Path, "-")
		root := strings.HasSuffix(h.Path, "-0")
		dir := h.Path
		if root {
			dir = "usb" + bus
			put(dir+"/idVendor", "1d6b")
			put(dir+"/idProduct", "0002")
			put(dir+"/manufacturer", "Linux 6.8.0 xhci-hcd")
			put(dir+"/product", "xHCI Host Controller")
			put(dir+"/serial", "0000:00:14.0")
		} else {
			put(dir+"/idVendor", "05e3")
			put(dir+"/idProduct", "0610")
			put(dir+"/manufacturer", "GenesysLogic")
			put(dir+"/product", "USB2.1 Hub")
		}
		put(dir+"/bDeviceClass", "09")
		put(dir+"/maxchild", strconv.Itoa(h.Ports))
		put(dir+"/speed", "480")

		// The interface directory is named after the CANONICAL path even for
		// a root hub ("usb3/3-0:1.0"); the port objects after the DIRECTORY.
		iface := dir + "/" + h.Path + ":1.0"
		put(iface+"/bInterfaceClass", "09")
		put(iface+"/bInterfaceSubClass", "00")
		put(iface+"/bInterfaceProtocol", "01")

		switch h.Power {
		case GangedPower:
			put(dir+"/wHubCharacteristics", "0x0088")
		case UnknownPower:
			// No port objects at all: the kernel exposed nothing to read.
		}
		if h.Power != UnknownPower {
			for p := 1; p <= h.Ports; p++ {
				pdir := fmt.Sprintf("%s/%s-port%d", iface, dir, p)
				put(pdir+"/connect_type", "hotplug")
				mode := fstest.MapFile{Data: []byte("0\n"), Mode: 0o444}
				if h.Power == PerPortPower {
					mode.Mode = 0o644
				}
				fsys[pdir+"/disable"] = &mode
			}
		}

		for port, kind := range h.Attached {
			path := h.Path + "." + strconv.Itoa(port)
			if root {
				path = bus + "-" + strconv.Itoa(port)
			}
			put(path+"/bDeviceClass", "00")
			put(path+"/speed", "480")
			di := path + "/" + path + ":1.0"
			switch kind {
			case fxPhone:
				put(path+"/idVendor", "18d1")
				put(path+"/idProduct", "4ee7")
				put(path+"/manufacturer", "Google")
				put(path+"/product", "Pixel 6a")
				put(path+"/serial", "PH"+strings.NewReplacer("-", "", ".", "").Replace(path))
				put(di+"/bInterfaceClass", "ff")
				put(di+"/bInterfaceSubClass", "42")
				put(di+"/bInterfaceProtocol", "01")
			case fxFastboot:
				put(path+"/idVendor", "18d1")
				put(path+"/idProduct", "4ee0")
				put(path+"/manufacturer", "Google")
				put(path+"/product", "Android")
				put(di+"/bInterfaceClass", "ff")
				put(di+"/bInterfaceSubClass", "42")
				put(di+"/bInterfaceProtocol", "03")
			case fxKeyboard:
				put(path+"/idVendor", "046d")
				put(path+"/idProduct", "c31c")
				put(path+"/manufacturer", "Logitech")
				put(path+"/product", "USB Keyboard")
				put(di+"/bInterfaceClass", "03")
				put(di+"/bInterfaceSubClass", "01")
				put(di+"/bInterfaceProtocol", "01")
			}
		}
	}
	return fsys
}

// rackFixture is the canonical tree: bus 3, a four-port root hub, and a
// seven-port per-port-switchable hub in root port 1 carrying two phones, one
// handset in fastboot, and a keyboard somebody left in port 5.
func rackFixture() fstest.MapFS {
	return sysfsFixture(
		fxHub{Path: "3-0", Ports: 4},
		fxHub{Path: "3-1", Ports: 7, Power: PerPortPower,
			Attached: map[int]fxKind{1: fxPhone, 2: fxPhone, 3: fxFastboot, 5: fxKeyboard}},
	)
}

func scanFixture(t *testing.T, fsys fstest.MapFS) *Tree {
	t.Helper()
	tree, err := Scan(context.Background(), FromFS(fsys, "fixture"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return tree
}

// ---------------------------------------------------------------------------

// TestFSSourceParsesAHubOfPhones: every attribute the rest of the package
// depends on comes out of the fixture the way it comes out of a real host.
//
// Falsify: in readInterfaces, build the prefix from d.DirName instead of
// d.Path — the root hub's interface is then never found, and on a real host
// neither is anything else's.
func TestFSSourceParsesAHubOfPhones(t *testing.T) {
	t.Parallel()

	devs, err := FromFS(rackFixture(), "fixture").Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	// Pointers, because IsAndroid has a pointer receiver and a map value is
	// not addressable.
	byPath := map[string]*Device{}
	for i := range devs {
		byPath[devs[i].Path] = &devs[i]
	}
	if len(devs) != 6 {
		t.Fatalf("parsed %d devices, want root + hub + 4 attached: %v", len(devs), keys(byPath))
	}

	root := byPath["3-0"]
	if !root.IsRoot || !root.IsHub || root.DirName != "usb3" || root.Bus != 3 || root.Chain != nil || root.Parent != "" {
		t.Errorf("root hub = %+v", root)
	}
	if root.MaxChild != 4 || len(root.Interfaces) != 1 || root.Interfaces[0].Class != hubDeviceClass {
		t.Errorf("root hub ports=%d interfaces=%v", root.MaxChild, root.Interfaces)
	}

	hub := byPath["3-1"]
	if !hub.IsHub || hub.IsRoot || hub.Parent != "3-0" || hub.MaxChild != 7 || hub.VendorID != 0x05e3 || hub.ProductID != 0x0610 {
		t.Errorf("hub = %+v", hub)
	}
	if hub.Power != PerPortPower || !strings.Contains(hub.PowerEvidence, "all 7 ports") {
		t.Errorf("hub power = %s (%s), want per_port on the strength of every port's control", hub.Power, hub.PowerEvidence)
	}

	phone := byPath["3-1.1"]
	if phone.IsHub || phone.Parent != "3-1" || phone.Bus != 3 || fmt.Sprint(phone.Chain) != "[1 1]" {
		t.Errorf("phone = %+v", phone)
	}
	if phone.Manufacturer != "Google" || phone.Product != "Pixel 6a" || phone.Serial != "PH311" || phone.Speed != "480" {
		t.Errorf("phone attributes = %q %q %q %q", phone.Manufacturer, phone.Product, phone.Serial, phone.Speed)
	}
	if !phone.IsAndroid() || !byPath["3-1.3"].IsAndroid() {
		t.Error("a handset exposing ADB or fastboot was not recognised as Android")
	}
	if byPath["3-1.5"].IsAndroid() || byPath["3-1"].IsAndroid() || root.IsAndroid() {
		t.Error("a keyboard or a hub was recognised as Android")
	}
}

// TestIsAndroidNeedsTheAdbOrFastbootTriple: vendor-specific class alone is a
// webcam; the subclass and protocol are what adb's own host code matches on.
//
// Falsify: drop the protocol argument from the HasInterface calls in
// IsAndroid — the (ff,42,02) case is then a phone.
func TestIsAndroidNeedsTheAdbOrFastbootTriple(t *testing.T) {
	t.Parallel()
	cases := []struct {
		iface Interface
		want  bool
	}{
		{Interface{0xff, 0x42, 0x01}, true},
		{Interface{0xff, 0x42, 0x03}, true},
		{Interface{0xff, 0x42, 0x02}, false},
		{Interface{0xff, 0xff, 0x01}, false},
		{Interface{0x03, 0x01, 0x01}, false},
		{Interface{0x09, 0x00, 0x01}, false},
	}
	for _, c := range cases {
		// A phone also exposes MTP alongside ADB; the extra interface must not
		// hide the one that matters.
		d := Device{Interfaces: []Interface{{0x06, 0x01, 0x01}, c.iface}}
		if got := d.IsAndroid(); got != c.want {
			t.Errorf("IsAndroid with %02x/%02x/%02x = %v, want %v",
				c.iface.Class, c.iface.SubClass, c.iface.Protocol, got, c.want)
		}
	}
	if (&Device{}).IsAndroid() {
		t.Error("a device with no interfaces read as Android")
	}
}

// TestBuildAttachesDevicesToPorts: a hub has one Port per downstream socket
// whether or not anything is in it, devices land on the port their path names,
// and a hub in a port is linked both ways.
//
// Falsify: in Build, index ports with `port` instead of `port - 1` — every
// device then lands one socket over and the last one falls off the hub.
func TestBuildAttachesDevicesToPorts(t *testing.T) {
	t.Parallel()

	fsys := rackFixture()
	for k, v := range sysfsFixture(fxHub{Path: "3-1.7", Ports: 4, Power: NoPower, Attached: map[int]fxKind{2: fxPhone}}) {
		fsys[k] = v
	}
	tree := scanFixture(t, fsys)

	if tree.Partial || tree.Source != "fixture" || len(tree.Controllers) != 1 || tree.Controllers[0].Bus != 3 {
		t.Fatalf("tree = partial=%v source=%q controllers=%d problems=%v",
			tree.Partial, tree.Source, len(tree.Controllers), tree.Problems)
	}
	hub := tree.Hub("3-1")
	if hub == nil || len(hub.Ports) != 7 || hub.Upstream != tree.Controllers[0].RootHub || hub.Controller != tree.Controllers[0] {
		t.Fatalf("hub 3-1 = %+v", hub)
	}
	for i, p := range hub.Ports {
		if p.Number != i+1 || p.Path != "3-1."+strconv.Itoa(i+1) {
			t.Errorf("port %d = %d/%s", i, p.Number, p.Path)
		}
	}
	if hub.Ports[0].Attached == nil || hub.Ports[0].Attached.Path != "3-1.1" || hub.Ports[3].Attached != nil {
		t.Errorf("ports 1 and 4 = %+v / %+v", hub.Ports[0].Attached, hub.Ports[3].Attached)
	}
	if hub.Occupied() != 5 || hub.AndroidPorts() != 3 || hub.ForeignPorts() != 1 {
		t.Errorf("occupied=%d android=%d foreign=%d, want 5/3/1 (a downstream hub is neither)",
			hub.Occupied(), hub.AndroidPorts(), hub.ForeignPorts())
	}

	child := tree.Hub("3-1.7")
	if child == nil || hub.Ports[6].Downstream != child || child.Upstream != hub {
		t.Fatalf("the hub in port 7 is not linked: %+v", hub.Ports[6])
	}
	if child.Ports[1].Attached == nil || !child.Ports[1].Attached.IsAndroid() || child.AndroidPorts() != 1 {
		t.Errorf("the phone below the downstream hub was not attached: %+v", child.Ports)
	}
	if tree.Device("3-1.7.2") == nil || tree.Device("3-1.9") != nil {
		t.Error("Device() does not answer for what is there and only what is there")
	}

	root := tree.Controllers[0].RootHub
	if len(root.Ports) != 4 || root.Ports[0].Path != "3-1" || root.Ports[0].Downstream != hub {
		t.Errorf("root hub ports = %+v; a root port is 3-1, never 3-0.1", root.Ports)
	}
	if got := fmt.Sprint(hubPaths(tree.Hubs())); got != "[3-0 3-1 3-1.7]" {
		t.Errorf("Hubs() = %s, want shallow before deep", got)
	}
}

// TestHubPowerNeedsPositiveEvidence: per_port is claimed only when every port
// carries a writable power control or the hub descriptor says so. Anything
// ambiguous is unknown, and unknown is not switchable.
//
// Falsify: in hubPower, return PerPortPower from the `default` arm of the
// port-control switch — a hub with control on some ports is then switchable.
func TestHubPowerNeedsPositiveEvidence(t *testing.T) {
	t.Parallel()

	read := func(fsys fstest.MapFS, path string) Device {
		t.Helper()
		devs, err := FromFS(fsys, "fixture").Devices(context.Background())
		if err != nil {
			t.Fatalf("Devices: %v", err)
		}
		for _, d := range devs {
			if d.Path == path {
				return d
			}
		}
		t.Fatalf("no device at %s", path)
		return Device{}
	}
	hub := func(p PowerSwitching) fstest.MapFS {
		return sysfsFixture(fxHub{Path: "3-0", Ports: 1}, fxHub{Path: "3-1", Ports: 4, Power: p})
	}

	if d := read(hub(PerPortPower), "3-1"); d.Power != PerPortPower || !d.Power.Switchable() {
		t.Errorf("every port controllable: %s (%s)", d.Power, d.PowerEvidence)
	}
	if d := read(hub(NoPower), "3-1"); d.Power != NoPower || d.Power.Switchable() {
		t.Errorf("read-only controls: %s (%s)", d.Power, d.PowerEvidence)
	}
	if d := read(hub(UnknownPower), "3-1"); d.Power != UnknownPower || d.Power.Switchable() {
		t.Errorf("no port objects: %s (%s)", d.Power, d.PowerEvidence)
	}
	if d := read(hub(GangedPower), "3-1"); d.Power != GangedPower || d.Power.Switchable() ||
		!strings.HasPrefix(d.PowerEvidence, "wHubCharacteristics=") {
		t.Errorf("descriptor says ganged: %s (%s)", d.Power, d.PowerEvidence)
	}

	// Control on some ports but not all is half an answer.
	mixed := hub(PerPortPower)
	mixed["3-1/3-1:1.0/3-1-port3/disable"] = &fstest.MapFile{Data: []byte("0\n"), Mode: 0o444}
	if d := read(mixed, "3-1"); d.Power != UnknownPower || !strings.Contains(d.PowerEvidence, "3 of 4") {
		t.Errorf("mixed controls: %s (%s)", d.Power, d.PowerEvidence)
	}

	// The descriptor wins over the port objects when both are present.
	descr := hub(NoPower)
	descr["3-1/wHubCharacteristics"] = &fstest.MapFile{Data: []byte("0x0089\n"), Mode: 0o444}
	if d := read(descr, "3-1"); d.Power != PerPortPower {
		t.Errorf("descriptor says per-port: %s (%s)", d.Power, d.PowerEvidence)
	}

	for p, want := range map[PowerSwitching]string{UnknownPower: "unknown", NoPower: "none", GangedPower: "ganged", PerPortPower: "per_port"} {
		if p.String() != want {
			t.Errorf("%d.String() = %q, want %q", p, p.String(), want)
		}
	}
}

// TestPartialReadIsReportedNotFatal: one unreadable entry costs the host that
// device, not its topology, and the tree says so — because an absent port on
// a partial scan is not evidence that the port is gone.
//
// Falsify: in FSSource.Devices, return the error from readDevice instead of
// appending to failed.
func TestPartialReadIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	fsys := rackFixture()
	delete(fsys, "3-1.2/idVendor")
	src := FromFS(fsys, "fixture")

	devs, err := src.Devices(context.Background())
	var partial *PartialReadError
	if !errors.As(err, &partial) || len(partial.Entries) != 1 || !strings.Contains(partial.Entries[0], "3-1.2") {
		t.Fatalf("Devices = %d devices, err %v; want a PartialReadError naming 3-1.2", len(devs), err)
	}
	if len(devs) != 5 {
		t.Errorf("%d devices came back alongside the error, want the 5 that could be read", len(devs))
	}

	tree, err := Scan(context.Background(), src)
	if err != nil {
		t.Fatalf("Scan returned an error for a partial read: %v", err)
	}
	if !tree.Partial || len(tree.Problems) != 1 || tree.Hub("3-1") == nil || tree.Hub("3-1").Ports[1].Attached != nil {
		t.Errorf("tree partial=%v problems=%v; the hub must still be there with port 2 read as empty",
			tree.Partial, tree.Problems)
	}

	// A device whose hub the scan did not report is inconsistent in the same
	// way: kept, attached to nothing, and the tree marked.
	orphan := rackFixture()
	for k := range orphan {
		if strings.HasPrefix(k, "3-1/") {
			delete(orphan, k)
		}
	}
	tree = scanFixture(t, orphan)
	if !tree.Partial || tree.Hub("3-1") != nil || tree.Device("3-1.1") == nil {
		t.Errorf("orphaned devices: partial=%v problems=%v", tree.Partial, tree.Problems)
	}
	if !strings.Contains(strings.Join(tree.Problems, ";"), "hangs off 3-1") {
		t.Errorf("problems do not name the missing hub: %v", tree.Problems)
	}

	// A device in a port the hub says it does not have: trust the device,
	// distrust the scan.
	overflow := sysfsFixture(fxHub{Path: "3-0", Ports: 1},
		fxHub{Path: "3-1", Ports: 2, Power: NoPower, Attached: map[int]fxKind{3: fxPhone}})
	tree = scanFixture(t, overflow)
	if !tree.Partial || !strings.Contains(strings.Join(tree.Problems, ";"), "port 3 of 3-1") {
		t.Errorf("a device beyond the hub's port count: partial=%v problems=%v", tree.Partial, tree.Problems)
	}

	// When NOTHING could be read the answer is an error, never an empty host.
	broken := rackFixture()
	for k := range broken {
		if strings.HasSuffix(k, "/idVendor") {
			delete(broken, k)
		}
	}
	if _, err := FromFS(broken, "fixture").Devices(context.Background()); err == nil || errors.As(err, &partial) {
		t.Errorf("a tree with no readable device returned %v, want a plain error", err)
	}

	// Two entries claiming one path cannot be registered without pointing a
	// slot at the wrong socket.
	if _, err := Build([]Device{{Path: "3-1", Bus: 3, Chain: []int{1}}, {Path: "3-1", Bus: 3, Chain: []int{1}}}); err == nil {
		t.Error("Build accepted two devices at one path")
	}
}

// TestSysfsNamesAndPaths pins the arithmetic the join key depends on: a root
// hub is "usb3" on disk and "3-0" everywhere else, and a device in its first
// port is "3-1", never "3-0.1".
//
// Falsify: make portPath return hub.Path + "." + port for root hubs too.
func TestSysfsNamesAndPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bus   int
		chain string
		root  bool
		ok    bool
	}{
		{"usb3", 3, "[]", true, true},
		{"3-1", 3, "[1]", false, true},
		{"3-1.4.2", 3, "[1 4 2]", false, true},
		{"3-1:1.0", 0, "[]", false, false},
		{"usb3-port1", 0, "[]", false, false},
		{"3-1.x", 0, "[]", false, false},
		{"-1", 0, "[]", false, false},
		{"usbmon", 0, "[]", false, false},
	}
	for _, c := range cases {
		bus, chain, root, ok := parseSysfsName(c.name)
		if ok != c.ok || (ok && (bus != c.bus || fmt.Sprint(chain) != c.chain || root != c.root)) {
			t.Errorf("parseSysfsName(%q) = (%d, %v, %v, %v), want (%d, %s, %v, %v)",
				c.name, bus, chain, root, ok, c.bus, c.chain, c.root, c.ok)
		}
	}

	if p := canonicalPath(3, nil, true); p != "3-0" {
		t.Errorf("root canonical path = %q", p)
	}
	if p := canonicalPath(3, []int{1, 4}, false); p != "3-1.4" {
		t.Errorf("canonical path = %q", p)
	}
	if p := parentPath(3, []int{1}); p != "3-0" {
		t.Errorf("parent of 3-1 = %q", p)
	}
	if p := parentPath(3, []int{1, 4, 2}); p != "3-1.4" {
		t.Errorf("parent of 3-1.4.2 = %q", p)
	}
	root := &Hub{Device: Device{Path: "3-0", Bus: 3, IsRoot: true}}
	if p := portPath(root, 1); p != "3-1" {
		t.Errorf("root port 1 = %q", p)
	}
	if p := portPath(&Hub{Device: Device{Path: "3-1.4", Bus: 3}}, 5); p != "3-1.4.5" {
		t.Errorf("hub port 5 = %q", p)
	}
}

// TestSysfsRefusesToPretend: "I cannot see the bus" and "the bus is empty"
// must never be the same answer, because the second one retires every slot
// on the host.
func TestSysfsRefusesToPretend(t *testing.T) {
	t.Parallel()
	_, err := Sysfs("")
	if runtime.GOOS != "linux" {
		if err == nil || !strings.Contains(err.Error(), "needs Linux") {
			t.Fatalf("Sysfs on %s = %v, want a refusal naming the platform", runtime.GOOS, err)
		}
		return
	}
	if _, err := Sysfs(t.TempDir() + "/not-a-bus"); err == nil {
		t.Fatal("Sysfs opened a directory that does not exist")
	}
}

func keys(m map[string]*Device) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hubPaths(hubs []*Hub) []string {
	out := make([]string, 0, len(hubs))
	for _, h := range hubs {
		out = append(out, h.Path)
	}
	return out
}
