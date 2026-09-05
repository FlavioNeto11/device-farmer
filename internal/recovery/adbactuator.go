package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// ---------------------------------------------------------------------------
// What a rung is allowed to claim
// ---------------------------------------------------------------------------

// Disposition is the actuator's diagnosis of one rung. It is deliberately a
// separate type from [Outcome]: Outcome is a database column constrained to
// five values, and this is the finer answer the ladder needs in order to
// decide what to do next.
//
// Three of these are the ones that matter, and folding any two of them
// together is how a device that was never broken ends up quarantined:
//
//   - DispositionRefused — the rung is not permitted here. There is no host
//     agent, the ADB server does not implement the verb, the devpath is
//     ambiguous. Nothing was done, and the device is exactly as it was.
//   - DispositionFailed — the rung ran and the hardware did not come back.
//     This is the only shape of evidence that justifies a more disruptive rung.
//   - DispositionUnreachable — nothing on this host answered. No rung will help
//     until that is fixed, so climbing spends the tier cooldowns of a whole
//     rack on a host that is simply gone.
//
// The remaining three describe rungs that had no verdict to give: the device
// came back, it stayed put, or the loop went away mid-action.
type Disposition string

const (
	// DispositionRecovered is claimed only after a state read came back
	// "device". No verb's own reply is ever taken as proof.
	DispositionRecovered Disposition = "recovered"
	// DispositionNoChange means the rung ran, the position was readable, and
	// the device is still not usable.
	DispositionNoChange Disposition = "no_change"
	// DispositionFailed means the rung ran and errored, or ran and left a
	// device that the host can see and cannot use.
	DispositionFailed Disposition = "failed"
	// DispositionRefused means the rung was not performed. The reason is
	// always recorded in human-readable form; see [RefusalOf].
	DispositionRefused Disposition = "refused"
	// DispositionUnreachable means the host agent or the host's ADB server
	// could not be contacted. It is a statement about the host and never about
	// the handset.
	DispositionUnreachable Disposition = "unreachable"
	// DispositionAborted means the caller's own loop ended the action.
	DispositionAborted Disposition = "aborted"
)

// Escalate reports whether this disposition is evidence that the hardware is
// still broken, which is the only evidence that justifies climbing to a more
// disruptive rung.
//
// Refused and unreachable are not that evidence. The first says the rung never
// ran; the second says nothing on the host answered. Treating either as a
// broken handset is the failure this whole type exists to prevent — a host
// that has gone away otherwise reads as a shelf of dead phones, and the ladder
// reboots, restarts and quarantines its way through devices whose only problem
// is on the other end of a socket.
func (d Disposition) Escalate() bool {
	return d == DispositionFailed || d == DispositionNoChange
}

// Outcome maps a disposition onto the value farm.recovery_attempts.outcome
// will accept. That column's CHECK list is ('recovered','no_change','failed',
// 'refused','aborted') and has no 'unreachable', so an unreachable host is
// recorded as a refusal — which is true as far as it goes, since the rung was
// not performed — and the two are told apart in the row by
// detail->>'disposition' and by the refusal text. Widening the CHECK would let
// the outcome column carry the distinction directly; until then this mapping is
// the one place that decides it.
func (d Disposition) Outcome() Outcome {
	switch d {
	case DispositionRecovered:
		return OutcomeRecovered
	case DispositionNoChange:
		return OutcomeNoChange
	case DispositionRefused, DispositionUnreachable:
		return OutcomeRefused
	case DispositionAborted:
		return OutcomeAborted
	default:
		return OutcomeFailed
	}
}

// Detail keys the actuator writes into every [Result]. They are constants
// because farm.recovery_attempts.detail is read by the API, the UI and by
// operators at 3am, and a key that drifts silently stops answering.
const (
	// DetailDisposition carries the [Disposition] as a string.
	DetailDisposition = "disposition"
	// DetailRefusal carries the human-readable reason a rung was refused or a
	// host was unreachable. It is what farm.recovery_attempts.refusal wants.
	DetailRefusal = "refusal"
	// DetailConfirmedState carries the ADB state actually read back after the
	// rung, so a "recovered" row can be audited against an observation rather
	// than against a verb's reply.
	DetailConfirmedState = "confirmed_state"
	// DetailRefusalKind classifies a refusal when the actuator can. It is
	// written only for the kinds the ladder acts on differently; today that
	// is [RefusalKindGanged], and its absence means "a refusal, reason in
	// DetailRefusal".
	DetailRefusalKind = "refusal_kind"

	// RefusalKindGanged is the DetailRefusalKind for [ErrRungRefusedGanged].
	// The word is shared with the node API's reason vocabulary so the same
	// string travels from uhubctl's answer to the metric label.
	RefusalKindGanged = "ganged"
)

// DispositionOf reports the diagnosis an actuator recorded in a Result. An
// actuator that recorded none is read through its Outcome, so a third-party
// Actuator implementation degrades to today's behaviour instead of being
// silently classified as unreachable.
func DispositionOf(r Result) Disposition {
	if s, ok := r.Detail[DetailDisposition].(string); ok && s != "" {
		return Disposition(s)
	}
	switch r.Outcome {
	case OutcomeRecovered:
		return DispositionRecovered
	case OutcomeNoChange:
		return DispositionNoChange
	case OutcomeRefused:
		return DispositionRefused
	case OutcomeAborted:
		return DispositionAborted
	default:
		return DispositionFailed
	}
}

// RefusalOf returns the human-readable reason a rung was refused or a host was
// unreachable, and "" when there is none.
func RefusalOf(r Result) string {
	s, _ := r.Detail[DetailRefusal].(string)
	return s
}

// RefusalKindOf returns the classification an actuator attached to a refusal,
// and "" when it attached none.
func RefusalKindOf(r Result) string {
	s, _ := r.Detail[DetailRefusalKind].(string)
	return s
}

// Record renders a Result as the two columns farm.recovery_attempts keeps for
// it: the outcome, and the refusal text — empty meaning the column stays NULL.
//
// It exists so the ladder has one call to make instead of a switch that has to
// remember the CHECK list, and so that the day the column list changes there is
// one place to change it.
func Record(r Result) (Outcome, string) {
	d := DispositionOf(r)
	out := d.Outcome()
	switch d {
	case DispositionRefused, DispositionUnreachable:
		reason := RefusalOf(r)
		if reason == "" {
			// A refusal with no reason is the gap this type exists to close:
			// the ladder stopped climbing and the row does not say why. Say
			// something true rather than leaving the column NULL.
			reason = fmt.Sprintf("the actuator reported %s without a reason", d)
		}
		return out, reason
	default:
		return out, ""
	}
}

// ---------------------------------------------------------------------------
// The actuator
// ---------------------------------------------------------------------------

// DefaultSettleInterval is how often a rung re-reads a position while waiting
// for the device to come back.
const DefaultSettleInterval = time.Second

// DefaultControlConfirm bounds how long the two cheap ADB rungs are given to
// prove they worked.
//
// It is much shorter than the action budget because the ladder walks its
// candidates one at a time: spending ninety seconds confirming a reconnect
// that did not take would park every other broken device in the fleet behind
// it. A reconnect or an attach either lands within a few seconds or has not
// landed; a reset, a power cycle or a reboot legitimately needs the whole
// budget, and gets it.
const DefaultControlConfirm = 5 * time.Second

// Sentinels a [HostRunner] wraps to say which of the three states its error
// means. They are matched with errors.Is, so an agent client can wrap them
// alongside its own error vocabulary.
//
// They live here rather than in the agent client because that client imports
// this package to hold the [HostRunner] interface, so the dependency can only
// run one way. A HostRunner that wraps neither sentinel and implements neither
// [RungFault] method is taken at face value: its error is a failed rung.
var (
	// ErrRungRefused means the agent declined: no such device node, uhubctl
	// absent, the kernel too old for USBDEVFS_RESET, policy forbidding it. The
	// device is fine and the ladder should move on rather than escalate.
	ErrRungRefused = errors.New("recovery: the host agent will not perform this rung")

	// ErrHostUnreachable means the agent was not reached, or answered
	// something that cannot be attributed to it. Nothing was learned about the
	// device.
	ErrHostUnreachable = errors.New("recovery: the host agent could not be reached")

	// ErrRungRefusedGanged is the one refusal the ladder needs to tell apart
	// from every other: the agent declined a VBUS cycle because the port's
	// power switch is shared with devices nobody authorised. It wraps
	// ErrRungRefused, so a caller that only asks "was it refused?" still gets
	// yes; a caller that asks this one learns that the rack, not the ladder,
	// is what needs changing — a rising rate here means the hub needs per-port
	// power switching, and that is a purchase order rather than a bug.
	ErrRungRefusedGanged = fmt.Errorf(
		"%w: the port shares one power domain with devices nobody authorised", ErrRungRefused)
)

// RungFault is the behavioural alternative to the sentinels above, for a
// [HostRunner] whose errors already carry this classification in their own
// vocabulary and would rather answer two questions than wrap two values.
type RungFault interface {
	error
	// RungRefused reports that the rung was declined and the device untouched.
	RungRefused() bool
	// HostUnreachable reports that the agent could not be contacted, so the
	// error says nothing about the device.
	HostUnreachable() bool
}

// ADBActuator performs the recovery rungs that can be reached through a host's
// ADB server, and honestly refuses the ones that cannot.
//
// The split matters. Tiers 1, 2, 5 and 7 are ADB verbs and are performed here.
// Tiers 3 and 4 need something only a process on the host itself can do —
// USBDEVFS_RESET on a device node, or toggling VBUS through uhubctl — and this
// actuator reports them as refused with a reason naming what is missing, so
// farm.recovery_attempts.refusal records why the ladder stopped climbing.
//
// Every answer it gives carries a [Disposition], because "no change" for a
// rung that was never attempted would be worse than useless: the ladder would
// conclude the hardware is unrecoverable and quarantine a device whose actual
// problem is that nobody is listening on the host.
//
// And no rung claims a recovery it did not watch happen. A verb's reply says
// the server accepted the request; only a state read says the device came back,
// and that read is the only thing this type will call proof. A false
// "recovered" is worse than a false "failed", because it suppresses the page
// that should have followed.
//
// Nothing here may end a lease. Recovery acts on behalf of a holder that keeps
// its device: the lease clock keeps ticking and the fence never moves.
type ADBActuator struct {
	log *slog.Logger

	// HostRunner, when set, performs the host-local rungs that ADB cannot
	// reach. It is the seam where farmd-node plugs in. Nil means this farm has
	// no host agent, and those rungs are refused rather than faked.
	HostRunner HostRunner

	// SettleInterval overrides [DefaultSettleInterval].
	SettleInterval time.Duration

	// ControlConfirm overrides [DefaultControlConfirm].
	ControlConfirm time.Duration

	mu      sync.Mutex
	clients map[string]*adbwire.Client // keyed by ADB endpoint
}

// HostRunner is implemented by an agent running on the device host, with
// access to /dev/bus/usb and to whatever hub control the rack provides.
//
// An implementation reports a declined rung and an unreachable agent
// differently — see [ErrRungRefused], [ErrHostUnreachable] and [RungFault] —
// because those two answers send the ladder in opposite directions.
type HostRunner interface {
	// USBReset re-enumerates one port via USBDEVFS_RESET.
	USBReset(ctx context.Context, hostID, devpath string) error
	// PortPower cycles VBUS for one port, authorising the disturbance of THAT
	// device only. On a ganged hub, where cutting one port cuts the whole power
	// domain, the agent refuses rather than taking the neighbours down as a
	// side effect. See DomainPowerRunner for the way to say otherwise.
	PortPower(ctx context.Context, hostID, devpath string) error
}

// DomainPowerRunner is the optional half of HostRunner that accepts a power
// domain acknowledgement: the positions, other than the target, that the
// control plane has checked lease policy for and is willing to see go dark.
//
// It is a SEPARATE interface rather than a third method on HostRunner so that
// adding it cannot break an existing implementation, and so that the default —
// a runner that only has PortPower — stays exactly as safe as it was. An
// acknowledgement is a widening of what the agent will permit, and a widening
// must be opted into on both sides: the control plane by populating
// Action.Acknowledged, the host by implementing this method.
//
// Nothing here is a promise that the neighbours are free. The agent still
// checks, against what is physically plugged into the hub at that instant, that
// every disturbed position is either the target or named here; anything else is
// a device nobody authorised and the cycle is refused. Being the last line is
// the point.
type DomainPowerRunner interface {
	PortPowerWithDomain(ctx context.Context, hostID, devpath string, acknowledged []string) error
}

// NewADBActuator returns an actuator that dials each host's ADB server lazily
// and caches the client, since a client is a dialer rather than a connection.
func NewADBActuator(log *slog.Logger, hostRunner HostRunner) *ADBActuator {
	if log == nil {
		log = slog.Default()
	}
	return &ADBActuator{
		log:        log,
		HostRunner: hostRunner,
		clients:    make(map[string]*adbwire.Client),
	}
}

// client returns the cached client for an endpoint, creating it on first use.
func (a *ADBActuator) client(endpoint string) (*adbwire.Client, error) {
	if endpoint == "" {
		return nil, errors.New("no adb_endpoint recorded in farm.hosts")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[endpoint]; ok {
		return c, nil
	}
	c := adbwire.New(endpoint)
	a.clients[endpoint] = c
	return c, nil
}

func (a *ADBActuator) settleInterval() time.Duration {
	if a.SettleInterval > 0 {
		return a.SettleInterval
	}
	return DefaultSettleInterval
}

func (a *ADBActuator) controlConfirm() time.Duration {
	if a.ControlConfirm > 0 {
		return a.ControlConfirm
	}
	return DefaultControlConfirm
}

// rung is one action in flight.
//
// It carries both contexts on purpose. ctx is the action's own budget, and its
// expiry means "the device did not come back in time" — a verdict about
// hardware. parent is the ladder's, and its expiry means the loop is shutting
// down, which is a verdict about nothing at all. Reporting the second as the
// first would write a shutdown into the record as evidence against a handset.
type rung struct {
	parent context.Context
	ctx    context.Context
	log    *slog.Logger
	act    Action
	// budget is the action timeout actually applied, which is not always
	// Action.Timeout: an unset one is defaulted. A refusal that has to name the
	// budget must name the one that was spent.
	budget time.Duration
}

// aborted reports whether the loop itself went away, as opposed to this
// action's budget elapsing.
func (r *rung) aborted() bool { return r.parent.Err() != nil }

// answer builds a Result, stamping the disposition and the reason so that
// every path out of this file records both.
func (r *rung) answer(d Disposition, reason string, detail map[string]any) Result {
	if detail == nil {
		detail = map[string]any{}
	}
	detail[DetailDisposition] = string(d)
	if reason != "" {
		detail[DetailRefusal] = reason
	}
	detail["tier"] = r.act.Tier
	detail["rung"] = r.act.TierName
	return Result{Outcome: d.Outcome(), Detail: detail}
}

// Recover performs one rung and reports what happened.
//
// It returns a nil error for every failure it can classify: a Result carrying
// a disposition is a more useful answer than an error the caller has to guess
// about, and guessing is what turns an unreachable host into a quarantined
// phone.
func (a *ADBActuator) Recover(ctx context.Context, act Action) (Result, error) {
	timeout := act.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r := &rung{
		parent: ctx,
		ctx:    actx,
		act:    act,
		budget: timeout,
		log: a.log.With(
			"tier", act.Tier, "rung", act.TierName,
			"rack_slot", act.RackSlot, "devpath", act.Devpath, "host", act.HostID),
	}

	// Every rung below is addressed by position, and so is the state read that
	// confirms it. A devpath the wire will not accept therefore fails locally,
	// identically, on every single poll — without a byte reaching the host — so
	// a rung run against one spends its whole action budget re-deriving the same
	// answer and then reports the host as the thing that never answered. Worse
	// for tiers 3 and 4: the agent would be asked to reset a position that
	// nothing can afterwards check. The devpath comes from farm.slots, so this
	// is a refusal naming the row an operator has to fix.
	if positionAddressed(act.Tier) {
		if err := adbwire.ValidateDevpath(act.Devpath); err != nil {
			reason := fmt.Sprintf(
				"tier %d (%s) is addressed by position and farm.slots.adb_devpath for %s on "+
					"host %s is %q, which cannot be addressed: %v",
				act.Tier, act.TierName, act.RackSlot, act.HostID, act.Devpath, err)
			r.log.Warn("recovery rung refused: the slot's devpath cannot be addressed",
				"err", err)
			return r.answer(DispositionRefused, reason,
				map[string]any{"error": err.Error()}), nil
		}
	}

	switch act.Tier {
	case 1:
		return a.control(r, adbwire.ControlReconnect, true), nil

	case 2:
		// Detach then attach. A detach that did not happen makes the attach
		// meaningless, so stop rather than report on a device that saw neither.
		//
		// Only the attach is confirmed. A successful detach is SUPPOSED to
		// leave the position unusable, so confirming after it would read a
		// working detach as a dead device and a failed detach as a recovery —
		// exactly backwards.
		if res := a.control(r, adbwire.ControlDetach, false); DispositionOf(res) != DispositionNoChange {
			return res, nil
		}
		return a.attachAfterDetach(r), nil

	case 3:
		return a.hostLocal(r, "USBDEVFS_RESET", nil, a.usbReset), nil

	case 4:
		return a.portPower(r), nil

	case 5:
		return a.reboot(r), nil

	case 7:
		return a.restartServer(r), nil

	default:
		// Tiers 0, 6 and 8 are database actions the ladder performs itself.
		// Reaching here means the tier table gained a rung nobody taught this
		// actuator, which is a configuration error worth surfacing.
		return r.answer(DispositionRefused, fmt.Sprintf(
			"tier %d (%s) is not an actuator rung; it is either a database "+
				"action performed by the ladder or a tier this build does not know",
			act.Tier, act.TierName), nil), nil
	}
}

// positionAddressed reports whether a tier is one this actuator performs, and
// therefore one whose action — or whose confirming state read — is aimed at a
// devpath. Tier 7 is included: host:kill needs no position, but the read that
// decides whether the server came back with the device does.
//
// The tiers not listed are the ladder's own database actions (0, 6, 8), which
// never reach here with anything to address.
func positionAddressed(tier int) bool {
	switch tier {
	case 1, 2, 3, 4, 5, 7:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// ADB rungs
// ---------------------------------------------------------------------------

// control runs one position-addressed ADB verb, and — when confirm is set —
// waits for a state read to say the device came back.
//
// A verb that succeeds without confirmation reports no_change, not recovered:
// the server accepting the request is not the device returning, and the
// difference is the whole reason this function takes the flag.
func (a *ADBActuator) control(r *rung, cmd adbwire.ControlCmd, confirm bool) Result {
	c, res, ok := a.endpointClient(r)
	if !ok {
		return res
	}

	reply, err := c.Control(r.ctx, r.act.Devpath, cmd)
	if err != nil {
		d, reason := a.classifyWire(r, string(cmd), err)
		r.log.Log(r.ctx, levelFor(d), "recovery rung did not complete",
			"cmd", string(cmd), "disposition", string(d), "err", err)
		return r.answer(d, reason, map[string]any{
			"cmd": string(cmd), "error": err.Error(),
		})
	}

	detail := map[string]any{"cmd": string(cmd), "reply": reply}
	if !confirm {
		return r.answer(DispositionNoChange, "", detail)
	}
	return a.confirm(r, c, string(cmd), a.controlConfirm(), false, detail)
}

// attachAfterDetach runs the second half of tier 2 and keeps the answer honest
// about the first half.
//
// The detach has already landed by the time this is called, so the position is
// no longer claimed by the ADB server. Every answer except a confirmed recovery
// therefore describes a device that WAS touched — and the refusal template says
// the opposite in so many words ("the device is as it was"), because it is
// written for a rung that never ran. Left uncorrected, an operator reading
// farm.recovery_attempts.refusal is told nothing happened while the position
// sits detached, and goes looking for a hardware fault that is really an
// unfinished rung waiting for the next attach.
func (a *ADBActuator) attachAfterDetach(r *rung) Result {
	res := a.control(r, adbwire.ControlAttach, true)
	if DispositionOf(res) == DispositionRecovered {
		return res
	}

	res.Detail["detached"] = true
	if reason := RefusalOf(res); reason != "" {
		res.Detail[DetailRefusal] = fmt.Sprintf(
			"%s; the detach before it did land, so %s on host %s is left detached until a "+
				"later attach re-claims it",
			reason, r.act.Devpath, r.act.HostID)
	}
	return res
}

// reboot asks the device to reboot and waits for it to come back.
//
// reboot: is a device service, so it takes two round trips: switch a
// connection to the position, then start the service on it. The phases are
// opened separately rather than through [adbwire.Client.OpenService] because
// only the second one can fail "on cue" — and the difference is not something
// the resulting error can be trusted to carry. A socket severed during the
// transport switch is a *TransportError exactly like one severed after the
// request, and reading both as "the device is going down now" makes this rung
// sit out its whole action budget waiting for a reboot the server never heard
// of, then report whatever the position happened to say. If that position was
// usable all along, the row claims a recovery this rung did not cause — the one
// lie this file must not tell, because it closes an incident nobody fixed.
func (a *ADBActuator) reboot(r *rung) Result {
	c, res, ok := a.endpointClient(r)
	if !ok {
		return res
	}

	tr, err := c.Transport(r.ctx, r.act.Devpath)
	if err != nil {
		d, reason := a.classifyWire(r, "reboot: via host:transport:", err)
		r.log.Log(r.ctx, levelFor(d), "device reboot was not issued",
			"phase", "transport", "disposition", string(d), "err", err)
		return r.answer(d, reason, map[string]any{
			"phase": "transport", "error": err.Error(),
		})
	}
	// One closer for the socket: Transport.Close is documented to be safe after
	// the stream has taken it, and both act on the same connection.
	defer func() { _ = tr.Close() }()

	// Past this point the request is on the wire, so a transport failure is the
	// device going down on cue and not a fault.
	if _, err := tr.Service(r.ctx, "reboot:"); err != nil && !adbwire.IsTransport(err) {
		d, reason := a.classifyWire(r, "reboot:", err)
		r.log.Log(r.ctx, levelFor(d), "device reboot was not issued",
			"phase", "service", "disposition", string(d), "err", err)
		return r.answer(d, reason, map[string]any{
			"phase": "service", "error": err.Error(),
		})
	}

	return a.confirm(r, c, "device_reboot", 0, false, map[string]any{"rebooted": true})
}

// restartServer kills the host's ADB server. Tier 7 exists because sometimes
// the server is the broken thing, but it severs EVERY device on the host, so
// the ladder only reaches it after the blast-radius check against live leases.
func (a *ADBActuator) restartServer(r *rung) Result {
	c, res, ok := a.endpointClient(r)
	if !ok {
		return res
	}

	r.log.Warn("restarting the host adb server; every device on this host is severed",
		"host", r.act.HostID, "endpoint", r.act.ADBEndpoint)

	if err := c.Kill(r.ctx); err != nil {
		d, reason := a.classifyWire(r, "host:kill", err)
		r.log.Log(r.ctx, levelFor(d), "the host adb server could not be killed",
			"disposition", string(d), "err", err)
		return r.answer(d, reason, map[string]any{"error": err.Error()})
	}

	// The container's restart policy brings the server back. Transport failures
	// during THIS wait are tolerated because they are the expected state — the
	// server we just killed is supposed to be missing for a moment. If it is
	// still missing when the budget runs out, that is a host that did not come
	// back, which is unreachable and not a broken handset.
	return a.confirm(r, c, "adb_restart", 0, true,
		map[string]any{"adb_server_restarted": true})
}

// ---------------------------------------------------------------------------
// Host-local rungs
// ---------------------------------------------------------------------------

func (a *ADBActuator) usbReset(ctx context.Context, act Action) error {
	return a.HostRunner.USBReset(ctx, act.HostID, act.Devpath)
}

// portPower performs tier 4, delivering the ladder's power-domain
// acknowledgement to the agent when there is one and the agent can take it.
//
// Three shapes, and the difference between them is recorded rather than
// inferred, because "the port was not cycled" and "the port was cycled and the
// device stayed dark" send an operator to opposite ends of the rack:
//
//   - no acknowledgement: plain PortPower. Unchanged, and the agent still
//     refuses a cycle that would darken a neighbour.
//   - acknowledgement, and the runner implements DomainPowerRunner: the list
//     goes to the agent, which re-checks it against the hub as it is right now.
//   - acknowledgement, and the runner does not: the acknowledgement is DROPPED
//     rather than smuggled through, so a runner that predates this seam cannot
//     be tricked into a wider blast radius than its own signature admits. That
//     is the safe direction — the agent will very likely refuse — but a refusal
//     with no explanation is the failure mode this package exists to avoid, so
//     the detail says the acknowledgement could not be delivered and names how
//     many positions it covered.
func (a *ADBActuator) portPower(r *rung) Result {
	const what = "VBUS power cycle"

	run := func(ctx context.Context, act Action) error {
		return a.HostRunner.PortPower(ctx, act.HostID, act.Devpath)
	}
	var extra map[string]any

	if len(r.act.Acknowledged) > 0 {
		// Copy: the caller's slice outlives this call in the attempt detail,
		// and an actuator must not hand the agent an alias of it.
		ack := append([]string(nil), r.act.Acknowledged...)
		// Recorded whatever happens next, including the no-host-agent refusal
		// in hostLocal. "Which neighbours had already been cleared" is the
		// first thing asked of a rung that did not run.
		extra = map[string]any{"acknowledged": ack}

		// A nil HostRunner type-asserts to ok=false. That is not the same
		// situation as a runner that simply predates this seam, so only the
		// second gets the undeliverable note; the first is refused by hostLocal
		// with a reason of its own.
		switch dr, ok := a.HostRunner.(DomainPowerRunner); {
		case ok:
			run = func(ctx context.Context, act Action) error {
				return dr.PortPowerWithDomain(ctx, act.HostID, act.Devpath, ack)
			}
			r.log.Info("cycling VBUS with an acknowledged power domain", "acknowledged", ack)
		case a.HostRunner != nil:
			extra["acknowledgement_undeliverable"] = fmt.Sprintf(
				"the control plane cleared %d other position(s) in this "+
					"power domain, but this host runner does not implement "+
					"recovery.DomainPowerRunner, so the agent was asked to cycle the target "+
					"alone and will refuse if the domain is ganged", len(ack))
			r.log.Warn("power domain acknowledgement cannot be delivered to this host runner",
				"acknowledged", ack)
		}
	}

	return a.hostLocal(r, what, extra, run)
}

// hostLocal performs a rung that needs an agent on the device host, or refuses
// it with a reason that names what is missing.
//
// extra is merged into every Detail this call can produce, so a fact about the
// ATTEMPT — which neighbours were acknowledged — survives into
// farm.recovery_attempts.detail whatever the rung then did.
func (a *ADBActuator) hostLocal(r *rung, what string, extra map[string]any, run func(context.Context, Action) error) Result {
	// The rung's own facts win: extra describes the request, kv the result.
	detail := func(kv map[string]any) map[string]any {
		out := make(map[string]any, len(kv)+len(extra))
		for k, v := range extra {
			out[k] = v
		}
		for k, v := range kv {
			out[k] = v
		}
		return out
	}

	if a.HostRunner == nil {
		reason := fmt.Sprintf(
			"tier %d (%s) needs %s on host %s, which only a farmd-node agent can do; "+
				"no host agent is configured for this farm",
			r.act.Tier, r.act.TierName, what, r.act.HostID)
		r.log.Warn("recovery rung refused: no host agent", "what", what)
		return r.answer(DispositionRefused, reason, detail(map[string]any{"what": what}))
	}

	if err := run(r.ctx, r.act); err != nil {
		d, reason, kind := a.classifyHostFault(r, what, err)
		r.log.Log(r.ctx, levelFor(d), "host-local recovery rung did not complete",
			"what", what, "disposition", string(d), "refusal_kind", kind, "err", err)
		kv := map[string]any{"what": what, "error": err.Error()}
		if kind != "" {
			kv[DetailRefusalKind] = kind
		}
		return r.answer(d, reason, detail(kv))
	}

	// The agent reporting success is the agent's half of the story. Whether the
	// handset re-enumerated is the half the ladder needs, and only the ADB
	// server can answer it.
	c, res, ok := a.endpointClient(r)
	if !ok {
		// The rung ran. Say so, and say why it cannot be confirmed, rather than
		// inventing either a recovery or a hardware failure out of a missing
		// endpoint — and do not leave the refusal template's "the rung was not
		// performed" standing over an agent call that just power-cycled a port.
		res.Detail = detail(res.Detail)
		res.Detail["what"] = what
		res.Detail["rung_ran"] = true
		res.Detail[DetailRefusal] = fmt.Sprintf(
			"%s; the farmd-node agent on host %s did perform %s first, so the device was "+
				"touched and whether it came back cannot be established from here",
			RefusalOf(res), r.act.HostID, what)
		return res
	}
	return a.confirm(r, c, what, 0, false, detail(map[string]any{"what": what}))
}

// ---------------------------------------------------------------------------
// Confirmation
// ---------------------------------------------------------------------------

// observation is what one settle window saw.
type observation struct {
	// state is the last state successfully observed, StateUnknown when none was.
	state adbwire.ConnState
	// usable is true only when a read came back "device".
	usable bool
	// reads counts observations that actually happened, so "the device is
	// offline" can be told from "nothing answered".
	reads int
	// probes counts state reads that were ATTEMPTED, which is what separates
	// "the host said nothing" from "we never asked". Only the first is a fact
	// about the host, and only the first may be recorded as one.
	probes int
	// wire is the last read error, nil when the last read worked.
	wire error
}

// confirm waits for the device to come back and turns what it saw into the
// rung's answer. window bounds the wait; zero means the whole action budget.
//
// This is the only place in this file that may return DispositionRecovered,
// and it does so only when a state read came back "device".
func (a *ADBActuator) confirm(r *rung, c *adbwire.Client, what string, window time.Duration, tolerateServerDown bool, detail map[string]any) Result {
	obs := a.settle(r, c, window, tolerateServerDown)
	if obs.state != "" {
		detail[DetailConfirmedState] = string(obs.state)
	}
	detail["state_reads"] = obs.reads
	detail["state_probes"] = obs.probes
	if obs.wire != nil {
		// Carried whatever the verdict turns out to be. The branches below
		// disagree about what the last read error MEANS, and none of them is
		// entitled to drop it: an operator reading the row at 3am needs the
		// error even when this rung decided not to draw a conclusion from it.
		detail["error"] = obs.wire.Error()
	}

	switch {
	case obs.usable:
		r.log.Info("recovery rung restored the device", "what", what)
		return r.answer(DispositionRecovered, "", detail)

	case r.aborted():
		// Checked BEFORE the wire error, not after. When the loop is shutting
		// down, every read still in flight fails, and the last one to fail
		// would otherwise be written up as a host that stopped answering. This
		// process going away is a fact about this process; the error itself is
		// still in the detail for anyone who wants to judge it.
		return r.answer(DispositionAborted, "", detail)

	case obs.wire != nil && (adbwire.IsTransport(obs.wire) || obs.reads == 0):
		// A transport error is the news whatever came before it — the server
		// dying after two good reads is what the row is about. Any OTHER kind
		// of read error only wins when nothing readable ever came back, because
		// then it is all the evidence there is, and the silence the branches
		// below would infer from it is not something anybody observed.
		d, reason := a.classifyWire(r, "get-state", obs.wire)
		r.log.Log(r.ctx, levelFor(d), "recovery rung could not be confirmed",
			"what", what, "disposition", string(d), "err", obs.wire)
		return r.answer(d, reason, detail)

	case obs.probes == 0:
		// Not one read was even attempted: the action budget was already spent
		// when the verb returned. That is a statement about how long the host
		// took to get here, and none at all about the device — so it must not
		// be recorded as the host falling silent, which is what the branch
		// below would say about a server nobody spoke to.
		reason := fmt.Sprintf(
			"tier %d (%s) ran on host %s but its %s action budget was gone before a single "+
				"state read could be attempted, so whether the device came back is unknown",
			r.act.Tier, r.act.TierName, r.act.HostID, r.budget)
		return r.answer(DispositionUnreachable, reason, detail)

	case obs.reads == 0:
		// Reads were attempted, none came back readable, and none came back as
		// an error either — a server that accepts connections and then says
		// nothing until the budget runs out. Nothing was learned about the
		// device, so nothing about the device may be recorded.
		reason := fmt.Sprintf(
			"tier %d (%s) ran on host %s but the adb server at %s never answered a state read "+
				"for %s (%d attempted), so whether the device came back is unknown",
			r.act.Tier, r.act.TierName, r.act.HostID, r.act.ADBEndpoint, r.act.Devpath, obs.probes)
		return r.answer(DispositionUnreachable, reason, detail)

	default:
		return r.answer(DispositionNoChange, "", detail)
	}
}

// settle polls the position until it reports "device" or the window closes.
//
// The first probe is deliberately delayed by one interval. A reboot the device
// has not acted on yet still answers "device", and a confirmation taken inside
// that window would record a recovery that never happened — which is the one
// lie this actuator must not tell, because it suppresses the page that should
// have followed.
func (a *ADBActuator) settle(r *rung, c *adbwire.Client, window time.Duration, tolerateServerDown bool) observation {
	ctx := r.ctx
	if window > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, window)
		defer cancel()
	}

	obs := observation{state: adbwire.StateUnknown}

	interval := a.settleInterval()
	// The interval and the window are both local configuration, and nothing
	// stops the first from being longer than the second — the shipped
	// [DefaultControlConfirm] is only five of the default intervals wide, so one
	// retuning line is all it takes. A window that closes before the first tick
	// takes no reads, and "no reads" is one branch away from reporting that the
	// host went silent: this loop's own clock, written into the record as an
	// outage, on a rack where nothing is wrong. Shrink the interval instead, so
	// there is always time to ask at least once.
	if dl, ok := ctx.Deadline(); ok {
		if budget := time.Until(dl); budget < 2*interval {
			interval = budget / 2
		}
	}
	if interval <= 0 {
		// The budget was gone before this function was entered. Probing with a
		// deadline already in the past would only manufacture a timeout to
		// misread; confirm reports "never asked" from probes == 0 instead.
		return obs
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return obs
		case <-t.C:
		}

		obs.probes++
		st, err := c.State(ctx, r.act.Devpath)
		if err != nil && windowClosed(ctx) {
			// The window closed underneath the probe. That is this rung's own
			// budget elapsing, and a read cut short by our own deadline is
			// evidence about nothing: recording it as a transport failure would
			// report a perfectly healthy host as unreachable for no reason
			// other than the clock. What the earlier probes saw still stands.
			return obs
		}
		switch {
		case err == nil:
			obs.state, obs.wire, obs.reads = st, nil, obs.reads+1
			if st == adbwire.StateDevice {
				obs.usable = true
				return obs
			}

		case adbwire.IsNotFound(err):
			// A position with no transport is an answer about the device, not a
			// failure to ask: the server is up and says nothing is plugged in
			// there. That is a re-enumeration that has not finished, or a
			// handset that did not come back.
			obs.state, obs.wire, obs.reads = adbwire.StateAbsent, nil, obs.reads+1

		case adbwire.IsTransport(err):
			obs.wire = err
			if !tolerateServerDown {
				// No rung will help while the server is gone, and polling a
				// dead host to the end of the budget only delays every other
				// device the loop has to get to.
				return obs
			}

		default:
			obs.wire = err
		}
	}
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

// endpointClient resolves the host's ADB client, or explains why it cannot.
//
// A host with no adb_endpoint is a configuration gap, not a broken handset:
// there is nowhere to send the rung, so the rung is refused and the device is
// left exactly as it was.
func (a *ADBActuator) endpointClient(r *rung) (*adbwire.Client, Result, bool) {
	c, err := a.client(r.act.ADBEndpoint)
	if err != nil {
		reason := fmt.Sprintf(
			"tier %d (%s) needs an adb server for host %s and there is none: %v",
			r.act.Tier, r.act.TierName, r.act.HostID, err)
		r.log.Warn("recovery rung refused: no adb endpoint", "err", err)
		return nil, r.answer(DispositionRefused, reason, map[string]any{"error": err.Error()}), false
	}
	return c, Result{}, true
}

// classifyWire turns an adbwire error into one of the three answers.
//
// The order is adbwire's own doctrine: a *TransportError is never
// cancellation, however it unwraps, because a dial bounded by a fallback
// timeout wraps context.DeadlineExceeded and reporting an unreachable host as
// an orderly shutdown hides exactly the outage an operator is looking for.
func (a *ADBActuator) classifyWire(r *rung, op string, err error) (Disposition, string) {
	if te, ok := adbwire.AsTransport(err); ok {
		return DispositionUnreachable, fmt.Sprintf(
			"tier %d (%s) could not reach the adb server for host %s at %s: %s during %q (%v); "+
				"no rung on this host will help until that server answers again",
			r.act.Tier, r.act.TierName, r.act.HostID, r.act.ADBEndpoint,
			te.Kind, te.Op, te.Err)
	}

	var amb *adbwire.AmbiguousTargetError
	if errors.As(err, &amb) {
		return DispositionRefused, fmt.Sprintf(
			"tier %d (%s) was not performed: devpath %s matches more than one transport on "+
				"host %s, so the adb server's view of the usb topology is wrong and this rung "+
				"could land on a device that is working (%s)",
			r.act.Tier, r.act.TierName, r.act.Devpath, r.act.HostID, amb.Reason)
	}

	var use *adbwire.UsageError
	if errors.As(err, &use) {
		return DispositionRefused, fmt.Sprintf(
			"tier %d (%s) was not attempted: %v; nothing addressed by a devpath this "+
				"malformed is safe to send",
			r.act.Tier, r.act.TierName, err)
	}

	if adbwire.IsNotFound(err) {
		// The server is up and has no transport at that position. That is the
		// fault the ladder is climbing towards, so it escalates.
		return DispositionFailed, ""
	}

	var pe *adbwire.ProtocolError
	if errors.As(err, &pe) {
		if deviceStateRefusal(pe.Reason) {
			// The server refused because of the device's own condition. That is
			// the fault itself, not a reason to stop climbing.
			return DispositionFailed, ""
		}
		return DispositionRefused, fmt.Sprintf(
			"tier %d (%s) was refused by the adb server on host %s for %q: %s "+
				"(a server build that does not implement the verb refuses it exactly like this)",
			r.act.Tier, r.act.TierName, r.act.HostID, op, pe.Reason)
	}

	if adbwire.IsCanceled(err) {
		if r.aborted() {
			return DispositionAborted, ""
		}
		// Our own action budget, not the loop's shutdown: the host took longer
		// than the rung was given. That is a statement about the host.
		return DispositionUnreachable, fmt.Sprintf(
			"tier %d (%s) ran out of its action budget waiting for the adb server for "+
				"host %s at %s to answer %q",
			r.act.Tier, r.act.TierName, r.act.HostID, r.act.ADBEndpoint, op)
	}

	return DispositionFailed, ""
}

// classifyHostFault turns a [HostRunner] error into one of the three answers,
// plus the refusal's kind when the error names one.
//
// It is [ClassifyHostFault] with the rung's identity bound in. The
// classification itself lives there and not here because the operator's slot
// power route calls a HostRunner too, and two copies of this decision drift:
// one files an unreachable host as a failed rung, the other files a caller's
// own budget as the host's fault, and the same wire answer ends up meaning two
// different things in the same column. TestClassifyHostFaultMatchesTheActuator
// holds the two to one answer; delegating is what makes that free.
func (a *ADBActuator) classifyHostFault(r *rung, what string, err error) (Disposition, string, string) {
	f := ClassifyHostFault(err, r.aborted())
	return f.Disposition, f.Reason(r.act.Tier, r.act.TierName, what, r.act.HostID), f.RefusalKind
}

// windowClosed reports whether ctx has run out, including the moment where its
// deadline has passed but the context has not been marked done yet.
//
// The socket carries the same deadline as the context, and the kernel reaches
// it first: a read that expires at the deadline returns "i/o timeout" a few
// microseconds before the context's own timer fires. Asking only ctx.Err() in
// that window would file our own budget elapsing as a host that stopped
// answering, which is the one claim this actuator must not make cheaply.
func windowClosed(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}

// deviceStateRefusal reports whether the ADB server's refusal is about the
// device's own condition rather than about the rung.
//
// "device offline" is the fault the ladder is climbing towards, and recording
// it as a refusal would stop the climb at the exact moment it should continue.
func deviceStateRefusal(reason string) bool {
	low := strings.ToLower(reason)
	return strings.Contains(low, "offline") ||
		strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "permissions")
}

// levelFor keeps an unreachable host at warning level and a refused rung at
// info: an operator scanning for warnings should find hosts that need looking
// at, not rungs that were correctly declined.
func levelFor(d Disposition) slog.Level {
	switch d {
	case DispositionUnreachable, DispositionFailed:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
