package api

// What /api/v1/capabilities may say when it cannot see.
//
// This route exists to answer "what will THIS process, against THIS database,
// actually do at 3am". Every field in it is observed rather than declared,
// which is the point — and which is exactly why a failed observation must not
// be published as an observation. The handler used to discard the error from
// all three of its database probes, so an unreachable Postgres produced a
// confident, well-formed, entirely false report: schema v0 with the advice to
// run a migration that had already been applied, seven control-plane loops
// marked as never having beaten, and a fleet of zero devices.
//
// The tests below are all the same shape: point the server at a database that
// is not there, and assert on what the answer does NOT claim.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// TestFenceEnforcementReportsOnlyThisProcessSideOfTheWire. The row used to
// say not_built. Now that the proxy and the client preamble exist, the honest
// answer depends on a certificate this process may or may not have been
// given, and even with one it is a statement about this process, not about
// which hosts run a proxy. So: unavailable without the certificate, naming
// both halves of the knob; enabled with it; and in both states the detail
// says enforcement is per host.
//
// Falsify: return the enabled row unconditionally from fenceEnforcement, or
// drop "per host" from its detail.
func TestFenceEnforcementReportsOnlyThisProcessSideOfTheWire(t *testing.T) {
	t.Parallel()

	const name = "Fence enforcement at the resource"

	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		wantState string
	}{
		{"no config at all", nil, "unavailable"},
		{"config without a client certificate", &config.Config{}, "unavailable"},
		{"config with a client certificate", &config.Config{
			FenceClient: config.FenceClient{TLS: &tls.Config{MinVersion: tls.VersionTLS13}},
		}, "enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{cfg: tc.cfg}
			got := s.fenceEnforcement()
			if got.Name != name {
				t.Fatalf("feature name = %q, want %q; the dashboard finds the row by name", got.Name, name)
			}
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
			text := got.How + " " + got.Detail
			if !strings.Contains(text, "per host") {
				t.Errorf("the row does not say enforcement is per host; a green row would then stand for the whole farm:\n%s", text)
			}
			if tc.wantState != "unavailable" {
				return
			}
			for _, want := range []string{config.EnvFenceClientCert + "/KEY/CA", "FARM_FENCE_TLS_"} {
				if !strings.Contains(text, want) {
					t.Errorf("the unavailable row does not name %s, so an operator cannot tell which half to set:\n%s", want, text)
				}
			}
			if !strings.Contains(got.Detail, "PostgreSQL only") {
				t.Errorf("the unavailable row does not say where the fence IS enforced:\n%s", got.Detail)
			}
		})
	}
}

// unreachableServer is a Server whose pool points at a port nothing listens on.
// pgxpool connects lazily, so construction succeeds and every query fails —
// which is precisely the production incident being reproduced.
func unreachableServer(t *testing.T) *Server {
	t.Helper()

	// Port 1 on the loopback: privileged, unbound, and refused immediately, so
	// the probes fail fast rather than sitting on a dial timeout.
	cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("parsing the dead DSN: %v", err)
	}
	cfg.ConnConfig.ConnectTimeout = 2 * time.Second
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("building a pool against a dead address: %v", err)
	}
	t.Cleanup(pool.Close)

	return &Server{
		pool:      pool,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		auth:      NewAllowAll(slog.New(slog.NewTextHandler(io.Discard, nil)), "test"),
		startedAt: time.Now(),
	}
}

func capabilitiesResponse(t *testing.T, s *Server) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return res, string(body)
}

// TestAnUnreadableDatabaseIsNotReportedAsAnEmptyFarm is the whole defect in one
// assertion. A 200 here is not merely a wrong status code: the dashboard raises
// its "the control plane is not answering" banner on 5xx, so answering 200
// suppressed the one true statement available and left the false report as the
// only thing on screen.
//
// Falsify: write StatusOK instead of StatusServiceUnavailable in
// handleCapabilities' failure branch.
func TestAnUnreadableDatabaseIsNotReportedAsAnEmptyFarm(t *testing.T) {
	t.Parallel()

	res, body := capabilitiesResponse(t, unreachableServer(t))

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; a report that could not be taken is not a report, "+
			"and a 2xx suppresses the dashboard banner that would say so:\n%s",
			res.StatusCode, http.StatusServiceUnavailable, body)
	}

	// The specific false claims the old handler emitted. None of them may
	// appear in an answer given by a process that cannot reach the database.
	for _, forbidden := range []string{
		"run farmd migrate up", // schema v0, invented from a failed read
		`"devices":0`,          // an empty fleet, invented from a failed read
		`"running":true`,       // any verdict at all about a loop we did not observe
		`"running":false`,
	} {
		if strings.Contains(strings.ReplaceAll(body, " ", ""), forbidden) {
			t.Errorf("the response asserts %q with no database to assert it from:\n%s",
				forbidden, body)
		}
	}
}

// TestEveryFailedProbeIsNamedWithItsConsequence. Reporting nothing is only
// better than reporting a default if the reader is told which observation is
// missing and what they must not conclude from the gap. "Schema unknown" left
// bare is read as v0 by the next person who looks.
//
// Falsify: drop the Consequence field from the ProbeFailure literals.
func TestEveryFailedProbeIsNamedWithItsConsequence(t *testing.T) {
	t.Parallel()

	_, body := capabilitiesResponse(t, unreachableServer(t))

	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Detail  []ProbeFailure `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("the failure response is not the standard error envelope: %v\n%s", err, body)
	}
	if env.Error.Code != CodeUnavailable {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeUnavailable)
	}
	if len(env.Error.Detail) != 3 {
		t.Fatalf("detail names %d failed probes, want all 3 (schema, roles, fleet):\n%s",
			len(env.Error.Detail), body)
	}
	for _, p := range env.Error.Detail {
		if p.Probe == "" || p.Error == "" || p.Consequence == "" {
			t.Errorf("probe failure %+v is missing a field; a gap with no stated "+
				"consequence is filled in by the reader with the default that is wrong", p)
		}
	}
	// The two defaults a reader substitutes on their own, denied by name.
	joined := strings.ToLower(body)
	for _, want := range []string{"not v0", "not zero"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the response does not deny the default a reader would substitute (%q):\n%s",
				want, body)
		}
	}
}

// TestWitnessExtensionsAreReportedOnlyWhileAJobrunnerBeats. The witness loop
// is started per placement by the jobrunner and by nothing else, so the
// feature is a fact about that role's heartbeat: the SQL function and the
// loop existing in the binary is not the same as a witness being presented
// for anything.
//
// Falsify: make the entry's State a constant "enabled".
func TestWitnessExtensionsAreReportedOnlyWhileAJobrunnerBeats(t *testing.T) {
	t.Parallel()

	s := unreachableServer(t)
	for _, tc := range []struct {
		running bool
		want    string
	}{
		{true, "enabled"},
		{false, "unavailable"},
	} {
		var got *FeatureStatus
		for _, f := range s.featureStatuses(context.Background(), []RoleStatus{{Component: "jobrunner", Running: tc.running}}) {
			if f.Name == "Witness extensions" {
				f := f
				got = &f
			}
		}
		if got == nil {
			t.Fatal("the capability list does not mention the witness at all; an operator would assume it does not exist")
		}
		if got.State != tc.want {
			t.Errorf("jobrunner running=%v: witness state = %q, want %q", tc.running, got.State, tc.want)
		}
		if got.Detail == "" || got.How == "" {
			t.Errorf("jobrunner running=%v: the witness entry says nothing about how it works or what its absence costs: %+v",
				tc.running, got)
		}
	}
}

// TestNoMigrationsAppliedIsStillAnAnswer guards the fix against overcorrecting.
// An empty goose_db_version is not a failure: v0 and "run farmd migrate up" are
// both true then, and turning that into a 503 would break the one case the
// note was written for — a fresh database that genuinely needs migrating.
//
// Falsify: drop the pgx.ErrNoRows branch from schemaInfo, so no-rows becomes
// an error; this test then fails while the two above still pass.
func TestNoMigrationsAppliedIsStillAnAnswer(t *testing.T) {
	pool := oneSessionPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`CREATE TEMP TABLE goose_db_version (
		   id serial primary key, version_id bigint, is_applied boolean, tstamp timestamptz)`); err != nil {
		t.Skipf("cannot create the temporary goose table here: %v", err)
	}

	// The name is unqualified so that the temp table above is what the probe
	// finds: pg_temp comes first in search_path. Spelling it out is also the
	// honest shape of this test now that the probe reads the configured name
	// rather than a literal — the table it looks in is a choice, and a test
	// that leaves the choice implicit is testing the default by accident.
	s := &Server{
		cfg:       &config.Config{MigrationsTable: "goose_db_version"},
		pool:      pool,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		auth:      NewAllowAll(slog.New(slog.NewTextHandler(io.Discard, nil)), "test"),
		startedAt: time.Now(),
	}

	info, err := s.schemaInfo(ctx)
	if err != nil {
		t.Fatalf("an empty goose_db_version must be an answer, not a failure: %v", err)
	}
	if info.Version != 0 || !strings.Contains(info.Note, "farmd migrate up") {
		t.Errorf("schemaInfo on an unmigrated database = %+v; want v0 with the migrate note", info)
	}
}

// TestSchemaVersionComesFromTheConfiguredTable is OBS-10: FARM_MIGRATIONS_TABLE
// is the migrator's knob and the probe's knob, or it is a knob that breaks the
// thing it configures.
//
// `farmd migrate` has always honoured it — cmd/farmd/migrate.go calls
// goose.SetTableName — while this probe named goose_db_version literally and
// unqualified. So an operator who renamed the table got a migration that
// worked and then a capability call that could not find a table, and because a
// failed probe is a 503 (OBS-09) the answer they were handed said the schema
// version of their fully migrated database was unknown.
//
// Both shapes are covered because they fail differently. A qualified name is
// the one that needs the split — quoting "pg_temp.farm_schema_version" as a
// single identifier asks for a table whose NAME contains a dot — and an
// unqualified one is the one that must still resolve through search_path, the
// way goose resolves the same value.
//
// Falsify: restore the literal `FROM goose_db_version` in schemaInfo. Both
// cases then fail — against an unmigrated database with the table missing
// entirely, and against a migrated one by reporting the real public schema
// version instead of the one written here.
func TestSchemaVersionComesFromTheConfiguredTable(t *testing.T) {
	pool := oneSessionPool(t)
	ctx := context.Background()

	// Deliberately NOT named goose_db_version: a probe that still reaches the
	// default table cannot accidentally pass this test.
	if _, err := pool.Exec(ctx,
		`CREATE TEMP TABLE farm_schema_version (
		   id serial primary key, version_id bigint, is_applied boolean, tstamp timestamptz)`); err != nil {
		t.Skipf("cannot create a temporary version table here: %v", err)
	}
	// Three rows in goose's own shape: a superseded version, the applied one,
	// and a rolled-back row above it. Only the last row with is_applied is the
	// answer, so a probe that dropped either the filter or the ordering would
	// report 16 or 18 rather than 17.
	if _, err := pool.Exec(ctx,
		`INSERT INTO farm_schema_version (version_id, is_applied, tstamp) VALUES
		   (16, true,  now() - interval '2 hours'),
		   (17, true,  now() - interval '1 hour'),
		   (18, false, now())`); err != nil {
		t.Fatalf("seeding the version table: %v", err)
	}

	for _, name := range []string{"pg_temp.farm_schema_version", "farm_schema_version"} {
		t.Run(name, func(t *testing.T) {
			s := &Server{
				cfg:       &config.Config{MigrationsTable: name},
				pool:      pool,
				log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
				auth:      NewAllowAll(slog.New(slog.NewTextHandler(io.Discard, nil)), "test"),
				startedAt: time.Now(),
			}

			info, err := s.schemaInfo(ctx)
			if err != nil {
				t.Fatalf("%s=%q is the table the migrator writes, so the probe must be able "+
					"to read it: %v", config.EnvMigrationsTable, name, err)
			}
			if info.Version != 17 {
				t.Errorf("schema version = %d, want 17 — the probe is not reading %q",
					info.Version, name)
			}
			if info.Applied == "" {
				t.Errorf("schema %+v carries no applied_at; the row has one", info)
			}
			// The note exists to send an operator to `farmd migrate up`, and a
			// migrated farm that is told to migrate is the OBS-10 report in
			// miniature.
			if info.Note != "" {
				t.Errorf("a migrated database was given the unmigrated note %q", info.Note)
			}
		})
	}
}

// oneSessionPool is a pool of exactly one connection, so that a TEMP table
// created through it is visible to every later query. Temp tables vanish with
// the session, which lets a migrated development database host these cases
// without being touched.
func oneSessionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; this case needs a real, reachable database")
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing DATABASE_URL: %v", err)
	}
	pc.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
