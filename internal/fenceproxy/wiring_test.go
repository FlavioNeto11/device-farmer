package fenceproxy

// The seam through which a deployment names the shell commands its own packages
// dial, and the enumeration a test walks instead of retyping.
//
// Both exist because of one defect with two halves. DefaultPolicy's comment said
// "internal/watchdog uses only host:track-devices-l" while that package's battery
// probe was opening "shell,v2,raw:dumpsys battery", so on every farm with the
// proxy on the battery-health loop was refused at the socket on every device. The
// first half of the fix is that the literal now travels from the package that
// owns it, through [Policy.AllowingDeviceServices]; the second is that no test
// anywhere may enumerate the classes by hand again, because the hand-written list
// is what failed to notice.
//
// These tests therefore assert the seam cannot be used to grant what it was not
// meant to grant, and that [Classes] cannot drift from [Class.Valid].

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// wideReq builds a request for a class that carries no fence, so the whitelist
// is the only thing a case below can be measuring.
func wideReq(class Class, service string) Request {
	return Request{
		Identity: Identity{Subject: "wiring-test", Class: class, NotAfter: t0.Add(time.Hour)},
		Service:  service,
		Bound:    devA,
	}
}

// TestClassesIsEveryClassValidAccepts parses Valid's switch and compares it to
// the list Classes returns.
//
// A test that walks the classes is only as good as the list it walks. Both lists
// are literal identifier lists in proxy.go, so the only thing that can go wrong
// is somebody editing one of them — which is exactly what this reads the syntax
// tree to catch.
func TestClassesIsEveryClassValidAccepts(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing proxy.go: %v", err)
	}

	var fromValid, fromClasses []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		switch fn.Name.Name {
		case "Valid":
			if fn.Recv == nil {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, e := range cc.List {
					if id, ok := e.(*ast.Ident); ok {
						fromValid = append(fromValid, id.Name)
					}
				}
				return true
			})
		case "Classes":
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				for _, e := range cl.Elts {
					if id, ok := e.(*ast.Ident); ok {
						fromClasses = append(fromClasses, id.Name)
					}
				}
				return true
			})
		}
		return true
	})

	if len(fromValid) == 0 || len(fromClasses) == 0 {
		t.Fatalf("the scan found %d classes in Valid and %d in Classes; it is asserting nothing, "+
			"which means one of the two functions was renamed or reshaped",
			len(fromValid), len(fromClasses))
	}
	sort.Strings(fromValid)
	sort.Strings(fromClasses)
	if strings.Join(fromValid, ",") != strings.Join(fromClasses, ",") {
		t.Errorf("Class.Valid accepts [%s] and Classes returns [%s]. Every test that walks the "+
			"classes walks Classes, so a class Valid accepts and Classes omits is a class no "+
			"assertion in this tree covers.",
			strings.Join(fromValid, " "), strings.Join(fromClasses, " "))
	}

	// And the list is not merely consistent with the parse: every element of it
	// satisfies Valid at run time.
	for _, c := range Classes() {
		if !c.Valid() {
			t.Errorf("Classes returns %q, which Class.Valid rejects", c)
		}
	}
}

// TestAllowingDeviceServicesPublishesWhatAPackageOwns is the happy path: a
// literal and a whole-string pattern handed in from outside are admitted for the
// named class, and for that class only.
func TestAllowingDeviceServicesPublishesWhatAPackageOwns(t *testing.T) {
	t.Parallel()

	const literal = "shell,v2,raw:dumpsys battery"
	pattern := regexp.MustCompile(`shell,v2,raw:printf '%s' 'df-[0-9a-f]{32}'`)
	templated := "shell,v2,raw:printf '%s' 'df-0123456789abcdef0123456789abcdef'"

	// base is held across the call on purpose: DefaultPolicy builds a fresh map
	// every time it is called, so comparing against a SECOND call would pass even
	// if the method had mutated the first one's map.
	base := DefaultPolicy()
	pol, err := base.AllowingDeviceServices(ClassMaintenance,
		[]string{literal}, []*regexp.Regexp{pattern})
	if err != nil {
		t.Fatalf("publishing a literal and a pattern to the maintenance class: %v", err)
	}

	for _, svc := range []string{literal, templated} {
		if d := Admit(wideReq(ClassMaintenance, svc), View{}, t0, pol); !d.Admitted() {
			t.Errorf("maintenance %q: outcome = %s (%s), want admit — a package published this "+
				"and the proxy still refuses it", svc, d.Outcome, d.Reason)
		}
	}

	// Published to one class, granted to one class. A widening that leaked into
	// another class would give the enroll credential the watchdog's reach.
	for _, svc := range []string{literal, templated} {
		if d := Admit(wideReq(ClassEnroll, svc), View{}, t0, pol); d.Admitted() {
			t.Errorf("enroll %q was admitted by a widening published to maintenance; the classes "+
				"are separate credentials and their reach must not be pooled", svc)
		}
	}

	// And the policy the method was called ON is untouched. It says it returns a
	// copy, and a copy that shared the Rules map would widen a policy somebody
	// else is already serving — internal/node holds one per host and a second
	// caller widening it would be invisible.
	if d := Admit(wideReq(ClassMaintenance, literal), View{}, t0, base); d.Admitted() {
		t.Error("the policy AllowingDeviceServices was called on now admits the published " +
			"literal, so it mutated its receiver's Rules map instead of copying it")
	}
}

// TestAllowingDeviceServicesRefusesAnythingButADeviceService pins what the
// signature is for. The seam exists to publish shell commands; a host service or
// a host-target verb put through it would sit in ServiceRules.Device where
// allows() never looks for one, and the caller would believe it had granted
// something it had not.
func TestAllowingDeviceServicesRefusesAnythingButADeviceService(t *testing.T) {
	t.Parallel()

	for _, svc := range []string{
		"host:kill",
		"host:version",
		"host-serial:" + devA + ":get-state",
		"host:transport:" + devA,
		"",
	} {
		if _, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance,
			[]string{svc}, nil); err == nil {
			t.Errorf("publishing %q as a device service was accepted; it would be unreachable in "+
				"ServiceRules.Device and the grant would be silent", svc)
		}
	}

	// host:kill in particular: accepting it here is how the one service this
	// file grants nobody would arrive through the back door.
	pol, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, []string{"host:kill"}, nil)
	if err == nil {
		if d := Admit(wideReq(ClassMaintenance, "host:kill"), View{}, t0, pol); d.Admitted() {
			t.Fatal("host:kill reached the maintenance whitelist through AllowingDeviceServices; " +
				"it severs every device on the host including the ones under live leases")
		}
	}
	if err != nil && !strings.Contains(err.Error(), "device") {
		t.Errorf("the refusal does not say what kind of service was wrong: %v", err)
	}
}

// TestAllowingDeviceServicesWillNotInventAClassBound asserts the one case that
// must not be silently accepted: a class with no entry in Policy.Rules. Creating
// the entry here would decide a class's whole bound from outside DefaultPolicy,
// and ClassLease is absent from Rules on purpose.
func TestAllowingDeviceServicesWillNotInventAClassBound(t *testing.T) {
	t.Parallel()

	_, err := DefaultPolicy().AllowingDeviceServices(ClassLease, []string{"reboot:"}, nil)
	if err == nil {
		t.Fatal("ClassLease was given a device whitelist from outside DefaultPolicy; the job " +
			"runner executes arbitrary step kinds and is bounded by its fence, and giving it a " +
			"whitelist would refuse every step kind nobody enumerated")
	}
	if !strings.Contains(err.Error(), "Policy.Rules") {
		t.Errorf("error = %q; it should name the omission (Policy.Rules) so the reader knows "+
			"where a class's bound is decided", err)
	}

	if _, err := DefaultPolicy().AllowingDeviceServices(Class("typo"), []string{"reboot:"}, nil); err == nil {
		t.Error("a class this proxy has never heard of was given a whitelist")
	}
}

// TestAllowingDeviceServicesRefusesANilPattern: a nil entry in the slice would
// panic inside allows on the first device service of the first connection, which
// is a crash on the data path rather than a configuration error at startup.
func TestAllowingDeviceServicesRefusesANilPattern(t *testing.T) {
	t.Parallel()

	if _, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, nil,
		[]*regexp.Regexp{nil}); err == nil {
		t.Fatal("a nil pattern was accepted; it would panic in allows on the first device " +
			"service that reached this class")
	}
}

// TestAllowingDeviceServicesRefusesAPatternThatCanNeverMatchADeviceService is the
// literal check applied to the other half of the seam.
//
// A pattern is consulted for a KindDevice service and nothing else, so one that
// can only ever match a host or host-target string sits in DevicePatterns where
// nothing looks for it. Accepting it silently is the failure this whole mechanism
// exists to prevent, arriving through the mechanism: the caller believes it
// published a grant, the caller it was for is refused forever at the ADB socket,
// and the symptom looks like broken hardware.
func TestAllowingDeviceServicesRefusesAPatternThatCanNeverMatchADeviceService(t *testing.T) {
	t.Parallel()

	for _, src := range []string{
		`host:kill`,                         // wholly literal, and a host service
		`host:version`,                      //
		`host:track-devices-l`,              //
		`host-serial:usb:3-1\.4:get-[a-z]+`, // a variable tail on a host-target head
		`host-usb:usb:3-1\.4:get-[a-z]+`,    //
		`host:transport:usb:3-1\.[0-9]`,     //
		`host[:]version`,                    // a literal head still growing into a marker
		`host-us[b]:usb:3-1\.4:get-state`,   //
		`host:tra[a-z]+:usb:3-1\.[0-9]`,     //
	} {
		if _, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, nil,
			[]*regexp.Regexp{regexp.MustCompile(src)}); err == nil {
			t.Errorf("pattern %q was published as a device pattern; it can never match a device "+
				"service, so the grant would be silent and its caller refused forever", src)
		}
	}

	// And the patterns that DO describe device services keep working, including
	// one whose very first character is variable — that case cannot be judged
	// from a literal prefix and is accepted deliberately.
	for _, src := range []string{
		`shell,v2,raw:dumpsys [a-z]+`,
		`localabstract:scrcpy_[0-9a-f]{8}`,
		`(shell|raw),v2,raw:id`,
	} {
		if _, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, nil,
			[]*regexp.Regexp{regexp.MustCompile(src)}); err != nil {
			t.Errorf("pattern %q was refused: %v. The check must reject only what can never be a "+
				"device service, or it becomes a second whitelist nobody wrote down.", src, err)
		}
	}

	// The shipped policy still builds. A check that rejected control()'s own
	// patterns would fail the node agent at startup on every fenced host.
	if _, err := DefaultPolicy().AllowingDeviceServices(ClassControl, nil,
		DefaultPolicy().Rules[ClassControl].DevicePatterns); err != nil {
		t.Errorf("the shipped control patterns cannot be re-published through the seam: %v", err)
	}
}

// TestAPatternsRepeatedRegionsMustAgree is the rule RE2 cannot state.
//
// A command template whose value appears twice compiles, through
// regexp.QuoteMeta, into two INDEPENDENT regions, and such a pattern admits a
// command in which they differ. For internal/enroll's brand write the two places
// are a guard ("the file is absent, empty or already holds this uid") and an
// install ("write this uid"), so independence admits "check for A, write B" —
// which passes the device-side guard and rebrands a phone that already carried
// somebody else's identity. Nothing about that string looks wrong; it is built
// from the right template out of the right alphabet, and only the correlation
// refuses it.
func TestAPatternsRepeatedRegionsMustAgree(t *testing.T) {
	t.Parallel()

	// guard-then-install in miniature: the same value in two places, plus a
	// second value that is genuinely allowed to differ from it.
	pattern := regexp.MustCompile(
		`shell,v2,raw:check (?P<uid>[0-9a-f]{4}) or (?P<prev>[0-9a-f]{4}); write (?P<uid>[0-9a-f]{4})`)

	pol, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, nil,
		[]*regexp.Regexp{pattern})
	if err != nil {
		t.Fatalf("publishing the pattern: %v", err)
	}
	admit := func(svc string) bool {
		return Admit(wideReq(ClassMaintenance, svc), View{}, t0, pol).Admitted()
	}

	// The shape the template produces: both "uid" groups hold the same value,
	// and "prev" is free to be anything.
	if !admit("shell,v2,raw:check aaaa or bbbb; write aaaa") {
		t.Fatal("the command the template actually builds was refused; every case below would " +
			"then pass for the wrong reason")
	}
	if !admit("shell,v2,raw:check aaaa or aaaa; write aaaa") {
		t.Error("two differently named groups holding the same value were refused; only groups " +
			"SHARING a name are correlated")
	}

	// The near miss. Right template, right alphabet, wrong command.
	if admit("shell,v2,raw:check aaaa or bbbb; write cccc") {
		t.Error("a command whose two \"uid\" regions disagree was admitted. Nothing in the " +
			"regexp refuses it — RE2 has no backreferences — so if this passes, a pattern built " +
			"by substituting one value into several places grants every combination of values, " +
			"and internal/enroll's brand guard stops guarding anything.")
	}

	// And the correlation is not a substitute for the alphabet: it constrains
	// which values may appear, never which characters.
	if admit("shell,v2,raw:check aaaa or bbbb; write aaaa; id") {
		t.Error("a whole-string match still accepted a suffix")
	}

	// An unnamed group is unconstrained, so the patterns already shipped in
	// control() keep working exactly as they did.
	free := regexp.MustCompile(`shell,v2,raw:pair ([0-9a-f]{4}) ([0-9a-f]{4})`)
	polFree, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, nil,
		[]*regexp.Regexp{free})
	if err != nil {
		t.Fatalf("publishing the unnamed pattern: %v", err)
	}
	if d := Admit(wideReq(ClassMaintenance, "shell,v2,raw:pair aaaa bbbb"), View{}, t0, polFree); !d.Admitted() {
		t.Errorf("two unnamed groups holding different values were refused (%s: %s); the "+
			"correlation must apply only where a pattern asks for it, or every existing pattern "+
			"silently narrows", d.Outcome, d.Reason)
	}
}

// TestAPublishedPatternCannotCarryASecondCommand is the assertion any pattern
// must earn, applied to the seam rather than to one pattern: a whole-string
// pattern published from outside admits the command it describes and nothing
// appended to it.
//
// The pattern here is the shape internal/enroll publishes — regexp.QuoteMeta of
// a literal command with one hex-digit variable region — which is why appending
// anything at all fails: there is no position in the pattern that accepts a
// character other than the ones spelled out in it.
func TestAPublishedPatternCannotCarryASecondCommand(t *testing.T) {
	t.Parallel()

	const admitted = "shell,v2,raw:printf '%s' 'df-0123456789abcdef0123456789abcdef' > /data/local/tmp/.farm/uid"
	pattern := regexp.MustCompile(regexp.QuoteMeta(
		"shell,v2,raw:printf '%s' 'df-UID' > /data/local/tmp/.farm/uid"))
	pattern = regexp.MustCompile(strings.Replace(pattern.String(),
		regexp.QuoteMeta("df-UID"), `df-[0-9a-f]{32}`, 1))

	pol, err := DefaultPolicy().AllowingDeviceServices(ClassMaintenance, nil,
		[]*regexp.Regexp{pattern})
	if err != nil {
		t.Fatalf("publishing the pattern: %v", err)
	}
	if d := Admit(wideReq(ClassMaintenance, admitted), View{}, t0, pol); !d.Admitted() {
		t.Fatalf("the unmodified command was refused (%s: %s); every case below would then pass "+
			"for the wrong reason", d.Outcome, d.Reason)
	}

	for _, tail := range []string{
		"; rm -rf /sdcard",
		" && id",
		" || id",
		" | sh",
		"\nid",
		"$(id)",
		"`id`",
		" > /sdcard/out",
		" &",
		" #",
	} {
		if d := Admit(wideReq(ClassMaintenance, admitted+tail), View{}, t0, pol); d.Admitted() {
			t.Errorf("the published command extended with %q was admitted; a whole-string pattern "+
				"that accepts a suffix is the prefix hole in another costume", tail)
		}
	}

	// And the variable region is hex digits, not an alphabet a command fits in.
	for _, bad := range []string{
		"shell,v2,raw:printf '%s' 'df-0123456789abcdef0123456789abcde;' > /data/local/tmp/.farm/uid",
		"shell,v2,raw:printf '%s' 'df-$(id)' > /data/local/tmp/.farm/uid",
	} {
		if d := Admit(wideReq(ClassMaintenance, bad), View{}, t0, pol); d.Admitted() {
			t.Errorf("%q was admitted; the pattern's only variable region must hold nothing but "+
				"hex digits", bad)
		}
	}
}
