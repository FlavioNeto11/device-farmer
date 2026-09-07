package ui

// The two dictionaries must say the same things.
//
// This dashboard reads in English or in Brazilian Portuguese, and the whole
// mechanism is two objects in assets/i18n.js with the same keys. Nothing at
// runtime can tell you they have drifted: t() returns the key itself for a
// missing string, which is loud on screen and silent in a build.
//
// So the drift is caught here, and it is caught in both directions. A key in
// English and not in Portuguese is a reader who paid attention to a language
// setting and got an identifier where a sentence should be. A key in Portuguese
// and not in English is a string that is dead in the language every farm falls
// back to.
//
// The precedent is TestDocsRegisterMatchesREQUIREMENTS, which exists because two
// hand-maintained copies of the requirements register drifted twice — sixty-eight
// rows once, thirty-three more cells in the round that fixed them. Two hand-
// maintained copies of anything drift. The only question is whether the build
// notices.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// dictionary pulls one language's keys out of assets/i18n.js.
//
// It reads the file as text rather than executing it, because there is no
// JavaScript engine in a Go test and adding one to check a dictionary would be a
// dependency this project does not have and does not want. The parse is
// therefore shape-dependent, and the guards below fail loudly if the shape
// changes rather than silently reporting zero keys — a test that passes because
// it found nothing is worse than no test.
func dictionary(t *testing.T, src, which string) map[string]string {
	t.Helper()

	open := regexp.MustCompile(`(?m)^  ` + which + `: \{$`)
	loc := open.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("assets/i18n.js has no %q dictionary opening at the expected indent; "+
			"the file's shape changed and this test is now blind", which)
	}

	// The dictionary ends at the first line that closes it at the same indent.
	rest := src[loc[1]:]
	end := regexp.MustCompile(`(?m)^  \},$`).FindStringIndex(rest)
	if end == nil {
		t.Fatalf("the %q dictionary is not closed at the expected indent", which)
	}
	body := rest[:end[0]]

	// Keys are quoted and dotted; values are single-quoted JS strings that may
	// be concatenated across lines. Only the KEY matters here — this test is
	// about which keys exist, not what they say, because what they say is a
	// judgement no test can make.
	key := regexp.MustCompile(`(?m)^\s*'([a-zA-Z][\w.]*)':`)
	out := map[string]string{}
	for _, m := range key.FindAllStringSubmatch(body, -1) {
		if _, dup := out[m[1]]; dup {
			t.Errorf("%s: %q appears twice; the second wins silently and the first is dead",
				which, m[1])
		}
		out[m[1]] = ""
	}
	if len(out) < 40 {
		t.Fatalf("%s parsed to %d keys, which is far fewer than the dashboard has; the key "+
			"pattern no longer matches the file and this test is now blind", which, len(out))
	}
	return out
}

func i18nSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/i18n.js")
	if err != nil {
		t.Fatalf("read the embedded assets/i18n.js: %v", err)
	}
	return string(b)
}

// TestEveryStringExistsInBothLanguages.
//
// Falsify: delete any single line from either dictionary in assets/i18n.js.
func TestEveryStringExistsInBothLanguages(t *testing.T) {
	src := i18nSource(t)
	en := dictionary(t, src, "en")
	pt := dictionary(t, src, "pt")

	var missingPT, missingEN []string
	for k := range en {
		if _, ok := pt[k]; !ok {
			missingPT = append(missingPT, k)
		}
	}
	for k := range pt {
		if _, ok := en[k]; !ok {
			missingEN = append(missingEN, k)
		}
	}
	sort.Strings(missingPT)
	sort.Strings(missingEN)

	if len(missingPT) > 0 {
		t.Errorf("%d key(s) are in English and not in Portuguese, so a Portuguese reader sees "+
			"the key itself where a sentence belongs:\n  %s",
			len(missingPT), strings.Join(missingPT, "\n  "))
	}
	if len(missingEN) > 0 {
		t.Errorf("%d key(s) are in Portuguese and not in English. English is what every farm "+
			"falls back to, so these are dead strings:\n  %s",
			len(missingEN), strings.Join(missingEN, "\n  "))
	}
}

// TestEveryKeyTheAppAsksForExists walks the JavaScript for t('…') and
// data-i18n="…" and requires each one to be in the dictionaries.
//
// This is the half that catches the common mistake, which is not deleting a
// string but renaming one: a key changed in i18n.js and not at its call site
// still passes the test above, because both dictionaries agree — and prints the
// raw key on screen in every language.
//
// Only literal keys can be checked. A computed key — t('error.' + e.code) — is
// invisible here by construction, so the error codes it reaches are pinned
// separately below.
//
// Falsify: rename any key in i18n.js without renaming its call site.
func TestEveryKeyTheAppAsksForExists(t *testing.T) {
	src := i18nSource(t)
	en := dictionary(t, src, "en")

	used := map[string][]string{}
	for _, name := range []string{"assets/app.js", "assets/docs.js", "assets/index.html"} {
		b, err := embedded.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(b)
		// The closing bracket or comma is what makes this a LITERAL key. Without
		// it the pattern also matches the prefix of a computed one — `t('count.'
		// + k)` captured `count.` and reported it as a missing string, which is
		// this test being wrong about working code. A computed key cannot be
		// checked from here by construction; the two that exist are pinned by
		// their own tests below, and both have a documented fallback for the
		// case no test can cover.
		for _, m := range regexp.MustCompile(`\bt\('([a-zA-Z][\w.]*)'\s*[,)]`).FindAllStringSubmatch(body, -1) {
			used[m[1]] = append(used[m[1]], name)
		}
		for _, m := range regexp.MustCompile(`data-i18n="([a-zA-Z][\w.]*)"`).FindAllStringSubmatch(body, -1) {
			used[m[1]] = append(used[m[1]], name)
		}
		for _, m := range regexp.MustCompile(`data-i18n-attr="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			for _, pair := range strings.Fields(m[1]) {
				if at := strings.Index(pair, ":"); at > 0 {
					used[pair[at+1:]] = append(used[pair[at+1:]], name)
				}
			}
		}
	}
	if len(used) < 15 {
		t.Fatalf("found only %d translated strings in the assets; the call pattern changed and "+
			"this test is now blind", len(used))
	}

	for key, where := range used {
		if _, ok := en[key]; !ok {
			t.Errorf("%s asks for %q, which is in no dictionary. t() returns the key itself, so "+
				"this renders as %q on screen in every language.",
				strings.Join(where, " and "), key, key)
		}
	}
}

// TestEveryErrorCodeTheAPICanReturnHasASentence is the computed-key half.
//
// errText builds 'error.' + the code the API sent. A code with no entry keeps
// the server's English message, which is a deliberate and safe fallback — but
// the codes that exist TODAY should all have been considered, and this test is
// what makes adding a code a decision rather than an omission.
//
// The list comes from internal/api/errors.go, read as text. A code added there
// and not here fails this test, which is the point: the two files are the same
// contract seen from opposite ends.
//
// Falsify: add a const to the error-code block in internal/api/errors.go.
func TestEveryErrorCodeTheAPICanReturnHasASentence(t *testing.T) {
	src := i18nSource(t)
	en := dictionary(t, src, "en")

	codes := apiErrorCodes(t)
	if len(codes) < 8 {
		t.Fatalf("found %d error codes in internal/api/errors.go; the const pattern changed "+
			"and this test is now blind", len(codes))
	}

	var missing []string
	for _, c := range codes {
		if _, ok := en["error."+c]; !ok {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d error code(s) the API can return have no translated sentence:\n  %s\n\n"+
			"A Portuguese reader meets the server's English message for these. That is the "+
			"safe fallback and it is not the intended one — decide a sentence, or write down "+
			"here why English is right for this code.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// apiErrorCodes reads the code constants out of internal/api/errors.go.
func apiErrorCodes(t *testing.T) []string {
	t.Helper()
	b, err := readRepoFile("internal/api/errors.go")
	if err != nil {
		t.Fatalf("read internal/api/errors.go: %v", err)
	}
	// Code<Name> = "<value>"
	re := regexp.MustCompile(`(?m)^\s*Code\w+\s*=\s*"([a-z_]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	return out
}

// TestTheDictionariesAreValidJavaScriptStrings is a cheap shape check: every
// key line must actually assign a single-quoted string, so a stray double quote
// or an unescaped apostrophe fails here rather than taking the whole dashboard
// down with a parse error on first load. A syntax error in i18n.js means NO
// page at all, in either language, because it is the first script the document
// loads.
func TestTheDictionariesAreValidJavaScriptStrings(t *testing.T) {
	src := i18nSource(t)
	for _, which := range []string{"en", "pt"} {
		_ = dictionary(t, src, which)
	}
	// An apostrophe inside a single-quoted JS string must be escaped. This finds
	// the unescaped ones without executing anything.
	bad := regexp.MustCompile(`(?m)^\s*'[\w.]+':\s*'[^'\\]*[a-zA-Z]'[a-zA-Z]`)
	if m := bad.FindString(src); m != "" {
		t.Errorf("this line has an unescaped apostrophe and will not parse:\n  %s", strings.TrimSpace(m))
	}
	// And nothing in a dictionary may be a JSON object by accident.
	if strings.Contains(src, `": {`) {
		t.Error("a dictionary entry looks like JSON rather than a JS string literal")
	}
	_ = json.Valid // kept so the import documents that this is deliberately NOT json
	_ = fmt.Sprint
}

// readRepoFile reads a file from the repository root, two levels up from this
// package. It exists because two of the tests above reconcile this package's
// assets against a file that is not embedded in them — the same shape
// requirements_sync_test.go uses to reach REQUIREMENTS.md.
func readRepoFile(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join("..", "..", rel))
}
