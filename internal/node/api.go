// The node API: the contract between a control plane and a farmd-node agent.
//
// # What is on the wire
//
// Three routes, all under /node/v1, all bearer-authenticated:
//
//	POST /node/v1/usb-reset    recovery tier 3, USBDEVFS_RESET
//	POST /node/v1/port-power   recovery tier 4, VBUS off, settle, on
//	GET  /node/v1/health       which epoch is in force, is the local adb server up
//
// [Agent.Handler] mounts them on the agent side; [Client] calls them from a
// control plane that runs somewhere else. This file is what the two halves
// agree on: the paths, the JSON, the meaning of every status code, and the
// checks a server must perform before it touches hardware.
//
// # Status codes are the whole point
//
// A caller across a network cannot see why nothing happened, so the code has
// to say. Four answers, and they mean genuinely different things:
//
//	200  the rung was performed
//	400  the request could not be read (ErrMalformedRequest)
//	401  the token was wrong (ErrUnauthorized)
//	409  the agent DECLINED — wrong host, unauthorised blast radius (ErrRefused)
//	501  this build cannot do it at all (ErrNotSupported)
//	5xx  the agent tried and the hardware did not come back
//
// Everything from 400 to 501 is a refusal: the agent did not touch the port,
// and repeating the identical request gets the identical answer. Only a 5xx
// means a rung was actually attempted and failed, and only that answer should
// push the recovery ladder toward a more destructive rung. A transport
// failure — no answer at all — is a third thing again, and [Client] reports it
// as [ErrUnreachable] rather than letting "the agent is down" be recorded as
// "the hardware is unrecoverable".
//
// # This table is the agent's whole vocabulary
//
// [StatusFor] emits 200, 400, 401, 409, 500 or 501 and nothing else, and every
// non-200 the agent writes carries its own reason as [OpResponse] JSON. Both
// halves of that sentence are load-bearing on the client side, because between
// a control plane and a device host there is usually an ingress, a mesh, or a
// tunnel, and every one of them can answer in the agent's place.
//
// So [Client] reads an answer it cannot attribute to the agent — a 502, 503 or
// 504, which no farmd-node ever writes, or any other 5xx whose body is not this
// API's JSON — as [ErrUnreachable] rather than as an attempted rung. That is
// deliberately the cautious direction: a genuine hardware failure misread as
// "unreachable" costs a delay and a page, while a pod restart misread as a
// failed rung is how a healthy phone gets rebooted and quarantined out from
// under a six-hour job. Anything else in the 4xx range is a refusal, since a
// far end that declines a request has not touched a port either way.
//
// # Why a devpath is checked against the host rather than trusted
//
// "usb:3-1.4" is a real port on every host in the fleet. A request that
// crossed a network carries a host_id precisely so the agent can check that it
// landed where the caller meant; without that check a misrouted tier-4 request
// cuts power to this rack because some other rack was sick. [ValidateOp] is
// that check, and it is the server's, not the client's: the caller is the
// party that might be wrong.
//
// # A node that serves nothing is a legitimate deployment
//
// [New] requires a token only when Config.Addr is set. An agent with no Addr
// serves no HTTP surface at all — it registers its host, discovers topology,
// enrolls devices and heartbeats, and the two hardware rungs are reachable
// only in-process — so there is no unauthenticated door to leave open and no
// credential to demand. The moment an Addr appears the token becomes
// mandatory, because these routes can cut power to ports holding live leases.

package node

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// The routes. They are constants because two processes have to agree on them:
// a typo on the client half would otherwise present as a 404 that reads like a
// dead agent. See TestAgentServesTheContractRoutes, which drives these
// constants against the real [Agent.Handler] mux so a rename on either side
// fails the build rather than a rack.
const (
	// APIPrefix is versioned so an agent and a control plane on different
	// releases fail loudly on an unknown route instead of quietly
	// misinterpreting each other's JSON.
	APIPrefix = "/node/v1"

	PathUSBReset  = APIPrefix + "/usb-reset"
	PathPortPower = APIPrefix + "/port-power"
	PathHealth    = APIPrefix + "/health"
)

// OpRequest is the body of both hardware operations.
//
// agent.go decodes into an unexported twin of this struct with
// DisallowUnknownFields set, so the tags here are not a convention: a field
// this type gains that the agent does not know is a 400 for every request the
// client sends.
type OpRequest struct {
	// HostID is the farm.hosts row the caller means. Required on the wire —
	// see the package comment on misrouting.
	HostID string `json:"host_id"`

	// Devpath is a USB position, "usb:3-1.4.2". Never a serial: duplicate OEM
	// serials are real, and a serial names no port.
	Devpath string `json:"devpath"`

	// Acknowledged lists the other devpaths in the same power domain whose
	// lease policy the caller has already checked and is willing to disturb.
	// Port power only. The agent still refuses if the domain contains anything
	// that is neither the target nor acknowledged, because the agent is the
	// only party that can see what is plugged into the hub right now.
	Acknowledged []string `json:"acknowledged,omitempty"`
}

// OpResponse is the answer to both operations.
//
// Refused restates in the body what the status code already says, so a client
// behind a proxy that rewrites status codes can still tell a declined rung
// from a failed one.
//
// Reason says WHICH refusal, in one word from the Reason* vocabulary, and it
// exists for the one refusal the control plane treats differently: a ganged
// power domain. Before it, the agent's ganged refusal was prose behind a
// generic 409, the client wrapped every 409 the same way, and the ladder
// counted "the rack needs per-port switching" under the same metric label as
// "a lease's policy said no" — two answers whose fixes are a purchase order
// and nothing, respectively. Empty on a 200, and empty from an agent older
// than this field, which a client reads as an unclassified refusal.
type OpResponse struct {
	OK      bool   `json:"ok,omitempty"`
	Error   string `json:"error,omitempty"`
	Refused bool   `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// The refusal vocabulary OpResponse.Reason is drawn from. [ReasonFor] is the
// only writer; a client compares against these and never against the prose.
const (
	// ReasonGanged: the port's power switch is shared with devices the caller
	// did not acknowledge. The agent did not touch the port. This is the one
	// reason the ladder records and counts on its own, as refused_ganged.
	ReasonGanged = "ganged"
	// ReasonPolicy: the agent's own rules declined — a request for another
	// host, a devpath that is not a USB position, a kernel that would undo
	// the cycle behind uhubctl's back. Nothing was touched.
	ReasonPolicy = "policy"
	// ReasonUnsupported: this build of the agent cannot perform the rung at
	// all (ErrNotSupported, HTTP 501).
	ReasonUnsupported = "unsupported"
)

// ReasonFor classifies an error into OpResponse.Reason. It is the server half
// of the vocabulary above; "" means the error is not a refusal.
func ReasonFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrGangedDomain):
		return ReasonGanged
	case errors.Is(err, ErrNotSupported):
		return ReasonUnsupported
	case errors.Is(err, ErrRefused):
		return ReasonPolicy
	default:
		return ""
	}
}

// HealthResponse is the answer to GET /node/v1/health.
//
// HostID is the identity the answering agent claims. A client that asked for
// one host and is answered by another has a routing problem, not a health
// problem, and [Client.Health] treats it as one.
type HealthResponse struct {
	HostID      string `json:"host_id"`
	HostEpoch   int64  `json:"host_epoch"`
	ADBServerUp bool   `json:"adb_server_up"`
	Platform    string `json:"platform"`
}

// vocabulary is one word of this package's error vocabulary that also answers
// to the recovery ladder's.
//
// The ladder classifies a host runner's error with errors.Is against
// [recovery.ErrRungRefused] and [recovery.ErrHostUnreachable], and takes an
// error that matches neither at face value: a failed rung, which it answers
// by escalating. This package's own words — ErrRefused, ErrNotSupported,
// ErrUnreachable — matched neither, so every refusal the agent made and every
// host it could not reach arrived at the ladder as broken hardware. Unwrap
// returning the recovery sentinel is what makes the two vocabularies one
// without either package importing the other's sentences: the message stays
// this package's, and errors.Is finds the ladder's word behind it.
type vocabulary struct {
	text  string
	means []error
}

func (v *vocabulary) Error() string   { return v.text }
func (v *vocabulary) Unwrap() []error { return v.means }

// Errors the contract adds to [ErrRefused] and [ErrNotSupported], which are
// declared in agent.go because the agent raises them locally too.
var (
	// ErrUnreachable means no answer arrived: the connection was refused, the
	// name did not resolve, the socket died, or the reply never came. It is
	// NOT a statement about the hardware and never about a lease. An agent
	// that is down leaves every device on its host leased, running, and
	// exactly where it was.
	ErrUnreachable error = &vocabulary{
		text:  "the host agent could not be reached",
		means: []error{recovery.ErrHostUnreachable},
	}

	// ErrGangedDomain is the refusal for a VBUS cycle on a port whose power
	// switch is shared with devices nobody acknowledged. It is a refusal —
	// errors.Is(err, ErrRefused) holds — with one more word attached, because
	// this is the refusal whose remedy is hardware: the rack needs per-port
	// switching. The agent raises it in front of uhubctl; the client raises it
	// again from a 409 whose reason is [ReasonGanged], so the ladder sees the
	// same error whether the agent answered in-process or over HTTP.
	ErrGangedDomain error = &vocabulary{
		text:  "refused by the host agent: ganged power domain",
		means: []error{ErrRefused, recovery.ErrRungRefusedGanged},
	}

	// ErrUnauthorized is a 401. It is a refusal — the rung is not permitted
	// from here — and in practice it means a token was rotated on one side
	// only, which is why it must never read as broken hardware.
	ErrUnauthorized = errors.New("the host agent rejected this token")

	// ErrMalformedRequest is a 400: the agent could not read the request, so
	// nothing was attempted. Resending it unchanged gets the same answer.
	ErrMalformedRequest = errors.New("the host agent could not read this request")
)

// IsRefused reports whether the agent declined to act.
//
// True means: the port was not touched, the request will get the same answer
// if it is repeated unchanged, and the recovery ladder must record a refusal
// rather than a failed rung — a rung recorded as failed is a rung the ladder
// answers by escalating to a more destructive one, on a device that is very
// probably in the middle of somebody's job.
func IsRefused(err error) bool { return errors.Is(err, ErrRefused) }

// IsUnreachable reports whether the agent could not be reached at all.
//
// True means nothing is known about the hardware: the rung may not have been
// attempted, and the right response is to retry later or to page whoever owns
// the host, never to conclude that the device is unrecoverable.
func IsUnreachable(err error) bool { return errors.Is(err, ErrUnreachable) }

// positionRE mirrors the CHECK constraint on farm.slots.usb_path, which is
// also what farm.slots.adb_devpath is generated from ('usb:' || usb_path). It
// admits neither a slash nor a dot-dot, which matters because on the agent
// this string is joined onto /sys and /dev.
var positionRE = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)

// DevpathPosition turns the farm's devpath form ("usb:3-1.4.2") into the USB
// position the kernel names ("3-1.4.2").
//
// Anything else — an emulator, a network target, a serial that wandered in
// where a devpath was expected — is refused rather than coerced, because the
// next thing that happens to this value is a power cycle. The refusal is
// [ErrRefused] and not [ErrMalformedRequest] so that it reads identically
// whether it was raised here or by the Linux half of the agent.
func DevpathPosition(devpath string) (string, error) {
	p := strings.TrimPrefix(strings.TrimSpace(devpath), "usb:")
	if !positionRE.MatchString(p) {
		return "", fmt.Errorf("node: %w: %q is not a USB position; hardware is addressed "+
			"only by devpath, because duplicate OEM serials are real and a serial names "+
			"no port", ErrRefused, devpath)
	}
	return p, nil
}

// ValidateOp is the check a server runs on a decoded request before it goes
// anywhere near a hub.
//
// hostID is the id of the host the agent actually runs on. The three answers
// it can give are the three refusals in the package comment: a missing field
// is a 400, a devpath that is not a position is a 409, and a request for
// another host is a 409 — the last being the one that keeps a misrouted tier-4
// from darkening the wrong rack.
func ValidateOp(req OpRequest, hostID string) error {
	if strings.TrimSpace(req.Devpath) == "" {
		return fmt.Errorf("node: %w: devpath is required; positions, never serials",
			ErrMalformedRequest)
	}
	// An HTTP request must name the host it means. The in-process seam may
	// leave this empty because a [recovery.HostRunner] call reaches exactly the
	// one agent it was handed to; a request that crossed a network could have
	// been routed anywhere.
	if strings.TrimSpace(req.HostID) == "" {
		return fmt.Errorf("node: %w: host_id is required on the node endpoint; send the "+
			"farm.hosts.id this request is meant for, because the same devpath names a "+
			"different physical port on every host", ErrMalformedRequest)
	}
	if _, err := DevpathPosition(req.Devpath); err != nil {
		return err
	}
	if req.HostID != hostID {
		return fmt.Errorf("node: %w: this request names host %q and %s is a port on host "+
			"%q; the agent will not act on a devpath that belongs to another host",
			ErrRefused, req.HostID, req.Devpath, hostID)
	}
	return nil
}

// StatusFor maps an error onto the status code this contract requires. It is
// the server half of the table in the package comment, and [Client] reads that
// table back the other way.
//
// The set it can return — 200, 400, 401, 409, 500, 501 — is closed on purpose,
// and [Client.statusError] depends on that: a gateway status arriving from a
// node address is proof that something other than the agent answered. Adding a
// code here without teaching the client half is how an intermediary's answer
// starts reading as a verdict on somebody's hardware.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrMalformedRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotSupported):
		return http.StatusNotImplemented
	case errors.Is(err, ErrRefused):
		return http.StatusConflict
	default:
		// The agent tried and the hardware did not come back. This is the only
		// answer that means a rung was actually attempted.
		return http.StatusInternalServerError
	}
}

// Authorize reports whether r carries the expected bearer token.
//
// The comparison is constant time over SHA-256 digests, so neither the
// token's bytes nor its length leak through timing. An empty want authorises
// nothing: a server with no credential configured has no way to recognise a
// caller, and treating "no token" as "any token" on routes that cut power to
// live racks would be the worst possible default.
func Authorize(r *http.Request, want string) bool {
	if want == "" {
		return false
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	expect := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(got[:], expect[:]) == 1
}
