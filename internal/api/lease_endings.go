package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The ledger of endings, over HTTP.
//
// # What this closes
//
// A lease ends when the job says so, when a deadline the user wrote down
// elapses, or when a human takes it back. Checking that at 3am means asking
// one question — "how did this lease end, and who ended it" — and
// migrations/00007_lease_events.sql already answers it: a trigger on
// farm.leases writes one 'lease_ended' row in the same transaction as every
// state change, and farm.v_lease_endings projects the fourteen columns that
// question wants out of the jsonb detail.
//
// Nothing read that view. The only HTTP path to an ending was
// GET /api/v1/events?subject=lease:<uuid>&kind=lease_ended, which hands back a
// raw jsonb blob to be read by eye, so every runbook in docs/runbooks told the
// operator to open psql instead. A control plane whose "why did this lease
// end" requires a database session has not made the ending recoverable; it has
// made it recoverable by whoever still has credentials at 3am.
//
// # It reads the view, not the table
//
// Every statement here selects from farm.v_lease_endings. That is not
// stylistic: the view is created by the schema owner and is not
// security_invoker, so SELECT on it is checked against the view alone and
// farm.events is read with the owner's rights underneath. A role granted this
// view gets exactly the lease_ended rows and exactly these columns, and no
// path to the rest of the timeline. migrations/00018_lease_endings_grants.sql
// is that grant, and says which roles hold it and which are refused.
//
// The JSON field names are the view's column names, unchanged. An operator who
// falls back to the SQL in a runbook — and one should always be able to — is
// then reading the same words in both places.
//
// # What is deliberately not joined
//
// No farm.devices, no farm.slots: an ending carries a device_id and not a rack
// position. The ledger is history, and farm.devices says where a device is
// NOW. migrations/00017_reslot.sql exists precisely so an operator can move a
// handset, so joining the current position onto a three-week-old ending would
// print a rack slot that is confidently wrong at the moment somebody is
// walking to it. The device id is in the row; GET /api/v1/devices/{id} says
// where that device is today, and dates its own answer.

// leaseEndingView is one row of farm.v_lease_endings.
//
// Almost every field is a pointer, and none is omitempty. farm.events permits
// NULL in every id column, and the two that matter most can genuinely be
// absent: release_reason is NULL when a lease reached a terminal state without
// anybody recording why — the exact failure this ledger exists to make
// visible, reported by farm.lease_ended_by as ended_by 'unaccounted'. A key
// that is present and null says "the ledger has no value here"; an omitted key
// would let a client read that as "this API does not carry the field".
type leaseEndingView struct {
	EndedAt       time.Time `json:"ended_at"`
	LeaseID       *string   `json:"lease_id"`
	DeviceID      *string   `json:"device_id"`
	SlotID        *int64    `json:"slot_id"`
	JobID         *string   `json:"job_id"`
	TenantID      *string   `json:"tenant_id"`
	Fence         *int64    `json:"fence"`
	ReleaseReason *string   `json:"release_reason"`
	EndedBy       *string   `json:"ended_by"`
	// HeldSeconds is how long the lease actually held the device, and
	// HeartbeatAgeS how stale the holder's last beat was when it ended. Read
	// together they separate "the holder was alive and said stop" from "the
	// holder went silent and the reaper acted".
	HeldSeconds   *float64 `json:"held_seconds"`
	HeartbeatAgeS *float64 `json:"heartbeat_age_s"`
	Holder        *string  `json:"holder"`
	Protected     *bool    `json:"protected"`
	// Backfilled marks a row reconstructed from farm.leases by 00007's bounded
	// backfill rather than written at the time. Its heartbeat_age_s is
	// measured against the lease's last recorded beat, so it is a
	// reconstruction and says so.
	Backfilled bool `json:"backfilled"`
}

// endingColumns is the projection both routes share.
//
// held_seconds and heartbeat_age_s are cast to float8 in SQL rather than
// scanned as numeric: the view derives them with round(...::numeric, 3), and
// three decimal places of a duration is a number to read, not an exact
// quantity to do arithmetic on.
const endingColumns = `
  v.ended_at, v.lease_id::text, v.device_id::text, v.slot_id, v.job_id::text,
  v.tenant_id, v.fence, v.release_reason, v.ended_by,
  v.held_seconds::float8, v.heartbeat_age_s::float8,
  v.holder, v.protected, v.backfilled`

func scanLeaseEnding(sc scanner) (leaseEndingView, error) {
	var v leaseEndingView
	err := sc.Scan(
		&v.EndedAt, &v.LeaseID, &v.DeviceID, &v.SlotID, &v.JobID,
		&v.TenantID, &v.Fence, &v.ReleaseReason, &v.EndedBy,
		&v.HeldSeconds, &v.HeartbeatAgeS,
		&v.Holder, &v.Protected, &v.Backfilled,
	)
	return v, err
}

// endedByClasses is the vocabulary farm.lease_ended_by produces, in the order
// the axiom names them plus the one that means the axiom was not honoured.
//
// The function in migrations/00007_lease_events.sql is the authority; this
// list exists only so a typo in ?ended_by= is a 400 naming the five words
// instead of a 200 carrying an empty ledger, which at 3am reads as "nothing
// ended" — the most expensive wrong answer this route could give.
// test/assertions_v19.sql asserts the two lists are the same set, so a sixth
// class added in SQL fails the suite here rather than silently becoming
// unaskable.
var endedByClasses = []string{"job", "deadline", "operator", "reaper", "unaccounted"}

// endingLimits are this route's page-size bounds, deliberately the same as the
// lease listing's: an operator who has learned what --limit means on one lease
// view should not have to learn it again on the other.
const (
	defaultEndingLimit = 200
	maxEndingLimit     = 2000
)

// handleLeaseEndings serves GET /api/v1/leases/endings.
//
// This is the listing four runbooks used to open psql for: recent endings,
// endings with one release_reason, endings by the reaper in the last six
// hours, operator revokes in the last week.
//
// The plan is the flattering one, unlike GET /api/v1/events. farm.events has
// carried events_kind_time (kind, at DESC) since 00001_core.sql, and the view
// pins kind to a constant, so the ordering this route asks for is that index's
// own and "the newest N endings" is a range over it. Every other filter —
// reason, holder, tenant — is a predicate applied within that range rather than
// a reason to walk the timeline, so a tenant with no endings at all is bounded
// by how many endings the farm has and not by how much history it has
// accumulated. test/assertions_v19.sql keeps the index there; on a small
// database the planner will pick something else and be right to, which is why
// that assertion is structural rather than a reading of a plan.
func (s *Server) handleLeaseEndings(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", defaultEndingLimit, 1, maxEndingLimit)

	var (
		conds []string
		args  []any
	)
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	// A filter value the database has no word for is refused rather than
	// applied. Both of these vocabularies are closed sets, and answering a
	// misspelt one with an empty list would report "no lease ended that way"
	// about a question that was never asked.
	if reason := queryString(r, "reason"); reason != "" {
		if !slices.Contains(permittedReleaseReasons, reason) {
			badRequest(w, "reason must be one of the release reasons farm.leases permits",
				map[string]any{"rejected_reason": reason, "permitted_release_reasons": permittedReleaseReasons})
			return
		}
		conds = append(conds, "v.release_reason = "+bind(reason))
	}
	if class := queryString(r, "ended_by"); class != "" {
		if !slices.Contains(endedByClasses, class) {
			badRequest(w, "ended_by is the class of ender farm.lease_ended_by derives from the "+
				"release reason", map[string]any{"rejected_ended_by": class, "permitted": endedByClasses})
			return
		}
		conds = append(conds, "v.ended_by = "+bind(class))
	}

	// The confinement comes from the token, never from the query string. An
	// operator is already unscoped, so ?tenant= only narrows their view; for a
	// tenant-scoped caller the parameter is not read at all.
	if tenant := tenantScope(r.Context()); tenant != "" {
		conds = append(conds, "v.tenant_id = "+bind(tenant))
	} else if t := queryString(r, "tenant"); t != "" {
		conds = append(conds, "v.tenant_id = "+bind(t))
	}

	if dev := queryString(r, "device"); dev != "" {
		// A device is named by uuid in the ledger and by farm_uid on a rack
		// label, and an operator reading an alert has whichever the alert
		// printed. The uid half is a scalar subquery over a unique index, so
		// it is evaluated once for the statement; a uid that matches nothing
		// yields NULL, which compares false rather than raising.
		p := bind(dev)
		conds = append(conds, "(v.device_id::text = "+p+" OR v.device_id = "+deviceByUID(p)+")")
	}
	if job := queryString(r, "job"); job != "" {
		conds = append(conds, "v.job_id::text = "+bind(job))
	}
	if holder := queryString(r, "holder"); holder != "" {
		conds = append(conds, "v.holder = "+bind(holder))
	}

	// since is a LOOKBACK, not an instant, for the same reason it is one on
	// GET /api/v1/events: every ended_at here is a server clock, and comparing
	// it against a caller's clock makes the window silently wrong by however
	// far that clock is off — a short list rather than an error, at the moment
	// somebody is counting.
	if raw := queryString(r, "since"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			badRequest(w, fmt.Sprintf("since must be a positive duration such as 30m, 6h or 168h — a "+
				"window back from now, because every timestamp here is the server's, and there is "+
				"no day unit. Got %q", raw), nil)
			return
		}
		conds = append(conds, "v.ended_at >= now() - "+bind(intervalSeconds(d))+"::interval")
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	query := fmt.Sprintf("SELECT %s FROM farm.v_lease_endings v %s ORDER BY v.ended_at DESC LIMIT %s",
		endingColumns, where, bind(limit))

	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.fail(w, r, "list lease endings", err)
		return
	}
	defer rows.Close()

	out := make([]leaseEndingView, 0, 64)
	// The three summaries are computed over the rows returned, exactly as the
	// lease listing computes its state counts, and `truncated` says when that
	// is a page rather than the whole answer.
	byClass := map[string]int{}
	byReason := map[string]int{}
	byHolder := map[string]int{}
	unaccounted := 0
	for rows.Next() {
		v, err := scanLeaseEnding(rows)
		if err != nil {
			s.fail(w, r, "scan lease ending", err)
			return
		}
		if v.EndedBy != nil {
			byClass[*v.EndedBy]++
			if *v.EndedBy == "unaccounted" {
				unaccounted++
			}
		}
		if v.ReleaseReason != nil {
			byReason[*v.ReleaseReason]++
		}
		if v.Holder != nil && *v.Holder != "" {
			byHolder[*v.Holder]++
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, "read lease endings", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"endings": out,
		"counts":  byClass,
		"reasons": byReason,
		// Who was holding them. One holder dominating a page of reclaims is a
		// broken supervisor; many holders means the control plane is the
		// suspect, and that is a different runbook.
		"holders": byHolder,
		// A lease that reached a terminal state with no release_reason. The
		// whole system exists to make that impossible, so it is counted at the
		// top level rather than left to be spotted in a column.
		"unaccounted": unaccounted,
		// The limit ACTUALLY applied, after queryInt clamped it. A caller that
		// asked for 5000 is handed 2000 rows and truncated: true, and a client
		// that echoed the number it sent would tell the operator to raise a
		// limit that is already at its ceiling — advice that cannot work, at
		// the moment they are trying to see further back.
		"limit":     limit,
		"truncated": len(out) == limit,
	})
}

// handleLeaseEnding serves GET /api/v1/leases/{id}/ending: one lease, one
// answer.
//
// Three of the four outcomes are 200, and that is the design rather than
// laxity. "This lease has not ended" and "this lease is terminal and the
// ledger has no row for it" are both answers to the question asked, and the
// second is an escalation in docs/runbooks/lease-fenced.md — a lease that
// reached a terminal state without the trigger writing a row means something
// moved that row or moved the state out of band. Reporting either as 404
// would make the tool that is supposed to explain an incident exit non-zero
// during one, and would spell the finding as an absence.
//
// 404 is reserved for the case that really is one: no lease with this id, and
// no ledger row for it either.
func (s *Server) handleLeaseEnding(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		badRequest(w, "lease id must be a uuid", nil)
		return
	}

	// The ledger first: it is a probe of events_lease_ending, the partial
	// unique index 00007 built for exactly this question.
	ending, found, err := s.leaseEnding(r, id)
	if err != nil {
		s.fail(w, r, "get lease ending", err)
		return
	}

	// The lease row is read whichever way that went. It is what distinguishes
	// "still held" from "terminal and unrecorded", and it is the tenant of
	// record for a lease that has not ended yet. It may be absent while a
	// ledger row exists: farm.events is append-only and outlives what it
	// describes, which is the point of keeping history somewhere other than
	// the row being overwritten.
	var (
		leaseState  *string
		leaseTenant *string
	)
	err = s.pool.QueryRow(r.Context(),
		`SELECT l.state, l.tenant_id FROM farm.leases l WHERE l.id = $1::uuid`, id).
		Scan(&leaseState, &leaseTenant)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, "get lease for ending", err)
		return
	}

	if !found && leaseState == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such lease, and no ending recorded for that id", nil)
		return
	}

	// Ownership comes from the ledger row when there is one and from the lease
	// otherwise. A tenant asking about somebody else's lease is told the id
	// does not exist, exactly as GET /api/v1/jobs/{id} does: whether another
	// tenant's lease id is real is not this caller's business.
	if tenant := tenantScope(r.Context()); tenant != "" {
		owner := leaseTenant
		if found {
			owner = ending.TenantID
		}
		if owner == nil || *owner != tenant {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such lease, and no ending recorded for that id", nil)
			return
		}
	}

	body := map[string]any{
		"lease_id":    id,
		"ended":       found,
		"lease_state": leaseState,
		"ending":      nil,
	}
	if found {
		body["ending"] = ending
	}
	switch {
	case found:
		// The row speaks for itself.
	case leaseState == nil:
		body["note"] = "no lease row with this id survives, and the ledger holds an ending for it " +
			"only if `ended` is true above. Nothing here says a lease was ended incorrectly."
	case *leaseState == "held" || *leaseState == "suspect":
		body["note"] = "this lease has not ended: it is " + *leaseState + ". A suspect lease is " +
			"still LIVE — it has been marked for alerting and nothing has been released."
	default:
		body["note"] = "this lease is " + *leaseState + " and the ledger has no row for it. Every " +
			"ending writes one in the same transaction as the state change (migrations/" +
			"00007_lease_events.sql), so a terminal lease without one means the state or the row " +
			"was changed out of band. Escalate: see docs/runbooks/lease-fenced.md."
	}
	writeJSON(w, http.StatusOK, body)
}

// leaseEnding reads the ledger row for one lease. found is false when the
// ledger has nothing for it, which is not an error.
func (s *Server) leaseEnding(r *http.Request, leaseID string) (leaseEndingView, bool, error) {
	query := fmt.Sprintf("SELECT %s FROM farm.v_lease_endings v WHERE v.lease_id = $1::uuid", endingColumns)
	v, err := scanLeaseEnding(s.pool.QueryRow(r.Context(), query, leaseID))
	switch {
	case err == nil:
		return v, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return leaseEndingView{}, false, nil
	default:
		return leaseEndingView{}, false, err
	}
}
