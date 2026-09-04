package jobspec

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Problems
// ---------------------------------------------------------------------------

// Problem is one defect in a spec, addressed the way the author wrote it.
//
// Path is a JSON pointer in dotted form — "steps[3].push.dest" — so an API can
// hand it straight back to the submitter and an editor can highlight the line.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// ValidationError carries EVERY problem [Validate] found, not the first.
//
// A person fixing a spec should need one round trip, not ten. That is the
// entire reason this type exists instead of a plain error: the first defect in
// a document is rarely the only one, and a validator that stops at it turns a
// five-minute edit into an afternoon of resubmissions.
// A spec may hold a thousand steps, so Problems is not bounded — an API
// renders all of it and a submitter wants all of it. Error() is bounded,
// because that string is what reaches a log line.
type ValidationError struct {
	Problems []Problem
}

// renderedProblems is how many problems Error() spells out. Past this the
// message says how many more there are and points at Problems, which still
// carries every one: a half-megabyte log line helps nobody at 3am.
const renderedProblems = 25

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "jobspec: invalid spec: " + e.Problems[0].String()
	}
	shown := e.Problems
	suffix := ""
	if len(shown) > renderedProblems {
		suffix = fmt.Sprintf("; and %d more (the full list is in ValidationError.Problems)",
			len(shown)-renderedProblems)
		shown = shown[:renderedProblems]
	}
	parts := make([]string, 0, len(shown))
	for _, p := range shown {
		parts = append(parts, p.String())
	}
	return fmt.Sprintf("jobspec: invalid spec, %d problems: %s%s",
		len(e.Problems), strings.Join(parts, "; "), suffix)
}

// validator accumulates problems and the cross-step bookkeeping that some
// rules need: a rule like "one artifact, one destination" cannot be decided by
// looking at a single step.
type validator struct {
	spec     Spec
	problems []Problem

	stepIDs     map[string]string // id            -> path of the first step using it
	handles     map[string]string // detached handle -> path
	resultPaths map[string]string // detached result path -> path
	pushDest    map[string]seen   // artifact sha256 -> where it was first pushed
	pullSource  map[string]seen   // artifact name   -> where it was first pulled from
}

// seen records the first use of an artifact so a later conflicting use can name
// its counterpart instead of just saying "conflict".
type seen struct {
	path   string // JSON path of the first step
	stepID string
	target string // device path involved
}

func (v *validator) add(path, format string, args ...any) {
	v.problems = append(v.problems, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

// Validate reports every problem in s, or nil when the spec can run.
//
// The rules are of three sorts, and it is worth knowing which is which:
//
//   - Shape rules the Go types cannot express (a non-empty id, a positive
//     timeout, a unique handle).
//   - Schema rules, taken from the database rather than from memory: the
//     artifact hash format is farm.artifacts' CHECK, the reset tiers are
//     farm.jobs.reset_tier's CHECK, and which kinds need an artifact at all is
//     farm.step_kinds.needs_artifact.
//   - Consistency rules that catch a spec which could not possibly do what its
//     author meant: a sleep longer than the step that contains it, a wait_for
//     whose own deadline outlives the step's, one artifact pushed to two
//     different paths.
//
// Nothing here checks whether an artifact EXISTS — that is a foreign key
// against farm.artifacts and belongs to the database, which owns the answer.
func Validate(s Spec) error {
	v := &validator{
		spec:        s,
		stepIDs:     map[string]string{},
		handles:     map[string]string{},
		resultPaths: map[string]string{},
		pushDest:    map[string]seen{},
		pullSource:  map[string]seen{},
	}

	if s.Version != SpecVersion {
		v.add("version", "must be %d, got %d; this control plane reads version %d only, "+
			"so resubmit against a build that speaks version %d rather than running a document it would have to guess at",
			SpecVersion, s.Version, SpecVersion, s.Version)
	}

	// A default that is itself out of range is reported once here rather than
	// once per step, so one mistake produces one message.
	checkInherited := true
	switch {
	case s.DefaultTimeout < 0:
		v.add("default_timeout", "must not be negative")
		checkInherited = false
	case s.DefaultTimeout.Std() > MaxStepTimeout:
		v.add("default_timeout", "must be at most %s, got %s", MaxStepTimeout, s.DefaultTimeout)
		checkInherited = false
	}
	checkExitCodes(v, "default_expect_exit", s.DefaultExpectExit)

	// Only the first MaxSteps steps are inspected. Past the cap the spec is
	// already rejected, and walking a runaway document would turn one bad
	// submission into an unbounded list of problems to allocate and render.
	steps := s.Steps
	switch {
	case len(steps) == 0:
		v.add("steps", "a spec must contain at least one step; a job with nothing to do "+
			"would hold a device for its whole lease and change nothing")
	case len(steps) > MaxSteps:
		v.add("steps", "a spec may contain at most %d steps, got %d; only the first %d were checked. "+
			"A list this long is a generated loop that wanted a shell_detached script on the device",
			MaxSteps, len(steps), MaxSteps)
		steps = steps[:MaxSteps]
	}

	for i, st := range steps {
		path := fmt.Sprintf("steps[%d]", i)
		checkStep(v, path, st, checkInherited)
	}

	if total := s.TotalTimeout(); total > MaxTotalTimeout {
		v.add("steps", "the step timeouts add up to %s, which is more than the %s a job may span",
			total, MaxTotalTimeout)
	}

	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: v.problems}
}

func checkStep(v *validator, path string, st Step, checkInherited bool) {
	// Id first: every later message quotes it.
	switch {
	case st.ID == "":
		v.add(path+".id", "must not be empty; a checkpoint records this id and a resume looks it up")
	case len(st.ID) > MaxStepIDLen:
		v.add(path+".id", "must be at most %d characters, got %d", MaxStepIDLen, len(st.ID))
	case !stepIDRe.MatchString(st.ID):
		v.add(path+".id", "%q must start alphanumeric and use only letters, digits and . _ - : /", st.ID)
	}
	if st.ID != "" {
		if first, dup := v.stepIDs[st.ID]; dup {
			v.add(path+".id", "duplicate step id %q, already used by %s", st.ID, first)
		} else {
			v.stepIDs[st.ID] = path
		}
	}

	if st.Payload == nil {
		v.add(path, "has no payload, so it has no kind")
		return
	}
	kind := st.Kind()
	// Defensive: Payload is a closed interface, so an unknown kind cannot be
	// constructed today. The check stays because the rule "known kinds" is one
	// the database enforces with a foreign key, and a rule worth a foreign key
	// is worth an assertion.
	info, known := kind.Info()
	if !known {
		v.add(path+".kind", "unknown kind %q; the vocabulary is %s", kind, kindList())
		return
	}

	// Timeout. The effective value is what actually bounds the step.
	eff := v.spec.StepTimeout(st)
	switch {
	case st.Timeout < 0:
		v.add(path+".timeout", "must not be negative")
	case st.Timeout > 0 && st.Timeout.Std() > MaxStepTimeout:
		v.add(path+".timeout", "must be at most %s, got %s", MaxStepTimeout, st.Timeout)
	// Skipped when default_timeout was already reported: one bad default must
	// not produce one message per step.
	case eff <= 0 && checkInherited:
		v.add(path+".timeout", "must be positive; neither the step nor the spec's default_timeout sets one")
	}

	// The artifact rule is driven by farm.step_kinds.needs_artifact rather
	// than by a list written out here, so adding a kind to the schema without
	// teaching this package about it fails loudly instead of silently letting
	// an artifact-less step through.
	if info.NeedsArtifact {
		ref, ok := st.Payload.(artifactStep)
		switch {
		case !ok:
			v.add(path, "kind %q needs an artifact but its payload names none; "+
				"the Go model has drifted from farm.step_kinds", kind)
		case !isSHA256(ref.artifactSHA256()):
			v.add(path+"."+string(kind)+".sha256",
				"must be 64 lowercase hex characters (farm.artifacts.sha256), got %q",
				ref.artifactSHA256())
		}
	}

	p := path + "." + string(kind)
	switch pl := st.Payload.(type) {
	case Push:
		checkDevicePath(v, p+".dest", pl.Dest)
		if pl.Mode != "" && !modeRe.MatchString(pl.Mode) {
			v.add(p+".mode", "must be three or four octal digits such as \"0755\", got %q; "+
				"omit the key entirely to leave whatever the transfer produced", pl.Mode)
		}
		if isSHA256(pl.SHA256) && isDevicePath(pl.Dest) {
			noteArtifactTarget(v, v.pushDest, pl.SHA256, "pushed to", p+".dest", st.ID, pl.Dest)
		}

	case Install:
		// sha256 is covered by the needs_artifact rule above; the package name
		// and version code live in farm.artifacts, and reinstall and grant are
		// booleans with no invalid value. Nothing further to check.

	case Uninstall:
		checkPackage(v, p+".package", pl.Package)

	case Shell:
		if strings.TrimSpace(pl.Command) == "" {
			v.add(p+".command", "must name the command to run on the device, such as \"am force-stop com.example.app\"")
		}
		checkExitCodes(v, p+".expect_exit", pl.ExpectExit)

	case ShellDetached:
		if strings.TrimSpace(pl.Command) == "" {
			v.add(p+".command", "must name the command to start on the device; "+
				"it runs detached, so this is the long work whose result the device keeps")
		}
		checkDevicePath(v, p+".result_path", pl.ResultPath)
		if pl.ResultPath != "" {
			if first, dup := v.resultPaths[pl.ResultPath]; dup {
				v.add(p+".result_path",
					"%q is already written by %s; two detached steps sharing one result file is one of them losing",
					pl.ResultPath, first)
			} else {
				v.resultPaths[pl.ResultPath] = path
			}
		}
		if !handleRe.MatchString(pl.Handle) {
			v.add(p+".handle",
				"%q must start alphanumeric and use only letters, digits and . _ - (it names a file on the device)",
				pl.Handle)
		} else if first, dup := v.handles[pl.Handle]; dup {
			v.add(p+".handle", "duplicate handle %q, already used by %s; "+
				"a handle is how a reconnected runner finds this process again", pl.Handle, first)
		} else {
			v.handles[pl.Handle] = path
		}

	case WaitFor:
		if strings.TrimSpace(pl.Probe) == "" {
			v.add(p+".probe", "must be a shell command that exits 0 once the condition holds, "+
				`such as "[ \"$(getprop sys.boot_completed)\" = \"1\" ]"`)
		}
		if pl.Interval <= 0 {
			v.add(p+".interval", "must be positive, got %s; write how often to re-run the probe, such as \"5s\"", pl.Interval)
		}
		if pl.Timeout <= 0 {
			v.add(p+".timeout", "must be positive, got %s; write how long the condition may take, such as \"10m\"", pl.Timeout)
		}
		if pl.Interval > 0 && pl.Timeout > 0 && pl.Interval > pl.Timeout {
			v.add(p+".interval", "%s is longer than the %s this step waits, so the probe would run once at most",
				pl.Interval, pl.Timeout)
		}
		// The step's own timeout must outlast the condition's, or the step
		// kills a probe that was still allowed to succeed.
		if pl.Timeout > 0 && eff > 0 && pl.Timeout.Std() > eff {
			v.add(p+".timeout", "%s is longer than the step's %s timeout, which would cut the probe short",
				pl.Timeout, Duration(eff))
		}

	case Pull:
		checkDevicePath(v, p+".path", pl.Path)
		checkArtifactName(v, p+".artifact", pl.Artifact)
		if isDevicePath(pl.Path) && artifactNameRe.MatchString(pl.Artifact) {
			noteArtifactTarget(v, v.pullSource, pl.Artifact, "pulled from", p+".path", st.ID, pl.Path)
		}

	case Assert:
		if strings.TrimSpace(pl.Probe) == "" {
			v.add(p+".probe", "must be a shell command whose standard output is the value to compare, "+
				`such as "getprop ro.build.version.sdk"`)
		}
		switch {
		case !pl.Operator.Valid():
			v.add(p+".op", "unknown operator %q; use one of %s", pl.Operator, operatorList())
		case pl.Operator == OpMatches:
			if _, err := regexp.Compile(pl.Value); err != nil {
				v.add(p+".value", "is not a valid RE2 expression: %v; note that RE2 has no backreferences or lookaround", err)
			}
		case pl.Operator.Numeric():
			if _, err := strconv.ParseFloat(strings.TrimSpace(pl.Value), 64); err != nil {
				v.add(p+".value", "operator %q compares numbers, and %q is not one; "+
					"use \"eq\" or \"contains\" to compare it as text", pl.Operator, pl.Value)
			}
		}

	case Reset:
		if !pl.Tier.Valid() {
			v.add(p+".tier", "unknown tier %q; farm.jobs.reset_tier permits %s",
				pl.Tier, "none, soft, medium, hard")
		}

	case Sleep:
		if pl.Duration <= 0 {
			v.add(p+".duration", "must be positive, got %s; write how long to wait, such as \"30s\"", pl.Duration)
		} else if eff > 0 && pl.Duration.Std() > eff {
			v.add(p+".duration", "%s is longer than the step's %s timeout, so the step could never finish",
				pl.Duration, Duration(eff))
		}

	default:
		// Unreachable while Payload is closed; if it ever fires, a kind was
		// added without rules and the spec would be accepted unchecked.
		v.add(path, "kind %q has no validation rules in this package", kind)
	}
}

// noteArtifactTarget enforces "one artifact, one path".
//
// Pushing the same bytes to two different places, or pulling two different
// files into one artifact name, is almost always a copy-paste that will be
// discovered as a mysteriously wrong file three hours into a run. Both
// directions are the same defect and both are refused.
func noteArtifactTarget(v *validator, index map[string]seen, key, verb, path, stepID, target string) {
	first, ok := index[key]
	if !ok {
		index[key] = seen{path: path, stepID: stepID, target: target}
		return
	}
	if first.target != target {
		v.add(path, "artifact %q is also %s %q by step %q; one artifact means one path",
			key, verb, first.target, first.stepID)
	}
}

func checkExitCodes(v *validator, path string, codes []int) {
	// Bounded before it is walked: a process has 256 exit codes, so a longer
	// list can only repeat itself, and an unbounded list would produce an
	// unbounded number of problems out of a single submitted field.
	if len(codes) > MaxExpectExit {
		v.add(path, "lists %d exit codes; a process has only %d, so list the distinct codes that mean success",
			len(codes), MaxExpectExit)
		return
	}
	dup := map[int]bool{}
	for i, c := range codes {
		if c < 0 || c > 255 {
			v.add(fmt.Sprintf("%s[%d]", path, i),
				"exit codes run from 0 to 255, got %d; a shell reports the low 8 bits only", c)
		}
		if dup[c] {
			v.add(fmt.Sprintf("%s[%d]", path, i), "exit code %d is listed twice; remove the duplicate", c)
		}
		dup[c] = true
	}
}

func checkDevicePath(v *validator, path, value string) {
	switch {
	case value == "":
		v.add(path, "must name an absolute path on the device, such as \"/data/local/tmp/app.apk\"")
	case !strings.HasPrefix(value, "/"):
		v.add(path, "%q must be an absolute device path; prefix it with \"/\", "+
			"there is no working directory to resolve it against", value)
	case !isDevicePath(value):
		// Deliberately narrow. These strings are interpolated into commands
		// that run on the device, so the safe set is the VALIDATED set: a path
		// that needs quoting is refused instead of quoted, and the farm chooses
		// its own paths anyway.
		v.add(path, "%q may contain only letters, digits and . _ - / and no \"..\" segment; "+
			"rename the file rather than quoting it — this path is interpolated into a device shell command", value)
	}
}

func checkArtifactName(v *validator, path, value string) {
	switch {
	case value == "":
		v.add(path, "must name the artifact the pulled bytes are stored under, such as \"logcat.txt\"")
	case len(value) > MaxArtifactNameLen:
		v.add(path, "must be at most %d characters, got %d; shorten it", MaxArtifactNameLen, len(value))
	case !artifactNameRe.MatchString(value):
		v.add(path, "%q must start alphanumeric and use only letters, digits and . _ -; "+
			"this is a name, not a path, so it may not contain \"/\"", value)
	}
}

func checkPackage(v *validator, path, value string) {
	if value == "" {
		v.add(path, "must name the Android package to remove, such as \"com.example.app\"")
		return
	}
	if !packageRe.MatchString(value) {
		v.add(path, "%q is not an Android package name; it is interpolated into a device shell command, "+
			"so only letters, digits, _ and dot-separated segments are accepted", value)
	}
}

// artifactStep is implemented by the payloads whose kind has
// step_kinds.needs_artifact = true. The generic check in checkStep uses it so
// the rule is driven by the schema rather than by a hard-coded pair of cases.
type artifactStep interface{ artifactSHA256() string }

func (p Push) artifactSHA256() string    { return p.SHA256 }
func (p Install) artifactSHA256() string { return p.SHA256 }

var (
	// stepIDRe also allows ':' and '/', which the tier expansions below use to
	// namespace their ids.
	stepIDRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	handleRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	artifactNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	modeRe         = regexp.MustCompile(`^[0-7]{3,4}$`)
	// packageRe is permissive about the number of segments on purpose:
	// "android" is a real package. What it excludes is everything that would
	// mean something to a shell.
	packageRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)*$`)
	devicePathRe = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)
)

// isSHA256 mirrors the CHECK on farm.artifacts.sha256:
// CHECK (sha256 ~ '^[0-9a-f]{64}$'). Written out rather than compiled so the
// comparison to the schema is one line of Go against one line of SQL.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isDevicePath(s string) bool {
	if !devicePathRe.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func operatorList() string {
	ops := []string{
		string(OpEQ), string(OpNE), string(OpContains), string(OpNotContains),
		string(OpMatches), string(OpGT), string(OpGE), string(OpLT), string(OpLE),
	}
	return strings.Join(ops, ", ")
}

// ---------------------------------------------------------------------------
// Reset tiers
// ---------------------------------------------------------------------------

// DeviceStateScript is where the runner materialises the device-side script
// that reapplies farm.device_state before a medium or hard reset runs.
//
// It cannot be a push step: push names an artifact by content hash, and the
// content of this script is not known until the device's current row in
// farm.device_state has been read, which happens after the spec was written.
// So the runner writes the file and the expansion below runs it. The step
// fails loudly when the file is missing, which is correct — a missing script
// means the durable state was NOT reapplied, and a device that quietly kept a
// previous job's settings is how a test suite starts failing for reasons
// nobody can reproduce.
const DeviceStateScript = "/data/local/tmp/farm/device_state.sh"

// Reset step timeouts. Generous, because they run on the slowest phone in the
// rack and a reset that times out costs an attempt.
const (
	clearTimeout       = 60 * time.Second
	uninstallTimeout   = 10 * time.Minute
	deviceStateTimeout = 5 * time.Minute
	rebootTimeout      = 2 * time.Minute

	// rebootSettle is how long the expansion waits after issuing the reboot
	// before it starts believing the device's answers again. See the
	// reset/reboot-settle step for why this is not optional.
	rebootSettle = 30 * time.Second

	bootWait = 10 * time.Minute
	bootPoll = 5 * time.Second
)

// ResetSteps expands a reset tier into the ordered steps that perform it.
//
// "soft", "medium" and "hard" mean nothing until somebody writes them down.
// Written down, they are:
//
//	none   — nothing at all.
//	soft   — clear the app data of every package the profile owns that is
//	         actually installed.
//	medium — soft, then uninstall every third-party package that is NOT in the
//	         profile, then reapply farm.device_state.
//	hard   — medium, then reboot, wait out the shutdown, and wait for
//	         boot_completed.
//
// The only input is the profile's package list, which is what makes reset
// generic: the farm does not need to know what a job installed, because
// anything outside the profile is by definition not supposed to be there.
// That is also why medium's uninstall is one shell step that enumerates the
// device rather than a list of uninstall steps — the set is discovered on the
// device, at reset time, not guessed at when the spec was written.
//
// Package names are validated before they are interpolated into a shell
// command; an invalid name is an error, never a quoted string, because these
// commands run as shell on a device holding somebody's lease.
//
// The returned ids are namespaced under "reset/" and are stable for a given
// tier and package list. A spec that expands two resets must rename one set:
// step ids are unique within a spec because checkpoints resolve them.
func ResetSteps(tier ResetTier, profilePackages []string) ([]Step, error) {
	if !tier.Valid() {
		return nil, fmt.Errorf("jobspec: unknown reset tier %q; farm.jobs.reset_tier permits none, soft, medium, hard", tier)
	}
	var bad []string
	// farm.profiles.packages is a plain text[] with no uniqueness constraint, so
	// a repeated name is a real row. Left alone it becomes two steps with the
	// same id, which Validate rejects as a duplicate id far away from the
	// profile that actually caused it.
	seenPkg := make(map[string]bool, len(profilePackages))
	for _, pkg := range profilePackages {
		switch {
		case !packageRe.MatchString(pkg):
			bad = append(bad, strconv.Quote(pkg))
		case seenPkg[pkg]:
			bad = append(bad, fmt.Sprintf("%s (listed more than once; step ids must be unique)", strconv.Quote(pkg)))
		case len(clearStepPrefix+pkg) > MaxStepIDLen:
			// The name becomes a step id, and an id the validator would reject
			// is caught here, where the message can name the package, rather
			// than later as a mystery problem at steps[n].id.
			bad = append(bad, fmt.Sprintf("%s (its step id would be %d characters, over the %d limit)",
				strconv.Quote(pkg), len(clearStepPrefix+pkg), MaxStepIDLen))
		}
		seenPkg[pkg] = true
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("jobspec: profile package list contains %d unusable name(s): %s; "+
			"fix them in farm.profiles.packages — they are interpolated into commands that run on a leased device",
			len(bad), strings.Join(bad, ", "))
	}
	if tier == TierNone {
		return nil, nil
	}

	steps := make([]Step, 0, len(profilePackages)+5)

	// soft — and the first half of medium and hard.
	for _, pkg := range profilePackages {
		steps = append(steps, Step{
			ID:      clearStepPrefix + pkg,
			Timeout: Duration(clearTimeout),
			Payload: Shell{Command: clearCommand(pkg)},
		})
	}
	if tier == TierSoft {
		// nil rather than an empty slice, so "nothing to do" has one spelling
		// whichever tier produced it.
		if len(steps) == 0 {
			return nil, nil
		}
		return steps, nil
	}

	steps = append(steps,
		Step{
			ID:      "reset/uninstall-unknown",
			Timeout: Duration(uninstallTimeout),
			Payload: Shell{Command: uninstallUnknownCommand(profilePackages)},
		},
		Step{
			ID:      "reset/device-state",
			Timeout: Duration(deviceStateTimeout),
			Payload: Shell{Command: "sh " + DeviceStateScript},
		},
	)
	if tier == TierMedium {
		return steps, nil
	}

	steps = append(steps,
		Step{
			ID:      "reset/reboot",
			Timeout: Duration(rebootTimeout),
			// The reboot severs the very socket that would report its exit
			// code, so its self-report is worthless and is ignored on purpose.
			// The wait_for below is the actual proof the device came back;
			// this is not a transport failure being tolerated, it is a command
			// whose success is only observable from the other side of it.
			ContinueOnError: true,
			Payload:         Shell{Command: "svc power reboot || reboot"},
		},
		Step{
			// A device does not go down the instant the reboot returns; it
			// takes seconds to tear adbd down. Without this pause the probe
			// below reads sys.boot_completed from the system that is still
			// shutting down, sees "1", and declares the reset finished — and
			// the job then starts on a device that reboots underneath it a few
			// seconds later. Since reset/reboot cannot fail (see above), this
			// pause is the only thing that makes the probe's answer mean
			// anything. It costs 30 seconds and buys the difference between
			// "the device came back" and "the device has not left yet".
			ID:      "reset/reboot-settle",
			Timeout: Duration(rebootSettle + 15*time.Second),
			Payload: Sleep{Duration: Duration(rebootSettle)},
		},
		Step{
			ID: "reset/boot-completed",
			// Outlasts the probe's own deadline, or the step would cut short a
			// device that was still allowed to be booting.
			Timeout: Duration(bootWait + time.Minute),
			Payload: WaitFor{
				// getprop exits 0 even when the property is unset, so the
				// value is compared rather than the exit code trusted.
				Probe:    `[ "$(getprop sys.boot_completed)" = "1" ]`,
				Interval: Duration(bootPoll),
				Timeout:  Duration(bootWait),
			},
		},
	)
	return steps, nil
}

// clearStepPrefix namespaces the per-package clear steps. Its length is part
// of the step-id budget ResetSteps checks against MaxStepIDLen.
const clearStepPrefix = "reset/clear/"

// clearCommand builds the device-side command that empties one package's data.
//
// The existence test in front is not defensive noise. pm clear prints "Failed"
// for a package that is not installed, and on a device that has not run this
// profile's job yet — a freshly enrolled phone, or one after a hard reset —
// none of the profile's packages are installed. Without the test, the very
// first reset of every new device fails, the attempt is charged, and a healthy
// phone is quarantined for being clean. A package that is absent already has
// no data to clear, which is the state the step was asking for.
//
// grep decides the verdict rather than the exit code because pm clear exits 0
// while printing "Failed" on several vendor builds. grep -x anchors the whole
// line, so a filter that also matched a longer name (pm list packages does a
// substring match) cannot be mistaken for the package itself.
func clearCommand(pkg string) string {
	return "pm list packages " + pkg + " | grep -qx \"package:" + pkg + "\" || exit 0; " +
		"pm clear " + pkg + " | grep -q Success"
}

// uninstallUnknownCommand builds the device-side loop that removes every
// third-party package the profile does not own.
//
// pm list packages -3 lists only third-party packages, so nothing shipped in
// the system image is at risk. rc is carried explicitly because a for loop
// exits with the status of its last iteration, which would hide a failure in
// any earlier one.
//
// The enumeration is run and checked before the loop instead of inside the
// command substitution of a for. Inlined, a package manager that is not
// answering yields an empty list, the loop runs zero times, and the step exits
// 0 — a reset that reports success having removed nothing, leaving one
// tenant's app installed for the next one. Failing here instead costs an
// attempt and says why.
func uninstallUnknownCommand(profilePackages []string) string {
	keep := " " + strings.Join(profilePackages, " ") + " "
	return "rc=0; keep=\"" + keep + "\"; " +
		"pkgs=$(pm list packages -3) || " +
		"{ echo \"pm list packages -3 failed; the package manager is not answering, " +
		"so foreign packages could not be enumerated\" >&2; exit 1; }; " +
		"for p in $(echo \"$pkgs\" | cut -d: -f2); do " +
		"case \"$keep\" in *\" $p \"*) continue;; esac; " +
		"pm uninstall --user 0 \"$p\" | grep -q Success || rc=1; " +
		"done; exit $rc"
}
