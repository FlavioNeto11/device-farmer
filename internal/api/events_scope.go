package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Tenant scoping and filters for GET /api/v1/events.
//
// THE HOLE THIS CLOSES
//
// /api/v1/events is registered with tenant("GET /api/v1/events", ...), so any
// tenant token reaches it, and the query behind it reads farm.events and
// farm.audit_log whole. tenantScope — the check that confines a tenant token to
// its own work, used in jobs.go, jobsteps.go, leases.go and artifacts.go — is
// not among them. A tenant that cannot list another tenant's leases can still
// read every lease_acquired, every job_attempt_finished and every lease_ended
// row in the farm from the timeline, along with the operator audit log: who
// drained which host, who revoked whose lease, and the reason they typed while
// doing it.
//
// THIS FILE IS THE SCOPING, NOT THE ROUTE. handleEvents in ops.go still carries
// its own const query and its own queryInt, and until it builds both from
// EventScopeFromRequest the paragraph above is a description of production, not
// of history. Nothing here can close the hole on its own: an unused type is
// exported surface, and Go will not tell anybody it is unreachable.
//
// WHAT A TENANT MAY SEE
//
// The rows it can be attributed to: an event naming one of its jobs, or one of
// its leases. farm.events carries no tenant_id column — it names entities, not
// owners — so ownership is resolved through the two tables that do carry it.
//
// WHAT THAT COSTS, MEASURED
//
// The plan is not the flattering one. EXPLAIN ANALYZE on a synthetic 200k-row
// farm.events (4000 jobs, half of them the asking tenant's) walks the
// events_at index newest-first and applies the ownership test as a filter over
// two hashed subplans:
//
//	tenant owns half     200 rows returned after 414 examined,   183 buffers, ~1 ms
//	tenant owns nothing    0 rows returned after 200,000 examined, 3110 buffers, 29 ms
//	unscoped operator    200 rows, index-only scan,                  8 buffers, 0.2 ms
//
// LIMIT bounds the answer, not the work. A tenant whose rows are old — or who
// has none — walks the whole timeline, and that cost grows with the farm's
// history rather than with the tenant's. An explicit ?since= turns it back
// into a bounded index range (0.9 ms for the same empty tenant over one hour),
// which is why the dashboard should send one.
//
// It is left as a scan rather than half-fixed. Making the empty case cheap
// needs an owner column on farm.events, maintained by every writer in
// internal/api, internal/reaper, internal/recovery and the watchdog; a default
// window imposed only on tenants would answer a question nobody asked, which
// is the failure this file's `since` handling exists to refuse.
//
// Rows naming neither a job nor a lease are fleet infrastructure —
// hub_quarantined from internal/recovery, device_offline and device_flapping
// from the watchdog, the enrollment trail — and belong to whoever operates the
// farm. A tenant reading them learns which physical devices are sick and when,
// which is the operator's picture of the fleet, not its own.
//
// farm.audit_log is dropped entirely for a tenant-scoped caller. Its rows are
// operator actions across the whole farm, keyed by a free-text subject
// ("host:h01", "lease:<uuid>") with no owner column to filter on, and their
// reason field is written by one human for other humans. Where an operator
// action ended a tenant's own lease, the tenant still sees it: the ledger row
// written by the trigger in 00007_lease_events.sql names the lease, so it
// arrives through the events half with release_reason 'operator_revoked'.
//
// An operator or admin token is unscoped, exactly as tenantScope defines it,
// and sees the farm.

const (
	// The page-size defaults GET /api/v1/events has always used. They stay
	// here rather than at the call site so Query and the handler cannot
	// disagree about what "truncated" means.
	defaultEventLimit = 200
	maxEventLimit     = 2000

	// A filter value long enough to be a mistake. Parameters are bound, never
	// interpolated, so this is not an injection guard: it refuses a megabyte
	// of query string before it becomes a megabyte of index probe.
	maxEventFilterLen = 256
)

// EventScope is a resolved request for the merged timeline: who is asking, and
// what they asked for.
//
// Build it with EventScopeFromRequest, which reads the caller's identity from
// the request context. Tenant is not a filter the caller supplies — it is the
// confinement their token carries — so nothing in the query string can widen
// it.
type EventScope struct {
	// Tenant confines the result to one tenant's jobs and leases. Empty means
	// farm-wide, which is what tenantScope returns for an operator or admin.
	Tenant string

	// Kind matches farm.events.kind and farm.audit_log.action exactly.
	// Exact, not a substring: the dashboard already does substring matching
	// client-side over what it fetched, and a server-side LIKE '%x%' over a
	// growing timeline is a table scan somebody adds during an incident.
	Kind string

	// Subject narrows to one entity. Either "<type>:<id>" — device, lease,
	// job, slot or host — or a bare id, which is tried against every id column.
	Subject string

	// Lookback bounds the window as an age rather than an instant: at >=
	// now() - Lookback, evaluated by the database. Zero means no bound.
	Lookback time.Duration

	// Limit is applied to each half of the merge and again to the result, so
	// a caller cannot be handed more than it asked for from either side. Read
	// it through PageSize, which is what the SQL and the echoed scope both
	// use; the field itself is whatever the caller set.
	Limit int
}

// EventScopeFromRequest resolves the caller's confinement and reads the
// filters. The returned error is a message for the client, suitable for
// badRequest; it names the parameter and what it accepts.
//
// A bad filter is refused rather than ignored. queryInt's habit of falling
// back to a default is right for a page size and wrong for a filter: silently
// answering about the whole farm when somebody asked about one device is a lie
// told at the exact moment they are trying to work out what went wrong.
func EventScopeFromRequest(r *http.Request) (EventScope, error) {
	// tenantScope answers "" for a request carrying no identity at all, and ""
	// means the farm. Everywhere else in this package that is harmless, because
	// the widest thing an unidentified caller could reach is a longer list of
	// jobs; here it is every operator action ever taken and the reason typed
	// while taking it. requireRole puts the identity on the context of every
	// route in router.go, so this cannot fire on a wired route — it fires when
	// somebody wires the merged timeline somewhere the auth middleware does not
	// run, and a loud refusal is the only outcome that makes that mistake
	// visible rather than silently generous.
	if _, ok := IdentityFrom(r.Context()); !ok {
		return EventScope{}, fmt.Errorf("this request carries no authenticated identity, " +
			"and the merged timeline is never served unscoped")
	}

	s := EventScope{
		// The confinement comes from the authenticated identity, never from
		// the query string. There is deliberately no ?tenant= override here:
		// on the jobs list one exists because an operator is already unscoped
		// and it only narrows their view, whereas here accepting the parameter
		// from a tenant-scoped caller would be the bug this file removes.
		Tenant: tenantScope(r.Context()),
		Limit:  queryInt(r, "limit", defaultEventLimit, 1, maxEventLimit),
	}

	kind := queryString(r, "kind")
	if len(kind) > maxEventFilterLen {
		return EventScope{}, fmt.Errorf("kind is longer than %d characters", maxEventFilterLen)
	}
	s.Kind = kind

	subject := queryString(r, "subject")
	if len(subject) > maxEventFilterLen {
		return EventScope{}, fmt.Errorf("subject is longer than %d characters", maxEventFilterLen)
	}
	if t, id, ok := splitEventSubject(subject); ok && id == "" {
		return EventScope{}, fmt.Errorf("subject %q names a %s but no id; write %s:<id>", subject, t, t)
	}
	s.Subject = subject

	// since is a LOOKBACK, not a timestamp, and that is a deliberate refusal
	// rather than a missing feature. Every `at` in this timeline is a server
	// clock; comparing it against an instant from a caller's clock makes the
	// window silently wrong by however far that clock is off, and the caller
	// reads a short list rather than an error. Both ends of a lookback are
	// evaluated by the database, so the skew cannot exist.
	if raw := queryString(r, "since"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return EventScope{}, fmt.Errorf("since must be a positive duration such as 15m, 2h or 72h "+
				"(a window back from now, because every timestamp here is the server's), got %q", raw)
		}
		s.Lookback = d
	}

	return s, nil
}

// TenantScoped reports whether this scope is confined to one tenant.
func (s EventScope) TenantScoped() bool { return s.Tenant != "" }

// PageSize is the limit actually applied, and it is the only limit anything
// here reads.
//
// EventScopeFromRequest already clamps, so this exists for the scope somebody
// builds by hand — a test, a future internal caller, a zero value passed by
// mistake. Left literal, Limit: 0 renders LIMIT 0, and LIMIT 0 is the worst
// answer this file could give: a 200 carrying an empty timeline, indexed and
// sorted and completely convincing, at the moment somebody is trying to find
// out what happened. An out-of-range page size is a caller's slip; an empty
// history is a lie about the farm.
func (s EventScope) PageSize() int {
	switch {
	case s.Limit < 1:
		return defaultEventLimit
	case s.Limit > maxEventLimit:
		return maxEventLimit
	default:
		return s.Limit
	}
}

// Describe is the scope as the response can echo it, so a caller reading a
// short list can tell a filtered view from an empty farm — and a tenant can
// see that the operator audit log was not part of the answer rather than
// concluding nobody has touched anything.
func (s EventScope) Describe() map[string]any {
	d := map[string]any{
		"limit":         s.PageSize(),
		"audit_include": !s.TenantScoped(),
	}
	if s.TenantScoped() {
		d["tenant"] = s.Tenant
	}
	if s.Kind != "" {
		d["kind"] = s.Kind
	}
	if s.Subject != "" {
		d["subject"] = s.Subject
	}
	if s.Lookback > 0 {
		d["since"] = s.Lookback.String()
	}
	return d
}

// Query renders the scope as SQL and its arguments.
//
// The column list is fixed and identical in both branches — at, source,
// action, actor, device_id, slot_id, lease_id, job_id, subject, reason,
// detail — because the caller scans it positionally.
func (s EventScope) Query() (string, []any) {
	// $1 is the limit, bound first so it can be named by all three LIMIT
	// clauses without being repeated in the argument list. PageSize, not
	// Limit: the handler echoes Describe alongside these rows, and a response
	// whose stated page size is not the one the database was given is the
	// thing an operator would trust while counting.
	args := []any{s.PageSize()}

	// Whether the operator audit log is part of the answer decides which
	// parameters exist at all. A bound parameter that no surviving half
	// mentions is not harmless padding: Postgres refuses to prepare a
	// statement whose parameter type it cannot infer, so every argument added
	// below is added only where something reads it.
	includeAudit := !s.TenantScoped()

	eventConds := []string{}
	auditConds := []string{}

	if s.Kind != "" {
		args = append(args, s.Kind)
		p := placeholder(len(args))
		eventConds = append(eventConds, "e.kind = "+p)
		if includeAudit {
			auditConds = append(auditConds, "a.action = "+p)
		}
	}

	if s.Lookback > 0 {
		// intervalSeconds renders a Go duration as an interval literal. A
		// duration is safe to send; an instant is not.
		args = append(args, intervalSeconds(s.Lookback))
		p := placeholder(len(args))
		eventConds = append(eventConds, "e.at >= now() - "+p+"::interval")
		if includeAudit {
			auditConds = append(auditConds, "a.at >= now() - "+p+"::interval")
		}
	}

	if s.Subject != "" {
		kind, id, typed := splitEventSubject(s.Subject)
		if !typed {
			kind, id = "", s.Subject
		}

		// The placeholder is offered before the argument is bound, so a
		// predicate that reads no id — "host:", which farm.events has no
		// column for — leaves the parameter list untouched instead of
		// stranding a value nothing mentions.
		cond, usesID := eventSubjectCond(kind, placeholder(len(args)+1))
		if usesID {
			args = append(args, id)
		}
		eventConds = append(eventConds, cond)

		// farm.audit_log.subject is written as "<type>:<id>" by every operator
		// route in this package, so a typed subject matches it whole and a
		// bare id matches its second half. The full form is also accepted for
		// a caller that copied a subject straight out of a previous response.
		//
		// The value bound is the REBUILT "kind:id", not the raw parameter.
		// splitEventSubject trims the id, so "lease: abc" reaches the events
		// half as "abc" while the raw string still carries the space; binding
		// the raw string here would silently filter the two halves of one
		// merge on two different subjects, and the audit half — the half that
		// records who did it — would be the one that quietly matched nothing.
		if includeAudit {
			if typed {
				args = append(args, kind+":"+id)
				auditConds = append(auditConds, "a.subject = "+placeholder(len(args)))
			} else {
				args = append(args, id)
				p := placeholder(len(args))
				auditConds = append(auditConds,
					"(a.subject = "+p+" OR split_part(a.subject, ':', 2) = "+p+")")
			}
		}
	}

	if s.TenantScoped() {
		args = append(args, s.Tenant)
		p := placeholder(len(args))
		// Either EXISTS attributes the row. A NULL job_id or lease_id makes
		// its own clause false, which is how the fleet rows that name no work
		// — hub_quarantined, device_offline, the enrolment trail — fall out
		// of a tenant's view rather than having to be listed and excluded.
		eventConds = append(eventConds,
			"(EXISTS (SELECT 1 FROM farm.jobs j WHERE j.id = e.job_id AND j.tenant_id = "+p+")"+
				" OR EXISTS (SELECT 1 FROM farm.leases l WHERE l.id = e.lease_id AND l.tenant_id = "+p+"))")
	}

	const eventHalf = `
  (SELECT e.at, 'event'::text AS source, e.kind AS action, coalesce(e.actor,'') AS actor,
          coalesce(e.device_id::text,'') AS device_id, e.slot_id,
          coalesce(e.lease_id::text,'') AS lease_id, coalesce(e.job_id::text,'') AS job_id,
          ''::text AS subject, ''::text AS reason, e.detail
     FROM farm.events e
    %s
    ORDER BY e.at DESC
    LIMIT $1)`

	const auditHalf = `
  (SELECT a.at, 'audit'::text, a.action, a.actor,
          ''::text, NULL::bigint, ''::text, ''::text,
          a.subject, coalesce(a.reason,''), a.detail
     FROM farm.audit_log a
    %s
    ORDER BY a.at DESC
    LIMIT $1)`

	halves := []string{fmt.Sprintf(eventHalf, whereClause(eventConds))}
	if includeAudit {
		halves = append(halves, fmt.Sprintf(auditHalf, whereClause(auditConds)))
	}

	query := "SELECT * FROM (" + strings.Join(halves, "\n  UNION ALL\n") +
		"\n) m\nORDER BY m.at DESC\nLIMIT $1"

	return query, args
}

// eventSubjectCond builds the predicate for one subject against farm.events,
// and reports whether it reads the id parameter p at all.
//
// A typed subject narrows to the single column that can hold it, which keeps
// the query on an index. A bare id is tried against all four, because an
// operator pasting a uuid out of a log line does not yet know whether it is a
// device, a lease or a job — that is usually the question.
//
// "host:" has no column here: farm.events names devices and slots, and a host
// is reached through them. Rather than pretend otherwise, this half is closed
// and the subject is answered from farm.audit_log, where host actions live.
func eventSubjectCond(kind, p string) (cond string, usesID bool) {
	switch kind {
	case "device":
		return "(e.device_id::text = " + p + " OR e.device_id = " + deviceByUID(p) + ")", true
	case "lease":
		return "e.lease_id::text = " + p, true
	case "job":
		return "e.job_id::text = " + p, true
	case "slot":
		return "e.slot_id::text = " + p, true
	case "host":
		return "false", false
	default:
		return "(e.device_id::text = " + p +
			" OR e.lease_id::text = " + p +
			" OR e.job_id::text = " + p +
			" OR e.slot_id::text = " + p +
			" OR e.device_id = " + deviceByUID(p) + ")", true
	}
}

// deviceByUID resolves a branded farm_uid to a device id.
//
// It is a scalar subquery over a unique index rather than a join, so the
// planner evaluates it once for the whole statement instead of per row, and a
// uid that matches nothing yields NULL — a comparison that is false, not an
// error.
func deviceByUID(p string) string {
	return "(SELECT d.id FROM farm.devices d WHERE d.farm_uid = " + p + ")"
}

// splitEventSubject reads the "<type>:<id>" form. ok is false for a bare
// value, which is not an error: it is matched against every id column instead.
func splitEventSubject(subject string) (kind, id string, ok bool) {
	prefix, rest, found := strings.Cut(subject, ":")
	if !found {
		return "", subject, false
	}
	switch prefix {
	case "device", "lease", "job", "slot", "host":
		return prefix, strings.TrimSpace(rest), true
	default:
		// A colon that is not one of ours belongs to the value. Nothing in the
		// farm mints such an id today, but guessing wrong here would silently
		// filter on half a string.
		return "", subject, false
	}
}

// whereClause joins conditions, or returns nothing when there are none.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conds, "\n      AND ")
}

// placeholder renders argument n as $n.
func placeholder(n int) string { return "$" + fmt.Sprint(n) }
