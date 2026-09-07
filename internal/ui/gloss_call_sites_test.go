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
import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// SCOPE.
//
// Every asset that glosses a word: app.js, device.js and fleet.js. It did not
// always. It was written to check app.js alone, with the five dead ids in
// device.js listed in this comment and reported in a pull request instead,
// because a unit that does not own a file does not get to fail its build over
// it. That comment ended by saying widening this test was the right change to
// make in the same commit that fixed them, and this is that commit:
//
//	device.js:301  term: 'admin_state'  -> the table has 'adminState'
//	device.js:440  term: 'slot'         -> no entry; the gloss was dropped, because
//	                                       inventing a term to satisfy a call site
//	                                       is the wrong way round
//	device.js:532  term: 'health'       -> no entry; 'health' was ADDED to the table
//	device.js:540  term: 'slot'         -> the table has 'slotState'
//	device.js:561  term: 'lease'        -> no entry; 'lease' was ADDED to the table
//
// Two of those five were not typos. `lease` and `health` are the two words this
// whole product turns on, and the glossary did not define either — which is why
// naming them failed silently rather than loudly. The fix for a missing
// definition is the definition.
//
// Falsify: change `term('rung'` back to `term('tier'` in assets/app.js.
func TestEveryGlossedWordInAppNamesATermThatExists(t *testing.T) {
	known := map[string]bool{}
	for _, g := range glossary(t) {
		known[g.id] = true
	}

	sources := glossSources(t)

	// Both spellings this file uses: the direct call, and termLabel(), which is
	// app.js's own guard wrapper around it. glossJobForm's table is a third
	// shape — ['#sel', 'id', label] — and is matched separately below.
	calls := regexp.MustCompile(`\b(?:term|termLabel)\('([A-Za-z_][\w]*)'`)
	table := regexp.MustCompile(`\['#[\w-]+',\s*'([A-Za-z_][\w]*)'`)
	// The device sheet does not call term() at its sites. It passes the id
	// through kv(label, value, {raw, term}) and a glossed() wrapper unwraps it,
	// so the id is only ever written as an object field. A pattern that knew
	// about term( alone read that file as having no glosses at all.
	field := regexp.MustCompile(`\bterm:\s*'([A-Za-z_][\w]*)'`)

	type site struct {
		file string
		line int
	}
	seen := map[string]site{}
	for _, f := range sources {
		for _, re := range []*regexp.Regexp{calls, table, field} {
			for _, loc := range re.FindAllStringSubmatchIndex(f.src, -1) {
				id := f.src[loc[2]:loc[3]]
				// The definition of termLabel itself, `term(id, label)`, and
				// the wrapper's own `{ term }` destructuring, name a parameter
				// rather than a term.
				if id == "id" || id == "term" {
					continue
				}
				if _, ok := seen[id]; !ok {
					seen[id] = site{f.name, strings.Count(f.src[:loc[0]], "\n") + 1}
				}
			}
		}
	}

	// The floor. Without it a renamed helper turns this into a test that passes
	// because it examined nothing, which is the failure every guard test in
	// this package is written against.
	if len(seen) < 12 {
		t.Fatalf("found only %d glossed word(s) across %d assets. The five views and the device "+
			"sheet gloss more than that between them, so a call shape changed and this test is "+
			"now blind. found: %v", len(seen), len(sources), sortedKeys(seen))
	}

	for _, id := range sortedKeys(seen) {
		if known[id] {
			continue
		}
		t.Errorf("%s:%d glosses %q, which is not an id in the TERMS table in "+
			"assets/terms.js.\n\nterm() returns the plain word for an unknown id rather than "+
			"throwing, so this renders correctly and silently does nothing: the reader sees the "+
			"word with no dotted underline and no definition behind it. The table is keyed by "+
			"the JavaScript name, not the database's — `rung`, `blastRadius`, "+
			"`disruptionPolicy`. If the word genuinely has no entry, the fix is the entry, not "+
			"a nearby id that means something else. Known ids: %v",
			seen[id].file, seen[id].line, id, sortedKeys(known))
	}
}

type glossAsset struct {
	name string
	src  string
}

func glossSources(t *testing.T) []glossAsset {
	t.Helper()
	names := []string{"assets/app.js", "assets/device.js", "assets/fleet.js"}
	out := make([]glossAsset, 0, len(names))
	for _, n := range names {
		b, err := embedded.ReadFile(n)
		if err != nil {
			t.Fatalf("read the embedded %s: %v", n, err)
		}
		out = append(out, glossAsset{n, string(b)})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
