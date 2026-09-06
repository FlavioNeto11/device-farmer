package jobspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// sha64 is a well-formed artifact hash: 64 lowercase hex characters, exactly
// what farm.artifacts.sha256's CHECK demands.
const sha64 = "ab7f00112233445566778899aabbccddeeff00112233445566778899aabbccdd"

const sha64b = "0011223344556677889900112233445566778899001122334455667788990011"

// ---------------------------------------------------------------------------
// The vocabulary agrees with the database
// ---------------------------------------------------------------------------

// TestKindTableMatchesMigration reads the INSERT that populates
// farm.step_kinds and requires this package's table to match it row for row,
// flag for flag, in order.
//
// This is the whole reason the Go table is allowed to exist. Without it the
// two drift silently: someone marks a kind non-idempotent in SQL because a
// resume repeated a side effect, the runner keeps re-running it because Go
// still says idempotent, and the bug survives the fix.
func TestKindTableMatchesMigration(t *testing.T) {
	t.Parallel()

	rows := parseStepKindsInsert(t, "../../migrations/00004_operate.sql")
	if len(rows) != len(kindTable) {
		t.Fatalf("migration inserts %d step kinds, this package declares %d", len(rows), len(kindTable))
	}
	for i, want := range rows {
		got := kindTable[i]
		if got != want {
			t.Errorf("kind %d: package has %+v, migration has %+v", i, got, want)
		}
	}

	// The constants must cover the table, so a kind cannot exist in the table
	// without a name a caller can write.
	consts := []Kind{
		KindPush, KindInstall, KindUninstall, KindShell, KindShellDetached,
		KindWaitFor, KindPull, KindAssert, KindReset, KindSleep,
	}
	if len(consts) != len(kindTable) {
		t.Fatalf("%d exported kind constants for %d table rows", len(consts), len(kindTable))
	}
	for i, k := range consts {
		if k != kindTable[i].Kind {
			t.Errorf("constant %d is %q, table row %d is %q", i, k, i, kindTable[i].Kind)
		}
		if !k.Valid() {
			t.Errorf("%q is in the table but Valid() says no", k)
		}
	}
	if Kind("teleport").Valid() {
		t.Error("Valid() accepted a kind that is not in farm.step_kinds")
	}
}

var stepKindRowRe = regexp.MustCompile(`^\('([a-z_]+)',.*(true|false),\s*(true|false)\)[,;]$`)

// parseStepKindsInsert extracts the seeded rows of farm.step_kinds from the
// migration. Rows may wrap across lines (one description is built with ||), so
// lines are accumulated until the row's closing paren.
func parseStepKindsInsert(t *testing.T, path string) []KindInfo {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	var header string
	for i, ln := range lines {
		if strings.Contains(ln, "INSERT INTO farm.step_kinds") {
			start = i + 1
			header = ln
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s no longer contains an INSERT INTO farm.step_kinds", path)
	}

	// The rows below are read by POSITION, so the column list is the thing that
	// gives those positions meaning. Swap idempotent and needs_artifact in the
	// migration and both this parse and kindTable would be wrong in the same
	// direction — the comparison would still pass, and every resume would then
	// re-run a step the database had marked unsafe to repeat. Pin the header.
	const wantHeader = "INSERT INTO farm.step_kinds (kind, description, idempotent, needs_artifact) VALUES"
	if got := strings.TrimSpace(strings.TrimRight(header, "\r")); got != wantHeader {
		t.Fatalf("the step_kinds INSERT column order changed, so the positional parse below is\n"+
			"no longer meaningful:\n got %s\nwant %s", got, wantHeader)
	}

	var (
		out []KindInfo
		buf string
	)
	for _, ln := range lines[start:] {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if ln == "" {
			continue
		}
		if buf != "" {
			buf += " " + ln
		} else {
			buf = ln
		}
		if !strings.HasSuffix(buf, "),") && !strings.HasSuffix(buf, ");") {
			continue
		}
		m := stepKindRowRe.FindStringSubmatch(buf)
		if m == nil {
			t.Fatalf("cannot parse step_kinds row: %s", buf)
		}
		out = append(out, KindInfo{
			Kind:          Kind(m[1]),
			Idempotent:    m[2] == "true",
			NeedsArtifact: m[3] == "true",
		})
		done := strings.HasSuffix(buf, ");")
		buf = ""
		if done {
			break
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no rows out of %s", path)
	}
	return out
}

// ---------------------------------------------------------------------------
// Round trips
// ---------------------------------------------------------------------------

// fullSpec exercises every one of the ten kinds in one document, so the
// round-trip tests cover the whole vocabulary rather than the easy half.
func fullSpec() Spec {
	return Spec{
		Version:           SpecVersion,
		DefaultTimeout:    Duration(5 * time.Minute),
		DefaultExpectExit: []int{0, 1},
		Steps: []Step{
			{ID: "push-apk", Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/app.apk", Mode: "0644"}},
			{ID: "install", Timeout: Duration(2 * time.Minute),
				Payload: Install{SHA256: sha64, Reinstall: true, Grant: true}},
			{ID: "drop-old", Payload: Uninstall{Package: "com.acme.old"}},
			{ID: "settle", Payload: Shell{Command: "am force-stop com.acme.app", ExpectExit: []int{0, 2}}},
			{ID: "soak", ContinueOnError: true, Payload: ShellDetached{
				Command:    "am instrument -w com.acme.test/androidx.test.runner.AndroidJUnitRunner",
				ResultPath: "/data/local/tmp/farm/soak.result",
				Handle:     "soak",
			}},
			{ID: "await-idle", Timeout: Duration(10 * time.Minute), Payload: WaitFor{
				Probe:    "dumpsys deviceidle get deep",
				Interval: Duration(3 * time.Second),
				Timeout:  Duration(9 * time.Minute),
			}},
			{ID: "collect", Payload: Pull{Path: "/data/local/tmp/farm/soak.result", Artifact: "soak.result"}},
			{ID: "check-sdk", Payload: Assert{Probe: "getprop ro.build.version.sdk", Operator: OpGE, Value: "30"}},
			{ID: "wipe", Payload: Reset{Tier: TierMedium}},
			{ID: "breathe", Payload: Sleep{Duration: 30 * Duration(time.Second)}},
		},
	}
}

// TestRoundTripIsByteIdentical pins the property a resume depends on: what a
// submit wrote is what a resume reads.
//
// Byte identity is claimed for Marshal -> Unmarshal -> Marshal only. Through
// jsonb the guarantee is value identity, because Postgres reorders object keys
// on the way in; that is why the decoded value is compared too.
func TestRoundTripIsByteIdentical(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec Spec
	}{
		{"every kind", fullSpec()},
		{"a wait on a detached handle", specOf(
			Step{ID: "soak/start", Payload: ShellDetached{
				Command: "sh /data/local/tmp/soak.sh", ResultPath: "/data/local/tmp/soak.result", Handle: "soak"}},
			Step{ID: "soak/await", Payload: WaitFor{
				Handle: "soak", Interval: Duration(30 * time.Second), Timeout: Duration(time.Hour)}},
		)},
		{"minimal", New(Step{ID: "only", Timeout: Duration(time.Minute), Payload: Shell{Command: "true"}})},
		{"no defaults", Spec{Version: SpecVersion, Steps: []Step{
			{ID: "s", Timeout: Duration(time.Second), Payload: Sleep{Duration: Duration(time.Second)}},
		}}},
		{"zero value", Spec{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			first, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back Spec
			if err := json.Unmarshal(first, &back); err != nil {
				t.Fatalf("unmarshal %s: %v", first, err)
			}
			if !reflect.DeepEqual(tc.spec, back) {
				t.Errorf("decoded value differs\n got %#v\nwant %#v", back, tc.spec)
			}
			second, err := json.Marshal(back)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(first) != string(second) {
				t.Errorf("not byte-identical\nfirst:  %s\nsecond: %s", first, second)
			}
		})
	}
}

// TestWireShapeIsPinned freezes the jsonb document. farm.jobs.spec rows
// written today are read by code shipped tomorrow, so a key rename is a
// migration, not a refactor, and it has to be seen.
func TestWireShapeIsPinned(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Version:           SpecVersion,
		DefaultTimeout:    Duration(5 * time.Minute),
		DefaultExpectExit: []int{0},
		Steps: []Step{
			{ID: "push-apk", Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/app.apk", Mode: "0644"}},
			{ID: "install", Timeout: Duration(2 * time.Minute),
				Payload: Install{SHA256: sha64, Reinstall: true, Grant: true}},
			{ID: "run", ContinueOnError: true, Payload: ShellDetached{
				Command: "sh /data/local/tmp/run.sh", ResultPath: "/data/local/tmp/run.out", Handle: "run"}},
		},
	}
	const want = `{"version":1,"default_timeout":"5m0s","default_expect_exit":[0],"steps":[` +
		`{"id":"push-apk","kind":"push","push":{"sha256":"` + sha64 + `","dest":"/data/local/tmp/app.apk","mode":"0644"}},` +
		`{"id":"install","kind":"install","timeout":"2m0s","install":{"sha256":"` + sha64 + `","reinstall":true,"grant":true}},` +
		`{"id":"run","kind":"shell_detached","continue_on_error":true,` +
		`"shell_detached":{"command":"sh /data/local/tmp/run.sh","result_path":"/data/local/tmp/run.out","handle":"run"}}]}`

	got, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("wire shape changed\n got %s\nwant %s", got, want)
	}
}

// TestSurvivesJSONBNormalisation is the other half of the round trip, and the
// half a Go-only test cannot reach.
//
// The fixture below is not hand-written: it is what Postgres 17 returned for
//
//	SELECT $j$<the document TestWireShapeIsPinned pins>$j$::jsonb::text;
//
// against the live schema. jsonb is a decomposed representation, so the keys
// come back in a completely different order from the one the encoder wrote —
// "version" and the defaults have moved to the end, and every object is
// reordered. A resume that depended on key order would already be broken here;
// decoding must produce exactly the value the submit held.
func TestSurvivesJSONBNormalisation(t *testing.T) {
	t.Parallel()

	const fromPostgres = `{"steps": [{"id": "push-apk", "kind": "push", "push": {"dest": ` +
		`"/data/local/tmp/app.apk", "mode": "0644", "sha256": "` + sha64 + `"}}, {"id": "install", ` +
		`"kind": "install", "install": {"grant": true, "sha256": "` + sha64 + `", "reinstall": true}, ` +
		`"timeout": "2m0s"}, {"id": "run", "kind": "shell_detached", "shell_detached": {"handle": "run", ` +
		`"command": "sh /data/local/tmp/run.sh", "result_path": "/data/local/tmp/run.out"}, ` +
		`"continue_on_error": true}], "version": 1, "default_timeout": "5m0s", "default_expect_exit": [0]}`

	got, err := Parse([]byte(fromPostgres))
	if err != nil {
		t.Fatalf("Parse of a jsonb-normalised spec: %v", err)
	}
	want := Spec{
		Version:           SpecVersion,
		DefaultTimeout:    Duration(5 * time.Minute),
		DefaultExpectExit: []int{0},
		Steps: []Step{
			{ID: "push-apk", Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/app.apk", Mode: "0644"}},
			{ID: "install", Timeout: Duration(2 * time.Minute),
				Payload: Install{SHA256: sha64, Reinstall: true, Grant: true}},
			{ID: "run", ContinueOnError: true, Payload: ShellDetached{
				Command: "sh /data/local/tmp/run.sh", ResultPath: "/data/local/tmp/run.out", Handle: "run"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a spec read back from jsonb differs\n got %#v\nwant %#v", got, want)
	}
}

func TestDurationJSON(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   Duration
		want string
	}{
		{0, `"0s"`},
		{Duration(90 * time.Second), `"1m30s"`},
		{Duration(2*time.Hour + 30*time.Minute), `"2h30m0s"`},
		{Duration(1500 * time.Millisecond), `"1.5s"`},
	} {
		got, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatalf("marshal %v: %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("marshal %d = %s, want %s", int64(tc.in), got, tc.want)
		}
		var back Duration
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", got, err)
		}
		if back != tc.in {
			t.Errorf("round trip %s = %v, want %v", got, back, tc.in)
		}
	}

	// A bare number is refused rather than assigned a unit, because whichever
	// unit were chosen, somebody would assume one of the other two.
	var d Duration
	if err := json.Unmarshal([]byte("30"), &d); err == nil {
		t.Error("a bare number was accepted as a duration")
	} else if !strings.Contains(err.Error(), "never a bare number") {
		t.Errorf("unhelpful error for a numeric duration: %v", err)
	}
	if err := json.Unmarshal([]byte(`"30 minutes"`), &d); err == nil {
		t.Error(`"30 minutes" was accepted as a duration`)
	}

	// null would otherwise decode as a no-op and surface as time.ParseDuration
	// complaining about "", which does not tell the author the key was at fault.
	err := json.Unmarshal([]byte("null"), &d)
	if err == nil {
		t.Fatal("null was accepted as a duration")
	}
	if !strings.Contains(err.Error(), "omit the key") {
		t.Errorf("the null message does not say what to do instead: %v", err)
	}
}

// TestUnmarshalRejects covers the documents a strict decoder must refuse. Each
// is a spec that would otherwise mean something other than what it says.
func TestUnmarshalRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			"kind disagrees with payload",
			`{"id":"a","kind":"push","install":{"sha256":"` + sha64 + `"}}`,
			`says kind "push" but carries a "install" payload`,
		},
		{
			"two payloads",
			`{"id":"a","kind":"push","push":{"sha256":"x","dest":"/tmp/a"},"sleep":{"duration":"1s"}}`,
			"carries 2 payloads",
		},
		{
			"no payload",
			`{"id":"a","kind":"push"}`,
			`carries no "push" payload`,
		},
		{
			"unknown kind",
			`{"id":"a","kind":"teleport","push":{"sha256":"x","dest":"/tmp/a"}}`,
			`unknown kind "teleport"`,
		},
		{
			"no kind",
			`{"id":"a","push":{"sha256":"x","dest":"/tmp/a"}}`,
			"has no kind",
		},
		{
			"unknown step field",
			`{"id":"a","kind":"sleep","sleep":{"duration":"1s"},"retries":3}`,
			`unknown field "retries"`,
		},
		{
			"unknown payload field",
			`{"id":"a","kind":"sleep","sleep":{"duration":"1s","jitter":"1s"}}`,
			`unknown field "jitter"`,
		},
		{
			"numeric duration",
			`{"id":"a","kind":"sleep","sleep":{"duration":30}}`,
			"never a bare number",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var s Step
			err := json.Unmarshal([]byte(tc.doc), &s)
			if err == nil {
				t.Fatalf("accepted %s", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// The same strictness at the spec level.
	var spec Spec
	if err := json.Unmarshal([]byte(`{"version":1,"steps":[],"parallel":true}`), &spec); err == nil {
		t.Error("spec accepted an unknown top-level field")
	}
}

// TestMarshalStepWithoutPayloadFails: a kind-less step has no valid encoding,
// and writing one to farm.jobs.spec would produce a row that fails to
// unmarshal on resume — a defect discovered at the worst possible moment.
func TestMarshalStepWithoutPayloadFails(t *testing.T) {
	t.Parallel()

	if _, err := json.Marshal(Step{ID: "orphan"}); err == nil {
		t.Fatal("a step with no payload marshalled successfully")
	}
	if got := (Step{}).Kind(); got != "" {
		t.Errorf("Kind() on an empty step = %q, want the empty string", got)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// bogusPayload has a kind that is not in farm.step_kinds. It cannot be built
// outside this package — Payload is closed — and exists only to prove the
// defensive branch in checkStep still fires if the vocabulary ever drifts.
type bogusPayload struct{}

func (bogusPayload) Kind() Kind { return "teleport" }
func (bogusPayload) payload()   {}

// driftedPush claims a kind whose step_kinds row says needs_artifact, while
// naming no artifact. Same purpose: prove the schema-driven check catches a
// model that stopped matching the database.
type driftedPush struct{}

func (driftedPush) Kind() Kind { return KindPush }
func (driftedPush) payload()   {}

func shellStep(id string) Step {
	return Step{ID: id, Timeout: Duration(time.Minute), Payload: Shell{Command: "true"}}
}

func specOf(steps ...Step) Spec {
	return Spec{Version: SpecVersion, DefaultTimeout: Duration(time.Minute), Steps: steps}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	manySteps := make([]Step, MaxSteps+1)
	for i := range manySteps {
		manySteps[i] = shellStep(fmt.Sprintf("s%d", i))
	}

	longSteps := make([]Step, 5)
	for i := range longSteps {
		longSteps[i] = Step{ID: fmt.Sprintf("long%d", i), Timeout: Duration(MaxStepTimeout),
			Payload: Shell{Command: "true"}}
	}

	cases := []struct {
		name  string
		spec  Spec
		want  []string // every problem path, in any order
		hints []string // substrings the combined message must contain
	}{
		{
			name: "a valid spec has no problems",
			spec: fullSpec(),
		},
		{
			name: "wrong version",
			spec: Spec{Version: 7, DefaultTimeout: Duration(time.Minute), Steps: []Step{shellStep("a")}},
			want: []string{"version"},
		},
		{
			name: "no steps",
			spec: Spec{Version: SpecVersion},
			want: []string{"steps"},
		},
		{
			name: "too many steps",
			spec: specOf(manySteps...),
			want: []string{"steps"},
		},
		{
			name:  "empty id",
			spec:  specOf(Step{Timeout: Duration(time.Minute), Payload: Shell{Command: "true"}}),
			want:  []string{"steps[0].id"},
			hints: []string{"a checkpoint records this id"},
		},
		{
			name: "id with a space",
			spec: specOf(Step{ID: "step one", Timeout: Duration(time.Minute), Payload: Shell{Command: "true"}}),
			want: []string{"steps[0].id"},
		},
		{
			name: "id too long",
			spec: specOf(Step{ID: strings.Repeat("x", MaxStepIDLen+1), Timeout: Duration(time.Minute),
				Payload: Shell{Command: "true"}}),
			want: []string{"steps[0].id"},
		},
		{
			name:  "duplicate ids",
			spec:  specOf(shellStep("dup"), shellStep("dup")),
			want:  []string{"steps[1].id"},
			hints: []string{`duplicate step id "dup"`},
		},
		{
			name: "no payload",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute)}),
			want: []string{"steps[0]"},
		},
		{
			name:  "kind outside the vocabulary",
			spec:  specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: bogusPayload{}}),
			want:  []string{"steps[0].kind"},
			hints: []string{`unknown kind "teleport"`},
		},
		{
			name:  "needs_artifact payload that names none",
			spec:  specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: driftedPush{}}),
			want:  []string{"steps[0]", "steps[0]"},
			hints: []string{"drifted from farm.step_kinds"},
		},
		{
			name:  "no timeout anywhere",
			spec:  Spec{Version: SpecVersion, Steps: []Step{{ID: "a", Payload: Shell{Command: "true"}}}},
			want:  []string{"steps[0].timeout"},
			hints: []string{"neither the step nor the spec's default_timeout"},
		},
		{
			name: "negative step timeout",
			spec: specOf(Step{ID: "a", Timeout: Duration(-time.Second), Payload: Shell{Command: "true"}}),
			want: []string{"steps[0].timeout"},
		},
		{
			name: "step timeout above the cap",
			spec: specOf(Step{ID: "a", Timeout: Duration(MaxStepTimeout + time.Second),
				Payload: Shell{Command: "true"}}),
			want: []string{"steps[0].timeout"},
		},
		{
			name: "default timeout above the cap reports once",
			spec: Spec{Version: SpecVersion, DefaultTimeout: Duration(MaxStepTimeout + time.Second),
				Steps: []Step{{ID: "a", Payload: Shell{Command: "true"}}, {ID: "b", Payload: Shell{Command: "true"}}}},
			want: []string{"default_timeout"},
		},
		{
			name:  "total is not sane",
			spec:  specOf(longSteps...),
			want:  []string{"steps"},
			hints: []string{"add up to 30h0m0s"},
		},
		{
			name: "bad default exit codes",
			spec: Spec{Version: SpecVersion, DefaultTimeout: Duration(time.Minute),
				DefaultExpectExit: []int{999}, Steps: []Step{shellStep("a")}},
			want: []string{"default_expect_exit[0]"},
		},

		// Payload rules, one kind at a time.
		{
			name: "push without a real sha256",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Push{SHA256: "AB", Dest: "/data/local/tmp/a"}}),
			want:  []string{"steps[0].push.sha256"},
			hints: []string{"64 lowercase hex"},
		},
		{
			name: "push with an uppercase sha256",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Push{SHA256: strings.ToUpper(sha64), Dest: "/data/local/tmp/a"}}),
			want: []string{"steps[0].push.sha256"},
		},
		{
			name: "push to a relative path",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Push{SHA256: sha64, Dest: "tmp/a"}}),
			want: []string{"steps[0].push.dest"},
		},
		{
			name: "push to a path that would need quoting",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/a b; rm -rf /"}}),
			want: []string{"steps[0].push.dest"},
		},
		{
			name: "push through a parent segment",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/../../system/x"}}),
			want: []string{"steps[0].push.dest"},
		},
		{
			name: "push with a nonsense mode",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/a", Mode: "rwxr-xr-x"}}),
			want: []string{"steps[0].push.mode"},
		},
		{
			name: "install without a real sha256",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Install{SHA256: "not-a-hash"}}),
			want: []string{"steps[0].install.sha256"},
		},
		{
			name: "uninstall a name that is not a package",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Uninstall{Package: "com.acme.app; reboot"}}),
			want: []string{"steps[0].uninstall.package"},
		},
		{
			name: "shell with no command",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Shell{Command: "   "}}),
			want: []string{"steps[0].shell.command"},
		},
		{
			name: "shell with impossible exit codes",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Shell{Command: "true", ExpectExit: []int{0, 300, 0}}}),
			want: []string{"steps[0].shell.expect_exit[1]", "steps[0].shell.expect_exit[2]"},
		},
		{
			name: "detached step missing everything",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: ShellDetached{}}),
			want: []string{
				"steps[0].shell_detached.command",
				"steps[0].shell_detached.result_path",
				"steps[0].shell_detached.handle",
			},
		},
		{
			name: "two detached steps sharing a handle and a result file",
			spec: specOf(
				Step{ID: "a", Timeout: Duration(time.Minute), Payload: ShellDetached{
					Command: "true", ResultPath: "/data/local/tmp/r", Handle: "h"}},
				Step{ID: "b", Timeout: Duration(time.Minute), Payload: ShellDetached{
					Command: "true", ResultPath: "/data/local/tmp/r", Handle: "h"}},
			),
			want: []string{
				"steps[1].shell_detached.result_path",
				"steps[1].shell_detached.handle",
			},
			hints: []string{"is one of them losing", "duplicate handle"},
		},
		{
			name: "wait_for with no clocks",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: WaitFor{Probe: "true"}}),
			want: []string{"steps[0].wait_for.interval", "steps[0].wait_for.timeout"},
		},
		{
			name: "wait_for polling slower than it waits",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: WaitFor{
				Probe: "true", Interval: Duration(time.Minute), Timeout: Duration(10 * time.Second)}}),
			want: []string{"steps[0].wait_for.interval"},
		},
		{
			name: "wait_for outliving its own step",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: WaitFor{
				Probe: "true", Interval: Duration(time.Second), Timeout: Duration(5 * time.Minute)}}),
			want:  []string{"steps[0].wait_for.timeout"},
			hints: []string{"cut the probe short"},
		},
		{
			// The whole point of the handle form: the wait judges a detached
			// command's published exit status instead of an operator's
			// `test -f`, which is true for 137 as readily as for 0.
			name: "wait_for on a detached handle declared above it",
			spec: specOf(
				Step{ID: "soak/start", Timeout: Duration(time.Minute), Payload: ShellDetached{
					Command: "sh /data/local/tmp/soak.sh", ResultPath: "/data/local/tmp/soak.result", Handle: "soak"}},
				Step{ID: "soak/await", Timeout: Duration(time.Minute), Payload: WaitFor{
					Handle: "soak", Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}},
			),
		},
		{
			name: "wait_for told to wait for two different things",
			spec: specOf(
				Step{ID: "soak/start", Timeout: Duration(time.Minute), Payload: ShellDetached{
					Command: "true", ResultPath: "/data/local/tmp/soak.result", Handle: "soak"}},
				Step{ID: "soak/await", Timeout: Duration(time.Minute), Payload: WaitFor{
					Probe: "true", Handle: "soak", Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}},
			),
			want:  []string{"steps[1].wait_for"},
			hints: []string{"Keep one"},
		},
		{
			name: "wait_for told to wait for nothing at all",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: WaitFor{
				Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}}),
			want:  []string{"steps[0].wait_for.probe"},
			hints: []string{"or set \"handle\" instead"},
		},
		{
			name: "wait_for naming a handle nothing declares",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: WaitFor{
				Handle: "soak", Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}}),
			want:  []string{"steps[0].wait_for.handle"},
			hints: []string{"no shell_detached step in this spec declares"},
		},
		{
			// Above the step that starts the work, the wait would find no
			// trace of it on the device and burn its entire timeout.
			name: "wait_for placed above the detached step it waits on",
			spec: specOf(
				Step{ID: "soak/await", Timeout: Duration(time.Minute), Payload: WaitFor{
					Handle: "soak", Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}},
				Step{ID: "soak/start", Timeout: Duration(time.Minute), Payload: ShellDetached{
					Command: "true", ResultPath: "/data/local/tmp/soak.result", Handle: "soak"}},
			),
			want:  []string{"steps[0].wait_for.handle"},
			hints: []string{"runs after this step"},
		},
		{
			name: "wait_for whose handle could not name a file",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: WaitFor{
				Handle: "soak; reboot", Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}}),
			want: []string{"steps[0].wait_for.handle"},
		},
		{
			name: "pull with a bad path and a bad name",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Pull{Path: "relative/file", Artifact: "../escape"}}),
			want: []string{"steps[0].pull.path", "steps[0].pull.artifact"},
		},
		{
			name: "assert with an unknown operator",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Assert{Probe: "getprop x", Operator: "approximately", Value: "1"}}),
			want: []string{"steps[0].assert.op"},
		},
		{
			name: "assert matching an uncompilable expression",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Assert{Probe: "getprop x", Operator: OpMatches, Value: "([unclosed"}}),
			want: []string{"steps[0].assert.value"},
		},
		{
			name: "assert comparing a number to a word",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Assert{Probe: "getprop x", Operator: OpGT, Value: "lots"}}),
			want: []string{"steps[0].assert.value"},
		},
		{
			name: "reset to a tier the schema forbids",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Reset{Tier: "nuclear"}}),
			want:  []string{"steps[0].reset.tier"},
			hints: []string{"none, soft, medium, hard"},
		},
		{
			name: "sleep for no time",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute), Payload: Sleep{}}),
			want: []string{"steps[0].sleep.duration"},
		},
		{
			name: "sleep longer than the step that contains it",
			spec: specOf(Step{ID: "a", Timeout: Duration(time.Minute),
				Payload: Sleep{Duration: Duration(2 * time.Minute)}}),
			want:  []string{"steps[0].sleep.duration"},
			hints: []string{"could never finish"},
		},

		// One artifact, one path — in both directions.
		{
			name: "one artifact pushed to two paths",
			spec: specOf(
				Step{ID: "a", Timeout: Duration(time.Minute),
					Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/one"}},
				Step{ID: "b", Timeout: Duration(time.Minute),
					Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/two"}},
			),
			want:  []string{"steps[1].push.dest"},
			hints: []string{"one artifact means one path"},
		},
		{
			name: "the same artifact at the same path twice is fine",
			spec: specOf(
				Step{ID: "a", Timeout: Duration(time.Minute),
					Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/one"}},
				Step{ID: "b", Timeout: Duration(time.Minute),
					Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/one"}},
			),
		},
		{
			name: "two artifacts at the same path is fine",
			spec: specOf(
				Step{ID: "a", Timeout: Duration(time.Minute),
					Payload: Push{SHA256: sha64, Dest: "/data/local/tmp/one"}},
				Step{ID: "b", Timeout: Duration(time.Minute),
					Payload: Push{SHA256: sha64b, Dest: "/data/local/tmp/one"}},
			),
		},
		{
			name: "one artifact name pulled from two paths",
			spec: specOf(
				Step{ID: "a", Timeout: Duration(time.Minute),
					Payload: Pull{Path: "/data/local/tmp/one", Artifact: "report"}},
				Step{ID: "b", Timeout: Duration(time.Minute),
					Payload: Pull{Path: "/data/local/tmp/two", Artifact: "report"}},
			),
			want: []string{"steps[1].pull.path"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Validate(tc.spec)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("valid spec rejected: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate returned %v (%T), want *ValidationError", err, err)
			}
			got := make([]string, 0, len(ve.Problems))
			for _, p := range ve.Problems {
				got = append(got, p.Path)
				if p.Message == "" {
					t.Errorf("problem at %s carries no message", p.Path)
				}
			}
			want := slices.Clone(tc.want)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("problem paths\n got %q\nwant %q\nfull: %v", got, want, err)
			}
			for _, hint := range tc.hints {
				if !strings.Contains(err.Error(), hint) {
					t.Errorf("message does not mention %q: %v", hint, err)
				}
			}
		})
	}
}

// TestValidateReportsEveryProblem is the rule that motivates ValidationError:
// a person fixing a spec should need one round trip, not ten.
func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Version: 0, // 1
		Steps: []Step{
			{Payload: Push{SHA256: "nope", Dest: "relative"}}, // 2 id, 3 timeout, 4 sha, 5 dest
			{ID: "b", Payload: Sleep{}},                       // 6 timeout, 7 duration
			{ID: "b", Payload: Uninstall{Package: "!"}},       // 8 duplicate id, 9 timeout, 10 package
		},
	}
	err := Validate(spec)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate returned %v (%T), want *ValidationError", err, err)
	}
	want := []string{
		"version",
		"steps[0].id", "steps[0].timeout", "steps[0].push.sha256", "steps[0].push.dest",
		"steps[1].timeout", "steps[1].sleep.duration",
		"steps[2].id", "steps[2].timeout", "steps[2].uninstall.package",
	}
	got := make([]string, 0, len(ve.Problems))
	for _, p := range ve.Problems {
		got = append(got, p.Path)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("problem paths\n got %q\nwant %q", got, want)
	}
	// Every problem is in the rendered message, or the caller has to ask twice.
	for _, p := range ve.Problems {
		if !strings.Contains(ve.Error(), p.Path) {
			t.Errorf("Error() omits %s", p.Path)
		}
	}
}

// Problems come back in step order, and a cross-step rule must not break that.
//
// A wait_for's handle can only be resolved against the whole step list, and
// the obvious way to write that — walk everything, then resolve — appends the
// handle problems after every other step's. An editor highlighting the list
// would then jump backwards, and Error() truncates at renderedProblems, so on
// a spec with many problems the deferred ones would be the first to be lost.
// collectHandles runs BEFORE the walk instead, so the rule is decided in place.
func TestProblemsAreInStepOrder(t *testing.T) {
	t.Parallel()

	spec := specOf(
		Step{ID: "wait", Timeout: Duration(time.Minute), Payload: WaitFor{
			Handle: "nothing-declares-this", Interval: Duration(time.Second), Timeout: Duration(30 * time.Second)}},
		Step{ID: "push", Timeout: Duration(time.Minute), Payload: Push{SHA256: "nope", Dest: "relative"}},
	)
	var ve *ValidationError
	if !errors.As(Validate(spec), &ve) {
		t.Fatal("Validate accepted a spec with a dangling handle and a broken push")
	}
	got := make([]string, 0, len(ve.Problems))
	for _, p := range ve.Problems {
		got = append(got, p.Path)
	}
	want := []string{"steps[0].wait_for.handle", "steps[1].push.sha256", "steps[1].push.dest"}
	if !slices.Equal(got, want) {
		t.Errorf("problem order\n got %q\nwant %q", got, want)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(fullSpec())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(got, fullSpec()) {
		t.Errorf("Parse round trip differs\n got %#v\nwant %#v", got, fullSpec())
	}

	// Well-formed but wrong comes back as a ValidationError, so an API handler
	// can render every problem instead of the first.
	var ve *ValidationError
	if _, err := Parse([]byte(`{"version":1,"steps":[]}`)); !errors.As(err, &ve) {
		t.Fatalf("Parse of an empty spec returned %v (%T), want *ValidationError", err, err)
	}
	// Malformed is a decode error, not a validation error.
	if _, err := Parse([]byte(`{"version":1,`)); err == nil {
		t.Fatal("Parse accepted truncated JSON")
	}
}

func TestSpecAccessors(t *testing.T) {
	t.Parallel()

	spec := fullSpec()

	i, st, ok := spec.StepByID("collect")
	if !ok || i != 6 || st.Kind() != KindPull {
		t.Errorf("StepByID(collect) = %d, %v, %v; want index 6 and a pull step", i, st.Kind(), ok)
	}
	if _, _, ok := spec.StepByID("nope"); ok {
		t.Error("StepByID found a step that does not exist")
	}

	// A wait_for carries only a handle; the paths its status probe reads come
	// from the detached payload, which is the only place they are written
	// down.
	if d, ok := spec.DetachedByHandle("soak"); !ok || d.ResultPath != "/data/local/tmp/farm/soak.result" {
		t.Errorf("DetachedByHandle(soak) = %+v, %v; want the soak step's result path", d, ok)
	}
	if _, ok := spec.DetachedByHandle("nope"); ok {
		t.Error("DetachedByHandle found a handle nothing declares")
	}
	if _, ok := spec.DetachedByHandle(""); ok {
		t.Error("DetachedByHandle matched the empty handle, which every non-detached step has")
	}

	// A step's own timeout wins; otherwise the spec default applies.
	if got := spec.StepTimeout(spec.Steps[1]); got != 2*time.Minute {
		t.Errorf("StepTimeout(install) = %s, want 2m", got)
	}
	if got := spec.StepTimeout(spec.Steps[0]); got != 5*time.Minute {
		t.Errorf("StepTimeout(push-apk) = %s, want the 5m default", got)
	}

	if got := spec.ExpectExit(Shell{ExpectExit: []int{7}}); !slices.Equal(got, []int{7}) {
		t.Errorf("ExpectExit = %v, want the step's own [7]", got)
	}
	if got := spec.ExpectExit(Shell{}); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("ExpectExit = %v, want the spec default [0 1]", got)
	}
	if got := (Spec{}).ExpectExit(Shell{}); !slices.Equal(got, []int{0}) {
		t.Errorf("ExpectExit with no defaults = %v, want [0]", got)
	}
}

// TestTotalTimeoutSaturates: the sum is compared against MaxTotalTimeout, so a
// sum that wrapped would read as a short spec and slip past the one guard that
// exists to reject it.
func TestTotalTimeoutSaturates(t *testing.T) {
	t.Parallel()

	huge := Duration(math.MaxInt64 - 1)
	spec := Spec{Version: SpecVersion, Steps: []Step{
		{ID: "a", Timeout: huge, Payload: Shell{Command: "true"}},
		{ID: "b", Timeout: huge, Payload: Shell{Command: "true"}},
	}}
	total := spec.TotalTimeout()
	if total < 0 {
		t.Fatalf("TotalTimeout wrapped to %s", total)
	}
	if total <= MaxTotalTimeout {
		t.Errorf("TotalTimeout = %s, which would pass the %s guard", total, MaxTotalTimeout)
	}
	if err := Validate(spec); err == nil {
		t.Error("a spec whose timeouts sum past the end of time was accepted")
	}

	// A negative step timeout must not subtract from the total and hide a spec
	// that is over budget.
	mixed := Spec{Version: SpecVersion, Steps: []Step{
		{ID: "a", Timeout: Duration(MaxStepTimeout), Payload: Shell{Command: "true"}},
		{ID: "b", Timeout: Duration(-100 * time.Hour), Payload: Shell{Command: "true"}},
	}}
	if got := mixed.TotalTimeout(); got != MaxStepTimeout {
		t.Errorf("TotalTimeout = %s, want %s; a negative timeout must not count against the sum",
			got, MaxStepTimeout)
	}
}

// TestValidateIsBounded: one submitted document must not turn into an unbounded
// number of problems to allocate, nor an unbounded error string to log.
func TestValidateIsBounded(t *testing.T) {
	t.Parallel()

	// Steps past the cap are not walked. Every one of these is invalid, so an
	// unbounded walk would report one problem per step.
	steps := make([]Step, MaxSteps+500)
	for i := range steps {
		steps[i] = Step{ID: fmt.Sprintf("s%d", i), Timeout: Duration(time.Minute), Payload: Sleep{}}
	}
	var ve *ValidationError
	if !errors.As(Validate(specOf(steps...)), &ve) {
		t.Fatal("an over-long spec of invalid steps was accepted")
	}
	// One per walked step, plus the handful of spec-level problems (the step
	// count and the timeout total). The 500 steps past the cap contribute none.
	if len(ve.Problems) > MaxSteps+5 {
		t.Errorf("%d problems for %d steps; the walk is not capped at %d",
			len(ve.Problems), len(steps), MaxSteps)
	}

	// The rendered message is capped even though Problems is not.
	msg := ve.Error()
	if strings.Count(msg, "steps[") > renderedProblems+1 {
		t.Errorf("Error() spelled out more than %d problems in a %d-character string",
			renderedProblems, len(msg))
	}
	if !strings.Contains(msg, "and ") || !strings.Contains(msg, "more") {
		t.Errorf("Error() truncated silently, without saying how many it dropped: %.200s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d problems", len(ve.Problems))) {
		t.Errorf("Error() does not report the true problem count: %.200s", msg)
	}

	// An expect_exit list longer than the number of exit codes that exist is
	// one problem, not one per entry.
	codes := make([]int, 10_000)
	for i := range codes {
		codes[i] = i % 300
	}
	spec := Spec{Version: SpecVersion, DefaultTimeout: Duration(time.Minute),
		DefaultExpectExit: codes, Steps: []Step{shellStep("a")}}
	if !errors.As(Validate(spec), &ve) {
		t.Fatal("a 10000-entry expect_exit list was accepted")
	}
	if len(ve.Problems) != 1 {
		t.Errorf("%d problems for one over-long expect_exit list, want 1", len(ve.Problems))
	}
}

// TestExpectExitIsACopy: a runner that sorted the returned slice in place would
// otherwise be editing the spec's own default, silently changing what counts as
// success for every later step.
func TestExpectExitIsACopy(t *testing.T) {
	t.Parallel()

	spec := Spec{Version: SpecVersion, DefaultExpectExit: []int{0, 1}}
	got := spec.ExpectExit(Shell{})
	got[0] = 99
	if spec.DefaultExpectExit[0] != 0 {
		t.Errorf("mutating the result changed the spec default to %v", spec.DefaultExpectExit)
	}

	sh := Shell{ExpectExit: []int{7}}
	own := spec.ExpectExit(sh)
	own[0] = 99
	if sh.ExpectExit[0] != 7 {
		t.Errorf("mutating the result changed the step's own list to %v", sh.ExpectExit)
	}
}

// TestDurationErrorIsBounded: a duration field holding a large document must
// not put that whole document into an error message bound for a log line.
func TestDurationErrorIsBounded(t *testing.T) {
	t.Parallel()

	blob := `{"x":"` + strings.Repeat("y", 4000) + `"}`
	var d Duration
	err := d.UnmarshalJSON([]byte(blob))
	if err == nil {
		t.Fatal("an object was accepted as a duration")
	}
	if len(err.Error()) > 400 {
		t.Errorf("error message is %d characters long", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("the message was cut without saying so: %.200s", err)
	}
}

// ---------------------------------------------------------------------------
// Reset tiers
// ---------------------------------------------------------------------------

func TestResetSteps(t *testing.T) {
	t.Parallel()

	pkgs := []string{"com.acme.app", "com.acme.helper"}

	cases := []struct {
		tier ResetTier
		want []string
	}{
		{TierNone, nil},
		{TierSoft, []string{
			"reset/clear/com.acme.app",
			"reset/clear/com.acme.helper",
		}},
		{TierMedium, []string{
			"reset/clear/com.acme.app",
			"reset/clear/com.acme.helper",
			"reset/uninstall-unknown",
			"reset/device-state",
		}},
		{TierHard, []string{
			"reset/clear/com.acme.app",
			"reset/clear/com.acme.helper",
			"reset/uninstall-unknown",
			"reset/device-state",
			"reset/reboot",
			"reset/reboot-settle",
			"reset/boot-completed",
		}},
	}

	for _, tc := range cases {
		t.Run(string(tc.tier), func(t *testing.T) {
			t.Parallel()

			steps, err := ResetSteps(tc.tier, pkgs)
			if err != nil {
				t.Fatalf("ResetSteps(%s): %v", tc.tier, err)
			}
			got := make([]string, 0, len(steps))
			for _, s := range steps {
				got = append(got, s.ID)
			}
			// Order is the contract: soft is a prefix of medium is a prefix
			// of hard, and the reboot comes after the state is reapplied.
			if !slices.Equal(got, tc.want) {
				t.Fatalf("steps\n got %q\nwant %q", got, tc.want)
			}
			if len(steps) == 0 {
				return
			}
			// The expansion must be a spec the rest of the package accepts.
			if err := Validate(New(steps...)); err != nil {
				t.Errorf("the %s expansion does not validate: %v", tc.tier, err)
			}
		})
	}
}

func TestResetHardWaitsForBoot(t *testing.T) {
	t.Parallel()

	steps, err := ResetSteps(TierHard, nil)
	if err != nil {
		t.Fatalf("ResetSteps: %v", err)
	}
	byID := map[string]Step{}
	order := map[string]int{}
	for i, s := range steps {
		byID[s.ID] = s
		order[s.ID] = i
	}

	reboot, ok := byID["reset/reboot"]
	if !ok {
		t.Fatal("hard reset has no reset/reboot step")
	}
	if !reboot.ContinueOnError {
		t.Error("the reboot step must not be able to fail the job: " +
			"it severs the socket that would report its own exit code")
	}

	// The settle pause is what makes the boot probe's answer mean anything:
	// without it the probe reads sys.boot_completed off the system that has not
	// finished shutting down, and the job starts on a device about to vanish.
	settle, ok := byID["reset/reboot-settle"]
	if !ok {
		t.Fatal("hard reset issues a reboot and then immediately trusts the device's answers; " +
			"there is no reset/reboot-settle pause between them")
	}
	if order["reset/reboot"] >= order["reset/reboot-settle"] ||
		order["reset/reboot-settle"] >= order["reset/boot-completed"] {
		t.Errorf("the settle pause is not between the reboot and the boot probe: %v", order)
	}
	sleep, ok := settle.Payload.(Sleep)
	if !ok {
		t.Fatalf("reset/reboot-settle is a %s step, want sleep", settle.Kind())
	}
	if sleep.Duration.Std() < 10*time.Second {
		t.Errorf("a %s pause is too short to outlast an adbd teardown", sleep.Duration)
	}

	last := steps[len(steps)-1]
	if last.ID != "reset/boot-completed" {
		t.Fatalf("hard reset ends with %s, want reset/boot-completed", last.ID)
	}
	wait, ok := last.Payload.(WaitFor)
	if !ok {
		t.Fatalf("hard reset ends with a %s step, want wait_for", last.Kind())
	}
	if !strings.Contains(wait.Probe, "sys.boot_completed") {
		t.Errorf("boot probe %q does not read sys.boot_completed", wait.Probe)
	}
	if wait.Timeout.Std() > last.Timeout.Std() {
		t.Errorf("the probe waits %s inside a %s step, so the step would cut it short",
			wait.Timeout, last.Timeout)
	}
}

// TestResetClearSkipsAbsentPackages pins the fix for the reset that fails on
// every brand-new device: pm clear prints "Failed" for a package that is not
// installed, and a fresh phone has none of the profile's packages.
func TestResetClearSkipsAbsentPackages(t *testing.T) {
	t.Parallel()

	steps, err := ResetSteps(TierSoft, []string{"com.acme.app"})
	if err != nil {
		t.Fatalf("ResetSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("soft reset of one package produced %d steps", len(steps))
	}
	cmd := steps[0].Payload.(Shell).Command
	if !strings.Contains(cmd, "|| exit 0") {
		t.Errorf("clear command has no path that succeeds when the package is absent: %s", cmd)
	}
	// -x anchors the whole line: pm list packages filters by substring, so an
	// unanchored match would clear com.acme.app because com.acme.app2 exists.
	if !strings.Contains(cmd, `grep -qx "package:com.acme.app"`) {
		t.Errorf("the installed-check is not anchored to the exact package name: %s", cmd)
	}
	if !strings.Contains(cmd, "pm clear com.acme.app | grep -q Success") {
		t.Errorf("the verdict is not taken from pm clear's output: %s", cmd)
	}

	// A soft reset with nothing to clear is nothing to do, spelled the same way
	// tier "none" spells it.
	empty, err := ResetSteps(TierSoft, nil)
	if err != nil {
		t.Fatalf("ResetSteps(soft, nil): %v", err)
	}
	if empty != nil {
		t.Errorf("soft reset of an empty profile returned %v, want nil", empty)
	}
}

// TestResetStepIDLength: a package name long enough to overflow a step id is
// rejected where the message can name it, not later as a mystery problem at
// steps[n].id.
func TestResetStepIDLength(t *testing.T) {
	t.Parallel()

	long := "com." + strings.Repeat("x", MaxStepIDLen)
	if !packageRe.MatchString(long) {
		t.Fatalf("test package %q is not a valid package name to begin with", long)
	}
	_, err := ResetSteps(TierSoft, []string{long})
	if err == nil {
		t.Fatal("ResetSteps accepted a package whose step id would exceed MaxStepIDLen")
	}
	if !strings.Contains(err.Error(), "step id") {
		t.Errorf("error does not say what is wrong with it: %v", err)
	}

	// farm.profiles.packages has no uniqueness constraint, so a repeated name is
	// a real row. It must be named here, not surface later as a duplicate step
	// id in a spec the submitter did not write.
	dup, err := ResetSteps(TierSoft, []string{"com.acme.app", "com.acme.app"})
	if err == nil {
		t.Fatalf("ResetSteps accepted a duplicated package and produced %d steps with colliding ids", len(dup))
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error does not name the duplication: %v", err)
	}
}

// TestResetUninstallFailsWhenItCannotEnumerate: a package manager that is not
// answering must not produce a reset that reports success having removed
// nothing, leaving one tenant's app installed for the next one.
func TestResetUninstallFailsWhenItCannotEnumerate(t *testing.T) {
	t.Parallel()

	cmd := uninstallUnknownCommand([]string{"com.acme.app"})
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("the command has no failing exit for an enumeration that did not work: %s", cmd)
	}
	if strings.Contains(cmd, "for p in $(pm list packages") {
		t.Errorf("the enumeration is inlined into the for, so its failure becomes an empty loop: %s", cmd)
	}
}

func TestResetMediumUninstallsOnlyForeignPackages(t *testing.T) {
	t.Parallel()

	steps, err := ResetSteps(TierMedium, []string{"com.acme.app", "com.acme.helper"})
	if err != nil {
		t.Fatalf("ResetSteps: %v", err)
	}
	var cmd string
	for _, s := range steps {
		if s.ID == "reset/uninstall-unknown" {
			cmd = s.Payload.(Shell).Command
		}
	}
	if cmd == "" {
		t.Fatal("no reset/uninstall-unknown step")
	}
	// -3 is what keeps a reset off the system image.
	if !strings.Contains(cmd, "pm list packages -3") {
		t.Errorf("command does not enumerate third-party packages only: %s", cmd)
	}
	for _, pkg := range []string{"com.acme.app", "com.acme.helper"} {
		if !strings.Contains(cmd, " "+pkg+" ") {
			t.Errorf("keep list does not protect %s: %s", pkg, cmd)
		}
	}
	// The device decides what is installed; the spec only says what to keep.
	if strings.Contains(cmd, "com.acme.other") {
		t.Errorf("command names a package the farm should not know about: %s", cmd)
	}

	// The state reapply comes last, after the uninstalls, and runs the script
	// the runner materialises from farm.device_state.
	last := steps[len(steps)-1]
	if last.ID != "reset/device-state" {
		t.Fatalf("medium ends with %s, want reset/device-state", last.ID)
	}
	if got := last.Payload.(Shell).Command; !strings.Contains(got, DeviceStateScript) {
		t.Errorf("device-state step runs %q, which is not %s", got, DeviceStateScript)
	}
}

// TestResetStepsRejectsUnsafePackages: these names are interpolated into a
// shell command that runs on a device somebody else is holding. A name that
// cannot be validated is an error, never a quoted string.
func TestResetStepsRejectsUnsafePackages(t *testing.T) {
	t.Parallel()

	for _, pkg := range []string{
		"com.acme.app; rm -rf /data",
		"com.acme.app && reboot",
		"$(id)",
		"com acme",
		"",
		"../etc/passwd",
	} {
		if _, err := ResetSteps(TierSoft, []string{pkg}); err == nil {
			t.Errorf("ResetSteps accepted package %q", pkg)
		}
	}

	if _, err := ResetSteps("nuclear", nil); err == nil {
		t.Error("ResetSteps accepted a tier farm.jobs.reset_tier forbids")
	}

	// An empty profile is legitimate: it means every third-party package is
	// foreign, which is exactly what a medium reset of a shared device wants.
	steps, err := ResetSteps(TierMedium, nil)
	if err != nil {
		t.Fatalf("ResetSteps with no packages: %v", err)
	}
	if err := Validate(New(steps...)); err != nil {
		t.Errorf("the empty-profile expansion does not validate: %v", err)
	}
}

// TestResetTierVocabulary pins the tiers against the CHECK on
// farm.jobs.reset_tier, which is also the reset payload's vocabulary.
func TestResetTierVocabulary(t *testing.T) {
	t.Parallel()

	for _, tier := range []ResetTier{TierNone, TierSoft, TierMedium, TierHard} {
		if !tier.Valid() {
			t.Errorf("%q is in the CHECK list but Valid() says no", tier)
		}
	}
	for _, tier := range []ResetTier{"", "NONE", "reboot", "nuclear"} {
		if tier.Valid() {
			t.Errorf("Valid() accepted %q, which the CHECK list forbids", tier)
		}
	}
}
