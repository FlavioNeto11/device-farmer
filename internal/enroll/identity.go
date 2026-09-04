package enroll

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// Shell is the slice of the ADB host protocol that identity gathering needs:
// run one command against one PHYSICAL POSITION and read what it printed.
// *adbwire.Client satisfies it.
//
// It is an interface so identity gathering can be exercised without an ADB
// server, not so a different addressing scheme can be substituted. Every
// method here takes a devpath. There is no serial-addressed variant and there
// must never be one: two phones can report the same OEM serial, and a probe
// that resolved its target by serial could read one clone's properties and
// write them onto the other clone's row.
type Shell interface {
	Shell(ctx context.Context, devpath, command string) (*adbwire.ShellResult, error)
}

// Property keys read on every sighting.
const (
	propManufacturer = "ro.product.manufacturer"
	propModel        = "ro.product.model"
	propName         = "ro.product.name"
	propDevice       = "ro.product.device"
	propRelease      = "ro.build.version.release"
	propSDK          = "ro.build.version.sdk"
	propABIList      = "ro.product.cpu.abilist"
	propBuildFP      = "ro.build.fingerprint"

	// Read for the hardware fingerprint. See [HardwareFingerprint] for why
	// these and not the build properties above.
	propHardware   = "ro.hardware"
	propBootSerial = "ro.boot.serialno"
	propSerial     = "ro.serialno"
)

// uidKey is the pseudo-property the probe emits the branded uid under. It is
// not a device property; the probe prints it itself, and no real key collides
// with it because every real key read here begins with "ro.".
const uidKey = "farm_uid"

// What a probe is allowed to say about itself.
//
// Everything a device prints ends up in a text column and inside a jsonb
// document, and PostgreSQL refuses both a NUL byte and any byte sequence that
// is not valid UTF-8 — 22021 on text and text[], 22P05 on jsonb. Verified
// against the live server: a single 0x00 or 0xff anywhere in the output makes
// the whole INSERT fail, which would lose the SIGHTING, not just the odd
// property. Since it would fail the same way on every cycle, that device would
// be permanently unenrollable and the record would never say why. So every
// device-supplied string is put through [truncate], which maps control bytes
// out and replaces invalid sequences, and is bounded on the way in.
const (
	// maxPropKeyLen and maxPropValue bound one property. Real keys are around
	// thirty characters and real values well under a hundred.
	maxPropKeyLen = 128
	maxPropValue  = 256

	// maxPropKeys bounds how many distinct keys a probe may report. It asks
	// for eleven and prints one of its own; a device answering with more than
	// this is not answering the question that was asked.
	maxPropKeys = 64
)

// probeProps is every property read on a sighting, in the order the device is
// asked for them.
var probeProps = []string{
	propManufacturer, propModel, propName, propDevice,
	propRelease, propSDK, propABIList, propBuildFP,
	propHardware, propBootSerial, propSerial,
}

// propKeyRe is what a property key may look like. The list above is a package
// constant and nothing observed from a device can extend it, so this is a
// guard against a future edit rather than against a phone — which is exactly
// when a shell-quoting mistake would otherwise ship unnoticed.
var propKeyRe = regexp.MustCompile(`^[a-z][a-z0-9._]*$`)

// probeCommand reads the brand and then the properties in ONE round trip.
//
// One call rather than twelve is not micro-optimisation: a host probes every
// attached device on every cycle, each ADB call is a fresh connect and
// transport negotiation, and some of those devices are mid-job. The brand is
// read first, and printed first, because it is the strongest evidence we will
// have — we wrote it.
//
// A missing brand file is not an error. An unbranded device is the normal
// state of a phone somebody just plugged in, so the read is silenced and its
// absence shows up as an empty value.
var probeCommand = buildProbeCommand(probeProps)

func buildProbeCommand(keys []string) string {
	var b strings.Builder
	b.WriteString(`printf '%s=%s\n' `)
	b.WriteString(uidKey)
	b.WriteString(` "$(cat `)
	b.WriteString(BrandPath)
	b.WriteString(` 2>/dev/null)"; for k in`)
	for _, k := range keys {
		if !propKeyRe.MatchString(k) {
			// A key that needs quoting would silently change what the device
			// is asked for. There is no runtime input here, so this can only
			// be a bad edit, and failing at package initialisation is how it
			// gets found.
			panic("enroll: property key is not shell-safe: " + strconv.Quote(k))
		}
		b.WriteString(" ")
		b.WriteString(k)
	}
	b.WriteString(`; do printf '%s=%s\n' "$k" "$(getprop "$k")"; done`)
	return b.String()
}

// Identity is one complete sighting of one physical position: what the ADB
// server said about it, what the device said about itself, and — when the
// device could not be read at all — why not.
//
// Every field is an OBSERVATION. Nothing here identifies a device on its own;
// identification happens in farm.resolve_device against what the fleet already
// knows.
type Identity struct {
	// Devpath is the position on the host's USB tree, "usb:3-1.4".
	Devpath string
	// USBPath is the same position in the schema's spelling, "3-1.4".
	USBPath string

	// Serial is what the ADB server reported for this transport. Evidence,
	// never an address, and deliberately not unique.
	Serial string
	// State and RawState are the transport's connection state, normalised
	// and as the server worded it.
	State    adbwire.ConnState
	RawState string
	// TransportID is the server's small integer handle. Meaningless without
	// the host epoch it was minted in, which is why the observation row
	// carries both or neither.
	TransportID int64

	// FarmUID is the brand read off the device, empty when it carries none.
	FarmUID string
	// MalformedUID holds a brand file whose content is not a farm uid. It is
	// recorded and never resolved on: something else wrote that file, and
	// treating it as identity would be trusting a stranger.
	MalformedUID string

	// HWFingerprint is the digest of the properties a factory reset does not
	// change, and FingerprintKeys names what went into it.
	HWFingerprint   []byte
	FingerprintKeys []string

	Manufacturer     string
	Model            string
	Product          string
	Codename         string
	AndroidRelease   string
	SDKInt           int
	ABIs             []string
	BuildFingerprint string

	// Props is everything the probe read, verbatim.
	Props map[string]string

	// Unreadable is empty when the device answered. Otherwise it is why it
	// could not be read: a connection state ("unauthorized", "offline"), a
	// "probe_timeout", "probe_truncated" or "probe_noisy" for an answer that
	// was not the one asked for, or "identity_incomplete" for a device that
	// answered with nothing that could tell it apart from another unit.
	Unreadable string
	// Err is the transport error behind an Unreadable probe, for logging.
	// It NEVER travels further than a log line and a counter: a device that
	// could not be read is a device we know nothing new about, and knowing
	// nothing about a device is not a reason to do anything to it.
	Err error

	// ProbeDuration is how long the device took to answer. A duration, not a
	// timestamp: no clock reading from this process is ever written to the
	// database.
	ProbeDuration time.Duration
}

// Readable reports whether the device answered the probe.
func (id Identity) Readable() bool { return id.Unreadable == "" }

// Resolvable reports whether this sighting carries enough evidence to be
// handed to farm.resolve_device, and when it does not, why.
//
// The bar exists because resolution ends in adoption: a sighting that matches
// nothing becomes a new row in farm.devices. Adopting on empty evidence
// creates a device that cannot be recognised next time — so it gets adopted
// again, and the fleet fills with phantoms. A device carrying our own brand is
// always resolvable, whatever else it did or did not say, because the brand
// alone answers the only question resolution asks.
func (id Identity) Resolvable() (bool, string) {
	if !id.Readable() {
		return false, id.Unreadable
	}
	if id.FarmUID != "" {
		return true, ""
	}
	if id.Serial == "" && id.Props[propBootSerial] == "" && id.Props[propSerial] == "" {
		return false, "identity_incomplete"
	}
	if id.Manufacturer == "" && id.Model == "" && id.Codename == "" {
		return false, "identity_incomplete"
	}
	return true, ""
}

// resolveProps returns the property bag farm.resolve_device stores on the row.
// The keys are the ones that function reads; sdk_int is a string because the
// function casts it, and it is omitted entirely unless it parsed into the
// range farm.devices accepts.
func (id Identity) resolveProps() map[string]any {
	p := map[string]any{}
	set := func(k, v string) {
		if v != "" {
			p[k] = v
		}
	}
	set("manufacturer", id.Manufacturer)
	set("model", id.Model)
	set("product", id.Product)
	set("device_codename", id.Codename)
	set("android_release", id.AndroidRelease)
	set("build_fingerprint", id.BuildFingerprint)
	if id.SDKInt > 0 {
		p["sdk_int"] = strconv.Itoa(id.SDKInt)
	}
	return p
}

// USBPath converts an ADB devpath into the schema's usb_path spelling.
//
// farm.slots.usb_path holds "3-1.4" and farm.slots.adb_devpath is the
// generated "usb:3-1.4". The ADB server speaks the second form; every lookup
// against topology speaks the first.
func USBPath(devpath string) string { return strings.TrimPrefix(devpath, "usb:") }

// Probe gathers identity from the device at dev.Devpath.
//
// It never returns an error. A device that cannot be read is an ordinary,
// expected outcome — a phone with the ADB dialog still on screen is the most
// common thing in a fresh rack — and it is recorded as a sighting with a
// reason rather than thrown away. The transport error, when there was one, is
// carried in Identity.Err for the log.
func Probe(ctx context.Context, sh Shell, dev adbwire.Device, timeout time.Duration) Identity {
	// Everything here except the devpath came off the far end of a USB cable:
	// the serial and the tail are USB descriptors the DEVICE supplies, and the
	// raw state is free text. All of them are stored, so all of them are
	// bounded and made storable first — see the note on maxPropValue. The
	// devpath is exempt because adbwire only ever records one that matches its
	// own devpath pattern.
	id := Identity{
		Devpath:     dev.Devpath,
		USBPath:     USBPath(dev.Devpath),
		Serial:      truncate(dev.Serial, maxPropValue),
		State:       dev.State,
		RawState:    truncate(dev.RawState, maxPropValue),
		TransportID: dev.TransportID,
		// The listing's own tail, kept as a fallback for a device whose
		// getprop answers are empty. The server substitutes underscores for
		// spaces in the model, so these are worse than the real properties
		// and are only used when the real ones are missing.
		Product:  truncate(dev.Product, maxPropValue),
		Model:    truncate(dev.Model, maxPropValue),
		Codename: truncate(dev.Codename, maxPropValue),
		Props:    map[string]string{},
	}

	// An unauthorized or offline transport is a real sighting of a real
	// phone, and the ADB server will refuse a service request for it. Do not
	// spend a connection finding that out.
	if !dev.State.Usable() {
		id.Unreadable = string(dev.State)
		return id
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	res, err := sh.Shell(cctx, dev.Devpath, probeCommand)
	id.ProbeDuration = time.Since(start)

	if err != nil {
		id.Err = err
		id.Unreadable = unreadableReason(ctx, err)
		// Whatever partial output arrived is deliberately dropped. Half a
		// property list is indistinguishable from a different device's
		// property list, and adoption on it would mint a phantom.
		return id
	}
	if res == nil {
		// Unreachable through *adbwire.Client, which never returns a nil
		// result with a nil error. Probes run in their own goroutines, so a
		// Shell implementation that did would take the whole process down
		// with a nil dereference instead of costing one sighting.
		id.Err = errors.New("enroll: the shell returned neither a result nor an error")
		id.Unreadable = "probe_failed"
		return id
	}

	props, complete := parseProps(res.Stdout)
	id.Props = props

	if uid := props[uidKey]; uid != "" {
		if uidRe.MatchString(uid) {
			id.FarmUID = uid
		} else {
			id.MalformedUID = uid
		}
	}

	id.Manufacturer = props[propManufacturer]
	id.Product = firstNonEmpty(props[propName], id.Product)
	id.Model = firstNonEmpty(props[propModel], id.Model)
	id.Codename = firstNonEmpty(props[propDevice], id.Codename)
	id.AndroidRelease = props[propRelease]
	id.BuildFingerprint = props[propBuildFP]
	id.ABIs = splitABIs(props[propABIList])

	// farm.devices constrains sdk_int to 1..100. A device that answered with
	// something outside that range answered with nonsense, and nonsense is
	// dropped here rather than turned into a check violation three statements
	// later, where it would take the whole observation down with it.
	if n, convErr := strconv.Atoi(props[propSDK]); convErr == nil && n >= 1 && n <= 100 {
		id.SDKInt = n
	}

	// An answer that had to be cut short, or that carried far more keys than
	// the probe asked for, is not the answer to this question. The missing or
	// extra part is indistinguishable from another device's answer, so no
	// fingerprint is computed from it and nothing is concluded: the sighting
	// is recorded with everything that did arrive, and resolution is skipped
	// exactly as it is for a device that would not talk at all.
	switch {
	case res.Truncated:
		id.Unreadable = "probe_truncated"
		return id
	case !complete:
		id.Unreadable = "probe_noisy"
		return id
	}

	// The sanitised serial, not dev.Serial: the digest must be computed over
	// the same bytes that were stored, or a device would fingerprint one way
	// here and another way from its own observation row.
	id.HWFingerprint, id.FingerprintKeys = HardwareFingerprint(props, id.Serial)
	return id
}

// unreadableReason classifies a failed probe for the observation record.
//
// The distinction that matters is between "this device did not answer" and
// "we stopped asking": a cancelled probe is a fact about this process
// shutting down, and recording it as a device problem would put a shutdown
// into the record as evidence against a phone.
func unreadableReason(parent context.Context, err error) string {
	if parent.Err() != nil {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "probe_timeout"
	}
	if te, ok := adbwire.AsTransport(err); ok && te.Timeout() {
		return "probe_timeout"
	}
	if adbwire.IsNotFound(err) {
		// The transport went away between the listing and the probe. Common,
		// and about the wire, not the phone.
		return "detached"
	}
	return "probe_failed"
}

// parseProps reads the probe's key=value lines, and reports whether the device
// stayed within what a probe may answer.
//
// A key it does not recognise is kept, a line without '=' is skipped, and a
// property whose value contains a newline loses its tail — such a value cannot
// be represented in this format and is not worth a second round trip, since
// none of the keys read here has ever carried one.
//
// Keys and values are bounded and made storable here rather than at the far
// end, because this is the one place every device-supplied property passes
// through: what leaves this function is what goes into farm.devices, into the
// observation row's text columns, and into its jsonb detail. The whole output
// is scanned in place rather than split, so a device that answers a
// twelve-line question with megabytes costs one pass and no per-line
// allocation.
//
// A false second return means the device produced more distinct keys than the
// probe could have asked for. The properties gathered so far are still
// returned — they are evidence and belong in the record — but the caller must
// not draw an identity from them.
func parseProps(out []byte) (map[string]string, bool) {
	props := make(map[string]string, len(probeProps)+1)
	rest := string(out)
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		line = strings.TrimRight(line, "\r")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = truncate(strings.TrimSpace(k), maxPropKeyLen)
		if k == "" {
			continue
		}
		if _, seen := props[k]; !seen && len(props) >= maxPropKeys {
			return props, false
		}
		props[k] = truncate(strings.TrimSpace(v), maxPropValue)
	}
	return props, true
}

func splitABIs(list string) []string {
	if list = strings.TrimSpace(list); list == "" {
		return nil
	}
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Hardware fingerprint
// ---------------------------------------------------------------------------

// fingerprintVersion is mixed into every digest. The set of properties below
// is a judgement call and will eventually be revised; when it is, every device
// in the fleet gets a new fingerprint. Versioning the input means the new
// digests are visibly different from the old ones instead of quietly
// colliding with them, and a fleet that suddenly stops matching on
// hw_fingerprint is a fleet whose devices are still found by their brand.
const fingerprintVersion = "device-farmer/hwfp/1"

// fingerprintMinComponents is how many of the five inputs must be present for
// a digest to be worth computing. Below this the digest stops distinguishing
// units and starts grouping them, and a fingerprint shared by forty phones is
// worse than none: farm.resolve_device answers 'ambiguous' for every one of
// them and the whole rack becomes unresolvable.
const fingerprintMinComponents = 3

// HardwareFingerprint hashes the properties that survive a factory reset, and
// returns the digest together with the names of the components that went into
// it.
//
// # Why these properties
//
// The fingerprint's whole job is to recognise a phone that came back wiped.
// A factory reset erases /data — which is where our brand lives, where
// Settings.Secure.android_id lives, and where anything an app could have
// written lives. Everything used here is read-only partition or bootloader
// data that a wipe does not touch:
//
//	manufacturer  ro.product.manufacturer  who made the unit
//	model         ro.product.model         what it is
//	device        ro.product.device        board codename
//	hardware      ro.hardware              SoC/platform from the kernel
//	serial        ro.boot.serialno, else ro.serialno, else the ADB serial —
//	              burned in at manufacture and the only component that
//	              distinguishes two identical units of the same model
//
// # Why NOT the build properties
//
// ro.build.fingerprint and ro.build.version.* also survive a factory reset,
// and they are deliberately excluded, because they do not survive an OTA. A
// fleet-wide update would change the fingerprint of every device on the same
// night — the identity ladder would fall through to serial-and-slot for the
// entire rack at once, and any phone that had also been re-cabled would be
// adopted as new. A wipe loses one device's brand; an OTA that reset every
// fingerprint would lose the fleet's.
//
// # Why the serial is required
//
// Without a serial, every unit of one model in the rack hashes to the same
// value, and farm.resolve_device reports 'ambiguous' rather than matching.
// The serial is not trusted as an identifier here — clone serials are exactly
// why this system addresses by devpath — it is used as one component of a
// digest, where two clones colliding is the honest answer.
//
// Returns nil when the evidence is too thin. A nil fingerprint is not an
// error: resolution simply moves down the ladder to serial-and-slot, and then
// to adoption, after which the device gets branded and never needs the
// fingerprint again.
func HardwareFingerprint(props map[string]string, adbSerial string) ([]byte, []string) {
	// Fixed order, so the digest does not depend on map iteration.
	components := []struct{ name, value string }{
		{"manufacturer", props[propManufacturer]},
		{"model", props[propModel]},
		{"device", props[propDevice]},
		{"hardware", props[propHardware]},
		{"serial", firstNonEmpty(props[propBootSerial], props[propSerial], adbSerial)},
	}

	var serial string
	present := 0
	for _, c := range components {
		if c.value != "" {
			present++
			if c.name == "serial" {
				serial = c.value
			}
		}
	}
	if serial == "" || present < fingerprintMinComponents {
		return nil, nil
	}

	h := sha256.New()
	// Length-prefixed, so that ("ab","c") and ("a","bc") cannot hash alike —
	// a collision between two real devices is not a theoretical concern here,
	// it is a wrong device being handed to a job.
	writeField(h, fingerprintVersion)
	used := make([]string, 0, len(components))
	for _, c := range components {
		if c.value == "" {
			continue
		}
		writeField(h, c.name)
		writeField(h, c.value)
		used = append(used, c.name)
	}
	return h.Sum(nil), used
}

func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	// hash.Hash never returns an error, which is why this ignores one.
	fmt.Fprintf(h, "%d:%s", len(s), s)
}
