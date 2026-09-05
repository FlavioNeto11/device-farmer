package main

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// TestNewRegistryBuilds is the test this package did not have, and the reason
// it now does.
//
// obs.TestEveryCollectorGroupIsRegistered reads newRegistry's SOURCE and checks
// that every package's Collectors() is named in it. That is a good test of the
// list and no test at all of the function: for a while the process and Go
// collectors were registered twice — once near the top and once at the bottom —
// and every role panicked at startup with "duplicate metrics collector
// registration attempted". go build, go vet and the whole suite stayed green,
// because nothing anywhere CALLED newRegistry.
//
// Windows hid half of it: collectors.NewProcessCollector describes nothing off
// Linux, so on this machine only the Go collector collided. The panic was found
// by running the shipped binary on Linux. A test that calls the function is
// cheaper than a virtual machine.
//
// Falsify: change either Register call in newRegistry back to MustRegister and
// register the same collector twice; this test panics rather than fails, which
// is exactly the startup a role would have.
func TestNewRegistryBuilds(t *testing.T) {
	t.Parallel()

	reg, err := newRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	if reg == nil {
		t.Fatal("newRegistry returned no registry")
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("the registry gathered nothing; every role would serve an empty /metrics")
	}

	// The two the binary owns. go_goroutines exists on every platform;
	// process_* does not — NewProcessCollector describes nothing off Linux —
	// so asserting it unconditionally would fail here for a correct build.
	// What must hold everywhere is that the Go collector is present and that
	// the farm's own counters came with it.
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !names[want] {
			t.Errorf("the registry has no %q: this process is scrapeable and its own "+
				"runtime is not, which is the difference between \"the reaper is quiet "+
				"because nothing is reclaimable\" and \"the reaper is wedged\"", want)
		}
	}

	// A farm counter from a package that is not obs, proving RegisterAll's
	// variadic groups actually arrived. This is the failure that shipped once:
	// RegisterAll called with no groups at all, so nine packages' counters were
	// incremented at runtime and reachable from no registry.
	var farm int
	for name := range names {
		if strings.HasPrefix(name, "farm_") {
			farm++
		}
	}
	if farm < 20 {
		t.Errorf("only %d farm_* metric families are registered; the package groups "+
			"are not reaching the registry", farm)
	}
}

// TestNewRegistryIsIndependentPerCall guards the property `farmd all` depends
// on: two roles in one process each build a registry, and neither may poison
// the other.
//
// Falsify: register the runtime collectors on prometheus.DefaultRegisterer
// instead of on reg; the second call then fails or duplicates.
func TestNewRegistryIsIndependentPerCall(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := newRegistry(log)
	if err != nil {
		t.Fatalf("first newRegistry: %v", err)
	}
	second, err := newRegistry(log)
	if err != nil {
		t.Fatalf("second newRegistry: %v", err)
	}
	if first == second {
		t.Fatal("newRegistry returned the same registry twice; two roles in one " +
			"process would double-count every counter in it")
	}

	a, err := first.Gather()
	if err != nil {
		t.Fatalf("gather first: %v", err)
	}
	b, err := second.Gather()
	if err != nil {
		t.Fatalf("gather second: %v", err)
	}
	if len(a) != len(b) {
		t.Errorf("the two registries hold different metric families (%d vs %d); "+
			"whichever role is built second gets a different /metrics", len(a), len(b))
	}
}

// TestNewRegistryDoesNotPanicOnADuplicate is the specific regression.
//
// A collector already present must be tolerated, not fatal. The registry a role
// gets is worth less than the role, and this file's whole argument is that a
// metrics fault must never stop a control plane.
func TestNewRegistryDoesNotPanicOnADuplicate(t *testing.T) {
	t.Parallel()

	reg, err := newRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	// Registering the Go collector again is exactly what the duplicate site
	// did. It must come back as AlreadyRegisteredError, which the caller
	// tolerates, and never as a panic.
	err = reg.Register(collectors.NewGoCollector())
	if err == nil {
		t.Fatal("re-registering the Go collector was accepted; the registry is not " +
			"the one newRegistry populated")
	}
	var dup prometheus.AlreadyRegisteredError
	if !errors.As(err, &dup) {
		t.Fatalf("re-registering gave %v, want AlreadyRegisteredError", err)
	}
}
