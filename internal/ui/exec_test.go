package ui

// The command catalogue claims things, and this file is what makes the claims
// checkable.
//
// The catalogue in assets/exec.js has two tiers. A VERIFIED entry carries a
// `source` — a file in this repository that sends that exact command to a real
// handset — and the dashboard renders that path next to a tick. An UNVERIFIED
// entry carries none and says on screen that nobody here has run it.
//
// That distinction is the only thing separating "this project sends this command
// to every device once a minute" from "this command generally exists on
// Android", and on screen the two look almost the same: a row in a list. So it
// has to be defended by the build rather than by good intentions, because the
// failure is silent and the consequence is an operator trusting a claim this
// project never made.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func execSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/exec.js")
	if err != nil {
		t.Fatalf("read the embedded assets/exec.js: %v", err)
	}
	return string(b)
}

// catEntry is one parsed catalogue row.
type catEntry struct {
	id       string
	template string
	source   string
}

// catalogue parses the entries out of assets/exec.js.
//
// By regex over the source text, because there is no JavaScript engine here and
// adding one for this would be a dependency the project does not want — the same
// reasoning i18n_test.go states for the dictionaries. The floor check below is
// what keeps a changed shape from turning this file into a suite that passes
// because it found nothing.
func catalogue(t *testing.T) []catEntry {
	t.Helper()
	src := execSource(t)

	// Each entry names id, template and source somewhere in its object literal;
	// the fields are matched independently and then zipped by order, because the
	// entries are written both multi-line and single-line.
	ids := regexp.MustCompile(`\bid:\s*'([\w]+)'`).FindAllStringSubmatch(src, -1)
	tpls := regexp.MustCompile(`\btemplate:\s*'([^']*)'`).FindAllStringSubmatch(src, -1)
	srcs := regexp.MustCompile(`\bsource:\s*'([^']*)'`).FindAllStringSubmatch(src, -1)

	if len(ids) < 15 || len(ids) != len(tpls) || len(ids) != len(srcs) {
		t.Fatalf("parsed %d ids, %d templates and %d sources out of assets/exec.js; they must "+
			"agree and there must be a catalogue. The entry shape changed and this test is "+
			"now blind.", len(ids), len(tpls), len(srcs))
	}

	out := make([]catEntry, 0, len(ids))
	for i := range ids {
		out = append(out, catEntry{id: ids[i][1], template: tpls[i][1], source: srcs[i][1]})
	}
	return out
}

// TestEveryVerifiedCommandIsOneThisRepoRuns.
//
// A `source` is a citation, and this reads the file it cites and looks for the
// command in it. Without this the tick beside a command means only that somebody
// typed a plausible path.
//
// The template's {param} regions are relaxed to a wildcard, because the source
// builds them at runtime — internal/enroll asks for eleven properties by
// substituting into one format string, so `getprop {prop}` will never appear in
// identity.go literally.
//
// Falsify: move `df -h` into the verified tier with any source path.
func TestEveryVerifiedCommandIsOneThisRepoRuns(t *testing.T) {
	var verified int
	for _, c := range catalogue(t) {
		if c.source == "" {
			continue
		}
		verified++

		b, err := os.ReadFile(filepath.Join("..", "..", c.source))
		if err != nil {
			t.Errorf("%s cites %s, which cannot be read: %v", c.id, c.source, err)
			continue
		}

		// The literal parts of the template, in order. Every one must appear.
		parts := regexp.MustCompile(`\{[a-z]+\}`).Split(c.template, -1)
		body := string(b)
		missing := false
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.Contains(body, part) {
				missing = true
				break
			}
		}
		if missing {
			t.Errorf("%s is in the VERIFIED tier and cites %s, but %q does not appear there.\n\n"+
				"The dashboard renders that path beside a tick and tells the operator this "+
				"project sends this command to real handsets. Either the citation is wrong, or "+
				"the entry belongs in the unverified tier where it says nobody here has run it.",
				c.id, c.source, c.template)
		}
	}
	if verified < 5 {
		t.Fatalf("only %d verified entries; the tier split has collapsed", verified)
	}
}

// TestUnverifiedCommandsClaimNoSource is the other direction, and it is the one
// that catches the tempting mistake: giving an ordinary Android command a
// plausible-looking citation so it moves up into the tier that looks better.
//
// Falsify: give `screencap -p` a source of internal/enroll/identity.go.
func TestUnverifiedCommandsClaimNoSource(t *testing.T) {
	src := execSource(t)
	var unverified int
	for _, c := range catalogue(t) {
		if c.source != "" {
			continue
		}
		unverified++
	}
	if unverified < 5 {
		t.Fatalf("only %d unverified entries; the tier split has collapsed", unverified)
	}

	// And the page must actually say what the empty source means. Without this
	// sentence the second tier is just a shorter list with no tick.
	for _, key := range []string{"exec.tier.unverifiedNote", "exec.unverifiedWhy"} {
		if !strings.Contains(src, "'"+key+"'") {
			t.Errorf("assets/exec.js never renders %s, so the unverified tier does not tell "+
				"the reader that nobody here has run these commands", key)
		}
	}
}

// TestTheGetpropChoicesAreTheEnrollersOwnProperties.
//
// PROBE_PROPS in assets/exec.js is a FOURTH copy of a list that already exists
// three times — the Go constants, the probe command built from them, and the
// documentation. A fourth copy in a JavaScript file is one no Go developer will
// ever open, so it drifts the moment somebody adds a property to the enroller
// and the dashboard quietly stops offering it.
//
// Falsify: add a property to probeProps in internal/enroll/identity.go.
func TestTheGetpropChoicesAreTheEnrollersOwnProperties(t *testing.T) {
	src := execSource(t)

	open := strings.Index(src, "const PROBE_PROPS = [")
	if open < 0 {
		t.Fatal("assets/exec.js has no PROBE_PROPS; this test is now blind")
	}
	end := strings.Index(src[open:], "];")
	if end < 0 {
		t.Fatal("PROBE_PROPS is not closed")
	}
	js := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([\w.]+)'`).FindAllStringSubmatch(src[open:open+end], -1) {
		js[m[1]] = true
	}

	b, err := readRepoFile("internal/enroll/identity.go")
	if err != nil {
		t.Fatalf("read internal/enroll/identity.go: %v", err)
	}
	// The properties are const strings; take every ro.* literal in the file,
	// which is the set the probe can possibly ask for.
	goProps := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(ro\.[\w.]+)"`).FindAllStringSubmatch(string(b), -1) {
		goProps[m[1]] = true
	}
	if len(goProps) < 8 {
		t.Fatalf("found %d ro.* properties in identity.go; the shape changed and this test is "+
			"now blind", len(goProps))
	}

	var missing, extra []string
	for p := range goProps {
		if !js[p] {
			missing = append(missing, p)
		}
	}
	for p := range js {
		if !goProps[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("internal/enroll asks every handset for %d propert(ies) the dashboard does not "+
			"offer:\n  %s\n\nPROBE_PROPS in assets/exec.js is a fourth copy of that list and "+
			"has drifted.", len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("the dashboard offers %d propert(ies) the enroller never reads:\n  %s\n\n"+
			"They are presented as the properties this project asks for, which is then untrue.",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// TestEveryCatalogueEntryHasBothSentencesInBothLanguages.
//
// The label and the explanation are computed keys — 'exec.cat.' + id + '.label'
// — so they are invisible to TestEveryKeyTheAppAsksForExists, whose regex only
// matches literals. An entry added without strings therefore passes every other
// test in this package and renders `exec.cat.foo.label` on screen, in both
// languages.
//
// Falsify: add an entry to the catalogue and no keys to i18n.js.
func TestEveryCatalogueEntryHasBothSentencesInBothLanguages(t *testing.T) {
	i18n := i18nSource(t)
	en := dictionary(t, i18n, "en")
	pt := dictionary(t, i18n, "pt")

	var missing []string
	for _, c := range catalogue(t) {
		for _, suffix := range []string{".label", ".why"} {
			key := "exec.cat." + c.id + suffix
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
		t.Errorf("%d catalogue string(s) do not exist:\n  %s\n\nt() returns the key itself for a "+
			"miss, so each of these renders as its own key on screen.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestNoCatalogueCommandIsTranslated encodes, as a build failure, the rule the
// documentation translation followed by hand for examples[].code: a translated
// command does not run.
//
// Falsify: add 'exec.cat.battery.command': 'dumpsys bateria' to i18n.js.
func TestNoCatalogueCommandIsTranslated(t *testing.T) {
	i18n := i18nSource(t)
	for _, c := range catalogue(t) {
		if len(c.template) < 6 {
			continue // too short to match by accident meaningfully
		}
		if strings.Contains(i18n, "'"+c.template+"'") {
			t.Errorf("the command %q appears as a value in assets/i18n.js. Commands are never "+
				"translated and never live in a dictionary — a translated command does not "+
				"run, and a command in a dictionary is one somebody will translate.", c.template)
		}
	}
}

// TestTheFenceFeatureNameTheDashboardMatchesIsTheOneTheAPISends.
//
// The pre-flight finds the fence row in GET /api/v1/capabilities by its English
// prose name. internal/api/capabilities_test.go already carries a comment saying
// "the dashboard finds the row by name"; until this feature existed that
// described an intention rather than a fact.
//
// Falsify: rename the feature in internal/api/capabilities.go.
func TestTheFenceFeatureNameTheDashboardMatchesIsTheOneTheAPISends(t *testing.T) {
	src := execSource(t)
	m := regexp.MustCompile(`const FENCE_FEATURE = '([^']+)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("assets/exec.js has no FENCE_FEATURE; this test is now blind")
	}

	b, err := readRepoFile("internal/api/capabilities.go")
	if err != nil {
		t.Fatalf("read internal/api/capabilities.go: %v", err)
	}
	if !strings.Contains(string(b), `"`+m[1]+`"`) {
		t.Errorf("the dashboard looks for a capability named %q and internal/api/capabilities.go "+
			"publishes no such name.\n\nThe pre-flight then reports the fence as unknown on "+
			"every farm, including the fenced ones where composing a command is guaranteed to "+
			"end in a 501.", m[1])
	}
}

// TestTheExecPanelImposesTheTimeoutFloorTheAPIDoesNot.
//
// internal/api clamps timeout_ms from ABOVE and has no floor: timeout_ms:1 is
// accepted as one millisecond, and comes back as a 502 that reads like a broken
// handset. The UI is the only place that floor can exist.
//
// The ceiling is checked in the other direction: the server clamps silently, so
// a UI offering more than the server accepts would promise a wait it will not
// honour.
//
// Falsify: change maxExecTimeout in internal/api/server.go to 10 minutes.
func TestTheExecPanelImposesTheTimeoutFloorTheAPIDoesNot(t *testing.T) {
	src := execSource(t)

	min := regexp.MustCompile(`const TIMEOUT_MIN = (\d+)`).FindStringSubmatch(src)
	max := regexp.MustCompile(`const TIMEOUT_MAX = (\d+)`).FindStringSubmatch(src)
	if min == nil || max == nil {
		t.Fatal("assets/exec.js has no TIMEOUT_MIN/TIMEOUT_MAX; this test is now blind")
	}
	if min[1] == "0" {
		t.Error("TIMEOUT_MIN is 0, which is the floor the API already fails to have")
	}

	b, err := readRepoFile("internal/api/server.go")
	if err != nil {
		t.Fatalf("read internal/api/server.go: %v", err)
	}
	// maxExecTimeout = 5 * time.Minute → 300000 ms.
	if !strings.Contains(string(b), "maxExecTimeout") {
		t.Fatal("internal/api/server.go no longer declares maxExecTimeout; this test is blind")
	}
	if max[1] != "300000" {
		t.Errorf("the panel offers a ceiling of %s ms. internal/api's maxExecTimeout is "+
			"5 * time.Minute and it clamps SILENTLY, so any other number here is a wait the "+
			"UI promises and the server does not honour.", max[1])
	}
}

// TestEveryScriptTheDashboardLoadsIsEmbeddedAndRequired.
//
// Adding an asset needs three edits in two files, and forgetting the third is
// silent: the file is embedded, served, and absent from the required-asset list,
// so a build that omits it starts happily and 404s at runtime. This generalises
// past the command builder — it is the check that makes that registration table
// enforce itself.
//
// Falsify: add a <script src> to index.html and touch nothing else.
func TestEveryScriptTheDashboardLoadsIsEmbeddedAndRequired(t *testing.T) {
	page, err := embedded.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read the embedded index.html: %v", err)
	}
	uiGo, err := readRepoFile("internal/ui/ui.go")
	if err != nil {
		t.Fatalf("read internal/ui/ui.go: %v", err)
	}

	scripts := regexp.MustCompile(`<script src="([^"]+)"`).FindAllStringSubmatch(string(page), -1)
	if len(scripts) < 3 {
		t.Fatalf("found %d scripts in index.html; the tag shape changed and this test is blind",
			len(scripts))
	}
	for _, m := range scripts {
		name := m[1]
		if _, err := embedded.ReadFile("assets/" + name); err != nil {
			t.Errorf("index.html loads %s and it is not in the //go:embed list: %v", name, err)
		}
		if !strings.Contains(string(uiGo), `"`+name+`"`) {
			t.Errorf("index.html loads %s and internal/ui/ui.go does not require it.\n\n"+
				"A missing required asset is caught at startup; one that is merely embedded "+
				"and unlisted 404s at runtime, and the page dies on a ReferenceError with "+
				"nothing in the server log.", name)
		}
	}
}

// TestTheDefaultIsOneTheDemoAnswers.
//
// The catalogue is ordered by intent, not by what a test fixture scripts —
// ordering an operator-facing list by a property of the demo would couple the two
// invisibly. But the DEFAULT selection is one decision, and it is worth making it
// one the demo answers, so that a first click on a simulated farm returns a real
// dump instead of the blank line the fake's catch-all gives anything it does not
// script.
//
// dumpsys battery earns the slot on its own terms: read-only, present on every
// handset, and the command internal/watchdog sends every minute. This pins the
// happy accident and nothing else.
//
// Falsify: change DEFAULT_ID to getprop, whose rendered command
// (getprop ro.build.fingerprint) the demo does not script.
func TestTheDefaultIsOneTheDemoAnswers(t *testing.T) {
	src := execSource(t)
	m := regexp.MustCompile(`const DEFAULT_ID = '(\w+)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("assets/exec.js has no DEFAULT_ID; this test is now blind")
	}

	var tpl string
	for _, c := range catalogue(t) {
		if c.id == m[1] {
			tpl = c.template
		}
	}
	if tpl == "" {
		t.Fatalf("DEFAULT_ID is %q, which is not in the catalogue", m[1])
	}

	demo, err := readRepoFile("internal/demo/demo.go")
	if err != nil {
		t.Fatalf("read internal/demo/demo.go: %v", err)
	}
	if demoScripts(t, string(demo), tpl) {
		return
	}
	t.Errorf("the default command is %q and internal/demo scripts no such command.\n\n"+
		"On a simulated farm the first click then returns one blank line with exit 0 — "+
		"the fake's catch-all — which reads as a command that worked and printed nothing.",
		tpl)
}

// demoScripts reports whether internal/demo answers this command, following one
// level of indirection.
//
// The indirection is not incidental and this test learned it the hard way: the
// demo registers the battery responder as
// adbwire.ShellService(watchdog.BatteryCommand), not as a literal — deliberately,
// so the fake answers exactly what the watchdog asks and the two cannot drift.
// A test that only looked for the literal would report a command the demo
// answers perfectly well as one it does not, which is a test lying about working
// code.
func demoScripts(t *testing.T, demo, command string) bool {
	t.Helper()
	if strings.Contains(demo, `"`+command+`"`) {
		return true
	}

	// Find a Go constant anywhere under internal/ whose value is this command,
	// and require the demo to name it.
	var named []string
	err := filepath.Walk(filepath.Join("..", ".."), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w+)\s*=\s*"` + regexp.QuoteMeta(command) + `"`)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			named = append(named, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	for _, name := range named {
		if strings.Contains(demo, name) {
			return true
		}
	}
	return false
}
