// Package api is device-farmer's HTTP surface: the fleet grid, the topology,
// the job and lease protocol, the operator actions, and the event stream the
// dashboard renders.
//
// # What this package may and may not do
//
// A lease is ended by the job, by a deadline the user wrote down, or by a
// human. Nothing else. This package therefore contains exactly three code
// paths that end a lease, and each one is one of those three things:
//
//   - POST /api/v1/leases/{id}/release — the holder says the job is over, and
//     names one of the seven reasons farm.leases.release_reason permits.
//   - POST /api/v1/jobs/{id}/cancel — the job is cancelled, which releases its
//     lease with reason 'job_cancelled'.
//   - POST /api/v1/leases/{id}/revoke — a human takes the device back, audited,
//     with their name on the row.
//
// Nothing else here may release a lease, and in particular:
//
//   - a failed ADB exec does not,
//   - a device that reads unhealthy in farm.v_fleet does not,
//   - a client disconnecting mid-request does not,
//   - SIGTERM does not. Shutdown drains in-flight requests and stops. The
//     replacement process re-attaches at the same fence; a control plane that
//     "cleaned up" its leases on the way out would convert every rolling
//     deploy into destroyed work, which is DeviceFarmer/STF issue #663 with a
//     Kubernetes trigger instead of a socket one.
//
// # The renewal path is the hot path
//
// POST /api/v1/leases/{id}/renew is the only request in this API whose failure
// costs a device. It answers one question — does the holder still exist — and
// it distinguishes two outcomes that must never be conflated: 410 with code
// "fenced" (terminal; abort) and 503 with code "transient" (retry; the lease is
// untouched). Everything else in this package exists to not get in its way:
// the HTTP server has no write timeout that could cut it short, bulk work runs
// on its own goroutines rather than in the request path, and the SSE poller
// stops querying entirely when nobody is watching.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// Tuning constants that are not worth an environment variable.
const (
	// maxRequestBody bounds any JSON request body. Job specs are the largest
	// legitimate payload and are nowhere near this.
	maxRequestBody = 1 << 20

	// defaultStreamInterval is how often the SSE poller re-reads the database
	// while at least one client is connected. Two seconds is fast enough that
	// a fleet grid feels live and slow enough that a wall-mounted dashboard is
	// not a load generator.
	defaultStreamInterval = 2 * time.Second

	// defaultExecTimeout and maxExecTimeout bound POST /devices/{id}/exec.
	defaultExecTimeout = 30 * time.Second
	maxExecTimeout     = 5 * time.Minute

	// defaultExecMaxOutput caps captured command output. adbwire truncates
	// rather than failing, so a chatty command does not look like a broken
	// device.
	defaultExecMaxOutput = 256 << 10

	// readinessTimeout bounds the database probe behind /readyz. It is short
	// on purpose: readiness answers "should traffic come here", and a probe
	// that hangs is a probe that fails open.
	readinessTimeout = 2 * time.Second
)

// Executor runs one shell command against one physical position. It is the
// narrow slice of *adbwire.Client this package uses, extracted so exec can be
// exercised against test/fakeadb without a real ADB server.
//
// Note the signature: it takes a devpath, never a serial. Duplicate OEM serials
// are real, and addressing by serial would let an operator's exec land on a
// healthy device holding somebody's six-hour lease.
type Executor interface {
	Shell(ctx context.Context, devpath, command string) (*adbwire.ShellResult, error)
}

// ExecutorFactory builds an Executor for one host's ADB endpoint.
type ExecutorFactory func(endpoint string, timeout time.Duration, maxOutput int) Executor

// defaultExecutorFactory dials the host's own ADB server.
func defaultExecutorFactory(endpoint string, timeout time.Duration, maxOutput int) Executor {
	return adbwire.New(endpoint,
		adbwire.WithCallTimeout(timeout),
		adbwire.WithMaxOutput(maxOutput))
}

// Server is the API. It is safe for concurrent use once constructed, and its
// zero value is not usable — call New.
type Server struct {
	// extraRoutes are mount functions contributed by the parent via WithRoutes.
	extraRoutes []func(*Server, *http.ServeMux)

	cfg    *config.Config
	pool   *pgxpool.Pool
	leases *lease.Store
	auth   Authenticator
	log    *slog.Logger

	reg     *prometheus.Registry
	metrics *httpMetrics

	stream         *streamHub
	streamInterval time.Duration

	newExecutor   ExecutorFactory
	execMaxOutput int

	uiMu sync.RWMutex
	ui   http.Handler

	handler     http.Handler
	handlerOnce sync.Once

	httpSrv *http.Server

	// bgCtx outlives individual requests and dies only at the end of Shutdown.
	// Work that must survive the request that started it — a bulk run, an audit
	// row for a client that hung up — hangs off this, never off r.Context().
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup

	startedAt time.Time
}

// Option configures a Server.
type Option func(*Server)

// WithAuthenticator supplies the authenticator. Without it, New refuses to
// build a server: a control plane that can revoke leases must never default to
// letting everyone in, and AllowAll has to be an explicit choice.
func WithAuthenticator(a Authenticator) Option {
	return func(s *Server) {
		if a != nil {
			s.auth = a
		}
	}
}

// WithLogger supplies the logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.log = l
		}
	}
}

// WithRegistry supplies the Prometheus registry to publish on /metrics.
//
// When the caller owns the registry it is assumed to have registered the
// process and runtime collectors itself; this package registers only what it
// and its dependencies define, and tolerates a collector the caller already
// registered.
func WithRegistry(r *prometheus.Registry) Option {
	return func(s *Server) {
		if r != nil {
			s.reg = r
		}
	}
}

// WithRoutes contributes extra routes to the server's mux at build time.
//
// It exists because some subsystems need a dependency the server has no
// business choosing — the artifact API needs a blob backend, and a control
// plane should not be deciding for its operator whether that is a directory or
// a bucket. The parent builds the subsystem and hands over a mount function.
//
// Mounts run before the UI's catch-all "/" is installed, and Go's ServeMux
// prefers the more specific pattern regardless, so a contributed route cannot
// be shadowed by the dashboard.
func WithRoutes(mount func(*Server, *http.ServeMux)) Option {
	return func(s *Server) {
		if mount != nil {
			s.extraRoutes = append(s.extraRoutes, mount)
		}
	}
}

// WithUI mounts the dashboard handler at "/". The parent owns the embedded
// assets, so the API only holds the hook.
func WithUI(h http.Handler) Option {
	return func(s *Server) { s.ui = h }
}

// WithStreamInterval sets the SSE poll period.
func WithStreamInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.streamInterval = d
		}
	}
}

// WithExecutorFactory replaces the ADB client factory used by
// POST /api/v1/devices/{id}/exec.
func WithExecutorFactory(f ExecutorFactory) Option {
	return func(s *Server) {
		if f != nil {
			s.newExecutor = f
		}
	}
}

// New builds a Server over an existing pool.
//
// The pool must be sized so the renewal path is never starved by dashboard
// queries: a holder that cannot borrow a connection for TTL+grace loses its
// device. config.Config.DBMaxConns is the knob.
func New(cfg *config.Config, pool *pgxpool.Pool, opts ...Option) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("api: nil config")
	}
	if pool == nil {
		return nil, errors.New("api: nil database pool")
	}

	s := &Server{
		cfg:            cfg,
		pool:           pool,
		leases:         lease.NewStore(pool),
		log:            slog.Default(),
		streamInterval: defaultStreamInterval,
		newExecutor:    defaultExecutorFactory,
		execMaxOutput:  defaultExecMaxOutput,
		startedAt:      time.Now(),
	}
	for _, o := range opts {
		o(s)
	}
	if s.auth == nil {
		return nil, fmt.Errorf("api: no authenticator: pass WithAuthenticator(NewStaticBearer(...)), "+
			"or WithAuthenticator(NewAllowAll(...)) to disable authentication deliberately "+
			"(see %s)", EnvAuthMode)
	}

	ownRegistry := s.reg == nil
	if ownRegistry {
		s.reg = prometheus.NewRegistry()
	}
	if err := s.registerMetrics(ownRegistry); err != nil {
		return nil, err
	}

	s.stream = newStreamHub(s.log, s.metrics)
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	return s, nil
}

// Registry exposes the registry backing /metrics, for a parent that wants to
// add its own collectors.
func (s *Server) Registry() *prometheus.Registry { return s.reg }

// SetUI mounts the dashboard after construction, for a parent that builds the
// UI handler later than it builds the server. Safe to call while serving.
func (s *Server) SetUI(h http.Handler) {
	s.uiMu.Lock()
	s.ui = h
	s.uiMu.Unlock()
}

// uiHandler returns the currently mounted dashboard, or nil.
func (s *Server) uiHandler() http.Handler {
	s.uiMu.RLock()
	defer s.uiMu.RUnlock()
	return s.ui
}

// httpMetrics are this package's own series. The lease and device metrics live
// in internal/obs and are published here only because this process owns the
// registry; nothing in this file may invent a second definition of them.
type httpMetrics struct {
	requests         *prometheus.CounterVec
	duration         *prometheus.HistogramVec
	inFlight         prometheus.Gauge
	streamClients    prometheus.Gauge
	streamDropped    prometheus.Counter
	streamPollErrors prometheus.Counter
	execs            *prometheus.CounterVec
	operatorActions  *prometheus.CounterVec
	bulkTargets      *prometheus.CounterVec
}

func (s *Server) registerMetrics(ownRegistry bool) error {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "farm", Subsystem: "api", Name: "requests_total",
			Help: "HTTP requests served, by route pattern, method and status class.",
		}, []string{"route", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "farm", Subsystem: "api", Name: "request_duration_seconds",
			Help:    "HTTP request latency by route pattern.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "farm", Subsystem: "api", Name: "requests_in_flight",
			Help: "Requests currently being served, excluding event streams.",
		}),
		streamClients: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "farm", Subsystem: "api", Name: "stream_clients",
			Help: "Connected Server-Sent Events clients.",
		}),
		streamDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "farm", Subsystem: "api", Name: "stream_clients_dropped_total",
			Help: "Event-stream clients disconnected for not keeping up.",
		}),
		streamPollErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "farm", Subsystem: "api", Name: "stream_poll_errors_total",
			Help: "Failed database polls in the event-stream loop.",
		}),
		execs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "farm", Subsystem: "api", Name: "device_execs_total",
			Help: "Operator shell commands run against a device, by outcome.",
		}, []string{"outcome"}),
		operatorActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "farm", Subsystem: "api", Name: "operator_actions_total",
			Help: "Audited operator actions, by action and outcome.",
		}, []string{"action", "outcome"}),
		bulkTargets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "farm", Subsystem: "api", Name: "bulk_targets_total",
			Help: "Bulk execution targets completed, by outcome.",
		}, []string{"outcome"}),
	}

	own := []prometheus.Collector{
		m.requests, m.duration, m.inFlight, m.streamClients,
		m.streamDropped, m.streamPollErrors, m.execs, m.operatorActions, m.bulkTargets,
	}
	for _, c := range own {
		if err := register(s.reg, c); err != nil {
			return fmt.Errorf("api: register metrics: %w", err)
		}
	}

	// Only publish somebody else's collectors into a registry we created. When
	// the parent passed one in, it decides what else lives there — and a
	// duplicate registration of the lease metrics would be an error, not a
	// merge.
	if ownRegistry {
		if err := obs.Register(s.reg); err != nil {
			var dup prometheus.AlreadyRegisteredError
			if !errors.As(err, &dup) {
				return fmt.Errorf("api: register farm metrics: %w", err)
			}
		}
		for _, c := range adbwire.Collectors() {
			if err := register(s.reg, c); err != nil {
				return fmt.Errorf("api: register adbwire metrics: %w", err)
			}
		}
	}

	s.metrics = m
	return nil
}

// register is Registry.Register with "already there" treated as success, so a
// parent that registered a shared collector first does not break startup.
func register(r *prometheus.Registry, c prometheus.Collector) error {
	err := r.Register(c)
	if err == nil {
		return nil
	}
	var dup prometheus.AlreadyRegisteredError
	if errors.As(err, &dup) {
		return nil
	}
	return err
}

// Run serves until ctx is cancelled, then shuts down gracefully.
//
// Cancelling ctx means "stop accepting new work and finish what is in flight".
// It does not mean "release the leases this process is holding", and there is
// no code path from here that could make it mean that.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.APIAddr)
	if err != nil {
		return fmt.Errorf("api: listen on %s: %w", s.cfg.APIAddr, err)
	}

	s.httpSrv = &http.Server{
		Handler: s.Handler(),
		// BaseContext is deliberately NOT ctx. If it were, cancelling the run
		// context would cancel every in-flight request at the same instant —
		// including a renewal that was one round trip from succeeding — and a
		// rolling deploy would start costing devices. bgCtx is cancelled only
		// after the drain has finished.
		BaseContext:       func(net.Listener) context.Context { return s.bgCtx },
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: GET /api/v1/stream is a long-lived response, and a
		// write deadline would sever the dashboard every N seconds. Slow-loris
		// protection comes from ReadHeaderTimeout and IdleTimeout instead.
		IdleTimeout: 120 * time.Second,
		ErrorLog:    slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	s.log.Info("api listening",
		"addr", ln.Addr().String(),
		"component", s.cfg.Component,
		"authenticator", s.auth.Name(),
		"ui_mounted", s.uiHandler() != nil)
	if s.auth.Name() == "allow-all" {
		s.log.Warn("AUTHENTICATION IS DISABLED on this listener: every caller is an operator")
	}

	// The workers are cancelled when Run returns, however it returns. Deriving
	// them from ctx alone leaks both goroutines when Serve fails on its own —
	// a closed listener, an accept error — because ctx is still live and
	// nothing else would ever tell the heartbeat to stop. A heartbeat that
	// outlives its server is worse than none: it keeps farm.reaper_arm seeing
	// a healthy API while no renewal can actually reach one.
	runCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()

	s.bg.Add(2)
	go func() { defer s.bg.Done(); s.heartbeat(runCtx) }()
	go func() { defer s.bg.Done(); s.runStream(runCtx) }()

	errCh := make(chan error, 1)
	go func() {
		err := s.httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		s.bgCancel()
		return err
	case <-ctx.Done():
	}

	grace := s.cfg.ShutdownGrace
	if grace <= 0 {
		grace = 30 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := s.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}

// Shutdown drains in-flight requests and stops.
//
// IT RELEASES NOTHING. Every lease this process was serving keeps its fence and
// its deadline, and the replacement process re-attaches to it through
// farm.lease_acquire's phase 1. If no replacement ever arrives, the lease ends
// the way every abandoned lease ends: heartbeats stop, the suspect sweep marks
// it, and farm.lease_reclaim takes it back behind a control-plane-gap refund
// and a quiesce gate. A local shutdown hook has neither of those protections,
// which is exactly why it does not get to make this decision.
func (s *Server) Shutdown(ctx context.Context) error {
	// Event streams never end on their own, so they are closed first. Otherwise
	// http.Shutdown would wait out the entire grace period on connections that
	// are healthy and idle by design, and real in-flight requests would be cut
	// off when it expired.
	s.stream.closeAll()

	var err error
	if s.httpSrv != nil {
		err = s.httpSrv.Shutdown(ctx)
	}

	// Background work (bulk runs) is asked to stop and then waited for, bounded
	// by whatever is left of the grace period.
	s.bgCancel()
	done := make(chan struct{})
	go func() { s.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("shutdown grace expired with background work still running; " +
			"no lease was released")
	}
	return err
}

// heartbeat writes farm.component_beat for this component.
//
// This is not liveness decoration. farm.reaper_arm computes the control-plane
// gap across every component on the renewal path, and refunds that gap to every
// live lease. If the API stops beating while the reaper and Postgres stay
// healthy, the reaper sees no gap, and after TTL+grace it reclaims every
// unprotected lease in the farm — the mass reclaim BLOCKER 8 in
// migrations/00002_lease.sql exists to prevent. A missed beat is therefore
// logged at WARN, and the recovery is logged too so the gap is visible in both
// directions.
func (s *Server) heartbeat(ctx context.Context) {
	every := s.cfg.Reaper.HeartbeatInterval
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()

	failing := false
	beat := func() {
		// Bounded independently of ctx: a beat that hangs until shutdown is a
		// beat that never lands.
		beatCtx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		if err := s.leases.ComponentBeat(beatCtx, s.cfg.Component); err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				s.log.Warn("component heartbeat failed; a control-plane gap is now accruing",
					"component", s.cfg.Component, "err", err)
				failing = true
			}
			return
		}
		if failing {
			s.log.Info("component heartbeat recovered", "component", s.cfg.Component)
			failing = false
		}
	}

	beat()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}

// handleHealthz is liveness.
//
// It deliberately does not touch the database. Liveness answers "should this
// process be killed and restarted", and killing the API because Postgres
// hiccuped would widen the very control-plane gap that farm.reaper_arm has to
// refund. Readiness is where the database belongs.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"component": s.cfg.Component,
		"uptime_s":  int64(time.Since(s.startedAt).Seconds()),
	})
}

// handleReadyz is readiness: can this instance serve requests that need the
// database.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		s.log.WarnContext(r.Context(), "readiness probe failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"the database is not reachable from this instance", nil)
		return
	}
	stat := s.pool.Stat()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ready",
		"component": s.cfg.Component,
		"database":  "reachable",
		"pool": map[string]int32{
			"acquired":     stat.AcquiredConns(),
			"idle":         stat.IdleConns(),
			"total":        stat.TotalConns(),
			"max":          stat.MaxConns(),
			"constructing": stat.ConstructingConns(),
		},
	})
}

// metricsHandler serves the registry.
func (s *Server) metricsHandler() http.Handler {
	return promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{
		Registry:          s.reg,
		ErrorHandling:     promhttp.ContinueOnError,
		EnableOpenMetrics: true,
	})
}
