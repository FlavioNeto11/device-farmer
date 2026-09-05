package topo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// The human-facing label
// ---------------------------------------------------------------------------
//
// A rack_slot is what an operator reads at 03:00 when a page says a device is
// unreachable. "R2-U14-H3-P5" sends them to a rack, a shelf, a hub and a
// socket. "30501029-78dd-4fd9" sends them to a spreadsheet, and then to the
// wrong device.
//
// The label is built from three things and nothing else:
//
//	R<rack>   farm.hosts.rack_id      where the box lives
//	U<unit>   farm.hosts.rack_unit    which shelf
//	H<token>  a STABLE hub ordinal    which hub
//	P<port>   the USB port number     which socket
//
// # Why the hub ordinal comes from the USB path
//
// The obvious ordinal — "this is the third hub I found" — is wrong, and wrong
// in a way that is invisible until it hurts:
//
//   - sysfs enumeration order is not a property of the hardware. It reflects
//     which hub answered first during boot, module load order, and whether a
//     device was already plugged in when the kernel came up. Reboot the host
//     and the third hub can become the first.
//
//   - A sorted index is stable only while the SET of hubs is unchanged.
//     Racking one more hub in an earlier root port renumbers every hub after
//     it.
//
// Either mistake is silently destructive here, because farm.register_slot
// overwrites farm.slots.rack_slot with whatever discovery passes. A shifted
// ordinal does not produce one bad label; it rewrites every label on the host
// in a single pass, while an operator is walking toward a rack holding a
// printout of the old ones.
//
// So the ordinal is a pure function of the USB path: bus number plus the chain
// of physical sockets the cable actually runs through. That string changes
// when, and only when, a human moves a cable — which is exactly when the label
// SHOULD change. "3-1.4" becomes "3.1.4", giving R2-U14-H3.1.4-P5.
//
// # Why there is an override map
//
// The one part of the USB path that is not purely physical is the bus number,
// which the kernel assigns in controller registration order. It is stable for
// fixed hardware, but a kernel upgrade or a change in module load order can
// move it. And operators label hubs with tape, in which case the tape is the
// authority, not sysfs.
//
// [Overrides] therefore lets an operator pin the hub token ("3-1.4" -> "3",
// giving the classic R2-U14-H3-P5) or pin a whole slot label outright. The
// mapping is keyed by USB path, so it survives everything except somebody
// moving the cable.

// Overrides is the operator-supplied naming map.
//
// Both maps are keyed by USB path — the same string as farm.hubs.usb_path and
// farm.slots.usb_path — because that key is a physical position rather than a
// database id, and it can be written down before the row exists.
type Overrides struct {
	// HubTokens maps a hub's USB path to the token that appears after "H".
	// Example: {"3-1.4": "3"} renders R2-U14-H3-P5.
	HubTokens map[string]string `json:"hub_tokens"`

	// SlotLabels maps a slot's USB path to a complete label, bypassing the
	// scheme entirely. For the rack where somebody already stuck numbers on
	// every socket.
	SlotLabels map[string]string `json:"slot_labels"`
}

// LoadOverrides reads an Overrides map from a JSON file:
//
//	{"hub_tokens": {"3-1.4": "3"}, "slot_labels": {"3-1.4.2": "R2-U14-H3-P2"}}
//
// An empty path is no overrides. Unknown keys are refused rather than
// skipped, because a file that spells "hubTokens" would otherwise load as an
// empty map and every label on the host would quietly fall back to the derived
// scheme.
//
// This reads the file and judges its shape; it does not judge the map. The
// values come back as written, and [New] sanitises them and refuses the ones
// that collide — once, when the process starts, and in one place, so that a
// map built in code and a map read from a file are held to the same rule.
func LoadOverrides(path string) (Overrides, error) {
	if path == "" {
		return Overrides{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Overrides{}, fmt.Errorf("topo: overrides: %w", err)
	}
	defer f.Close()

	var ov Overrides
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ov); err != nil {
		return Overrides{}, fmt.Errorf("topo: overrides %s: %w", path, err)
	}
	// One object and nothing after it. A second object, or a stray brace, is
	// a file that was hand-edited into two, and the half that loaded is not
	// what the operator believes is in force.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Overrides{}, fmt.Errorf("topo: overrides %s: trailing data after the JSON object", path)
	}
	return ov, nil
}

// normalizeOverrides sanitises every override and refuses the ones that
// collide. It runs once per map: in [New] for the map a Discoverer carries,
// and in [NewLabeler] for a labeler built on its own. A map that came through
// either is clean, and [newLabeler] takes it as it is.
func normalizeOverrides(ov Overrides) (Overrides, error) {
	out := Overrides{
		HubTokens:  make(map[string]string, len(ov.HubTokens)),
		SlotLabels: make(map[string]string, len(ov.SlotLabels)),
	}

	// Sorted, so that a map holding two bad entries always reports the same one
	// first: an error message that changes between runs of the same config is
	// an error message operators stop believing.
	seen := make(map[string]string, len(ov.HubTokens))
	for _, usbPath := range sortedKeys(ov.HubTokens) {
		tok := sanitizeField(ov.HubTokens[usbPath])
		if tok == "" {
			return Overrides{}, fmt.Errorf("topo: hub token override for %q is empty after sanitising %q",
				usbPath, ov.HubTokens[usbPath])
		}
		if other, dup := seen[tok]; dup {
			return Overrides{}, fmt.Errorf("topo: hub token %q is claimed by both %s and %s; "+
				"one label pointing at two hubs sends an operator to the wrong rack position",
				tok, other, usbPath)
		}
		seen[tok] = usbPath
		out.HubTokens[usbPath] = tok
	}

	seenLabel := make(map[string]string, len(ov.SlotLabels))
	for _, usbPath := range sortedKeys(ov.SlotLabels) {
		clean := sanitizeLabel(ov.SlotLabels[usbPath])
		if clean == "" {
			return Overrides{}, fmt.Errorf("topo: slot label override for %q is empty after sanitising %q",
				usbPath, ov.SlotLabels[usbPath])
		}
		if other, dup := seenLabel[clean]; dup {
			return Overrides{}, fmt.Errorf("topo: slot label %q is claimed by both %s and %s; "+
				"two sockets under one label send an operator to the wrong device",
				clean, other, usbPath)
		}
		seenLabel[clean] = usbPath
		out.SlotLabels[usbPath] = clean
	}
	return out, nil
}

// Labeler renders rack_slot labels for one host.
//
// The zero value is not usable; construct with [NewLabeler].
type Labeler struct {
	host     string
	rack     string
	rackUnit int
	ov       Overrides
}

// NewLabeler builds a labeler from a host's rack coordinates.
//
// rack and rackUnit come from farm.hosts.rack_id and farm.hosts.rack_unit; an
// empty rack or a rackUnit of zero simply drops that field from the label
// rather than inventing a position. hostID is required, because a label with
// no rack and no unit still has to say which machine the hub is plugged into.
//
// It returns an error when two overrides would produce the same hub token: two
// hubs answering to "H3" is worse than no label at all, since it sends an
// operator confidently to the wrong one.
func NewLabeler(hostID, rack string, rackUnit int, ov Overrides) (*Labeler, error) {
	if strings.TrimSpace(hostID) == "" {
		return nil, fmt.Errorf("topo: a labeler needs a host id")
	}
	clean, err := normalizeOverrides(ov)
	if err != nil {
		return nil, err
	}
	return newLabeler(hostID, rack, rackUnit, clean), nil
}

// newLabeler builds a labeler from a map that normalizeOverrides has already
// been over. It cannot fail: a clean map has nothing left to refuse, and the
// host id was checked by whoever holds the map. Discovery calls this on every
// pass with the map New sanitised, which is why the sanitising is not here.
func newLabeler(hostID, rack string, rackUnit int, clean Overrides) *Labeler {
	return &Labeler{
		host:     strings.TrimSpace(hostID),
		rack:     strings.TrimSpace(rack),
		rackUnit: rackUnit,
		ov:       clean,
	}
}

// HubToken returns the "H" component for a hub.
func (l *Labeler) HubToken(hubPath string) string {
	if tok, ok := l.ov.HubTokens[hubPath]; ok {
		return tok
	}
	return HubOrdinal(hubPath)
}

// Slot renders the label for one port of one hub.
//
// slotPath is the USB path a device in that port has, which is what the
// SlotLabels override is keyed by.
func (l *Labeler) Slot(hubPath, slotPath string, port int) string {
	if label, ok := l.ov.SlotLabels[slotPath]; ok {
		return label
	}

	fields := make([]string, 0, 4)
	if rack := rackField(l.rack); rack != "" {
		fields = append(fields, rack)
	}
	if l.rackUnit > 0 {
		fields = append(fields, unitField(l.rackUnit))
	}
	if len(fields) == 0 {
		// No rack coordinates recorded for this host. The host id is the only
		// locus left, and a label without one is unwalkable.
		fields = append(fields, sanitizeLabel(l.host))
	}
	fields = append(fields, "H"+l.HubToken(hubPath))
	fields = append(fields, "P"+strconv.Itoa(port))
	return strings.Join(fields, "-")
}

// Check verifies that the hubs actually present produce distinct tokens.
//
// NewLabeler can only see collisions between overrides. This sees collisions
// between an override and a derived token — {"3-1.4": "3.1"} on a host that
// also has the hub "3-1" is the case that matters, since [HubOrdinal] derives
// "3.1" for that hub too. Discovery calls this before it writes any label.
func (l *Labeler) Check(hubPaths []string) error {
	seen := make(map[string]string, len(hubPaths))
	sorted := append([]string(nil), hubPaths...)
	sort.Strings(sorted)
	for _, p := range sorted {
		tok := l.HubToken(p)
		if other, dup := seen[tok]; dup {
			return fmt.Errorf("topo: hubs %s and %s both label as H%s on host %s; "+
				"fix the override map before these labels are written",
				other, p, tok, l.host)
		}
		seen[tok] = p
	}
	return nil
}

// HubOrdinal derives the stable hub token from a USB path.
//
// It is the path with the bus separator turned into a dot, so it stays
// injective: "3-1.4" -> "3.1.4", "3-0" (a root hub) -> "3.0". Two different
// physical positions can never collapse onto one token, and the token of a
// hub does not depend on any other hub existing.
func HubOrdinal(usbPath string) string {
	tok := sanitizeField(strings.ReplaceAll(usbPath, "-", "."))
	if tok == "" {
		return "0"
	}
	return tok
}

// rackField renders the rack component.
//
// Numeric rack ids get the conventional "R" prefix ("r1", "R1" and "1" all
// render as "R1"). A rack id that is already a name keeps it, because
// "RDC1-A7" reads like a typo where "DC1-A7" reads like a rack.
func rackField(rack string) string {
	rack = sanitizeLabel(rack)
	if rack == "" {
		return ""
	}
	trimmed := rack
	if rest, cut := strings.CutPrefix(rack, "R"); cut {
		trimmed = rest
	} else if rest, cut := strings.CutPrefix(rack, "r"); cut {
		trimmed = rest
	}
	if _, err := strconv.Atoi(trimmed); err == nil {
		return "R" + trimmed
	}
	return strings.ToUpper(rack)
}

// unitField renders the rack unit, zero-padded to two digits so that labels
// sort the way the shelves are stacked.
func unitField(unit int) string {
	if unit < 100 {
		return fmt.Sprintf("U%02d", unit)
	}
	return "U" + strconv.Itoa(unit)
}

// sanitizeField keeps a label component to characters that survive a log line,
// a URL and a label printer. "-" is excluded because it separates fields.
func sanitizeField(s string) string {
	return sanitize(s, false)
}

// sanitizeLabel is sanitizeField plus "-", for values that are a whole label
// or a whole field of their own (a host id, a rack name, an override).
func sanitizeLabel(s string) string {
	return sanitize(s, true)
}

func sanitize(s string, allowDash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '-' && allowDash:
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
