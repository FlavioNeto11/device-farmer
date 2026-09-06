package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/runner"
)

// Capabilities reports what this deployment can actually do, read from the
// running system rather than written down beside it.
//
// The distinction matters. A feature list in a README describes what the
// project can do; an operator needs to know what THIS process, against THIS
// database, with THIS configuration, will do at 3am. Those diverge the moment
// somebody deploys without a host agent, or forgets FARM_API_TOKENS, or runs a
// binary older than the schema.
//
// Every field here is therefore observed: the schema version comes from the
// migration table, the roles come from their own heartbeats, the host agents
// come from farm.hosts, and the authentication mode comes from the
// Authenticator this server was actually built with.
type Capabilities struct {
	Build    BuildInfo         `json:"build"`
	Schema   SchemaInfo        `json:"schema"`
	Auth     AuthInfo          `json:"auth"`
	Roles    []RoleStatus      `json:"roles"`
	Features []FeatureStatus   `json:"features"`
	Fleet    map[string]int    `json:"fleet"`
	Limits   map[string]string `json:"limits"`
}

// ProbeFailure is one observation this report tried to make and could not.
//
// Consequence is carried because the reader's instinct on a missing number is
// to substitute the obvious default, and the obvious default is wrong in every
// case here: an unreadable goose table is not schema v0, and an unreadable
// heartbeat table is not seven dead loops.
type ProbeFailure struct {
	Probe       string `json:"probe"`
	Error       string `json:"error"`
	Consequence string `json:"consequence"`
}

type BuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	Go        string `json:"go"`
	Platform  string `json:"platform"`
	StartedAt string `json:"started_at"`
	UptimeS   int64  `json:"uptime_s"`
}

type SchemaInfo struct {
	Version int    `json:"version"`
	Applied string `json:"applied_at,omitempty"`
	// Migrations are embedded in the binary, so a version the binary does not
	// carry means somebody deployed an old image against a newer database.
	Note string `json:"note,omitempty"`
}

type AuthInfo struct {
	Mode string `json:"mode"`
	Open bool   `json:"open"`
	// Consequence is spelled out rather than left to the reader, because
	// "allow-all" reads as a configuration value and not as a warning.
	Consequence string `json:"consequence,omitempty"`
	Fix         string `json:"fix,omitempty"`
}

// RoleStatus is one control-plane component and whether it is beating.
//
// A role that has stopped beating is not merely absent from a list: the
// reaper's gap detection reads these same heartbeats, so a dead component is
// also the thing that stops leases being reclaimed while it is gone.
type RoleStatus struct {
	Component string `json:"component"`
	Running   bool   `json:"running"`
	LastBeatS *int64 `json:"last_beat_s,omitempty"`
	Meaning   string `json:"meaning"`
}

// FeatureStatus is one capability and the honest state of it.
type FeatureStatus struct {
	Name   string `json:"name"`
	State  string `json:"state"` // enabled | degraded | unavailable | not_built
	How    string `json:"how"`
	Detail string `json:"detail,omitempty"`
}

// knownRoles is the set the dashboard expects to see beating, with what each
// one's absence actually costs. The reaper's own gap detection reads
// farm.component_heartbeat for the renewal path, so this list and that one
// describe the same thing from two directions.
var knownRoles = []struct{ component, meaning string }{
	{"api", "serves this page and the HTTP API"},
	{"scheduler", "matches queued jobs to free devices; without it nothing is ever placed"},
	{"jobrunner", "runs job specs on leased devices; without it jobs sit in running, holding a device each"},
	{"reaper", "the only automatic release path; without it an abandoned lease is never reclaimed"},
	{"recovery", "the recovery ladder; without it a stuck device stays stuck until a human acts"},
	{"watchdog", "device health; without it the fleet view goes stale and quarantine never fires"},
	{"node", "host agent on a USB host; without it recovery tiers 3 and 4 are refused"},
	{"enroll", "adopts newly plugged devices; without it a new handset never joins the fleet"},
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Three reads, and every one of them can fail. What matters is that a
	// failure is not allowed to become an assertion.
	//
	// This handler used to discard all three errors, and the result was worse
	// than an outage: with Postgres unreachable it answered 200 with "schema
	// v0 — no migrations applied; run farmd migrate up", every role marked as
	// never having beaten, and a fleet of zero devices. Each of those is a
	// specific, confident, false claim, and each sends an operator somewhere
	// unhelpful — to re-run a migration against a healthy schema, or to go
	// hunting for seven dead loops that are all running fine.
	//
	// So a report that could not be taken is not a report. The 200 also
	// suppressed the dashboard's own "the control plane is not answering"
	// banner, which fires on 5xx, hiding the one true statement available.
	schema, schemaErr := s.schemaInfo(ctx)
	roles, rolesErr := s.roleStatuses(ctx)
	fleet, fleetErr := s.fleetCounts(ctx)

	var failed []ProbeFailure
	for _, p := range []struct {
		name, consequence string
		err               error
	}{
		{"schema", "the applied migration version is unknown; it is NOT v0", schemaErr},
		{"roles", "no conclusion may be drawn about which control-plane loops are beating", rolesErr},
		{"fleet", "device, host and lease counts are unknown; they are NOT zero", fleetErr},
	} {
		if p.err != nil {
			failed = append(failed, ProbeFailure{
				Probe: p.name, Error: p.err.Error(), Consequence: p.consequence,
			})
		}
	}
	if len(failed) > 0 {
		s.log.ErrorContext(ctx, "the capability report could not be taken",
			"failed_probes", len(failed), "err", errors.Join(schemaErr, rolesErr, fleetErr))
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			fmt.Sprintf("this deployment's capabilities could not be observed: %d of 3 "+
				"database probes failed. Nothing is reported, rather than reporting a "+
				"default as a fact.", len(failed)),
			failed)
		return
	}

	caps := Capabilities{
		Build:  s.buildInfo(),
		Schema: schema,
		Auth:   s.authInfo(),
		Roles:  roles,
		Fleet:  fleet,
		Limits: s.limits(),
	}
	caps.Features = s.featureStatuses(ctx, caps.Roles)
	writeJSON(w, http.StatusOK, caps)
}

func (s *Server) buildInfo() BuildInfo {
	b := BuildInfo{
		Go:        runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		StartedAt: s.startedAt.UTC().Format(time.RFC3339),
		UptimeS:   int64(time.Since(s.startedAt).Seconds()),
		Version:   "dev",
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			b.Version = bi.Main.Version
		}
		for _, st := range bi.Settings {
			if st.Key == "vcs.revision" {
				b.Revision = st.Value
			}
		}
	}
	// What the linker was told wins over what the toolchain inferred. For a
	// `go build` of a main module the toolchain infers "(devel)" and the block
	// above leaves the "dev" default standing, which is how a released image
	// came to report itself as a development build. The image is also compiled
	// -buildvcs=false, so vcs.revision is absent there and the stamp is the only
	// source of a revision at all.
	if s.build.Version != "" {
		b.Version = s.build.Version
	}
	if s.build.Revision != "" {
		b.Revision = s.build.Revision
	}
	return b
}

func (s *Server) schemaInfo(ctx context.Context) (SchemaInfo, error) {
	// The table is FARM_MIGRATIONS_TABLE, not necessarily goose_db_version.
	// This query used to name goose_db_version literally and unqualified,
	// which made it wrong twice over on a farm that had set the variable: it
	// read the wrong table, and it read it through whatever search_path the
	// connection happened to carry while the configured default is
	// public-qualified. The operator saw a migration that worked followed by
	// a capability probe that failed, and — a failed probe being a 503 — was
	// told their fully migrated database had no schema.
	table, err := s.migrationsTable()
	if err != nil {
		return SchemaInfo{}, err
	}

	var (
		v  *int
		at *time.Time
	)
	// goose owns this table; reading it is how the API learns which schema it
	// is actually talking to rather than which one it shipped with.
	err = s.pool.QueryRow(ctx,
		`SELECT version_id, tstamp FROM `+table+
			` WHERE is_applied ORDER BY id DESC LIMIT 1`).Scan(&v, &at)

	// No rows is an ANSWER, and the only one of the two that may be reported:
	// goose has a table and nothing applied, so v0 and "run farmd migrate up"
	// are both true. Any other error means the table could not be read, and
	// then v0 is a guess wearing the clothes of a measurement.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SchemaInfo{}, fmt.Errorf("reading %s: %w", table, err)
	}

	out := SchemaInfo{}
	if v != nil {
		out.Version = *v
	}
	if at != nil {
		out.Applied = at.UTC().Format(time.RFC3339)
	}
	if out.Version == 0 {
		out.Note = "no migrations applied; run farmd migrate up"
	}
	return out, nil
}

// migrationsTable renders FARM_MIGRATIONS_TABLE as a quoted SQL identifier,
// ready to be concatenated into a statement.
//
// Concatenated, because a table name is an identifier and not a value: it
// cannot be $1. The in-repo precedent is cmd/farmd's SET ROLE, which quotes
// FARM_DB_ROLE the same way. The split matters as much as the quoting — see
// config.MigrationsTableParts for why quoting the default as a single part
// would name a table nobody has.
//
// The name is re-checked here rather than trusted, because config.Validate is
// the boot path and this Server can also be built around a Config assembled in
// code. Checking is what makes the quoting safe to rely on: for a value the
// same function has already passed, Sanitize only adds quotes, and the pair
// leaves no way for an operator-set variable to become an injection point.
func (s *Server) migrationsTable() (string, error) {
	// An empty name is a hand-built Config rather than an operator's choice —
	// Load always fills this in — so the default is what it would have got.
	name := config.DefaultMigrationsTable
	if s.cfg != nil && s.cfg.MigrationsTable != "" {
		name = s.cfg.MigrationsTable
	}
	schema, table, err := config.MigrationsTableParts(name)
	if err != nil {
		return "", fmt.Errorf("this process holds a Config that never went through "+
			"config.Validate, which refuses this at boot: %s (%q) %w",
			config.EnvMigrationsTable, name, err)
	}
	if schema == "" {
		return pgx.Identifier{table}.Sanitize(), nil
	}
	return pgx.Identifier{schema, table}.Sanitize(), nil
}

func (s *Server) authInfo() AuthInfo {
	name := "none"
	if s.auth != nil {
		name = s.auth.Name()
	}
	info := AuthInfo{Mode: name}
	if name == "allow-all" || name == "none" {
		info.Open = true
		info.Consequence = "every request is granted the operator role: anyone who can reach " +
			"this port can revoke leases, drain hosts and power-cycle slots"
		info.Fix = "set FARM_API_TOKENS to a token list, or supply an Authenticator"
	}
	return info
}

func (s *Server) roleStatuses(ctx context.Context) ([]RoleStatus, error) {
	// One row per component that has ever beaten, with how long ago. A
	// watchdog is per-host and beats as "watchdog:h01", so prefixes match.
	beats := map[string]int64{}
	rows, err := s.pool.Query(ctx,
		`SELECT component, GREATEST(0, round(extract(epoch FROM now() - beat_at)))::bigint
		   FROM farm.component_heartbeat`)
	if err != nil {
		return nil, fmt.Errorf("reading farm.component_heartbeat: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var ago int64
		if err := rows.Scan(&c, &ago); err != nil {
			return nil, fmt.Errorf("scanning farm.component_heartbeat: %w", err)
		}
		beats[c] = ago
	}
	// A row set that stopped early is a PARTIAL heartbeat table, and a partial
	// one reads exactly like a farm with dead roles. Checked, because not
	// reporting that when it is untrue is the whole job of this function.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading farm.component_heartbeat: %w", err)
	}

	// A component is "running" if it beat recently. The window is generous
	// because these loops have different periods, and a role reported dead
	// because it is merely slow sends an operator chasing nothing.
	const staleAfter = 120

	out := make([]RoleStatus, 0, len(knownRoles))
	for _, kr := range knownRoles {
		st := RoleStatus{Component: kr.component, Meaning: kr.meaning}
		var best *int64
		for c, ago := range beats {
			if c == kr.component || len(c) > len(kr.component) &&
				c[:len(kr.component)] == kr.component && c[len(kr.component)] == ':' {
				a := ago
				if best == nil || a < *best {
					best = &a
				}
			}
		}
		if best != nil {
			st.LastBeatS = best
			st.Running = *best <= staleAfter
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Server) fleetCounts(ctx context.Context) (map[string]int, error) {
	var devices, hosts, healthy, leased, quarantined, jobs, artifacts int
	if err := s.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM farm.devices),
       (SELECT count(*) FROM farm.hosts),
       (SELECT count(*) FROM farm.device_runtime WHERE health = 'healthy'),
       (SELECT count(*) FROM farm.leases WHERE state IN ('held','suspect')),
       (SELECT count(*) FROM farm.quarantines WHERE closed_at IS NULL),
       (SELECT count(*) FROM farm.jobs WHERE state IN ('queued','running')),
       (SELECT count(*) FROM farm.artifacts)`).
		Scan(&devices, &hosts, &healthy, &leased, &quarantined, &jobs, &artifacts); err != nil {
		// Zero devices and zero live leases is a coherent, readable and entirely
		// wrong picture of a full rack. Refuse to draw it.
		return nil, fmt.Errorf("counting the fleet: %w", err)
	}

	return map[string]int{
		"devices":          devices,
		"hosts":            hosts,
		"healthy":          healthy,
		"live_leases":      leased,
		"open_quarantines": quarantined,
		"active_jobs":      jobs,
		"artifacts":        artifacts,
	}, nil
}

func (s *Server) limits() map[string]string {
	l := map[string]string{}
	if s.cfg != nil {
		l["lease_ttl"] = s.cfg.Lease.TTL.String()
		l["lease_grace"] = s.cfg.Lease.Grace.String()
		l["renew_interval"] = s.cfg.Lease.RenewInterval.String()
		l["witness_interval"] = s.cfg.Lease.WitnessInterval.String()
		l["witness_max_extensions"] = itoa(s.cfg.Lease.MaxWitnessExtensions)
		// Derived from the witness interval, never set: what the marker
		// actually runs on, and how stale its evidence may be and still count.
		l["marker_interval"] = s.cfg.Lease.MarkerInterval().String()
		l["witness_max_evidence_age"] = s.cfg.Lease.MaxEvidenceAge().String()
		l["slot_rearm"] = s.cfg.Lease.SlotRearm.String()
		l["reaper_gap_floor"] = s.cfg.Reaper.GapFloor.String()
	}
	l["device_exec_max_output"] = byteSize(s.execMaxOutput)
	return l
}

func (s *Server) featureStatuses(ctx context.Context, roles []RoleStatus) []FeatureStatus {
	running := map[string]bool{}
	for _, r := range roles {
		running[r.Component] = r.Running
	}

	out := []FeatureStatus{
		{
			Name: "Device leasing", State: "enabled",
			How: "farm.lease_acquire / lease_renew / lease_release, fenced in PostgreSQL",
			Detail: "A connectivity failure cannot end a lease: farm.leases.release_reason " +
				"is CHECK-constrained to a domain with no connectivity value.",
		},
		{
			Name: "Job execution", State: stateIf(running["jobrunner"], "enabled", "unavailable"),
			How: "internal/runner over the step vocabulary in farm.step_kinds",
			Detail: ifElse(running["jobrunner"],
				"Steps run against leased devices; a transport failure is retried inside the lease.",
				"No jobrunner is beating, so allocated jobs will sit in 'running' holding a device each."),
		},
		{
			// Enabled iff the jobrunner beats, because the jobrunner is the
			// role that starts one witness loop per placement; the SQL
			// function and the loop existing in the binary is not the same
			// as a witness being presented for anything.
			Name: "Witness extensions", State: stateIf(running["jobrunner"], "enabled", "unavailable"),
			How: "runner.Marker rewrites " + runner.MarkerPath + " on the device; lease.WitnessLoop " +
				"presents it through farm.lease_witness every FARM_LEASE_WITNESS_INTERVAL",
			Detail: ifElse(running["jobrunner"],
				"A job that can still touch its device keeps its lease through a control-plane outage "+
					"longer than ttl+grace, for up to FARM_LEASE_WITNESS_MAX_EXTENSIONS consecutive "+
					"extensions. A refused witness ends nothing; only farm.lease_renew can report fencing.",
				"No jobrunner is beating, so no placement is producing on-device evidence: leases are "+
					"protected by ttl+grace and the control-plane gap refund only."),
		},
		{
			Name: "Automatic reclamation", State: stateIf(running["reaper"], "enabled", "unavailable"),
			How: "farm.lease_reclaim behind a grace band, a control-plane gap refund and a quiesce gate",
			Detail: ifElse(running["reaper"],
				"The only automatic release path in the system.",
				"No reaper is beating: an abandoned lease will never be reclaimed and its device stays out of the pool."),
		},
		{
			Name: "Health monitoring", State: stateIf(running["watchdog"], "enabled", "unavailable"),
			How: "track-devices streams per host, reconciled into farm.device_runtime",
			Detail: ifElse(running["watchdog"],
				"Health is a separate clock and can never touch a lease.",
				"No watchdog is beating: the fleet view is stale and quarantine will not fire."),
		},
		{
			Name: "Recovery ladder", State: stateIf(running["recovery"], "enabled", "unavailable"),
			How: "tiers stored in farm.recovery_tiers, refused when blast radius exceeds a live lease's policy",
			// A host agent beating is a fact about the fleet; holding a client
			// to ask one with is a fact about THIS process, and only the second
			// decides what the operator's power button does. Reporting the
			// first alone let this line say tier 4 can act while
			// POST /api/v1/slots/{id}/power was answering 503 on every slot.
			Detail: ifElse(running["node"],
				"A host agent is present, so tiers 3 and 4 (USB reset, port power) can act. "+
					ifElse(s.hostRunner != nil,
						"This api process holds a node client too, so an operator can cycle a port from here.",
						"This api process holds no node client, so an operator's POST /api/v1/slots/{id}/power "+
							"still answers 503 and only the recovery role's own rungs act."),
				"No host agent is beating: tiers 3 and 4 are REFUSED with a reason, not silently skipped."),
		},
		{
			// The ladder's runner lives in the recovery process; this one is
			// THIS process's, and it is the only thing that decides whether
			// the operator's power button does anything.
			Name: "Operator port power", State: stateIf(s.hostRunner != nil, "enabled", "unavailable"),
			How: "POST /api/v1/slots/{id}/power cycles VBUS through the host's farmd-node agent, " +
				"synchronously, under the ladder's tier-4 policy and lock, and closes its own " +
				"farm.recovery_attempts row with the agent's answer",
			Detail: ifElse(s.hostRunner != nil,
				"This api process holds a node client, so a slot power cycle asks the host's agent "+
					"at farm.hosts.node_endpoint and answers with the outcome.",
				"This api process has no node client (FARM_NODE_TOKEN is unset), so "+
					"POST /api/v1/slots/{id}/power answers 503 and records no attempt."),
		},
		{
			Name: "Dynamic enrollment", State: stateIf(running["enroll"] || running["node"], "enabled", "unavailable"),
			How: "farm.resolve_device: branded farm_uid, then hardware fingerprint, then serial AND slot, then adopt",
			Detail: ifElse(running["enroll"] || running["node"],
				"A newly plugged device is observed, resolved and branded automatically.",
				"No enroller is beating: a handset plugged in now will not join the fleet until one runs."),
		},
		{
			Name: "File transfer", State: "enabled",
			How: "the ADB sync protocol implemented natively in internal/adbwire",
			Detail: "push, pull and install stream in both directions; a 200MB artifact is never " +
				"buffered in memory.",
		},
		{
			Name: "Artifacts", State: "enabled",
			How: "content-addressed by sha256, with EnsureOnDevice skipping a push the device already has",
		},
		{
			Name: "Live updates", State: "enabled",
			How: "server-sent events on /api/v1/stream, polled and diffed server-side",
		},
		{
			// Not "not_built" any more: bearer authentication is written and
			// AuthenticatorFor refuses to start without an explicit choice, so
			// reaching this branch means somebody set FARM_API_AUTH=allow-all
			// on purpose. Saying a built thing does not exist would send an
			// operator looking for work that is already done instead of at the
			// one variable that turns it on.
			Name: "Authentication", State: "unavailable",
			How: "built (static bearer), and deliberately switched off with FARM_API_AUTH=allow-all",
			Detail: "Every caller is granted the operator role. Do not expose this port to a " +
				"network you do not control; set FARM_API_TOKENS to close it. The seam for " +
				"OIDC is documented in internal/api/auth.go.",
		},
		s.fenceEnforcement(),
		{
			Name: "Helm chart", State: "enabled",
			How: "deploy/helm/device-farmer, one Deployment per role; docker-compose's 'farm' " +
				"profile carries the same split for a single machine",
			Detail: "The chart refuses DATABASE_URL, FARM_API_TOKENS and FARM_COMPONENT in " +
				"config.extra: the first two are credentials, and one shared FARM_COMPONENT " +
				"would make the reaper's gap accounting blind to the role that is down.",
		},
	}

	if s.auth != nil && !s.authInfo().Open {
		for i := range out {
			if out[i].Name == "Authentication" {
				out[i].State = "enabled"
				out[i].How = "static bearer tokens (" + s.auth.Name() + ")"
				out[i].Detail = ""
			}
		}
	}
	return out
}

// fenceEnforcement reports the client half of the fence proxy as THIS process
// sees it: whether every ADB connection it opens presents a certificate and
// announces its fence.
//
// That is all it can see. Enforcement happens per host, in the proxy that
// fronts that host's ADB server, and nothing here can observe which hosts run
// one. So even "enabled" is a statement about this process's side of the
// wire, and the detail says so rather than letting a green row stand for a
// property of the whole farm.
func (s *Server) fenceEnforcement() FeatureStatus {
	const detail = "Enforcement is per host: a host running the proxy (FARM_FENCE_TLS_*) refuses a " +
		"revoked fence at the ADB socket, and a host without one relies on the holder to honour " +
		"the fence floor PostgreSQL raised. This process cannot see which hosts run the proxy."
	if s.cfg == nil || !s.cfg.FenceClient.Enabled() {
		return FeatureStatus{
			Name: "Fence enforcement at the resource", State: "unavailable",
			How: "built (internal/fenceproxy + the adbwire admission preamble), and switched off in " +
				"this process: set FARM_FENCE_CLIENT_CERT/KEY/CA here and FARM_FENCE_TLS_* on each host",
			Detail: "This process dials ADB servers in the clear and sends no fence, so " +
				"the fence is enforced in PostgreSQL only and honoured by the client. " + detail,
		}
	}
	return FeatureStatus{
		Name: "Fence enforcement at the resource", State: "enabled",
		How: "mutual TLS to each host's fence proxy (FARM_FENCE_CLIENT_CERT/KEY/CA), every " +
			"connection announcing its class and, for a job, its fence",
		Detail: detail,
	}
}

func stateIf(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func ifElse(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func byteSize(n int) string {
	switch {
	case n <= 0:
		return "unbounded"
	case n >= 1<<20:
		return itoa(n/(1<<20)) + " MiB"
	case n >= 1<<10:
		return itoa(n/(1<<10)) + " KiB"
	default:
		return itoa(n) + " B"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
