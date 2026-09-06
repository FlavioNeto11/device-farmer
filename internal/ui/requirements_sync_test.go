package ui

// The register on disk and the register in the Docs tab must say the same
// thing.
//
// REQUIREMENTS.md is the source; assets/docs/requirements.json is what an
// operator reads in the browser, and it is a hand-maintained copy. That copy
// has drifted twice already — sixty-eight rows disagreed once, and by the end
// of the round that fixed them thirty-three more cells had gone out of step —
// because nothing in the build reads both. Drift here is not cosmetic: the two
// surfaces disagree about whether a requirement is MET, and the one that is
// wrong is the one on the screen.
//
// So this test is the reconciliation, run on every build. It is deliberately
// strict about the whole row rather than about status alone: an evidence cell
// that still cites a migration replaced three versions ago is the same defect
// arriving more quietly.
//
// A pipe inside a code span still ends a Markdown cell, so cells are split on
// UNESCAPED pipes and the escapes are dropped on the way in — the JSON holds
// data, not Markdown, and a cell reading `a|b` should hold exactly that. The
// column-count check below is what catches an unescaped one: a row that renders
// with phantom columns on GitHub fails here first.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// registerRow matches a row of any of the register's per-area tables. The id
// column is the key; the header and separator rows do not match.
var registerRow = regexp.MustCompile(`^\| [A-Z]+-\d+ \|`)

// splitCells splits a Markdown table row on unescaped pipes, unescaping `\|`
// into a literal pipe. strings.Split cannot be used: it would cut inside a code
// span that names a CLI alternation, and every such row would look malformed.
func splitCells(line string) []string {
	body := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\' && i+1 < len(body) && body[i+1] == '|':
			cur.WriteByte('|')
			i++
		case body[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(body[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// markdownRegister reads REQUIREMENTS.md from the repository root and returns
// every row by id.
func markdownRegister(t *testing.T) map[string][]string {
	t.Helper()
	path := filepath.Join("..", "..", "REQUIREMENTS.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	rows := make(map[string][]string)
	for n, line := range strings.Split(string(b), "\n") {
		if !registerRow.MatchString(line) {
			continue
		}
		cells := splitCells(line)
		if len(cells) != 5 {
			t.Errorf("REQUIREMENTS.md:%d: %s has %d columns, want 5 (id, requirement, origin, status, evidence).\n"+
				"A pipe inside a code span still ends the cell; write it as %q.",
				n+1, cells[0], len(cells), `\|`)
			continue
		}
		if prev, dup := rows[cells[0]]; dup {
			t.Errorf("REQUIREMENTS.md:%d: %s appears twice; the first said %q", n+1, cells[0], prev[1])
		}
		rows[cells[0]] = cells
	}
	if len(rows) == 0 {
		t.Fatal("REQUIREMENTS.md parsed to zero rows — the table format changed and this test is now blind")
	}
	return rows
}

// jsonRegister reads the rows out of the embedded Docs page, which is what the
// browser renders.
func jsonRegister(t *testing.T) map[string][]string {
	t.Helper()
	var page struct {
		Sections []struct {
			Table struct {
				Rows [][]string `json:"rows"`
			} `json:"table"`
		} `json:"sections"`
	}
	b, err := embedded.ReadFile("assets/docs/requirements.json")
	if err != nil {
		t.Fatalf("read the embedded requirements.json: %v", err)
	}
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatalf("decode requirements.json: %v", err)
	}
	id := regexp.MustCompile(`^[A-Z]+-\d+$`)
	rows := make(map[string][]string)
	for _, s := range page.Sections {
		for _, r := range s.Table.Rows {
			if len(r) == 0 || !id.MatchString(strings.TrimSpace(r[0])) {
				continue
			}
			rows[strings.TrimSpace(r[0])] = r
		}
	}
	if len(rows) == 0 {
		t.Fatal("requirements.json parsed to zero rows — the page structure changed and this test is now blind")
	}
	return rows
}

func TestDocsRegisterMatchesREQUIREMENTS(t *testing.T) {
	md := markdownRegister(t)
	js := jsonRegister(t)

	for id := range md {
		if _, ok := js[id]; !ok {
			t.Errorf("%s is in REQUIREMENTS.md and missing from the Docs tab, so the browser cannot see it", id)
		}
	}
	for id := range js {
		if _, ok := md[id]; !ok {
			t.Errorf("%s is in the Docs tab and missing from REQUIREMENTS.md, so nothing re-verifies it", id)
		}
	}

	columns := []string{"id", "requirement", "origin", "status", "evidence"}
	for id, want := range md {
		got, ok := js[id]
		if !ok {
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%s: the Docs tab has %d columns, REQUIREMENTS.md has %d", id, len(got), len(want))
			continue
		}
		for i := 1; i < len(want); i++ {
			if got[i] != want[i] {
				t.Errorf("%s: the %s column disagrees.\n  REQUIREMENTS.md: %s\n  Docs tab:        %s",
					id, columns[i], want[i], got[i])
			}
		}
	}
}
