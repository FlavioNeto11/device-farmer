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
