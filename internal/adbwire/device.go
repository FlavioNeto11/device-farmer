package adbwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ConnState is the connection state the ADB server reports for a transport.
//
// The constants below are spelled exactly as farm.device_runtime.adb_state's
// CHECK constraint spells them, so the watchdog can store a ConnState
// verbatim and a typo becomes a compile error on one side and a constraint
// violation on the other, rather than a silently unmatched row.
type ConnState string

// Connection states. StateAbsent is ours, not the server's: it means the
// device was not in the listing at all.
const (
	StateDevice        ConnState = "device"
	StateOffline       ConnState = "offline"
	StateUnauthorized  ConnState = "unauthorized"
	StateAuthorizing   ConnState = "authorizing"
	StateConnecting    ConnState = "connecting"
	StateDetached      ConnState = "detached"
	StateNoPermissions ConnState = "no_permissions"
	StateBootloader    ConnState = "bootloader"
	StateRecovery      ConnState = "recovery"
	StateSideload      ConnState = "sideload"
	StateRescue        ConnState = "rescue"
	StateHost          ConnState = "host"
	StateAbsent        ConnState = "absent"
	StateUnknown       ConnState = "unknown"
)

// Usable reports whether the ADB server would accept a service request for a
// transport in this state. It describes the wire, nothing more.
func (s ConnState) Usable() bool { return s == StateDevice }

// ParseConnState normalises the server's free-text state into the schema's
// vocabulary.
//
// The server does not restrict itself to single words: a permissions problem
// is reported as a whole sentence pointing at a documentation URL, which is
// why this matches on prefixes and substrings rather than on equality.
//
// The exact-match arm lists ALL fourteen values, including the three the
// server never emits, so that parsing is total over the schema's vocabulary
// and therefore a round trip: a value read back out of
// farm.device_runtime.adb_state parses to itself. Omitting "absent" here would
// silently rewrite it to "unknown" on the way back in, turning "we have never
// seen this device" into "we cannot tell", which are different alarms.
func ParseConnState(raw string) ConnState {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch ConnState(s) {
	case StateDevice, StateOffline, StateUnauthorized, StateAuthorizing,
		StateConnecting, StateDetached, StateNoPermissions, StateBootloader,
		StateRecovery, StateSideload, StateRescue, StateHost, StateAbsent,
		StateUnknown:
		return ConnState(s)
	}
	switch {
	case s == "":
		return StateUnknown
	case strings.Contains(s, "permission"):
		// "no permissions; see [http://…]" and "insufficient permissions
		// for device" are the same condition: udev rules on the host.
		return StateNoPermissions
	case strings.HasPrefix(s, "unauthorized"):
		return StateUnauthorized
	case strings.HasPrefix(s, "authorizing"):
		return StateAuthorizing
	case strings.HasPrefix(s, "connecting"):
		return StateConnecting
	default:
		return StateUnknown
	}
}

// Device is one line of a long-format device listing.
//
// Serial is recorded because operators use it and because the schema keeps it
// as an observation, but it is never an address: see [Device.Target].
type Device struct {
	// Serial is what the device reports about itself. It is NOT unique.
	Serial string
	// Devpath is the position in the USB tree, e.g. "usb:3-1.4.2". This is
	// the only field in this struct that identifies a physical position,
	// and the only one any targeted call accepts.
	Devpath string
	// State is the normalised connection state.
	State ConnState
	// RawState is the server's own wording, kept for operator-facing logs.
	RawState string
	// Product, Model and Codename come from the listing's key:value tail.
	// The server substitutes underscores for spaces in Model.
	Product  string
	Model    string
	Codename string
	// TransportID is the server's small integer handle. It is reused after
	// an adb server restart, so it is meaningless without the host epoch
	// the schema pairs it with; never treat it as an identity.
	TransportID int64
	// Extra holds any key:value the server emitted that we do not model.
	Extra map[string]string
}

// Target returns the devpath to address this device by, or an error if the
// listing carried none.
//
// Emulators and network-attached devices have no devpath. They cannot be
// power-cycled, cannot be found in a rack, and must not be silently
// substituted with serial addressing, so this refuses rather than falling
// back.
func (d Device) Target() (string, error) {
	if d.Devpath == "" {
		return "", &UsageError{
			Op:     "target",
			Detail: "device has no devpath and cannot be addressed by position",
			Value:  d.Serial,
			kind:   ErrInvalidDevpath,
		}
	}
	return d.Devpath, nil
}

// Snapshot is one complete view of everything attached to an ADB server.
//
// Snapshots are whole-state, never deltas: the server re-sends the entire
// list on every change, and this package passes that property through so a
// consumer that misses one is not left with a corrupted model.
type Snapshot struct {
	// At is when the snapshot was received.
	At time.Time
	// Endpoint is the ADB server it came from.
	Endpoint string
	// Sequence counts snapshots within one tracker, starting at 1. It is
	// zero for a one-shot [Client.Devices] listing, which belongs to no
	// tracker and therefore has no position in any series.
	Sequence uint64
	// Devices is the full listing, in the order the server sent it.
	Devices []Device
}

// ByDevpath indexes the snapshot by physical position. Devices without a
// devpath are omitted, because they have no position to key on.
func (s Snapshot) ByDevpath() map[string]Device {
	m := make(map[string]Device, len(s.Devices))
	for _, d := range s.Devices {
		if d.Devpath != "" {
			m[d.Devpath] = d
		}
	}
	return m
}

// AmbiguousSerials returns the serials that more than one attached device is
// currently reporting.
//
// This is the collision the whole devpath-addressing rule exists to survive,
// and it is cheap to detect here, so the watchdog can set
// farm.devices.serial_ambiguous instead of discovering the problem during a
// recovery action.
func (s Snapshot) AmbiguousSerials() []string {
	counts := make(map[string]int, len(s.Devices))
	for _, d := range s.Devices {
		if d.Serial != "" {
			counts[d.Serial]++
		}
	}
	var out []string
	for serial, n := range counts {
		if n > 1 {
			out = append(out, serial)
		}
	}
	// Map iteration order is randomised, and this value ends up in operator
	// logs and in a database column. An unstable order would make two
	// identical observations look like two different ones.
	slices.Sort(out)
	return out
}

// ---------------------------------------------------------------------------
// Listing parser
// ---------------------------------------------------------------------------

// tailKeys are the key:value tokens the long listing appends after the state.
var tailKeys = map[string]struct{}{
	"product":      {},
	"model":        {},
	"device":       {},
	"transport_id": {},
}

// noSerialToken is what the ADB server prints in the serial column for a
// transport whose serial it has not read yet — an unauthorized device, most
// commonly, which is exactly the device an operator is trying to find.
//
// It is three whitespace-separated words in a whitespace-separated format.
// Splitting the line into fields and taking field zero as the serial therefore
// records "(no" as a serial and shifts every later column by two, so the state
// of the one device that most needs attention parses as "unknown".
const noSerialToken = "(no serial number)"

// extraKeyRe is what an unmodelled key:value token's key must look like
// before the parser will believe it is a key at all.
var extraKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parseDeviceList parses the payload of host:devices-l or a
// host:track-devices-l frame.
//
// The state field is not a single token: a host missing its udev rules
// reports a whole sentence, complete with spaces and a URL. Parsing therefore
// walks the tail backwards over the tokens it recognises and treats
// everything left between the serial and that tail as the state. Splitting on
// whitespace and taking field 1 — the obvious implementation — mislabels
// exactly the devices an operator most needs to see.
func parseDeviceList(payload string) []Device {
	lines := strings.Split(payload, "\n")
	out := make([]Device, 0, len(lines))
	for _, line := range lines {
		// TrimSpace also removes the \r of a CRLF-terminated listing.
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A trailing informational line such as "* daemon started *"
		// is not a device.
		if strings.HasPrefix(line, "*") {
			continue
		}
		d, ok := parseDeviceLine(line)
		if !ok {
			continue
		}
		out = append(out, d)
	}
	return out
}

func parseDeviceLine(line string) (Device, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return Device{}, false
	}

	var d Device
	rest := fields[1:]
	if strings.HasPrefix(line, noSerialToken) {
		// Three words of sentinel, and an empty serial is the honest record:
		// the server has not read one. Leaving it empty also keeps this
		// device out of AmbiguousSerials, where a shared literal
		// "(no serial number)" would otherwise manufacture a collision
		// between every unauthorized device on the host.
		if len(fields) < 4 {
			return Device{}, false
		}
		rest = fields[3:]
	} else {
		d.Serial = fields[0]
	}

	// Walk backwards over the recognised tail. One pass, not two: an
	// unmodelled key:value must not be able to STOP the walk, because the
	// devpath sits to the left of the tail and a server that adds a field
	// between them would otherwise cost us the position — the device would
	// still be listed, but with an empty Devpath it would drop out of
	// ByDevpath and become unaddressable for as long as the field existed.
	tailStart := len(rest)
	for tailStart > 0 {
		tok := rest[tailStart-1]

		// Checked before the key:value arm: "usb:3-1.4.2" splits on its
		// first colon into a plausible-looking key "usb", so testing for a
		// key first would file the devpath under Extra and lose it. This is
		// the same predicate the addressing calls use, so a devpath recorded
		// here is always one ValidateDevpath will accept.
		if isDevpath(tok) {
			d.Devpath = tok
			tailStart--
			continue
		}

		key, val, found := strings.Cut(tok, ":")
		if !found {
			break // a bare word: the state begins here
		}
		if _, known := tailKeys[key]; known {
			switch key {
			case "product":
				d.Product = val
			case "model":
				d.Model = val
			case "device":
				d.Codename = val
			case "transport_id":
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					d.TransportID = n
				}
			}
			tailStart--
			continue
		}
		// A key:value we do not model is kept verbatim rather than
		// discarded. extraKeyRe is strict on purpose: the "no permissions"
		// state ends in a bracketed URL whose scheme would otherwise be
		// eaten as a key, silently deleting the most useful half of the
		// message and swallowing the words before it.
		if !extraKeyRe.MatchString(key) {
			break
		}
		if d.Extra == nil {
			d.Extra = make(map[string]string, 2)
		}
		d.Extra[key] = val
		tailStart--
	}

	d.RawState = strings.Join(rest[:tailStart], " ")
	d.State = ParseConnState(d.RawState)
	return d, true
}

// ---------------------------------------------------------------------------
// Devpath validation
// ---------------------------------------------------------------------------

// devpathRe accepts what the ADB server actually mints for a USB transport:
// "usb:<bus>-<port>[.<port>…]" on Linux and "usb:<location>" on Darwin.
//
// The pattern is a security boundary, not a formatting nicety. A devpath is
// interpolated into a colon-delimited service string, so a value containing a
// colon, a space or a newline could terminate the field early and retarget
// the request at another device — and the request on the other end of that
// mistake may be a reboot aimed at a phone that is three hours into somebody
// else's run.
var devpathRe = regexp.MustCompile(`^usb:[0-9A-Za-z][0-9A-Za-z._-]*$`)

// maxDevpathLen bounds a devpath. A USB tree is at most seven tiers deep, so
// anything remotely near this is not a position.
const maxDevpathLen = 128

// isDevpath is the single predicate behind both the listing parser and
// [ValidateDevpath], so that what this package will record and what it will
// address can never drift apart.
func isDevpath(s string) bool {
	return s != "" && len(s) <= maxDevpathLen && devpathRe.MatchString(s)
}

// ValidateDevpath reports whether s is a devpath this package will address.
// It rejects everything it is not certain of.
func ValidateDevpath(s string) error {
	if s == "" {
		return &UsageError{Op: "devpath", Detail: "empty devpath", kind: ErrInvalidDevpath}
	}
	if len(s) > maxDevpathLen {
		return &UsageError{Op: "devpath", Detail: "devpath is implausibly long", Value: s, kind: ErrInvalidDevpath}
	}
	if !devpathRe.MatchString(s) {
		return &UsageError{
			Op:     "devpath",
			Detail: `devpath must look like "usb:3-1.4.2"`,
			Value:  s,
			kind:   ErrInvalidDevpath,
		}
	}
	return nil
}

// validateServiceString rejects payloads that cannot be framed. Service
// strings legitimately carry arbitrary shell text, so only NUL — which the
// server treats as a terminator — and the length limit are enforced.
func validateServiceString(op, service string) error {
	if service == "" {
		return &UsageError{Op: op, Detail: "empty service string"}
	}
	if strings.IndexByte(service, 0) >= 0 {
		return &UsageError{Op: op, Detail: "service string contains NUL"}
	}
	if len(service) > maxMessage {
		return &UsageError{
			Op:     op,
			Detail: fmt.Sprintf("service string of %d bytes exceeds the %d-byte length prefix", len(service), maxMessage),
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Device-side shell, protocol v2
// ---------------------------------------------------------------------------

// ShellPacketID identifies a frame in the shell v2 stream.
type ShellPacketID byte

// Shell v2 packet identifiers.
const (
	ShellStdin      ShellPacketID = 0
	ShellStdout     ShellPacketID = 1
	ShellStderr     ShellPacketID = 2
	ShellExit       ShellPacketID = 3
	ShellCloseStdin ShellPacketID = 4
	ShellWindowSize ShellPacketID = 5
	ShellInvalid    ShellPacketID = 255
)

// shellHeaderLen is one id byte plus a little-endian uint32 length.
const shellHeaderLen = 5

// MaxShellPacket bounds a single shell v2 payload. The length field is a
// uint32, so a desynchronised stream would otherwise ask this process for
// four gigabytes of memory on the strength of four garbage bytes.
const MaxShellPacket = 1 << 20

// ShellService builds the device-side service string for a command.
//
// "v2" multiplexes stdout and stderr and carries a real exit status, which a
// plain "shell:" stream does not. "raw" suppresses the pty, so output is not
// mangled by line-ending translation — the difference between a parseable
// logcat capture and a corrupted one.
func ShellService(command string) string { return "shell,v2,raw:" + command }

// ShellPacketReader reads the framed side of a shell v2 stream.
type ShellPacketReader struct {
	r   io.Reader
	hdr [shellHeaderLen]byte
}

// NewShellPacketReader wraps r. r is normally a [Stream].
func NewShellPacketReader(r io.Reader) *ShellPacketReader {
	return &ShellPacketReader{r: r}
}

// Next returns the next packet. It returns io.EOF when the device closed the
// stream, which is the normal end of a command that produced no exit frame.
func (s *ShellPacketReader) Next() (ShellPacketID, []byte, error) {
	// io.ReadFull distinguishes the two endings that matter: io.EOF on a
	// clean packet boundary is the normal end of a command, while
	// io.ErrUnexpectedEOF mid-header means the stream was cut.
	if _, err := io.ReadFull(s.r, s.hdr[:]); err != nil {
		return ShellInvalid, nil, err
	}
	id := ShellPacketID(s.hdr[0])
	n := binary.LittleEndian.Uint32(s.hdr[1:])
	if n > MaxShellPacket {
		return ShellInvalid, nil, fmt.Errorf("adbwire: shell packet of %d bytes exceeds the %d-byte cap", n, MaxShellPacket)
	}
	if n == 0 {
		return id, nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return id, nil, err
	}
	return id, buf, nil
}

// WriteShellPacket writes one framed packet, for stdin and for window-size
// changes.
func WriteShellPacket(w io.Writer, id ShellPacketID, payload []byte) error {
	if len(payload) > MaxShellPacket {
		return &UsageError{
			Op:     "shell_write",
			Detail: fmt.Sprintf("payload of %d bytes exceeds the %d-byte cap", len(payload), MaxShellPacket),
		}
	}
	hdr := make([]byte, shellHeaderLen+len(payload))
	hdr[0] = byte(id)
	binary.LittleEndian.PutUint32(hdr[1:shellHeaderLen], uint32(len(payload)))
	copy(hdr[shellHeaderLen:], payload)
	_, err := w.Write(hdr)
	return err
}

// ShellResult is the drained output of a shell v2 command.
type ShellResult struct {
	// Stdout and Stderr are the demultiplexed streams.
	Stdout []byte
	Stderr []byte
	// ExitCode is the device-side status. It is -1 when the stream ended
	// without an exit frame.
	ExitCode int
	// Exited distinguishes "exited with 0" from "never told us".
	Exited bool
	// Truncated records that the output cap was reached and the rest was
	// discarded.
	Truncated bool
}

// DrainShellV2 reads a shell v2 stream to its end.
//
// limit bounds the total captured output; pass 0 for the default. Reaching
// the cap stops capture and sets Truncated — it does not fail, because a
// chatty command should not look like a broken device.
func DrainShellV2(r io.Reader, limit int) (*ShellResult, error) {
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	res := &ShellResult{ExitCode: -1}
	pr := NewShellPacketReader(r)
	total := 0
	for {
		id, payload, err := pr.Next()
		if err != nil {
			if err == io.EOF {
				return res, nil
			}
			return res, err
		}
		switch id {
		case ShellStdout, ShellStderr:
			if total >= limit {
				res.Truncated = true
				continue
			}
			if total+len(payload) > limit {
				payload = payload[:limit-total]
				res.Truncated = true
			}
			total += len(payload)
			if id == ShellStdout {
				res.Stdout = append(res.Stdout, payload...)
			} else {
				res.Stderr = append(res.Stderr, payload...)
			}
		case ShellExit:
			if len(payload) > 0 {
				res.ExitCode = int(payload[0])
				res.Exited = true
			}
		default:
			// Packets we do not model (window size echoes, and any id a
			// future server adds) are skipped rather than treated as a
			// desynchronised stream.
		}
	}
}
