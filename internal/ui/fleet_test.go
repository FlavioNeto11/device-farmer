package ui

// The fleet grid reconciles; it does not rebuild. This file is what keeps it
// that way after everybody who remembers why has moved on.
//
// The grid used to end its render in body.replaceChildren(frag). Measured
// against a running demo, that cost: the fleet body rebuilt six times in twelve
// seconds; a tile focused and then left alone for eight seconds gone from the
// document with document.activeElement fallen back to <body>; and browser
// automation unable to click a device at all, because the element it had just
// read went stale before the click landed. A human moving a mouse toward a tile
// is in the same race.
//
// None of that shows up in a screenshot, and none of it fails a build. It is a
// keyboard operator quietly losing their place every few seconds, which is the
// kind of defect that gets rediscovered rather than remembered. So it is
// asserted here, over the text of the asset, the way this package already
// asserts the command catalogue in exec_test.go and the dictionaries in
// i18n_test.go: there is no JavaScript engine in this test binary and adding
// one for this would be a dependency the project does not want.
//
// Every test below carries a floor. A regex over source text that finds nothing
// passes, silently, forever; the floors are what turn a changed shape into a
// failure instead of a suite that has quietly stopped looking.

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// assetSource reads an asset WITH ITS COMMENTS REMOVED.
//
// Every assertion in this file is about what the code does, and a comment that
// quotes code satisfies or trips those assertions without being code. Both
// directions were live here before this call was added: the paragraph at the
// top of fleet.js quotes the very call TestTheFleetDoesNotReplaceItsBody
// forbids, and commenting a field out of deviceSig left it visible to
// TestTheTileSignatureCoversEveryFieldTheTileRenders — a test that passed over
// the exact mutation it names as its falsification.
func assetSource(t *testing.T, name string) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("read the embedded assets/%s: %v", name, err)
	}
	return stripJSComments(string(b))
}

// jsFunc returns the body of a top-level function declaration.
//
// These files are written in one style: top-level declarations start at column
// zero and everything inside them is indented, so the first "\n}" after the
// header is the end of the function. A file that stops obeying that trips the
// floor in whichever test asked for the body, rather than silently returning a
// fragment.
func jsFunc(t *testing.T, src, name string) string {
	t.Helper()
	open := strings.Index(src, "\nfunction "+name+"(")
	if open < 0 {
		t.Fatalf("no top-level `function %s(` in the source; the shape changed and this test is now blind", name)
	}
	rest := src[open+1:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("`function %s(` is never closed at column zero; this test is now blind", name)
	}
	return rest[:end]
}

// stripJSComments blanks out line and block comments.
//
// A test that forbids a call must not be tripped by the comment explaining why
// the call is forbidden, and in this repository that comment is very likely to
// exist — it is the house style to write down what went wrong last time.
//
// Newlines are preserved so line numbers do not move. This is a lexer with one
// deliberate hole: `//` inside a string literal would start a comment here.
// None of these assets contains one, and the floors below fail loudly if that
// stops being true, which is the trade a hand-rolled scanner is worth against
// pulling in a JavaScript parser for four tests.
func stripJSComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	keepNewlines := func(s string) {
		for _, r := range s {
			if r == '\n' {
				b.WriteByte('\n')
			}
		}
	}
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			keepNewlines(src[i : i+2+end+2])
			i += 2 + end + 2
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				return b.String()
			}
			b.WriteByte('\n')
			i += end + 1
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

// jsFields returns every `d.<field>` read in a chunk of source.
func jsFields(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bd\.([A-Za-z_]\w*)`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheFleetDoesNotReplaceItsBody is the regression guard for the focus loss
// described at the top of this file.
//
// It is deliberately blunt: whatever assets/fleet.js binds $('#fleet-body') to
// may not have replaceChildren called on it, anywhere. There is no "render
// path" a reviewer could argue a new call sits outside of — every call in this
// file runs under renderFleet — and a narrower rule is one the next person
// works around by accident.
//
// Falsify: restore `body.replaceChildren(frag)` at the end of renderFleet.
func TestTheFleetDoesNotReplaceItsBody(t *testing.T) {
	src := assetSource(t, "fleet.js")

	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?:const|let|var)\s+(\w+)\s*=\s*\$\('#fleet-body'\)`).FindAllStringSubmatch(src, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("assets/fleet.js never binds $('#fleet-body') to a name. Either the fleet body is " +
			"reached some other way now or the grid moved out of this file; either way this test " +
			"is blind and has to be rewritten rather than deleted.")
	}

	for _, name := range sorted(names) {
		if strings.Contains(src, name+".replaceChildren(") {
			t.Errorf("assets/fleet.js calls %s.replaceChildren(), where %s is $('#fleet-body').\n\n"+
				"That throws away every tile on the grid and builds new ones. It is called from the "+
				"event stream, from the poller and from a fifteen-second clock tick, so a keyboard "+
				"operator's focused tile leaves the document — and document.activeElement falls back "+
				"to <body> — every few seconds. Patch the tiles in place through reconcile() instead.",
				name, name)
		}
	}
	if strings.Contains(src, "$('#fleet-body').replaceChildren(") {
		t.Error("assets/fleet.js calls replaceChildren() straight off $('#fleet-body'), which is the " +
			"same wholesale rebuild by another spelling.")
	}

	// And the positive half: the grid must still be drawn by something. A file
	// that neither replaces the body nor reconciles it draws nothing at all,
	// and would otherwise pass the assertions above.
	render := jsFunc(t, src, "renderFleet")
	if len(render) < 400 {
		t.Fatalf("renderFleet is %d characters long; the view moved and this test is now blind", len(render))
	}
	if !strings.Contains(src, "function reconcile(") {
		t.Error("assets/fleet.js has no reconcile(); the grid is drawn by something this test does not know about")
	}
	if !strings.Contains(render, "reconcile(body,") {
		t.Error("renderFleet never reconciles the fleet body itself, so whatever is in it is not " +
			"being kept across renders")
	}
}

// TestTheTileSignatureCoversEveryFieldTheTileRenders is the contract between
// deviceTile and deviceSig, checked rather than remembered.
//
// A reconciler skips a node whose signature has not changed. So a field the
// tile renders and the signature omits does not merely repaint late — it never
// repaints at all, and freezes on screen at whatever it was when the tile was
// first drawn. Health is in that list. An operator reading "healthy" off a
// frozen tile is worse off than one reading a rebuilt grid.
//
// The other direction is cheaper but still wrong: a field in the signature that
// the tile does not show repaints the tile for a change nobody can see.
//
// The check is over direct `d.field` reads in each function body, which is how
// both are written. A field the tile reaches only inside a helper it passes the
// whole row to is invisible from here — leaseChips(d) is the existing case, and
// it works because deviceTile also reads leaseState and protected itself. A
// helper added later that hides a field is the gap in this test, and the fix is
// to read the field in deviceTile too.
//
// Falsify: delete d.health from the deviceSig list.
func TestTheTileSignatureCoversEveryFieldTheTileRenders(t *testing.T) {
	src := assetSource(t, "fleet.js")
	rendered := jsFields(jsFunc(t, src, "deviceTile"))
	covered := jsFields(jsFunc(t, src, "deviceSig"))

	if len(rendered) < 10 {
		t.Fatalf("only %d device fields read in deviceTile; the tile is written some other way now "+
			"and this test is blind", len(rendered))
	}
	if len(covered) < 10 {
		t.Fatalf("only %d device fields in deviceSig; the signature is built some other way now and "+
			"this test is blind", len(covered))
	}

	var missing []string
	for _, f := range sorted(rendered) {
		if !covered[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		t.Errorf("deviceTile renders %s, and deviceSig does not cover them.\n\n"+
			"The reconciler compares signatures to decide whether to patch a tile, so each of these "+
			"is drawn once and then frozen there for as long as the tile lives. Add them to the "+
			"deviceSig list.", strings.Join(missing, ", "))
	}

	var extra []string
	for _, f := range sorted(covered) {
		if !rendered[f] {
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		t.Errorf("deviceSig covers %s, which deviceTile does not render.\n\n"+
			"Every change to one of those repaints the tile for something nobody can see. Drop them "+
			"from the signature, or render them.", strings.Join(extra, ", "))
	}
}

// TestTheFleetKeysTilesByDeviceIDAlone.
//
// The key is the one thing in a reconciler that cannot be got wrong quietly.
// Two rows under one key make one tile stand for both, and the second one's
// health, lease and battery appear under the first one's rack slot — which is
// how an operator power-cycles the wrong handset. Unlike a rebuild, it looks
// completely fine on screen.
//
// Rack slot, serial and usb path are all labels a human can edit, duplicate or
// leave off. The id is the row's identity in the database. So the key reads the
// id and nothing else.
//
// Falsify: key tiles by d.rackSlot, or add `|| d.serial` to deviceKey.
func TestTheFleetKeysTilesByDeviceIDAlone(t *testing.T) {
	src := assetSource(t, "fleet.js")
	body := jsFunc(t, src, "deviceKey")
	read := jsFields(body)

	if len(read) == 0 {
		t.Fatal("deviceKey reads no field off the device row at all; it is built some other way now " +
			"and this test is blind")
	}
	for _, f := range sorted(read) {
		if f != "id" {
			t.Errorf("deviceKey reads d.%s. The tile key must be the device id and nothing else: "+
				"every other candidate is a label that can be edited, duplicated or absent, and two "+
				"rows sharing a key shows one device's data under another device's name.", f)
		}
	}

	if !strings.Contains(jsFunc(t, src, "renderFleet"), "deviceKey") {
		t.Error("renderFleet does not reconcile the devices with deviceKey, so whatever keys the " +
			"tiles is not the function this test just checked")
	}
}

// TestTheGridHoldsStillWhileSomebodyIsWorkingInIt.
//
// Keeping the elements alive is most of the fix, but not all of it: a device
// leaving the fleet, or the order changing, still moves the tile a keyboard
// operator is standing on. So structural change is deferred while focus is
// inside the grid or the device sheet is open — and, because a grid that
// silently withholds news is its own failure, what is being held back is said
// out loud in the live region with a control that applies it.
//
// Falsify: replace `fleetHeldBack()` in renderFleet with `false`, delete the
// #dlg-device check from fleetHeldBack, or drop the t('fleet.held')
// announcement.
func TestTheGridHoldsStillWhileSomebodyIsWorkingInIt(t *testing.T) {
	src := assetSource(t, "fleet.js")
	guard := jsFunc(t, src, "fleetHeldBack")
	if len(guard) < 60 {
		t.Fatalf("fleetHeldBack is %d characters long; the guard was gutted or moved and this test "+
			"is blind", len(guard))
	}

	for _, want := range []string{"document.activeElement", "#fleet-body", "#dlg-device"} {
		if !strings.Contains(guard, want) {
			t.Errorf("fleetHeldBack never looks at %s, so it cannot tell whether anybody is working "+
				"in the grid", want)
		}
	}
	if !strings.Contains(jsFunc(t, src, "renderFleet"), "fleetHeldBack()") {
		t.Error("renderFleet never asks fleetHeldBack(), so the guard is dead code and tiles move " +
			"under whoever is on them")
	}

	// Held-back news that is never announced is just a stale screen. Both
	// strings are checked by TestEveryKeyTheAppAsksForExists for existing in
	// both dictionaries; what is checked here is that they are asked for at all.
	for _, key := range []string{"fleet.held", "fleet.heldApply"} {
		if !strings.Contains(src, "t('"+key+"'") {
			t.Errorf("assets/fleet.js never renders %s, so a grid holding a change back never says "+
				"so and never offers to apply it", key)
		}
	}
}

// TestThePollerLeavesTheFleetToTheStream.
//
// The five-second fallback poller and the event stream overlap: stopPolling
// runs when the stream opens and again on the first event after it recovers,
// which leaves windows where both are feeding the same view. For six of the
// seven views a redundant refetch is only wasted bandwidth. For the fleet it is
// a second, unsynchronised source of truth for the one grid an operator works
// inside, and every answer it produces arrives at the reconciler as a change.
//
// Falsify: restore `loadFor(state.view)` with no skip set in startPolling.
func TestThePollerLeavesTheFleetToTheStream(t *testing.T) {
	body := jsFunc(t, assetSource(t, "app.js"), "startPolling")
	if len(body) < 80 {
		t.Fatalf("startPolling is %d characters long; the poller was rewritten and this test is blind",
			len(body))
	}
	if !strings.Contains(body, "state.conn.mode") {
		t.Error("startPolling never reads state.conn.mode, so it refetches the fleet every five " +
			"seconds whether or not the event stream is already delivering it. Gate the fleet " +
			"refetch on the connection state rather than inventing a second flag for it.")
	}
	if !strings.Contains(body, "'live'") {
		t.Error("startPolling reads the connection state but not for 'live', which is the one mode " +
			"in which the stream is already telling us about the fleet")
	}
	// Reading the mode is not the same as acting on it. loadFor's second
	// argument is what actually holds the fleet back; a call without one
	// refetches everything the view needs, stream or no stream.
	if regexp.MustCompile(`loadFor\(\s*state\.view\s*\)`).MatchString(body) {
		t.Error("startPolling calls loadFor(state.view) with no skip set, so it refetches the fleet " +
			"on every tick regardless of what it just read out of the connection state")
	}
}
