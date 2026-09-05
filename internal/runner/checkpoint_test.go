package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// Re-attaching is the mechanism, not an optimisation.
//
// A pod eviction is the most ordinary event in a Kubernetes control plane. The
// replacement process acquires the same lease by job id, gets the SAME device
// at the SAME fence, and must then answer a question the device cannot answer
// for it: which of these steps have already happened? Everything below is that
// answer, and the thing it must never do is repeat a side effect.

// schemaKinds is farm.step_kinds as the database holds it. The idempotent flag
// is read from the vocabulary rather than remembered in the test, so a change
// to the migration shows up here as a changed decision rather than as a test
// that quietly kept asserting yesterday's rules.
func schemaKinds() map[jobspec.Kind]kindInfo {
	out := make(map[jobspec.Kind]kindInfo, len(jobspec.Kinds()))
	for _, info := range jobspec.Kinds() {
		out[info.Kind] = kindInfo{Idempotent: info.Idempotent, NeedsArtifact: info.NeedsArtifact}
	}
	return out
}

// resumeSpec is a spec whose steps span the two classes that matter: one that
// may be repeated (uninstall) and two that may not (shell, shell_detached).
func resumeSpec() jobspec.Spec {
	return jobspec.New(
		jobspec.Step{ID: "clean", Payload: jobspec.Uninstall{Package: "com.example.app"}},
		jobspec.Step{ID: "seed", Payload: jobspec.Shell{Command: "am broadcast -a SEED"}},
		jobspec.Step{ID: "soak", Payload: jobspec.ShellDetached{
			Command: "sh /data/local/tmp/soak.sh", ResultPath: "/data/local/tmp/soak.rc", Handle: "soak"}},
		jobspec.Step{ID: "collect", Payload: jobspec.Pull{Path: "/data/local/tmp/soak.log", Artifact: "soak.log"}},
	)
}

func placement() Placement {
	return Placement{JobID: "job-1", DeviceID: "dev-1", Devpath: "usb:3-1.4", LeaseID: "lease-1", Fence: 42}
}

// ---------------------------------------------------------------------------
// Which placement does this progress describe?
// ---------------------------------------------------------------------------

// The fence is what makes the test trustworthy: it is monotonic and unique per
// lease, so a matching fence means no other holder has had the device in
// between. Nobody has reset it, nothing has been reallocated, and the state the
// checkpoint describes is still on the hardware in front of us.
func TestCheckpointBelongsOnlyToItsOwnPlacement(t *testing.T) {
	t.Parallel()

	p := placement()
	good := Checkpoint{Version: checkpointVersion, Attempt: 2, Fence: p.Fence, DeviceID: p.DeviceID}
	if !good.belongsTo(p) {
		t.Fatal("a checkpoint from this very placement was rejected")
	}

	for _, tc := range []struct {
		name string
		mut  func(*Checkpoint)
	}{
		{"a lower fence: somebody else has held the device since", func(c *Checkpoint) { c.Fence-- }},
		{"a higher fence: a newer attempt owns this job", func(c *Checkpoint) { c.Fence++ }},
		{"another device entirely", func(c *Checkpoint) { c.DeviceID = "dev-2" }},
		{"a version this binary does not understand", func(c *Checkpoint) { c.Version = checkpointVersion + 1 }},
		{"no attempt recorded", func(c *Checkpoint) { c.Attempt = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			if c.belongsTo(p) {
				t.Fatalf("resumed against %+v; starting over on a clean device is the safe answer", c)
			}
		})
	}
}

// A checkpoint that does not parse is treated as absent: the job starts over on
// a fresh placement, which is safe, rather than resuming against a structure
// this binary guessed at.
func TestUnreadableCheckpointIsTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{nil, {}, []byte("not json"), []byte(`{"version":`), []byte(`[]`)} {
		got := parseCheckpoint(raw)
		if got.belongsTo(placement()) {
			t.Fatalf("parseCheckpoint(%q) produced a resumable checkpoint", raw)
		}
	}

	want := Checkpoint{Version: checkpointVersion, Attempt: 3, Fence: 42, DeviceID: "dev-1"}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := parseCheckpoint(body); !got.belongsTo(placement()) || got.Attempt != 3 {
		t.Fatalf("a well-formed checkpoint did not round trip: %+v", got)
	}
}

// The hash pins the step LIST — position, id and kind — and deliberately
// ignores step arguments: changing a timeout or a package name does not
// invalidate the record of which steps already ran, but reordering or replacing
// them does.
func TestSpecHashPinsTheStepListAndNotItsArguments(t *testing.T) {
	t.Parallel()

	base := resumeSpec()
	same := resumeSpec()
	same.Steps[0].Payload = jobspec.Uninstall{Package: "com.example.other"}
	same.Steps[1].Timeout = jobspec.Duration(9 * time.Minute)
	if specHash(base) != specHash(same) {
		t.Fatal("changing a package name or a timeout invalidated the record of what already ran")
	}

	reordered := resumeSpec()
	reordered.Steps[0], reordered.Steps[1] = reordered.Steps[1], reordered.Steps[0]
	if specHash(base) == specHash(reordered) {
		t.Fatal("reordering the steps left the hash unchanged; a resume would land on the wrong step")
	}

	renamed := resumeSpec()
	renamed.Steps[2].ID = "soak2"
	if specHash(base) == specHash(renamed) {
		t.Fatal("renaming a step left the hash unchanged")
	}

	retyped := resumeSpec()
	retyped.Steps[1].Payload = jobspec.Assert{Probe: "true", Operator: jobspec.OpEQ, Value: ""}
	if specHash(base) == specHash(retyped) {
		t.Fatal("replacing a step's kind left the hash unchanged")
	}
}

// ---------------------------------------------------------------------------
// What a resume runs, skips and refuses
// ---------------------------------------------------------------------------

// A fresh placement runs everything. It gets a device that is not carrying our
// half-finished work, so there is nothing to skip and nothing to re-attach to.
func TestAFreshPlacementSkipsNothing(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	ckpt := Checkpoint{Completed: []CompletedStep{{Index: 0, ID: "clean"}, {Index: 1, ID: "seed"}}}
	pl, err := planResume(spec, ckpt, schemaKinds(), false)
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	for i := range spec.Steps {
		if pl.skip[i] {
			t.Fatalf("step %d was skipped on a placement that is not resuming", i)
		}
		if pl.reattach[i] {
			t.Fatalf("step %d would re-attach on a fresh placement", i)
		}
	}
}

// Steps recorded complete are skipped. This is the half of the resume that
// saves the work; the other half is the refusal below that protects it.
func TestResumeSkipsStepsAlreadyComplete(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	ckpt := Checkpoint{
		Version: checkpointVersion, Attempt: 1, Fence: 42, DeviceID: "dev-1",
		SpecHash:  specHash(spec),
		Completed: []CompletedStep{{Index: 0, ID: "clean"}, {Index: 1, ID: "seed"}},
	}
	pl, err := planResume(spec, ckpt, schemaKinds(), true)
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if !pl.skip[0] || !pl.skip[1] {
		t.Fatalf("completed steps were not skipped: %+v", pl.skip)
	}
	if pl.skip[2] || pl.skip[3] {
		t.Fatalf("a step that never ran was skipped: %+v", pl.skip)
	}
}

// A non-idempotent step that was interrupted is REFUSED rather than repeated.
// Re-running one would repeat a side effect that may well have already
// happened, and a job that fails with a clear message is strictly better than a
// farm that silently does things twice.
func TestResumeRefusesToRepeatAnInterruptedNonIdempotentStep(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	ckpt := Checkpoint{
		Version: checkpointVersion, Attempt: 1, Fence: 42, DeviceID: "dev-1",
		SpecHash:  specHash(spec),
		Completed: []CompletedStep{{Index: 0, ID: "clean"}},
		InFlight:  &InFlightStep{Index: 1, ID: "seed", Kind: jobspec.KindShell},
	}

	// The premise the refusal rests on: the vocabulary says a shell step may
	// not be repeated. If that ever changes in farm.step_kinds this test should
	// stop meaning what it says, loudly.
	if schemaKinds()[jobspec.KindShell].Idempotent {
		t.Fatal("farm.step_kinds now calls 'shell' idempotent; this test no longer tests a refusal")
	}

	pl, err := planResume(spec, ckpt, schemaKinds(), true)
	if err == nil {
		t.Fatal("planResume agreed to re-run an interrupted shell step")
	}
	if !strings.Contains(err.Error(), "not idempotent") {
		t.Fatalf("error = %q, want it to say why the step may not be repeated", err)
	}
	if pl.refusedIndex != 1 {
		t.Fatalf("refusedIndex = %d, want 1 so the step's own row records the refusal", pl.refusedIndex)
	}
}

// An interrupted step the vocabulary calls idempotent simply runs again.
func TestResumeRerunsAnInterruptedIdempotentStep(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	if !schemaKinds()[jobspec.KindUninstall].Idempotent {
		t.Fatal("farm.step_kinds no longer calls 'uninstall' idempotent")
	}
	ckpt := Checkpoint{
		Version: checkpointVersion, Attempt: 1, Fence: 42, DeviceID: "dev-1",
		SpecHash: specHash(spec),
		InFlight: &InFlightStep{Index: 0, ID: "clean", Kind: jobspec.KindUninstall},
	}
	pl, err := planResume(spec, ckpt, schemaKinds(), true)
	if err != nil {
		t.Fatalf("planResume refused a repeatable step: %v", err)
	}
	if pl.skip[0] {
		t.Fatal("an interrupted idempotent step was skipped rather than re-run")
	}
	if pl.reattach[0] {
		t.Fatal("an uninstall was treated as a detached command")
	}
}

// A shell_detached step that was in flight is the case this whole design exists
// for: its work is running ON THE DEVICE, so the correct move is neither to
// skip it nor to start a second copy, but to go and look for it.
func TestResumeReattachesToDetachedWorkRatherThanStartingASecondCopy(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	if schemaKinds()[jobspec.KindShellDetached].Idempotent {
		t.Fatal("farm.step_kinds now calls 'shell_detached' idempotent; re-attaching would be pointless")
	}
	ckpt := Checkpoint{
		Version: checkpointVersion, Attempt: 1, Fence: 42, DeviceID: "dev-1",
		SpecHash:  specHash(spec),
		Completed: []CompletedStep{{Index: 0, ID: "clean"}, {Index: 1, ID: "seed"}},
		InFlight:  &InFlightStep{Index: 2, ID: "soak", Kind: jobspec.KindShellDetached},
	}
	pl, err := planResume(spec, ckpt, schemaKinds(), true)
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if !pl.reattach[2] {
		t.Fatal("a detached step that was in flight would be started a second time")
	}
	if pl.skip[2] {
		t.Fatal("a detached step that was in flight was skipped without looking for it")
	}
}

// The in-flight marker goes down before a step and is cleared after it. A crash
// in between leaves both records, and 'completed' is the stronger statement.
func TestCompletedBeatsAStaleInFlightMarker(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	ckpt := Checkpoint{
		Version: checkpointVersion, Attempt: 1, Fence: 42, DeviceID: "dev-1",
		SpecHash:  specHash(spec),
		Completed: []CompletedStep{{Index: 1, ID: "seed"}},
		InFlight:  &InFlightStep{Index: 1, ID: "seed", Kind: jobspec.KindShell},
	}
	pl, err := planResume(spec, ckpt, schemaKinds(), true)
	if err != nil {
		t.Fatalf("planResume refused a step it had already completed: %v", err)
	}
	if !pl.skip[1] {
		t.Fatal("a completed step was re-run because a stale in-flight marker survived")
	}
	if pl.reattach[1] {
		t.Fatal("a completed step would be re-attached to")
	}
}

// A resume whose spec no longer describes the same work is refused outright.
// Resuming at index 4 of a list that no longer has the same steps at 0..3 is
// how a job silently skips its own setup.
func TestResumeRefusesASpecThatChangedUnderneathIt(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	kinds := schemaKinds()

	for _, tc := range []struct {
		name string
		ckpt Checkpoint
		want string
	}{
		{
			name: "the step list was edited",
			ckpt: Checkpoint{SpecHash: "0000", Attempt: 4},
			want: "spec changed",
		},
		{
			name: "progress names a step index the spec no longer has",
			ckpt: Checkpoint{SpecHash: specHash(spec), Completed: []CompletedStep{{Index: 9, ID: "gone"}}},
			want: "the spec has 4 steps",
		},
		{
			name: "progress names a different step at that index",
			ckpt: Checkpoint{SpecHash: specHash(spec), Completed: []CompletedStep{{Index: 0, ID: "not-clean"}}},
			want: "refusing to resume",
		},
		{
			name: "an in-flight index the spec no longer has",
			ckpt: Checkpoint{SpecHash: specHash(spec), InFlight: &InFlightStep{Index: 12, ID: "gone"}},
			want: "in flight",
		},
		{
			name: "a different step id at the in-flight index",
			ckpt: Checkpoint{SpecHash: specHash(spec), InFlight: &InFlightStep{Index: 2, ID: "elsewhere"}},
			want: "refusing to resume",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planResume(spec, tc.ckpt, kinds, true)
			if err == nil {
				t.Fatal("planResume resumed into a spec that no longer describes the recorded work")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The default for a kind nobody has classified is "may not be repeated". A step
// must never be re-run on a guess.
func TestAnUnclassifiedKindIsNeverAssumedRepeatable(t *testing.T) {
	t.Parallel()

	st := jobspec.Step{ID: "x", Payload: jobspec.Shell{Command: "true"}}
	if stepIsIdempotent(st, map[jobspec.Kind]kindInfo{}) {
		t.Fatal("a kind missing from farm.step_kinds was assumed repeatable")
	}
	if !stepIsIdempotent(st, map[jobspec.Kind]kindInfo{jobspec.KindShell: {Idempotent: true}}) {
		t.Fatal("the vocabulary's own answer was ignored")
	}

	// planResume's idem map is what decides whether the checkpoint has to go
	// down before a step runs, so it must carry the same answer.
	spec := resumeSpec()
	pl, err := planResume(spec, Checkpoint{}, schemaKinds(), false)
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	for i, st := range spec.Steps {
		want := schemaKinds()[st.Kind()].Idempotent
		if pl.idempotent(i) != want {
			t.Fatalf("step %d (%s): idempotent = %t, want the vocabulary's %t",
				i, st.Kind(), pl.idempotent(i), want)
		}
	}
}

// ---------------------------------------------------------------------------
// Recording progress
// ---------------------------------------------------------------------------

func TestMarkingProgressIsIdempotentAndClearsTheInFlightMarker(t *testing.T) {
	t.Parallel()

	spec := resumeSpec()
	c := newCheckpoint(3, placement(), spec)
	if c.Version != checkpointVersion || c.Attempt != 3 || c.Fence != 42 || c.LastIndex != -1 {
		t.Fatalf("newCheckpoint = %+v", c)
	}
	if c.SpecHash != specHash(spec) {
		t.Fatal("newCheckpoint did not pin the step list")
	}

	c.markInFlight(1, spec.Steps[1])
	if c.InFlight == nil || c.InFlight.Index != 1 || c.InFlight.Kind != jobspec.KindShell {
		t.Fatalf("markInFlight = %+v", c.InFlight)
	}
	if c.LastIndex != 1 {
		t.Fatalf("LastIndex = %d, want 1", c.LastIndex)
	}

	c.markDone(1, spec.Steps[1])
	if c.InFlight != nil {
		t.Fatal("markDone left the in-flight marker behind; a resume would refuse a finished step")
	}
	if len(c.Completed) != 1 || c.Completed[0].ID != "seed" {
		t.Fatalf("Completed = %+v", c.Completed)
	}

	// A retry inside the same step must not grow the list without bound.
	c.markDone(1, spec.Steps[1])
	c.markDone(1, spec.Steps[1])
	if len(c.Completed) != 1 {
		t.Fatalf("Completed = %+v after repeated markDone; the row would grow forever", c.Completed)
	}
}
