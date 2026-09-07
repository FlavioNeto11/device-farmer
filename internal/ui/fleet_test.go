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
	body, ok := jsFuncIn(src, name)
	if !ok {
		t.Fatalf("no closed top-level `function %s(` in the source; the shape changed and this test is now blind", name)
	}
	return body
}

// jsFuncIn is jsFunc without the fatal: it reports whether the function is
// there at all, which is what the field walk below needs when it follows a call
// into a file that may not define it.
func jsFuncIn(src, name string) (string, bool) {
	open := strings.Index(src, "\nfunction "+name+"(")
	if open < 0 {
		return "", false
	}
	rest := src[open+1:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
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
// pulling in a JavaScript parser for six tests.
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

// wholeRowCalls returns the names of the functions this chunk of source hands
// the WHOLE device row to — `helper(d)`, `helper(d, cond)`, `subjectOf(x, d)`.
//
// It is the fix for the one hole the first version of the signature test
// documented and then lived with. That test read direct `d.field` accesses in
// deviceTile only, so a field the tile reached through a helper was invisible
// to it: the tile's own comment said the workaround was to read the field in
// deviceTile too. That was tolerable when leaseChips(d) was the only case. It
// stopped being tolerable when unit 3 rewrote the tile around deviceCondition,
// availabilityOf, availabilityChip, availabilityLine and tileFlags — six of the
// tile's sixteen fields are now read nowhere but inside those, and "add a dummy
// read so a regex can see it" is a test bending the code to fit itself.
//
// A bare `d` as any argument, found by walking the parentheses rather than by a
// regex, so `batteryEl(d.battery)` (a field, not the row) and
// `openDevice(subjectOf(tr, d))` (a nested call, not the row) are told apart
// from `tileFlags(d, cond)`.
func wholeRowCalls(src string) []string {
	var out []string
	call := regexp.MustCompile(`\b([A-Za-z_]\w*)\s*\(`)
	for _, loc := range call.FindAllStringSubmatchIndex(src, -1) {
		name := src[loc[2]:loc[3]]
		args, ok := jsArgs(src, loc[1]-1)
		if !ok {
			continue
		}
		for _, a := range args {
			if strings.TrimSpace(a) == "d" {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// jsArgs splits the argument list whose opening parenthesis is at src[open],
// respecting nesting and string literals. Comments are already gone.
func jsArgs(src string, open int) ([]string, bool) {
	depth := 0
	start := open + 1
	var args []string
	for i := open; i < len(src); i++ {
		switch c := src[i]; c {
		case '\'', '"', '`':
			for i++; i < len(src); i++ {
				if src[i] == '\\' {
					i++
					continue
				}
				if src[i] == c {
					break
				}
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 && c == ')' {
				args = append(args, src[start:i])
				return args, true
			}
		case ',':
			if depth == 1 {
				args = append(args, src[start:i])
				start = i + 1
			}
		}
	}
	return nil, false
}

// deviceFieldsRendered is the transitive field list of a renderer: every
// `d.field` it reads itself, plus every field read by the helpers it hands the
// whole row to, recursively, in either of the two files that draw the fleet.
//
// The remaining hole is a helper that takes the row under some other parameter
// name. That direction fails LOUDLY rather than quietly — the fields it reads
// go missing from the rendered set and turn up as "covered but not rendered" in
// the test below — which is the safe way round for a check whose whole job is
// to notice a divergence.
func deviceFieldsRendered(t *testing.T, root string, sources ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	seen := map[string]bool{}

	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, src := range sources {
			body, ok := jsFuncIn(src, name)
			if !ok {
				continue
			}
			for f := range jsFields(body) {
				out[f] = true
			}
			for _, next := range wholeRowCalls(body) {
				walk(next)
			}
		}
	}

	if _, ok := jsFuncIn(sources[0], root); !ok {
		t.Fatalf("no top-level `function %s(` in assets/fleet.js; the renderer was moved or renamed "+
			"and this test is now blind", root)
	}
	walk(root)
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

// checkSignature is the two-way contract between a renderer and its signature.
//
// A reconciler skips a node whose signature has not changed. So a field the node
// renders and the signature omits does not merely repaint late — it never
// repaints at all, and freezes on screen at whatever it was when the node was
// first drawn. Health is in both of these lists. An operator reading "healthy"
// off a frozen tile is worse off than one reading a rebuilt grid.
//
// The other direction is cheaper but still wrong: a field in the signature that
// the node does not show repaints it for a change nobody can see.
func checkSignature(t *testing.T, what, renderer, sig string, floor int, fleetSrc, appSrc string) {
	t.Helper()
	rendered := deviceFieldsRendered(t, renderer, fleetSrc, appSrc)
	covered := jsFields(jsFunc(t, fleetSrc, sig))

	if len(rendered) < floor {
		t.Fatalf("only %d device fields reached from %s (%s); the %s is written some other way now "+
			"and this test is blind. Found: %s",
			len(rendered), renderer, what, what, strings.Join(sorted(rendered), ", "))
	}
	if len(covered) < floor {
		t.Fatalf("only %d device fields in %s; the signature is built some other way now and this "+
			"test is blind", len(covered), sig)
	}

	var missing []string
	for _, f := range sorted(rendered) {
		if !covered[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s renders %s, and %s does not cover them.\n\n"+
			"The reconciler compares signatures to decide whether to patch a node, so each of these "+
			"is drawn once and then frozen there for as long as the node lives. Add them to the %s list.",
			renderer, strings.Join(missing, ", "), sig, sig)
	}

	var extra []string
	for _, f := range sorted(covered) {
		if !rendered[f] {
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		t.Errorf("%s covers %s, which %s does not render.\n\n"+
			"Every change to one of those repaints the node for something nobody can see. Drop them "+
			"from the signature, or render them.", sig, strings.Join(extra, ", "), renderer)
	}
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
// The two panes inside the body are held to the same rule from the other end:
// they are the containers the reconcilers key their children on, so the test
// asks that both of them ARE reconciled, in the two functions that draw them.
//
// Falsify: restore `body.replaceChildren(frag)` at the end of renderFleet, or
// put `body.replaceChildren(cards, tbl)` back in fleetModeBoxes.
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

	// BOTH modes. The table is the one a farm of more than forty devices opens
	// in, so a reconciled card grid beside a rebuilt table would leave the
	// defect in place for exactly the farms it was measured on.
	for fn, what := range map[string]string{
		"renderFleetCards": "the card grid's host blocks, hub blocks and tiles",
		"fleetTableInto":   "the table's rows",
	} {
		body, ok := jsFuncIn(src, fn)
		if !ok {
			t.Errorf("assets/fleet.js has no %s(), so %s are drawn by something this test does not know about", fn, what)
			continue
		}
		if !strings.Contains(body, "reconcile(") {
			t.Errorf("%s never calls reconcile(), so %s are not being kept across renders", fn, what)
		}
	}
	for _, fn := range []string{"renderFleetCards(", "fleetTableInto("} {
		if !strings.Contains(render, fn) {
			t.Errorf("renderFleet never calls %s, so whatever draws that mode is not the function "+
				"this test just checked", fn)
		}
	}
}

// TestTheTileSignatureCoversEveryFieldTheTileRenders is the contract between
// deviceTile and deviceSig, checked rather than remembered.
//
// The field list is DERIVED from deviceTile — including the helpers the tile
// hands the whole row to, which is where six of its sixteen fields now live
// after unit 3's two-chip rewrite — and never written down twice. `fence` and
// `adbState` left the tile face for the device sheet in that rewrite and are
// correctly absent from the signature; `holder` arrived on it, in the sentence
// under the chips that says who to go and ask, and is correctly present.
//
// Falsify: delete d.health from the deviceSig list (it should fail as a field
// the tile renders and the signature does not cover), or add d.fence to it (it
// should fail as a field the signature covers and the tile does not render).
func TestTheTileSignatureCoversEveryFieldTheTileRenders(t *testing.T) {
	checkSignature(t, "tile", "deviceTile", "deviceSig", 14,
		assetSource(t, "fleet.js"), assetSource(t, "app.js"))
}

// TestTheTableRowSignatureCoversEveryFieldTheRowRenders is the same contract
// for the other mode.
//
// A table row shows a different set from a tile: no holder, no battery word,
// but the host, the hub path and the HOST's admin_state — a drained host takes
// every device on it out of the pool and a table has no host header to say so.
// Two renderers, two signatures, one rule, and neither list written by hand.
//
// The columns are reached through fleetColumns, whose cells are written as
// `(d) => cell(d)` precisely so this walk can follow them.
//
// Falsify: delete d.health from the fleetRowSig list, or drop the
// `hostAdminState` line.
func TestTheTableRowSignatureCoversEveryFieldTheRowRenders(t *testing.T) {
	checkSignature(t, "table row", "fleetColumns", "fleetRowSig", 16,
		assetSource(t, "fleet.js"), assetSource(t, "app.js"))
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
// id and nothing else — in both modes, because a tile and a row stand for the
// same handset and switching modes must not renumber the farm.
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

	for _, fn := range []string{"renderFleetCards", "fleetTableInto"} {
		body, ok := jsFuncIn(src, fn)
		if !ok {
			t.Errorf("assets/fleet.js has no %s()", fn)
			continue
		}
		if !strings.Contains(body, "deviceKey") {
			t.Errorf("%s does not key its devices with deviceKey, so whatever keys them is not the "+
				"function this test just checked", fn)
		}
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
// The guard reads :focus-visible rather than containing raw focus. Raw
// containment froze the grid permanently for every mouse user, because a click
// leaves focus on the tile and closing the device sheet puts it back.
//
// Falsify: replace `fleetHeldBack()` in renderFleet with `false`, delete the
// #dlg-device check from fleetHeldBack, drop the :focus-visible test, or drop
// the t('fleet.held') announcement.
func TestTheGridHoldsStillWhileSomebodyIsWorkingInIt(t *testing.T) {
	src := assetSource(t, "fleet.js")
	guard := jsFunc(t, src, "fleetHeldBack")
	if len(guard) < 60 {
		t.Fatalf("fleetHeldBack is %d characters long; the guard was gutted or moved and this test "+
			"is blind", len(guard))
	}

	for _, want := range []string{"document.activeElement", "#fleet-body", "#dlg-device", ":focus-visible"} {
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
