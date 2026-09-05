// Package obs defines the metric surface of device-farmer.
//
// The metric set is a design document. What a farm chooses to page on
// becomes, within a quarter, what its operators believe the system is
// for — and then what its automation is built to silence. So the choice
// of which series exist, and which of them may wake a human, is a
// statement of the invariant rather than a reporting detail.
//
// # The invariant
//
// A lease is ended by the job, by a deadline the user wrote down, or by a
// human. Nothing else. Not a socket error, not a probe timeout, not a
// device going offline, not a pod dying.
//
// Three clocks are never collapsed:
//
//  1. Lease liveness — leases.heartbeat_at / expires_at. Answers only
//     "does the entity holding this lease still exist?"
//  2. Job liveness — device-side progress. An alerting concern that can
//     never release a device.
//  3. Device health — device_runtime.adb_state. Drives the watchdog and
//     touches a lease exactly never.
//
// DeviceFarmer/STF issue #663 — open and unanswered since 2023 — fuses
// clock 3 and a transport failure of clock 1 into a release decision. A
// roughly 90-minute ECONNRESET on a device that was fine the whole time
// releases it mid-run and destroys multi-hour work. That fusion is the
// entire bug, and it is far more often introduced by an alert than by a
// line of code: someone pages on transport errors, someone else automates
// the response, and now a socket error ends leases.
//
// # The alerting philosophy
//
// Alert on lost work. Alert on correlated physical failure. Never alert
// on transport noise.
//
// Lost work is the only true emergency. Everything that ends a lease
// legitimately is already visible in the reason label, and exactly one of
// those reasons means the control plane destroyed something:
//
//	increase(farm_lease_reaped_total{reason="holder_expired"}[15m]) > 0
//	increase(farm_lease_renew_failures_total{kind="fenced"}[10m]) > 0
//	farm_lease_suspect{protected="true"} > 0   // for: 5m
//
// The first two are tombstones for work already gone; both series are
// created at zero by Register so the rules are armed from the first
// scrape rather than from the first casualty. The third is the opposite —
// a warning ahead of time. A protected lease is never auto-reclaimed, so
// a protected lease sitting in suspect is a long job whose supervisor has
// stopped renewing, waiting for a human. It will wait indefinitely, which
// is correct and is also why nobody notices without this rule.
//
// Correlated physical failure is the other page, and the point of paging
// on the correlation instead of the members is that twelve simultaneous
// device alerts are noise a human silences, while one hub alert is a
// person walking to a rack:
//
//	sum by (host, hub) (farm_device_health{state="healthy"}) == 0
//	  and sum by (host, hub) (farm_device_health) > 1
//
//	sum by (host, hub) (rate(farm_recovery_attempts_total{outcome="failed"}[15m])) > 0.05
//
// This is why farm_device_health carries host and hub and is zero-filled
// across every health state, and why the physical counters carry
// rack_slot: the response to a hub failure is a physical action, and the
// alert should name the place to walk to.
//
// Ticket, do not page:
//
//	increase(farm_control_plane_gap_seconds_sum[1h]) > 600
//	sum(farm_slot_rearm_pending) > <a fraction of fleet size>
//	farm_reaper_unbeaten_components > 0                       for 10m
//
// A control-plane gap is our downtime and is worth someone's Monday, but
// it destroys nothing: farm.reaper_arm adds the gap back to every live
// lease's deadline and quiesces the reaper for the longest TTL it could
// have missed. The tenant pays nothing for our outage. Treating this as
// an emergency is what leads to disabling the quiesce gate, and disabling
// the quiesce gate is how a control-plane restart mass-reclaims a farm.
//
// The third is a reaper that refuses to arm because a name in
// FARM_REAPER_COMPONENTS has never written a heartbeat row. Nothing is
// reclaimed while it stands, which is the safe direction; the `for` covers
// a cold start, where the reaper's first cycle can beat the other roles to
// the table. Past that it is a component that has not started or a name
// the farm does not run — see docs/runbooks/component-beat-failing.md.
//
// # Never
//
//	farm_adb_transport_blips_total
//
// This counter exists and must never appear in an alerting rule. Not with
// a high threshold, not with a long window, not "just as a warning that
// routes to a channel". A blip means a socket died. Sockets die — to
// hub renegotiation, to cable seating, to an adb server restart, to the
// device rebooting under its own test. The device is very probably still
// sitting there doing the work it was leased for.
//
// Paging on it recreates #663 by a shorter path than a code change:
// it trains operators to read a transport blip as a device loss, and
// automation written to make the page stop follows within the quarter.
// The blip counter is for correlation on a dashboard — which hub is
// shedding sockets — and its only legitimate escalation is a human
// draining that hub so it takes no NEW allocations. Existing leases are
// not part of the response.
//
// The same reasoning shapes the code. internal/adbwire may not import
// internal/lease and may not contain lease vocabulary, enforced in CI. A
// transport error's only permitted effects are a typed error returned to
// its caller and a counter increment. Accordingly TransportBlip is the
// only symbol here that adbwire may call; it takes no lease, job or fence
// identifier and returns nothing, so transport code has no value to
// branch on and this package cannot become the side channel that the
// import ban was meant to close.
//
// # Label discipline
//
// Every label value is a named type with a closed constant set, and the
// enums mirror their CHECK constraints exactly — ReleaseReason has seven
// members because farm.leases.release_reason permits seven, and it has no
// connectivity member because passing 'device_offline' to
// farm.lease_release raises check_violation (SQLSTATE 23514). The absence
// is the design. A value the database would refuse is a value that
// releases nothing, so there is nothing for this package to count either.
//
// Values outside a set are folded rather than passed through, since an
// open label is unbounded cardinality. The fold never invents a claim it
// cannot prove: an unclassified renewal failure becomes "transient" and
// not "fenced", because asserting a fence pages a human about work that
// is fine; an unclassified recovery becomes "failed" and not "recovered",
// because a false success suppresses a page that should have fired.
//
// Attribution beyond these labels — which job, which device, which
// actor — is deliberately absent. farm.events is the forensic record: its
// rows carry job_id, device_id, slot_id and actor, and the reaper role is
// granted INSERT on it for exactly that reason. Metrics answer "is
// something wrong, and where"; they are not an audit log with worse
// retention.
package obs
