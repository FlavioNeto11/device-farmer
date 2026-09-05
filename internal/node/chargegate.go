// Charge gating — a VBUS SET-POINT the agent holds, with a dead-man's switch.
//
// # Why this is not the power cycle
//
// [Agent.PortPowerWithDomain] is recovery tier 4, and the shape of that rung is
// deliberate: uhubctl.go's portPower arms a deferred power-on BEFORE it cuts
// anything, so the only way out of that function — success, error, panic,
// cancelled context, expired deadline — is through an attempt to give the port
// its power back. That is exactly right for recovery, where a port left dark is
// a device removed from the farm until a human walks to the rack.
//
// It is exactly wrong for charge limiting. Holding a phone below a charge
// ceiling means keeping VBUS off for minutes or hours, on purpose, and a verb
// whose only exit is a power-on cannot express that. So this file adds a
// different verb rather than weakening the cycle: set the port's power to a
// stated value and HOLD it there. Nothing here changes tier 4.
//
// # Everything the cycle refuses, the gate refuses harder
//
// A gate inherits every guard the cycle has, for stronger reasons:
//
//   - checkKernelFloor. Before Linux 6.0 the USB core silently re-powers a port
//     switched off behind its back. A cycle on such a kernel is a lie that
//     lasts three seconds; a GATE on such a kernel is a lie that lasts hours,
//     and the control plane would spend that whole time believing it was
//     protecting a battery it was actually charging to 100%.
//
//   - checkBlastRadius, evaluated against LIVE sysfs rather than the database,
//     because the agent is the only party that can see what is plugged into the
//     hub at this instant. On a hub without per-port switching (no "ppps" in
//     the descriptor) holding one port dark holds every port dark — and unlike
//     a cycle, it holds them dark indefinitely. A gate is strictly more
//     disruptive than a cycle, so it is refused under strictly the same rule.
//
//   - the host check. A devpath crossed a network to get here, and "usb:3-1.4"
//     is a real port on every host in the fleet. A misrouted gate does not
//     merely reset the wrong rack, it starves the wrong rack.
//
// # The dead-man's switch, and why it points at ON
//
// A gate is a continuous assertion, and a continuous assertion needs a live
// asserter. If the control plane crashes, is redeployed, or is partitioned away
// while a port is held dark, nothing is left to release it: the phone
// discharges to zero and drops out of the farm. That failure is worse than the
// one gating exists to prevent — an over-charged battery ages, a flat one is
// gone today, mid-job.
//
// So every off-gate carries a deadline the AGENT enforces locally, out of its
// own clock, and the control plane keeps the gate alive by re-asserting it
// before that deadline. When nothing renews, the agent restores power itself
// and says so loudly. The fail-safe direction is ON, always:
//
//	charging a phone that should not be charging   slow, reversible, cosmetic
//	a phone held dark that nobody meant to hold    immediate, unrecoverable
//
// [MaxChargeGateHold] caps a single assertion so that a caller cannot recreate
// the very failure the deadline exists to prevent by asking for "off for three
// days". Policy that wants hours renews; the renewal IS the proof that the
// policy is still alive.
//
// Two limits are honest to state. First, the hold lives in this process's
// memory, so a hard kill of the agent (SIGKILL, or a crash) leaves the port
// dark with nothing to expire it — which is why [Agent.chargeGateLoop] releases
// every gate on a clean shutdown, and why a control plane must treat a node
// restart as "my gates are gone" and re-assert them. A host that lost power
// lost its hub's power too and comes back charging, so the residue is narrow:
// the agent killed while the host stays up. The recovery is the same sentence
// portPower prints — `uhubctl -l <hub> -p <port> -a on`.
//
// Second, on a ganged hub the gate is the whole power domain; that is what
// checkBlastRadius makes the caller acknowledge, and it means releasing any one
// port in that domain releases all of them.
//
// This file is the MECHANISM. Nothing here decides when a device should be
// gated — no battery threshold, no lease policy, no schedule. That belongs to
// the control plane, and this agent will not be the component that quietly
// invents a charging policy of its own.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The power states, spelled exactly as farm.device_runtime.charge_gate spells
// them, so a value read off this endpoint can be written to that column without
// a translation table nobody maintains.
const (
	// ChargePowerOn means VBUS is on: the device charges and enumerates.
	ChargePowerOn = "on"
	// ChargePowerOff means VBUS is held off: the device neither charges nor
	// appears on the bus.
	ChargePowerOff = "off"
	// ChargePowerUnknown is what a gate reports after the agent tried to
	// restore power and could not. It is not a state anybody asks for; it is
	// the agent declining to claim a state it cannot prove.
	ChargePowerUnknown = "unknown"
)

const (
	// MaxChargeGateHold is the longest one assertion may keep a port dark.
	//
	// It is a ceiling on TRUST, not on gating: a policy that wants to hold a
	// phone at 60% all weekend re-asserts every few minutes for as long as it
	// is alive. The cap is what bounds the damage when it stops being alive —
	// after at most this long with nobody renewing, every gated phone starts
	// charging again. Half an hour of unwanted charging is a rounding error
	// against a battery's life; half a day of unwanted discharging is a dead
	// device and a failed job.
	MaxChargeGateHold = 30 * time.Minute

	// chargeGateRestoreRetry is how long the agent waits before trying again
	// when restoring power to an expired gate failed. The hold is kept, not
	// dropped: dropping it would leave a possibly dark port with nothing left
	// in this process that intends to fix it.
	chargeGateRestoreRetry = 30 * time.Second

	// chargeGateReleaseBudget bounds the power-on of ONE port. It covers the
	// platform's power-on retries and the spacing between them — the same
	// reasoning as uhubctl.go's restoreTimeout, which this file cannot name
	// because that constant lives behind a Linux build tag.
	//
	// It is spent per port and never shared. A host holding twenty gates that
	// divided one budget between them would give the first few ports a real
	// attempt and hand the rest an already-cancelled context, which is the
	// difference between "nineteen phones came back" and "three did".
	chargeGateReleaseBudget = 20 * time.Second

	// chargeGateShutdownBudget caps the whole release on the way out.
	//
	// Shutdown restores are a race against a service manager: systemd's
	// default TimeoutStopSec is 90s, after which the process is killed and
	// every remaining port stays dark. Stopping at a minute leaves room for
	// the rest of Run's shutdown and guarantees the ports that did not get
	// their turn are NAMED in the log rather than lost in a SIGKILL.
	chargeGateShutdownBudget = 60 * time.Second

	// maxChargeGateBody bounds a request body. A gate request is a few hundred
	// bytes plus an acknowledged list; anything larger is either a mistake or
	// an attempt to make the process holding the USB bus the memory problem.
	maxChargeGateBody = 64 << 10
)

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// ChargeGateRequest asks the agent to drive one port's VBUS to a set-point.
//
// Power is a STRING and not a bool on purpose. A missing, misspelled or null
// JSON boolean decodes to false, and false in a power field means "cut power to
// this port" — the one value that must never be reachable by accident. An empty
// or unrecognised string is refused with the reason instead.
type ChargeGateRequest struct {
	// HostID is the farm.hosts.id this request is meant for. Required on the
	// HTTP surface; see [Agent.checkHost].
	HostID string `json:"host_id"`

	// Devpath is the USB position ("usb:3-1.4.2"), never a serial. Duplicate
	// OEM serials are real and a serial names no port.
	Devpath string `json:"devpath"`

	// Power is [ChargePowerOff] or [ChargePowerOn].
	Power string `json:"power"`

	// HoldSeconds is how long an off-gate stays asserted without a renewal.
	// Required for "off", ignored for "on", and capped at [MaxChargeGateHold].
	// Fractional values are accepted; they are mostly useful in tests.
	HoldSeconds float64 `json:"hold_seconds,omitempty"`

	// Acknowledged lists the other devpaths in the power domain whose lease
	// policy the caller has checked and is willing to disturb. On a ganged hub
	// this is not optional bookkeeping: without it the agent refuses.
	Acknowledged []string `json:"acknowledged,omitempty"`

	// Reason is free text carried into the agent's logs and back out on the
	// status, so an operator staring at a dark port can read why it is dark.
	Reason string `json:"reason,omitempty"`
}

// ChargeGate is what the agent is currently doing to one port.
type ChargeGate struct {
	HostID  string `json:"host_id"`
	Devpath string `json:"devpath"`
	Hub     string `json:"hub,omitempty"`
	Port    int    `json:"port,omitempty"`

	// Power is the set-point the agent last drove and believes is in force.
	Power string `json:"power"`

	// Held is true while a deadline is armed for this port. A released port is
	// reported once, with Held false, and then forgotten.
	Held bool `json:"held"`

	// ExpiresAt is when the agent will restore power unless the gate is
	// re-asserted before then. Zero when nothing is held.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	Reason       string   `json:"reason,omitempty"`
	Acknowledged []string `json:"acknowledged,omitempty"`
}

// chargeGateList is the body of GET /node/v1/charge-gate.
type chargeGateList struct {
	HostID string       `json:"host_id"`
	Gates  []ChargeGate `json:"gates"`
}

// hold converts HoldSeconds to a Duration, refusing anything above the cap
// before the multiplication rather than after it: hold_seconds of 1e300 is
// valid JSON and would otherwise overflow into a negative Duration.
func (r ChargeGateRequest) hold() (time.Duration, error) {
	if r.HoldSeconds > MaxChargeGateHold.Seconds() {
		return 0, fmt.Errorf("node: %w: a charge gate may be asserted for at most %s at a "+
			"time and this request asked for %.0fs; hold a port dark for longer by "+
			"re-asserting the gate before it expires, which is what proves to this agent "+
			"that whoever asked for it is still running",
			ErrRefused, MaxChargeGateHold, r.HoldSeconds)
	}
	if r.HoldSeconds <= 0 {
		return 0, nil
	}
	return time.Duration(r.HoldSeconds * float64(time.Second)), nil
}

// parseChargePower turns the wire value into a direction. Anything that is not
// one of the two spellings is refused rather than coerced.
func parseChargePower(power string) (on bool, err error) {
	switch power {
	case ChargePowerOn:
		return true, nil
	case ChargePowerOff:
		return false, nil
	default:
		return false, fmt.Errorf("node: %w: power must be %q or %q, and %q is neither; "+
			"this endpoint will not guess a direction, because one of the two answers "+
			"cuts power to a port that may be holding a live lease",
			ErrRefused, ChargePowerOff, ChargePowerOn, power)
	}
}

// ---------------------------------------------------------------------------
// The platform seam
// ---------------------------------------------------------------------------

// chargePort is one hub port, resolved from a devpath. It is the address the
// gate is keyed by: a hub location and a port number, which is what uhubctl
// switches and therefore what a set-point actually applies to.
type chargePort struct {
	Devpath string
	USBPath string
	Hub     string
	Port    int
}

func (p chargePort) key() string { return p.Hub + "/" + strconv.Itoa(p.Port) }

// chargeGateOps is the hardware half of this file. It is a seam of its own
// rather than two more methods on platformOps so that the Linux implementation
// lives beside the gate it serves and neither file has to be edited to change
// the other.
type chargeGateOps interface {
	// resolveChargePort validates a devpath and names the hub port behind it.
	// It touches no hardware.
	resolveChargePort(devpath string) (chargePort, error)

	// setChargePower drives VBUS to on/off and returns whether it got as far
	// as issuing a power-changing command.
	//
	// touched is the contract the bookkeeping depends on. false means nothing
	// was switched — a refusal, an unsupported host, a hub that could not be
	// read — and the port is in exactly the state it was in before the call.
	// true means a command was issued; for a failed off, the implementation has
	// already attempted to put power back, the same way portPower does.
	setChargePower(ctx context.Context, p chargePort, on bool, acknowledged []string, o opsConfig) (touched bool, err error)
}

// chargePlatform is replaced by chargegate_linux.go's init on Linux. Everywhere
// else it stays this value, so the package builds and tests on a laptop and
// every gate request there is answered with a refusal that names the platform
// rather than a success nobody performed.
var chargePlatform chargeGateOps = unsupportedChargeGate{}

type unsupportedChargeGate struct{}

func (unsupportedChargeGate) resolveChargePort(string) (chargePort, error) {
	return chargePort{}, fmt.Errorf("node: %w: %s", ErrNotSupported, chargeGateUnsupported())
}

func (unsupportedChargeGate) setChargePower(context.Context, chargePort, bool, []string, opsConfig) (bool, error) {
	return false, fmt.Errorf("node: %w: %s", ErrNotSupported, chargeGateUnsupported())
}

func chargeGateUnsupported() string {
	return "holding a port's VBUS at a set-point needs uhubctl and Linux sysfs, and this " +
		"agent is built for " + runtime.GOOS + "/" + runtime.GOARCH +
		"; charge gating cannot be performed here"
}

// ---------------------------------------------------------------------------
// The holds
// ---------------------------------------------------------------------------

// heldGate is one port this agent is keeping dark, and the deadline after which
// it stops.
type heldGate struct {
	port         chargePort
	power        string
	reason       string
	acknowledged []string
	expires      time.Time
	// gen distinguishes this assertion from the one that replaced it, so a
	// timer that fired while a renewal was in flight can recognise itself as
	// stale and do nothing.
	gen   uint64
	timer *time.Timer
}

func (h *heldGate) status(hostID string) ChargeGate {
	return ChargeGate{
		HostID: hostID, Devpath: h.port.Devpath, Hub: h.port.Hub, Port: h.port.Port,
		Power: h.power, Held: true, ExpiresAt: h.expires,
		Reason: h.reason, Acknowledged: h.acknowledged,
	}
}

// chargeGateState is one agent's gates.
type chargeGateState struct {
	// hw serialises every hardware action THIS FILE performs. Two uhubctl
	// invocations racing on the same hub is a fight the hub arbitrates and
	// nobody can read afterwards; gating happens a few times a minute at most,
	// so being boring here costs nothing. Lock order is always hw then mu.
	//
	// It does not reach tier 3 or tier 4 — a recovery cycle takes no lock here
	// and is free to switch a gated port. That is the right precedence (a
	// device being repaired under a live lease outranks a battery
	// optimisation), and see [Agent.chargeGateSuperseded] for how the gate
	// stops claiming a port that recovery has taken back.
	hw sync.Mutex

	// hostID labels this state's metrics. A gauge shared across agents would
	// report whichever one wrote last.
	hostID string

	mu   sync.Mutex
	held map[string]*heldGate
	gen  uint64
	// stopping is set once the agent has begun releasing gates on the way out.
	// From that moment a new off-gate is refused: nothing would survive this
	// process to expire it, and a port left dark by an agent that is exiting is
	// the exact failure the deadline exists to prevent.
	stopping bool
}

func (s *chargeGateState) setGauge() {
	chargeGatesHeld.WithLabelValues(s.hostID).Set(float64(len(s.held)))
}

// Gate state is held per agent in a package-level registry rather than in a
// field on Agent, so that adding this verb does not reshape the struct every
// other loop in the package shares.
//
// Only asserting a gate creates an entry; the read paths look it up without
// creating one, so an agent that never gates anything never appears here. An
// entry outlives the agent's gate loop on purpose — it carries the stopping
// flag that keeps a late request from asserting a gate nothing will expire —
// and is released with the process.
var (
	chargeGatesMu      sync.Mutex
	chargeGatesByAgent = map[*Agent]*chargeGateState{}
)

func (a *Agent) chargeGates() *chargeGateState {
	chargeGatesMu.Lock()
	defer chargeGatesMu.Unlock()
	s := chargeGatesByAgent[a]
	if s == nil {
		s = &chargeGateState{hostID: a.cfg.HostID, held: make(map[string]*heldGate)}
		chargeGatesByAgent[a] = s
	}
	return s
}

// chargeGatesIfAny is the read path: it never creates state, so listing or
// releasing gates on an agent that has none leaves nothing behind.
func (a *Agent) chargeGatesIfAny() *chargeGateState {
	chargeGatesMu.Lock()
	defer chargeGatesMu.Unlock()
	return chargeGatesByAgent[a]
}

func forgetChargeGates(a *Agent) {
	chargeGatesMu.Lock()
	defer chargeGatesMu.Unlock()
	delete(chargeGatesByAgent, a)
}

// put installs or refreshes a hold and arms its dead-man's switch. The caller
// holds s.hw.
func (s *chargeGateState) put(a *Agent, port chargePort, req ChargeGateRequest, hold time.Duration) ChargeGate {
	key := port.key()

	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.held[key]; ok && old.timer != nil {
		old.timer.Stop()
	}
	s.gen++
	gen := s.gen
	h := &heldGate{
		port:         port,
		power:        ChargePowerOff,
		reason:       req.Reason,
		acknowledged: append([]string(nil), req.Acknowledged...),
		expires:      time.Now().Add(hold),
		gen:          gen,
	}
	// The timer is armed under the same lock that publishes the hold so the
	// two can never disagree about which generation is current. Its callback
	// takes s.hw first, which the caller of put is holding, so a hold short
	// enough to fire immediately simply waits its turn.
	h.timer = time.AfterFunc(hold, func() { s.expire(a, key, gen) })
	s.held[key] = h
	s.setGauge()
	return h.status(s.hostID)
}

// drop forgets a hold and disarms it.
func (s *chargeGateState) drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.held[key]
	if !ok {
		return
	}
	if h.timer != nil {
		h.timer.Stop()
	}
	delete(s.held, key)
	s.setGauge()
}

func (s *chargeGateState) snapshot() []ChargeGate {
	s.mu.Lock()
	out := make([]ChargeGate, 0, len(s.held))
	for _, h := range s.held {
		out = append(out, h.status(s.hostID))
	}
	s.mu.Unlock()
	// Sorted so two reads of an unchanged fleet compare equal, which is what
	// makes this endpoint usable for reconciliation after a control-plane
	// restart.
	sort.Slice(out, func(i, j int) bool { return out[i].Devpath < out[j].Devpath })
	return out
}

// expire is the dead-man's switch firing: nobody renewed, so the port gets its
// power back.
func (s *chargeGateState) expire(a *Agent, key string, gen uint64) {
	s.hw.Lock()
	defer s.hw.Unlock()

	s.mu.Lock()
	h, ok := s.held[key]
	// A stale generation means a renewal replaced this assertion while the
	// timer was in flight; a deadline still in the future means the same thing
	// and the newer timer already covers it.
	if !ok || h.gen != gen || time.Now().Before(h.expires) {
		s.mu.Unlock()
		return
	}
	port, deadline := h.port, h.expires
	s.mu.Unlock()

	a.log.Warn("a charge gate expired without being renewed; restoring VBUS. Whatever "+
		"was holding this port dark has stopped renewing it, and a port held dark by a "+
		"control plane that is no longer running is a phone that discharges to nothing",
		"devpath", port.Devpath, "hub", port.Hub, "port", port.Port, "deadline", deadline)
	chargeGateExpiries.Inc()

	// Detached from everything: whatever stopped renewing this gate is not a
	// reason to leave the port dark.
	ctx, cancel := context.WithTimeout(context.Background(), chargeGateReleaseBudget)
	defer cancel()

	_, err := chargePlatform.setChargePower(ctx, port, true, nil, a.ops)
	chargeGateSets.WithLabelValues(ChargePowerOn, outcomeLabel(err)).Inc()
	if err != nil {
		a.log.Error("THE PORT IS PROBABLY LEFT WITHOUT POWER: an expired charge gate could "+
			"not be released, so the device behind it is unreachable and not charging; "+
			"the agent will keep trying, and a human can settle it now with "+
			"`uhubctl -l <hub> -p <port> -a on` on this host",
			"devpath", port.Devpath, "hub", port.Hub, "port", port.Port,
			"retry_in", chargeGateRestoreRetry, "err", err)
		s.rearm(a, key, gen, chargeGateRestoreRetry)
		return
	}
	a.log.Info("charge gate released; VBUS restored",
		"devpath", port.Devpath, "hub", port.Hub, "port", port.Port)
	s.drop(key)
}

// rearm keeps a hold whose release failed, marks its power unknown — the agent
// no longer knows what that port is doing — and schedules another attempt.
func (s *chargeGateState) rearm(a *Agent, key string, gen uint64, in time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.held[key]
	if !ok || h.gen != gen {
		return
	}
	s.gen++
	h.gen = s.gen
	h.power = ChargePowerUnknown
	h.expires = time.Now().Add(in)
	next := h.gen
	h.timer = time.AfterFunc(in, func() { s.expire(a, key, next) })
}

// releaseAll restores power to every gated port. It runs on the way out of the
// process: a hold is only as alive as the agent enforcing it, and an agent that
// is stopping cannot enforce anything.
//
// This is a deliberate exception to [Agent.Run]'s rule that cancellation does
// not put hardware back. A cycle that finished left the port powered; a gate
// that is still asserted is the one piece of state this agent leaves behind
// that gets worse with time.
// stopping reports whether this agent has begun releasing its gates for good.
func (s *chargeGateState) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// releaseAll restores power to every gated port.
//
// final marks the release that happens on the way out of the process, after
// which no new off-gate may be asserted. It is taken under s.hw, so a request
// already inside [Agent.SetChargeGate] finishes first and then has its gate
// released here; a request that arrives afterwards is refused rather than
// creating a hold that no timer will survive to expire.
//
// A final release is a deliberate exception to [Agent.Run]'s rule that
// cancellation does not put hardware back. A cycle that finished left the port
// powered; a gate that is still asserted is the one piece of state this agent
// leaves behind that gets worse with time.
func (s *chargeGateState) releaseAll(ctx context.Context, a *Agent, why string, final bool) {
	s.hw.Lock()
	defer s.hw.Unlock()

	s.mu.Lock()
	if final {
		s.stopping = true
	}
	ports := make([]chargePort, 0, len(s.held))
	for key, h := range s.held {
		if h.timer != nil {
			h.timer.Stop()
		}
		ports = append(ports, h.port)
		delete(s.held, key)
	}
	s.setGauge()
	s.mu.Unlock()

	if len(ports) == 0 {
		return
	}
	// Deterministic order, so the ports that ran out of budget are the same
	// ones on every restart and an operator reading two logs can compare them.
	sort.Slice(ports, func(i, j int) bool { return ports[i].Devpath < ports[j].Devpath })

	a.log.Info("releasing every charge gate", "gates", len(ports), "why", why)
	for i, port := range ports {
		if err := ctx.Err(); err != nil {
			// Out of overall budget. Naming what is left is the whole point:
			// these are the ports a human has to go and switch on.
			a.log.Error("THE FOLLOWING PORTS ARE PROBABLY LEFT WITHOUT POWER: the charge "+
				"gate release ran out of time before reaching them, and no timer will "+
				"survive this process to try again; a human must run "+
				"`uhubctl -l <hub> -p <port> -a on` on this host for each",
				"remaining", chargePortNames(ports[i:]), "err", err)
			return
		}
		// One budget per port. See chargeGateReleaseBudget.
		pctx, cancel := context.WithTimeout(ctx, chargeGateReleaseBudget)
		_, err := chargePlatform.setChargePower(pctx, port, true, nil, a.ops)
		cancel()

		chargeGateSets.WithLabelValues(ChargePowerOn, outcomeLabel(err)).Inc()
		if err != nil {
			a.log.Error("THE PORT IS PROBABLY LEFT WITHOUT POWER: a charge gate could not be "+
				"released while the agent was stopping, and no timer will survive this "+
				"process to try again; a human must run `uhubctl -l <hub> -p <port> -a on` "+
				"on this host",
				"devpath", port.Devpath, "hub", port.Hub, "port", port.Port, "err", err)
			continue
		}
		a.log.Info("charge gate released; VBUS restored",
			"devpath", port.Devpath, "hub", port.Hub, "port", port.Port)
	}
}

func chargePortNames(ports []chargePort) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, p.Devpath)
	}
	return out
}

// ---------------------------------------------------------------------------
// The verb
// ---------------------------------------------------------------------------

// SetChargeGate drives the VBUS of the port behind req.Devpath to req.Power and
// holds it there.
//
// An "off" is asserted for req.HoldSeconds and no longer: if nothing re-asserts
// it before then, this agent restores power on its own. An "on" releases any
// hold on that port immediately. Re-asserting an existing gate re-runs the full
// hardware path — including the blast-radius check against live sysfs, because
// a phone may have been plugged into the domain since the last assertion — and
// then extends the deadline.
//
// What happens when the hardware call fails is the interesting half:
//
//   - An off refused, or unsupported, with nothing switched: any hold already
//     in force is left exactly as it was, deadline and all. Dropping it here
//     would disarm the only thing that will ever turn that port back on, and a
//     renewal refused because a neighbour appeared in a ganged domain is
//     precisely the moment that matters.
//   - An off attempted and failed: the platform has already tried to restore
//     power, so the hold is dropped — this agent will not claim to be holding a
//     port it just handed back.
//   - An on that failed: the hold is KEPT, with its original deadline. Its
//     dead-man's switch is now the retry, which is better than a released
//     gate whose port may still be dark and which nothing is watching.
func (a *Agent) SetChargeGate(ctx context.Context, req ChargeGateRequest) (ChargeGate, error) {
	// The metric's power label is only ever "on", "off" or this one constant.
	// req.Power is attacker-controlled text until parseChargePower has vetted
	// it, and a label taken straight from a request body is an unbounded
	// cardinality bomb in the process that holds the USB bus.
	on, err := parseChargePower(req.Power)
	if err != nil {
		chargeGateSets.WithLabelValues("invalid", "refused").Inc()
		return ChargeGate{}, err
	}
	what := "charge gate " + req.Power
	if err := a.checkHost(req.HostID, what, req.Devpath); err != nil {
		chargeGateSets.WithLabelValues(req.Power, "refused").Inc()
		return ChargeGate{}, err
	}

	// Only an off-gate is bound by the hold rules, and the asymmetry matters:
	// hold_seconds means nothing to a release, and refusing a RELEASE over a
	// field it ignores would leave a port dark because the caller reused the
	// request struct it had already filled in. Every refusal in this file has
	// to fail towards power, not away from it.
	var hold time.Duration
	if !on {
		if hold, err = req.hold(); err != nil {
			chargeGateSets.WithLabelValues(req.Power, "refused").Inc()
			return ChargeGate{}, err
		}
		if hold <= 0 {
			chargeGateSets.WithLabelValues(req.Power, "refused").Inc()
			return ChargeGate{}, fmt.Errorf("node: %w: an off gate must carry hold_seconds. "+
				"There is no such thing as holding a port dark forever here: the hold is a "+
				"deadline this agent enforces out of its own clock, and it is the only thing "+
				"that turns the port back on if whoever asked for it stops running",
				ErrRefused)
		}
	}

	port, err := chargePlatform.resolveChargePort(req.Devpath)
	if err != nil {
		chargeGateSets.WithLabelValues(req.Power, outcomeLabel(err)).Inc()
		return ChargeGate{}, err
	}

	s := a.chargeGates()
	s.hw.Lock()
	defer s.hw.Unlock()

	// The agent has begun handing its ports back. A release is still welcome —
	// it is the direction we are going anyway — but a new off-gate would create
	// a hold whose only enforcer is a process that is about to exit.
	if !on && s.isStopping() {
		chargeGateSets.WithLabelValues(req.Power, "refused").Inc()
		return ChargeGate{}, fmt.Errorf("node: %w: this agent is shutting down and has "+
			"already released its charge gates; it will not hold %s dark, because nothing "+
			"would survive this process to give the port its power back",
			ErrRefused, port.Devpath)
	}

	a.log.Info("driving VBUS to a set-point", "devpath", port.Devpath, "hub", port.Hub,
		"port", port.Port, "power", req.Power, "hold", hold,
		"acknowledged", req.Acknowledged, "reason", req.Reason)

	touched, err := chargePlatform.setChargePower(ctx, port, on, req.Acknowledged, a.ops)
	chargeGateSets.WithLabelValues(req.Power, outcomeLabel(err)).Inc()
	if err != nil {
		if touched && !on {
			// The platform's own guard has put power back. Keeping a hold now
			// would advertise a gate that is not in force.
			s.drop(port.key())
		}
		a.log.Warn("the charge gate was not applied", "devpath", port.Devpath,
			"power", req.Power, "hardware_touched", touched,
			"hold_dropped", touched && !on, "err", err)
		return ChargeGate{}, err
	}

	if on {
		s.drop(port.key())
		return ChargeGate{
			HostID: a.cfg.HostID, Devpath: port.Devpath, Hub: port.Hub, Port: port.Port,
			Power: ChargePowerOn, Held: false, Reason: req.Reason,
		}, nil
	}
	return s.put(a, port, req, hold), nil
}

// ChargeGates reports every gate this agent is currently holding. It is how a
// control plane that just restarted finds out which ports it is responsible for
// renewing — and which ones it must release because it no longer wants them.
func (a *Agent) ChargeGates() []ChargeGate {
	s := a.chargeGatesIfAny()
	if s == nil {
		return nil
	}
	return s.snapshot()
}

// ReleaseChargeGates restores power to every port this agent holds. Exported
// for a control plane that wants to hand the hardware back deliberately rather
// than by dying. The agent stays willing to gate afterwards; only the shutdown
// release closes the door.
func (a *Agent) ReleaseChargeGates(ctx context.Context, why string) {
	if s := a.chargeGatesIfAny(); s != nil {
		s.releaseAll(ctx, a, why, false)
	}
}

// chargeGateSuperseded drops a gate that recovery has just taken back.
//
// Tier 4 ends with the port powered on, and tier 3 can re-enumerate a device
// the gate believed was off the bus. Neither consults this file — recovery
// under a live lease outranks a battery optimisation, and that precedence is
// correct — but the gate must then stop advertising a set-point that is no
// longer in force, or the control plane spends up to MaxChargeGateHold
// believing it is protecting a battery that is charging to 100%.
//
// A refused or unsupported rung touched nothing, so it supersedes nothing.
func (a *Agent) chargeGateSuperseded(devpath string, err error) {
	if errors.Is(err, ErrRefused) || errors.Is(err, ErrNotSupported) {
		return
	}
	s := a.chargeGatesIfAny()
	if s == nil {
		return
	}
	port, perr := chargePlatform.resolveChargePort(devpath)
	if perr != nil {
		return
	}
	s.mu.Lock()
	_, held := s.held[port.key()]
	s.mu.Unlock()
	if !held {
		return
	}
	a.log.Warn("a recovery rung took back a port this agent was holding dark; the charge "+
		"gate is dropped rather than left claiming a set-point it no longer controls",
		"devpath", port.Devpath, "hub", port.Hub, "port", port.Port)
	s.drop(port.key())
}

// chargeGateLoop owns the gates' lifetime. It does nothing until the agent is
// stopping, and then it gives every gated port its power back before Run
// returns. See releaseAll for why this is worth waiting for.
func (a *Agent) chargeGateLoop(ctx context.Context) error {
	<-ctx.Done()
	// Detached and budgeted: the cancellation that brought us here must not
	// also cancel the restores it makes necessary.
	rel, cancel := context.WithTimeout(context.WithoutCancel(ctx), chargeGateShutdownBudget)
	defer cancel()
	// chargeGates rather than chargeGatesIfAny: creating the state here is what
	// records the stopping flag, so a request that arrives during the HTTP
	// server's own shutdown grace is refused instead of installing a hold this
	// process will not be alive to expire.
	a.chargeGates().releaseAll(rel, a, "the agent is shutting down", true)
	// Never an error: a failed restore is logged at ERROR with the command a
	// human should run, and taking the process down over it would only remove
	// the one thing still able to retry.
	return nil
}

// ---------------------------------------------------------------------------
// The HTTP surface
// ---------------------------------------------------------------------------

// registerChargeGateRoutes adds the gate verb to the node endpoint. It is
// called from [Agent.Handler] with the same token digest the other routes use.
func (a *Agent) registerChargeGateRoutes(mux *http.ServeMux, want []byte) {
	mux.HandleFunc("POST /node/v1/charge-gate", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r, want) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorised"})
			return
		}
		var req ChargeGateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChargeGateBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if req.Devpath == "" {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": "devpath is required; positions, never serials"})
			return
		}
		// Same rule as the other operations: a request that crossed a network
		// must name the host it means, because "usb:3-1.4" is a real port on
		// every host in the fleet and this verb starves whichever one it hits.
		if req.HostID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "host_id is required on the node endpoint; send the farm.hosts.id " +
					"this request is meant for, because the same devpath names a different " +
					"physical port on every host"})
			return
		}
		if req.Power != ChargePowerOn && req.Power != ChargePowerOff {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("power must be %q or %q", ChargePowerOff, ChargePowerOn)})
			return
		}

		// Detached from the request socket, exactly as opHandler does it: a
		// client that hangs up mid-call does not get to leave a port dark or
		// half-switched. The deadline is this agent's own budget.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), a.opBudget())
		defer cancel()

		gate, err := a.SetChargeGate(ctx, req)
		if err != nil {
			writeJSON(w, chargeGateHTTPStatus(err), map[string]any{
				"error": err.Error(), "refused": errors.Is(err, ErrRefused)})
			return
		}
		writeJSON(w, http.StatusOK, gate)
	})

	// Reading the gates is behind the same token as setting them: the list of
	// ports this host is holding dark is a map of which phones are unreachable
	// right now, which is reconnaissance in the same sense health is.
	mux.HandleFunc("GET /node/v1/charge-gate", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r, want) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorised"})
			return
		}
		writeJSON(w, http.StatusOK, chargeGateList{
			HostID: a.cfg.HostID, Gates: a.ChargeGates(),
		})
	})
}

// chargeGateHTTPStatus follows the status vocabulary the node endpoint already
// established: 409 the agent declined, 501 this build cannot, 5xx it was
// attempted and the hardware failed.
func chargeGateHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrRefused):
		return http.StatusConflict
	case errors.Is(err, ErrNotSupported):
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	chargeGateSets = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "charge_gate_sets_total",
		Help: "VBUS set-point requests on this host, by requested power and outcome. " +
			"refused means the agent declined; unsupported means this build or this " +
			"kernel cannot hold a port dark honestly.",
	}, []string{"power", "outcome"})

	chargeGateExpiries = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "charge_gate_expiries_total",
		Help: "Charge gates this agent released because nobody renewed them. Any " +
			"sustained rate here means the control plane stopped asserting gates it " +
			"still believes it holds.",
	})

	// Labelled by host because one process may hold more than one Agent — a
	// bare gauge would report whichever agent wrote last, and one agent's
	// shutdown would zero a number another agent's dark ports are still behind.
	chargeGatesHeld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "node", Name: "charge_gates_held",
		Help: "Ports this host is currently holding without VBUS. Every one of them is " +
			"a device that is neither charging nor reachable over USB.",
	}, []string{"host"})
)

func init() {
	for _, power := range []string{ChargePowerOn, ChargePowerOff} {
		for _, outcome := range []string{"ok", "failed", "refused", "unsupported"} {
			chargeGateSets.WithLabelValues(power, outcome)
		}
	}
}
