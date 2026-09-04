// Command farmd is the entire device-farmer control plane in one static
// binary. Every role — api, scheduler, reaper, watchdog, node, ctl — is a
// subcommand of this program rather than its own image.
//
// That is a deliberate choice, not a packaging shortcut. The roles disagree
// about exactly one thing, the lease, and the cheapest way to keep them
// agreeing is to make it impossible to deploy a reaper built from a different
// commit than the scheduler it races with. One binary, one schema version, one
// set of lease constants, nine ways to start it.
//
// # Shutdown
//
// SIGTERM and SIGINT cancel the root context. Cancellation means "stop
// accepting new work and finish what is in flight". It does NOT mean "release
// the leases this process holds", and no code reachable from this file may
// make it mean that.
//
// The reason is the whole thesis of the project. In Kubernetes a SIGTERM is
// the most ordinary event there is: a node drain, a rolling update, a spot
// preemption, an OOM restart, a HPA scale-down. None of those is evidence that
// the job on the phone died. A pod that "cleans up after itself" by releasing
// its leases on the way out converts routine cluster churn into destroyed
// work — which is exactly the shape of DeviceFarmer/STF issue #663, where a
// transport-level event ends a six-hour run.
//
// So the correct behaviour on shutdown is to let the lease keep running. The
// replacement pod calls farm.lease_acquire with the same job_id and gets the
// same lease, the same device, and the SAME FENCE back (Phase 1 of
// lease_acquire in migrations/00002_lease.sql). The fence is not bumped
// precisely because the job's own work may still be attached to the device;
// bumping it would fence out its own process.
//
// If the replacement never arrives, the lease ends the way every lease ends
// without a holder: heartbeats stop, the suspect sweep marks it, the grace
// band elapses, and farm.lease_reclaim — the only automatic release path in
// the system — takes it back. That path has a control-plane gap refund and a
// quiesce gate in front of it. A local os.Exit does not.
//
// A second signal restores the default disposition, so an impatient operator
// pressing Ctrl-C twice gets an immediate kill.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// Build metadata, set with -ldflags "-X main.version=... -X main.commit=...".
var (
	version   = "dev"
	commit    = ""
	buildDate = ""
)

// Exit codes. They are distinct so a supervisor, a CI step, or a human reading
// a CrashLoopBackOff can tell "you typed it wrong" from "it broke" from "that
// role does not exist yet".
const (
	exitOK             = 0
	exitFailure        = 1
	exitUsage          = 2
	exitNotImplemented = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	role, rest := args[0], args[1:]

	// These answer without touching the environment or the database, so they
	// work in a scratch container and in a broken deployment.
	switch role {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "version", "-v", "--version":
		printVersion(stdout)
		return exitOK
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A plain signal channel rather than signal.NotifyContext: the shutdown
	// banner must fire on an actual signal, not merely on the root context
	// ending, or every ordinary exit claims to have been evicted.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case s := <-sigs:
			fmt.Fprintf(stderr, "farmd: %v received; draining in-flight work. "+
				"Live leases are intentionally NOT released — the replacement "+
				"re-attaches at the same fence.\n", s)
			cancel()
			// Hand the signals back to the runtime. The first one asked for a
			// graceful stop; a second one means the operator has stopped asking.
			signal.Stop(sigs)
		case <-ctx.Done():
		}
	}()

	var err error
	switch role {
	case "migrate":
		err = runMigrate(ctx, rest, stdout, stderr)
	case "api", "scheduler", "reaper", "watchdog", "recovery",
		"jobrunner", "node", "all", "demo":
		err = runRole(ctx, role, rest, stderr)
	case "ctl":
		// ctl talks to the API over HTTP and never to the database, so it needs
		// no DSN and must keep working when the control plane is the thing
		// being investigated.
		cfg, cerr := config.Load("ctl", config.WithoutDatabase())
		if cerr != nil {
			fmt.Fprintln(stderr, "Configuration preflight FAILED.")
			return exitFailure
		}
		return runCtl(ctx, cfg, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "farmd: unknown role %q\n\n", role)
		usage(stderr)
		return exitUsage
	}

	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, errNotImplemented):
		return exitNotImplemented
	case errors.Is(err, context.Canceled):
		// A signal during a long operation is an orderly stop, but it did not
		// finish, so it is not a success either.
		fmt.Fprintln(stderr, "farmd: interrupted before completion")
		return exitFailure
	default:
		fmt.Fprintf(stderr, "farmd %s: %v\n", role, err)
		return exitFailure
	}
}

// runRole loads configuration, opens the database, and dispatches to the
// role's runner. Configuration and connectivity are verified BEFORE the role
// starts, so a bad manifest fails at rollout rather than at the first request.
func runRole(ctx context.Context, role string, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet(role, flag.ContinueOnError)
	fs.SetOutput(stderr)
	hosts := fs.Int("hosts", 2, "demo only: simulated ADB hosts")
	devices := fs.Int("devices", 56, "demo only: simulated devices across those hosts")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	cfg, err := config.Load(role)
	if err != nil {
		fmt.Fprintln(stderr, "Configuration preflight FAILED.")
		return err
	}
	log := newLogger(cfg, stderr)

	pool, err := openPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	// Closing the pool ends our connections. It does NOT end any lease: the
	// only automatic release path is farm.lease_reclaim, which lives behind a
	// grace band, a control-plane gap refund and a quiesce gate.
	defer pool.Close()

	reg, err := newRegistry(log)
	if err != nil {
		return err
	}

	switch role {
	case "api":
		return runAPI(ctx, cfg, log, pool, reg)
	case "scheduler":
		return runScheduler(ctx, cfg, log, pool)
	case "reaper":
		return runReaper(ctx, cfg, log, pool)
	case "recovery":
		return runRecovery(ctx, cfg, log, pool)
	case "watchdog":
		return runWatchdog(ctx, cfg, log, pool)
	case "jobrunner":
		return runJobRunner(ctx, cfg, log, pool)
	case "node":
		return runNode(ctx, cfg, log, pool)
	case "all":
		return runAll(ctx, cfg, log, pool, reg)
	case "demo":
		return runDemo(ctx, cfg, log, pool, reg, *hosts, *devices)
	}
	return fmt.Errorf("unreachable role %q", role)
}

// errNotImplemented marks a role that exists in the CLI surface but has no
// implementation compiled in yet.
var errNotImplemented = errors.New("not yet implemented")

// notImplemented refuses loudly. A role that silently no-ops is worse than a
// missing binary: the pod goes Ready, the readiness probe passes, and the farm
// looks healthy while nothing renews a lease.
//
// It still runs the configuration preflight first, so that a manifest with a
// bad lease/fence relationship is rejected the day it is written rather than
// the day the role starts working.
//
// A failed preflight is reported as a FAILURE (exit 1), not as
// not-implemented (exit 3). The two codes exist to be told apart: a CI gate or
// a rollout script that tolerates "this role is not built yet" must not also
// tolerate "your slot rearm is shorter than the node's self-fence timeout",
// which is a manifest that will hand one phone to two jobs the moment the role
// does ship.
func notImplemented(role string, stderr io.Writer, load func(string, ...config.Option) (*config.Config, error)) error {
	fmt.Fprintf(stderr, "farmd %s: %s in this build.\n\n", role, errNotImplemented)
	fmt.Fprintf(stderr,
		"The %q role is part of farmd's committed surface, but its implementation is\n"+
			"not wired into this binary. Exiting %d so a supervisor restarts and a deploy\n"+
			"fails visibly, instead of leaving a process that looks alive and does nothing.\n\n",
		role, exitNotImplemented)

	cfg, err := load(role)
	if err != nil {
		fmt.Fprintln(stderr, "Configuration preflight FAILED; that is the more urgent problem.")
		return err
	}
	fmt.Fprintf(stderr, "Configuration preflight passed:\n%s\n", indent(cfg.Summary()))
	return errNotImplemented
}

func indent(s string) string {
	out := make([]byte, 0, len(s)+8)
	atLineStart := true
	for i := 0; i < len(s); i++ {
		if atLineStart && s[i] != '\n' {
			out = append(out, ' ', ' ')
		}
		out = append(out, s[i])
		atLineStart = s[i] == '\n'
	}
	return string(out)
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "farmd %s\n", version)
	if commit != "" {
		fmt.Fprintf(w, "commit:  %s\n", commit)
	} else if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				fmt.Fprintf(w, "commit:  %s\n", s.Value)
			}
		}
	}
	if buildDate != "" {
		fmt.Fprintf(w, "built:   %s\n", buildDate)
	}
	fmt.Fprintf(w, "go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `farmd — device-farmer control plane

Usage: farmd <role> [flags]

Roles:
  migrate     apply, roll back, or inspect the database schema
  api         tenant-facing HTTP API; job submission and lease renewal
  scheduler   matches queued jobs to free devices via farm.lease_acquire
  reaper      suspect sweep and the single automatic release path
  watchdog    device health only; it can never touch a lease
  jobrunner   runs job specs on leased devices; re-attaches after an eviction
  recovery    the recovery ladder; acts for a holder that keeps its device
  all         every control-plane role in one process (laptop / single node)
  demo        simulated hardware plus the real control plane; needs no phones
  node        host agent: USB discovery, enrollment, and the hardware rungs
  ctl         operator CLI against the API
  version     build information

Every role reads its configuration from the environment; DATABASE_URL is
required by all of them except ctl. Run "farmd migrate -h" for its flags.

SIGTERM and SIGINT drain in-flight work. They do not release leases: a pod
eviction is not evidence that the job on the phone died, and the replacement
re-attaches to the same lease at the same fence.
`)
}
