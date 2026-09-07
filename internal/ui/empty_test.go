package ui

// An empty panel is the one screen that has to explain itself.
//
// Every other screen has rows on it: a reader who does not understand a column
// can at least see that the thing works. An empty panel has nothing but its own
// sentence, and for most of this dashboard's life that sentence named a SQL
// object — "farm.recovery_tiers rows 0 through 8 appear here", "farm.events and
// farm.audit_log are merged here". Every one of those is true, and each is
// exactly what an operator wants in year two when they start writing their own
// queries.
//
// It is not what a person wants on the morning they first open the page. To
// them a view name is not an explanation, it is evidence that this software was
// written for somebody else; and the conclusion they reach in front of a blank
// grid — "it is broken" — is the one thing the panel had a chance to prevent.
//
// So the rule this file encodes is not "do not name the database". It is:
//
//	naming a database view at a newcomer is fine as background, and not fine
//	as the only thing on screen.
//
// The background half lives behind a "Where this comes from" <details>. The
// other half is the third argument to emptyState(): the button that changes the
// situation. This test says a panel may not cite the schema without offering
// one, and it says so about the RULE rather than about the four instances that
// prompted it — a panel added next year gets the same treatment with nobody
// having to remember this conversation.

import (
	"regexp"
	"strings"
	"testing"
)

// The scripts that draw empty panels. A file missing from this list is not
// checked at all, which is the same silent hole i18n_test.go documents for its
// own hand-kept list; the floor in the test below is what makes a wrong list
// visible instead of quietly halving the coverage.
var emptyStateAssets = []string{
	"assets/app.js",
	"assets/fleet.js",
	"assets/device.js",
}

// englishStrings parses the en dictionary out of assets/i18n.js WITH ITS
// VALUES.
//
// i18n_test.go parses the same object for its keys and deliberately drops the
// values, because which keys exist is checkable and what they say is a
// judgement. This test needs the text: after the redesign the sentence naming
// farm.v_fleet is no longer written at the call site, it is a key looked up
// there, so a check that read only the call site would find no SQL object
// anywhere and pass over nothing at all.
//
// By regex over the source, for the reason every test in this package states:
// there is no JavaScript engine here and adding one to read a dictionary would
// be a dependency this project does not want. Entries written as one line are
// matched; an entry concatenated across lines is not, and is simply invisible
// here rather than being half-read.
func englishStrings(t *testing.T) map[string]string {
	t.Helper()

	b, err := embedded.ReadFile("assets/i18n.js")
	if err != nil {
		t.Fatalf("read the embedded assets/i18n.js: %v", err)
	}
	src := string(b)

	open := regexp.MustCompile(`(?m)^  en: \{$`).FindStringIndex(src)
	if open == nil {
		t.Fatal("no en dictionary at an indent of two spaces in assets/i18n.js; the file's shape " +
			"changed and this test is now blind")
	}
	rest := src[open[1]:]
	end := regexp.MustCompile(`(?m)^  \},?$`).FindStringIndex(rest)
	if end == nil {
		t.Fatal("the en dictionary in assets/i18n.js is not closed at an indent of two spaces")
	}
	body := rest[:end[0]]

	out := map[string]string{}
	line := regexp.MustCompile(`(?m)^\s*'([\w.]+)':\s*'((?:[^'\\]|\\.)*)'\s*,?\s*$`)
	for _, m := range line.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	if len(out) < 150 {
		t.Fatalf("parsed only %d en strings with their values; the dictionary's shape changed and "+
			"this test can no longer resolve the sentences it checks", len(out))
	}
	return out
}

// esCall is one emptyState() call site.
type esCall struct {
	file string
	line int
	args []string
}

// callArgs splits the argument list of the call whose opening parenthesis is at
// src[open], respecting nesting, string literals and comments. It returns the
// arguments and whether the call was closed.
//
// Depth counting rather than a regex, because the arguments are arrow functions
// and array literals — `emptyAction(t('x'), () => { ... })` — and a regex that
// tried to find the top-level commas in that would be wrong in a way that
// silently changed the argument count, which is the only thing this test reads.
//
// COMMENTS ARE SKIPPED, and that is not tidiness. These call sites are heavily
// commented, this project writes prose with apostrophes in it, and an
// apostrophe inside a comment between two arguments would otherwise open a
// string literal that never closes — turning somebody's explanatory sentence
// into a red build with a message about a parse. A regex literal containing an
// unbalanced quote would still defeat this; there are none here, and the
// closing guard below reports the failure as a parse rather than as a verdict.
func callArgs(src string, open int) ([]string, bool) {
	depth := 0
	var args []string
	start := open + 1
	var quote byte
	for i := open; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
					i += nl
					continue
				}
				return nil, false
			}
			if src[i+1] == '*' {
				end := strings.Index(src[i+2:], "*/")
				if end < 0 {
					return nil, false
				}
				i += 2 + end + 1
				continue
			}
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				if c != ')' {
					return nil, false
				}
				return append(args, strings.TrimSpace(src[start:i])), true
			}
		case ',':
			if depth == 1 {
				args = append(args, strings.TrimSpace(src[start:i]))
				start = i + 1
			}
		}
	}
	return nil, false
}

// emptyStateCalls finds every emptyState(...) call in the dashboard's scripts.
// The declaration itself is skipped: `function emptyState(title, detail,
// action)` is not a panel anybody sees.
func emptyStateCalls(t *testing.T) []esCall {
	t.Helper()

	var out []esCall
	for _, name := range emptyStateAssets {
		b, err := embedded.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		for _, loc := range regexp.MustCompile(`\bemptyState\(`).FindAllStringIndex(src, -1) {
			if strings.HasSuffix(src[:loc[0]], "function ") {
				continue
			}
			args, ok := callArgs(src, loc[1]-1)
			if !ok {
				t.Fatalf("%s: an emptyState( call at byte %d is not closed; the parse is wrong "+
					"and this test is now blind", name, loc[0])
			}
			out = append(out, esCall{
				file: name,
				line: strings.Count(src[:loc[0]], "\n") + 1,
				args: args,
			})
		}
	}
	return out
}

// TestNoEmptyStateCitesASQLObjectWithoutAnAction.
//
// The check resolves each call: the literal text of its arguments, plus the
// English value of every t('key') inside them, because the sentences moved into
// the dictionary and the call site now carries only their names. A resolved
// panel that says `farm.` anywhere must pass a third argument — the action.
//
// It does NOT check that the sentence is gone. It should not be gone. It is the
// right answer to a question this panel is often asked, and deleting it would
// have traded one reader for another; it is collapsed behind provenance()
// instead. What this test forbids is the schema being the whole of what an
// empty screen offers.
//
// Falsify: revert the Fleet empty state — put farm.v_fleet back as the second
// argument with no third — and this fails naming fleet.js.
func TestNoEmptyStateCitesASQLObjectWithoutAnAction(t *testing.T) {
	en := englishStrings(t)
	calls := emptyStateCalls(t)

	// The floor. Without it, a rename of emptyState or a change to how the
	// arguments are written turns this file into a suite that passes because it
	// examined nothing — which is the failure mode every guard test in this
	// package is written against.
	if len(calls) < 15 {
		t.Fatalf("found only %d emptyState( calls across %s; the dashboard has far more, so the "+
			"call shape changed and this test is now blind",
			len(calls), strings.Join(emptyStateAssets, ", "))
	}

	key := regexp.MustCompile(`\bt\('([\w.]+)'`)

	// "farm." followed by the start of an identifier, not by a space or the end
	// of a sentence. A plain Contains fired on "Nothing is wrong with the farm."
	// -- an English sentence that happens to end in the word this schema is
	// named after -- and reported working prose as a citation. A guard test that
	// is wrong about correct code gets weakened by the next person to meet it,
	// which costs more than the case it was catching.
	sqlObject := regexp.MustCompile(`farm\.[a-z_]`)
	var cited int
	for _, c := range calls {
		joined := strings.Join(c.args, "\n")
		resolved := joined
		for _, m := range key.FindAllStringSubmatch(joined, -1) {
			if v, ok := en[m[1]]; ok {
				resolved += "\n" + v
			}
		}
		if !sqlObject.MatchString(resolved) {
			continue
		}
		cited++
		if len(c.args) < 3 || c.args[2] == "" {
			t.Errorf("%s:%d names a SQL object and offers nothing to press. It has %d argument(s); "+
				"an empty panel that cites the schema must pass a third — the action that would put "+
				"something on this screen. The sentence stays, inside provenance(); it just may not "+
				"be the only thing there.", c.file, c.line, len(c.args))
		}
	}

	// The second floor, and the one that catches the subtler regression: if the
	// provenance sentences were deleted rather than demoted, every call above
	// would be skipped and this test would report success over zero findings.
	if cited < 4 {
		t.Errorf("only %d empty panel(s) cite a SQL object at all. Either the provenance sentences "+
			"were deleted instead of collapsed into \"Where this comes from\" — they are true and "+
			"they are what an operator wants in year two — or the keys they live under can no "+
			"longer be resolved from the call site, and this test is checking nothing.", cited)
	}
}
