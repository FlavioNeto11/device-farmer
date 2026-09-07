package ui

// A gloss that names a word the glossary does not have is not an error. It is a
// plain word.
//
// terms.js is deliberate about that: `term(id, label)` returns a text node for
// an unknown id, because it is called from render paths that repaint under a
// live event stream and a glossary that can take a view down is worse than a
// word without a dotted underline. That is the right call, and it is also why
// this file exists — the failure mode it buys is silence. The word renders. The
// column heads correctly. Nothing appears in the console. The only symptom is
// that the definition an operator was promised is not there, and the only
// person who finds out is the one who did not already know the word.
//
// It had already happened. Three of the four glosses in assets/app.js were
// calling ids that are not in the table: `tier` for what terms.js calls `rung`,
// `blast_radius` for `blastRadius`, `disruption_policy` for `disruptionPolicy`.
// The snake_case spellings are the database's, which is exactly why they were
// reached for — and the table is keyed by the JavaScript name. Three words on
// the Recovery ladder and the Jobs table shipped with no definition attached.
//
// SCOPE, AND WHAT IS DELIBERATELY OUTSIDE IT.
//
// This checks assets/app.js. It does not check assets/device.js, and that is
// not because device.js is clean — it is not. The device sheet calls the same
// glossary through its own `glossed(o.term, label)` wrapper with five ids that
// are not in the TERMS table either:
//
//	device.js:301  term: 'admin_state'   -> the table has 'adminState'
//	device.js:440  term: 'slot'          -> the table has 'slotState'
//	device.js:532  term: 'health'        -> the table has no such entry
//	device.js:540  term: 'slot'          -> the table has 'slotState'
//	device.js:561  term: 'lease'         -> the table has 'holder'
//
// Those five words render on the Overview, Health and Lease tabs with no
// underline and no definition, exactly as the three in app.js did. device.js
// belongs to the unit that wrote the device sheet, and a unit that does not own
// a file does not get to fail its build over it — so the finding is reported in
// the pull request rather than asserted here. Widening this test to cover
// device.js is the right change to make in the same commit that fixes them.

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryGlossedWordInAppNamesATermThatExists.
//
// Falsify: change `term('rung'` back to `term('tier'` in assets/app.js.
func TestEveryGlossedWordInAppNamesATermThatExists(t *testing.T) {
	known := map[string]bool{}
	for _, g := range glossary(t) {
		known[g.id] = true
	}

	src := appSourceForGloss(t)

	// Both spellings this file uses: the direct call, and termLabel(), which is
	// app.js's own guard wrapper around it. glossJobForm's table is a third
	// shape — ['#sel', 'id', label] — and is matched separately below.
	calls := regexp.MustCompile(`\b(?:term|termLabel)\('([A-Za-z_][\w]*)'`)
	table := regexp.MustCompile(`\['#[\w-]+',\s*'([A-Za-z_][\w]*)'`)

	seen := map[string]int{}
	for _, re := range []*regexp.Regexp{calls, table} {
		for _, loc := range re.FindAllStringSubmatchIndex(src, -1) {
			id := src[loc[2]:loc[3]]
			// The definition of termLabel itself, `term(id, label)`, names a
			// parameter rather than a term.
			if id == "id" {
				continue
			}
			if _, ok := seen[id]; !ok {
				seen[id] = strings.Count(src[:loc[0]], "\n") + 1
			}
		}
	}

	// The floor. Without it a renamed helper turns this into a test that passes
	// because it examined nothing, which is the failure every guard test in
	// this package is written against.
	if len(seen) < 6 {
		t.Fatalf("found only %d glossed word(s) in assets/app.js. The five views this file "+
			"draws gloss more than that, so the call shape changed and this test is now blind. "+
			"found: %v", len(seen), sortedKeys(seen))
	}

	for _, id := range sortedKeys(seen) {
		if known[id] {
			continue
		}
		t.Errorf("assets/app.js:%d glosses %q, which is not an id in the TERMS table in "+
			"assets/terms.js.\n\nterm() returns the plain word for an unknown id rather than "+
			"throwing, so this renders correctly and silently does nothing: the reader sees the "+
			"word with no dotted underline and no definition behind it. The table is keyed by "+
			"the JavaScript name, not the database's — `rung`, `blastRadius`, "+
			"`disruptionPolicy`. Known ids: %v", seen[id], id, sortedKeys(known))
	}
}

func appSourceForGloss(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read the embedded assets/app.js: %v", err)
	}
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
