package recovery

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// TestRefusalKind pins the rule: 'refused_ganged' iff the blocking lease is on
// a neighbour AND the domain is ganged. A per-port neighbour or the device's
// own lease is a policy refusal — the rack could have switched one port, or
// nobody else was involved, so the signal "buy per-port hubs" must not fire.
//
// Falsify: drop the neighbour condition, and the device's own lease on a
// ganged hub starts counting as a hardware complaint.
func TestRefusalKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		neighbour bool
		powerKind string
		want      obs.RecoveryOutcome
	}{
		{"own lease, ganged domain", false, "ganged", obs.OutcomeRefusedPolicy},
		{"own lease, per-port domain", false, "per_port", obs.OutcomeRefusedPolicy},
		{"neighbour, ganged domain", true, "ganged", obs.OutcomeRefusedGanged},
		{"neighbour, per-port domain", true, "per_port", obs.OutcomeRefusedPolicy},
		{"neighbour, no switch at all", true, "none", obs.OutcomeRefusedPolicy},
		{"neighbour, unknown domain kind", true, "", obs.OutcomeRefusedPolicy},
	}
	for _, tc := range cases {
		if got := RefusalKind(tc.neighbour, tc.powerKind); got != tc.want {
			t.Errorf("%s: RefusalKind(%v, %q) = %q, want %q",
				tc.name, tc.neighbour, tc.powerKind, got, tc.want)
		}
	}
}

// TestBlastRadiusCheckUsesRefusalKind proves the exported rule is the one the
// ladder runs, not a copy kept next to it: checkBlastRadius calls RefusalKind
// and names neither refusal constant itself.
//
// Falsify: inline the classification back into checkBlastRadius.
func TestBlastRadiusCheckUsesRefusalKind(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ladder.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ladder.go: %v", err)
	}
	var check *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "checkBlastRadius" {
			check = fd
		}
	}
	if check == nil {
		t.Fatal("ladder.go has no checkBlastRadius")
	}
	calls, inline := false, false
	ast.Inspect(check.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "RefusalKind" {
				calls = true
			}
		case *ast.SelectorExpr:
			if x.Sel.Name == "OutcomeRefusedGanged" || x.Sel.Name == "OutcomeRefusedPolicy" {
				inline = true
			}
		}
		return true
	})
	if !calls {
		t.Error("checkBlastRadius does not call RefusalKind")
	}
	if inline {
		t.Error("checkBlastRadius names a refusal outcome directly; the classification " +
			"must come from RefusalKind so every writer of the label agrees")
	}
}

// TestPolicyPermits pins the order of farm.jobs.disruption_policy and the
// fail-closed reading of a value the ladder does not know.
//
// Falsify: make policyRank's default arm return 2, and an unrecognised policy
// starts permitting a port power cycle.
func TestPolicyPermits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		policy, required string
		want             bool
	}{
		{"no_disruption", "no_disruption", true},
		{"no_disruption", "allow_soft_reset", false},
		{"no_disruption", "allow_port_power_cycle", false},
		{"allow_soft_reset", "no_disruption", true},
		{"allow_soft_reset", "allow_soft_reset", true},
		{"allow_soft_reset", "allow_port_power_cycle", false},
		{"allow_port_power_cycle", "no_disruption", true},
		{"allow_port_power_cycle", "allow_soft_reset", true},
		{"allow_port_power_cycle", "allow_port_power_cycle", true},
		// A policy this package has never heard of ranks with no_disruption:
		// it permits an observe rung and nothing above it.
		{"allow_everything", "no_disruption", true},
		{"allow_everything", "allow_soft_reset", false},
		{"", "allow_port_power_cycle", false},
	}
	for _, tc := range cases {
		if got := PolicyPermits(tc.policy, tc.required); got != tc.want {
			t.Errorf("PolicyPermits(%q, %q) = %v, want %v", tc.policy, tc.required, got, tc.want)
		}
	}
}
