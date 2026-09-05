package topo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOverrides covers the file the node reads its naming map from.
//
// The cases that matter are the silent ones. A misspelled key would load as
// an empty map and every label on the host would fall back to the derived
// scheme, with nothing in the log to say the file was ignored; a duplicate
// token would load fine and fail every discovery pass instead of the process
// that started with it.
func TestLoadOverrides(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "overrides.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("empty path is no overrides", func(t *testing.T) {
		ov, err := LoadOverrides("")
		if err != nil {
			t.Fatal(err)
		}
		if len(ov.HubTokens) != 0 || len(ov.SlotLabels) != 0 {
			t.Errorf("got %+v, want an empty map", ov)
		}
	})

	t.Run("a well-formed file loads sanitised", func(t *testing.T) {
		p := write(t, `{
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
		// Sanitised on the way in, so what the summary and the labeler see is
		// the same string.
		if got := ov.HubTokens["3-1.5"]; got != "4" {
			t.Errorf("HubTokens[3-1.5] = %q, want the trimmed 4", got)
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
	})

	t.Run("an unknown key is refused, not skipped", func(t *testing.T) {
		p := write(t, `{"hubTokens": {"3-1.4": "3"}}`)
		_, err := LoadOverrides(p)
		if err == nil {
			t.Fatal("a file with a misspelled key loaded as an empty map")
		}
		if !strings.Contains(err.Error(), "hubTokens") {
			t.Errorf("refusal does not name the unknown key: %v", err)
		}
	})

	t.Run("a duplicate hub token fails at load", func(t *testing.T) {
		p := write(t, `{"hub_tokens": {"3-1.4": "3", "3-1.5": "3"}}`)
		_, err := LoadOverrides(p)
		if err == nil {
			t.Fatal("two hubs claiming one token loaded")
		}
		for _, want := range []string{"3-1.4", "3-1.5", `"3"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not mention %s: %v", want, err)
			}
		}
	})

	t.Run("an empty token fails at load", func(t *testing.T) {
		p := write(t, `{"hub_tokens": {"3-1.4": "---"}}`)
		if _, err := LoadOverrides(p); err == nil || !strings.Contains(err.Error(), "empty after sanitising") {
			t.Errorf("a token that sanitises to nothing loaded: %v", err)
		}
	})

	t.Run("trailing data is refused", func(t *testing.T) {
		p := write(t, `{"hub_tokens": {"3-1.4": "3"}} {"hub_tokens": {"3-1.4": "4"}}`)
		if _, err := LoadOverrides(p); err == nil || !strings.Contains(err.Error(), "trailing") {
			t.Errorf("a file holding two objects loaded the first and dropped the second: %v", err)
		}
	})

	t.Run("malformed json names the file", func(t *testing.T) {
		p := write(t, `{"hub_tokens": {`)
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
