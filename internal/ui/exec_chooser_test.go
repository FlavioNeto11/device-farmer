package ui

// The command chooser, and the widget options that were documented and never
// written.
//
// Two defects are pinned here, and the second one is the interesting one.
//
// The first is a layout claim: the catalogue used to render inline, twenty-four
// buttons under ten headings, above the field an operator was trying to type in
// — and with both tiers open at once, so IDENTITY and HEALTH each appeared
// twice and the wall read as one duplicated list rather than as two claims of
// different standing. Moving it into a dialog is only a fix while it stays
// there, and nothing but a test keeps a catalogue out of a panel.
//
// The second is a comment that described code that did not exist.
// commandBuilder's own header documented `compact` and `onRun`; grep found
// `compact` exactly once in the whole file, in that comment. The bulk form —
// the one that reaches every device a selector matches — went on shipping a
// bare <input> with no catalogue, no wire line and no floor under its timeout,
// while the widget's documentation said otherwise. A comment that lies is worse
// than no comment: the next person reads it as a contract and builds on it.

import (
	"regexp"
	"strings"
	"testing"
)

// The two files the bulk form is spread across. Named for this file so that a
// sibling adding its own helper for the same asset does not collide here.
func chooserIndexSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read the embedded assets/index.html: %v", err)
	}
	return string(b)
}

func chooserAppSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read the embedded assets/app.js: %v", err)
	}
	return string(b)
}

// TestTheBulkFormMountsTheSameBuilder.
//
// One wrong command in the device drawer reaches one handset. The same command
// in the bulk form reaches every device the selector matched — fifty-six of them
// on the demo farm — so the form with the WEAKER pre-flight was the one with the
// larger blast radius. It had no catalogue, no `shell,v2,raw:` line and no
// timeout floor, and the reason was that nothing mounted the widget that has
// all three.
//
// Falsify: revert Bulk to a plain <input id="bulk-command">.
func TestTheBulkFormMountsTheSameBuilder(t *testing.T) {
	page := chooserIndexSource(t)
	app := chooserAppSource(t)

	// The mount point must still exist, or this test has gone blind on a
	// renamed id rather than proving anything about the form.
	if !strings.Contains(page, `id="bulk-command"`) {
		t.Fatal("assets/index.html has no #bulk-command at all; the bulk form changed shape " +
			"and this test can no longer see it")
	}

	bare := regexp.MustCompile(`<input[^>]*\bid="bulk-command"`)
	if m := bare.FindString(page); m != "" {
		t.Errorf("the bulk form still carries a bare command <input>:\n  %s\n\n"+
			"That field has no catalogue, no wire line and no timeout floor, and it is the "+
			"one that addresses the whole fleet. It must be a mount point for "+
			"commandBuilder({ compact: true }).", strings.TrimSpace(m))
	}

	if !strings.Contains(app, "commandBuilder(") {
		t.Error("assets/app.js never calls commandBuilder(. The bulk form is then building its " +
			"own command field again, which is the state this test exists to prevent.")
	}
	if strings.Contains(app, `$('#bulk-command').value`) {
		t.Error("assets/app.js still reads $('#bulk-command').value. The command must come from " +
			"the builder — builder.command() — or the widget is mounted for decoration and " +
			"the run is submitted from somewhere else.")
	}
}

// TestTheCommandBuilderOptionsAreRead is the guard for the defect this file is
// named after: a doc comment describing an API the code does not have.
//
// commandBuilder's header lists its options one per line. Every name it lists
// must be read in the function body. Nothing else in this package could have
// caught it — the code compiled, the page rendered, the tests passed, and the
// only thing that was wrong was that the documentation was fiction.
//
// Falsify: document a third option in commandBuilder's header and do not
// implement it.
func TestTheCommandBuilderOptionsAreRead(t *testing.T) {
	src := execSource(t)

	// The options block: from `* opts:` to the end of that comment.
	open := strings.Index(src, "* opts:")
	if open < 0 {
		t.Fatal("assets/exec.js has no `* opts:` block in commandBuilder's header; the comment " +
			"changed shape and this test is now blind")
	}
	shut := strings.Index(src[open:], "*/")
	if shut < 0 {
		t.Fatal("commandBuilder's header comment is not closed")
	}

	// `   *   name     description` — a name at exactly three spaces past the
	// star, followed by the gap that separates it from its prose. A
	// continuation line is indented further and matches nothing here.
	names := regexp.MustCompile(`(?m)^\s*\*\s{3}(\w+)\s{2,}\S`).
		FindAllStringSubmatch(src[open:open+shut], -1)
	if len(names) < 3 {
		t.Fatalf("parsed %d documented options out of commandBuilder's header, want at least 3; "+
			"the comment's shape changed and this test is now blind", len(names))
	}

	// The function body, which is where a documented option has to be read.
	bodyAt := strings.Index(src, "function commandBuilder(opts) {")
	if bodyAt < 0 {
		t.Fatal("assets/exec.js no longer declares function commandBuilder(opts); this test is blind")
	}
	end := strings.Index(src[bodyAt:], "function execPanel(")
	if end < 0 {
		t.Fatal("assets/exec.js no longer declares execPanel after commandBuilder; this test " +
			"cannot bound the function body")
	}
	body := src[bodyAt : bodyAt+end]

	for _, m := range names {
		name := m[1]
		if strings.Contains(body, "o."+name) || strings.Contains(body, "opts."+name) {
			continue
		}
		t.Errorf("commandBuilder documents an option %q and never reads it.\n\n"+
			"A comment that describes behaviour the function does not have is how the bulk "+
			"form came to ship a bare <input> for months while the widget's own header said "+
			"it supported one. Implement it, or stop documenting it.", name)
	}
}

// TestTheCatalogueIsBehindAChooser.
//
// The catalogue belongs in a dialog with a filter and a one-tier-at-a-time
// segmented control. Rendering both tiers is what made the group names look
// duplicated — IDENTITY and HEALTH once under each — so the structural fix is
// that only one tier is ever built into the panel's reach.
//
// Falsify: re-inline the two tiers in commandBuilder's node.
func TestTheCatalogueIsBehindAChooser(t *testing.T) {
	src := execSource(t)
	page := chooserIndexSource(t)

	m := regexp.MustCompile(`const CHOOSER_DIALOG_ID = '([\w-]+)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("assets/exec.js has no CHOOSER_DIALOG_ID; the catalogue is no longer behind a " +
			"dialog, or this test is blind")
	}
	if !strings.Contains(page, `id="`+m[1]+`"`) {
		t.Errorf("assets/exec.js fills a dialog #%s and assets/index.html has no such element.\n\n"+
			"The chooser then never opens and the Pick a command button does nothing at all, "+
			"silently — openChooser returns false and the operator sees no dialog.", m[1])
	}

	// Both tier notes must be rendered, and both must be rendered in the
	// chooser rather than in the widget. The chooser is declared above
	// commandBuilder, so a note that has moved back into the panel appears
	// after the function's own opening line.
	panelAt := strings.Index(src, "function commandBuilder(opts) {")
	if panelAt < 0 {
		t.Fatal("assets/exec.js no longer declares function commandBuilder(opts); this test is blind")
	}
	for _, key := range []string{"exec.tier.verifiedNote", "exec.tier.unverifiedNote"} {
		at := strings.Index(src, "'"+key+"'")
		if at < 0 {
			t.Errorf("assets/exec.js never renders %s. The tier note is the claim the tier makes; "+
				"without it the second list is just a shorter one with no tick.", key)
			continue
		}
		if strings.Count(src, "'"+key+"'") != 1 {
			t.Errorf("%s is rendered more than once in assets/exec.js. Two tiers on screen at "+
				"once is the duplicated-group-names bug returning.", key)
		}
		if at > panelAt {
			t.Errorf("%s is rendered inside commandBuilder rather than in the chooser.\n\n"+
				"That is the catalogue back in the panel: twenty-four buttons under ten "+
				"headings above the field, both tiers at once, IDENTITY and HEALTH twice.", key)
		}
	}

	// And the panel's own way in is a single button.
	if !strings.Contains(src, "'exec.chooser.open'") {
		t.Error("assets/exec.js does not render exec.chooser.open, so the panel has no button " +
			"that opens the chooser and the catalogue is unreachable")
	}
}
