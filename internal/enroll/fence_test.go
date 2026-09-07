package enroll

// The samples the fence patterns are built from, and the near misses built
// beside them.
//
// fence.go's patterns are what a host's proxy admits for this package, and two
// of the three uids in it are self-asserting: buildFencePattern panics if the
// region it substituted does not match the command. fenceSampleThird is not,
// because it appears only inside a near miss — a string that must be REFUSED.
// A refusal is a weak assertion: it passes whether the string was refused for
// the reason the test is about or for some accident, so the accident has to be
// ruled out here.

import (
	"strings"
	"testing"
)

// TestTheFenceSampleUIDsAreRealFarmUIDs is the assertion fence.go's comment
// claims. Without it, a fenceSampleThird edited to something outside uidRe would
// leave every near-miss test passing — refused by the pattern's ALPHABET rather
// than by the named-group correlation, which is the rule those tests exist to
// exercise and the only thing standing between a maintenance credential and
// rebranding a phone over its own guard.
func TestTheFenceSampleUIDsAreRealFarmUIDs(t *testing.T) {
	t.Parallel()

	for name, uid := range map[string]string{
		"fenceSampleUID":   fenceSampleUID,
		"fenceSamplePrev":  fenceSamplePrev,
		"fenceSampleThird": fenceSampleThird,
	} {
		if !IsFarmUID(uid) {
			t.Errorf("%s = %q is not a farm uid. A sample outside uidRe makes the pattern built "+
				"from it, or the near miss built from it, prove something other than what its "+
				"test says it proves.", name, uid)
		}
	}

	// Distinct, or a "near miss" would be the real command and the correlation
	// rule would be admitting it rather than refusing it.
	for _, pair := range [][2]string{
		{fenceSampleUID, fenceSamplePrev},
		{fenceSampleUID, fenceSampleThird},
		{fenceSamplePrev, fenceSampleThird},
	} {
		if pair[0] == pair[1] {
			t.Errorf("two fence sample uids are the same value (%q); a near miss built from them "+
				"is not a near miss", pair[0])
		}
	}
}

// TestTheNearMissesAreTheRightCommandWithTheWrongUID guards the other half. A
// near miss must differ from the command this package builds ONLY in which uid
// is installed — if it differed in any other character it would be refused by
// the alphabet, and the correlation rule would go untested.
func TestTheNearMissesAreTheRightCommandWithTheWrongUID(t *testing.T) {
	t.Parallel()

	misses := FenceDeviceNearMisses()
	examples := FenceDevicePatternExamples()
	if len(misses) != len(examples) {
		t.Fatalf("%d near misses for %d templates; each template needs one", len(misses), len(examples))
	}

	for i, miss := range misses {
		if miss == examples[i] {
			t.Errorf("near miss %d is identical to the command this package builds; it is not a "+
				"near miss and its refusal would be a bug", i)
			continue
		}
		// Same length and same alphabet: a farm uid is a fixed-width string of
		// hex digits, so swapping one for another changes no other property of
		// the command. Anything else means the near miss stopped being one.
		if len(miss) != len(examples[i]) {
			t.Errorf("near miss %d is %d bytes and the command it mimics is %d; a uid swap changes "+
				"no length, so this differs by more than the uid and would be refused by the "+
				"alphabet rather than by the correlation rule",
				i, len(miss), len(examples[i]))
		}
		for _, meta := range []string{";", "&&", "||", "|", "\n", "$(", "`", ">"} {
			if strings.Count(miss, meta) != strings.Count(examples[i], meta) {
				t.Errorf("near miss %d contains a different number of %q than the command it "+
					"mimics; it must differ only in which uid is installed", i, meta)
			}
		}
	}
}
