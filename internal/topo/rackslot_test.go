package topo

// What these tests protect: a rack_slot names ONE socket, and it is derived
// from nothing that can change behind an operator's back. The hub token is a
// function of the USB path — not of enumeration order, not of how many hubs
// exist — and an override map that would make two sockets answer to one label
// is refused before a label is written.

import (
	"strings"
	"testing"
)

// TestLabelerRendersRackUnitHubPort: the default scheme, the two override
// levels, and what happens when the host has no rack coordinates.
//
// Falsify: in HubOrdinal, replace "-" with "" instead of "." — "3-1.4" and
// "31.4" then collide with "31-4", and the derived label loses its bus.
func TestLabelerRendersRackUnitHubPort(t *testing.T) {
	t.Parallel()

	l, err := NewLabeler("host-a", "r2", 14, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Slot("3-1.4", "3-1.4.5", 5); got != "R2-U14-H3.1.4-P5" {
		t.Errorf("derived label = %q", got)
	}
	if got := l.Slot("3-0", "3-2", 2); got != "R2-U14-H3.0-P2" {
		t.Errorf("root hub label = %q", got)
	}

	pinned, err := NewLabeler("host-a", "2", 7, Overrides{
		HubTokens:  map[string]string{"3-1.4": "3"},
		SlotLabels: map[string]string{"3-1.4.6": " shelf 9 / socket B "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pinned.Slot("3-1.4", "3-1.4.5", 5); got != "R2-U07-H3-P5" {
		t.Errorf("hub token override = %q", got)
	}
	if got := pinned.Slot("3-1.4", "3-1.4.6", 6); got != "shelf_9_socket_B" {
		t.Errorf("slot label override = %q, want it sanitised and used whole", got)
	}
	if got := pinned.Slot("3-1.5", "3-1.5.1", 1); got != "R2-U07-H3.1.5-P1" {
		t.Errorf("an un-overridden hub next to an overridden one = %q", got)
	}

	// No rack, no unit: the host id is the only place left to walk to.
	bare, err := NewLabeler(" node-07 ", "", 0, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.Slot("1-2", "1-2.3", 3); got != "node-07-H1.2-P3" {
		t.Errorf("label without rack coordinates = %q", got)
	}
	named, err := NewLabeler("node-07", "DC1-A7", 42, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := named.Slot("1-2", "1-2.3", 3); got != "DC1-A7-U42-H1.2-P3" {
		t.Errorf("label with a named rack = %q", got)
	}

	if _, err := NewLabeler("  ", "r1", 1, Overrides{}); err == nil {
		t.Error("a labeler without a host id was accepted")
	}
}

// TestNewLabelerRefusesOverridesThatCollide: two hubs answering to "H3" is
// worse than no label, and the refusal is deterministic so an operator can
// fix the map from the message.
//
// Falsify: delete the `seen` check in NewLabeler's HubTokens loop.
func TestNewLabelerRefusesOverridesThatCollide(t *testing.T) {
	t.Parallel()

	_, err := NewLabeler("h", "r1", 1, Overrides{HubTokens: map[string]string{"3-1": "3", "3-2": "3"}})
	if err == nil || !strings.Contains(err.Error(), `"3"`) || !strings.Contains(err.Error(), "3-1") || !strings.Contains(err.Error(), "3-2") {
		t.Errorf("duplicate hub tokens: %v", err)
	}
	_, err = NewLabeler("h", "r1", 1, Overrides{SlotLabels: map[string]string{"3-1.1": "A1", "3-1.2": "A1"}})
	if err == nil || !strings.Contains(err.Error(), `"A1"`) {
		t.Errorf("duplicate slot labels: %v", err)
	}
	_, err = NewLabeler("h", "r1", 1, Overrides{HubTokens: map[string]string{"3-1": " -- "}})
	if err == nil || !strings.Contains(err.Error(), "empty after sanitising") {
		t.Errorf("a token that sanitises to nothing: %v", err)
	}
	_, err = NewLabeler("h", "r1", 1, Overrides{SlotLabels: map[string]string{"3-1.1": "!!!"}})
	if err == nil || !strings.Contains(err.Error(), "empty after sanitising") {
		t.Errorf("a label that sanitises to nothing: %v", err)
	}

	// The same bad map reports the same collision every time.
	first := ""
	for i := 0; i < 20; i++ {
		_, err := NewLabeler("h", "r1", 1, Overrides{HubTokens: map[string]string{"3-3": "x", "3-1": "x", "3-2": "x"}})
		if err == nil {
			t.Fatal("three hubs on one token were accepted")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("the error changed between runs of the same config:\n%s\n%s", first, err)
		}
	}
}

// TestLabelerCheckCatchesAnOverrideCollidingWithADerivedToken: NewLabeler can
// only see the overrides. {"3-1.4": "3.1"} is fine on its own and wrong on a
// host that also has the hub "3-1", whose derived token is "3.1".
//
// Falsify: make Check return nil.
func TestLabelerCheckCatchesAnOverrideCollidingWithADerivedToken(t *testing.T) {
	t.Parallel()

	l, err := NewLabeler("h", "r1", 1, Overrides{HubTokens: map[string]string{"3-1.4": "3.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Check([]string{"3-1.4", "3-2"}); err != nil {
		t.Errorf("no collision on this host, got %v", err)
	}
	err = l.Check([]string{"3-2", "3-1.4", "3-1"})
	if err == nil || !strings.Contains(err.Error(), "H3.1") || !strings.Contains(err.Error(), "3-1.4") {
		t.Errorf("override colliding with a derived token: %v", err)
	}
}

// TestHubOrdinalIsInjective: the token is the path with the bus separator
// turned into a dot, so two positions can never share one and no other hub's
// existence changes it.
func TestHubOrdinalIsInjective(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"3-1.4": "3.1.4", "3-0": "3.0", "12-3": "12.3", "": "0"}
	seen := map[string]string{}
	for in, want := range cases {
		got := HubOrdinal(in)
		if got != want {
			t.Errorf("HubOrdinal(%q) = %q, want %q", in, got, want)
		}
		if other, dup := seen[got]; dup && other != in && got != "0" {
			t.Errorf("HubOrdinal(%q) and HubOrdinal(%q) both give %q", in, other, got)
		}
		seen[got] = in
	}
}

// TestRackAndUnitFieldConventions: numeric racks get the R prefix however
// they were written, named racks keep their name, and units are zero-padded
// so labels sort the way shelves are stacked.
func TestRackAndUnitFieldConventions(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"1": "R1", "r1": "R1", "R1": "R1", " 12 ": "R12", "DC1-A7": "DC1-A7", "rack b": "RACK_B", "": ""} {
		if got := rackField(in); got != want {
			t.Errorf("rackField(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[int]string{7: "U07", 14: "U14", 100: "U100"} {
		if got := unitField(in); got != want {
			t.Errorf("unitField(%d) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeField("a-b c/d"); got != "a_b_c_d" {
		t.Errorf("sanitizeField = %q; a dash separates fields and may not appear inside one", got)
	}
	if got := sanitizeLabel("__a-b__"); got != "a-b" {
		t.Errorf("sanitizeLabel = %q", got)
	}
}
