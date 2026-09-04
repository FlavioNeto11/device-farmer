// Package jobspec is the user-facing contract of the farm: what a job asks a
// device to do, written down once and read back unchanged.
//
// # The vocabulary is closed, and the database owns it
//
// farm.jobs.spec is a jsonb column. Anything at all fits in a jsonb column,
// which is exactly why the step vocabulary is not defined here: it is defined
// by the rows of farm.step_kinds (migrations/00004_operate.sql) and this
// package restates those rows, in their order, with the two behavioural flags
// each row carries. A test in this package reads the migration and fails if the
// two ever drift. The alternative — a Go model that "remembers" the schema — is
// how a spec written today stops meaning the same thing when it resumes
// tomorrow.
//
// Ten kinds, no eleventh, and no escape hatch that runs arbitrary structured
// data. A step the runner does not understand is a step it must refuse, and a
// closed vocabulary is what lets it tell the difference between "I cannot do
// this" and "I did something else".
//
// # A step's kind cannot disagree with its payload
//
// [Step] deliberately has no Kind field. It has a [Payload], and the kind is
// derived from it by [Step.Kind]. A struct with both a Kind field and a payload
// admits Kind: "push" carrying an Install payload, and every consumer then has
// to decide which of the two to believe. Here that state does not exist: the
// payload IS the kind. The wire form still carries an explicit "kind" key —
// jsonb is read by psql and by humans, not only by this package — and
// [Step.UnmarshalJSON] refuses any document where the key and the payload
// disagree, so the invariant is re-established at the only place it can be
// violated.
//
// [Payload] is closed the same way: its second method is unexported, so the ten
// types in this file are the only implementations that can ever exist.
//
// # Round-tripping
//
// A submit writes a spec; a resume, possibly hours and one process later, reads
// it back and must get the same instructions. Two different guarantees hold:
//
//   - In Go, Marshal -> Unmarshal -> Marshal is byte-identical. Every field has
//     a fixed position in a struct, nothing is encoded through a map, and
//     durations have one canonical spelling. This is tested.
//   - Through jsonb it is value-identical but NOT byte-identical, because jsonb
//     is a decomposed representation: Postgres reorders object keys and drops
//     duplicates on the way in. Nothing here depends on key order, and no key
//     appears twice, so what comes back out unmarshals to an equal [Spec].
//     Never checksum the raw jsonb text and expect it to match what was sent.
//
// # Durations
//
// Every duration is a string in Go's own syntax ("90s", "5m", "2h30m"), never a
// bare number. A number would have to mean seconds, or milliseconds, or
// nanoseconds, and whichever was chosen someone would eventually assume one of
// the other two — in a field that decides how long a phone is held. See
// [Duration].
//
// # What is NOT in a spec
//
// There is no retry, backoff, reconnect or timeout-of-the-transport anywhere in
// this vocabulary. A dropped ADB socket is not a step outcome and has no
// spelling here; it is retried inside the lease the job still holds, by the
// runner, and it can no more end a step than it can end a lease. The one place
// the wire's unreliability is visible at all is [ShellDetached], which exists
// precisely so that a long command's result lives in a file on the device
// rather than in a socket the control plane happens to be holding.
package jobspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SpecVersion is the only version of the document shape this package writes,
// and the only one [Validate] accepts.
//
// It is stored explicitly rather than inferred so that a future shape change
// can be recognised instead of half-parsed: a runner reading a version it does
// not know must refuse the job, not guess at the parts it recognises.
const SpecVersion = 1

// Bounds. These are guards against a spec that could never succeed, not policy:
// the real deadline a user cares about is farm.jobs.max_runtime, which is the
// only user-supplied clock allowed to end a lease.
const (
	// MaxSteps caps how many steps one job may carry. A jsonb document with
	// more than this is a generated loop that wanted a script.
	MaxSteps = 1000

	// MaxStepIDLen bounds a step id. Ids land in farm.job_steps.step_id and in
	// checkpoints, and are read by humans at 3am.
	MaxStepIDLen = 128

	// MaxArtifactNameLen bounds the name a pull stores its bytes under. Both
	// this and MaxStepIDLen are this package's policy, not the schema's:
	// farm.job_steps.step_id and farm.artifacts.name are unconstrained text.
	MaxArtifactNameLen = 128

	// MaxStepTimeout is the longest any single step may be given. A soak test
	// that genuinely runs longer belongs in a shell_detached step, whose whole
	// point is that the device owns the result and the step returns promptly.
	MaxStepTimeout = 6 * time.Hour

	// MaxTotalTimeout bounds the sum of every step's effective timeout. A spec
	// whose steps could not all finish inside a day is a spec that will be
	// killed by its own max_runtime with the work half done.
	MaxTotalTimeout = 24 * time.Hour

	// MaxExpectExit bounds an expect_exit list. A process has 256 distinct exit
	// codes, so a longer list can only repeat itself — and an unbounded list is
	// an unbounded number of validation problems out of one submitted document.
	MaxExpectExit = 256
)

// maxDuration is the largest time.Duration, used to saturate a sum rather than
// let it wrap. A wrapped total reads as a small number and would slip past the
// MaxTotalTimeout guard, which is the one thing that guard exists to catch.
const maxDuration = time.Duration(math.MaxInt64)

// ---------------------------------------------------------------------------
// Durations
// ---------------------------------------------------------------------------

// Duration is a time.Duration that marshals as a quoted Go duration string.
//
// Unmarshalling a JSON *number* is a deliberate error rather than a unit guess.
// The zero value marshals as "0s" and is only ever written where a field is
// mandatory; optional duration fields carry omitempty, so an unset duration is
// an absent key and round-trips as one.
//
// Note that canonicalisation happens on the way out, not on the way in: a
// hand-written "5m" unmarshals fine and marshals back as "5m0s". That is a
// property of Unmarshal -> Marshal, which nothing depends on. What is
// guaranteed, and tested, is Marshal -> Unmarshal -> Marshal.
type Duration time.Duration

// Std returns d as a plain time.Duration, for arithmetic and for passing to
// context.WithTimeout.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer with the same spelling MarshalJSON writes.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, time.Duration(d).String()), nil
}

// UnmarshalJSON implements json.Unmarshaler.
//
// Only a JSON string is accepted. The offending token is echoed through
// [snippet] rather than whole: a duration field holding a large object would
// otherwise put that whole object into an error message that ends up in a log
// line or an HTTP response.
func (d *Duration) UnmarshalJSON(data []byte) error {
	// Caught before the decode, which would otherwise treat null as a no-op and
	// leave a bare "" for time.ParseDuration to complain about — a message that
	// does not tell the author the key was the problem.
	if string(bytes.TrimSpace(data)) == "null" {
		return errors.New("jobspec: a duration may not be null; write a quoted Go duration " +
			"such as \"90s\", or omit the key entirely to inherit the spec default")
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("jobspec: %s is not a duration; write a quoted Go duration such as \"90s\", \"5m\" or \"2h30m\", never a bare number", snippet(data))
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("jobspec: %q is not a duration: %w; write a quoted Go duration such as \"90s\", \"5m\" or \"2h30m\"", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// ---------------------------------------------------------------------------
// The closed vocabulary
// ---------------------------------------------------------------------------

// Kind names a step kind. The values are exactly the primary keys of
// farm.step_kinds.
type Kind string

// The ten kinds, in the order they are inserted into farm.step_kinds.
const (
	KindPush          Kind = "push"
	KindInstall       Kind = "install"
	KindUninstall     Kind = "uninstall"
	KindShell         Kind = "shell"
	KindShellDetached Kind = "shell_detached"
	KindWaitFor       Kind = "wait_for"
	KindPull          Kind = "pull"
	KindAssert        Kind = "assert"
	KindReset         Kind = "reset"
	KindSleep         Kind = "sleep"
)

// KindInfo is one row of farm.step_kinds, minus the description.
//
// The description is prose for operators and stays in the database, where it
// can be corrected without a release. The two booleans are behaviour — they
// decide whether a resume may repeat a step and whether an artifact must exist
// before the step can run — so they are mirrored here and pinned by
// TestKindTableMatchesMigration.
type KindInfo struct {
	Kind Kind

	// Idempotent reports whether re-running the step after a crash leaves the
	// same outcome. A non-idempotent step must be checkpointed BEFORE it runs,
	// or a resume repeats its side effect.
	Idempotent bool

	// NeedsArtifact reports whether the step's payload must name an artifact
	// by content hash. [Validate] requires a 64-hex-character sha256 for every
	// such step, matching the CHECK on farm.artifacts.sha256.
	NeedsArtifact bool
}

// kindTable mirrors, in order, the INSERT INTO farm.step_kinds in
// migrations/00004_operate.sql. Do not edit it by hand without editing that
// migration: the test that reads the file will say so if you do.
var kindTable = []KindInfo{
	{KindPush, true, true},
	{KindInstall, true, true},
	{KindUninstall, true, false},
	{KindShell, false, false},
	{KindShellDetached, false, false},
	{KindWaitFor, true, false},
	{KindPull, true, false},
	{KindAssert, true, false},
	{KindReset, true, false},
	{KindSleep, true, false},
}

// Kinds returns the vocabulary in schema order. The returned slice is a copy.
func Kinds() []KindInfo { return append([]KindInfo(nil), kindTable...) }

// Info returns the schema facts about k.
func (k Kind) Info() (KindInfo, bool) {
	for _, info := range kindTable {
		if info.Kind == k {
			return info, true
		}
	}
	return KindInfo{}, false
}

// Valid reports whether k is one of the ten kinds.
func (k Kind) Valid() bool {
	_, ok := k.Info()
	return ok
}

func (k Kind) String() string { return string(k) }

// ---------------------------------------------------------------------------
// Payloads
// ---------------------------------------------------------------------------

// Payload is the kind-specific body of a step.
//
// The interface is closed: payload() is unexported, so no package outside this
// one can add an eleventh kind by satisfying it. That is the Go half of the
// same rule the database enforces with a foreign key from farm.job_steps.kind
// to farm.step_kinds.
type Payload interface {
	// Kind reports which of the ten kinds this payload is. It is the single
	// source of truth for a step's kind.
	Kind() Kind

	// payload closes the interface to this package.
	payload()
}

// Push copies an artifact onto the device. Idempotent: the same bytes at the
// same path twice is the same device.
type Push struct {
	// SHA256 is the content hash of the artifact, and is the artifact's only
	// name. It must exist in farm.artifacts before the step runs.
	SHA256 string `json:"sha256"`

	// Dest is an absolute path on the device.
	Dest string `json:"dest"`

	// Mode is an optional octal permission string ("0755") applied after the
	// copy. Empty means leave whatever the transfer produced.
	Mode string `json:"mode,omitempty"`
}

// Install installs an APK artifact.
type Install struct {
	// SHA256 names the APK in farm.artifacts. The package name and version
	// code are columns there; the spec does not repeat them, so it cannot
	// contradict them.
	SHA256 string `json:"sha256"`

	// Reinstall keeps the existing app's data (pm install -r).
	Reinstall bool `json:"reinstall,omitempty"`

	// Grant grants all runtime permissions at install time (pm install -g).
	Grant bool `json:"grant,omitempty"`
}

// Uninstall removes a package.
type Uninstall struct {
	Package string `json:"package"`
}

// Shell runs a command and waits for it.
//
// Not idempotent, and the vocabulary says so: farm.step_kinds marks shell
// false, which is what tells a resume it may not blindly re-run one.
//
// A shell step is for commands that finish promptly. Anything long belongs in
// [ShellDetached], because a command whose result arrives over a socket the
// control plane is holding open makes that socket the source of truth, and
// sockets to a rack of USB phones fail constantly.
type Shell struct {
	Command string `json:"command"`

	// ExpectExit is the set of exit codes treated as success. Empty means the
	// spec-level default, which is {0}.
	ExpectExit []int `json:"expect_exit,omitempty"`
}

// ShellDetached starts a command under nohup setsid and returns immediately.
//
// This is the shape of every long-running job in the farm, and it exists for
// one reason: the DEVICE owns the result. The command writes its output and
// its exit code to ResultPath on the device's own filesystem, so a dropped ADB
// transport at minute 90 costs a reconnect and nothing else — the work keeps
// running, and the answer is still on the phone when the socket comes back.
// This is the direct countermeasure to DeviceFarmer/STF issue #663, where a
// ~90 minute ECONNRESET ended somebody's multi-hour run.
type ShellDetached struct {
	Command string `json:"command"`

	// ResultPath is the absolute device-side path the wrapper writes the
	// command's output and exit status to. It must be unique within the spec:
	// two detached steps sharing one path is one of them silently losing.
	ResultPath string `json:"result_path"`

	// Handle is a short stable token the runner uses to find this process
	// again after a reconnect or a resume — it names the pid file and the log
	// beside ResultPath. It is unique within the spec and restricted to
	// characters that are safe in a device-side filename.
	Handle string `json:"handle"`
}

// WaitFor polls a shell probe until it exits zero.
//
// The probe is a device-side test, so a transport error while polling is just
// a poll that did not happen: the runner reconnects and keeps polling until
// Timeout, which is a deadline on the CONDITION, not on the connection.
type WaitFor struct {
	Probe    string   `json:"probe"`
	Interval Duration `json:"interval"`

	// Timeout is how long the condition may take. The enclosing step's timeout
	// must be at least this long, or the step would cut its own probe short.
	Timeout Duration `json:"timeout"`
}

// Pull copies a file off the device and stores it as a named artifact.
type Pull struct {
	// Path is the absolute device-side path to read.
	Path string `json:"path"`

	// Artifact is the name to store the bytes under. Its sha256 is not known
	// until the bytes exist, so unlike push and install this names content
	// that does not exist yet.
	Artifact string `json:"artifact"`
}

// Assert fails the job unless a probe reports the expected value.
//
// The runner compares the probe's trimmed standard output against Value using
// Operator. An assert is the honest way to end a job early: it is a statement
// about the DEVICE's answer, never about the transport that carried it.
type Assert struct {
	Probe    string   `json:"probe"`
	Operator Operator `json:"op"`
	Value    string   `json:"value"`
}

// Reset applies a reset tier. See [ResetSteps] for what each tier expands to.
type Reset struct {
	Tier ResetTier `json:"tier"`
}

// Sleep waits a fixed duration on the control-plane side.
type Sleep struct {
	// Duration must fit inside the step's effective timeout; a sleep longer
	// than the step that contains it can never finish.
	Duration Duration `json:"duration"`
}

func (Push) Kind() Kind          { return KindPush }
func (Install) Kind() Kind       { return KindInstall }
func (Uninstall) Kind() Kind     { return KindUninstall }
func (Shell) Kind() Kind         { return KindShell }
func (ShellDetached) Kind() Kind { return KindShellDetached }
func (WaitFor) Kind() Kind       { return KindWaitFor }
func (Pull) Kind() Kind          { return KindPull }
func (Assert) Kind() Kind        { return KindAssert }
func (Reset) Kind() Kind         { return KindReset }
func (Sleep) Kind() Kind         { return KindSleep }

func (Push) payload()          {}
func (Install) payload()       {}
func (Uninstall) payload()     {}
func (Shell) payload()         {}
func (ShellDetached) payload() {}
func (WaitFor) payload()       {}
func (Pull) payload()          {}
func (Assert) payload()        {}
func (Reset) payload()         {}
func (Sleep) payload()         {}

// Operator is the comparison an [Assert] step applies to its probe's output.
type Operator string

// The comparison vocabulary. Closed, like everything else here.
const (
	OpEQ          Operator = "eq"
	OpNE          Operator = "ne"
	OpContains    Operator = "contains"
	OpNotContains Operator = "not_contains"
	OpMatches     Operator = "matches" // RE2, compiled at validation time
	OpGT          Operator = "gt"      // numeric
	OpGE          Operator = "ge"      // numeric
	OpLT          Operator = "lt"      // numeric
	OpLE          Operator = "le"      // numeric
)

// Valid reports whether o is one of the nine operators.
func (o Operator) Valid() bool {
	switch o {
	case OpEQ, OpNE, OpContains, OpNotContains, OpMatches, OpGT, OpGE, OpLT, OpLE:
		return true
	default:
		return false
	}
}

// Numeric reports whether o compares numbers, in which case both the probe
// output and the expected value must parse as one.
func (o Operator) Numeric() bool {
	switch o {
	case OpGT, OpGE, OpLT, OpLE:
		return true
	default:
		return false
	}
}

func (o Operator) String() string { return string(o) }

// ResetTier mirrors, exactly, the CHECK on farm.jobs.reset_tier:
//
//	CHECK (reset_tier IN ('none','soft','medium','hard'))
type ResetTier string

const (
	TierNone   ResetTier = "none"
	TierSoft   ResetTier = "soft"
	TierMedium ResetTier = "medium"
	TierHard   ResetTier = "hard"
)

// Valid reports whether t is one of the four tiers the schema permits.
func (t ResetTier) Valid() bool {
	switch t {
	case TierNone, TierSoft, TierMedium, TierHard:
		return true
	default:
		return false
	}
}

func (t ResetTier) String() string { return string(t) }

// ---------------------------------------------------------------------------
// Step
// ---------------------------------------------------------------------------

// Step is one instruction. Its kind is its payload's kind; see the package doc
// for why there is no Kind field.
type Step struct {
	// ID is unique within the spec and STABLE across edits, because
	// farm.jobs.checkpoint and farm.job_steps.step_id both point at it. Change
	// an id and a resume of an in-flight job loses its place.
	ID string

	// Timeout bounds this step. Zero means inherit [Spec.DefaultTimeout]; the
	// effective value must be positive, so a spec that sets neither is
	// rejected rather than quietly running without a bound.
	Timeout Duration

	// ContinueOnError lets the job proceed when this step fails. It is about
	// the STEP's own failure — a non-zero exit, a failed assert — and never
	// about the transport, which is retried inside the lease and never
	// surfaces as a step failure at all.
	ContinueOnError bool

	// Payload carries the kind. Never nil in a step that survived unmarshal or
	// validation.
	Payload Payload
}

// Kind reports the step's kind, which is its payload's kind. A step with no
// payload has no kind, and reports the empty string rather than panicking, so
// that [Validate] can report it as a problem instead of crashing on it.
func (s Step) Kind() Kind {
	if nilPayload(s.Payload) {
		return ""
	}
	return s.Payload.Kind()
}

// stepEnvelope is the wire shape of a step, and the reason the encoder's output
// is deterministic: fixed fields in a fixed order, no maps anywhere.
//
// The payload lives under a key named for its kind rather than under a generic
// "with", so that the jsonb is queryable by kind from SQL —
// spec->'steps' @> '[{"install":{"sha256":"…"}}]' finds every job that
// installed a given build — and so that a mismatch between the "kind" key and
// the payload is a shape error rather than a silent reinterpretation.
type stepEnvelope struct {
	ID              string   `json:"id"`
	Kind            Kind     `json:"kind"`
	Timeout         Duration `json:"timeout,omitempty"`
	ContinueOnError bool     `json:"continue_on_error,omitempty"`

	// Exactly one of these is non-nil, and it is the one named by Kind.
	// Declared in schema order so the encoder's key order matches the
	// vocabulary's.
	Push          *Push          `json:"push,omitempty"`
	Install       *Install       `json:"install,omitempty"`
	Uninstall     *Uninstall     `json:"uninstall,omitempty"`
	Shell         *Shell         `json:"shell,omitempty"`
	ShellDetached *ShellDetached `json:"shell_detached,omitempty"`
	WaitFor       *WaitFor       `json:"wait_for,omitempty"`
	Pull          *Pull          `json:"pull,omitempty"`
	Assert        *Assert        `json:"assert,omitempty"`
	Reset         *Reset         `json:"reset,omitempty"`
	Sleep         *Sleep         `json:"sleep,omitempty"`
}

// present returns the payloads carried by the envelope, in schema order.
func (e *stepEnvelope) present() []Payload {
	var out []Payload
	if e.Push != nil {
		out = append(out, *e.Push)
	}
	if e.Install != nil {
		out = append(out, *e.Install)
	}
	if e.Uninstall != nil {
		out = append(out, *e.Uninstall)
	}
	if e.Shell != nil {
		out = append(out, *e.Shell)
	}
	if e.ShellDetached != nil {
		out = append(out, *e.ShellDetached)
	}
	if e.WaitFor != nil {
		out = append(out, *e.WaitFor)
	}
	if e.Pull != nil {
		out = append(out, *e.Pull)
	}
	if e.Assert != nil {
		out = append(out, *e.Assert)
	}
	if e.Reset != nil {
		out = append(out, *e.Reset)
	}
	if e.Sleep != nil {
		out = append(out, *e.Sleep)
	}
	return out
}

// set stores p in the field named by its kind.
func (e *stepEnvelope) set(p Payload) error {
	// Both Shell and *Shell satisfy Payload, because every Kind() has a value
	// receiver and a pointer's method set includes it. Go offers no way to
	// exclude the pointer form, so the package accepts both rather than
	// failing at marshal time on a spec that compiled cleanly. Validation
	// normalises the same way; see derefPayload.
	switch v := derefPayload(p).(type) {
	case Push:
		e.Push = &v
	case Install:
		e.Install = &v
	case Uninstall:
		e.Uninstall = &v
	case Shell:
		e.Shell = &v
	case ShellDetached:
		e.ShellDetached = &v
	case WaitFor:
		e.WaitFor = &v
	case Pull:
		e.Pull = &v
	case Assert:
		e.Assert = &v
	case Reset:
		e.Reset = &v
	case Sleep:
		e.Sleep = &v
	default:
		// Unreachable while Payload stays closed, and an error rather than a
		// panic because an unwritable step must not take a process down.
		return fmt.Errorf("jobspec: %T is not a step payload", p)
	}
	e.Kind = p.Kind()
	return nil
}

// MarshalJSON implements json.Marshaler.
//
// A step with no payload has no valid encoding and says so. Writing a
// kind-less step to farm.jobs.spec would produce a row that fails to unmarshal
// on resume, which is a defect discovered at the worst possible moment.
func (s Step) MarshalJSON() ([]byte, error) {
	if nilPayload(s.Payload) {
		return nil, fmt.Errorf("jobspec: step %q has no payload", s.ID)
	}
	env := stepEnvelope{
		ID:              s.ID,
		Timeout:         s.Timeout,
		ContinueOnError: s.ContinueOnError,
	}
	if err := env.set(s.Payload); err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// UnmarshalJSON implements json.Unmarshaler.
//
// Strict on purpose. An unknown key is an error rather than a silently dropped
// field, because a dropped field is a spec that means something different from
// what its author wrote — and because a spec written by a NEWER control plane
// must be refused by an older runner, not partially executed.
func (s *Step) UnmarshalJSON(data []byte) error {
	var env stepEnvelope
	if err := strictDecode(data, &env); err != nil {
		return fmt.Errorf("jobspec: step: %w", err)
	}

	if env.Kind == "" {
		return fmt.Errorf("jobspec: step %q has no kind; add a \"kind\" key naming one of %s",
			env.ID, kindList())
	}
	if !env.Kind.Valid() {
		return fmt.Errorf("jobspec: step %q has unknown kind %q; the vocabulary is %s. "+
			"A kind this control plane does not know is refused rather than skipped, "+
			"because a skipped step is a job that did less than it says it did",
			env.ID, env.Kind, kindList())
	}

	found := env.present()
	switch len(found) {
	case 0:
		return fmt.Errorf("jobspec: step %q of kind %q carries no %q payload; add a %q object beside the kind",
			env.ID, env.Kind, env.Kind, env.Kind)
	case 1:
	default:
		names := make([]string, 0, len(found))
		for _, p := range found {
			names = append(names, string(p.Kind()))
		}
		return fmt.Errorf("jobspec: step %q carries %d payloads (%s); a step is exactly one kind, so split it into %d steps",
			env.ID, len(found), strings.Join(names, ", "), len(found))
	}
	if got := found[0].Kind(); got != env.Kind {
		return fmt.Errorf("jobspec: step %q says kind %q but carries a %q payload; "+
			"correct whichever of the two is wrong — they cannot both be honoured",
			env.ID, env.Kind, got)
	}

	*s = Step{
		ID:              env.ID,
		Timeout:         env.Timeout,
		ContinueOnError: env.ContinueOnError,
		Payload:         found[0],
	}
	return nil
}

// ---------------------------------------------------------------------------
// Spec
// ---------------------------------------------------------------------------

// Spec is the whole document stored in farm.jobs.spec: an ordered list of
// steps plus the job-level defaults they inherit from.
//
// Order is the only control flow there is. There are no branches, no loops and
// no conditionals, because a spec has to be resumable from a checkpoint on a
// different process on a different day, and "where was I" must be answerable
// with an index and a step id.
type Spec struct {
	// Version is [SpecVersion]. Always written, always checked.
	Version int `json:"version"`

	// DefaultTimeout is the timeout for every step that does not set its own.
	DefaultTimeout Duration `json:"default_timeout,omitempty"`

	// DefaultExpectExit is the success set for shell steps that do not name
	// one. Nil means {0}.
	DefaultExpectExit []int `json:"default_expect_exit,omitempty"`

	// Steps run in order, exactly once each.
	Steps []Step `json:"steps"`
}

// New returns an empty spec at the current version, ready for steps.
func New(steps ...Step) Spec {
	return Spec{Version: SpecVersion, Steps: steps}
}

// UnmarshalJSON implements json.Unmarshaler, strictly. See
// [Step.UnmarshalJSON] for why unknown keys are refused.
func (s *Spec) UnmarshalJSON(data []byte) error {
	// The alias sheds the method set, so decoding it does not recurse.
	type alias Spec
	var a alias
	if err := strictDecode(data, &a); err != nil {
		return fmt.Errorf("jobspec: spec: %w", err)
	}
	*s = Spec(a)
	return nil
}

// Parse decodes and validates a spec read from farm.jobs.spec.
//
// This is the door every caller should use: a spec that only decodes is a spec
// that parsed, not a spec that can run. The returned error is a
// [*ValidationError] when the document was well-formed but wrong, so an API
// handler can render every problem at once.
func Parse(data []byte) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return Spec{}, err
	}
	if err := Validate(s); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// StepTimeout returns the timeout that actually applies to st: its own if set,
// otherwise the spec default. Zero means the spec is invalid, and [Validate]
// reports it.
func (s Spec) StepTimeout(st Step) time.Duration {
	if st.Timeout > 0 {
		return st.Timeout.Std()
	}
	return s.DefaultTimeout.Std()
}

// TotalTimeout is the sum of every step's positive effective timeout: the
// longest a spec could legitimately take if every step used all of its budget.
//
// The sum saturates at [maxDuration] and skips non-positive timeouts. Both
// matter because this value is compared against MaxTotalTimeout: a wrapped sum
// or a negative step timeout would let an impossible spec read as a short one
// and pass the guard that exists to reject it. Non-positive timeouts are
// reported on their own by [Validate].
func (s Spec) TotalTimeout() time.Duration {
	var total time.Duration
	for _, st := range s.Steps {
		d := s.StepTimeout(st)
		if d <= 0 {
			continue
		}
		if total > maxDuration-d {
			return maxDuration
		}
		total += d
	}
	return total
}

// ExpectExit returns the success exit codes that apply to sh, falling back to
// the spec default and then to {0}.
//
// The result is a copy: a runner that sorted or appended to it in place would
// otherwise be editing the spec's own default, silently changing what counts as
// success for every later step.
func (s Spec) ExpectExit(sh Shell) []int {
	switch {
	case len(sh.ExpectExit) > 0:
		return append([]int(nil), sh.ExpectExit...)
	case len(s.DefaultExpectExit) > 0:
		return append([]int(nil), s.DefaultExpectExit...)
	default:
		return []int{0}
	}
}

// StepByID finds a step by the id a checkpoint recorded. The index is returned
// because a resume needs the position, and the position is not derivable from
// the id anywhere else.
func (s Spec) StepByID(id string) (int, Step, bool) {
	for i, st := range s.Steps {
		if st.ID == id {
			return i, st, true
		}
	}
	return 0, Step{}, false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// strictDecode unmarshals exactly one JSON value into v, refusing unknown
// fields and trailing content.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing content after the JSON value")
	}
	return nil
}

// snippet bounds a fragment of a caller's document before it goes into an
// error message. Error strings are logged and returned over HTTP, so an
// unbounded one turns a malformed field into a megabyte of log.
func snippet(data []byte) string {
	const max = 64
	if len(data) <= max {
		return string(data)
	}
	return string(data[:max]) + "... (truncated)"
}

// kindList renders the vocabulary for an error message.
func kindList() string {
	names := make([]string, 0, len(kindTable))
	for _, info := range kindTable {
		names = append(names, string(info.Kind))
	}
	return strings.Join(names, ", ")
}
