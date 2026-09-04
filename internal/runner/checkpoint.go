package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// checkpointVersion is written into every checkpoint. A checkpoint whose
// version this binary does not understand is not resumed — starting over on a
// dirty device is worse than starting over on a clean one, so an unreadable
// checkpoint sends the job to a fresh placement rather than to a guess.
const checkpointVersion = 1

// Checkpoint is farm.jobs.checkpoint: enough to pick a run back up exactly
// where a killed process left it, and not one byte more.
//
// It exists for one event, which is the most ordinary event in a Kubernetes
// control plane: the pod running a job is evicted. Its replacement acquires
// the same lease by job id, gets the SAME device at the SAME fence, and must
// then answer a question the device cannot answer for it — which of these
// steps have already happened? That is what this is.
type Checkpoint struct {
	Version int `json:"version"`

	// Attempt, Fence and DeviceID identify the placement this checkpoint
	// describes. A checkpoint from a different fence describes work done on a
	// device we no longer hold, or on a device that has been reset since, and
	// is therefore not resumable — only re-runnable from the start.
	Attempt  int    `json:"attempt"`
	Fence    int64  `json:"fence"`
	DeviceID string `json:"device_id"`

	// SpecHash pins the step list this progress was recorded against. An
	// operator editing a running job's spec is rare and legitimate; resuming
	// index 4 of a list that no longer has the same steps at indexes 0..3 is
	// neither.
	SpecHash string `json:"spec_hash"`

	// LastIndex is the furthest step this placement has touched. Purely for
	// operators reading the row; the resume itself is driven by Completed.
	LastIndex int `json:"last_index"`

	Completed []CompletedStep `json:"completed"`

	// InFlight is the step that had begun and had not finished. It is written
	// BEFORE a non-idempotent step runs, so that "we do not know whether this
	// happened" is a recorded fact rather than something to be inferred from
	// silence.
	InFlight *InFlightStep `json:"in_flight,omitempty"`
}

// CompletedStep is one step this placement finished.
type CompletedStep struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
}

// InFlightStep is the step that was running when everything stopped.
type InFlightStep struct {
	Index int          `json:"index"`
	ID    string       `json:"id"`
	Kind  jobspec.Kind `json:"kind"`
}

func newCheckpoint(attempt int, p Placement, spec jobspec.Spec) Checkpoint {
	return Checkpoint{
		Version:   checkpointVersion,
		Attempt:   attempt,
		Fence:     p.Fence,
		DeviceID:  p.DeviceID,
		SpecHash:  specHash(spec),
		LastIndex: -1,
	}
}

// parseCheckpoint reads farm.jobs.checkpoint. A checkpoint that does not parse
// is treated as absent: the job starts over on a fresh placement, which is
// safe, rather than resuming against a structure this binary guessed at.
func parseCheckpoint(raw []byte) Checkpoint {
	var c Checkpoint
	if len(raw) == 0 {
		return Checkpoint{}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Checkpoint{}
	}
	return c
}

// belongsTo reports whether this checkpoint describes THIS placement: same
// device, same fence, same lease incarnation.
//
// The fence is what makes the test trustworthy. It is monotonic and unique per
// lease, so a matching fence means no other holder has had the device in
// between — nobody has reset it, nothing has been reallocated, and the state
// the checkpoint describes is still on the hardware in front of us.
func (c Checkpoint) belongsTo(p Placement) bool {
	return c.Version == checkpointVersion &&
		c.Attempt > 0 &&
		c.Fence == p.Fence &&
		c.DeviceID == p.DeviceID
}

func (c *Checkpoint) markInFlight(index int, st jobspec.Step) {
	c.InFlight = &InFlightStep{Index: index, ID: st.ID, Kind: st.Kind()}
	c.LastIndex = index
}

func (c *Checkpoint) markDone(index int, st jobspec.Step) {
	for _, done := range c.Completed {
		if done.Index == index {
			c.InFlight = nil
			c.LastIndex = index
			return
		}
	}
	c.Completed = append(c.Completed, CompletedStep{Index: index, ID: st.ID})
	c.InFlight = nil
	c.LastIndex = index
}

// specHash fingerprints the step list by position, id and kind — the three
// things a resume relies on. It deliberately ignores step arguments: changing
// a timeout or a package name does not invalidate the record of which steps
// already ran, but reordering or replacing them does.
func specHash(spec jobspec.Spec) string {
	h := sha256.New()
	for i, st := range spec.Steps {
		fmt.Fprintf(h, "%d\x00%s\x00%s\n", i, st.ID, st.Kind())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// plan is the decision, taken before the first step runs, about what this
// attempt will skip, what it will re-run, and what it will re-attach to.
type plan struct {
	skip     map[int]bool
	reattach map[int]bool
	idem     map[int]bool

	// refusedIndex is the step a refusal is about, or -1.
	refusedIndex int
}

// idempotent reports whether step i may be repeated. It is what decides
// whether the checkpoint has to go down before the step runs.
func (p plan) idempotent(i int) bool { return p.idem[i] }

// planResume works out how a re-attached attempt should proceed.
//
// The rules, in the order they are applied:
//
//  1. Not resuming (a fresh placement, a non-resumable job, or a checkpoint
//     from another fence): everything runs, and nothing is skipped. Fresh
//     placements get a device that is not carrying our half-finished work.
//  2. The spec must be the same list it was when the progress was recorded.
//  3. Steps recorded complete are skipped.
//  4. The step that was in flight is re-run if — and only if — it can be
//     repeated without changing the outcome. farm.step_kinds is the authority
//     on that, read from the database rather than remembered in Go.
//  5. A shell_detached step that was in flight is a special case, and it is
//     the case this design exists for: its work is running ON THE DEVICE, so
//     the correct move is neither to skip it nor to start a second copy, but
//     to look for its marker files and re-attach to the run already in
//     progress.
//  6. Anything else in flight is REFUSED. Re-running an interrupted
//     non-idempotent step would repeat a side effect that may well have
//     already happened — a payment, an install, a factory reset — and a job
//     that fails with a clear message is strictly better than a farm that
//     silently does things twice.
func planResume(spec jobspec.Spec, ckpt Checkpoint, kinds map[jobspec.Kind]kindInfo, resuming bool) (plan, error) {
	pl := plan{
		skip:         make(map[int]bool, len(spec.Steps)),
		reattach:     make(map[int]bool, 1),
		idem:         make(map[int]bool, len(spec.Steps)),
		refusedIndex: -1,
	}
	for i, st := range spec.Steps {
		pl.idem[i] = stepIsIdempotent(st, kinds)
	}
	if !resuming {
		return pl, nil
	}

	if ckpt.SpecHash != "" && ckpt.SpecHash != specHash(spec) {
		return pl, fmt.Errorf(
			"runner: the job spec changed since attempt %d recorded its progress; refusing to resume into a different step list",
			ckpt.Attempt)
	}

	for _, done := range ckpt.Completed {
		if done.Index < 0 || done.Index >= len(spec.Steps) {
			return pl, fmt.Errorf(
				"runner: checkpoint records step %d as complete but the spec has %d steps; refusing to resume",
				done.Index, len(spec.Steps))
		}
		if got := spec.Steps[done.Index].ID; got != done.ID {
			return pl, fmt.Errorf(
				"runner: checkpoint records %q as complete at index %d but the spec has %q there; refusing to resume",
				done.ID, done.Index, got)
		}
		pl.skip[done.Index] = true
	}

	if ckpt.InFlight == nil {
		return pl, nil
	}

	in := *ckpt.InFlight
	if in.Index < 0 || in.Index >= len(spec.Steps) {
		return pl, fmt.Errorf(
			"runner: checkpoint records step %d as in flight but the spec has %d steps; refusing to resume",
			in.Index, len(spec.Steps))
	}
	st := spec.Steps[in.Index]
	if st.ID != in.ID {
		pl.refusedIndex = in.Index
		return pl, fmt.Errorf(
			"runner: checkpoint records %q as in flight at index %d but the spec has %q there; refusing to resume",
			in.ID, in.Index, st.ID)
	}
	if pl.skip[in.Index] {
		// It completed after the in-flight marker went down and the marker was
		// never cleared. Completed wins: it is the stronger statement.
		return pl, nil
	}

	switch {
	case st.Kind() == jobspec.KindShellDetached:
		pl.reattach[in.Index] = true
		return pl, nil
	case pl.idem[in.Index]:
		return pl, nil
	default:
		pl.refusedIndex = in.Index
		return pl, fmt.Errorf(
			"runner: step %d (%s) of kind %s was interrupted and cannot be repeated safely "+
				"(farm.step_kinds says it is not idempotent); refusing to run it a second time — "+
				"a job whose long work runs in a shell_detached step resumes here instead, because "+
				"the device, not this process, owns that result",
			in.Index, st.ID, st.Kind())
	}
}

// stepIsIdempotent asks the database's vocabulary, which is the authority:
// farm.step_kinds.idempotent is a stored fact about a stored vocabulary, so a
// spec written today still resumes tomorrow the way its author expected. The
// default for an unrecognised kind is false — a step nobody has classified
// must not be repeated on a guess.
func stepIsIdempotent(st jobspec.Step, kinds map[jobspec.Kind]kindInfo) bool {
	info, ok := kinds[st.Kind()]
	return ok && info.Idempotent
}

// saveCheckpoint writes progress, fenced.
//
// The WHERE clause is the whole point. A process that lost its lease an hour
// ago and only now gets its statement through must not overwrite the progress
// of the attempt that replaced it: its fence is below the one recorded, so the
// update matches nothing. Zero rows is therefore an ordinary outcome, logged
// and not raised — by the time it happens there is nothing left to abort, and
// the checkpoint on the server is newer than ours anyway.
//
// updated_at is set by Postgres inside the same statement. No client clock
// touches this row.
func (r *Runner) saveCheckpoint(ctx context.Context, log *slog.Logger, p Placement, c Checkpoint) {
	const q = `
UPDATE farm.jobs
   SET checkpoint = $2::jsonb || jsonb_build_object('updated_at', now())
 WHERE id = $1::uuid
   AND COALESCE((checkpoint->>'fence')::bigint, 0) <= $3`

	body, err := json.Marshal(c)
	if err != nil {
		log.Error("could not encode the checkpoint", "err", err)
		return
	}

	cctx, cancel := r.db(ctx)
	defer cancel()

	tag, err := r.cfg.Pool.Exec(cctx, q, p.JobID, string(body), p.Fence)
	switch {
	case err != nil:
		// A checkpoint that did not land costs a re-run of one step on the
		// next attempt, or a refusal to resume. It never costs the device, so
		// it must not end the run.
		log.Error("could not write the job checkpoint", "last_index", c.LastIndex, "err", err)
	case tag.RowsAffected() == 0:
		log.Warn("checkpoint not written: a newer fence holds this job",
			"fence", p.Fence, "last_index", c.LastIndex)
	}
}
