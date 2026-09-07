package ui

// The order of the exec panel, defended by the build.
//
// This is not a style preference. The panel shipped once with the catalogue
// first — twenty-four buttons under ten headings, two and a half screens of it
// — and then the command field, and then the wire line, and then a six-row
// pre-flight written in the order internal/api evaluates its refusals, and only
// after all of that a small Run button. Every one of those pieces is worth
// having. Together, in that order, they put two screens of reference material
// between an operator and the two controls they came for, and the user reported
// the same complaint twice: it is still not clear how to operate this.
//
// Reference material is what you read when the thing you did failed. It is not
// what you read on the way in. So the field and Run come first, and the
// pre-flight sits below Run in a summary that says whether anything blocks.
//
// Nothing here checks that the panel is pretty, because no test can. It checks
// the one property that was wrong, in the one direction it was wrong in.

import (
	"strings"
	"testing"
)

// TestTheRunButtonComesBeforeThePreflight.
//
// Both halves matter and they fail for different reasons. The class literals
// catch a stylesheet-driven rewrite that reintroduces the old order by building
// the nodes in it; the composition check catches the smaller and likelier
// mistake, which is moving one variable in the tree that assembles the panel
// while leaving every declaration where it is.
//
// Falsify: move the pre-flight back above the run row — either by declaring
// cmd-preflight before cmd-run-row, or by moving preflightBox above runRow in
// the cmd-builder composition.
func TestTheRunButtonComesBeforeThePreflight(t *testing.T) {
	src := execSource(t)

	// The floor: if these identifiers are gone the panel was rewritten and this
	// test is blind, which is worse than a failure because it reports success.
	const (
		runClass = "cmd-run-row"
		pfClass  = "cmd-preflight"
	)
	run := strings.Index(src, runClass)
	pf := strings.Index(src, pfClass)
	if run < 0 || pf < 0 {
		t.Fatalf("assets/exec.js names %q at %d and %q at %d; a negative index means the panel "+
			"no longer builds the control this test is about, and this test is now blind",
			runClass, run, pfClass, pf)
	}
	if run > pf {
		t.Errorf("assets/exec.js builds %q before %q.\n\n"+
			"The pre-flight is reference material: it answers \"why did that not run\", which is a "+
			"question asked after pressing Run and not before finding it. Above the run row it is "+
			"six rows of the server's own evaluation order standing between the operator and the "+
			"button — which is the arrangement the user complained about twice.", pfClass, runClass)
	}

	// And the tree that actually decides what the reader sees.
	open := strings.Index(src, "const node = el('div', { class: 'cmd-builder' }")
	if open < 0 {
		t.Fatal("assets/exec.js no longer composes a cmd-builder node; this test is now blind")
	}
	tree := src[open:]
	if end := strings.Index(tree, "\n\n"); end > 0 {
		tree = tree[:end]
	}
	runAt := strings.Index(tree, "runRow")
	pfAt := strings.Index(tree, "preflightBox")
	if runAt < 0 || pfAt < 0 {
		t.Fatalf("the cmd-builder composition mounts runRow at %d and preflightBox at %d; the "+
			"names changed and this half of the test is blind:\n\n%s", runAt, pfAt, tree)
	}
	if runAt > pfAt {
		t.Errorf("the cmd-builder tree mounts preflightBox before runRow, so the panel renders "+
			"the pre-flight above the Run row however the declarations above are ordered:\n\n%s", tree)
	}
}
