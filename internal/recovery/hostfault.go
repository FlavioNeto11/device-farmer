package recovery

import (
	"context"
	"errors"
	"fmt"
)

// HostFault is what a [HostRunner] error means, in the vocabulary
// farm.recovery_attempts records.
//
// It exists so that every caller of a HostRunner — the ladder's actuator and
// the operator's slot power route alike — reads the agent's answer through
// ONE table. Two tables drift: one files an unreachable host as a failed
// rung, the other files a caller's own budget as the host's fault, and the
// same wire answer ends up meaning two different things in the same column.
type HostFault struct {
	// Disposition is the answer: refused, unreachable, aborted or failed. It
	// is never recovered or no_change — those need a state read, and an
	// error is not one.
	Disposition Disposition

	// BudgetElapsed reports that Disposition came from the caller's own
	// context expiring — its action budget — and not from anything the agent
	// said. It is set only alongside DispositionUnreachable, and it is what
	// tells "the agent said nothing in time" from "the agent said it could
	// not be reached".
	BudgetElapsed bool

	// RefusalKind classifies a refusal when the wire answer carried enough to
	// say what kind it was. It is [RefusalKindGanged] for
	// [ErrRungRefusedGanged] and "" for every other refusal, and it is the
	// value that belongs under [DetailRefusalKind] in the attempt detail.
	//
	// It is set only alongside DispositionRefused. "" there does not mean
	// "not ganged" — it means the refusal did not say, and the reason text is
	// the only account of it.
	RefusalKind string

	// Err is the error that was classified.
	Err error
}

// ClassifyHostFault turns a [HostRunner] error into one of the answers a
// host-local rung can give.
//
// The agent's own classification is consulted first and the caller's context
// second, because an agent that says "unreachable" has told us something the
// context cannot: that the round trip never got an answer it could attribute.
// A runner speaks either [RungFault] or the two sentinels [ErrRungRefused]
// and [ErrHostUnreachable]; an error that speaks neither and is not a context
// error is taken at face value as a rung that ran and failed.
//
// aborted reports whether the CALLER'S loop ended — a shutdown — as opposed
// to the action's own budget elapsing. Only the second is a statement about
// the host; the first is a statement about nothing at all, and is filed as
// aborted so a shutdown never reads as evidence against a handset.
func ClassifyHostFault(err error, aborted bool) HostFault {
	f := HostFault{Err: err}

	// The kind is read off the error itself rather than off which arm found
	// the refusal: an agent may answer with a RungFault that also wraps
	// ErrRungRefusedGanged, and the ganged/not-ganged distinction is the one
	// an operator acts on — it says whether the rack needs per-port power
	// switching or whether this one lease said no.
	refused := func() HostFault {
		f.Disposition = DispositionRefused
		if errors.Is(err, ErrRungRefusedGanged) {
			f.RefusalKind = RefusalKindGanged
		}
		return f
	}

	var fault RungFault
	if errors.As(err, &fault) {
		switch {
		case fault.HostUnreachable():
			f.Disposition = DispositionUnreachable
			return f
		case fault.RungRefused():
			return refused()
		}
	}
	switch {
	case errors.Is(err, ErrHostUnreachable):
		f.Disposition = DispositionUnreachable
		return f
	case errors.Is(err, ErrRungRefused):
		return refused()
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if aborted {
			f.Disposition = DispositionAborted
			return f
		}
		// Our own action budget, not the loop's shutdown: the host took
		// longer than the rung was given. That is a statement about the host.
		f.Disposition = DispositionUnreachable
		f.BudgetElapsed = true
		return f
	}

	f.Disposition = DispositionFailed
	return f
}

// Reason renders the sentence farm.recovery_attempts.refusal wants for this
// fault, naming the rung, the operation and the host, and "" for the two
// dispositions that carry no refusal: a rung that ran and failed, and a loop
// that went away.
//
// The sentences are the ones the ladder has always written, so an operator
// reading the column cannot tell from the wording which process asked.
func (f HostFault) Reason(tier int, tierName, what, hostID string) string {
	switch f.Disposition {
	case DispositionUnreachable:
		if f.BudgetElapsed {
			return fmt.Sprintf(
				"tier %d (%s) ran out of its action budget waiting for the farmd-node agent on "+
					"host %s to finish %s",
				tier, tierName, hostID, what)
		}
		return fmt.Sprintf(
			"tier %d (%s) needs %s on host %s and the farmd-node agent there could not be "+
				"reached (%v); nothing was learned about the device, and no rung on this host "+
				"will help until that agent answers again",
			tier, tierName, what, hostID, f.Err)
	case DispositionRefused:
		return fmt.Sprintf(
			"tier %d (%s) was refused by the farmd-node agent on host %s: %v; %s was not "+
				"performed and the device is as it was",
			tier, tierName, hostID, f.Err, what)
	}
	return ""
}
