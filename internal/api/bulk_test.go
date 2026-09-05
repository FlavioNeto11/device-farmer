package api

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// hasCond reports whether the rendered WHERE clause contains an exact
// condition. Conditions are compared whole rather than by substring so that
// "f.health = 'healthy'" cannot be satisfied by "f.health = $1".
func hasCond(conds []string, want string) bool {
	for _, c := range conds {
		if c == want {
			return true
		}
	}
	return false
}

func condsWithPrefix(conds []string, prefix string) []string {
	var out []string
	for _, c := range conds {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// TestBulkSelectorDefaultsExcludeSickDevices is the regression test for a bulk
// run that reached hardware it had no business reaching. An empty selector used
// to mean "every attached, enabled device", which included devices that were
// offline, unauthorized or mid-reboot, and devices sitting under an open
// quarantine with the recovery ladder actively power-cycling them.
func TestBulkSelectorDefaultsExcludeSickDevices(t *testing.T) {
	conds, args := bulkSelectorWhere(bulkSelector{})

	if !hasCond(conds, "f.health = 'healthy'") {
		t.Errorf("an empty selector does not restrict health; conditions were %q", conds)
	}
	if !hasCond(conds, "f.quarantine_id IS NULL") {
		t.Errorf("an empty selector reaches quarantined devices; conditions were %q", conds)
	}
	// The addressability conditions must survive the change.
	for _, want := range []string{
		"f.adb_devpath IS NOT NULL",
		"f.adb_endpoint IS NOT NULL",
		"f.admin_state = 'enabled'",
	} {
		if !hasCond(conds, want) {
			t.Errorf("condition %q went missing; conditions were %q", want, conds)
		}
	}
	if len(args) != 0 {
		t.Errorf("an empty selector should bind no arguments, got %v", args)
	}
}

// TestBulkSelectorHealthOptIns covers the two ways an operator says they mean
// it. Naming a health is itself the opt-in: ANDing 'healthy' onto a request for
// 'offline' would match nothing and read as a bug rather than as a refusal.
func TestBulkSelectorHealthOptIns(t *testing.T) {
	t.Run("explicit health replaces the default", func(t *testing.T) {
		conds, args := bulkSelectorWhere(bulkSelector{Health: "offline"})
		if hasCond(conds, "f.health = 'healthy'") {
			t.Fatalf("asking for offline devices also demanded healthy ones: %q", conds)
		}
		if got := condsWithPrefix(conds, "f.health"); len(got) != 1 {
			t.Fatalf("expected exactly one health condition, got %q", got)
		}
		if len(args) != 1 || args[0] != "offline" {
			t.Fatalf("the health value was not bound as an argument: %v", args)
		}
	})

	t.Run("include_unhealthy drops the health condition", func(t *testing.T) {
		conds, _ := bulkSelectorWhere(bulkSelector{IncludeUnhealthy: true})
		if got := condsWithPrefix(conds, "f.health"); len(got) != 0 {
			t.Fatalf("include_unhealthy still filtered on health: %q", got)
		}
		// Health and quarantine are separate facts: wanting to reach a
		// degraded handset is not asking to interrupt the recovery ladder.
		if !hasCond(conds, "f.quarantine_id IS NULL") {
			t.Fatalf("include_unhealthy also lifted the quarantine exclusion: %q", conds)
		}
	})

	t.Run("include_quarantined lifts only the quarantine exclusion", func(t *testing.T) {
		conds, _ := bulkSelectorWhere(bulkSelector{IncludeQuarantined: true})
		if hasCond(conds, "f.quarantine_id IS NULL") {
			t.Fatalf("include_quarantined did not lift the exclusion: %q", conds)
		}
		if !hasCond(conds, "f.health = 'healthy'") {
			t.Fatalf("include_quarantined also lifted the health exclusion: %q", conds)
		}
	})

	t.Run("both opt-ins reach everything addressable", func(t *testing.T) {
		conds, _ := bulkSelectorWhere(bulkSelector{IncludeUnhealthy: true, IncludeQuarantined: true})
		if len(conds) != 3 {
			t.Fatalf("expected only the three addressability conditions, got %q", conds)
		}
	})
}

var bulkPlaceholderRe = regexp.MustCompile(`\$(\d+)`)

// TestBulkSelectorPlaceholdersMatchArgs guards the hazard in building a WHERE
// clause and its argument list side by side: a condition that renders $3 while
// only two arguments were appended is a runtime error from Postgres on an
// operator's command, not a compile error here. Every selector field is
// exercised at once so the numbering is checked at its densest.
func TestBulkSelectorPlaceholdersMatchArgs(t *testing.T) {
	conds, args := bulkSelectorWhere(bulkSelector{
		Pool:      "default",
		Host:      "h01",
		Hub:       "3-1",
		Health:    "degraded",
		Model:     "Pixel",
		DeviceIDs: []string{"df-0000"},
	})

	seen := map[int]bool{}
	for _, m := range bulkPlaceholderRe.FindAllStringSubmatch(strings.Join(conds, " AND "), -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unparsable placeholder %q", m[0])
		}
		if n < 1 || n > len(args) {
			t.Fatalf("condition references $%d but only %d argument(s) were bound", n, len(args))
		}
		seen[n] = true
	}
	for i := 1; i <= len(args); i++ {
		if !seen[i] {
			t.Errorf("argument $%d was bound but no condition uses it", i)
		}
	}
}

// TestLeaseListDoesNotBroadcastHolderInstance belongs with these because both
// are the same shape of defect — an operator-facing read handing out more than
// it should — and this unit adds no other test file to package api.
//
// holder_instance is the only private member of the triple farm.lease_renew
// matches on, (id, fence, holder_instance); lease_id and fence are already
// published by /api/v1/fleet and by the event stream. Listing it made every
// live lease renewable by anyone who could read the list.
func TestLeaseListDoesNotBroadcastHolderInstance(t *testing.T) {
	if strings.Contains(leaseColumns, "holder_instance") {
		t.Error("leaseColumns selects holder_instance into a farm-wide listing")
	}

	body, err := json.Marshal(leaseView{})
	if err != nil {
		t.Fatalf("marshal leaseView: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal leaseView: %v", err)
	}
	if _, found := fields["holder_instance"]; found {
		t.Error("a listed lease carries holder_instance in its JSON")
	}
	// holder and holder_epoch are descriptions, not credentials, and the
	// operator view would be poorer without them.
	for _, want := range []string{"holder", "holder_epoch"} {
		if _, found := fields[want]; !found {
			t.Errorf("a listed lease no longer carries %q", want)
		}
	}
}

// TestLeaseColumnsMatchScanTargets keeps the projection and the scan in step.
// scanLease passes positional pointers, so a column removed from one and not
// the other is a scan error on every list request rather than a build failure.
func TestLeaseColumnsMatchScanTargets(t *testing.T) {
	// No column expression in leaseColumns contains a comma of its own, so a
	// plain split counts them.
	cols := strings.Split(leaseColumns, ",")
	if got, want := len(cols), reflect.TypeOf(leaseView{}).NumField(); got != want {
		t.Fatalf("leaseColumns selects %d column(s) but leaseView has %d field(s)", got, want)
	}
}
