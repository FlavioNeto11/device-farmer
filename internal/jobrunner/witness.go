package jobrunner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/runner"
)

// The witness, wired.
//
// A lease has two ways of saying "I am still here". The renewal loop asks
// Postgres whether this PROCESS still exists, and it is what a healthy farm
// runs on. The witness answers a different question — is the WORK still
// running on the device? — from evidence gathered on the device itself, and
// it is what a job has left when the control plane cannot be reached:
// farm.lease_reclaim declines to take any lease whose witness_at is younger
// than one grace period. Both halves were written and tested long before
// anything started them; this file is the one call site that does.
//
// It lives here and not in the runner on purpose. The runner sees a lease
// only as runner.Holder — Context and Fenced — and that narrowness is a
// design decision, not an omission: nothing reachable from a step executor
// may extend a lease, and a witness started inside the runner would put the
// evidence and the thing it is evidence of in the same package. So for every
// placement this loop builds the on-device marker on the job's own device
// connection, hands it to the holder's witness loop as its Evidence, and
// stops both when the placement ends.
//
// Nothing in this file ends anything. A marker that cannot be written is a
// transport failure retried inside the lease the job still holds; a witness
// that is refused or errors is recorded and the next tick tries again; a
// placement whose marker cannot even be constructed runs anyway, protected by
// TTL+grace and the gap refund alone, and says so in the log. There is no
// path from any outcome here to a release, a cancellation or a requeue.
//
// And nothing in this file makes the placement wait. Stopping the witness
// signals its two goroutines and returns; the summary is written by a
// goroutine of its own once both are gone. The one thing the placement does
// wait for is the delete of its own marker, for at most the marker's short
// remove budget, because that has to happen before the device is released.

// witness is the on-device marker and the loop that presents it, for one
// placement. The zero value is not useful; a nil *witness is a placement
// that runs without one, and every method is a no-op on it.
type witness struct {
	marker *runner.Marker
	loop   *lease.WitnessLoop
	log    *slog.Logger

	// wg is the loop's own group, so Run's orderly stop waits for the
	// summary the way it waits for the placements that produced it.
	wg *sync.WaitGroup

	// cancel ends the marker's refresh goroutine independently of the holder,
	// so remove can silence the marker before deleting the proof. A refresh
	// racing the delete would put the marker straight back.
	cancel context.CancelFunc
	// done is closed when the marker goroutine has exited; reported when the
	// summary has been written and the counts folded into the metrics.
	done     chan struct{}
	reported chan struct{}
	once     sync.Once
}

// startWitness starts the marker and the witness loop for one placement.
//
// It returns nil, and the placement runs without a witness, when the marker
// cannot be constructed. The witness is protection a job may lack, never a
// precondition for running it: refusing a placement here would turn a
// missing safeguard into the very harm the safeguard exists to prevent.
func (jr *JobRunner) startWitness(h *lease.Holder, dev runner.Conn, p runner.Placement, log *slog.Logger) *witness {
	mcfg := jr.cfg.MarkerConfig
	// The loop's base logger, not this placement's: NewMarker adds the
	// job, device, lease and fence itself, and a logger that already carries
	// them would print every one twice.
	mcfg.Logger = jr.cfg.Logger
	m, err := runner.NewMarker(dev, p, mcfg)
	if err != nil {
		witnesslessTotal.Inc()
		log.Warn("no on-device lease marker for this placement, so no witness will be presented; "+
			"the job runs protected by ttl+grace and the gap refund only", "err", err)
		return nil
	}

	wcfg := jr.cfg.WitnessConfig
	wcfg.Logger = log
	wcfg.Hooks = witnessHooks()
	loop, err := h.StartWitness(m, wcfg)
	if err != nil {
		witnesslessTotal.Inc()
		log.Warn("the witness loop could not be started; the job runs protected by ttl+grace "+
			"and the gap refund only", "err", err)
		return nil
	}

	w := &witness{
		marker: m, loop: loop, log: log, wg: &jr.wg,
		done: make(chan struct{}), reported: make(chan struct{}),
	}
	// Descended from the holder's context, so the refresh stops the instant
	// the lease is no longer held — fenced, released, or stopped — without
	// anyone remembering to say so.
	ctx, cancel := context.WithCancel(h.Context())
	w.cancel = cancel
	go func() {
		defer close(w.done)
		m.Run(ctx)
	}()
	return w
}

// stop halts the witness loop and the marker's refreshes and returns at once.
// Idempotent, and safe on a nil witness.
//
// It waits for neither goroutine. The witness loop may be mid round trip to
// the database, detached from cancellation on purpose (a witness worth
// presenting is worth finishing) and bounded only by its own timeout; the
// placement's unwinding — the holder's stop, the release of the device, the
// return of runJob on SIGTERM — must not sit behind it. A goroutine collects
// both, writes the summary and folds the counts into the metrics; Run's
// orderly stop waits for that goroutine as it waits for the placement.
//
// It does not touch the lease. Stopping the witness stops producing
// evidence, and nothing else.
func (w *witness) stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		w.cancel()
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer close(w.reported)
			w.loop.Stop()
			<-w.done
			w.report()
		}()
	})
}

// remove deletes the marker from the device, after a placement ended with a
// verdict of its own. A courtesy to the next holder, who would otherwise
// find a stale marker to replace; the fence inside the marker is what makes
// a leftover harmless, so a failure here costs nothing and is only logged.
//
// It stops the witness first and waits for the marker goroutine — and only
// for that one — because the proof must not be deleted while something could
// still write it back. The wait is short: the marker's refresh is cancelled,
// not waited out.
//
// The delete is skipped when there is nothing of ours on the device to
// delete. A superseded marker is a newer holder's, which Remove would refuse
// to touch anyway; a marker that never landed has left nothing behind, and a
// device that never took a write is not one the release should wait on.
func (w *witness) remove(ctx context.Context) {
	if w == nil {
		return
	}
	w.stop()
	<-w.done

	switch st := w.marker.Stats(); {
	case st.Superseded:
		w.log.Info("leaving the on-device lease marker alone: it is a newer holder's, not ours",
			"path", runner.MarkerPath)
	case st.Refreshes == 0:
		w.log.Info("no on-device lease marker to remove: no write of ours ever landed on the device",
			"path", runner.MarkerPath, "failed_refreshes", st.Failures)
	default:
		if err := w.marker.Remove(ctx); err != nil {
			w.log.Warn("could not remove the on-device lease marker; the next holder replaces it by its fence",
				"path", runner.MarkerPath, "err", err)
		}
	}
}

// report writes the placement's evidence summary and folds the marker's
// counts into the metrics. The marker has no hooks — it is a runner type and
// the runner exports no metrics — so its refreshes are counted here, once,
// when the placement ends.
func (w *witness) report() {
	ws, ms := w.loop.Stats(), w.marker.Stats()
	markerRefreshTotal.WithLabelValues(markerOK).Add(float64(ms.Refreshes))
	markerRefreshTotal.WithLabelValues(markerFailed).Add(float64(ms.Failures))

	attrs := []any{
		"witness_accepted", ws.Accepted, "witness_refused", ws.Refused,
		"witness_errors", ws.Errors, "witness_skipped", ws.Skipped,
		"marker_refreshes", ms.Refreshes, "marker_failures", ms.Failures,
	}
	switch {
	case ms.Superseded:
		// The marker already said this at Warn, the moment it found the
		// newer holder's file; the summary only closes the account. Whether
		// THIS lease ended was decided by farm.lease_renew, not by the file.
		w.log.Info("witness stopped; the device carried a newer holder's marker and no evidence "+
			"was presented after that", append(attrs, "err", ms.LastError)...)
	case ws.Refused+ws.Errors > 0 || ms.Failures > 0:
		w.log.Info("witness stopped; some evidence did not land, and none of it touched the lease",
			append(attrs, "last_marker_error", ms.LastError)...)
	default:
		w.log.Debug("witness stopped", attrs...)
	}
}

// witnessHooks count the loop's outcomes. Every hook runs on the witness
// goroutine and does nothing but increment.
func witnessHooks() lease.WitnessHooks {
	return lease.WitnessHooks{
		OnWitnessed: func(lease.Lease, time.Time, int) { witnessTotal.WithLabelValues(witnessAccepted).Inc() },
		OnRefused:   func(lease.Lease) { witnessTotal.WithLabelValues(witnessRefused).Inc() },
		OnError:     func(lease.Lease, error) { witnessTotal.WithLabelValues(witnessError).Inc() },
		OnSkipped:   func(lease.Lease, time.Duration) { witnessTotal.WithLabelValues(witnessSkipped).Inc() },
	}
}

// The outcome labels, used by the hooks that increment them and by the
// seeding in Collectors, so the two cannot drift apart.
const (
	witnessAccepted = "accepted"
	witnessRefused  = "refused"
	witnessError    = "error"
	witnessSkipped  = "skipped"

	markerOK     = "ok"
	markerFailed = "failed"
)

// witnessOutcomes and markerOutcomes are the label domains, pre-seeded in
// Collectors so the series exist before the first placement ends.
var (
	witnessOutcomes = []string{witnessAccepted, witnessRefused, witnessError, witnessSkipped}
	markerOutcomes  = []string{markerOK, markerFailed}
)

var (
	witnessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_jobrunner_witness_total",
		Help: "Witness ticks by outcome: accepted, refused, error, skipped. None of them can end a lease: " +
			"refused is the extension cap or a stale fence, error is retried on the next tick, and skipped " +
			"means the on-device marker was not fresh enough to present. A sustained rate of error is this " +
			"replica unable to reach the control plane; a sustained rate of skipped is a placement unable " +
			"to reach its device.",
	}, []string{"outcome"})

	markerRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_jobrunner_marker_refreshes_total",
		Help: "On-device lease marker writes by outcome (ok, failed), folded in when a placement ends. " +
			"A failed write is a transport fact retried inside the lease, and a burst of them is expected " +
			"around every reset tier above 'none', which reboots the phone: alert on a sustained rate, or " +
			"on farm_jobrunner_witness_total{outcome=\"skipped\"}, never on a single increment. A write " +
			"cut short by the placement ending is counted as neither.",
	}, []string{"outcome"})

	witnesslessTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_jobrunner_witnessless_total",
		Help: "Placements that ran with no witness at all, because the marker could not be constructed " +
			"or the witness loop could not be started. They are protected by ttl+grace and the gap " +
			"refund only.",
	})
)
