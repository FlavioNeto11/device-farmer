package jobspec

import (
	"encoding/json"
	"testing"
	"time"
)

// TestPointerPayloadsBehaveLikeValues pins a hazard the type system cannot
// remove: both Shell and *Shell satisfy Payload, because every payload's
// Kind() has a value receiver and a pointer's method set includes its value
// receivers. Go offers no way to exclude the pointer form, so a spec written
// with pointers COMPILES — and therefore it must also marshal, validate, and
// mean exactly what the value form means.
//
// Both halves of that were broken and were found by running the demo rather
// than by reading the code. Marshalling a pointer payload failed at submission
// time with "not a step payload". Validation was worse: its type switch listed
// only the value cases, so a pointer payload fell through to the default arm
// and every rule written for that kind was skipped. A spec that could not run
// would have been accepted unchecked and only failed once it reached a device
// holding a live lease.
func TestPointerPayloadsBehaveLikeValues(t *testing.T) {
	t.Parallel()

	const cmd = "getprop sys.boot_completed"
	byValue := New(Step{
		ID:      "probe",
		Timeout: Duration(10 * time.Second),
		Payload: Shell{Command: cmd},
	})
	byPointer := New(Step{
		ID:      "probe",
		Timeout: Duration(10 * time.Second),
		Payload: &Shell{Command: cmd},
	})

	wantJSON, err := json.Marshal(byValue)
	if err != nil {
		t.Fatalf("the value form failed to marshal: %v", err)
	}
	gotJSON, err := json.Marshal(byPointer)
	if err != nil {
		t.Fatalf("the pointer form failed to marshal: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("pointer and value forms marshal differently\n pointer: %s\n value:   %s",
			gotJSON, wantJSON)
	}

	if err := Validate(byValue); err != nil {
		t.Fatalf("the value form failed to validate: %v", err)
	}
	if err := Validate(byPointer); err != nil {
		t.Errorf("the pointer form failed to validate while the value form passed: %v", err)
	}

	// The rules must actually RUN for a pointer payload, not merely fail to
	// fire. An empty command is invalid; if the switch missed the pointer case
	// this passes, and that silence is the dangerous half of the bug.
	unrunnable := New(Step{
		ID:      "probe",
		Timeout: Duration(time.Second),
		Payload: &Shell{Command: ""},
	})
	if err := Validate(unrunnable); err == nil {
		t.Error("an empty shell command passed validation through a pointer payload: " +
			"the rules are being skipped, which is how a spec that cannot run " +
			"reaches a device that is already leased")
	}
}

// TestNilPointerPayloadIsRejected covers the other half of accepting pointers:
// a typed nil satisfies the interface too, and dereferencing it would panic
// inside the marshaller. It must be reported, not crash the API handler that
// is validating somebody's submission.
func TestNilPointerPayloadIsRejected(t *testing.T) {
	t.Parallel()

	var nilShell *Shell
	s := New(Step{ID: "probe", Timeout: Duration(time.Second), Payload: nilShell})

	if _, err := json.Marshal(s); err == nil {
		t.Error("a nil pointer payload marshalled successfully; it carries no command " +
			"and would reach a device as an empty step")
	}
	if err := Validate(s); err == nil {
		t.Error("a nil pointer payload passed validation")
	}
}
