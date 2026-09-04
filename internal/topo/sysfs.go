package topo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// DefaultSysfsRoot is where the Linux USB core publishes one directory per
// enumerated USB device. Every fact this package needs about the physical
// tree is a text file underneath it.
const DefaultSysfsRoot = "/sys/bus/usb/devices"

// ---------------------------------------------------------------------------
// Why this file reads an fs.FS instead of calling os.ReadFile on /sys
// ---------------------------------------------------------------------------
//
// The USB tree is Linux-only, but the code that turns a tree into slots is
// not, and neither is the arithmetic that turns a USB path into a rack label.
// Reading through [io/fs.FS] keeps all three testable on any machine: a test
// hands [FromFS] an [testing/fstest.MapFS] holding a fixture tree and gets the
// same parser the production path uses. There are no build tags in this
// package, so `go build ./...` and `go test ./...` behave identically on
// Windows and on Linux.
//
// The one thing that genuinely cannot work off Linux is opening the real
// /sys, and [Sysfs] refuses that explicitly rather than returning an empty
// tree. An empty tree is the dangerous answer: discovery would conclude that
// every slot on the host has vanished. Reconciliation has its own guards
// against that (see reconcileRemovals), but the first guard is refusing to
// pretend a scan happened at all.
//
// Two sysfs details drive the parsing below and are easy to get wrong:
//
//   - The entries in /sys/bus/usb/devices are SYMLINKS into /sys/devices, so
//     an fs.DirEntry for a USB device reports mode Symlink, not Dir. Nothing
//     here filters on IsDir, and nothing uses fs.WalkDir, which does not
//     follow symlinks and would therefore see an empty tree on a real host.
//
//   - A device's kernel name encodes its position and is the same string ADB
//     reports as a devpath, so it is the join key for the whole system:
//     "3-1.4" is bus 3, root port 1, then port 4 of the hub in that root
//     port. Root hubs are named "usb3" but address themselves as "3-0", and
//     a device in root port 1 is "3-1", NOT "3-0.1". That asymmetry is why
//     portPath below special-cases the root hub.

// PowerSwitching is what a hub can do to VBUS on one downstream port.
//
// Only [PerPortPower] permits a single-device power cycle. Every other value,
// including [UnknownPower], is reported to the database as "not switchable",
// which makes farm.register_slot create ONE ganged power domain for the hub.
// That is the entire point: the recovery ladder reads the domain to compute a
// blast radius, and a hub wrongly marked per-port would let it cut power to
// seven devices — six of them mid-run, holding live leases — while believing
// it had touched one. Positive evidence is required to claim per-port; the
// absence of evidence is never read as capability.
type PowerSwitching uint8

const (
	// UnknownPower means the kernel exposed nothing we could read. Treated as
	// not switchable.
	UnknownPower PowerSwitching = iota
	// NoPower means the hub reports that VBUS is always on: nothing to switch.
	NoPower
	// GangedPower means one switch feeds every downstream port.
	GangedPower
	// PerPortPower means each port's VBUS is switched independently.
	PerPortPower
)

func (p PowerSwitching) String() string {
	switch p {
	case NoPower:
		return "none"
	case GangedPower:
		return "ganged"
	case PerPortPower:
		return "per_port"
	default:
		return "unknown"
	}
}

// Switchable reports whether one port can be power-cycled without disturbing
// its neighbours.
func (p PowerSwitching) Switchable() bool { return p == PerPortPower }

// USB interface triples that identify an Android device on the wire. adb's own
// host code recognises its interface by exactly this class/subclass/protocol,
// which is why it is usable as evidence here: it is what makes a phone a phone
// rather than a webcam, and it does not depend on the vendor id table.
const (
	vendorSpecificClass = 0xff
	androidSubClass     = 0x42
	adbProtocol         = 0x01
	fastbootProtocol    = 0x03
)

const hubDeviceClass = 0x09

// usbMaxHubPorts is the ceiling the USB hub descriptor itself imposes:
// bNbrPorts is one byte. It is not a policy limit — that is
// HubFilter.MinPorts and schemaMaxPorts — but a sanity bound on a number read
// out of a file. Without it, one corrupt "maxchild" sizes a slice and takes
// the process down with an out-of-memory panic before any filter is consulted.
const usbMaxHubPorts = 255

// Interface is one USB interface descriptor of a device.
type Interface struct {
	Class    uint8
	SubClass uint8
	Protocol uint8
}

// Device is one node of the USB tree as sysfs describes it.
//
// It is an observation, never an identity. Serial is recorded because it is
// useful in a log line, and for no other purpose: duplicate OEM serials are
// real, and resolving a device by serial is what lets a reset land on the
// wrong clone. Slots are addressed by Path.
type Device struct {
	// Path is the canonical USB address: "3-0" for the root hub of bus 3,
	// "3-1" for a device in its first root port, "3-1.4" below a hub. This is
	// the string stored in farm.slots.usb_path and the one ADB reports as
	// "usb:3-1.4".
	Path string

	// DirName is the sysfs directory this device was read from. It differs
	// from Path only for root hubs ("usb3" versus "3-0"), and it matters
	// because per-port directories are named after it.
	DirName string

	Bus   int
	Chain []int // downstream port numbers from the root hub; nil for a root hub

	// Parent is the Path of the device upstream of this one, "" for a root hub.
	Parent string

	VendorID     uint16
	ProductID    uint16
	Manufacturer string
	Product      string
	Serial       string
	DeviceClass  uint8
	Speed        string // as the kernel prints it: "480", "5000", "10000"

	IsRoot   bool
	IsHub    bool
	MaxChild int // downstream port count; meaningful only for a hub

	Power PowerSwitching
	// PowerEvidence records WHY Power says what it says, so an operator who
	// disagrees with a ganged domain can see which file was read.
	PowerEvidence string

	Interfaces []Interface
}

// HasInterface reports whether the device exposes the given triple.
func (d *Device) HasInterface(class, sub, proto uint8) bool {
	return slices.ContainsFunc(d.Interfaces, func(i Interface) bool {
		return i.Class == class && i.SubClass == sub && i.Protocol == proto
	})
}

// IsAndroid reports whether the device is currently exposing an ADB or
// fastboot interface.
//
// This answers "is a phone plugged in HERE, right now", which is evidence for
// adopting a hub into the farm. It is NOT a device identity and it is not a
// health signal: a phone booting, in MTP-only mode, or with USB debugging off
// answers false while remaining physically present in its slot. Nothing in
// this package removes a slot because this went false.
func (d *Device) IsAndroid() bool {
	return d.HasInterface(vendorSpecificClass, androidSubClass, adbProtocol) ||
		d.HasInterface(vendorSpecificClass, androidSubClass, fastbootProtocol)
}

// Source yields the USB devices of one host.
type Source interface {
	// Devices returns every USB device the source can see. The order is
	// unspecified; Scan sorts.
	Devices(ctx context.Context) ([]Device, error)
	// Describe names the source for logs and for Tree.Source.
	Describe() string
}

// FSSource reads a sysfs-shaped tree out of an fs.FS.
type FSSource struct {
	fsys  fs.FS
	label string
}

// Sysfs returns a Source over the real kernel USB tree.
//
// root defaults to [DefaultSysfsRoot]. It fails on anything but Linux instead
// of returning nothing: "I cannot see the bus" and "the bus is empty" must
// never be the same answer, because the second one means every slot on this
// host disappeared.
func Sysfs(root string) (*FSSource, error) {
	if root == "" {
		root = DefaultSysfsRoot
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("topo: USB discovery reads %s and needs Linux, not %s; "+
			"run discovery on the farm host itself, or hand FromFS a fixture in a test",
			root, runtime.GOOS)
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("topo: cannot read the USB tree at %s: %w; "+
			"if this is a container, bind-mount /sys and check that it is not masked", root, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("topo: %s is not a directory; point Sysfs at the bus directory, "+
			"normally %s", root, DefaultSysfsRoot)
	}
	return &FSSource{fsys: os.DirFS(root), label: root}, nil
}

// FromFS returns a Source over an arbitrary fs.FS laid out like
// /sys/bus/usb/devices. This is the injection point for tests: hand it an
// fstest.MapFS and the production parser runs unchanged.
func FromFS(fsys fs.FS, label string) *FSSource {
	if label == "" {
		label = "fs"
	}
	return &FSSource{fsys: fsys, label: label}
}

// Describe implements Source.
func (s *FSSource) Describe() string { return s.label }

// Devices implements Source.
//
// An entry that cannot be parsed is skipped and reported through the returned
// error only if NOTHING could be read; a single malformed device must not cost
// the host its whole topology. Callers learn about partial reads from
// [Tree.Partial], which is what suppresses removal reconciliation.
func (s *FSSource) Devices(ctx context.Context) ([]Device, error) {
	ents, err := fs.ReadDir(s.fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("topo: reading %s: %w", s.label, err)
	}

	var (
		out    []Device
		failed []string
	)
	for _, ent := range ents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := ent.Name()
		bus, chain, root, ok := parseSysfsName(name)
		if !ok {
			// Interfaces ("3-1:1.0"), port objects and anything else the bus
			// directory carries. Not an error.
			continue
		}
		d, err := s.readDevice(name, bus, chain, root)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		out = append(out, d)
	}

	if len(out) == 0 && len(failed) > 0 {
		return nil, fmt.Errorf("topo: no USB device in %s could be read, so this pass says "+
			"nothing about the host; check that /sys is mounted read-write and unmasked "+
			"and that the process may traverse it: %s",
			s.label, strings.Join(failed, "; "))
	}
	if len(failed) > 0 {
		// Reported, not fatal: Scan marks the tree partial and discovery then
		// registers what it saw without retiring anything.
		return out, &PartialReadError{Source: s.label, Entries: failed}
	}
	return out, nil
}

// PartialReadError means some devices were read and some were not. The devices
// that WERE read are returned alongside it and are safe to register; what is
// not safe is concluding that anything is gone.
type PartialReadError struct {
	Source  string
	Entries []string
}

func (e *PartialReadError) Error() string {
	return fmt.Sprintf("topo: %d USB entries under %s could not be read: %s",
		len(e.Entries), e.Source, strings.Join(e.Entries, "; "))
}

func (s *FSSource) readDevice(dir string, bus int, chain []int, root bool) (Device, error) {
	d := Device{
		DirName: dir,
		Path:    canonicalPath(bus, chain, root),
		Bus:     bus,
		Chain:   chain,
		IsRoot:  root,
	}
	if !root {
		d.Parent = parentPath(bus, chain)
	}

	// One listing of the device directory serves both the interface descriptors
	// and the port objects. Its failure is fatal FOR THIS DEVICE on purpose: a
	// device whose directory cannot be listed has been read incompletely, and
	// the caller turns that into Tree.Partial, which suppresses removal
	// reconciliation. The alternative — carrying on with an empty interface
	// list — would make a phone look like it is not a phone, un-adopt its hub,
	// and hand reconciliation an entire hub's worth of "vanished" slots.
	ents, err := fs.ReadDir(s.fsys, dir)
	if err != nil {
		return Device{}, fmt.Errorf("listing %s: %w", dir, err)
	}

	if d.VendorID, err = s.readHex16(dir, "idVendor"); err != nil {
		return Device{}, err
	}
	if d.ProductID, err = s.readHex16(dir, "idProduct"); err != nil {
		return Device{}, err
	}
	if d.DeviceClass, err = s.readHex8(dir, "bDeviceClass"); err != nil {
		return Device{}, err
	}
	d.Manufacturer, _ = s.readText(dir, "manufacturer")
	d.Product, _ = s.readText(dir, "product")
	d.Serial, _ = s.readText(dir, "serial")
	d.Speed, _ = s.readText(dir, "speed")

	d.IsHub = d.DeviceClass == hubDeviceClass
	if d.IsHub {
		// maxchild is the hub's downstream port count and the reason a slot
		// exists for a port nothing is plugged into yet.
		n, err := s.readInt(dir, "maxchild")
		if err != nil {
			return Device{}, err
		}
		d.MaxChild = n
		d.Power, d.PowerEvidence = s.hubPower(d, ents)
	}

	if d.Interfaces, err = s.readInterfaces(d, ents); err != nil {
		return Device{}, err
	}
	return d, nil
}

// readInterfaces reads the interface descriptors of a device. Interface
// directories live inside the device directory and are named after the
// CANONICAL path ("usb3/3-0:1.0"), not after the directory name.
//
// An interface directory that is present but unreadable is an error rather
// than a skip, because the three bytes it holds are the whole of the evidence
// that a device is a phone. Losing them silently downgrades a populated hub to
// "no Android device attached", which is a removal decision made from a failed
// read.
func (s *FSSource) readInterfaces(d Device, ents []fs.DirEntry) ([]Interface, error) {
	prefix := d.Path + ":"
	var out []Interface
	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		sub := path.Join(d.DirName, name)
		class, err := s.readHex8(sub, "bInterfaceClass")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		subc, err := s.readHex8(sub, "bInterfaceSubClass")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		proto, err := s.readHex8(sub, "bInterfaceProtocol")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, Interface{Class: class, SubClass: subc, Protocol: proto})
	}
	return out, nil
}

// hubPower decides whether this hub can switch VBUS per port.
//
// Two sources of evidence, in order:
//
//  1. wHubCharacteristics, when the kernel exposes it. Bits 1..0 are the
//     Logical Power Switching Mode of the USB hub descriptor: 00 ganged,
//     01 individual, 1x no switching.
//
//  2. The per-port "disable" control. The USB core only publishes that
//     attribute for ports whose power it can actually cut, so its presence on
//     every port is the practical answer to "can uhubctl do anything here",
//     and its absence is the practical answer to "no".
//
// Anything ambiguous — some ports controllable and some not, no attribute at
// all — resolves to unknown, which callers treat as not switchable. Erring
// toward ganged costs a wider blast radius on a recovery attempt and a
// refusal. Erring the other way costs somebody's six-hour run.
func (s *FSSource) hubPower(d Device, ents []fs.DirEntry) (PowerSwitching, string) {
	if raw, ok := s.readText(d.DirName, "wHubCharacteristics"); ok {
		if v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(raw), "0x"), 16, 16); err == nil {
			ev := "wHubCharacteristics=" + raw
			switch v & 0x03 {
			case 0x00:
				return GangedPower, ev
			case 0x01:
				return PerPortPower, ev
			default:
				return NoPower, ev
			}
		}
	}

	ports, controllable, unreadable := s.countPortControls(d, ents)
	switch {
	case unreadable > 0 && controllable < ports:
		// Some interface directory would not list. Whatever it holds cannot be
		// counted as evidence of per-port control, and half an answer here is
		// how a ganged hub gets recorded as switchable.
		return UnknownPower, fmt.Sprintf(
			"%d interface directories under %s could not be listed", unreadable, d.DirName)
	case ports == 0:
		return UnknownPower, "no port directories under " + d.DirName
	case controllable == ports:
		return PerPortPower, fmt.Sprintf("port power control present on all %d ports", ports)
	case controllable == 0:
		return NoPower, fmt.Sprintf("no port power control on any of %d ports", ports)
	default:
		return UnknownPower, fmt.Sprintf("port power control on %d of %d ports", controllable, ports)
	}
}

// countPortControls walks the hub's port objects, which the kernel hangs off
// the hub's interface directory and names "<sysfs dir>-port<N>".
//
// unreadable counts interface directories that would not list. It is returned
// rather than swallowed so that hubPower can say "I do not know" instead of
// "no port has power control", which are the same number here and very
// different claims about the hardware.
func (s *FSSource) countPortControls(d Device, ents []fs.DirEntry) (ports, controllable, unreadable int) {
	prefix := d.Path + ":"
	portMark := d.DirName + "-port"
	for _, ent := range ents {
		if !strings.HasPrefix(ent.Name(), prefix) {
			continue
		}
		iface := path.Join(d.DirName, ent.Name())
		portEnts, err := fs.ReadDir(s.fsys, iface)
		if err != nil {
			unreadable++
			continue
		}
		for _, pe := range portEnts {
			if !strings.HasPrefix(pe.Name(), portMark) {
				continue
			}
			if _, err := strconv.Atoi(strings.TrimPrefix(pe.Name(), portMark)); err != nil {
				continue
			}
			ports++
			if s.writableAttr(path.Join(iface, pe.Name()), "disable") {
				controllable++
			}
		}
	}
	return ports, controllable, unreadable
}

// writableAttr reports whether an attribute exists and is writable. A
// read-only "disable" is a status readout, not a control, and cannot cycle
// anything.
func (s *FSSource) writableAttr(dir, name string) bool {
	info, err := fs.Stat(s.fsys, path.Join(dir, name))
	if err != nil {
		return false
	}
	return info.Mode().Perm()&0o200 != 0
}

func (s *FSSource) readText(dir, name string) (string, bool) {
	b, err := fs.ReadFile(s.fsys, path.Join(dir, name))
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", false
	}
	return v, true
}

func (s *FSSource) readHex16(dir, name string) (uint16, error) {
	raw, ok := s.readText(dir, name)
	if !ok {
		return 0, fmt.Errorf("missing %s", name)
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(raw), "0x"), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", name, raw, err)
	}
	return uint16(v), nil
}

func (s *FSSource) readHex8(dir, name string) (uint8, error) {
	raw, ok := s.readText(dir, name)
	if !ok {
		return 0, fmt.Errorf("missing %s", name)
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(raw), "0x"), 16, 8)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", name, raw, err)
	}
	return uint8(v), nil
}

func (s *FSSource) readInt(dir, name string) (int, error) {
	raw, ok := s.readText(dir, name)
	if !ok {
		return 0, fmt.Errorf("missing %s", name)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", name, raw, err)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// USB path arithmetic
// ---------------------------------------------------------------------------

// parseSysfsName splits a /sys/bus/usb/devices entry name.
//
// "usb3"    -> bus 3, root hub
// "3-1"     -> bus 3, chain [1]
// "3-1.4.2" -> bus 3, chain [1 4 2]
// anything else (interfaces "3-1:1.0", port objects, stray files) -> !ok
func parseSysfsName(name string) (bus int, chain []int, root bool, ok bool) {
	if strings.ContainsAny(name, ":/") {
		return 0, nil, false, false
	}
	if rest, cut := strings.CutPrefix(name, "usb"); cut {
		b, err := strconv.Atoi(rest)
		if err != nil || b < 0 {
			return 0, nil, false, false
		}
		return b, nil, true, true
	}
	busStr, portStr, cut := strings.Cut(name, "-")
	if !cut {
		return 0, nil, false, false
	}
	b, err := strconv.Atoi(busStr)
	if err != nil || b < 0 {
		return 0, nil, false, false
	}
	parts := strings.Split(portStr, ".")
	chain = make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, nil, false, false
		}
		chain = append(chain, n)
	}
	return b, chain, false, true
}

// canonicalPath renders the address the rest of the system uses. A root hub
// addresses itself as bus-0 even though its directory is named usbN.
func canonicalPath(bus int, chain []int, root bool) string {
	if root || len(chain) == 0 {
		return strconv.Itoa(bus) + "-0"
	}
	var b strings.Builder
	b.WriteString(strconv.Itoa(bus))
	for i, p := range chain {
		if i == 0 {
			b.WriteByte('-')
		} else {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(p))
	}
	return b.String()
}

// parentPath is the address of whatever this device is plugged into.
func parentPath(bus int, chain []int) string {
	if len(chain) <= 1 {
		return canonicalPath(bus, nil, true)
	}
	return canonicalPath(bus, chain[:len(chain)-1], false)
}

// portPath is the address a device would have if plugged into the given port
// of the given hub. On a root hub that is "3-1", not "3-0.1"; everywhere else
// it is the hub's own path plus a dotted port number.
func portPath(hub *Hub, port int) string {
	if hub.IsRoot {
		return strconv.Itoa(hub.Bus) + "-" + strconv.Itoa(port)
	}
	return hub.Path + "." + strconv.Itoa(port)
}

// compareDevices orders the tree deterministically: by bus, then down the
// port chain, shallow before deep. Two scans of unchanged hardware therefore
// produce byte-identical work, which is what makes discovery a no-op the
// second time it runs.
func compareDevices(a, b Device) int {
	if a.Bus != b.Bus {
		return a.Bus - b.Bus
	}
	return slices.Compare(a.Chain, b.Chain)
}

// ---------------------------------------------------------------------------
// The tree
// ---------------------------------------------------------------------------

// Hub is a hub in the built tree, with one entry per downstream port.
//
// The upstream link is named Upstream rather than Parent because the embedded
// Device already carries Parent as a USB path string, and one of the two would
// have silently shadowed the other.
type Hub struct {
	Device
	Controller *Controller
	Upstream   *Hub
	Ports      []Port
}

// Port is one downstream position of a hub: a slot, in farm.slots terms,
// whether or not anything is plugged into it.
type Port struct {
	Number int
	// Path is the USB address a device in this port has.
	Path string
	// Attached is the device currently enumerated here, nil when the port is
	// empty. A slot outlives whatever is in it.
	Attached *Device
	// Downstream is the hub in this port, when the attached device is a hub.
	Downstream *Hub
}

// Controller is a root bus: one xHCI/EHCI controller as far as we can tell
// from sysfs.
type Controller struct {
	Bus     int
	RootHub *Hub
}

// Tree is the USB topology of one host.
type Tree struct {
	// Source describes where the tree was read from.
	Source string
	// Controllers are ordered by bus number.
	Controllers []*Controller
	// Partial is true when the scan is known to be incomplete: an entry failed
	// to parse, or a device referenced a parent that was not present. A partial
	// tree may be registered — everything in it was really observed — but it
	// may NOT be used to conclude that a slot has disappeared.
	Partial bool
	// Problems explains Partial in operator-readable terms.
	Problems []string

	hubs    map[string]*Hub
	devices map[string]*Device
}

// Hubs returns every hub in the tree, ordered by USB path.
func (t *Tree) Hubs() []*Hub {
	out := make([]*Hub, 0, len(t.hubs))
	for _, h := range t.hubs {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b *Hub) int { return compareDevices(a.Device, b.Device) })
	return out
}

// Hub returns the hub at a USB path, if any.
func (t *Tree) Hub(usbPath string) *Hub { return t.hubs[usbPath] }

// Device returns the device at a USB path, if any.
func (t *Tree) Device(usbPath string) *Device { return t.devices[usbPath] }

// Scan reads a source and builds the tree.
//
// A [PartialReadError] from the source is not returned as an error: the
// devices that were read are real and are kept. It becomes Tree.Partial, which
// discovery honours by registering what it saw and retiring nothing.
func Scan(ctx context.Context, src Source) (*Tree, error) {
	devs, err := src.Devices(ctx)
	var partial *PartialReadError
	switch {
	case errors.As(err, &partial):
		// keep devs
	case err != nil:
		return nil, err
	}

	t, err := Build(devs)
	if err != nil {
		return nil, err
	}
	t.Source = src.Describe()
	if partial != nil {
		t.Partial = true
		t.Problems = append(t.Problems, partial.Entries...)
	}
	return t, nil
}

// Build assembles a tree from a flat device list.
func Build(devs []Device) (*Tree, error) {
	t := &Tree{
		hubs:    make(map[string]*Hub),
		devices: make(map[string]*Device),
	}

	sorted := slices.Clone(devs)
	slices.SortFunc(sorted, compareDevices)

	// Two passes so a hub is addressable before its children are linked, and
	// so duplicate paths are caught rather than silently overwriting.
	for i := range sorted {
		d := &sorted[i]
		if _, dup := t.devices[d.Path]; dup {
			// Fatal, and deliberately so: a USB path is the join key for slots,
			// devpaths and recovery actions, and a tree in which one key names
			// two devices cannot be registered without pointing a slot at the
			// wrong socket. Nothing is written and nothing is retired.
			return nil, fmt.Errorf("topo: two USB devices claim the path %q; the source is "+
				"not a real sysfs tree — check what Source.Describe() names", d.Path)
		}
		t.devices[d.Path] = d
		if d.IsHub {
			t.hubs[d.Path] = &Hub{Device: *d}
		}
	}

	// Everything below walks the SORTED slice rather than the maps, so that a
	// second scan of unchanged hardware produces identical output down to the
	// order of Problems.
	byBus := make(map[int]*Controller)
	for i := range sorted {
		h, isHub := t.hubs[sorted[i].Path]
		if !isHub {
			continue
		}
		if h.IsRoot {
			byBus[h.Bus] = &Controller{Bus: h.Bus, RootHub: h}
		}
		if h.MaxChild < 0 || h.MaxChild > usbMaxHubPorts {
			// Not fatal for the pass, and not clamped either: a hub whose port
			// count is impossible gets no ports, so nothing is registered for
			// it, and the tree is marked incomplete so that its existing slots
			// are not read as gone.
			t.Partial = true
			t.Problems = append(t.Problems, fmt.Sprintf(
				"hub %s reports %d ports, which no USB hub descriptor can express (0..%d)",
				h.Path, h.MaxChild, usbMaxHubPorts))
			continue
		}
		h.Ports = make([]Port, 0, h.MaxChild)
		for p := 1; p <= h.MaxChild; p++ {
			h.Ports = append(h.Ports, Port{Number: p, Path: portPath(h, p)})
		}
	}

	for i := range sorted {
		h, isHub := t.hubs[sorted[i].Path]
		if !isHub || h.IsRoot {
			continue
		}
		// A hub whose upstream hub is missing is reported by the attach loop
		// below, which sees every device rather than only the hubs.
		if up, ok := t.hubs[h.Device.Parent]; ok {
			h.Upstream = up
		}
	}

	// Attach devices to the ports they occupy.
	for i := range sorted {
		d := &sorted[i]
		if d.IsRoot {
			continue
		}
		if len(d.Chain) == 0 {
			// Unreachable from [FSSource], which never produces a non-root
			// device without a port chain, but Build and Source are exported:
			// a caller's own Source must get a problem report, not a panic on
			// the index below.
			t.Partial = true
			t.Problems = append(t.Problems, fmt.Sprintf(
				"device %s is not a root hub but names no port; its Source built it wrong", d.Path))
			continue
		}
		up, ok := t.hubs[d.Parent]
		if !ok {
			// Every USB device except a root hub hangs off a hub, so a missing
			// parent means the listing and the devices under it disagree — the
			// hub was unplugged mid-scan, or /sys was read through a mount that
			// is coming and going. Either way the ports of that hub are absent
			// from this pass for a reason that has nothing to do with the
			// hardware, and absence must not be read as removal.
			t.Partial = true
			t.Problems = append(t.Problems, fmt.Sprintf(
				"device %s hangs off %s, which this scan did not report", d.Path, d.Parent))
			continue
		}
		port := d.Chain[len(d.Chain)-1]
		idx := port - 1
		if idx < 0 || idx >= len(up.Ports) {
			// A device in a port the hub says it does not have. Trust the
			// device: something is enumerated there. The scan is inconsistent,
			// which is enough to forbid concluding anything has vanished.
			t.Partial = true
			t.Problems = append(t.Problems,
				fmt.Sprintf("device %s sits in port %d of %s, which reports %d ports",
					d.Path, port, up.Path, up.MaxChild))
			continue
		}
		up.Ports[idx].Attached = d
		if h, isHub := t.hubs[d.Path]; isHub {
			up.Ports[idx].Downstream = h
		}
	}

	for _, h := range t.hubs {
		h.Controller = byBus[h.Bus]
	}

	t.Controllers = make([]*Controller, 0, len(byBus))
	for _, c := range byBus {
		t.Controllers = append(t.Controllers, c)
	}
	slices.SortFunc(t.Controllers, func(a, b *Controller) int { return a.Bus - b.Bus })

	return t, nil
}

// Occupied reports how many of the hub's ports currently hold a device.
func (h *Hub) Occupied() int {
	n := 0
	for _, p := range h.Ports {
		if p.Attached != nil {
			n++
		}
	}
	return n
}

// AndroidPorts reports how many of the hub's ports currently hold a device
// exposing an ADB or fastboot interface.
func (h *Hub) AndroidPorts() int {
	n := 0
	for _, p := range h.Ports {
		if p.Attached != nil && p.Attached.IsAndroid() {
			n++
		}
	}
	return n
}

// ForeignPorts reports how many ports hold something that is neither a hub nor
// an Android device — a keyboard, a NIC, a dongle. A hub full of those is
// somebody's desk, not a rack position.
func (h *Hub) ForeignPorts() int {
	n := 0
	for _, p := range h.Ports {
		if p.Attached == nil || p.Downstream != nil {
			continue
		}
		if !p.Attached.IsAndroid() {
			n++
		}
	}
	return n
}

// Model is the best human name for the hub.
func (h *Hub) Model() string {
	switch {
	case h.Product != "" && h.Manufacturer != "":
		return h.Manufacturer + " " + h.Product
	case h.Product != "":
		return h.Product
	case h.Manufacturer != "":
		return h.Manufacturer
	default:
		return fmt.Sprintf("%04x:%04x", h.VendorID, h.ProductID)
	}
}
