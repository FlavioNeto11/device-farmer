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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/api"
	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/ctl"
	"github.com/flaviopadilha/device-farmer/internal/demo"
	"github.com/flaviopadilha/device-farmer/internal/enroll"
	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
	"github.com/flaviopadilha/device-farmer/internal/janitor"
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
//
// With FARM_DB_ROLE set, every physical connection executes SET ROLE before
// the pool hands it out. That is what puts the role firewall of
// migrations/00002_lease.sql in force for the whole process rather than for
// the three functions that carry their own SET: a reaper started as
// farm_reaper cannot read farm.device_runtime from ANY statement it runs, not
// only from inside farm.lease_reclaim.
//
// SET ROLE is a session setting, so it survives every Acquire and Release on
// that connection; AfterConnect runs once per connection and that is enough.
// AfterRelease is deliberately NOT used to RESET ROLE. The reset would undo
// the one SET the connection ever received, so from its second acquisition on
// the connection would run as the login user — a firewall in force for
// exactly one acquisition per connection. Re-issuing SET ROLE in BeforeAcquire
// would repair that at a round trip per acquire and buy nothing: every
// acquirer in this process is the same role, so there is nobody to reset
// between. One SET per connection, kept for its life, is the whole mechanism.
//
// A refused SET ROLE — the login user is not a member of the role, which is
// the grant migration 00015 makes — surfaces from Ping and the role does not
// start. Starting anyway would mean running as the login user while the
// startup summary says otherwise.
func openPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if cfg.DBMaxConns > 0 {
		pc.MaxConns = cfg.DBMaxConns
	}
	if cfg.DBRole != "" {
		stmt := "SET ROLE " + pgx.Identifier{cfg.DBRole}.Sanitize()
		pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s refused — the login user must be a member of %s, "+
					"which migration 00015 grants: %w", stmt, cfg.DBRole, err)
			}
			return nil
		}
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
// Every package that measures something is listed here. obs cannot fetch these
// itself — six of the ten import it, so it has to stay a leaf (see
// obs.RegisterAll) — which means this call site is the only thing standing
// between a package's counters and /metrics. It was called with no groups at
// all for long enough that nine of the ten sets were incremented at runtime and
// reachable from no registry: every lease, scheduling and recovery counter
// outside obs read as a loop that had never run. obs.TestEveryCollectorGroupIsRegistered
// fails if a new package's Collectors() is ever left off this list.
//
// The list is not conditioned on the role, and one consequence is worth
// knowing before reading a graph. /metrics is served by the api role alone
// (internal/api/router.go), so in the split deployment in docker-compose.yml
// the api process now publishes the loop counters at a permanent 0 while the
// processes that increment them still expose no endpoint. Inside 'all' and
// 'demo' — one process running every loop, which is what this binary is for —
// the numbers are real. Making them real in the split shape needs a metrics
// listener in the loop roles, not a shorter list here: trimming the list would
// only put those packages back where they were, measured by nobody.
func newRegistry(log *slog.Logger) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()

	// A metric naming collision must not take down a control plane. Every
	// counter this fails to register is a graph an operator loses; every
	// lease it would have protected is one it does not. Log it and carry on.
	if err := obs.RegisterAll(reg, log,
		adbwire.Collectors(),
		enroll.Collectors(),
		fenceproxy.Collectors(),
		janitor.Collectors(),
		jobrunner.Collectors(),
		node.Collectors(),
		reaper.Collectors(),
		recovery.Collectors(),
		scheduler.Collectors(),
		topo.Collectors(),
		watchdog.Collectors(),
	); err != nil {
		log.Error("some metrics could not be registered; the control plane is "+
			"running but /metrics is incomplete", "err", err)
	}

	// The process and Go runtime collectors are the binary's decision, not
	// obs's: a library that registers them makes two roles in one process
	// collide over them.
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(collectors.NewGoCollector())
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

	// Job step and attempt visibility. Without these an operator cannot see
	// which step a job is on, what it printed, or which devices it has already
	// failed on — and that last one is how a job problem is told apart from a
	// device problem.
	opts = append(opts, api.WithRoutes(func(srv *api.Server, mux *http.ServeMux) {
		js, jerr := api.NewJobStepsAPI(srv)
		if jerr != nil {
			log.Error("job step endpoints are disabled", "err", jerr)
			return
		}
		js.Register(mux)
	}))

	// The artifact API needs a blob backend, which is a deployment choice the
	// server has no business making, so it is contributed rather than built in.
	if store, backend, aerr := openArtifacts(cfg, pool); aerr != nil {
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
			// Artifact bytes. Without this a farm can store a 200MB APK it
			// cannot hand back, and the blob store is the only copy.
			a.RegisterBlobRoutes(mux)
			// Reclaiming the bytes. DELETE removes the row and keeps the blob
			// by design, so without this route the directory only grows and
			// the sweep that exists to empty it has no caller. It runs only
			// when an operator asks, and dry by default.
			gc, gerr := api.NewBlobGC(srv, backend, api.WithBlobGCGrace(cfg.ArtifactGCGrace))
			if gerr != nil {
				log.Warn("artifact GC endpoint is disabled", "err", gerr,
					"consequence", "disk under the blob root is reclaimable only by hand")
				return
			}
			gc.Register(mux)
		}))
	}
	// The reaper's kill switch. It is mounted here rather than in the router's
	// own table because farm.reaper_state governs the reclaim loop, and a farm
	// running without a reaper still has to be able to read the switch and say
	// why it moved.
	opts = append(opts, api.WithRoutes(func(srv *api.Server, mux *http.ServeMux) {
		srv.RegisterReaperAdmin(mux)
	}))

	// Authentication is a deployment decision, and it is decided by
	// configuration rather than by this line.
	//
	// This used to hardcode AllowAll, so FARM_API_TOKENS was read by
	// internal/config and then discarded: every deployment was open and no
	// setting could close it. AuthenticatorFor now refuses to start an
	// exposed listener with no credentials, which is safe with respect to
	// the invariant in a way that booting open is not — an api that will not
	// start goes stale in farm.component_heartbeat, farm.reaper_arm records
	// that as a control-plane gap, and the gap is REFUNDED onto every live
	// lease. Refusing to boot costs a tenant nothing; booting open costs
	// them whatever the first stranger who finds the port decides to revoke.
	auth, err := api.AuthenticatorFor(cfg, log)
	if err != nil {
		return err
	}
	opts = append(opts, api.WithAuthenticator(auth))

	// POST /slots/{id}/power performs its VBUS cycle through the same node
	// client the recovery ladder uses, resolved per host from
	// farm.hosts.node_endpoint. Without the token the route answers 503 and
	// records nothing, which is what a farm with no host agent can honestly
	// say; it must not open an attempt row that nothing will ever close.
	// cfg.Node.Token, not os.Getenv: U12 moved this into the config so
	// Summary() reports whether it is set (never its value) and so every role
	// reads one parsed value rather than the environment at three call sites.
	if token := cfg.Node.Token; token == "" {
		log.Warn("no FARM_NODE_TOKEN set, so operator slot power cycles are unavailable",
			"consequence", "POST /api/v1/slots/{id}/power answers 503 on every slot, "+
				"and a port that needs its VBUS cut has to be cycled by hand",
			"fix", "set FARM_NODE_TOKEN to the same value the farmd node agents use")
	} else {
		c, cerr := node.NewClient(node.ClientConfig{
			Resolver: node.NewDBResolver(pool),
			Token:    token,
			Logger:   log.With("component", "node-client"),
		})
		if cerr != nil {
			return fmt.Errorf("node client: %w", cerr)
		}
		opts = append(opts, api.WithHostRunner(c))
	}

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

// runJanitor closes rows whose process died: a step left "running" by an
// evicted jobrunner, its attempt, a bulk target from a dead run.
//
// It closes ORPHANS ONLY, and that distinction is the whole difficulty. A step
// is an orphan when its lease is no longer live — never merely because it has
// been running a long time. A six-hour shell_detached step is exactly what
// this system is for, and sweeping it for looking stale would be #663 wearing
// a different hat.
func runJanitor(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	j, err := janitor.New(janitor.Config{
		Pool:      pool,
		Component: cfg.ComponentFor("janitor"),
		Logger:    log,
	})
	if err != nil {
		return err
	}
	return j.Run(ctx)
}

func runRecovery(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	// The two rungs ADB cannot perform — a USB device reset and a VBUS power
	// cycle — happen on the physical machine, so the ladder drives them
	// through each host's farmd-node agent.
	//
	// This used to pass a nil HostRunner, which meant tiers 3 and 4 were
	// refused on every deployment forever with "no host agent is configured
	// for this farm". That refusal was honest and unfixable: nothing in the
	// tree spoke to a node agent, and farm.hosts had no column saying where
	// one listens.
	//
	// A farm with no agents still works. The client resolves each host's
	// endpoint per call, and a host with node_endpoint NULL produces the same
	// honest refusal as before — for that host only, rather than fleet-wide.
	var runner recovery.HostRunner
	token := cfg.Node.Token
	if token == "" {
		log.Warn("no "+config.EnvNodeToken+" set, so recovery tiers 3 and 4 stay refused",
			"consequence", "a device that needs a USB reset or a port power cycle "+
				"will climb to quarantine instead of being recovered",
			"fix", "set "+config.EnvNodeToken+" to the same value the farmd node agents use")
	} else {
		c, cerr := node.NewClient(node.ClientConfig{
			Resolver: node.NewDBResolver(pool),
			Token:    token,
			Logger:   log.With("component", "node-client"),
		})
		if cerr != nil {
			return fmt.Errorf("node client: %w", cerr)
		}
		runner = c
	}

	l, err := recovery.New(recovery.Config{
		Pool:      pool,
		Actuator:  recovery.NewADBActuator(log, runner),
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
		"janitor":   func(c context.Context) error { return runJanitor(c, cfg, log, pool) },
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
				Hosts:            hosts,
				Devices:          devices,
				Logger:           log.With("component", "demo"),
				ReaperComponents: cfg.Reaper.Components,
			})
		},
		"api":       func(c context.Context) error { return runAPI(c, cfg, log, pool, reg) },
		"scheduler": func(c context.Context) error { return runScheduler(c, cfg, log, pool) },
		"reaper":    func(c context.Context) error { return runReaper(c, cfg, log, pool) },
		"recovery":  func(c context.Context) error { return runRecovery(c, cfg, log, pool) },
		"jobrunner": func(c context.Context) error { return runJobRunner(c, cfg, log, pool) },
		"janitor":   func(c context.Context) error { return runJanitor(c, cfg, log, pool) },
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
//
// The backend is returned beside the store because the sweep that reclaims
// disk needs to enumerate what the backend holds, which the Store's seam
// deliberately does not offer.
func openArtifacts(cfg *config.Config, pool *pgxpool.Pool) (*artifacts.Store, *artifacts.DirBackend, error) {
	root := os.Getenv("FARM_ARTIFACT_DIR")
	if root == "" {
		root = filepath.Join(os.TempDir(), "device-farmer-artifacts")
	}
	backend, err := artifacts.NewDirBackend(root)
	if err != nil {
		return nil, nil, fmt.Errorf("artifact backend at %s: %w", root, err)
	}
	store, err := artifacts.NewStore(pool, backend)
	if err != nil {
		return nil, nil, err
	}
	return store, backend, nil
}

// runJobRunner executes jobs on the devices the scheduler allocated. It is the
// component that turns a lease into work actually happening on a phone.
func runJobRunner(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	inst, err := lease.NewHolderInstance()
	if err != nil {
		return fmt.Errorf("mint holder instance: %w", err)
	}

	store, _, err := openArtifacts(cfg, pool)
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
			// farm.lease_witness as p_max_extensions on every witness the
			// loop below presents.
			WitnessMaxExtensions: cfg.Lease.MaxWitnessExtensions,
		},
		// The witness cadence, validated against the grace band and the floor
		// at startup and consumed here: the jobrunner starts one witness loop
		// per placement, fed by the marker it keeps fresh on the job's device.
		// This is the second half of the #663 countermeasure — a job that
		// can still touch its device keeps its lease through a control-plane
		// outage longer than ttl+grace — and until this field was passed no
		// role started it.
		//
		// Only the interval is set. The marker cadence and the evidence
		// window are derived from it inside the jobrunner by the rule config
		// owns (config.MarkersPerWitnessTick, config.EvidenceWindow), which
		// is the same rule the startup summary printed a moment ago.
		WitnessConfig: lease.WitnessConfig{Interval: cfg.Lease.WitnessInterval},
		SlotRearm:     cfg.Lease.SlotRearm,
		Logger:        log,
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

	// The naming map is read here and judged by topo.New below, both before
	// the first pass: a collision in it is a mistake in a file, and a file
	// should fail the process that was given it, not every scan after.
	overrides, err := topo.LoadOverrides(cfg.Topo.OverridesPath)
	if err != nil {
		return fmt.Errorf("%s: %w", config.EnvTopoOverrides, err)
	}
	src, err := topo.Sysfs(cfg.Topo.SysfsRoot)
	if err != nil {
		return fmt.Errorf("read the USB tree: %w "+
			"(a node agent must run on the Linux host the devices are plugged into)", err)
	}
	disco, err := topo.New(topo.Config{
		Pool:      pool,
		HostID:    hostID,
		Source:    src,
		Overrides: overrides,
		Filter: topo.HubFilter{
			Include:         cfg.Topo.Include,
			Exclude:         cfg.Topo.Exclude,
			MinPorts:        cfg.Topo.MinPorts,
			AdoptEmpty:      cfg.Topo.AdoptEmpty,
			IncludeRootHubs: cfg.Topo.IncludeRootHubs,
		},
		Actor: "farmd-node",
		// Off unless the manifest says otherwise. See config.Topo for why the
		// default must stay off: the first pass against a hand-seeded host
		// would otherwise retire every slot on it.
		RetireVanished:    cfg.Topo.RetireVanished,
		MaxRetireFraction: cfg.Topo.MaxRetireFraction,
		DryRun:            cfg.Topo.DryRun,
		// The node agent paces discovery through Discover below and never
		// calls Discoverer.Run, so Interval reaches nothing here; it is set so
		// the two intervals cannot disagree if that ever changes.
		Interval:    cfg.Topo.Interval,
		CallTimeout: cfg.Topo.CallTimeout,
		Logger:      log.With("component", "topo"),
	})
	if err != nil {
		return fmt.Errorf("topology discovery: %w", err)
	}

	enr, err := enroll.New(enroll.Config{
		Pool:        pool,
		Component:   cfg.ComponentFor("enroll"),
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
		Pool:        pool,
		HostID:      hostID,
		ADBEndpoint: cfg.Node.ADBEndpoint,
		// Per-host, exactly like the watchdog component above:
		// farm.component_heartbeat is keyed by component, so a constant "node"
		// would have every host agent in the farm overwriting one row and
		// keeping it fresh for the hosts that are dead.
		Component:    node.DefaultComponentPrefix + hostID,
		AgentVersion: version,
		// Discovery must run before enrollment: a device at a USB position with
		// no registered slot cannot be resolved, only recorded as pending.
		Discover: func(c context.Context) error {
			_, derr := disco.Once(c)
			return derr
		},
		// One knob for both: this is the interval the agent ticks on, and the
		// budget it gives each pass.
		DiscoverInterval: cfg.Topo.Interval,
		// Run, not EnrollOnce. node.EnrollFunc is a continuous job: a function
		// that performs one pass and returns is read as a fault, restarted on a
		// doubling backoff, and warned about every cycle — so a healthy farm
		// would enroll less and less often, and a phone plugged in at 02:00
		// would wait out the worst-case delay. Run also owns enrollment's own
		// interval and logs each cycle's summary, which the one-pass call
		// discarded along with it.
		Enroll: enr.Run,
		Addr:   cfg.NodeAddr,
		// node.New refuses an HTTP surface with no credential, which is
		// right: an unauthenticated endpoint that power-cycles USB ports is
		// worse than no endpoint. Serving nothing is a legitimate deployment
		// — discovery and enrollment still run — so an empty Addr needs no
		// token, and a non-empty one needs this.
		Token:  cfg.Node.Token,
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
