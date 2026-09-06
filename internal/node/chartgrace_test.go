package node

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// nodeDaemonSet is the chart template that runs this agent in a cluster. The
// path is relative to this package, which is where `go test` runs.
const nodeDaemonSet = "../../deploy/helm/device-farmer/templates/node.yaml"

// TestNodeDaemonSetGraceCoversTheAgentsOwnDrain holds two files together: the
// budgets this package spends on the way out, and the number the Helm chart
// refuses to render a terminationGracePeriodSeconds below.
//
// They are one fact stated twice. The kubelet SIGKILLs at the end of the grace
// period, and what a kill costs here is a port's power: [Agent.opBudget] is the
// window an in-flight VBUS cycle gets to turn the power back ON, and
// chargeGateShutdownBudget is the window every port the charge policy is
// holding dark gets to be released. They run side by side and take the same
// hardware mutex, so the sum is what the pod must be allowed to spend.
//
// None of the four terms behind opBudget is settable from the environment, so
// the only way the chart's number can go stale is a constant in this package
// changing — which is precisely the change that would leave a farm's phones
// dark on the next node drain, with the chart still reporting a legal value.
//
// Falsify: raise DefaultOpGrace by a second, or edit the number in the
// template. Either one fails this and nothing else.
func TestNodeDaemonSetGraceCoversTheAgentsOwnDrain(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(nodeDaemonSet)
	if err != nil {
		// A checkout without deploy/ is not a farm with a stale guard.
		t.Skipf("cannot read %s: %v", nodeDaemonSet, err)
	}
	m := regexp.MustCompile(`\{\{-\s*\$drain\s*:=\s*([0-9]+)\s*-\}\}`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer declares $drain, so nothing stops a node DaemonSet from "+
			"being rendered with a grace period shorter than this agent's own shutdown",
			nodeDaemonSet)
	}
	chart, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("$drain in %s is %q: %v", nodeDaemonSet, m[1], err)
	}

	// The real path: New applies the defaults a role gets when nothing sets
	// these, which is every deployment, because no environment variable
	// reaches them.
	want := int((testAgent(t, testHost).opBudget() + chargeGateShutdownBudget) / time.Second)
	if chart != want {
		t.Errorf("%s refuses below %d seconds and this agent's drain is %d "+
			"(opBudget + chargeGateShutdownBudget); the chart would admit a grace period "+
			"that lets the kubelet kill the agent mid-restore, and the ports it was "+
			"holding dark stay dark until a human runs uhubctl",
			nodeDaemonSet, chart, want)
	}
}
