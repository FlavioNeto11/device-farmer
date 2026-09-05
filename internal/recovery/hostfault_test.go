package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
)

// hostFaultErr is a runner error that speaks RungFault rather than the
// sentinels.
type hostFaultErr struct {
	msg                  string
	refused, unreachable bool
}

func (e hostFaultErr) Error() string         { return e.msg }
func (e hostFaultErr) RungRefused() bool     { return e.refused }
func (e hostFaultErr) HostUnreachable() bool { return e.unreachable }

// hostFaultCases is every shape of HostRunner error the classification has
// to tell apart, with what it must say.
func hostFaultCases() []struct {
	name          string
	err           error
	aborted       bool
	disposition   Disposition
	budgetElapsed bool
} {
	return []struct {
		name          string
		err           error
		aborted       bool
		disposition   Disposition
		budgetElapsed bool
	}{
		{"sentinel refused", fmt.Errorf("%w: no uhubctl", ErrRungRefused), false, DispositionRefused, false},
		{"sentinel unreachable", fmt.Errorf("%w: dial", ErrHostUnreachable), false, DispositionUnreachable, false},
		{"RungFault refused", hostFaultErr{msg: "declined", refused: true}, false, DispositionRefused, false},
		{"RungFault unreachable", hostFaultErr{msg: "gone", unreachable: true}, false, DispositionUnreachable, false},
		{"RungFault neither", hostFaultErr{msg: "ran and failed"}, false, DispositionFailed, false},
		{"unreachable wins over a deadline inside it",
			fmt.Errorf("%w: %w", ErrHostUnreachable, context.DeadlineExceeded), false, DispositionUnreachable, false},
		{"action budget elapsed", fmt.Errorf("cut short: %w", context.DeadlineExceeded), false, DispositionUnreachable, true},
		{"action cancelled by its own budget", context.Canceled, false, DispositionUnreachable, true},
		{"loop shutdown", context.Canceled, true, DispositionAborted, false},
		{"loop shutdown at the deadline", context.DeadlineExceeded, true, DispositionAborted, false},
		{"plain failure", errors.New("port stayed dark"), false, DispositionFailed, false},
	}
}

// TestClassifyHostFault is the table.
//
// Falsify: swap the order of the sentinel and context checks in
// ClassifyHostFault; "unreachable wins over a deadline inside it" fails.
func TestClassifyHostFault(t *testing.T) {
	t.Parallel()
	for _, tc := range hostFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := ClassifyHostFault(tc.err, tc.aborted)
			if f.Disposition != tc.disposition {
				t.Errorf("disposition = %q, want %q", f.Disposition, tc.disposition)
			}
			if f.BudgetElapsed != tc.budgetElapsed {
				t.Errorf("BudgetElapsed = %v, want %v", f.BudgetElapsed, tc.budgetElapsed)
			}
			if !errors.Is(f.Err, tc.err) {
				t.Errorf("Err does not carry the classified error")
			}
			if f.Disposition == DispositionRecovered || f.Disposition == DispositionNoChange {
				t.Errorf("an error classified as %q; only a state read may say that", f.Disposition)
			}
		})
	}
}

// TestClassifyHostFaultMatchesTheActuator holds the exported classification
// and the actuator's own, rung-bound one, to the same answers and the same
// refusal sentences, so a caller outside the ladder — the operator's slot
// power route — writes rows the ladder's reader cannot tell from its own.
//
// Falsify: change any sentence in HostFault.Reason.
func TestClassifyHostFaultMatchesTheActuator(t *testing.T) {
	t.Parallel()
	const what = "VBUS power cycle"
	a := &ADBActuator{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	act := Action{Tier: 4, TierName: "port_power", HostID: testHost, Devpath: testDevpath, RackSlot: testRackSlot}

	for _, tc := range hostFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.aborted {
				cancel()
			}
			r := &rung{parent: parent, ctx: parent, act: act, log: a.log}

			gotD, gotReason := a.classifyHostFault(r, what, tc.err)
			f := ClassifyHostFault(tc.err, r.aborted())
			if gotD != f.Disposition {
				t.Fatalf("actuator says %q, ClassifyHostFault says %q", gotD, f.Disposition)
			}
			if want := f.Reason(act.Tier, act.TierName, what, act.HostID); gotReason != want {
				t.Fatalf("actuator reason:\n  %q\nHostFault.Reason:\n  %q", gotReason, want)
			}
			if (gotReason == "") != (gotD == DispositionFailed || gotD == DispositionAborted) {
				t.Fatalf("reason %q for disposition %q; failed and aborted carry none, the rest must", gotReason, gotD)
			}
		})
	}
}
