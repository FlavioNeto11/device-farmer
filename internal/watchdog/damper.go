package watchdog

import (
	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// This file is the damper, exported.
//
// The decision that turns an observation into a health value lives in exactly
// two places: DamperSQL, which is the CASE expression the reconcile statement
// evaluates against the server's now(), and Damp, its line-for-line mirror in
// Go. Anything else that writes farm.device_runtime.health from an observation
// — the demo's simulator is the one that exists — must embed DamperSQL rather
// than carry a CASE of its own. Two dampers with different rules on one table
// means every threshold an operator calibrates on the second writer's output
// is calibrated against that writer, not against this loop; damper_test.go
// keeps the two definitions here equal and keeps the reconcile statement
// using this one.

// HealthFor maps an ADB connection state onto the health vocabulary of
// farm.device_runtime.health. It is the candidate the damper is asked about.
//
// Every arm is a statement about the WIRE, never about the lease on the device.
// "offline" here means the ADB server cannot talk to it; it does not mean the
// job running on it has stopped, and it may never be read as a reason to take
// the device away from that job.
func HealthFor(s adbwire.ConnState) obs.HealthState {
	switch s {
	case adbwire.StateDevice:
		return obs.HealthHealthy
	case adbwire.StateOffline:
		return obs.HealthOffline
	case adbwire.StateUnauthorized:
		return obs.HealthUnauthorized
	case adbwire.StateAuthorizing, adbwire.StateConnecting:
		return obs.HealthBooting
	case adbwire.StateAbsent, adbwire.StateDetached:
		return obs.HealthMissing
	case adbwire.StateBootloader, adbwire.StateRecovery,
		adbwire.StateSideload, adbwire.StateRescue:
		// The device is alive and addressable, just not in a state that can run
		// a test. Distinguished from 'offline' because the operator response is
		// different: one is a cable, the other is a mode.
		return obs.HealthRecovering
	case adbwire.StateNoPermissions:
		// udev rules on the host, not a fault of the device.
		return obs.HealthDegraded
	default:
		return obs.HealthUnknown
	}
}

// DamperSQL is the health transition as one SQL CASE expression. It yields the
// next value of farm.device_runtime.health and reads these columns from the
// row source it is evaluated over:
//
//	cur_health   text     the row's health before this observation
//	suppressed   boolean  suppress_until IS NOT NULL AND suppress_until > now()
//	candidate    text     HealthFor(the observed adb_state)
//	bad          boolean  candidate <> 'healthy'
//	consec_bad   int      already advanced for this observation
//	consec_good  int      already advanced for this observation
//	credits      numeric  the token bucket after refill, before any charge
//	min_bad      int      Config.MinBad
//	min_good     int      Config.MinGood
//
// The column names are unqualified on purpose, so the expression can be
// dropped into any SELECT whose FROM produces them.
const DamperSQL = `CASE
           -- Retirement is an administrative fact, quarantine belongs to the
           -- recovery ladder, and 'parked' is a human (or a charge limiter)
           -- saying "out of service ON PURPOSE". None of the three is an
           -- observation this loop may overwrite.
           --
           -- 'parked' is the one that changes what this loop MEANS. A parked
           -- device usually has no VBUS, so the ADB tracker reports it absent
           -- and HealthFor() calls that 'missing' — which is true about the
           -- wire and false about the device. Writing it would put a perfectly
           -- good handset in front of the recovery ladder, which would climb
           -- to a port power cycle and then quarantine it. The authority for
           -- the state is farm.devices.admin_state='parked', which this role
           -- can read and cannot write; the value here is its mirror, and
           -- migration 00008 carries a trigger that holds it even if this CASE
           -- is ever edited away.
           WHEN cur_health IN ('retired','quarantined','parked') THEN cur_health
           -- An induced reset is in flight: the transport is EXPECTED to drop,
           -- so a drop proves nothing.
           WHEN suppressed THEN cur_health
           WHEN candidate = cur_health THEN cur_health
           -- 'unknown' is the absence of history, not a history of instability:
           -- nobody has looked at this device since it was enrolled, or since a
           -- quarantine was closed. Hysteresis exists to damp oscillation and
           -- there is nothing here to oscillate against, so one good look is
           -- enough. The token is still charged.
           WHEN NOT bad AND cur_health = 'unknown' AND credits >= 1 THEN candidate
           -- Falling is free but debounced: a device that is failing must leave
           -- the schedulable set even with an empty bucket.
           WHEN bad AND consec_bad >= min_bad THEN candidate
           -- Rising costs a token on top of the hysteresis. This is the
           -- expensive direction because it is the one that puts a device back
           -- in front of a tenant.
           WHEN NOT bad AND consec_good >= min_good AND credits >= 1 THEN candidate
           ELSE cur_health
         END`

// DampInput is one evaluation of the damper: the same nine values DamperSQL
// reads, minus `bad`, which is derived from Candidate rather than supplied so
// the two cannot disagree.
type DampInput struct {
	Current    obs.HealthState // the row's health before this observation
	Candidate  obs.HealthState // HealthFor(observed state)
	Suppressed bool            // suppress_until is in the future
	ConsecBad  int             // already advanced for this observation
	ConsecGood int             // already advanced for this observation
	Credits    float64         // after refill, before any charge
	MinBad     int
	MinGood    int
}

// Damp is DamperSQL in Go, arm for arm and in the same order. It exists for
// callers that need the verdict without a round trip — and as the oracle the
// SQL is tested against.
func Damp(in DampInput) obs.HealthState {
	bad := in.Candidate != obs.HealthHealthy
	switch {
	case in.Current == obs.HealthRetired || in.Current == obs.HealthQuarantined ||
		in.Current == obs.HealthParked:
		return in.Current
	case in.Suppressed:
		return in.Current
	case in.Candidate == in.Current:
		return in.Current
	case !bad && in.Current == obs.HealthUnknown && in.Credits >= 1:
		return in.Candidate
	case bad && in.ConsecBad >= in.MinBad:
		return in.Candidate
	case !bad && in.ConsecGood >= in.MinGood && in.Credits >= 1:
		return in.Candidate
	default:
		return in.Current
	}
}
