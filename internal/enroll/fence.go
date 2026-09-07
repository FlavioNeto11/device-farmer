package enroll

// What this package needs from a host's fence proxy.
//
// internal/fenceproxy admits a connection that carries no lease fence only to an
// exact list of ADB service strings, and for a class that may open a shell that
// list is the ONLY thing between the credential and a root shell on every
// handset in the rack. This file is this package's half of that list: the
// service strings it can actually put on the wire, published as values so that
// whoever builds a proxy admits the command this package sends rather than a
// copy of it typed into another file.
//
// The copy is the failure being prevented, and it is not hypothetical.
// fenceproxy.DefaultPolicy used to carry a comment saying enrolment's commands
// "are literals the enroller must publish into this list at wiring time rather
// than strings guessed at here" — and nothing published them, because nothing
// could: the proxy may not import this package (it imports a database driver)
// and the enroller never built a policy. The list stayed empty and the comment
// described a mechanism that did not exist. It exists now, it is
// fenceproxy.Policy.AllowingDeviceServices, and cmd/farmd's fencePolicy is the
// caller that hands it what is below.
//
// # Which class these reach
//
// Both, and for different reasons:
//
//   - fenceproxy.ClassEnroll is the class designed for them — it is the only one
//     whose reason for existing is that a brand-new handset has to be asked what
//     it is before it can be adopted. Nothing in this tree issues an
//     enroll-class certificate yet, so publishing here bounds the class ahead of
//     the credential rather than behind it.
//
//   - fenceproxy.ClassMaintenance reaches them today. POST
//     /api/v1/slots/{id}/rebrand builds a [Brander] over the API server's ADB
//     client, and that client announces the maintenance class, so the two brand
//     commands below arrive with that certificate. Leaving them off the
//     maintenance list meant the rebrand route was refused at the ADB socket on
//     every fenced farm.
//
// # Literals and templates
//
// [FenceDeviceServices] is the fixed commands: they are package-level values
// built from constants, identical on every run, so an exact-match whitelist
// holds them with nothing templated and nothing to get wrong.
//
// [FenceDevicePatterns] is the two brand WRITES, which interpolate a farm uid.
// A pattern over a shell command line is the one entry on a whitelist that can
// be written unsafely, and fenceproxy.ServiceRules says at length why: `;`,
// `&&`, a newline and `$( )` all extend a command that started out looking
// fine. These earn it a different way from the one pattern already shipped in
// that package — the scrcpy spawn restricts the ALPHABET, while these are
// LITERAL TEMPLATES. regexp.QuoteMeta takes the command this package actually
// builds and turns every character of it into a literal; the only region left
// variable is the uid, replaced by the regexp form of the CHECK constraint the
// uid already satisfies, thirty-two hex digits. There is no `.`, no `+` and no
// character class anywhere else in the pattern, so there is no position in an
// admitted string where a second command could be hidden, and the proxy's
// whole-string span check means there is no suffix either.
//
// The commands are read through the same builders the wire path uses, never
// retyped, so a pattern cannot describe a command this package no longer sends.
// buildFencePattern panics at package initialisation if the template and the
// command have come apart, which is the right moment to find out.
//
// # What is still reachable, said plainly
//
// A rebrand legitimately overwrites the uid the operator authorised it against,
// and a whitelist cannot tell an authorised rebrand from an unauthorised one.
// With [FenceDeviceServices]'s brand read admitted alongside, a holder of the
// certificate these are published to can read a phone's uid and then rebrand it
// to any well-formed uid — the same thing POST /api/v1/slots/{id}/rebrand offers
// an authenticated operator, reached without the API. What the patterns take
// away is the ability to run anything ELSE, and the ability to write a uid the
// device-side guard was not shown; what they cannot take away is the act the
// route exists to perform. cmd/farmd's fencePolicy records that trade where the
// grant is made, and docs/design/fence-proxy.md section 7.3 says what removes it:
// an enroll-class certificate for the API server, so this stops being pooled
// with the recovery ladder's reach.

import (
	"regexp"
	"strings"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// uidPattern is uidRe's body as a regexp source: the same thirty-two hex digits
// farm.devices' CHECK constraint permits, and nothing else. It is the only
// variable region in any pattern this file produces, and every character it can
// match is a hex digit — so an admitted command cannot carry a `;`, a `$(`, a
// newline or anything else that would make a second command out of it.
const uidPattern = `df-[0-9a-f]{32}`

// The names given to the uid regions, and they are load-bearing rather than
// documentation.
//
// Each of the commands below interpolates its uid into more than one place —
// once into the guard that decides whether the write may happen and once into
// the write itself — and RE2 has no backreferences, so a pattern built by
// substitution has as many INDEPENDENT regions as the command has occurrences.
// Such a pattern admits "guard that the file holds A, then write B", which is a
// command this package never builds and which walks straight through the
// device-side guard whose whole job is to stop one phone being rebranded over
// another's identity.
//
// fenceproxy.ServiceRules refuses a match whose groups sharing a name disagree,
// which is the correlation the regexp engine cannot state. So every occurrence
// of the SAME value is given the same name here, and the two different values in
// a rebrand — the uid being written and the one the operator authorised the
// overwrite against — are given different names, because they are genuinely
// allowed to differ.
const (
	uidGroup  = "uid"
	prevGroup = "prev"
)

// The uids used to build the patterns and the examples beside them. They are
// never written to a device: buildFencePattern substitutes uidPattern for them,
// and the examples exist so a guard test can push one real service string per
// shape through an admission decision instead of asserting a regexp against
// itself.
//
// They must be real uids, and TestTheFenceSampleUIDsAreRealFarmUIDs asserts it.
// Two of them are self-asserting — buildFencePattern's whole-string check
// panics if the region it substituted does not match — but fenceSampleThird is
// only ever interpolated into a near miss, so nothing here would notice it
// drifting outside uidRe. If it did, every near-miss assertion would still pass
// and would be passing for the WRONG REASON: refused by the pattern's alphabet
// instead of by the named-group correlation, silently retiring the coverage of
// the one rule that keeps the brand guard meaningful.
const (
	fenceSampleUID  = "df-0123456789abcdef0123456789abcdef"
	fenceSamplePrev = "df-fedcba9876543210fedcba9876543210"
	// fenceSampleThird is only ever the uid a near miss tries to install: a
	// third value so that a rebrand near miss installs something neither arm of
	// its guard admitted.
	fenceSampleThird = "df-abad1deaabad1deaabad1deaabad1dea"
)

// FenceDeviceServices returns the device-side ADB service strings this package
// opens that are fixed literals: the identity probe and the brand read.
//
// A fence proxy must admit exactly these for the class whose credential the
// enroller presents. See this file's header for which classes that is and why
// they are published from here rather than named in the proxy.
func FenceDeviceServices() []string {
	return []string{
		adbwire.ShellService(probeCommand),
		adbwire.ShellService(brandReadCmd),
	}
}

// FenceDevicePatterns returns the whole-string patterns that admit the two brand
// writes, whose commands interpolate a farm uid and therefore cannot be matched
// exactly.
//
// Each pattern is regexp.QuoteMeta of the command this package builds, with the
// uid regions replaced by [uidPattern]. [FenceDevicePatternExamples] returns one
// real service string per pattern, in the SAME ORDER, so a caller can prove each
// pattern admits the command it was built from and refuses anything appended to
// it.
func FenceDevicePatterns() []*regexp.Regexp {
	// A copy: the patterns are built once at initialisation so their self-checks
	// run in EVERY binary that imports this package rather than only in the one
	// role that builds a proxy, and a caller that could append to the shared
	// slice would be widening a whitelist from arm's length.
	return append([]*regexp.Regexp(nil), fenceDevicePatterns...)
}

// fenceDevicePatterns is built at package initialisation, which is the point:
// buildFencePattern's checks are assertions about this package's own commands,
// and a binary that imports internal/enroll should fail at start rather than at
// the moment a proxy is configured. cmd/farmd's node role is the only caller of
// FenceDevicePatterns, so deferring the build to it would have meant the API
// server and the watchdog never ran these checks at all.
var fenceDevicePatterns = []*regexp.Regexp{
	buildFencePattern(brandWriteCmd(fenceSampleUID),
		uidRegion{uidGroup, fenceSampleUID}),
	buildFencePattern(brandReplaceCmd(fenceSampleUID, fenceSamplePrev),
		uidRegion{uidGroup, fenceSampleUID}, uidRegion{prevGroup, fenceSamplePrev}),
}

// uidRegion names one interpolated value in a command template. Every occurrence
// of value becomes a capture group called name, and the proxy requires all
// groups sharing a name to have captured the same text.
type uidRegion struct {
	name  string
	value string
}

// FenceDevicePatternExamples returns one service string per entry of
// [FenceDevicePatterns], in the same order, built from the same command builders
// with a real farm uid in place.
//
// It exists so that the assertion "the proxy admits what this package dials" can
// be made against a STRING that goes on a wire, rather than against a regexp
// compared with another regexp. A test that only checked the patterns would pass
// just as happily if the commands had changed underneath them.
func FenceDevicePatternExamples() []string {
	return []string{
		adbwire.ShellService(brandWriteCmd(fenceSampleUID)),
		adbwire.ShellService(brandReplaceCmd(fenceSampleUID, fenceSamplePrev)),
	}
}

// FenceDeviceNearMisses returns service strings that a proxy must REFUSE even
// though they are built from the same templates as [FenceDevicePatternExamples].
//
// They exist because the dangerous mistake here is not a pattern that admits
// obvious nonsense — that is caught by anyone reading it — but a pattern that
// admits a command which looks exactly right and is not one this package would
// ever send. Every entry below is a guard on one farm uid followed by an install
// of a DIFFERENT one: "check that the phone holds A, then write B onto it". No
// code path in this package builds that, and a proxy that admitted it would let
// a credential rebrand a phone whose identity it had merely read, which is the
// one thing [ConflictError] exists to make impossible. RE2 has no
// backreferences, so nothing in the regexp itself refuses these; what refuses
// them is the correlation the pattern declares by naming its repeated regions,
// enforced by fenceproxy.ServiceRules.
//
// A caller that admits these has not lost the ability to run a second command —
// the alphabet still stops that — it has lost the guard.
func FenceDeviceNearMisses() []string {
	return []string{
		// A first-write guard on one uid, installing another.
		adbwire.ShellService(brandGuard(fenceSampleUID) + brandInstall(fenceSamplePrev)),
		// The same substitution inside a rebrand, whose guard legitimately
		// admits two uids and whose install must still be one of them.
		adbwire.ShellService(brandGuard(fenceSampleUID, fenceSamplePrev) + brandInstall(fenceSampleThird)),
	}
}

// buildFencePattern turns one command into a whole-string pattern over its
// service string, with each region's value replaced by a capture group named
// after it and matching [uidPattern].
//
// It panics rather than returning an error, and at package initialisation
// rather than at admission time, because there is no runtime input here: every
// argument comes from this package's own builders, so a failure means the
// commands and this file have come apart in an edit. Finding that when the
// binary starts is the difference between a failed build and a farm whose
// enrolment is refused at the ADB socket with nobody knowing why.
func buildFencePattern(cmd string, regions ...uidRegion) *regexp.Regexp {
	service := adbwire.ShellService(cmd)
	src := regexp.QuoteMeta(service)
	for _, r := range regions {
		quoted := regexp.QuoteMeta(r.value)
		if !strings.Contains(src, quoted) {
			panic("enroll: fence pattern: the command does not contain the uid " + r.value +
				" it was built with, so the pattern would have no variable region: " + service)
		}
		src = strings.ReplaceAll(src, quoted, `(?P<`+r.name+`>`+uidPattern+`)`)
	}
	re := regexp.MustCompile(src)
	// The pattern must match the command it was built from, end to end. The
	// proxy checks the match SPAN rather than trusting a pattern's anchors, so a
	// pattern that matched only part of this string would be admitted against
	// nothing and enrolment would be refused with the whitelist looking correct.
	if loc := re.FindStringIndex(service); loc == nil || loc[0] != 0 || loc[1] != len(service) {
		panic("enroll: fence pattern does not match the whole command it was built from: " + service)
	}
	// Every region must have produced at least one group, or the correlation the
	// proxy enforces would be enforced over nothing.
	for _, r := range regions {
		var found bool
		for _, name := range re.SubexpNames() {
			if name == r.name {
				found = true
				break
			}
		}
		if !found {
			panic("enroll: fence pattern has no capture group named " + r.name +
				", so the proxy has no correlation to enforce on it: " + re.String())
		}
	}
	return re
}
