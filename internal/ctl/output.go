package ctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Output format
// ---------------------------------------------------------------------------

// Format is how a command renders its result.
type Format string

const (
	// FormatTable is the human rendering: aligned columns, group headers, and
	// prose under the table when a number needs explaining.
	FormatTable Format = "table"
	// FormatJSON is the scripting rendering. Where a command is one API call,
	// it emits the API's own response body verbatim (re-indented, nothing
	// added and nothing removed), so a script that parses ctl output is
	// parsing the API's schema rather than a second one this package invented
	// and would have to keep in step.
	FormatJSON Format = "json"
)

// ParseFormat maps the -o value onto a Format.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "table", "text":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	}
	return "", fmt.Errorf("%w: -o takes table or json, not %q", ErrUsage, s)
}

// unslotted is what a device with no rack position prints in the rack column.
//
// The rack slot is the primary human identifier in every listing here, because
// an operator acts on a device by walking to it. An empty cell in that column
// reads as "this listing failed to fetch the value", and somebody who cannot
// tell that apart from "this device is not in a rack" walks a data hall
// looking for hardware that is sitting on a bench. So the absence is spelled
// out, in the same column, at the same width.
const unslotted = "(unslotted)"

// unallocated marks a row whose subject holds no device at all — a queued job,
// for instance. It is deliberately a different word from unslotted: "nothing is
// allocated to this" and "this is allocated to a device nobody has racked" send
// an operator to two different places.
const unallocated = "(unallocated)"

// unknownSlot marks a row whose rack position could not be resolved. It is
// never printed as unslotted, because reporting a lookup failure as a fact
// about the hardware is how a listing lies.
const unknownSlot = "(unknown)"

func rackSlotOf(p *string) string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return unslotted
	}
	return *p
}

// ---------------------------------------------------------------------------
// Table
// ---------------------------------------------------------------------------

// defaultCellWidth is the widest a single cell may print before it is clipped.
//
// A cell is never wrapped. Wrapping a long command or an ADB error across two
// lines destroys the column grid that makes a listing scannable, and the row
// that gets wrapped is invariably the interesting one. The full value is one
// `-o json` away.
const defaultCellWidth = 60

type tableRow struct {
	cells   []string
	section string
	kind    rowKind
}

type rowKind int

const (
	rowData rowKind = iota
	rowSection
	rowBlank
)

// Table renders aligned columns. Column widths are measured across every data
// row before anything is written, so the grid is decided once and no row can
// shift it.
type Table struct {
	headers []string
	rows    []tableRow
	maxCell int
}

// NewTable starts a table with the given column headings.
func NewTable(headers ...string) *Table {
	return &Table{headers: headers, maxCell: defaultCellWidth}
}

// MaxCell overrides the per-cell clip width.
func (t *Table) MaxCell(n int) *Table {
	if n > 0 {
		t.maxCell = n
	}
	return t
}

// Row appends a data row. Missing trailing cells print empty.
func (t *Table) Row(cells ...string) {
	t.rows = append(t.rows, tableRow{cells: cells, kind: rowData})
}

// Section appends a full-width heading between rows — a host, a hub, a bulk
// run. It takes no part in column measurement, so grouping a listing never
// widens its columns.
func (t *Table) Section(format string, args ...any) {
	if len(t.rows) > 0 {
		t.rows = append(t.rows, tableRow{kind: rowBlank})
	}
	t.rows = append(t.rows, tableRow{section: fmt.Sprintf(format, args...), kind: rowSection})
}

// Len reports how many data rows the table holds.
func (t *Table) Len() int {
	n := 0
	for _, r := range t.rows {
		if r.kind == rowData {
			n++
		}
	}
	return n
}

// Render writes the table. Cells are clipped in place first so that the width
// used for measuring is the width that is printed.
func (t *Table) Render(w io.Writer) error {
	cols := len(t.headers)
	widths := make([]int, cols)
	for i, h := range t.headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for ri := range t.rows {
		r := &t.rows[ri]
		if r.kind != rowData {
			continue
		}
		for i := 0; i < cols && i < len(r.cells); i++ {
			r.cells[i] = clip(r.cells[i], t.maxCell)
			if n := utf8.RuneCountInString(r.cells[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var b strings.Builder
	writeCells(&b, t.headers, widths)
	rule := make([]string, cols)
	for i := range rule {
		rule[i] = strings.Repeat("-", widths[i])
	}
	writeCells(&b, rule, widths)

	for _, r := range t.rows {
		switch r.kind {
		case rowBlank:
			b.WriteByte('\n')
		case rowSection:
			b.WriteString(r.section)
			b.WriteByte('\n')
		default:
			writeCells(&b, r.cells, widths)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeCells pads every column but the last. The last is written bare so no
// line carries trailing whitespace into a diff, a paste or a grep.
func writeCells(b *strings.Builder, cells []string, widths []int) {
	last := len(widths) - 1
	for i, width := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		if i == last {
			b.WriteString(strings.TrimRight(cell, " "))
			break
		}
		b.WriteString(cell)
		pad := width - utf8.RuneCountInString(cell) + 2
		if pad < 1 {
			pad = 1
		}
		b.WriteString(strings.Repeat(" ", pad))
	}
	b.WriteByte('\n')
}

// clip flattens a cell onto one line and bounds its width.
//
// Flattening is what actually enforces "never wrap mid-cell": an ADB error or
// a shell command carrying a newline would otherwise break the grid in a way
// no amount of padding can repair.
func clip(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\v', '\f':
			return ' '
		}
		return r
	}, s)
	if max <= 1 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// ---------------------------------------------------------------------------
// Fields — the detail rendering
// ---------------------------------------------------------------------------

// Fields is an aligned key/value block, for the single-subject views where a
// table of one row would be harder to read than a list.
type Fields struct {
	pairs [][2]string
}

// Add appends a pair.
func (f *Fields) Add(key, value string) {
	f.pairs = append(f.pairs, [2]string{key, value})
}

// Addf appends a formatted pair.
func (f *Fields) Addf(key, format string, args ...any) {
	f.Add(key, fmt.Sprintf(format, args...))
}

// AddOpt appends a pair only when the value is present, so an absent optional
// reads as absent rather than as an empty string that might mean either.
func (f *Fields) AddOpt(key string, value *string) {
	if value != nil && strings.TrimSpace(*value) != "" {
		f.Add(key, *value)
	}
}

// Gap starts a new block within the same alignment.
func (f *Fields) Gap() { f.pairs = append(f.pairs, [2]string{"", ""}) }

// Len reports how many pairs were added, blank separators included.
func (f *Fields) Len() int { return len(f.pairs) }

// Render writes the block, keys padded to the widest.
func (f *Fields) Render(w io.Writer) error {
	width := 0
	for _, p := range f.pairs {
		if n := utf8.RuneCountInString(p[0]); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, p := range f.pairs {
		if p[0] == "" && p[1] == "" {
			b.WriteByte('\n')
			continue
		}
		b.WriteString(p[0])
		b.WriteString(":")
		pad := width - utf8.RuneCountInString(p[0]) + 1
		if pad < 1 {
			pad = 1
		}
		b.WriteString(strings.Repeat(" ", pad))
		// Values are not clipped here. A detail view is the place an operator
		// goes when they want the whole value, and a truncated endpoint or
		// devpath there would be worse than a long line.
		b.WriteString(strings.ReplaceAll(p[1], "\n", " "))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// ---------------------------------------------------------------------------
// Printer
// ---------------------------------------------------------------------------

// Printer routes a command's result to stdout in the requested format.
//
// Human commentary — the preflight of a destructive command, a confirmation
// prompt, a warning that a listing was truncated — goes to stderr, never here.
// That split is what lets `ctl fleet -o json | jq` work on a terminal where the
// operator can still read the warning.
type Printer struct {
	out    io.Writer
	format Format
}

// NewPrinter builds a printer over out.
func NewPrinter(out io.Writer, format Format) *Printer {
	return &Printer{out: out, format: format}
}

// JSON writes a value this package assembled.
func (p *Printer) JSON(v any) error {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	buf = append(buf, '\n')
	_, err = p.out.Write(buf)
	return err
}

// RawJSON writes an API response body through unchanged but re-indented.
func (p *Printer) RawJSON(b []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		// The body is not JSON. Passing it through is still the most useful
		// thing to do: the operator sees what the server actually said.
		_, werr := p.out.Write(append(bytes.TrimRight(b, "\n"), '\n'))
		return werr
	}
	buf.WriteByte('\n')
	_, err := p.out.Write(buf.Bytes())
	return err
}

// Table renders a table, in table format only.
func (p *Printer) Table(t *Table) error {
	if p.format != FormatTable {
		return nil
	}
	return t.Render(p.out)
}

// Fields renders a key/value block, in table format only.
func (p *Printer) Fields(f *Fields) error {
	if p.format != FormatTable {
		return nil
	}
	return f.Render(p.out)
}

// Text writes a line of prose to stdout, in table format only. It is for
// sentences that belong with the result — a legend, a total — not for
// warnings, which belong on stderr where a pipeline cannot swallow them.
func (p *Printer) Text(format string, args ...any) {
	if p.format != FormatTable {
		return
	}
	fmt.Fprintf(p.out, format+"\n", args...)
}

// Blank writes a separating newline, in table format only.
func (p *Printer) Blank() {
	if p.format != FormatTable {
		return
	}
	fmt.Fprintln(p.out)
}

// ---------------------------------------------------------------------------
// Cell formatting
// ---------------------------------------------------------------------------

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// dash renders an absent value as an em dash rather than as nothing, so a
// missing cell is visibly missing.
func dash(p *string) string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return "—"
	}
	return *p
}

func dashInt(p *int) string {
	if p == nil {
		return "—"
	}
	return strconv.Itoa(*p)
}

func dashInt64(p *int64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatInt(*p, 10)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// shortID abbreviates a uuid for a column where it is context rather than the
// thing being identified. Every listing that shortens an id says so beneath
// the table, and -o json always carries ids in full.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// androidOf renders the OS column: release and API level are read together far
// more often than apart.
func androidOf(release *string, sdk *int) string {
	switch {
	case release == nil && sdk == nil:
		return "—"
	case sdk == nil:
		return *release
	case release == nil:
		return "api " + strconv.Itoa(*sdk)
	}
	return *release + " (api " + strconv.Itoa(*sdk) + ")"
}

func batteryOf(pct *int, tempDeci *int) string {
	if pct == nil {
		return "—"
	}
	out := strconv.Itoa(*pct) + "%"
	if tempDeci != nil {
		out += fmt.Sprintf(" %.1fC", float64(*tempDeci)/10)
	}
	return out
}

// duration renders a count of seconds compactly. Negative values keep their
// sign, because a lease whose expiry is 40 seconds in the past is a fact an
// operator needs, not a number to clamp to zero.
func duration(seconds int64) string {
	neg := seconds < 0
	if neg {
		seconds = -seconds
	}
	var out string
	switch {
	case seconds < 60:
		out = strconv.FormatInt(seconds, 10) + "s"
	case seconds < 3600:
		out = fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	case seconds < 86400:
		out = fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
	default:
		out = fmt.Sprintf("%dd%02dh", seconds/86400, (seconds%86400)/3600)
	}
	if neg {
		return "-" + out
	}
	return out
}

// millis renders a measured elapsed time from the server.
//
// A shell command on a phone usually finishes in a few hundred milliseconds,
// and truncating that to whole seconds reports every one of them as "0s" —
// which erases the only number in the line that distinguishes a fast device
// from one that took twenty seconds to answer.
func millis(ms int64) string {
	if ms > -1000 && ms < 1000 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	return duration(ms / 1000)
}

// ago renders how long ago a server timestamp was, for display only.
//
// It subtracts the local clock from a server instant, which makes it an
// estimate and nothing more. Every value that decides anything — a lease
// expiry, a reclaim deadline — is computed by Postgres against its own now()
// and arrives already counted down; this function is never used for one of
// those.
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return duration(int64(time.Since(*t).Seconds())) + " ago"
}

func stamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// countsLine renders a map of counters in a stable order, since a Go map's
// iteration order would otherwise reshuffle the summary on every invocation
// and make two runs impossible to diff.
func countsLine(counts map[string]int, order ...string) string {
	parts := make([]string, 0, len(counts))
	seen := make(map[string]bool, len(counts))
	for _, k := range order {
		if n, ok := counts[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(counts))
	for k := range counts {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// sortStrings is an insertion sort. The slices here are counter keys — a
// handful of state names — and pulling in sort for them is not worth the
// import.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
