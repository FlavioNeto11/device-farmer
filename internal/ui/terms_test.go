package ui

// The glossary claims four things, and this file is what makes each of them
// checkable by the build rather than by somebody remembering.
//
// A glossed term is a word on screen — fence, witness, rung — with three pieces
// of writing attached: one sentence in English, the same sentence in
// Portuguese, and the identifier the word maps to in the database. The first
// two are prose and are translated. The third is a column name and must NOT be,
// because the entire argument for keeping these words in English is that an
// operator matches them by eye against psql, ctl and a log line.
//
// All three are computed keys — 'term.' + id + '.short' — so every one of them
// is invisible to TestEveryKeyTheAppAsksForExists, whose regex only matches
// literals. A term added with no strings therefore passes every other test in
// this package and renders `term.foo.short` on screen, in both languages.
//
// The fourth claim is the "read the full definition" control. It names a
// section of assets/docs/<area>.json by its heading, and a heading is prose
// somebody will eventually improve. Without a test, that edit turns every deep
// link into a dead end silently — the reader gets an empty page and no error
// anywhere.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func termsSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/terms.js")
	if err != nil {
		t.Fatalf("read the embedded assets/terms.js: %v", err)
	}
	return string(b)
}

// glossTerm is one parsed entry of the TERMS table in assets/terms.js.
type glossTerm struct {
	id      string
	area    string
	heading string
}

// glossary parses the entries out of assets/terms.js.
//
// By regex over the source text, for the reason exec_test.go and i18n_test.go
// both state: there is no JavaScript engine here and adding one for this would
// be a dependency the project does not want. The floor below is what keeps a
// changed shape from turning this file into a suite that passes because it
// found nothing.
func glossary(t *testing.T) []glossTerm {
	t.Helper()
	src := termsSource(t)

	// One entry per line, in this exact order, which is how the table is
	// written. A heading may carry an escaped apostrophe — one section of
	// lease.json is called "Protected leases and 'hold and page'".
	re := regexp.MustCompile(`\{\s*id:\s*'([A-Za-z][\w]*)',\s*area:\s*'([a-z]+)',\s*heading:\s*'((?:[^'\\]|\\.)*)'\s*\}`)
	ms := re.FindAllStringSubmatch(src, -1)
	if len(ms) < 12 {
		t.Fatalf("parsed %d glossary entries out of assets/terms.js; there must be a glossary "+
			"and it must be shaped {id, area, heading}. The entry shape changed and this test "+
			"is now blind.", len(ms))
	}

	out := make([]glossTerm, 0, len(ms))
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m[1]] {
			t.Errorf("the term id %q appears twice in assets/terms.js; the second wins silently "+
				"and the first is dead", m[1])
		}
		seen[m[1]] = true
		out = append(out, glossTerm{id: m[1], area: m[2], heading: unescapeJS(m[3])})
	}
	return out
}

// unescapeJS undoes the backslash escapes a single-quoted JS string literal can
// carry. Only the two that occur here: an escaped quote and an escaped
// backslash.
func unescapeJS(s string) string {
	s = strings.ReplaceAll(s, `\\`, "\x00")
	s = strings.ReplaceAll(s, `\'`, "'")
	return strings.ReplaceAll(s, "\x00", `\`)
}

// dictionaryValues is dictionary() with the values kept.
//
// i18n_test.go's reader deliberately discards them — it is about which keys
// exist, and what they say is a judgement no test can make. This file needs one
// value compared against another, which is a judgement a test can make exactly
// because it is a comparison and not an opinion.
func dictionaryValues(t *testing.T, src, which string) map[string]string {
	t.Helper()

	open := regexp.MustCompile(`(?m)^  ` + which + `: \{$`)
	loc := open.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no %q dictionary opening in assets/i18n.js; the file's shape changed and this "+
			"test is now blind", which)
	}
	rest := src[loc[1]:]
	end := regexp.MustCompile(`(?m)^  \},?$`).FindStringIndex(rest)
	if end == nil {
		t.Fatalf("the %q dictionary is not closed", which)
	}
	body := rest[:end[0]]

	pair := regexp.MustCompile(`(?m)^\s*'([a-zA-Z][\w.]*)':\s*'((?:[^'\\]|\\.)*)'`)
	out := map[string]string{}
	for _, m := range pair.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	if len(out) < 40 {
		t.Fatalf("parsed %d %q strings with values out of assets/i18n.js; the dictionary shape "+
			"changed and this test is now blind", len(out), which)
	}
	return out
}

// TestEveryGlossedTermHasBothSentencesInBothLanguages.
//
// The sentence and the identifier are computed keys — 'term.' + id + '.short' —
// so they are invisible to TestEveryKeyTheAppAsksForExists, whose regex only
// matches literals. A term added without strings renders its own key on screen
// in both languages and nothing else says so.
//
// Falsify: add a term to assets/terms.js with no `pt` string in i18n.js.
func TestEveryGlossedTermHasBothSentencesInBothLanguages(t *testing.T) {
	src := i18nSource(t)
	en := dictionary(t, src, "en")
	pt := dictionary(t, src, "pt")

	var missing []string
	for _, g := range glossary(t) {
		for _, suffix := range []string{".short", ".ident"} {
			key := "term." + g.id + suffix
			if _, ok := en[key]; !ok {
				missing = append(missing, key+" (en)")
			}
			if _, ok := pt[key]; !ok {
				missing = append(missing, key+" (pt)")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d glossary string(s) do not exist:\n  %s\n\nt() returns the key itself for a "+
			"miss, so each of these renders as its own key inside the dialog that was supposed "+
			"to explain the word.", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestEveryTermPointsAtADocsSectionThatExists.
//
// The dialog's "read the full definition" opens one section of one area, named
// by its English heading. The headings are prose in a JSON file and somebody
// will improve one; without this test that edit turns every deep link on that
// section into an empty page, with no error in the console, no failure in the
// build and nothing on screen to say what happened.
//
// Falsify: rename the fence section in assets/docs/lease.json.
func TestEveryTermPointsAtADocsSectionThatExists(t *testing.T) {
	type docFile struct {
		Sections []struct {
			Heading string `json:"heading"`
		} `json:"sections"`
	}

	headings := map[string]map[string]bool{}
	for _, g := range glossary(t) {
		if _, done := headings[g.area]; !done {
			name := "assets/docs/" + g.area + ".json"
			b, err := embedded.ReadFile(name)
			if err != nil {
				t.Errorf("a term points at the area %q and %s cannot be read: %v", g.area, name, err)
				headings[g.area] = map[string]bool{}
				continue
			}
			var doc docFile
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s does not parse as JSON: %v", name, err)
			}
			if len(doc.Sections) == 0 {
				t.Fatalf("%s has no sections; the document shape changed and this test is now blind", name)
			}
			set := map[string]bool{}
			for _, s := range doc.Sections {
				set[s.Heading] = true
			}
			headings[g.area] = set
		}

		if !headings[g.area][g.heading] {
			t.Errorf("the term %q points at %s / %q, and no section of assets/docs/%s.json has "+
				"that heading.\n\nThe dialog's \"read the full definition\" would open that area "+
				"and find nothing, which is the one failure a reader cannot tell from the "+
				"feature not existing.", g.id, g.area, g.heading, g.area)
		}
	}
}

// TestEveryTranslatedDocKeepsItsSectionsInTheSameOrder.
//
// The deep link names a section by its ENGLISH heading, and the translated
// documents call the same section something else. docs.js therefore crosses the
// language boundary by INDEX: it resolves the English heading to a position and
// reads the heading back out of whichever document is on screen.
//
// That is only sound while the two documents carry the same sections in the same
// order, and nothing else in this tree says they must. Insert one section into
// lease.pt.json and every deep link below it takes a Portuguese reader to a
// DIFFERENT definition — stated with exactly the confidence of the right one,
// which is worse than the empty page the index mapping exists to avoid.
//
// The fingerprint is `source`: the file each section cites is a path, so it is
// the one field on a section that is never translated.
//
// Falsify: swap two sections in assets/docs/lease.pt.json.
func TestEveryTranslatedDocKeepsItsSectionsInTheSameOrder(t *testing.T) {
	type docFile struct {
		Sections []struct {
			Heading string `json:"heading"`
			Source  string `json:"source"`
		} `json:"sections"`
	}
	read := func(name string) *docFile {
		b, err := embedded.ReadFile("assets/docs/" + name)
		if err != nil {
			return nil
		}
		var d docFile
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("assets/docs/%s does not parse as JSON: %v", name, err)
		}
		return &d
	}

	areas := map[string]bool{}
	for _, g := range glossary(t) {
		areas[g.area] = true
	}
	if len(areas) < 3 {
		t.Fatalf("the glossary points at only %d areas; the entries changed and this test is "+
			"now blind", len(areas))
	}

	var compared int
	for area := range areas {
		en := read(area + ".json")
		if en == nil {
			continue // TestEveryTermPointsAtADocsSectionThatExists reports the missing file
		}
		pt := read(area + ".pt.json")
		if pt == nil {
			continue // an untranslated area is an ordinary state; docs.js falls back to English
		}
		if len(en.Sections) != len(pt.Sections) {
			t.Errorf("assets/docs/%s.json has %d sections and %s.pt.json has %d.\n\nThe glossary's "+
				"deep link crosses the language boundary by index, so a document with sections "+
				"added or removed sends a Portuguese reader to the wrong definition.",
				area, len(en.Sections), area, len(pt.Sections))
			continue
		}
		for i := range en.Sections {
			compared++
			if en.Sections[i].Source != pt.Sections[i].Source {
				t.Errorf("assets/docs/%s: section %d cites %q in English and %q in Portuguese.\n\n"+
					"The two documents have been reordered relative to each other. A deep link "+
					"resolves a section by position, so it now lands a Portuguese reader on a "+
					"section that is not the one the word was glossed against.",
					area, i, en.Sections[i].Source, pt.Sections[i].Source)
			}
		}
	}
	if compared < 40 {
		t.Fatalf("compared only %d section pairs; the documents or their `source` fields changed "+
			"and this test is now blind", compared)
	}
}

// TestNoTermIdentifierIsTranslated is the machine-readable form of the argument
// the whole glossary rests on.
//
// Every glossed word is also a column name, an API field or a value an operator
// meets in psql, in ctl output and in a log line. That is why the word on screen
// is not replaced with a friendlier one, and it is why the identifier beside it
// in the dialog must be the same bytes in both languages: a translated column
// name is one nobody can find in the database that does not have it.
//
// Falsify: translate disruption_policy in the pt dictionary.
func TestNoTermIdentifierIsTranslated(t *testing.T) {
	src := i18nSource(t)
	en := dictionaryValues(t, src, "en")
	pt := dictionaryValues(t, src, "pt")

	var checked int
	for _, g := range glossary(t) {
		key := "term." + g.id + ".ident"
		a, okA := en[key]
		b, okB := pt[key]
		if !okA || !okB {
			continue // TestEveryGlossedTermHasBothSentencesInBothLanguages owns this
		}
		checked++
		if a != b {
			t.Errorf("%s is %q in English and %q in Portuguese.\n\nAn identifier is never "+
				"translated. This one is what the schema, the API and the logs say, and the "+
				"dialog prints it so an operator can match it by eye against a psql session — "+
				"which is impossible if the page says something the database does not.",
				key, a, b)
		}
	}
	if checked < 12 {
		t.Fatalf("compared only %d identifiers; the keys or the dictionary shape changed and "+
			"this test is now blind", checked)
	}
}

// TestTheDocsDeepLinkIsAFunctionNotAnAnchor makes docs.js's own comment
// executable.
//
// That comment records why the table of contents is built from buttons: the app
// routes on location.hash, so an <a href="#doc-s-4"> would rewrite the route and
// land the reader on the Fleet grid instead of the section they asked for. The
// glossary's deep link is the same mechanism reached from a different file, and
// an anchor here would fail in the same way — visibly, but only to whoever
// clicked it.
//
// Falsify: replace the openDocs call in assets/terms.js with an href="#…".
func TestTheDocsDeepLinkIsAFunctionNotAnAnchor(t *testing.T) {
	src := termsSource(t)

	if !strings.Contains(src, "window.openDocs(") {
		t.Error("assets/terms.js never calls window.openDocs(. The dialog's \"read the full " +
			"definition\" is the only thing on this dashboard that links a word to the page " +
			"defining it; without the call it is a button that does nothing.")
	}
	if strings.Contains(src, `href="#`) {
		t.Error("assets/terms.js contains an href=\"#…\". The app routes on location.hash, so " +
			"a fragment link rewrites the route and lands the reader on the Fleet grid. " +
			"docs.js carries the comment that says so; this is that comment as a build failure.")
	}

	docs := docsSource(t)
	if !strings.Contains(docs, "window.openDocs = openDocs") {
		t.Error("assets/docs.js does not export window.openDocs. terms.js calls it, and a call " +
			"to an undefined global is a glossary whose \"read the full definition\" silently " +
			"does nothing.")
	}
}
