package jobrunner

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// The renewal loop, observed.
//
// A lease has two ways of saying "I am still here", and witness.go wires the
// second one. This file wires the first. internal/lease has exported
// HolderHooks — OnRenewed, OnTransientError, OnFenced — for as long as it has
// had a renewal loop, precisely so that the loop's outcomes could be measured
// without that package importing a metrics registry. Nothing supplied them.
//
// Every holder in a running farm is built by runJob, so "nothing supplied
// them" meant exactly one thing: when a holder's renewals started failing —
// the condition the witness loop exists to survive, and the only condition in
// which the witness is load bearing at all — the entire external record was a
// "lease renewal failed, retrying (lease NOT lost)" line in one replica's log.
// /metrics read identically to a farm where nothing had gone wrong. The
// operator learned about it afterwards, from the reaper marking the lease
// suspect, or from the witness quietly carrying it, or from neither.
//
// # A failed renewal is not a lease ending, and is never counted as one
//
// At the instant these hooks run the lease still has its deadline, the device
// is still ours and the job is still running. So nothing here touches
// farm_jobrunner_releases_total, farm_jobrunner_fenced_total or
// farm_lease_reaped_total — the meters whose subject is a lease that stopped.
// The counter below is about ATTEMPTS, and its Help says so, because a metric
// that let a database hiccup be read as a device loss would put a number on
// DeviceFarmer/STF #663 and then dress the number up as evidence.
//
// farm_jobrunner_fenced_total is the closest neighbour and is deliberately
// still separate: it counts the RUNS that found themselves fenced at a
// decision site and consequently wrote nothing and released nothing. The
// fenced outcome here counts the RENEWAL that discovered it. One is a
// bookkeeping refusal, the other is the moment the device stopped being ours,
// and an operator reconstructing an incident wants both timestamps.
//
// # The line this file must not redraw
//
// internal/lease draws it once, in Holder.run: zero rows from farm.lease_renew
// is FENCING and terminal, and every other failure is TRANSIENT and is retried
// inside the lease the job still holds. internal/obs draws the same line for
// the fleet-wide counter, in RenewFailureKind, and folds an unrecognised kind
// to KindTransient so a fence is never claimed without proof.
//
// So this file classifies nothing. It receives the verdict — the holder calls
// OnTransientError or OnFenced, never both, having already decided — and
// reports it under the label internal/obs already uses for it. There is no
// third opinion here that could disagree with the other two, and no branch in
// which one could be mistaken for the other.
//
// # Two registers, one event
//
// Each failure is recorded twice, because the two counters answer different
// questions.
//
// obs.LeaseRenewFailure is the fleet-wide one, farm_lease_renew_failures_total
// {kind}, and DeviceFarmerLeaseFenced — a critical page — is written over it.
// The alerting rules file has listed "api, jobrunner, scheduler, reaper" as
// its publishers for as long as it has existed; only the api and the demo ever
// incremented it, so the page could fire for a hand-driven renewal through the
// HTTP endpoint and not for the holders that carry the farm's actual work.
// This is the half of the wiring that makes that list true.
//
// farm_jobrunner_renewals_total is this loop's own, and it exists because the
// fleet counter counts only failures. "Three transient failures" is not a
// finding on its own: three among six hundred attempts is a database that
// hiccuped and recovered, three among three is a replica whose every lease is
// silently burning its TTL, and those are not the same morning. Counting the
// SUCCESSES is what lets a rule tell them apart, and what makes
// DeviceFarmerLeaseRenewalsFailing possible: it asks whether this replica has
// had failures AND landed nothing, which is a question with no threshold in it
// to tune against internal/lease's backoff constants.

// renewalHooks are the observation callbacks handed to every holder this loop
// builds.
//
// Each one runs synchronously on the renewal goroutine — the goroutine whose
// timeliness is what keeps a lease alive — so each does nothing but increment.
// No logging: Holder.run already logs every branch with the lease, device,
// job, fence, attempt and backoff attached, and a second line here would say
// less, twice. No call back into the holder either: HolderHooks documents that
// Stop, Release and Witness join the renewal loop and would deadlock from
// inside one of these.
func renewalHooks() lease.HolderHooks {
	return lease.HolderHooks{
		OnRenewed: func(_ lease.Lease, res lease.RenewResult) {
			// A self-heal is an ordinary successful renewal that happened to
			// land on a lease the sweeper had already marked suspect: nothing
			// was released, no work was lost and the fence never moved. It
			// gets its own label rather than a share of "ok" because it is the
			// one success in this set that reports an incident — the renewal
			// path was failing for long enough to be noticed, and has
			// recovered. Worth a graph, never worth a page.
			if res.WasSuspect {
				renewalsTotal.WithLabelValues(renewalSelfHealed).Inc()
				return
			}
			renewalsTotal.WithLabelValues(renewalOK).Inc()
		},

		// attempt and retryIn are deliberately dropped. Both are per-holder
		// state that the holder's own log line already carries beside the
		// lease it belongs to, and a label built from either would be an
		// unbounded dimension on a counter — the cardinality rule
		// internal/obs enforces on every label it defines.
		OnTransientError: func(lease.Lease, int, error, time.Duration) {
			renewalsTotal.WithLabelValues(renewalTransient).Inc()
			obs.LeaseRenewFailure(obs.KindTransient)
		},

		// Fires once, immediately before the holder's context is cancelled, so
		// this increment lands while the ADB sockets derived from that context
		// are still being torn down. That ordering is why it is here and not
		// at the place the run eventually notices: by then the process has
		// already spent its unwinding, and a scrape in between would have
		// shown nothing.
		OnFenced: func(lease.Lease) {
			renewalsTotal.WithLabelValues(renewalFenced).Inc()
			obs.LeaseRenewFailure(obs.KindFenced)
		},
	}
}

// The outcome labels, used by the hooks that increment them and by the seeding
// in Collectors, so the two cannot drift apart.
//
// "transient" and "fenced" are spelled exactly as internal/obs spells the two
// RenewFailureKind values. One distinction, one pair of words, so an operator
// moving between farm_jobrunner_renewals_total and farm_lease_renew_failures_total
// is not asked to learn a second vocabulary for the same fact.
const (
	renewalOK         = "ok"
	renewalSelfHealed = "self_healed"
	renewalTransient  = "transient"
	renewalFenced     = "fenced"
)

// renewalOutcomes is the label domain, pre-seeded in Collectors so all four
// series exist before the first placement does.
var renewalOutcomes = []string{renewalOK, renewalSelfHealed, renewalTransient, renewalFenced}

var renewalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "farm_jobrunner_renewals_total",
	Help: "Lease renewal ATTEMPTS by this replica's holders, by outcome, partitioning every " +
		"branch of the renewal loop. ok landed; self_healed landed on a lease the sweeper had " +
		"marked suspect, so an incident ended with no work lost and no fence moved; transient " +
		"is a call that did not complete and proves NOTHING about the lease, which keeps its " +
		"deadline and its device while the holder retries on a backoff; fenced is " +
		"farm.lease_renew returning zero rows, the one unambiguous proof the lease is gone. " +
		"None of these is a lease ending — that is farm_jobrunner_releases_total. Do not alert " +
		"on transient increments, nor on a share of them: a database blip read as a device loss " +
		"is the failure this system exists to prevent. Alert on transient climbing while ok and " +
		"self_healed have gone to ZERO, which is a renewal path that is down rather than one " +
		"that stuttered, and needs no threshold.",
}, []string{"outcome"})
