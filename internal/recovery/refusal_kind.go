package recovery

import "github.com/flaviopadilha/device-farmer/internal/obs"

// This file is the blast-radius rule, exported.
//
// The ladder decides in exactly one place whether a rung may be climbed in
// the presence of live leases — checkBlastRadius — and that decision has two
// halves: whether a lease's disruption_policy PERMITS the rung, and, when it
// does not, what KIND of refusal that is. Both halves are pure functions of
// values already in the database, and both are exported here so that the one
// other producer of the refusal label — the demo's simulated ladder — asks the
// same functions instead of keeping a second copy of the rule next to the
// first. Two copies drift, and the label an operator alerts on ("buy per-port
// hubs" fires on refused_ganged) must mean one thing on every farm, simulated
// or not.

// PolicyPermits reports whether a lease carrying policy may be disturbed by a
// rung that requires at least `required`. Policies are ordered
// no_disruption < allow_soft_reset < allow_port_power_cycle, and a policy
// this package does not recognise ranks lowest: a future value must not
// become permission by default.
func PolicyPermits(policy, required string) bool {
	return policyRank(policy) >= policyRank(required)
}

// RefusalKind classifies a blast-radius refusal for the metric label.
//
// A refusal is 'refused_ganged' when the lease that forbids the tier is on a
// NEIGHBOUR and the power domain is ganged — the classic case: cutting power
// to the broken phone also cuts it for the one next to it that is running
// somebody's six-hour test. Everything else — the device's own lease, or a
// neighbour on a domain that could have switched one port — is
// 'refused_policy'. The distinction is the signal an operator reads to decide
// whether the rack needs per-port switching (a rising refused_ganged rate) or
// whether tenants' disruption policies are simply strict (refused_policy);
// the ladder is not broken in either case.
//
// It is a pure function, and it is the ONLY producer of that classification:
// checkBlastRadius calls it, and so does the demo's simulated refusal, so a
// refusal narrated on a demo carries the same label the ladder would have
// written for the same lease in the same domain.
func RefusalKind(neighbour bool, powerKind string) obs.RecoveryOutcome {
	if neighbour && powerKind == "ganged" {
		return obs.OutcomeRefusedGanged
	}
	return obs.OutcomeRefusedPolicy
}
