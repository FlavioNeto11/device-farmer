package api

import (
	"strings"
	"testing"
	"time"
)

// TestReaperNoteSaysRefusedBeforeItSaysQuiesced.
//
// The note is the sentence an operator reads instead of the columns, and a
// refusal to arm has to be the first thing it says. A quiesce window ends by
// itself; a refusal ends only when the unbeaten component writes a row, and a
// note that led with "quiesced for another 900s" would send the operator away
// to wait for something that is never going to happen.
func TestReaperNoteSaysRefusedBeforeItSaysQuiesced(t *testing.T) {
	refusal := "refused to arm: watched component(s) ghost have never written a heartbeat"
	at := time.Date(2026, 9, 5, 3, 14, 0, 0, time.UTC)
	v := &reaperStateView{
		Enabled:            true,
		QuiesceRemaining:   900,
		QuiesceUntil:       at.Add(15 * time.Minute),
		Refusal:            &refusal,
		RefusedAt:          &at,
		WatchedComponents:  []string{"reaper", "api", "scheduler", "jobrunner", "ghost"},
		UnbeatenComponents: []string{"ghost"},
		ReclaimableNow:     3,
	}

	note := reaperNote(v)
	for _, want := range []string{"REFUSED TO ARM", "ghost", "reclaims nothing", "FARM_REAPER_COMPONENTS"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "QUIESCED") {
		t.Errorf("note leads with the quiesce window while a refusal stands:\n%s", note)
	}

	// Once the component has beaten, the note says the reaper will clear it
	// by itself rather than telling the operator to do something.
	v.UnbeatenComponents = nil
	if note := reaperNote(v); !strings.Contains(note, "re-arms by itself") {
		t.Errorf("note does not say the refusal clears on its own once every component has beaten:\n%s", note)
	}

	// With no refusal standing but a watched name that has never beaten, an
	// ARMED note warns that the next arm will refuse: the operator reading
	// it is usually about to press enable.
	v.Refusal, v.RefusedAt = nil, nil
	v.QuiesceRemaining = 0
	v.UnbeatenComponents = []string{"ghost"}
	note = reaperNote(v)
	if !strings.Contains(note, "ARMED") || !strings.Contains(note, "NEXT arm will refuse") {
		t.Errorf("an armed reaper with an unbeaten watched component was not warned about:\n%s", note)
	}
}
