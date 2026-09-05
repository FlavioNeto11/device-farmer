package topo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLoadOverrides covers the file the node reads its naming map from.
//
// The cases that matter are the silent ones. A misspelled key would load as
// an empty map and every label on the host would fall back to the derived
// scheme, with nothing in the log to say the file was ignored; a file that
// was hand-edited into two objects would load the first and drop the second.
// What the values MEAN is not judged here — see TestNewJudgesTheOverridesOnce.
func TestLoadOverrides(t *testing.T) {
	t.Run("empty path is no overrides", func(t *testing.T) {
		ov, err := LoadOverrides("")
		if err != nil {
			t.Fatal(err)
		}
		if len(ov.HubTokens) != 0 || len(ov.SlotLabels) != 0 {
			t.Errorf("got %+v, want an empty map", ov)
		}
	})

	t.Run("a well-formed file loads as written", func(t *testing.T) {
		p := writeOverrides(t, `{
			"hub_tokens":  {"3-1.4": "3", "3-1.5": " 4 "},
			"slot_labels": {"3-1.4.2": "R2-U14-H3-P2"}
		}`)
		ov, err := LoadOverrides(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := ov.HubTokens["3-1.4"]; got != "3" {
			t.Errorf("HubTokens[3-1.4] = %q, want 3", got)
		}
		// As written: the loader reads, it does not sanitise. That happens
		// once, in New, so a map from a file and a map built in code are
		// judged by the same code path.
		if got := ov.HubTokens["3-1.5"]; got != " 4 " {
			t.Errorf("HubTokens[3-1.5] = %q, want the untouched \" 4 \"", got)
		}
		if got := ov.SlotLabels["3-1.4.2"]; got != "R2-U14-H3-P2" {
			t.Errorf("SlotLabels[3-1.4.2] = %q", got)
		}
		// And it renders, which is the whole point of the file.
		l, err := NewLabeler("h1", "2", 14, ov)
		if err != nil {
			t.Fatal(err)
		}
		if got := l.Slot("3-1.4", "3-1.4.5", 5); got != "R2-U14-H3-P5" {
			t.Errorf("Slot() = %q, want R2-U14-H3-P5", got)
		}
		if got := l.Slot("3-1.5", "3-1.5.2", 2); got != "R2-U14-H4-P2" {
			t.Errorf("Slot() = %q, want the trimmed token in R2-U14-H4-P2", got)
		}
	})

	t.Run("an unknown key is refused, not skipped", func(t *testing.T) {
		p := writeOverrides(t, `{"hubTokens": {"3-1.4": "3"}}`)
		_, err := LoadOverrides(p)
		if err == nil {
			t.Fatal("a file with a misspelled key loaded as an empty map")
		}
		if !strings.Contains(err.Error(), "hubTokens") {
			t.Errorf("refusal does not name the unknown key: %v", err)
		}
	})

	t.Run("trailing data is refused", func(t *testing.T) {
		p := writeOverrides(t, `{"hub_tokens": {"3-1.4": "3"}} {"hub_tokens": {"3-1.4": "4"}}`)
		if _, err := LoadOverrides(p); err == nil || !strings.Contains(err.Error(), "trailing") {
			t.Errorf("a file holding two objects loaded the first and dropped the second: %v", err)
		}
	})

	t.Run("malformed json names the file", func(t *testing.T) {
		p := writeOverrides(t, `{"hub_tokens": {`)
		_, err := LoadOverrides(p)
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("refusal does not name the file: %v", err)
		}
	})

	t.Run("a missing file is an error", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "absent.json")
		if _, err := LoadOverrides(p); err == nil {
			t.Error("a path that does not exist loaded as no overrides")
		}
	})
}

// TestNewJudgesTheOverridesOnce. The naming map is sanitised and checked for
// collisions in exactly one place, New, and the passes build their labelers
// from the map New kept. Before this, LoadOverrides normalised the map at
// startup and Once normalised it again every five minutes — the same file
// judged twice, with the second verdict reported as a failed scan.
//
// A duplicate token therefore has to stop the process that was given the
// file; a pass has nothing left to refuse in the map.
func TestNewJudgesTheOverridesOnce(t *testing.T) {
	base := func(ov Overrides) Config {
		return Config{
			Pool:      lazyPool(t),
			HostID:    "h1",
			Source:    FromFS(fstest.MapFS{}, "test"),
			Overrides: ov,
		}
	}

	t.Run("a duplicate hub token is refused by New", func(t *testing.T) {
		_, err := New(base(Overrides{HubTokens: map[string]string{"3-1.4": "3", "3-1.5": "3"}}))
		if err == nil {
			t.Fatal("two hubs claiming one token built a discoverer")
		}
		for _, want := range []string{"Config.Overrides", "3-1.4", "3-1.5", `"3"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not mention %s: %v", want, err)
			}
		}
	})

	t.Run("a token that sanitises to nothing is refused by New", func(t *testing.T) {
		_, err := New(base(Overrides{HubTokens: map[string]string{"3-1.4": "---"}}))
		if err == nil || !strings.Contains(err.Error(), "empty after sanitising") {
			t.Errorf("a token that sanitises to nothing built a discoverer: %v", err)
		}
	})

	t.Run("a duplicate slot label is refused by New", func(t *testing.T) {
		_, err := New(base(Overrides{SlotLabels: map[string]string{
			"3-1.4.1": "R1-U01-H1-P1", "3-1.4.2": "R1-U01-H1-P1",
		}}))
		if err == nil || !strings.Contains(err.Error(), "two sockets under one label") {
			t.Errorf("two sockets under one label built a discoverer: %v", err)
		}
	})

	t.Run("the map a pass uses is the sanitised one", func(t *testing.T) {
		d, err := New(base(Overrides{
			HubTokens:  map[string]string{"3-1.5": " 4 "},
			SlotLabels: map[string]string{"3-1.4.2": " R2-U14-H3-P2 "},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got := d.overrides.HubTokens["3-1.5"]; got != "4" {
			t.Errorf("Discoverer holds HubTokens[3-1.5] = %q, want the sanitised 4", got)
		}
		if got := d.overrides.SlotLabels["3-1.4.2"]; got != "R2-U14-H3-P2" {
			t.Errorf("Discoverer holds SlotLabels[3-1.4.2] = %q, want it trimmed", got)
		}
		// What Once does with it, minus the database: the labeler is built
		// from the kept map as it is, and renders the sanitised token.
		l := newLabeler(d.cfg.HostID, "2", 14, d.overrides)
		if got := l.Slot("3-1.5", "3-1.5.2", 2); got != "R2-U14-H4-P2" {
			t.Errorf("Slot() = %q, want R2-U14-H4-P2", got)
		}
		if got := l.Slot("3-1.4", "3-1.4.2", 2); got != "R2-U14-H3-P2" {
			t.Errorf("Slot() = %q, want the overridden R2-U14-H3-P2", got)
		}
	})

	t.Run("a file with a collision loads and then fails New", func(t *testing.T) {
		// The path the node role takes: read the file, hand the map to New.
		// The file is well-formed JSON, so the loader has no complaint; the
		// discoverer is what refuses to exist.
		p := writeOverrides(t, `{"hub_tokens": {"3-1.4": "3", "3-1.5": "3"}}`)
		ov, err := LoadOverrides(p)
		if err != nil {
			t.Fatalf("the loader judged the values, which is New's job: %v", err)
		}
		if _, err := New(base(ov)); err == nil {
			t.Fatal("a colliding map read from a file built a discoverer")
		}
	})
}

// lazyPool is a pool that never connects: pgx v5 dials on first use, and
// nothing here uses it. New needs a non-nil pool, not a database.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func writeOverrides(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------------------
// The labelling scheme itself (U14). Above: the override FILE and the
// judgement made on it once, at construction (U12).
// ---------------------------------------------------------------------------

// What these tests protect: a rack_slot names ONE socket, and it is derived
// from nothing that can change behind an operator's back. The hub token is a
// function of the USB path — not of enumeration order, not of how many hubs
// exist — and an override map that would make two sockets answer to one label
// is refused before a label is written.

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
