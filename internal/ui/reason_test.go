package ui

// The reason field, held to what the server actually requires.
//
// The dashboard demanded a typed reason for all six of its confirmable actions.
// The server demands one for five of them: POST /api/v1/jobs/{id}/cancel decodes
// a body its own comment calls "optional here" and never looks at the field, and
// farm.audit_log has stored nullif($4,'') into a nullable column since
// migrations/00003_ops.sql. So one of those six demands was the page's own
// invention, and an operator cancelling a job had to write a sentence nobody
// would ever read.
//
// Relaxing that is easy to get wrong in two specific ways, and both of them are
// silent:
//
//   1. Relaxing the JavaScript and leaving `required` on the input. The browser
//      blocks the submit event before any script runs, so the whole change is
//      invisible and looks like a bug in the new code rather than a leftover in
//      the old markup. TestTheReasonInputIsNotHTMLRequired is the guard for it.
//
//   2. Relaxing an action the server still refuses. That trades a clear
//      requirement for a 400 the operator meets after clicking Confirm, which
//      reads like an outage. TestTheConfirmDialogRequiresAReasonOnlyWhereTheServerDoes
//      is the guard, and it is the strongest test here because it does not
//      believe a comment: it resolves each route through internal/api/router.go
//      to the handler that serves it and reads that handler's own source.
//
// The style is the one exec_test.go and i18n_test.go already use — Go asserting
// over the TEXT of the assets, with a floor under every parse so a changed shape
// fails loudly instead of passing over zero findings.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func appSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read the embedded assets/app.js: %v", err)
	}
	return string(b)
}

func indexSource(t *testing.T) string {
	t.Helper()
	b, err := embedded.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read the embedded assets/index.html: %v", err)
	}
	return string(b)
}

// confirmSpec is one openConfirm({…}) literal in assets/app.js.
type confirmSpec struct {
	fn     string // the function it sits in, for a legible failure
	mode   string // spec.reason: "required", "optional", or "" if absent
	route  string // spec.route, the route the message names to the operator
	posted string // the route derived from the api.post(…) call in spec.run
	line   int
}

// confirmSpecs parses every openConfirm literal out of assets/app.js.
//
// By text, because there is no JavaScript engine in a Go test — the reasoning
// i18n_test.go states for the dictionaries and exec_test.go for the command
// catalogue. The brace scanner below skips string literals and comments so that
// a brace inside either cannot unbalance the count, and every caller checks a
// floor.
func confirmSpecs(t *testing.T) []confirmSpec {
	t.Helper()
	src := appSource(t)

	mode := regexp.MustCompile(`\breason:\s*'(\w+)'`)
	route := regexp.MustCompile(`\broute:\s*'([^']*)'`)
	enclosing := regexp.MustCompile(`(?m)^function (\w+)\(`)

	var out []confirmSpec
	for at := 0; ; {
		i := strings.Index(src[at:], "openConfirm({")
		if i < 0 {
			break
		}
		i += at
		open := i + len("openConfirm(")
		body, ok := jsBlock(src, open)
		if !ok {
			t.Fatalf("the openConfirm literal at byte %d is not closed; the brace scanner is "+
				"lost and this test is now blind", i)
		}
		at = open + len(body)

		spec := confirmSpec{
			fn:     "?",
			posted: postedRoute(body),
			line:   1 + strings.Count(src[:i], "\n"),
		}
		if m := enclosing.FindAllStringSubmatch(src[:i], -1); len(m) > 0 {
			spec.fn = m[len(m)-1][1]
		}
		if m := mode.FindStringSubmatch(body); m != nil {
			spec.mode = m[1]
		}
		if m := route.FindStringSubmatch(body); m != nil {
			spec.route = m[1]
		}
		out = append(out, spec)
	}
	return out
}

// jsBlock returns the {…} block beginning at the first brace at or after open,
// braces included. Strings and comments are skipped whole.
func jsBlock(src string, open int) (string, bool) {
	start := strings.IndexByte(src[open:], '{')
	if start < 0 {
		return "", false
	}
	start += open

	depth := 0
	for i := start; i < len(src); i++ {
		switch c := src[i]; c {
		case '\'', '"', '`':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return "", false
			}
			i = j
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				k := strings.IndexByte(src[i:], '\n')
				if k < 0 {
					return "", false
				}
				i += k
			} else if i+1 < len(src) && src[i+1] == '*' {
				k := strings.Index(src[i+2:], "*/")
				if k < 0 {
					return "", false
				}
				i += 2 + k + 1
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// postedRoute derives the API route from the api.post(…) call inside a confirm
// spec: the single-quoted fragments of its first argument, joined by {id}.
//
// `api.post('hosts/' + encodeURIComponent(host) + '/drain', { reason })` gives
// `hosts/{id}/drain`, which is the pattern internal/api/router.go registers. It
// is derived from the CALL rather than declared beside it, so a spec cannot
// claim to post somewhere it does not.
func postedRoute(body string) string {
	at := strings.Index(body, "api.post(")
	if at < 0 {
		return ""
	}
	var lits []string
	depth := 1
	for i := at + len("api.post("); i < len(body) && depth > 0; i++ {
		switch c := body[i]; c {
		case '\'':
			j := i + 1
			for j < len(body) && body[j] != '\'' {
				if body[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(body) {
				return ""
			}
			if depth == 1 {
				lits = append(lits, body[i+1:j])
			}
			i = j
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 1 {
				depth = 0 // the end of the first argument, which is the path
			}
		}
	}
	return strings.Join(lits, "{id}")
}

// apiSources reads every non-test file in internal/api.
func apiSources(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "api")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/api: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read internal/api/%s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) < 5 {
		t.Fatalf("found %d files in internal/api; the package moved and this test is now blind", len(out))
	}
	return out
}

// handlerFor resolves a route pattern to the Server method registered for it.
func handlerFor(t *testing.T, pattern string) string {
	t.Helper()
	b, err := readRepoFile("internal/api/router.go")
	if err != nil {
		t.Fatalf("read internal/api/router.go: %v", err)
	}
	re := regexp.MustCompile(`"POST /api/v1/` + regexp.QuoteMeta(pattern) + `",\s*s\.(\w+)\)`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		return ""
	}
	return m[1]
}

// serverMethodBody returns the source of `func (s *Server) name(…) {…}`.
func serverMethodBody(files map[string]string, name string) string {
	re := regexp.MustCompile(`(?m)^func \(s \*Server\) ` + regexp.QuoteMeta(name) + `\(`)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		src := files[n]
		loc := re.FindStringIndex(src)
		if loc == nil {
			continue
		}
		if body, ok := goBlock(src, loc[1]); ok {
			return body
		}
	}
	return ""
}

// goBlock is jsBlock for Go: the {…} block beginning at the first brace at or
// after open, with strings, runes and comments skipped whole.
func goBlock(src string, open int) (string, bool) {
	start := strings.IndexByte(src[open:], '{')
	if start < 0 {
		return "", false
	}
	start += open

	depth := 0
	for i := start; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return "", false
			}
			i = j
		case '`':
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				return "", false
			}
			i += 1 + j
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				k := strings.IndexByte(src[i:], '\n')
				if k < 0 {
					return "", false
				}
				i += k
			} else if i+1 < len(src) && src[i+1] == '*' {
				k := strings.Index(src[i+2:], "*/")
				if k < 0 {
					return "", false
				}
				i += 2 + k + 1
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// handlerSource is a handler's own body plus the bodies of the Server methods it
// hands the request straight to.
//
// The delegation is not a nicety. handleHostDrain is four lines that call
// s.setHostAdminState(w, r, …), and every check that route makes — the refusal
// this file is looking for included — lives there. A reader of handleHostDrain
// alone would conclude that draining a host needs no reason, which is exactly
// the wrong answer and exactly the falsification this test must catch.
func handlerSource(t *testing.T, files map[string]string, name string) string {
	t.Helper()
	body := serverMethodBody(files, name)
	if body == "" {
		return ""
	}
	seen := map[string]bool{name: true}
	for _, m := range regexp.MustCompile(`\bs\.(\w+)\(w, r`).FindAllStringSubmatch(body, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		body += "\n" + serverMethodBody(files, m[1])
	}
	return body
}

// reasonRefusal is the shape every one of these handlers uses to turn an empty
// reason away: the trimmed field compared against "", answered by badRequest or
// writeError.
var reasonRefusal = regexp.MustCompile(`(?s)(?:reason|req\.Reason\))\s*==\s*""\s*\{\s*(?:badRequest|writeError)`)

// TestTheConfirmDialogRequiresAReasonOnlyWhereTheServerDoes.
//
// For every action the dialog marks 'optional', this resolves the route it
// actually posts to through internal/api/router.go and reads the handler that
// serves it. If that handler still refuses an empty reason, the dialog is
// promising something the server will not honour, and the operator meets a 400
// after clicking Confirm — a requirement turned into what reads like a bug.
//
// It checks the UI's claim against the SERVER rather than against a comment,
// which is the only version of this test worth having: comments about another
// package's behaviour are exactly what goes stale.
//
// Falsify: mark hosts/{id}/drain optional.
func TestTheConfirmDialogRequiresAReasonOnlyWhereTheServerDoes(t *testing.T) {
	specs := confirmSpecs(t)
	if len(specs) < 6 {
		t.Fatalf("parsed %d openConfirm literals out of assets/app.js; there are at least six "+
			"and the parse has gone blind", len(specs))
	}
	files := apiSources(t)

	var optional, detected int
	for _, s := range specs {
		if s.posted == "" {
			t.Errorf("%s (app.js:%d) opens a confirm whose run() posts nowhere this test can "+
				"read; it cannot be checked against the server", s.fn, s.line)
			continue
		}
		// The route named on screen and the route actually posted to must be the
		// same one, or the message that teaches the operator which rule they met
		// names a rule from somewhere else. An ABSENT route is the same failure
		// in its quietest form: showConfirmNotice falls back to the empty string
		// and tells the operator that "POST /api/v1/ answers 400", which is a
		// route that does not exist.
		if s.route != s.posted {
			t.Errorf("%s (app.js:%d) declares route %q but posts to %q; the message that names "+
				"the rule to the operator would name a route this action does not call",
				s.fn, s.line, s.route, s.posted)
		}

		handler := handlerFor(t, s.posted)
		if handler == "" {
			t.Errorf("%s (app.js:%d) posts to %q, which internal/api/router.go does not register "+
				"as a POST; either the dashboard calls a route that does not exist or this "+
				"test can no longer find it", s.fn, s.line, s.posted)
			continue
		}
		src := handlerSource(t, files, handler)
		if src == "" {
			t.Fatalf("cannot find func (s *Server) %s in internal/api; this test is now blind "+
				"about %s", handler, s.posted)
		}

		refuses := reasonRefusal.MatchString(src)
		if refuses {
			detected++
		}
		if s.mode != "optional" {
			continue
		}
		optional++
		if refuses {
			t.Errorf("%s (app.js:%d) tells the operator a reason is OPTIONAL for %s, but %s "+
				"still refuses a request with an empty one.\n\n"+
				"The dialog would send it and the server would answer 400, which reads as a "+
				"bug rather than as a rule. Either the server changed, or this action is not "+
				"one of the optional ones.",
				s.fn, s.line, s.posted, handler)
		}
	}

	if optional < 1 {
		t.Error("no confirm action is marked reason: 'optional'. POST /api/v1/jobs/{id}/cancel " +
			"accepts a request with no reason and the dashboard used to demand one anyway; " +
			"if that demand is back, this is the test that should have stopped it.")
	}
	if detected < 4 {
		t.Fatalf("only %d of these handlers were seen to refuse an empty reason, and at least "+
			"four do. The refusal in internal/api no longer matches the pattern this test "+
			"looks for, so a route marked optional would pass here whatever the server does.",
			detected)
	}
}

// TestEveryConfirmActionDeclaresItsReasonMode.
//
// openConfirm defaults to 'required' precisely so that an action added without
// thinking still asks for a reason. That default is a safety net and not a
// licence to leave the field off: which of the two cases an action is in was
// decided by reading internal/api, and an action that never states its mode is
// an action nobody has read the server for.
//
// Falsify: add a seventh action without one.
func TestEveryConfirmActionDeclaresItsReasonMode(t *testing.T) {
	specs := confirmSpecs(t)
	if len(specs) < 6 {
		t.Fatalf("parsed %d openConfirm literals out of assets/app.js; there are at least six "+
			"and the parse has gone blind", len(specs))
	}

	var undeclared []string
	for _, s := range specs {
		switch s.mode {
		case "required", "optional":
		case "":
			undeclared = append(undeclared, s.fn+" (app.js:"+strconv.Itoa(s.line)+")")
		default:
			t.Errorf("%s (app.js:%d) declares reason: %q, which is neither 'required' nor "+
				"'optional'; reasonRequired() reads anything but 'optional' as required, so "+
				"this is a typo that silently keeps the strict behaviour", s.fn, s.line, s.mode)
		}
	}
	if len(undeclared) > 0 {
		t.Errorf("%d confirm action(s) do not say whether the server requires a reason:\n  %s\n\n"+
			"Read the handler in internal/api and write reason: 'required' or reason: "+
			"'optional' on the spec. The default is 'required', which is safe and is not a "+
			"substitute for having looked.",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
}

// TestTheReasonInputIsNotHTMLRequired.
//
// This is the guard against "we relaxed the JavaScript and nothing changed",
// which is the exact trap this whole change exists to escape. A `required`
// attribute makes the browser refuse to fire the submit event at all, so
// wireConfirm's handler — where the server's real rule is applied — never runs,
// and the operator gets a native tooltip demanding a reason for an action that
// does not need one. The relaxation would be invisible and the cause would be a
// leftover in markup nobody was editing.
//
// Falsify: re-add it.
func TestTheReasonInputIsNotHTMLRequired(t *testing.T) {
	src := indexSource(t)

	var line string
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, `id="confirm-reason"`) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal(`no element with id="confirm-reason" in assets/index.html; the confirm dialog ` +
			`changed shape and this test is now blind`)
	}
	if regexp.MustCompile(`(?:^|\s)required(?:[\s=>]|$)`).MatchString(line) {
		t.Errorf("the reason input is marked `required` in the HTML:\n  %s\n\n"+
			"That attribute blocks the submit event before any script runs, so the mode "+
			"openConfirm decided — and the server actually enforces — is never consulted. "+
			"The guard belongs in wireConfirm, where it can tell the two cases apart.",
			strings.TrimSpace(line))
	}
	// The label and the note are what tell the operator which case this is, and
	// the input has to point at the note for a screen reader to read them
	// together.
	if !strings.Contains(line, `aria-describedby="confirm-note"`) {
		t.Errorf("the reason input does not describe itself with confirm-note:\n  %s\n\n"+
			"Without it the sentence that says what the reason is for is announced to "+
			"nobody using a screen reader.", strings.TrimSpace(line))
	}
}
