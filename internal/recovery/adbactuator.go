package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// ADBActuator performs the recovery rungs that can be reached through a host's
// ADB server, and honestly refuses the ones that cannot.
//
// The split matters. Tiers 1, 2, 5 and 7 are ADB verbs and are performed here.
// Tiers 3 and 4 need something only a process on the host itself can do —
// USBDEVFS_RESET on a device node, or toggling VBUS through uhubctl — and this
// actuator reports them as refused with a reason naming what is missing, so
// farm.recovery_attempts.refusal records why the ladder stopped climbing.
//
// Reporting "no change" for a rung that was never attempted would be worse
// than useless: the ladder would conclude the hardware is unrecoverable and
// quarantine a device whose actual problem is that nobody is listening on the
// host.
//
// Nothing here may end a lease. Recovery acts on behalf of a holder that keeps
// its device: the lease clock keeps ticking and the fence never moves.
type ADBActuator struct {
	log *slog.Logger

	// HostRunner, when set, performs the host-local rungs that ADB cannot
	// reach. It is the seam where farmd-node plugs in. Nil means this farm has
	// no host agent, and those rungs are refused rather than faked.
	HostRunner HostRunner

	mu      sync.Mutex
	clients map[string]*adbwire.Client // keyed by ADB endpoint
}

// HostRunner is implemented by an agent running on the device host, with
// access to /dev/bus/usb and to whatever hub control the rack provides.
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
		return nil, errors.New("recovery: host has no adb_endpoint recorded")
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

// Recover performs one rung and reports what happened.
func (a *ADBActuator) Recover(ctx context.Context, act Action) (Result, error) {
	timeout := act.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log := a.log.With(
		"tier", act.Tier, "rung", act.TierName,
		"rack_slot", act.RackSlot, "devpath", act.Devpath, "host", act.HostID)

	switch act.Tier {
	case 1:
		return a.control(ctx, log, act, adbwire.ControlReconnect)

	case 2:
		// Detach then attach. A failed detach makes attach meaningless, so
		// stop rather than report a success the device never saw.
		res, err := a.control(ctx, log, act, adbwire.ControlDetach)
		if err != nil || res.Outcome == OutcomeFailed {
			return res, err
		}
		return a.control(ctx, log, act, adbwire.ControlAttach)

	case 3:
		return a.hostLocal(ctx, log, act, "USBDEVFS_RESET", nil,
			func() error { return a.HostRunner.USBReset(ctx, act.HostID, act.Devpath) })

	case 4:
		return a.portPower(ctx, log, act)

	case 5:
		return a.reboot(ctx, log, act)

	case 7:
		return a.restartServer(ctx, log, act)

	default:
		// Tiers 0, 6 and 8 are database actions the ladder performs itself.
		// Reaching here means the tier table gained a rung nobody taught this
		// actuator, which is a configuration error worth surfacing.
		return Result{
			Outcome: OutcomeRefused,
			Detail: map[string]any{
				"refusal": fmt.Sprintf(
					"tier %d (%s) is not an actuator rung; it is either a database "+
						"action performed by the ladder or an unknown tier",
					act.Tier, act.TierName),
			},
		}, nil
	}
}

// control runs one position-addressed ADB verb.
func (a *ADBActuator) control(ctx context.Context, log *slog.Logger, act Action, cmd adbwire.ControlCmd) (Result, error) {
	c, err := a.client(act.ADBEndpoint)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Detail: map[string]any{"error": err.Error()}}, nil
	}

	reply, err := c.Control(ctx, act.Devpath, cmd)
	if err != nil {
		if ctx.Err() != nil {
			return Result{Outcome: OutcomeAborted,
				Detail: map[string]any{"cmd": string(cmd), "error": err.Error()}}, nil
		}
		// A refusal is the server saying it will not; a transport failure is
		// the host being unreachable. Only the second means no rung can help.
		outcome := OutcomeFailed
		if adbwire.IsTransport(err) {
			log.Warn("recovery rung could not reach the host adb server",
				"cmd", string(cmd), "err", err)
		} else {
			log.Info("recovery rung refused by the adb server",
				"cmd", string(cmd), "err", err)
		}
		return Result{Outcome: outcome, Detail: map[string]any{
			"cmd": string(cmd), "error": err.Error(),
			"transport": adbwire.IsTransport(err),
		}}, nil
	}

	// Confirm rather than assume. The verb returning OKAY only means the
	// server accepted it; whether the device came back is a separate question
	// and is the one the ladder actually needs answered.
	state, serr := c.State(ctx, act.Devpath)
	detail := map[string]any{"cmd": string(cmd), "reply": reply}
	if serr != nil {
		detail["state_error"] = serr.Error()
		return Result{Outcome: OutcomeNoChange, Detail: detail}, nil
	}
	detail["state"] = string(state)
	if state == adbwire.StateDevice {
		log.Info("recovery rung restored the device", "cmd", string(cmd))
		return Result{Outcome: OutcomeRecovered, Detail: detail}, nil
	}
	return Result{Outcome: OutcomeNoChange, Detail: detail}, nil
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
//     the detail says the acknowledgement could not be delivered and names the
//     positions it covered.
func (a *ADBActuator) portPower(ctx context.Context, log *slog.Logger, act Action) (Result, error) {
	const what = "VBUS power cycle"

	run := func() error { return a.HostRunner.PortPower(ctx, act.HostID, act.Devpath) }
	var extra map[string]any

	if len(act.Acknowledged) > 0 {
		// Copy: the caller's slice outlives this call in the attempt detail,
		// and an actuator must not hand the agent an alias of it.
		ack := append([]string(nil), act.Acknowledged...)
		// Recorded whatever happens next, including the no-host-agent refusal
		// below. "Which neighbours had already been cleared" is the first thing
		// asked of a rung that did not run.
		extra = map[string]any{"acknowledged": ack}

		// A nil HostRunner type-asserts to ok=false. That is not the same
		// situation as a runner that simply predates this seam, so only the
		// second gets the undeliverable note; the first is refused by hostLocal
		// with a reason of its own.
		switch dr, ok := a.HostRunner.(DomainPowerRunner); {
		case ok:
			run = func() error { return dr.PortPowerWithDomain(ctx, act.HostID, act.Devpath, ack) }
			log.Info("cycling VBUS with an acknowledged power domain", "acknowledged", ack)
		case a.HostRunner != nil:
			extra["acknowledgement_undeliverable"] = fmt.Sprintf(
				"the control plane checked lease policy for %d other position(s) in this "+
					"power domain, but this host runner does not implement "+
					"recovery.DomainPowerRunner, so the agent was asked to cycle the target "+
					"alone and will refuse if the domain is ganged", len(ack))
			log.Warn("power domain acknowledgement cannot be delivered to this host runner",
				"acknowledged", ack)
		}
	}

	return a.hostLocal(ctx, log, act, what, extra, run)
}

// hostLocal performs a rung that needs an agent on the device host, or refuses
// it with a reason that names what is missing.
//
// extra is merged into every Detail this call can produce, so a fact about the
// ATTEMPT — which neighbours were acknowledged — survives into
// farm.recovery_attempts.detail whatever the rung then did.
func (a *ADBActuator) hostLocal(ctx context.Context, log *slog.Logger, act Action, what string, extra map[string]any, run func() error) (Result, error) {
	detail := func(kv map[string]any) map[string]any {
		out := make(map[string]any, len(kv)+len(extra))
		for k, v := range extra {
			out[k] = v
		}
		// The rung's own facts win: extra describes the request, kv the result.
		for k, v := range kv {
			out[k] = v
		}
		return out
	}

	if a.HostRunner == nil {
		refusal := fmt.Sprintf(
			"tier %d (%s) needs %s on host %s, which only a farmd-node agent can do; "+
				"no host agent is configured for this farm",
			act.Tier, act.TierName, what, act.HostID)
		log.Warn("recovery rung refused: no host agent", "what", what)
		return Result{Outcome: OutcomeRefused,
			Detail: detail(map[string]any{"refusal": refusal})}, nil
	}

	if err := run(); err != nil {
		if ctx.Err() != nil {
			return Result{Outcome: OutcomeAborted,
				Detail: detail(map[string]any{"error": err.Error()})}, nil
		}
		return Result{Outcome: OutcomeFailed,
			Detail: detail(map[string]any{"what": what, "error": err.Error()})}, nil
	}

	// Re-enumeration is not instant. Poll for the device rather than declaring
	// victory on the syscall returning.
	if c, err := a.client(act.ADBEndpoint); err == nil {
		if a.waitForDevice(ctx, c, act.Devpath) {
			log.Info("recovery rung restored the device", "what", what)
			return Result{Outcome: OutcomeRecovered,
				Detail: detail(map[string]any{"what": what})}, nil
		}
	}
	return Result{Outcome: OutcomeNoChange, Detail: detail(map[string]any{"what": what})}, nil
}

// reboot asks the device to reboot and waits for it to come back.
func (a *ADBActuator) reboot(ctx context.Context, log *slog.Logger, act Action) (Result, error) {
	c, err := a.client(act.ADBEndpoint)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Detail: map[string]any{"error": err.Error()}}, nil
	}

	// reboot: is a device service, so it goes through a transport. The socket
	// dying as the device goes down is the expected outcome, not a failure.
	stream, err := c.OpenService(ctx, act.Devpath, "reboot:")
	if err != nil && !adbwire.IsTransport(err) {
		return Result{Outcome: OutcomeFailed,
			Detail: map[string]any{"error": err.Error()}}, nil
	}
	if stream != nil {
		_ = stream.Close()
	}

	if a.waitForDevice(ctx, c, act.Devpath) {
		log.Info("device rebooted and returned")
		return Result{Outcome: OutcomeRecovered, Detail: map[string]any{"rebooted": true}}, nil
	}
	if ctx.Err() != nil {
		return Result{Outcome: OutcomeAborted,
			Detail: map[string]any{"refusal": "reboot did not complete before the action timeout"}}, nil
	}
	return Result{Outcome: OutcomeNoChange, Detail: map[string]any{"rebooted": true}}, nil
}

// restartServer kills the host's ADB server. Tier 7 exists because sometimes
// the server is the broken thing, but it severs EVERY device on the host, so
// the ladder only reaches it after the blast-radius check against live leases.
func (a *ADBActuator) restartServer(ctx context.Context, log *slog.Logger, act Action) (Result, error) {
	c, err := a.client(act.ADBEndpoint)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Detail: map[string]any{"error": err.Error()}}, nil
	}

	log.Warn("restarting the host adb server; every device on this host is severed",
		"host", act.HostID, "endpoint", act.ADBEndpoint)

	if err := c.Kill(ctx); err != nil {
		return Result{Outcome: OutcomeFailed,
			Detail: map[string]any{"error": err.Error()}}, nil
	}

	// The container's restart policy brings the server back; we wait for it
	// rather than assume, because reporting recovery for a host that stayed
	// down would hide a dead host behind a successful-looking rung.
	if a.waitForDevice(ctx, c, act.Devpath) {
		return Result{Outcome: OutcomeRecovered,
			Detail: map[string]any{"adb_server_restarted": true}}, nil
	}
	return Result{Outcome: OutcomeNoChange,
		Detail: map[string]any{"adb_server_restarted": true,
			"note": "server was killed but the device has not returned yet"}}, nil
}

// waitForDevice polls until the position reports "device" or the context ends.
func (a *ADBActuator) waitForDevice(ctx context.Context, c *adbwire.Client, devpath string) bool {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if st, err := c.State(ctx, devpath); err == nil && st == adbwire.StateDevice {
				return true
			}
		}
	}
}
