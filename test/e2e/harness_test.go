package e2e

// The fixture: a scratch database, simulated hardware, and the roles a scenario
// asked for, running as real processes. Read doc.go first for why any of this
// is a subprocess at all.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/api"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/demo"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// farmdPackage is built by import path rather than by a relative one: `go test`
// runs with the package directory as the working directory, so "./cmd/farmd"
// would resolve to test/e2e/cmd/farmd and there is nothing there.
const farmdPackage = "github.com/flaviopadilha/device-farmer/cmd/farmd"

// setupLockKey serialises scratch-database creation with every OTHER suite in
// this repository. The migration set creates cluster-wide ROLES behind IF NOT
// EXISTS, and two suites migrating at once can both observe a role missing and
// both try to create it. This is the same key internal/runner, internal/reaper
// and the rest take on the admin database, and it must stay the same key: a
// private one would serialise this package against itself and nothing else.
const setupLockKey int64 = 0x64665f74657374 // "df_test"

var (
	// farmdBinary is the shipped binary, built once by TestMain. Empty when
	// DATABASE_URL was unset and every scenario is going to skip.
	farmdBinary string

	// adminDSN is DATABASE_URL: a database that already exists, on a cluster
	// this suite is allowed to CREATE DATABASE on. No scenario ever writes to
	// it; it is only where the scratch databases are created and dropped from.
	adminDSN string

	// farmSeq disambiguates two scratch databases whose tests have names that
	// sanitise to the same thing.
	farmSeq atomic.Int64
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

// runSuite owns the binary's lifetime. It returns an exit code rather than
// calling os.Exit itself, so the deferred cleanup below actually runs.
func runSuite(m *testing.M) (code int) {
	adminDSN = strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
	if adminDSN == "" {
		// No database, no scenarios — and therefore no reason to spend thirty
		// seconds linking a binary nothing will start. Every test in this
		// package skips from newFarm.
		return m.Run()
	}

	dir, err := os.MkdirTemp("", "farmd-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: scratch directory: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	bin := filepath.Join(dir, "farmd")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	// One build for the whole suite. Building per scenario would multiply a
	// linker run by the number of tests for a binary that cannot have changed
	// in between.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, farmdPackage)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		// A failure here is not a skip. A suite that quietly tested nothing
		// while reporting success is worse than one that is red.
		fmt.Fprintf(os.Stderr, "e2e: go build %s: %v\n", farmdPackage, err)
		return 1
	}
	farmdBinary = bin

	// Sweep what a PREVIOUS run could not clean up before starting this one.
	//
	// t.Cleanup does not run when the process does not get to finish: `go test
	// -timeout` kills the binary, and so does Ctrl-C. Every scenario that was
	// in flight then leaves a migrated scratch database behind, and there is no
	// later moment at which that run can tidy up — the only process that will
	// ever be in a position to do it is the next one. Without this the cluster
	// accumulates df_e2e_* databases until somebody notices, and each one
	// holds a full schema.
	//
	// Only this package's own names, only ones old enough that no concurrent
	// run could own them, and failures are reported rather than fatal: a
	// cluster that will not let us drop a stale database is not a reason to
	// refuse to test the farm.
	sweepStaleDatabases()

	return m.Run()
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

// farmOpts describes the farm a scenario wants. The zero value is valid: no
// roles, the default physical tree, no environment overrides.
type farmOpts struct {
	// Roles are the farmd subcommands to start, by name: "api", "scheduler",
	// "reaper", "recovery", "jobrunner", "janitor", "chargepolicy",
	// "watchdog". They start in the order given.
	//
	// A scenario asks for the roles whose behaviour it is about, and no
	// others. Starting the whole control plane everywhere would put three
	// farm-wide sweeps beside every assertion, and then a scenario about the
	// scheduler could fail because the recovery ladder acted.
	Roles []string

	// Seed is handed to demo.Seed. Zero fields fall back to the harness
	// defaults in newFarm, which are smaller than the demo's own.
	Seed demo.SeedOptions

	// Env is added to every role's environment, last, so it wins over
	// everything the harness decided. This is the escape hatch for a scenario
	// that needs a lease TTL, a reaper interval or a witness cadence other
	// than the shipped defaults.
	//
	// The names the harness owns — the two addresses, the database, the
	// component name and the token spec — are REFUSED here rather than
	// applied, because overriding them does not fail, it desynchronises. See
	// envFor, which names each one and what to use instead.
	Env map[string]string

	// Tenants are extra tenant credentials, keyed by the name a scenario will
	// pass to f.get/f.post/f.Ctl, valued by the farm.tenants row each may see.
	//
	// The harness always mints "operator" and "tenant"; the second is scoped
	// to the seeded tenant. A scenario about SCOPE needs two tenants that must
	// not see each other, and a single credential cannot express that — which
	// is why this is a field and not a constant. The tenant rows are created
	// if they do not exist, because farm.jobs will not reference one that does
	// not.
	Tenants map[string]string
}

// farm is one scenario's whole world.
type farm struct {
	t    *testing.T
	opts farmOpts

	dbName  string
	dsn     string
	created bool // the scratch database exists and teardown must drop it
	admin   *pgx.Conn
	pool    *pgxpool.Pool

	seed demo.SeedResult

	// adb holds one fake ADB server per seeded host, keyed by host id.
	adb map[string]*fakeadb.Server

	roles       map[string]*roleProc
	roleOrder   []string
	metricsAddr map[string]string

	apiAddr     string // "127.0.0.1:<port>", empty when no api role was asked for
	apiURL      string
	tokens      map[string]string // "operator" / "tenant" -> bearer token
	tokenSpec   string
	artifactDir string
}

// newFarm builds the fixture: a scratch database, the seeded physical tree,
// one fake ADB server per host, and the requested roles as processes.
//
// It skips the test when DATABASE_URL is unset. Everything it creates is
// registered for teardown before it can fail, so a scenario that dies halfway
// through setup still drops its database and kills its processes.
func newFarm(t *testing.T, opts farmOpts) *farm {
	t.Helper()
	if adminDSN == "" {
		t.Skip("DATABASE_URL is not set; skipping the end-to-end scenarios")
	}

	// Smaller than internal/demo's own defaults (2 hosts x 4 hubs x 7 ports =
	// 56 handsets). Two hosts is what keeps "one ADB server per host" honest;
	// two hubs is what keeps the seeder's faulty hub separable from its clone
	// pair; four ports is the smallest width at which the seeder still plants
	// its degraded pair. Everything past that is watchdog probes a scenario
	// pays for on every cycle.
	if opts.Seed.Hosts == 0 {
		opts.Seed.Hosts = 2
	}
	if opts.Seed.HubsPerHost == 0 {
		opts.Seed.HubsPerHost = 2
	}
	if opts.Seed.SlotsPerHub == 0 {
		opts.Seed.SlotsPerHub = 4
	}

	f := &farm{
		t:           t,
		opts:        opts,
		adb:         make(map[string]*fakeadb.Server),
		roles:       make(map[string]*roleProc),
		metricsAddr: make(map[string]string),
		tokens: map[string]string{
			"operator": "e2e-operator-" + randomish(),
			"tenant":   "e2e-tenant-" + randomish(),
		},
		artifactDir: t.TempDir(),
	}
	t.Cleanup(f.teardown)

	ctx := t.Context()
	f.dbName = scratchName(t)

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connecting to DATABASE_URL to create a scratch database: %v", err)
	}
	f.admin = admin

	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		t.Fatalf("taking the shared setup lock: %v", err)
	}
	_, createErr := admin.Exec(ctx, `CREATE DATABASE `+quoteIdent(f.dbName))
	if createErr == nil {
		// Set before anything else can fail: from here on the database exists,
		// and teardown owns dropping it even if the migration never lands.
		f.created = true
		f.dsn = dsnFor(t, adminDSN, f.dbName)
		createErr = f.migrate(t)
	}
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
		t.Errorf("the shared setup lock was not given back: %v", err)
	}
	if createErr != nil {
		t.Fatalf("preparing the scratch database %s: %v", f.dbName, createErr)
	}

	pc, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatalf("parsing the scratch DSN: %v", err)
	}
	pc.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatalf("connecting to the scratch database: %v", err)
	}
	f.pool = pool
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging the scratch database: %v", err)
	}

	f.seed, err = demo.Seed(ctx, pool, opts.Seed)
	if err != nil {
		t.Fatalf("seeding the farm: %v", err)
	}
	t.Logf("seeded %d hosts, %d hubs, %d slots, %d devices in rack %s (pool %s, tenant %s, queue %s)",
		len(f.seed.Hosts), f.seed.Hubs, f.seed.Slots, f.seed.Devices,
		f.seed.Rack, f.seed.Pool, f.seed.Tenant, f.seed.Queue)

	f.startHardware(t)
	f.startRoles(t)
	return f
}

// migrate applies the schema with the SHIPPED binary and its EMBEDDED
// migration set, rather than with goose in this process. It is one more
// deployment step this suite gets to prove: an image whose migrations went
// missing fails here, in a test, instead of in an init container.
func (f *farm) migrate(t *testing.T) error {
	t.Helper()
	out, errOut, code, err := runBinary(t, 3*time.Minute, cleanEnv(), "migrate", "up", "-dsn", f.dsn)
	if err != nil {
		return fmt.Errorf("farmd migrate up: %w\n%s", err, strings.TrimSpace(errOut))
	}
	if code != 0 {
		// goose names the failing migration on stderr; without it this error
		// says only that the schema is not there.
		return fmt.Errorf("farmd migrate up exited %d:\n%s\n%s", code,
			strings.TrimSpace(out), strings.TrimSpace(errOut))
	}
	t.Logf("migrated %s: %s", f.dbName, strings.TrimSpace(lastLine(out+errOut)))
	return nil
}

// teardown stops everything and drops the database. It is registered before
// the first thing it has to clean up, so every field it touches may be nil.
//
// The order matters: processes first, because a process still holding a
// connection makes DROP DATABASE wait; hardware next, because a role mid-shot
// at a closed listener logs noise nobody asked for; then the pool, then the
// database.
func (f *farm) teardown() {
	t := f.t

	for i := len(f.roleOrder) - 1; i >= 0; i-- {
		if p := f.roles[f.roleOrder[i]]; p != nil {
			p.stop()
		}
	}
	for host, srv := range f.adb {
		if err := srv.Close(); err != nil {
			t.Logf("closing the fake adb server for %s: %v", host, err)
		}
	}
	if f.pool != nil {
		f.pool.Close()
	}

	if f.admin != nil {
		// A fresh context: t.Context() is already cancelled by the time
		// cleanups run, and a cancelled DROP leaves a database behind on the
		// developer's cluster for every scenario that ever ran.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if f.created {
			if _, err := f.admin.Exec(ctx,
				`DROP DATABASE IF EXISTS `+quoteIdent(f.dbName)+` WITH (FORCE)`); err != nil {
				t.Errorf("dropping the scratch database %s: %v", f.dbName, err)
			}
		}
		if err := f.admin.Close(ctx); err != nil {
			t.Logf("closing the admin connection: %v", err)
		}
	}

	// Reported last, so a role that died on its own is named even when the
	// scenario failed for some other reason first — usually it IS the other
	// reason.
	for _, name := range f.roleOrder {
		if p := f.roles[name]; p != nil {
			p.report(t)
		}
	}
}

// ---------------------------------------------------------------------------
// Accessors a scenario uses
// ---------------------------------------------------------------------------

// DB is the pool for this scenario's own database.
func (f *farm) DB() *pgxpool.Pool { return f.pool }

// Seed reports what internal/demo wrote: the host ids, the clone positions, the
// faulty hub, and the pool, tenant and queue every job has to name.
func (f *farm) Seed() demo.SeedResult { return f.seed }

// ADB is the fake ADB server standing in for one host's hardware. Use it to
// make a device drop off the bus, flap, or answer a scripted command; nothing
// it can be made to do may end a lease, which is what makes it worth having.
func (f *farm) ADB(t *testing.T, host string) *fakeadb.Server {
	t.Helper()
	srv, ok := f.adb[host]
	if !ok {
		t.Fatalf("no fake adb server for host %q; this farm seeded %v", host, f.seed.Hosts)
	}
	return srv
}

// API is the base URL of the api role, e.g. "http://127.0.0.1:53312".
func (f *farm) API(t *testing.T) string {
	t.Helper()
	if f.apiURL == "" {
		t.Fatalf("this farm has no api role; add \"api\" to farmOpts.Roles")
	}
	return f.apiURL
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

// roleProc is one farmd process.
type roleProc struct {
	name string
	env  []string
	cmd  *exec.Cmd
	done chan struct{}
	logs sync.WaitGroup

	mu       sync.Mutex
	stopping bool
	exited   bool
	waitErr  error
}

func (f *farm) startRoles(t *testing.T) {
	t.Helper()

	if contains(f.opts.Roles, "api") || contains(f.opts.Roles, "all") || contains(f.opts.Roles, "demo") {
		f.apiAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
		f.apiURL = "http://" + f.apiAddr
	}
	specs := []string{
		fmt.Sprintf("%s:operator:e2e-operator", f.tokens["operator"]),
		fmt.Sprintf("%s:tenant:e2e-tenant:%s", f.tokens["tenant"], f.seed.Tenant),
	}
	for _, name := range sortedKeys(f.opts.Tenants) {
		tenant := f.opts.Tenants[name]
		if name == "operator" || name == "tenant" {
			t.Fatalf("farmOpts.Tenants may not redefine %q, which every scenario relies on; "+
				"pick another name", name)
		}
		f.ensureTenant(t, tenant)
		f.tokens[name] = "e2e-" + name + "-" + randomish()
		specs = append(specs, fmt.Sprintf("%s:tenant:e2e-%s:%s", f.tokens[name], name, tenant))
	}
	f.tokenSpec = strings.Join(specs, ",")

	for _, name := range f.opts.Roles {
		f.metricsAddr[name] = fmt.Sprintf("127.0.0.1:%d", freePort(t))
		f.roles[name] = &roleProc{name: name, env: f.envFor(name)}
		f.roleOrder = append(f.roleOrder, name)
		f.StartRole(t, name)
	}
}

// waitReady blocks until a role is actually serving.
//
// Every role binds its own listener on FARM_METRICS_ADDR, and cmd/farmd binds
// it BEFORE the role's loop starts — after the configuration has been
// validated and the database connected. So one /healthz there is a uniform
// readiness signal even for the roles that have no HTTP surface of their own,
// and it is why StartRole can promise that the role is up when it returns.
// Without that promise every scenario would open with its own sleep.
func (f *farm) waitReady(t *testing.T, name string) {
	t.Helper()
	if addr := f.metricsAddr[name]; addr != "" && !strings.EqualFold(addr, config.MetricsOff) {
		f.Eventually(t, 90*time.Second, "the "+name+" role to answer on its own listener at "+addr,
			func() error {
				if err := httpProbe(t, "http://"+addr+"/healthz"); err != nil {
					// The hint belongs on the failure, not in every success
					// line: this is the wait a misconfigured role dies in, and
					// the dial error alone does not say which of the three
					// things went wrong.
					return fmt.Errorf("%w — cmd/farmd binds this listener before the role starts "+
						"work, so a role that never answers here refused its configuration, could "+
						"not reach the database, or lost a race for the port", err)
				}
				return nil
			})
	}
	if f.apiURL != "" && (name == "api" || name == "all" || name == "demo") {
		f.Eventually(t, 90*time.Second, "the "+name+" role to serve the API at "+f.apiAddr,
			func() error { return httpProbe(t, f.apiURL+"/healthz") })
	}
}

// httpProbe is one GET that reports failure as an error instead of failing the
// test, so a wait loop can use it.
func httpProbe(t *testing.T, target string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s = %d", target, res.StatusCode)
	}
	return nil
}

// envFor builds one role's environment.
//
// It starts from the OS environment with every FARM_* name and DATABASE_URL
// stripped, so a developer's shell — or the very DATABASE_URL this suite reads
// to find the cluster — cannot reach into a role and change what the scenario
// is testing. Everything the role sees is decided here or by farmOpts.Env.
func (f *farm) envFor(role string) []string {
	env := map[string]string{
		config.EnvDatabaseURL: f.dsn,

		// The heartbeat key is the role's own name. The harness deliberately
		// does not invent per-process names: FARM_COMPONENT renames the row a
		// role writes to farm.component_heartbeat, and farm.reaper_arm reads
		// gap accounting out of exactly those rows, so a renamed component
		// that is not also renamed in FARM_REAPER_COMPONENTS is refused at
		// startup — or, worse, watched under a name nothing writes.
		config.EnvComponent: role,
		config.EnvLogLevel:  "info",

		// Every role gets its own listener on a port the harness chose. A
		// hard-coded 9090 collides with whatever the developer already runs,
		// and two roles in one scenario would collide with each other.
		config.EnvMetricsAddr: f.metricsAddr[role],

		// Per-farm, so two scenarios cannot see each other's blobs and the
		// artifact GC has a directory it owns.
		"FARM_ARTIFACT_DIR": f.artifactDir,

		config.EnvReaperComponent: strings.Join(f.reaperComponents(), ","),
	}
	if f.apiAddr != "" {
		// Set for every role, not just api: `all` and `demo` serve the API
		// too, and a role that never binds it is unaffected by the value.
		env[config.EnvAPIAddr] = f.apiAddr
		env[config.EnvAPIBaseURL] = f.apiURL
	}
	env[api.EnvAuthTokens] = f.tokenSpec

	// A scenario may set anything EXCEPT what the harness has already told
	// itself. It records the addresses it chose, the database it created and
	// the credentials it minted, and then probes readiness, scrapes /metrics,
	// signs requests and drops the database against those recorded values. An
	// override here does not fail — it desynchronises: the role comes up
	// perfectly on the address the scenario asked for, and the harness spends
	// ninety seconds probing the one it remembers, then fails naming the role.
	//
	// The one that is not merely confusing is DATABASE_URL. Pointing a role at
	// another farm's database defeats the single thing one-database-per-
	// scenario exists to guarantee, and the damage lands in a scenario that
	// did nothing wrong.
	//
	// So they are refused by name, with the reason and the way to get what was
	// wanted. Everything else — timings, thresholds, knobs under test — is
	// applied last and wins, which is the point of the field.
	reserved := map[string]string{
		config.EnvDatabaseURL: "each scenario gets its own database; pointing a role at " +
			"another one puts a farm-wide sweep into somebody else's fixtures. Seed what " +
			"you need with farmOpts.Seed instead",
		config.EnvAPIAddr:    "the harness chose this port and probes it; use f.API(t)",
		config.EnvAPIBaseURL: "derived from the port the harness chose; use f.API(t)",
		config.EnvMetricsAddr: "the harness chose this port and reads readiness and /metrics " +
			"from it; use f.Metrics(t, role)",
		config.EnvComponent: "each role heartbeats under its own name and " +
			"FARM_REAPER_COMPONENTS is derived from it; renaming one disarms the reaper",
		api.EnvAuthTokens: "the harness mints the credentials f.get/f.post/f.Ctl sign with; " +
			"add to farmOpts.Tokens instead",
	}
	for k, v := range f.opts.Env {
		if why, no := reserved[k]; no {
			f.t.Fatalf("farmOpts.Env sets %s, which the harness owns: %s", k, why)
		}
		env[k] = v
	}

	out := cleanEnv()
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// ensureTenant creates a farm.tenants row if the scenario named one the seed
// did not write. A token scoped to a tenant that does not exist authenticates
// and then cannot file a job, which reads as an authorisation bug.
func (f *farm) ensureTenant(t *testing.T, tenant string) {
	t.Helper()
	if tenant == "" {
		t.Fatal("farmOpts.Tenants has an empty tenant id; a scoped token needs a row to scope to")
	}
	_, err := f.pool.Exec(t.Context(),
		`INSERT INTO farm.tenants (id, name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING`, tenant)
	if err != nil {
		t.Fatalf("creating tenant %q for a scenario credential: %v", tenant, err)
	}
}

// sortedKeys keeps the token spec, and so every role's environment, stable
// across runs. Map order would make two identical runs differ in a way that is
// invisible until something logs the spec.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reaperComponents is FARM_REAPER_COMPONENTS narrowed to the roles this farm
// actually runs.
//
// The shipped default names reaper, api, scheduler and jobrunner, and since
// migration 00012 a name in that list with no farm.component_heartbeat row
// makes farm.reaper_arm REFUSE to arm: the reaper then reclaims nothing, for
// the whole scenario, and says so only in a log line and a gauge. A scenario
// that starts a reaper and a jobrunner but no api would hit that and read it
// as "the reaper is broken".
//
// Narrowing is safe in the other direction too, because gap accounting is
// about components that are supposed to be up: a component this farm never
// started has no outage to refund.
func (f *farm) reaperComponents() []string {
	multiplexed := contains(f.opts.Roles, "all") || contains(f.opts.Roles, "demo")
	var out []string
	for _, name := range config.DefaultReaperComponents {
		if multiplexed || contains(f.opts.Roles, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		// config refuses an empty list, and a farm with no renewal-path role
		// has nothing to watch anyway. The shipped default is the honest
		// value to hand a farm that will never call farm.reaper_arm.
		return config.DefaultReaperComponents
	}
	return out
}

// StartRole starts a role that is not running. The role must be one this farm
// was built with, because its port and environment were decided then.
func (f *farm) StartRole(t *testing.T, name string) {
	t.Helper()
	p, ok := f.roles[name]
	if !ok {
		t.Fatalf("this farm has no %q role; it was built with %v", name, f.opts.Roles)
	}
	if p.running() {
		t.Fatalf("the %s role is already running", name)
	}

	cmd := exec.Command(farmdBinary, name)
	cmd.Env = p.env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe for the %s role: %v", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe for the %s role: %v", name, err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the %s role: %v", name, err)
	}

	p.mu.Lock()
	p.cmd, p.done, p.stopping, p.exited, p.waitErr = cmd, make(chan struct{}), false, false, nil
	p.mu.Unlock()

	p.logs.Add(2)
	go streamLines(t, name, stdout, &p.logs)
	go streamLines(t, name, stderr, &p.logs)
	go func() {
		// Wait only after both pipes have hit EOF: os/exec closes them from
		// Wait, and a reader racing that loses the last thing the role said —
		// which is the line a failure is usually explained by.
		p.logs.Wait()
		err := cmd.Wait()
		p.mu.Lock()
		p.exited, p.waitErr = true, err
		done := p.done
		p.mu.Unlock()
		close(done)
	}()
	t.Logf("started the %s role (pid %d, metrics %s)", name, cmd.Process.Pid, f.metricsAddr[name])
	f.waitReady(t, name)
}

// StopRole takes a role away, the way a node drain or an OOM kill does.
//
// It kills rather than asking politely, and that is on purpose twice over.
// Windows has no SIGTERM to send, so a portable graceful stop does not exist
// here; and the system's central claim is that a role dying abruptly does NOT
// end a lease — SIGKILL is simply the strongest form of the eviction it says
// it survives. A scenario that wants the graceful path should exercise it
// through the API instead.
func (f *farm) StopRole(t *testing.T, name string) {
	t.Helper()
	p, ok := f.roles[name]
	if !ok {
		t.Fatalf("this farm has no %q role; it was built with %v", name, f.opts.Roles)
	}
	if !p.running() {
		t.Fatalf("the %s role is not running", name)
	}
	p.stop()
	t.Logf("stopped the %s role", name)
}

func (p *roleProc) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil && !p.exited
}

// stop kills the process and waits for its output to drain. It is safe to call
// on a role that has already exited or was never started.
func (p *roleProc) stop() {
	p.mu.Lock()
	if p.cmd == nil || p.exited {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	cmd, done := p.cmd, p.done
	p.mu.Unlock()

	_ = cmd.Process.Kill()
	<-done
}

// report fails the test for a role that exited on its own. A control plane
// role that dies mid-scenario is never an acceptable outcome: either the
// scenario meant to stop it, in which case stopping is set, or the binary
// fell over and every assertion after that point was measuring a farm that
// was not running.
func (p *roleProc) report(t *testing.T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exited || p.stopping {
		return
	}
	// A clean exit is reported too. A control-plane role runs until it is
	// cancelled, so exit 0 in the middle of a scenario means it decided there
	// was nothing to do — which is a finding, not a success.
	t.Errorf("the %s role exited on its own (%s); its last output is in the %q lines above",
		p.name, exitDescription(p.waitErr), p.name+" |")
}

func exitDescription(err error) string {
	if err == nil {
		return "status 0"
	}
	return err.Error()
}

// deadRole names a role that exited without being asked to, for the benefit of
// a wait that can no longer succeed.
func (f *farm) deadRole() string {
	for _, name := range f.roleOrder {
		p := f.roles[name]
		p.mu.Lock()
		dead := p.cmd != nil && p.exited && !p.stopping
		err := p.waitErr
		p.mu.Unlock()
		if dead {
			return fmt.Sprintf("the %s role exited (%s)", name, exitDescription(err))
		}
	}
	return ""
}

// streamLines copies one pipe into the test log, prefixed with the role name.
// Every line a role prints while a scenario runs is evidence, and a scenario
// that fails with none of it is a scenario nobody can act on.
func streamLines(t *testing.T, name string, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	// A resolved-configuration block is a few hundred bytes a line; a stack
	// trace from a panicking role is not. Truncating one would hide the very
	// thing it was printed for.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		t.Logf("%s | %s", name, sc.Text())
	}
}

// ---------------------------------------------------------------------------
// Small pieces
// ---------------------------------------------------------------------------

// freePort asks the kernel for a port nobody is listening on and hands it
// straight back.
//
// There is a window between the close here and the role's own bind, and
// nothing can remove it: a port cannot be held open and bound by a
// subprocess at the same time. It is small, and the two failures it can cause
// are both loud — an api that cannot bind exits and is reported by
// roleProc.report, and a metrics listener that cannot bind logs an error and
// leaves farm_metrics_listener_up at 0.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the probe listener: %v", err)
	}
	return port
}

// cleanEnv is the OS environment with every farm variable removed. See envFor.
func cleanEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "FARM_") || upper == config.EnvDatabaseURL {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runBinary runs the shipped binary once and waits for it. It returns the
// combined output and the exit code; err is non-nil only when the process
// could not be run at all, because an exit code is an answer and this suite
// asserts on several of them.
func runBinary(t *testing.T, timeout time.Duration, env []string, args ...string) (stdout, stderr string, exit int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, farmdBinary, args...)
	cmd.Env = env
	// Separate buffers, not CombinedOutput. ctl keeps the two streams apart on
	// purpose — internal/ctl/ctl.go puts warnings and the blast-radius block on
	// stderr so a listing stays machine-readable when it is piped into jq — and
	// merging them here would hand a scenario a stdout that parses until the
	// first run in which ctl warns about something.
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	runErr := cmd.Run()

	// A timeout is the HARNESS killing the process, not the process choosing an
	// exit code, and the two must not read the same. A ctl verb that legitimately
	// exits 3 or 4 is part of its contract; a ctl verb that was killed at 30s is
	// a hung API, and a scenario that cannot tell them apart will assert on the
	// wrong one.
	if ctx.Err() != nil {
		return outBuf.String(), errBuf.String(), -1, fmt.Errorf(
			"the harness killed `farmd %s` after %s: %w",
			strings.Join(args, " "), timeout, ctx.Err())
	}
	switch {
	case runErr == nil:
		return outBuf.String(), errBuf.String(), 0, nil
	case cmd.ProcessState != nil:
		return outBuf.String(), errBuf.String(), cmd.ProcessState.ExitCode(), nil
	default:
		return outBuf.String(), errBuf.String(), -1, runErr
	}
}

// scratchName turns a test name into a database name: lowercase, no
// punctuation, unique per process and per call.
func scratchName(t *testing.T) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '_'
	}, t.Name())
	if len(safe) > 28 {
		safe = safe[:28]
	}
	// Postgres identifiers stop at 63 bytes; this is comfortably inside.
	return fmt.Sprintf("df_e2e_%s_%d_%d", safe, os.Getpid(), farmSeq.Add(1))
}

// sweepStaleDatabases drops scratch databases a killed run left behind.
//
// What makes this safe to run while other copies of this suite are running is
// the connection test, not a clock: every live scenario holds a pgxpool
// connection to its own database for its whole length, so a df_e2e_ database
// with nothing connected is one nobody is using. Dropping a live one WITH
// (FORCE) would take a working run down, which is why the query asks
// pg_stat_activity rather than assuming from the name.
//
// The one window it cannot see is between another run's CREATE DATABASE and
// its first connection — microseconds, and the loser gets a clear error from
// its own migrate step rather than a corrupt run.
func sweepStaleDatabases() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return
	}
	defer admin.Close()

	rows, err := admin.Query(ctx, `
SELECT d.datname
  FROM pg_database d
  LEFT JOIN pg_stat_activity a ON a.datname = d.datname
 WHERE d.datname LIKE 'df_e2e_%'
 GROUP BY d.datname
HAVING count(a.pid) = 0`)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			stale = append(stale, name)
		}
	}
	rows.Close()

	for _, name := range stale {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: a previous run left %s behind and it could not be "+
				"dropped: %v\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "e2e: dropped %s, left behind by a run that did not finish\n", name)
	}
}

// dsnFor rewrites the database name in the admin DSN, keeping every other
// parameter — sslmode above all, since the shipped default is "prefer" and a
// dropped sslmode=disable breaks every farm on a cluster that refuses TLS.
func dsnFor(t *testing.T, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", config.EnvDatabaseURL, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("%s must be a postgres:// URL, got scheme %q", config.EnvDatabaseURL, u.Scheme)
	}
	u.Path = "/" + name
	return u.String()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	return lines[len(lines)-1]
}

// randomish makes the tokens of two farms differ, so a token left in a stale
// environment cannot authenticate against the wrong scenario's API.
func randomish() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
}
