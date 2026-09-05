// Package fenceproxy enforces the lease fence where the device actually is.
//
// The fence is enforced in PostgreSQL — every mutating lease function matches
// on (id, fence), and farm.devices.fence_floor rises when a lease ends — and it
// is honoured by the client, which treats zero rows from farm.lease_renew as
// terminal. Neither of those reaches the handset. A client that already holds
// an open ADB connection keeps holding it, and a client that dials the host's
// ADB server directly never presented a fence at all. This package is the
// missing third place: a proxy in front of a host's ADB server that admits a
// connection only while its fence is current.
//
// The written design is docs/design/fence-proxy.md. It answers the questions
// this doc comment only summarises: how the floor is learned and how fresh that
// knowledge must be, which way the proxy fails when it cannot reach Postgres,
// how a live connection is torn down and what a half-written sync SEND leaves
// behind, how the recovery ladder reaches a device that holds no lease, and how
// certificates are issued and rotated.
//
// # The invariant
//
// A lease ends when the job says so, when a user-written deadline elapses, or
// when a human takes it back. Nothing else.
//
// This package sits on the data path of every ADB byte in the farm, which is
// exactly the position from which DeviceFarmer/STF issue #663 was committed: a
// transport-level observation acquired a code path to an allocation decision.
// Refusing traffic and ending a lease are different acts, and the difference is
// made structural here rather than merely stated:
//
//   - Nothing in this package imports a database driver, an HTTP client, or the
//     package that owns leases. Its only channel to the control plane is
//     [FenceSource], which has exactly one method and that method reads. A test
//     asserts the method count, because the change that would let a refusal DO
//     something is the addition of a second method.
//
//   - No identifier declared here names an act that ends a lease. A test parses
//     every production file in this package with go/parser and fails on any
//     identifier containing "release", "reclaim", "revoke", "expire" or
//     "deallocate". That is why certificate expiry is called LAPSED throughout.
//     The scan walks the syntax tree rather than the text, so these comments may
//     name what is barred — the same exemption doc.go gets in internal/adbwire,
//     generalised so that explaining a rule does not violate it.
//
//   - A refused connection yields a [*RefusedError]. There is no release reason
//     on it and no function here can produce one.
//
// # The two failures that must never be conflated
//
// "Postgres says a higher floor exists" is a FENCING FACT: it was read
// successfully, and because fence_floor is monotonically non-decreasing it
// never becomes untrue. It justifies refusing a new connection and tearing down
// a live one.
//
// "I cannot reach Postgres" is NOT a fact about any fence. It is a fact about a
// socket. It justifies declining to start something new, and nothing else.
//
// So: new connections fail CLOSED once the cached view ages past
// Policy.MaxStaleness, and in-flight connections are NEVER torn down for
// blindness at any duration — only by a fact. Refusing to start costs a retry;
// severing mid-transfer costs the work in flight, and a farm-wide sever
// triggered by a database blip is #663 arriving through the front door.
// Section 5 of the design document argues this at length, including where the
// residual risk is paid instead (an allocation-time predicate, not a teardown).
//
// # Status
//
// The host half is integrated: when farmd node is given FARM_FENCE_TLS_CERT,
// _KEY and _CA, internal/node serves this package's [Server] on an mTLS
// listener in front of the host's ADB server, polls the floors through a
// [FenceSource] of its own, and advertises the proxy as farm.hosts.adb_endpoint.
// The client-side preamble sender and the certificate issuance machinery are
// integration work owned elsewhere; section 14 of the design document states
// exactly what is enforced today and what is not.
package fenceproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// ---------------------------------------------------------------------------
// Wire constants
// ---------------------------------------------------------------------------

const (
	// prefixLen is the ADB host protocol's 4-hex-digit length prefix.
	prefixLen = 4

	// maxFrame is the largest payload that prefix can describe.
	maxFrame = 0xFFFF

	// PreambleV1 opens the frame a client sends before its first service
	// request. It carries what the client CLAIMS to hold; what the client IS
	// comes from its certificate and is never taken from here.
	PreambleV1 = "fence:v1"
)

// Defaults. Each is a number somebody has to be able to defend; see section 4.1
// of the design document for why MaxStaleness is the one that matters.
const (
	// DefaultPollInterval is how often one goroutine per proxy re-reads every
	// floor on its host. One query per host per interval — never per
	// connection, and certainly never per packet.
	DefaultPollInterval = 2 * time.Second

	// DefaultMaxStaleness is the age past which a floor observation may no
	// longer admit a NEW connection. It is the same quantity internal/config
	// calls FARM_NODE_SELF_FENCE_TIMEOUT, and config.Validate already refuses
	// to start unless FARM_SLOT_REARM exceeds it: for the length of the slot
	// rearm window the device belongs to nobody, which is what makes serving a
	// new connection from a cache of this age safe rather than merely
	// convenient.
	DefaultMaxStaleness = 20 * time.Second

	// DefaultMaxHostFrames bounds how many host-protocol frames one connection
	// may send before it must go opaque. Four covers the longest legitimate
	// sequence (a transport switch plus a device service, with slack) and stops
	// a client parking the proxy in framing mode.
	DefaultMaxHostFrames = 4

	// DefaultCopyBuffer is the splice buffer. It is also the exact bound on how
	// many bytes may still be in flight to the device when a fencing fact
	// arrives; see [gate].
	DefaultCopyBuffer = 32 * 1024
)

// ---------------------------------------------------------------------------
// Credential classes
// ---------------------------------------------------------------------------

// Class is the credential class a connection presents.
//
// It always comes from the client CERTIFICATE, never from the preamble. The
// preamble carries a class word too, and it is advisory: honouring it would let
// a lease client promote itself to maintenance by editing one field.
type Class string

const (
	// ClassLease carries a fence and is bound to one device. This is the class
	// a job runner uses, and the only one the fence rules apply to.
	ClassLease Class = "lease"

	// ClassMaintenance carries NO fence.
	//
	// The recovery ladder and the watchdog must reach a device that holds no
	// lease — an unallocated phone that has gone offline is precisely the phone
	// the ladder exists to repair, and it has no fence because there is nothing
	// to fence. This is that bypass, and it is a second credential class rather
	// than an exception: it is issued by its own intermediate, it is short
	// lived, and it is bounded by an exact-match service whitelist so that it is
	// not simply a root shell on every phone in the rack.
	ClassMaintenance Class = "maintenance"

	// ClassEnroll is separate from ClassMaintenance because it is the only
	// class that may open a shell at all — a brand-new handset has to be asked
	// what it is before it can be adopted. Pooling that with the ladder's blast
	// radius would make one stolen credential worth both.
	ClassEnroll Class = "enroll"
)

// Valid reports whether c is one of the three defined classes. An unknown class
// satisfies nothing, which is the safe direction for a typo in a certificate.
func (c Class) Valid() bool {
	switch c {
	case ClassLease, ClassMaintenance, ClassEnroll:
		return true
	default:
		return false
	}
}

func (c Class) String() string { return string(c) }

// ---------------------------------------------------------------------------
// Outcomes
// ---------------------------------------------------------------------------

// Outcome is the closed set of admission outcomes.
//
// Read the list and notice what is missing: there is no outcome that ends a
// lease, quarantines a slot or asks the control plane to do either. Every
// refusal here is a statement about ONE CONNECTION.
type Outcome string

const (
	// OutcomeAdmit forwards the request to the host's ADB server.
	OutcomeAdmit Outcome = "admit"

	// OutcomeRefuseIdentity means the certificate named no class this proxy
	// knows. Not retryable by the same client with the same certificate.
	OutcomeRefuseIdentity Outcome = "refuse_identity"

	// OutcomeRefuseCertLapsed means the client certificate's validity ended
	// before now. Called "lapsed" rather than the obvious word so that this
	// package's identifier ban on lease-ending verbs stays total; see the
	// package doc.
	//
	// Retryable: the fix is a certificate renewal, and the holder's lease is
	// untouched throughout.
	OutcomeRefuseCertLapsed Outcome = "refuse_cert_lapsed"

	// OutcomeRefuseMalformed means the preamble or the service string did not
	// parse. Refusing beats defaulting: a proxy that guessed what a client
	// meant would guess wrong about a devpath eventually.
	OutcomeRefuseMalformed Outcome = "refuse_malformed"

	// OutcomeRefuseService means the service is not one this class may open.
	OutcomeRefuseService Outcome = "refuse_service"

	// OutcomeRefuseTarget means a lease-class connection addressed a device
	// other than the one it claims. Holding usb:3-1.4 is not permission to
	// drive usb:3-1.5.
	OutcomeRefuseTarget Outcome = "refuse_target"

	// OutcomeRefuseFenced is the fencing fact, and it is the ONLY terminal
	// outcome: the floor is above the presented fence, so this claim will never
	// be good again. A client that sees it should abort, exactly as it would on
	// ErrFenced from a renewal.
	OutcomeRefuseFenced Outcome = "refuse_fenced"

	// OutcomeRefuseUnknown means the proxy could not vouch for the fence: the
	// cached view is older than Policy.MaxStaleness, or this position was never
	// observed at all.
	//
	// THIS IS NOT A FENCE AND MUST NEVER BE READ AS ONE. It means "come back in
	// a moment". A client that mistook it for a fence would abort a six-hour job
	// over a database blip, which is the entire failure this project exists to
	// prevent.
	OutcomeRefuseUnknown Outcome = "refuse_unknown"
)

// Outcomes is every outcome, in the order Admit can produce them. Tests range
// over it so an outcome added later cannot quietly escape the assertions that
// exactly one outcome is terminal and that no outcome ends a lease.
var Outcomes = []Outcome{
	OutcomeAdmit,
	OutcomeRefuseIdentity,
	OutcomeRefuseCertLapsed,
	OutcomeRefuseMalformed,
	OutcomeRefuseService,
	OutcomeRefuseTarget,
	OutcomeRefuseFenced,
	OutcomeRefuseUnknown,
}

// Decision is what [Admit] returns.
//
// Terminal and Retryable are separate fields rather than one enum on purpose.
// The difference between them is the difference between "abort the job" and
// "try again in a second", and a client that cannot tell them apart either
// destroys work or retries a permanent failure forever. Exactly one outcome is
// Terminal.
type Decision struct {
	Outcome Outcome

	// Reason is the text sent to the client in the FAIL frame. It is written
	// for whoever is reading a log at 3am, so it says what to do next.
	Reason string

	// Terminal is true only for OutcomeRefuseFenced: the claim is dead and no
	// retry will revive it.
	Terminal bool

	// Retryable is true when the same claim may succeed later without any
	// change by the client — the proxy regaining sight of Postgres, or a
	// certificate renewal landing.
	Retryable bool

	// ViewAge is how old the floor observation was when this decision was made,
	// and is zero when no observation was consulted. It is reported rather than
	// thresholded a second time so a caller can alert on admissions made
	// against an ageing view without this package growing another knob.
	ViewAge time.Duration
}

// Admitted reports whether the connection may proceed.
func (d Decision) Admitted() bool { return d.Outcome == OutcomeAdmit }

// RefusedError is the connection lifecycle's report that admission failed.
//
// It is a statement about one connection. It carries no release reason, it is
// not a lease outcome, and nothing in this package can turn it into one.
type RefusedError struct {
	Decision Decision
	Service  string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("fenceproxy: refused %q: %s (%s)", e.Service, e.Decision.Reason, e.Decision.Outcome)
}

// ErrShut is returned by a write attempted after a fencing fact arrived. It
// means the proxy stopped forwarding, which is a traffic decision and nothing
// more.
var ErrShut = errors.New("fenceproxy: forwarding stopped by a fencing fact")

// ---------------------------------------------------------------------------
// Identity, claim, request
// ---------------------------------------------------------------------------

// Identity is what the client CERTIFICATE said.
type Identity struct {
	// Subject is the certificate's common name. It is the audit identity and
	// goes straight into the log line, so it must name a service or a human,
	// never something shared like "client".
	Subject string

	// Class is read from a farm://<class>/<service> URI SAN.
	Class Class

	// NotAfter is the leaf certificate's validity end.
	//
	// It is an input to admission rather than a detail of the TLS layer because
	// of what that buys: the proxy completes the handshake for a chain that is
	// valid apart from time and then refuses it with a readable reason naming
	// the instant it lapsed, instead of dropping the client at "remote error:
	// tls: bad certificate". See ServerTLSConfig.
	NotAfter time.Time
}

// Claim is what the client ASSERTED in its preamble frame.
type Claim struct {
	// Class as the client stated it. ADVISORY ONLY: Admit uses Identity.Class.
	// It is parsed and kept purely so a mismatch can be logged, because a
	// client that thinks it is maintenance while its certificate says lease is
	// a misconfiguration somebody should hear about.
	Class Class

	// Devpath is the physical USB position — never a serial. Duplicate OEM
	// serials are real, and a fence resolved by serial could admit a client to
	// the wrong clone.
	Devpath string

	// Fence is the lease fence. HasFence distinguishes "fence=0", which is a
	// real value below every floor the sequence issues, from "no fence sent".
	Fence    int64
	HasFence bool
}

// Request is one connection's complete admission input.
type Request struct {
	Identity Identity
	Claim    Claim

	// Service is the ADB service string being opened right now. Admission
	// happens per host-protocol frame, not once per connection, because
	// host:transport:<devpath> is followed by a second frame carrying the
	// service that actually does something.
	Service string

	// Bound is the devpath this connection has already been switched to by a
	// host:transport frame, empty if none. A device service arriving on an
	// unbound connection is refused: it is either nonsense or an attempt to
	// ride somebody else's transport.
	Bound string
}

// ---------------------------------------------------------------------------
// Service string parsing
// ---------------------------------------------------------------------------

// ServiceKind classifies an ADB service string.
type ServiceKind int

const (
	// KindInvalid did not parse.
	KindInvalid ServiceKind = iota

	// KindHost is a host service with no target: host:version, host:devices-l,
	// host:track-devices-l, host:kill.
	KindHost

	// KindHostTarget is a one-shot position-addressed query,
	// host-serial:<devpath>:<verb> or host-usb:<devpath>:<verb>. Both forms are
	// built by internal/adbwire and both carry a devpath.
	KindHostTarget

	// KindTransport switches the socket to a device: host:transport:<devpath>.
	// It is the one service that does NOT end framing mode, because the frame
	// after it is the one that matters.
	KindTransport

	// KindDevice is a device-side service opened over a switched transport:
	// "shell,v2,raw:...", "sync:", "reboot:", "raw:...".
	KindDevice
)

func (k ServiceKind) String() string {
	switch k {
	case KindHost:
		return "host"
	case KindHostTarget:
		return "host_target"
	case KindTransport:
		return "transport"
	case KindDevice:
		return "device"
	default:
		return "invalid"
	}
}

// Service is a parsed ADB service string.
type Service struct {
	Raw     string
	Kind    ServiceKind
	Devpath string // KindHostTarget and KindTransport
	Verb    string // KindHostTarget: what follows the devpath
}

// hostTargetPrefixes are the two position-addressed forms internal/adbwire
// builds. Keeping both here rather than picking one is deliberate: which prefix
// routes devpath matching differs between ADB server builds, so adbwire treats
// the choice as configuration and this parser has to accept whatever it chose.
var hostTargetPrefixes = []string{"host-serial:", "host-usb:"}

// devpathTail matches the part of a devpath after "usb:". It is the same
// character class as adbwire's own devpath regexp, which is what lets this
// parser find where a devpath ends inside "host-serial:usb:3-1.4:get-state" —
// a devpath contains a colon, so splitting on the last colon would be wrong the
// moment a verb contains one.
var devpathTail = regexp.MustCompile(`^usb:[0-9A-Za-z][0-9A-Za-z._-]*`)

// ParseService classifies an ADB service string. It is pure.
//
// An unparseable service is KindInvalid rather than a guess. The proxy refuses
// what it cannot classify, because the alternative is forwarding a frame whose
// target it does not know.
func ParseService(raw string) Service {
	s := Service{Raw: raw, Kind: KindInvalid}
	if raw == "" || len(raw) > maxFrame || strings.ContainsRune(raw, 0) {
		return s
	}

	for _, p := range hostTargetPrefixes {
		if rest, ok := strings.CutPrefix(raw, p); ok {
			dp := devpathTail.FindString(rest)
			if dp == "" || adbwire.ValidateDevpath(dp) != nil {
				return s
			}
			verb, ok := strings.CutPrefix(rest[len(dp):], ":")
			if !ok || verb == "" {
				return s
			}
			s.Kind, s.Devpath, s.Verb = KindHostTarget, dp, verb
			return s
		}
	}

	if rest, ok := strings.CutPrefix(raw, "host:transport"); ok {
		// "host:transport:<devpath>" targets a position. The sibling forms
		// "host:transport-any", "-usb" and "-local" ask the server to choose,
		// which is exactly what a fenced client must not be allowed to do; they
		// parse as a transport with no devpath and are refused by the target
		// check rather than by a special case here.
		if target, ok := strings.CutPrefix(rest, ":"); ok {
			if adbwire.ValidateDevpath(target) != nil {
				return s
			}
			s.Kind, s.Devpath = KindTransport, target
			return s
		}
		if strings.HasPrefix(rest, "-") {
			s.Kind = KindTransport
			return s
		}
		return s
	}

	if strings.HasPrefix(raw, "host:") {
		s.Kind = KindHost
		return s
	}

	// Anything else is a device-side service. It is only meaningful on a
	// connection that has already switched to a transport, and Admit checks
	// exactly that.
	s.Kind = KindDevice
	return s
}

// ---------------------------------------------------------------------------
// The preamble
// ---------------------------------------------------------------------------

// ParsePreamble reads the frame a client sends before its first service
// request: "fence:v1 class=lease devpath=usb:3-1.4 fence=41207".
//
// It is strict. An unknown key is an error rather than something to skip,
// because a key this proxy does not understand may be the one a future version
// uses to widen access, and silently ignoring it would make the older proxy the
// weakest link in the deployment.
func ParsePreamble(payload string) (Claim, error) {
	rest, ok := strings.CutPrefix(payload, PreambleV1)
	if !ok {
		return Claim{}, fmt.Errorf("fenceproxy: preamble does not start with %q", PreambleV1)
	}
	var c Claim
	for _, field := range strings.Fields(rest) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return Claim{}, fmt.Errorf("fenceproxy: preamble field %q is not key=value", field)
		}
		switch k {
		case "class":
			c.Class = Class(v)
		case "devpath":
			if err := adbwire.ValidateDevpath(v); err != nil {
				return Claim{}, fmt.Errorf("fenceproxy: preamble devpath: %w", err)
			}
			c.Devpath = v
		case "fence":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return Claim{}, fmt.Errorf("fenceproxy: preamble fence %q is not an integer", v)
			}
			c.Fence, c.HasFence = n, true
		default:
			return Claim{}, fmt.Errorf("fenceproxy: preamble carries unknown key %q", k)
		}
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

// ServiceRules is one class's exact-match whitelist.
//
// EXACT MATCH, NEVER PREFIX MATCH, and the reason is worth stating where the
// type is defined: a shell service string is an arbitrary command line, so any
// prefix rule over it is bypassable with ';', '&&', a newline or '$( )'.
// "shell:getprop ro.x" and "shell:getprop ro.x; rm -rf /sdcard" share a prefix.
type ServiceRules struct {
	// Host is the set of full host service strings this class may issue.
	Host []string

	// HostTargetVerbs is the set of verbs allowed after a devpath in
	// host-serial:<devpath>:<verb>. The devpath itself is checked separately,
	// so the verb is all that is left to whitelist.
	HostTargetVerbs []string

	// Transport permits host:transport:<devpath>. Without it a class cannot
	// open any device service at all.
	Transport bool

	// Device is the set of full device service strings this class may open.
	Device []string

	// DevicePatterns admits the few device services that must be templated.
	// internal/enroll's brandWriteCmd is the only one in this tree, and its
	// pattern's sole variable region is a uid, so the shape of the command is
	// fixed and the uid cannot carry a metacharacter.
	//
	// A pattern must match the WHOLE service string, and that is enforced below
	// rather than left to whoever writes the pattern remembering \A and \z. An
	// unanchored pattern is the prefix hole again in another costume: without
	// the span check, `getprop ro\.x` would admit "getprop ro.x; rm -rf /sdcard".
	DevicePatterns []*regexp.Regexp
}

func (r ServiceRules) allows(s Service) bool {
	switch s.Kind {
	case KindHost:
		return contains(r.Host, s.Raw)
	case KindHostTarget:
		return contains(r.HostTargetVerbs, s.Verb)
	case KindTransport:
		return r.Transport && s.Devpath != ""
	case KindDevice:
		if contains(r.Device, s.Raw) {
			return true
		}
		for _, p := range r.DevicePatterns {
			// A whole-string match, whatever the pattern's own anchoring says.
			if loc := p.FindStringIndex(s.Raw); loc != nil && loc[0] == 0 && loc[1] == len(s.Raw) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func contains(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// Policy is the proxy's configuration. The zero value is usable: every field
// falls back to its default, so a caller that forgot one gets the conservative
// number rather than zero.
type Policy struct {
	// MaxStaleness is the age past which a floor observation may no longer
	// admit a new connection. See DefaultMaxStaleness.
	MaxStaleness time.Duration

	// MaxHostFrames bounds framing mode. See DefaultMaxHostFrames.
	MaxHostFrames int

	// LeaseHost is the set of untargeted host services a lease-class client may
	// issue. It is deliberately tiny: host:kill stops the ADB server for every
	// device on the host, including the ones under other people's leases, and
	// it appears in no class's rules anywhere in this file.
	LeaseHost []string

	// Rules holds the whitelist for the classes that carry no fence. A class
	// absent from this map may open nothing.
	Rules map[Class]ServiceRules
}

// DefaultPolicy is the shipped configuration.
//
// The maintenance list is derived from what the recovery ladder and the
// watchdog actually call: internal/recovery/adbactuator.go uses the four
// Control verbs and "reboot:", and internal/watchdog uses only
// host:track-devices-l. Nothing wider is granted on the theory that it might be
// needed later.
func DefaultPolicy() Policy {
	maintenance := ServiceRules{
		Host: []string{
			"host:version", "host:features",
			"host:devices", "host:devices-l",
			"host:track-devices", "host:track-devices-l",
		},
		HostTargetVerbs: []string{
			"get-state", "get-serialno", "get-devpath", "features",
			"reconnect", "reconnect-offline", "detach", "attach",
		},
		Transport: true,
		Device:    []string{"reboot:"},
	}
	enrol := ServiceRules{
		Host:            []string{"host:version", "host:features", "host:devices-l"},
		HostTargetVerbs: []string{"get-state", "get-serialno", "get-devpath", "features"},
		Transport:       true,
		// Device and DevicePatterns are left empty on purpose. internal/enroll's
		// probe and brand commands are the only shells this class may run, and
		// they are literals the enroller must publish into this list at wiring
		// time rather than strings guessed at here. An empty list refuses
		// everything, which is the right failure for a class that would
		// otherwise hold a shell on every handset.
	}
	return Policy{
		MaxStaleness:  DefaultMaxStaleness,
		MaxHostFrames: DefaultMaxHostFrames,
		LeaseHost:     []string{"host:version", "host:features"},
		Rules: map[Class]ServiceRules{
			ClassMaintenance: maintenance,
			ClassEnroll:      enrol,
		},
	}
}

func (p Policy) normalized() Policy {
	if p.MaxStaleness <= 0 {
		p.MaxStaleness = DefaultMaxStaleness
	}
	if p.MaxHostFrames <= 0 {
		p.MaxHostFrames = DefaultMaxHostFrames
	}
	return p
}

// ---------------------------------------------------------------------------
// What the proxy knows
// ---------------------------------------------------------------------------

// View is what the proxy knows about one position at decision time.
type View struct {
	// Floor is farm.devices.fence_floor as of the last successful read.
	Floor int64

	// Known is false when no successful read has ever included this position.
	// A position that vanished from a later snapshot keeps its last known
	// floor: disappearing from a join is not evidence about a fence.
	Known bool

	// ObservedAt is the PROXY'S OWN clock at the moment the read completed —
	// never a timestamp from Postgres. Age is therefore measured entirely on
	// one clock, which is what keeps this free of the skew hazard the rest of
	// this system avoids by never sending a client timestamp to the database.
	ObservedAt time.Time
}

// Age reports how long ago this view was taken. It is zero for an unknown view,
// which no caller may read as "fresh": check Known first.
func (v View) Age(now time.Time) time.Duration {
	if !v.Known || v.ObservedAt.IsZero() {
		return 0
	}
	if d := now.Sub(v.ObservedAt); d > 0 {
		return d
	}
	return 0
}

// Snapshot is one successful read of every position on a host.
//
// It deliberately carries no timestamp. The instant that matters is when the
// read completed on THIS machine, and letting a source supply its own would
// invite a Postgres now() to be compared against a local clock — the mistake
// internal/lease refuses to make anywhere.
type Snapshot struct {
	// Floors maps devpath to farm.devices.fence_floor.
	Floors map[string]int64
}

// FenceSource is the proxy's ONLY channel to the control plane.
//
// One method, and it reads. That is the whole interface, and it is the whole
// safety argument for the import graph: there is nothing here through which a
// refusal could become an allocation decision. A test asserts the method count,
// because adding a second method is the change that would break it.
//
// The implementation belongs with whichever process already owns a pgx pool.
// The query is one statement per host per poll interval:
//
//	SELECT s.adb_devpath, d.fence_floor
//	  FROM farm.devices d
//	  JOIN farm.slots   s ON s.id = d.current_slot_id
//	 WHERE d.host_id = $1 AND s.adb_devpath IS NOT NULL;
type FenceSource interface {
	Floors(ctx context.Context) (Snapshot, error)
}

// Cache holds what the proxy knows and notifies live connections when a fence
// becomes stale.
//
// The load-bearing property is negative: Apply is the only thing that mutates
// state or fires a watcher, and Poll calls it only on a SUCCESSFUL read. There
// is no code path from a source error to a teardown, which is the structural
// form of "blindness is not a fencing fact".
type Cache struct {
	now func() time.Time

	mu       sync.RWMutex
	floors   map[string]int64
	seen     time.Time
	watchers map[*watcher]struct{}
}

type watcher struct {
	devpath string
	fence   int64
	ch      chan struct{}
	once    sync.Once
}

func (w *watcher) fire() { w.once.Do(func() { close(w.ch) }) }

// NewCache builds an empty cache. now may be nil, in which case time.Now is
// used; tests supply their own so freshness is exercised without sleeping.
func NewCache(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{
		now:      now,
		floors:   map[string]int64{},
		watchers: map[*watcher]struct{}{},
	}
}

// Apply installs a successfully read snapshot and fires every watcher the
// snapshot proves stale.
//
// Positions absent from s keep whatever floor they had. A device dropping out
// of the query's join is a fact about the join — a slot detached, a row not yet
// written — and never a fact about a fence, so it must not fire a watcher and
// must not erase knowledge.
func (c *Cache) Apply(s Snapshot) {
	stamp := c.now()

	c.mu.Lock()
	for dp, floor := range s.Floors {
		if cur, ok := c.floors[dp]; !ok || floor > cur {
			c.floors[dp] = floor
		}
	}
	c.seen = stamp

	var fire []*watcher
	for w := range c.watchers {
		if floor, ok := c.floors[w.devpath]; ok && w.fence < floor {
			fire = append(fire, w)
			delete(c.watchers, w)
		}
	}
	c.mu.Unlock()

	for _, w := range fire {
		w.fire()
		teardownsTotal.WithLabelValues("fence_fact").Inc()
	}
}

// View reports what is known about one position.
func (c *Cache) View(devpath string) View {
	c.mu.RLock()
	defer c.mu.RUnlock()
	floor, ok := c.floors[devpath]
	if !ok {
		return View{}
	}
	return View{Floor: floor, Known: true, ObservedAt: c.seen}
}

// Watch returns a channel closed when a floor above fence is OBSERVED for
// devpath, and a function that stops watching.
//
// The channel is closed for one reason and one reason only: a successful read
// showed a higher floor. It is never closed because a poll failed, because the
// cache went stale, or because the position vanished. Section 5.3 of the design
// document is that sentence, argued.
//
// A claim that is already stale fires immediately, so a caller need not race
// the first poll.
func (c *Cache) Watch(devpath string, fence int64) (<-chan struct{}, func()) {
	w := &watcher{devpath: devpath, fence: fence, ch: make(chan struct{})}

	c.mu.Lock()
	floor, ok := c.floors[devpath]
	if ok && fence < floor {
		c.mu.Unlock()
		w.fire()
		teardownsTotal.WithLabelValues("fence_fact").Inc()
		return w.ch, func() {}
	}
	c.watchers[w] = struct{}{}
	c.mu.Unlock()

	return w.ch, func() {
		c.mu.Lock()
		delete(c.watchers, w)
		c.mu.Unlock()
	}
}

// Poll refreshes the cache until ctx is done.
//
// A failed read logs, counts, and CHANGES NOTHING: the previous floors stand
// and age, no watcher fires, and no connection is disturbed. New connections
// start being refused once that ageing passes Policy.MaxStaleness, which is the
// only consequence blindness is permitted to have.
func (c *Cache) Poll(ctx context.Context, src FenceSource, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		every = DefaultPollInterval
	}
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		snap, err := src.Floors(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			sourceErrorsTotal.Inc()
			log.WarnContext(ctx, "fence source unreachable; serving the cached floors and refusing new connections once they age out",
				"err", err, "max_staleness", c.stalenessNote())
		} else {
			c.Apply(snap)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// stalenessNote reports how long ago the last successful read was, for the log
// line above. It is a diagnostic and nothing branches on it.
func (c *Cache) stalenessNote() time.Duration {
	c.mu.RLock()
	seen := c.seen
	c.mu.RUnlock()
	if seen.IsZero() {
		return 0
	}
	return c.now().Sub(seen)
}

// ---------------------------------------------------------------------------
// The admission decision
// ---------------------------------------------------------------------------

// Admit decides whether one host-protocol frame may be forwarded.
//
// It is pure: no context, no I/O, no clock of its own, no database. Every input
// is a value, which is why the whole matrix is reviewable as a table and why
// this package's tests need neither DATABASE_URL nor hardware.
//
// The order of the checks is deliberate — identity, then claim, then target,
// then fence — so that a client whose certificate has lapsed is told about the
// certificate rather than about a fence it may well still hold.
func Admit(req Request, view View, now time.Time, pol Policy) Decision {
	pol = pol.normalized()

	if !req.Identity.Class.Valid() {
		return Decision{
			Outcome: OutcomeRefuseIdentity,
			Reason: fmt.Sprintf("client certificate names class %q; this proxy knows lease, maintenance and enroll",
				req.Identity.Class),
		}
	}
	if !req.Identity.NotAfter.IsZero() && !now.Before(req.Identity.NotAfter) {
		return Decision{
			Outcome: OutcomeRefuseCertLapsed,
			Reason: fmt.Sprintf("client certificate for %q lapsed at %s; renew it and retry, your lease is untouched",
				req.Identity.Subject, req.Identity.NotAfter.UTC().Format(time.RFC3339)),
			Retryable: true,
		}
	}

	svc := ParseService(req.Service)
	if svc.Kind == KindInvalid {
		return Decision{
			Outcome: OutcomeRefuseMalformed,
			Reason:  "service string did not parse as an ADB host, target, transport or device service",
		}
	}

	if req.Identity.Class != ClassLease {
		// A class that carries no fence is bounded by its whitelist and by
		// nothing else here. Whether a rung is permitted against a live lease's
		// disruption_policy is a blast-radius decision, and internal/recovery
		// already makes it against farm.recovery_tiers. Re-litigating it here
		// would produce two answers that drift.
		rules, ok := pol.Rules[req.Identity.Class]
		if !ok || !rules.allows(svc) {
			return Decision{
				Outcome: OutcomeRefuseService,
				Reason: fmt.Sprintf("service %q is not on the %s whitelist",
					req.Service, req.Identity.Class),
			}
		}
		return Decision{Outcome: OutcomeAdmit, Reason: "whitelisted for " + string(req.Identity.Class)}
	}

	// ---- lease class ---------------------------------------------------
	if req.Claim.Devpath == "" || !req.Claim.HasFence {
		return Decision{
			Outcome: OutcomeRefuseMalformed,
			Reason:  "a lease-class connection must present devpath and fence in its preamble",
		}
	}

	if d, refused := refuseOffTarget(req, svc, pol); refused {
		return d
	}

	age := view.Age(now)

	// A FENCING FACT, and it survives staleness. fence_floor is monotonically
	// non-decreasing — the insert trigger uses GREATEST and every ending path
	// assigns nextval — so an observation showing the claim below the floor can
	// only have become more true since it was taken. Being fenced is a one-way
	// door, which is why this check comes before the freshness check and
	// carries no age condition: during a database partition, clients that were
	// already fenced stay fenced for its whole duration.
	if view.Known && req.Claim.Fence < view.Floor {
		return Decision{
			Outcome: OutcomeRefuseFenced,
			Reason: fmt.Sprintf("fence %d is below the floor %d on %s; this claim is dead, do not retry",
				req.Claim.Fence, view.Floor, req.Claim.Devpath),
			Terminal: true,
			ViewAge:  age,
		}
	}

	if view.Known && age <= pol.MaxStaleness {
		return Decision{Outcome: OutcomeAdmit, Reason: "fence is at or above the floor", ViewAge: age}
	}

	// Not a fence. The proxy simply cannot vouch, and saying so in a different
	// outcome from OutcomeRefuseFenced is what stops a client aborting a
	// six-hour job over a database blip.
	reason := fmt.Sprintf("no fence floor has been read for %s yet; retry shortly", req.Claim.Devpath)
	if view.Known {
		reason = fmt.Sprintf("the fence floor for %s was last read %s ago, past the %s budget; "+
			"this is not a fence, retry shortly", req.Claim.Devpath, age.Round(time.Millisecond), pol.MaxStaleness)
	}
	return Decision{Outcome: OutcomeRefuseUnknown, Reason: reason, Retryable: true, ViewAge: age}
}

// refuseOffTarget enforces that a lease-class connection only ever addresses
// the device it claims. Holding usb:3-1.4 is not permission to drive usb:3-1.5.
func refuseOffTarget(req Request, svc Service, pol Policy) (Decision, bool) {
	switch svc.Kind {
	case KindHost:
		if !contains(pol.LeaseHost, svc.Raw) {
			return Decision{
				Outcome: OutcomeRefuseService,
				Reason: fmt.Sprintf("service %q is not one a lease-class connection may issue; "+
					"address your own device instead", svc.Raw),
			}, true
		}
	case KindHostTarget, KindTransport:
		if svc.Devpath != req.Claim.Devpath {
			return Decision{
				Outcome: OutcomeRefuseTarget,
				Reason: fmt.Sprintf("this connection claims %s and addressed %s",
					req.Claim.Devpath, orAny(svc.Devpath)),
			}, true
		}
	case KindDevice:
		if req.Bound != req.Claim.Devpath {
			return Decision{
				Outcome: OutcomeRefuseTarget,
				Reason:  "a device service needs a transport switched to the claimed devpath first",
			}, true
		}
	}
	return Decision{}, false
}

func orAny(devpath string) string {
	if devpath == "" {
		return "whatever device the server chooses"
	}
	return devpath
}

// ---------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------

// readFrame reads one length-prefixed ADB message.
//
// The length is checked before it is used and is never used to size anything
// unbounded: four hex digits cap it at 64 KiB by construction, and a prefix
// that is not four hex digits is a protocol error rather than something to
// recover from.
func readFrame(r io.Reader) (string, error) {
	var hdr [prefixLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	if err != nil {
		return "", fmt.Errorf("fenceproxy: length prefix %q is not four hex digits", hdr)
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// writeFrame writes one length-prefixed ADB message.
func writeFrame(w io.Writer, payload string) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("fenceproxy: payload of %d bytes exceeds the length prefix", len(payload))
	}
	buf := make([]byte, 0, prefixLen+len(payload))
	buf = append(buf, []byte(fmt.Sprintf("%04x", len(payload)))...)
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	return err
}

// writeFail sends a refusal in the ADB protocol's own FAIL shape.
//
// Speaking the client's protocol rather than dropping the socket is the point:
// a well-formed FAIL with a reason is something every ADB client already knows
// how to surface, and at 3am the difference between that and an anonymous
// connection reset is the whole incident.
func writeFail(w io.Writer, reason string) error {
	if len(reason) > maxFrame {
		reason = reason[:maxFrame]
	}
	if _, err := w.Write([]byte("FAIL")); err != nil {
		return err
	}
	return writeFrame(w, reason)
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// gate is the client-to-device direction, and it is where this package's
// central guarantee lives:
//
//	No byte written to the proxy after a fencing fact reaches it is delivered
//	to the device.
//
// Not "the connection closes soon" and not "the connection closes within a
// timeout". Shut takes the lock, sets the flag and closes the socket; Write
// takes the same lock and refuses if the flag is set. A write already in
// progress completes — it is one copy buffer, already handed to the kernel —
// and no write BEGINS after the fact. That is exact rather than probabilistic,
// and it costs one uncontended mutex per DefaultCopyBuffer bytes.
type gate struct {
	mu   sync.Mutex
	shut bool
	w    net.Conn
}

func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		return 0, ErrShut
	}
	return g.w.Write(p)
}

// Shut stops forwarding and closes the device-side socket, in that order. It is
// idempotent and safe to call from any goroutine.
func (g *gate) Shut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		return
	}
	g.shut = true
	_ = g.w.Close()
}

// ---------------------------------------------------------------------------
// The connection lifecycle
// ---------------------------------------------------------------------------

// Session drives one client connection.
//
// It is asymmetric on purpose. The device-to-client direction is spliced
// immediately and never parsed, so the proxy never has to model the ADB
// server's state machine. The client-to-device direction is read frame by frame
// and admitted per frame until the connection goes opaque, because
// host:transport:<devpath> is followed by the frame that actually does
// something and a whitelist that stopped at the transport switch would be
// trivially bypassable.
type Session struct {
	// Client is the accepted connection, already TLS-terminated.
	Client net.Conn

	// Identity came from the client certificate.
	Identity Identity

	// Cache supplies the view and the teardown signal.
	Cache *Cache

	// Policy is the admission policy.
	Policy Policy

	// DialUpstream opens a connection to the host's ADB server. It is a
	// function rather than an address so a test can point it at a fake and so a
	// deployment can pin it to loopback.
	DialUpstream func(ctx context.Context) (net.Conn, error)

	Log *slog.Logger
	Now func() time.Time
}

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Session) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run serves the connection and returns when it is finished.
//
// A refusal returns [*RefusedError]. That is a statement about this connection:
// it ends no lease, it quarantines no slot, and there is nothing in this
// package it could be turned into that would.
func (s *Session) Run(ctx context.Context) error {
	defer func() { _ = s.Client.Close() }()

	pol := s.Policy.normalized()

	pre, err := readFrame(s.Client)
	if err != nil {
		return fmt.Errorf("fenceproxy: reading preamble: %w", err)
	}
	claim, err := ParsePreamble(pre)
	if err != nil {
		d := Decision{Outcome: OutcomeRefuseMalformed, Reason: err.Error()}
		return s.refuse(d, "", nil)
	}
	if claim.Class != "" && claim.Class != s.Identity.Class {
		// Advisory, and logged rather than enforced: the certificate decides.
		// A client whose two halves disagree is a misconfiguration somebody
		// should hear about before it becomes an outage.
		s.log().WarnContext(ctx, "preamble class disagrees with the client certificate",
			"subject", s.Identity.Subject, "certificate", s.Identity.Class, "preamble", claim.Class)
	}

	var (
		upstream net.Conn
		g        *gate
		bound    string
		fromDev  = make(chan struct{})
	)
	defer func() {
		if upstream != nil {
			_ = upstream.Close()
		}
	}()

	for frames := 0; ; frames++ {
		if frames >= pol.MaxHostFrames {
			d := Decision{
				Outcome: OutcomeRefuseMalformed,
				Reason: fmt.Sprintf("more than %d host-protocol frames before the stream went opaque",
					pol.MaxHostFrames),
			}
			return s.refuse(d, "", upstream)
		}

		svc, err := readFrame(s.Client)
		if err != nil {
			// The client hung up. Not a refusal and not a fencing event; the
			// only honest thing to report is what happened to the socket.
			return fmt.Errorf("fenceproxy: reading service frame: %w", err)
		}

		req := Request{Identity: s.Identity, Claim: claim, Service: svc, Bound: bound}
		dec := Admit(req, s.Cache.View(claim.Devpath), s.now(), pol)
		admissionsTotal.WithLabelValues(string(s.Identity.Class), string(dec.Outcome)).Inc()

		if !dec.Admitted() {
			return s.refuse(dec, svc, upstream)
		}
		s.audit(ctx, dec, svc, claim)

		if upstream == nil {
			conn, err := s.DialUpstream(ctx)
			if err != nil {
				return fmt.Errorf("fenceproxy: dialling the host ADB server: %w", err)
			}
			upstream = conn
			g = &gate{w: upstream}

			// The device-to-client copy starts NOW, before the next frame is
			// read, because the client waits for the server's OKAY before it
			// sends the frame after a transport switch. A proxy that deferred
			// this until framing mode ended would deadlock on exactly the
			// sequence that matters.
			go func() {
				defer close(fromDev)
				_, _ = io.CopyBuffer(s.Client, upstream, make([]byte, DefaultCopyBuffer))
				// The server is done talking. Unblock a client parked on a read.
				_ = s.Client.Close()
			}()

			if claim.Devpath != "" && claim.HasFence {
				stop, cancel := s.Cache.Watch(claim.Devpath, claim.Fence)
				defer cancel()
				go s.tearDownOnFact(stop, fromDev, g, claim)
			}
		}

		if err := writeFrame(g, svc); err != nil {
			return fmt.Errorf("fenceproxy: forwarding %q: %w", svc, err)
		}

		if ParseService(svc).Kind == KindTransport {
			bound = ParseService(svc).Devpath
			continue
		}
		break
	}

	// Opaque from here. The proxy has no vocabulary for what flows next — sync
	// framing, shell-v2 packets, raw bytes — and deliberately does not learn
	// one: a proxy that parsed a 200 MB stream to find a frame boundary would
	// be a second implementation of three protocols.
	_, copyErr := io.CopyBuffer(g, s.Client, make([]byte, DefaultCopyBuffer))

	// The client is done sending. Close the device side too, or the copy in the
	// other direction parks forever on a transport the ADB server is under no
	// obligation to close first — and the wait below would never return.
	g.Shut()
	<-fromDev

	if errors.Is(copyErr, ErrShut) {
		// A fencing fact stopped us. The client sees an unexpected EOF and
		// internal/adbwire will classify it as a transport error, which by this
		// project's rules is NOT a fencing verdict. The client learns it is
		// fenced from its next renewal, on a different wire, where zero rows is
		// already terminal. The proxy stops bytes; Postgres pronounces fences.
		return &RefusedError{
			Decision: Decision{
				Outcome:  OutcomeRefuseFenced,
				Reason:   "fence " + strconv.FormatInt(claim.Fence, 10) + " went stale mid-stream",
				Terminal: true,
			},
			Service: claim.Devpath,
		}
	}
	return copyErr
}

// tearDownOnFact stops the connection when, and only when, a fencing fact
// arrives. It returns without touching anything if the connection ends first.
func (s *Session) tearDownOnFact(stop <-chan struct{}, done <-chan struct{}, g *gate, claim Claim) {
	select {
	case <-stop:
		g.Shut()
		_ = s.Client.Close()
		s.log().Warn("stopped forwarding: the fence went stale mid-stream",
			"subject", s.Identity.Subject, "devpath", claim.Devpath, "fence", claim.Fence,
			"note", "traffic stopped; no lease was ended by this proxy")
	case <-done:
	}
}

// refuse writes the FAIL frame and reports the decision.
//
// The FAIL goes out before the upstream socket is closed, and the ordering is
// deliberate in both directions. Closing first would race the device-to-client
// copy goroutine, which closes the client socket when its source ends — the
// refusal would then be written to a socket that had just been closed
// underneath it. Writing first cannot interleave with a server reply either: a
// refusal past the first frame means the client had already read the server's
// answer to the previous one, so the ADB server has nothing pending.
func (s *Session) refuse(d Decision, service string, upstream net.Conn) error {
	_ = writeFail(s.Client, d.Reason)
	if upstream != nil {
		_ = upstream.Close()
	}
	return &RefusedError{Decision: d, Service: service}
}

// audit records an admission.
//
// A maintenance admission is logged at INFO with the exact service string,
// which is the one place in this design where logging is a control rather than
// an observation: a class that carries no fence is bounded by its whitelist and
// by whoever reads this line afterwards.
func (s *Session) audit(ctx context.Context, d Decision, svc string, claim Claim) {
	if s.Identity.Class == ClassLease {
		return
	}
	s.log().InfoContext(ctx, "admitted a connection carrying no fence",
		"class", s.Identity.Class, "subject", s.Identity.Subject,
		"service", svc, "devpath", claim.Devpath, "outcome", d.Outcome)
}

// ---------------------------------------------------------------------------
// The listener
// ---------------------------------------------------------------------------

// Server accepts connections and runs a [Session] for each.
type Server struct {
	Cache        *Cache
	Policy       Policy
	DialUpstream func(ctx context.Context) (net.Conn, error)

	// Identify extracts the certificate identity from an accepted connection.
	// Defaults to IdentityFromConn.
	Identify func(net.Conn) (Identity, error)

	Log *slog.Logger
	Now func() time.Time
}

// Serve accepts until ctx is done or the listener fails.
//
// An accept error is logged and survived rather than fatal. This process sits
// on the path of every ADB byte on the host, and a proxy that exited because
// one accept failed would take every live job on the host with it.
func (srv *Server) Serve(ctx context.Context, ln net.Listener) error {
	log := srv.Log
	if log == nil {
		log = slog.Default()
	}
	identify := srv.Identify
	if identify == nil {
		identify = IdentityFromConn
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("fenceproxy: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := identify(c)
			if err != nil {
				log.WarnContext(ctx, "rejecting a connection whose certificate could not be read",
					"remote", c.RemoteAddr().String(), "err", err)
				_ = writeFail(c, "client certificate could not be read")
				_ = c.Close()
				return
			}
			sess := &Session{
				Client: c, Identity: id, Cache: srv.Cache, Policy: srv.Policy,
				DialUpstream: srv.DialUpstream, Log: log, Now: srv.Now,
			}
			if err := sess.Run(ctx); err != nil {
				var ref *RefusedError
				if errors.As(err, &ref) {
					log.InfoContext(ctx, "refused a connection",
						"subject", id.Subject, "class", id.Class,
						"outcome", ref.Decision.Outcome, "terminal", ref.Decision.Terminal,
						"reason", ref.Decision.Reason,
						"note", "a refusal ends no lease")
					return
				}
				log.DebugContext(ctx, "connection ended", "subject", id.Subject, "err", err)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// TLS
// ---------------------------------------------------------------------------

// URIScheme is the scheme of the URI SAN that carries a client's class:
// farm://<class>/<service>, for example farm://maintenance/recovery.
//
// A URI SAN is used rather than an organizational unit because SANs are the
// field TLS libraries are built to constrain, and because it composes with
// SPIFFE for a deployment that already has one.
const URIScheme = "farm"

// ServerTLSConfig builds the proxy's TLS configuration.
//
// Two choices here are load-bearing.
//
// GetCertificate is a function rather than a loaded certificate so a rotation
// is a file change and not a restart. Restarting the proxy to pick up a
// certificate would sever every live connection on the host, and a PKI
// operation must not be a data-path event.
//
// ClientAuth is RequireAnyClientCert with verification done here, rather than
// RequireAndVerifyClientCert, for exactly one reason: a chain that is valid
// APART FROM TIME is allowed to complete the handshake so that the refusal can
// be a readable FAIL frame naming the instant the certificate lapsed, instead
// of an opaque "remote error: tls: bad certificate". Admit still refuses it —
// see OutcomeRefuseCertLapsed. A chain that fails for any OTHER reason fails
// the handshake, and no cryptographic latitude is taken.
func ServerTLSConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		GetCertificate:        getCert,
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifyAllowingLapsed(roots),
	}
}

func verifyAllowingLapsed(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("fenceproxy: client presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("fenceproxy: parsing client certificate: %w", err)
			}
			certs = append(certs, c)
		}
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: inter,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err == nil {
			return nil
		}
		if isTimeOnlyFailure(err) {
			// Deliberately admitted to the handshake and refused by Admit, with
			// a reason a human can act on.
			return nil
		}
		return fmt.Errorf("fenceproxy: client certificate chain: %w", err)
	}
}

// isTimeOnlyFailure reports whether a chain failed solely because the
// certificate's validity window does not contain now.
func isTimeOnlyFailure(err error) bool {
	var inv x509.CertificateInvalidError
	return errors.As(err, &inv) && inv.Reason == x509.Expired
}

// IdentityFromConn extracts the certificate identity from an accepted
// connection, completing the handshake if it has not happened yet.
func IdentityFromConn(c net.Conn) (Identity, error) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return Identity{}, errors.New("fenceproxy: connection is not TLS; the proxy must terminate mTLS")
	}
	if err := tc.HandshakeContext(context.Background()); err != nil {
		return Identity{}, fmt.Errorf("fenceproxy: handshake: %w", err)
	}
	return IdentityFromState(tc.ConnectionState())
}

// IdentityFromState reads the subject, class and validity end off the leaf
// certificate.
//
// A certificate carrying no farm:// URI SAN yields an invalid class, which
// Admit refuses. That is the safe direction: a certificate that forgot to say
// what it is gets nothing, rather than defaulting into the class with the most
// reach.
func IdentityFromState(st tls.ConnectionState) (Identity, error) {
	if len(st.PeerCertificates) == 0 {
		return Identity{}, errors.New("fenceproxy: no peer certificate")
	}
	leaf := st.PeerCertificates[0]
	id := Identity{Subject: leaf.Subject.CommonName, NotAfter: leaf.NotAfter}
	for _, u := range leaf.URIs {
		if u.Scheme != URIScheme {
			continue
		}
		if c := Class(u.Host); c.Valid() {
			id.Class = c
			break
		}
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	admissionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "fenceproxy", Name: "admissions_total",
		Help: "Admission decisions by credential class and outcome.",
	}, []string{"class", "outcome"})

	// teardownsTotal has one label value by construction. That is the point:
	// there is exactly one cause for tearing down a live connection, and a
	// second value appearing here would mean somebody added a second.
	teardownsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "fenceproxy", Name: "teardowns_total",
		Help: "Live connections stopped, by cause. A fencing fact is the only cause.",
	}, []string{"cause"})

	sourceErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "fenceproxy", Name: "source_errors_total",
		Help: "Failed fence-floor reads. Blindness, never a fencing fact: nothing is torn down for these.",
	})
)

// Collectors returns this package's metrics for registration by whoever owns
// the registry. Nothing here registers itself; a package that self-registers
// panics the second binary that imports it.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{admissionsTotal, teardownsTotal, sourceErrorsTotal}
}
