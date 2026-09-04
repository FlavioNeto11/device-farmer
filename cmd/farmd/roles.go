package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/api"
	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/ctl"
	"github.com/flaviopadilha/device-farmer/internal/demo"
	"github.com/flaviopadilha/device-farmer/internal/enroll"
	"github.com/flaviopadilha/device-farmer/internal/jobrunner"
	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/node"
	"github.com/flaviopadilha/device-farmer/internal/obs"
	"github.com/flaviopadilha/device-farmer/internal/reaper"
	"github.com/flaviopadilha/device-farmer/internal/recovery"
	"github.com/flaviopadilha/device-farmer/internal/runner"
	"github.com/flaviopadilha/device-farmer/internal/scheduler"
	"github.com/flaviopadilha/device-farmer/internal/topo"
	"github.com/flaviopadilha/device-farmer/internal/ui"
	"github.com/flaviopadilha/device-farmer/internal/watchdog"
)

// newLogger builds the structured logger every role shares.
func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
}

// openPool connects and verifies the connection before the role starts, so a
// bad DSN fails at startup rather than on the first request.
func openPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if cfg.DBMaxConns > 0 {
		pc.MaxConns = cfg.DBMaxConns
	}

	dialCtx, cancel := context.WithTimeout(ctx, cfg.DBConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(dialCtx, pc)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(dialCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// newRegistry builds a metrics registry with the farm collectors installed.
//
// Every package's collectors are registered in every role, not just the ones
// this role will run. Prometheus counters exist from registration, so a series
// that is registered and never touched exports a zero — and an alert written
// over a series that does not exist yields no result and therefore never
// fires. farm_jobrunner_release_errors_total must be armed from the first
// scrape of a fresh process, not from the first casualty. The unused ones cost
// one zero-valued line each.
//
// internal/api registers its own request series into whatever registry it is
// handed, so they are absent here.
//
// A collector the registry refuses is skipped with a WARN rather than being
// fatal. Two packages currently declare farm_recovery_attempts_total with
// different label sets — internal/obs with the physical position, internal
// /recovery without — and only one of them can win a registry. That
// disagreement is worth fixing where it lives; it is not worth refusing to
// start a scheduler over.
func newRegistry(log *slog.Logger) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	if err := obs.Register(reg); err != nil {
		return nil, fmt.Errorf("register metrics: %w", err)
	}
	collectors := [][]prometheus.Collector{
		adbwire.Collectors(),
		enroll.Collectors(),
		jobrunner.Collectors(),
		node.Collectors(),
		reaper.Collectors(),
		recovery.Collectors(),
		scheduler.Collectors(),
		topo.Collectors(),
		watchdog.Collectors(),
	}
	for _, set := range collectors {
		for _, c := range set {
			err := reg.Register(c)
			if err == nil {
				continue
			}
			var dup prometheus.AlreadyRegisteredError
			if errors.As(err, &dup) {
				continue
			}
			log.Warn("a metric could not be registered and will not be exported",
				"err", err,
				"consequence", "an alert written over that series will never fire")
		}
	}
	return reg, nil
}

// withMetrics runs a role with a /metrics listener beside it on
// FARM_METRICS_ADDR.
//
// Only the api role served metrics before this, off its own listener, so every
// other role — the scheduler that allocates, the reaper that is the sole
// automatic release path, the jobrunner that holds the leases — exported
// nothing at all. farm_lease_reaped_total{reason="holder_expired"} and
// farm_lease_renew_failures_total{kind="fenced"} are the two series that mean
// work was destroyed, and they are incremented by exactly those roles.
//
// The API keeps /metrics on its own listener too. Serving the same registry on
// two addresses costs nothing and lets one scrape configuration name one port
// for every role in the farm.
//
// The listener is bound BEFORE the role starts, so a port that is already
// taken is a startup failure naming FARM_METRICS_ADDR rather than a role that
// comes up healthy and exports nothing. A listener that dies later is only
// logged: an already-running control plane must not be torn down because a
// scrape endpoint went away, since tearing it down is what stops leases being
// renewed.
func withMetrics(ctx context.Context, cfg *config.Config, log *slog.Logger, reg *prometheus.Registry, role func(context.Context) error) error {
	addr, want := cfg.MetricsListenAddr()
	if !want {
		// Either metrics are switched off, or this role's API server is
		// already publishing the same registry at the same address. Which of
		// the two it is belongs in the startup summary, not in a second line
		// here.
		return role(ctx)
	}

	srv, ln, err := newMetricsServer(addr, reg)
	if err != nil {
		return err
	}
	log.Info("metrics listening", "addr", ln.Addr().String(), "path", "/metrics")

	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the metrics listener stopped; this process is still doing its work "+
				"but is now invisible to scrapes", "err", err, "addr", addr)
		}
	}()
	defer func() {
		// The parent context is already cancelled by the time this runs, so the
		// shutdown deadline is taken from the process's grace rather than from
		// a context that has none left.
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
		defer cancel()
		_ = srv.Shutdown(stop)
		<-served
	}()

	return role(ctx)
}

// newMetricsServer builds the metrics listener. Split out of withMetrics so a
// test can bind it without running a role.
func newMetricsServer(addr string, reg *prometheus.Registry) (*http.Server, net.Listener, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry:          reg,
		ErrorHandling:     promhttp.ContinueOnError,
		EnableOpenMetrics: true,
	}))
	// A liveness endpoint that answers without touching Postgres. A role whose
	// database is unreachable is unhealthy, but it is NOT a role that should be
	// restarted: restarting it is how a control-plane outage becomes a
	// mass-reclaim once the gap refund stops being written.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("%s=%s: listen: %w (set it to a free port, "+
			"or to %q to serve no metrics endpoint)",
			config.EnvMetricsAddr, addr, err, config.MetricsOff)
	}
	return &http.Server{
		Handler: mux,
		// This listener is exposed in the same sense the API's is, and a
		// half-open connection that never sends a header must not hold a slot
		// forever.
		ReadHeaderTimeout: 10 * time.Second,
	}, ln, nil
}

// runGroup runs every function until the context ends, and returns the first
// non-context error. It cancels its siblings on the first real failure, since
// a control plane running with its scheduler dead and its API alive is a farm
// that looks healthy and allocates nothing.
func runGroup(ctx context.Context, fns map[string]func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for name, fn := range fns {
		wg.Add(1)
		go func(name string, fn func(context.Context) error) {
			defer wg.Done()
			err := fn(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				mu.Lock()
				if first == nil {
					first = fmt.Errorf("%s: %w", name, err)
				}
				mu.Unlock()
				cancel()
			}
		}(name, fn)
	}
	wg.Wait()
	return first
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

func runAPI(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, reg *prometheus.Registry) error {
	// The server reads cfg.Component for its heartbeat. Inside `all` and `demo`
	// that field names the PROCESS ("all", "demo"), so the API was beating
	// under a key farm.reaper_arm does not watch: its outage was invisible to
	// gap accounting while every other component's was refunded. A copy is
	// taken rather than mutating the shared config, because five other roles in
	// this process are reading the same struct.
	if name := cfg.ComponentFor("api"); name != cfg.Component {
		dup := *cfg
		dup.Component = name
		cfg = &dup
	}

	opts := []api.Option{
		api.WithLogger(log),
		api.WithRegistry(reg),
		// The dashboard ships inside the binary and is mounted at "/", so a
		// farm has an operator interface with nothing else deployed.
		api.WithUI(ui.Handler()),
	}

	// The artifact API needs a blob backend, which is a deployment choice the
	// server has no business making, so it is contributed rather than built in.
	if store, aerr := openArtifacts(cfg, pool); aerr != nil {
		log.Warn("artifact endpoints are disabled", "err", aerr,
			"consequence", "jobs with push or install steps will fail at the first artifact")
	} else {
		opts = append(opts, api.WithRoutes(func(srv *api.Server, mux *http.ServeMux) {
			a, aerr := api.NewArtifactAPI(srv, store)
			if aerr != nil {
				log.Warn("artifact endpoints are disabled", "err", aerr)
				return
			}
			a.Register(mux)
		}))
	}
	// Authentication is a deployment decision. Absent a token spec the server
	// runs open and says so loudly rather than quietly.
	opts = append(opts, api.WithAuthenticator(api.NewAllowAll(log, "anonymous")))

	srv, err := api.New(cfg, pool, opts...)
	if err != nil {
		return err
	}
	log.Info("api listening", "addr", cfg.APIAddr, "ui", "/")
	return srv.Run(ctx)
}

func runScheduler(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	// A fresh instance id per process start. It is what farm.lease_renew
	// matches on, so a restarted scheduler is a different holder instance even
	// on the same host with the same pod name — which is exactly the point:
	// holder identity must not survive a process it did not survive.
	inst, err := lease.NewHolderInstance()
	if err != nil {
		return fmt.Errorf("mint holder instance: %w", err)
	}

	s, err := scheduler.New(scheduler.Config{
		Pool:           pool,
		Store:          lease.NewStore(pool),
		Component:      cfg.ComponentFor("scheduler"),
		Holder:         hostname(),
		HolderInstance: inst,
		SlotRearm:      cfg.Lease.SlotRearm,
		Logger:         log,
	})
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

func runReaper(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	r, err := reaper.New(reaper.Config{
		Pool:       pool,
		Store:      lease.NewStore(pool),
		Component:  cfg.ComponentFor("reaper"),
		Interval:   cfg.Reaper.Interval,
		Batch:      cfg.Reaper.Batch,
		GapFloor:   cfg.Reaper.GapFloor,
		Components: cfg.Reaper.Components,
		Rearm:      cfg.Lease.SlotRearm,
		Logger:     log,
	})
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

func runRecovery(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	// No host agent is configured here, so tiers 3 and 4 are refused with a
	// reason naming what is missing rather than silently skipped.
	l, err := recovery.New(recovery.Config{
		Pool:      pool,
		Actuator:  recovery.NewADBActuator(log, nil),
		Component: cfg.ComponentFor("recovery"),
		Logger:    log,
	})
	if err != nil {
		return err
	}
	return l.Run(ctx)
}

// watchdogsForHosts starts one watchdog per registered host. Health is
// per-host because the ADB server is per-host: one stream, one epoch, one
// blast radius.
func watchdogsForHosts(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) (map[string]func(context.Context) error, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, adb_endpoint FROM farm.hosts WHERE admin_state <> 'disabled' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]func(context.Context) error)
	for rows.Next() {
		var id, endpoint string
		if err := rows.Scan(&id, &endpoint); err != nil {
			return nil, err
		}
		cfgCopy := watchdog.Config{
			Pool:        pool,
			Component:   cfg.ComponentFor("watchdog") + ":" + id,
			HostID:      id,
			ADBEndpoint: endpoint,
			Interval:    cfg.WatchdogInterval,
			Logger:      log.With("host", id),
		}
		out["watchdog:"+id] = func(ctx context.Context) error {
			w, err := watchdog.New(cfgCopy)
			if err != nil {
				return err
			}
			return w.Run(ctx)
		}
	}
	return out, rows.Err()
}

func runWatchdog(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	// A single-host watchdog is the production shape: one pod per host, named
	// by FARM_HOST_ID. Without one, supervise every host in this process.
	if cfg.Node.HostID != "" {
		w, err := watchdog.New(watchdog.Config{
			Pool:        pool,
			Component:   cfg.ComponentFor("watchdog") + ":" + cfg.Node.HostID,
			HostID:      cfg.Node.HostID,
			ADBEndpoint: cfg.Node.ADBEndpoint,
			Interval:    cfg.WatchdogInterval,
			Logger:      log,
		})
		if err != nil {
			return err
		}
		return w.Run(ctx)
	}

	fns, err := watchdogsForHosts(ctx, cfg, log, pool)
	if err != nil {
		return err
	}
	if len(fns) == 0 {
		return errors.New("no hosts registered; set FARM_HOST_ID or seed farm.hosts")
	}
	log.Info("supervising every registered host", "hosts", len(fns))
	return runGroup(ctx, fns)
}

// runAll runs the whole control plane in one process. It is the right shape
// for a laptop, a smoke test and a single-node farm, and the wrong shape for
// production, where each role scales and fails independently.
func runAll(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, reg *prometheus.Registry) error {
	fns := map[string]func(context.Context) error{
		"api":       func(c context.Context) error { return runAPI(c, cfg, log, pool, reg) },
		"scheduler": func(c context.Context) error { return runScheduler(c, cfg, log, pool) },
		"reaper":    func(c context.Context) error { return runReaper(c, cfg, log, pool) },
		"recovery":  func(c context.Context) error { return runRecovery(c, cfg, log, pool) },
		// Without this the scheduler allocates devices and nothing ever runs on
		// them: jobs sit in 'running' forever holding a phone each.
		"jobrunner": func(c context.Context) error { return runJobRunner(c, cfg, log, pool) },
	}
	wds, err := watchdogsForHosts(ctx, cfg, log, pool)
	if err != nil {
		return err
	}
	for k, v := range wds {
		fns[k] = v
	}
	log.Info("running the full control plane in one process", "roles", len(fns))
	return runGroup(ctx, fns)
}

// runDemo seeds a farm of simulated hardware, starts fake ADB servers, and
// runs the REAL control plane against them.
//
// The fake is only the hardware. The scheduler, the lease store, the reaper
// and the recovery ladder are the production code paths, because a demo that
// stubs the logic proves nothing about the logic.
func runDemo(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, reg *prometheus.Registry, hosts, devices int) error {
	fmt.Fprintf(os.Stderr, `
  device-farmer demo — SIMULATED HARDWARE
  ---------------------------------------
  %d hosts and %d devices are in-process fake ADB servers. No phone is
  attached and nothing here touches real hardware.

  Everything above the wire is real: the scheduler allocates through
  farm.lease_acquire, holders renew through farm.lease_renew, and the reaper
  is the same one that runs in production.

  Watch for the line "device dropped offline mid-lease". The lease must NOT
  end. That is the whole point of the system, and it is the one thing STF
  issue #663 gets wrong.

  Dashboard: http://%s/
`, hosts, devices, cfg.APIAddr)

	// The simulation owns the hardware and the seed; the control plane runs
	// beside it against the same database.
	fns := map[string]func(context.Context) error{
		"demo": func(c context.Context) error {
			return demo.Run(c, pool, demo.Options{
				Hosts:   hosts,
				Devices: devices,
				Logger:  log.With("component", "demo"),
			})
		},
		"api":       func(c context.Context) error { return runAPI(c, cfg, log, pool, reg) },
		"scheduler": func(c context.Context) error { return runScheduler(c, cfg, log, pool) },
		"reaper":    func(c context.Context) error { return runReaper(c, cfg, log, pool) },
		"recovery":  func(c context.Context) error { return runRecovery(c, cfg, log, pool) },
		"jobrunner": func(c context.Context) error { return runJobRunner(c, cfg, log, pool) },
	}

	// Watchdogs are started after the simulation has registered its hosts, so
	// they find real endpoints to stream from.
	fns["watchdogs"] = func(c context.Context) error {
		if err := waitForHosts(c, pool, hosts); err != nil {
			return err
		}
		wds, err := watchdogsForHosts(c, cfg, log, pool)
		if err != nil {
			return err
		}
		if len(wds) == 0 {
			return errors.New("demo registered no hosts")
		}
		return runGroup(c, wds)
	}

	return runGroup(ctx, fns)
}

// waitForHosts blocks until the simulation has registered its ADB endpoints.
func waitForHosts(ctx context.Context, pool *pgxpool.Pool, want int) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM farm.hosts WHERE adb_endpoint <> ''`).Scan(&n); err == nil && n >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// openArtifacts builds the content-addressed artifact store. The blob backend
// is a local directory here; the Backend interface is the seam for object
// storage, and nothing above it knows the difference.
func openArtifacts(cfg *config.Config, pool *pgxpool.Pool) (*artifacts.Store, error) {
	root := os.Getenv("FARM_ARTIFACT_DIR")
	if root == "" {
		root = filepath.Join(os.TempDir(), "device-farmer-artifacts")
	}
	backend, err := artifacts.NewDirBackend(root)
	if err != nil {
		return nil, fmt.Errorf("artifact backend at %s: %w", root, err)
	}
	return artifacts.NewStore(pool, backend)
}

// runJobRunner executes jobs on the devices the scheduler allocated. It is the
// component that turns a lease into work actually happening on a phone.
func runJobRunner(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	inst, err := lease.NewHolderInstance()
	if err != nil {
		return fmt.Errorf("mint holder instance: %w", err)
	}

	store, err := openArtifacts(cfg, pool)
	if err != nil {
		return err
	}

	exec, err := runner.New(runner.Config{
		Pool:      pool,
		Logger:    log,
		Artifacts: store,
	})
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}

	jr, err := jobrunner.New(jobrunner.Config{
		Pool:           pool,
		Store:          lease.NewStore(pool),
		Runner:         exec,
		Component:      cfg.ComponentFor("jobrunner"),
		Holder:         hostname(),
		HolderInstance: inst,
		// One ADB client per device connection. The client is a dialer rather
		// than a connection, so this is cheap and keeps each job's transport
		// failures to itself.
		Dial: func(endpoint, devpath string) (runner.Conn, error) {
			return jobrunner.NewDeviceConn(adbwire.New(endpoint), devpath)
		},
		// The renewal cadence is how a holder proves it is alive, so it is the
		// one timing knob that is load bearing on the invariant. Without this
		// field the loop fell back to lease.DefaultRenewInterval and
		// FARM_LEASE_RENEW_INTERVAL changed nothing — including in the
		// direction that matters: an operator who raised the lease TTL and
		// slowed renewal to match got the old fast cadence, and one who
		// LOWERED the TTL toward the floor got a cadence config had checked
		// against a TTL the holder was not using. The startup assertion that
		// three renewals must fit inside one TTL was being made about a number
		// with no destination.
		HolderConfig: lease.HolderConfig{
			Interval: cfg.Lease.RenewInterval,
			// Caps consecutive witness-only extensions. It reaches
			// farm.lease_witness as p_max_extensions — for whenever something
			// in this build starts producing a witness; nothing does yet.
			WitnessMaxExtensions: cfg.Lease.MaxWitnessExtensions,
		},
		SlotRearm: cfg.Lease.SlotRearm,
		Logger:    log,
	})
	if err != nil {
		return err
	}
	return jr.Run(ctx)
}

// runNode is the agent that lives on a bare-metal device host, beside the USB
// hubs. It is the only component with access to /dev/bus/usb and to hub power,
// which is why recovery tiers 3 and 4 are refused when it is absent rather
// than silently reported as ineffective.
func runNode(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	hostID := cfg.Node.HostID
	if hostID == "" {
		return errors.New("FARM_HOST_ID is required: a node agent speaks for exactly one " +
			"physical host, and guessing which one would attach a rack of phones to the wrong row")
	}

	src, err := topo.Sysfs("/sys")
	if err != nil {
		return fmt.Errorf("read the USB tree: %w "+
			"(a node agent must run on the Linux host the devices are plugged into)", err)
	}
	disco, err := topo.New(topo.Config{
		Pool:   pool,
		HostID: hostID,
		Source: src,
		Actor:  "farmd-node",
		Logger: log.With("component", "topo"),
	})
	if err != nil {
		return fmt.Errorf("topology discovery: %w", err)
	}

	enr, err := enroll.New(enroll.Config{
		Pool:        pool,
		Component:   "enroll",
		HostID:      hostID,
		ADBEndpoint: cfg.Node.ADBEndpoint,
		Connect: func(endpoint string) (enroll.Host, error) {
			return adbwire.New(endpoint), nil
		},
		Logger: log.With("component", "enroll"),
	})
	if err != nil {
		return fmt.Errorf("enrollment: %w", err)
	}

	agent, err := node.New(node.Config{
		Pool:         pool,
		HostID:       hostID,
		ADBEndpoint:  cfg.Node.ADBEndpoint,
		Component:    "node",
		AgentVersion: version,
		// Discovery must run before enrollment: a device at a USB position with
		// no registered slot cannot be resolved, only recorded as pending.
		Discover: func(c context.Context) error {
			_, derr := disco.Once(c)
			return derr
		},
		Enroll: func(c context.Context) error {
			_, err := enr.EnrollOnce(c)
			return err
		},
		Addr:   cfg.NodeAddr,
		Logger: log,
	})
	if err != nil {
		return err
	}
	return agent.Run(ctx)
}

func runCtl(ctx context.Context, cfg *config.Config, args []string, stdout, stderr io.Writer) int {
	return ctl.Main(ctx, cfg, args, stdout, stderr)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "farmd"
	}
	return h
}
