package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The reaper's kill switch, and the only way to reach it that is not psql.
//
// # Why this file exists
//
// farm.reaper_state.enabled is read by farm.lease_reclaim on every call, and
// farm.lease_reclaim is THE ONLY AUTOMATIC RELEASE PATH IN THE SYSTEM. That one
// boolean is therefore the lever that stops the farm taking devices back from
// running jobs. Until this file, it was reachable only by somebody with a
// database shell — which is the wrong constraint at two in the morning, and a
// constraint that pushes the person under pressure towards an unaudited UPDATE
// against a table with no history.
//
// # What the switch does and does not stop
//
// It gates reclamation and nothing else. A lease still ends when its job ends,
// when farm.jobs.max_runtime — a number the user wrote down — elapses, and when
// a human revokes it. farm.lease_mark_suspect still runs, so leases still go
// suspect and still alert; suspect releases nothing and a heartbeat inside the
// grace band still heals one back to held at the same fence. Disabling the
// reaper is not a freeze on the farm, and an operator who believes it is will
// be surprised by a max_runtime expiry. The responses below say so in words.
//
// # The asymmetry between off and on
//
// Turning it OFF is safe in the only direction that matters: nothing is
// reclaimed, so nothing can be taken from a job that is still working.
//
// Turning it ON is not, and the reason is the same one farm.reaper_arm exists
// for. While the reaper is off, leases keep aging: holders that stopped
// heartbeating pile up past their reclaimable_at with nothing to sweep them.
// Flipping enabled back to true would hand that entire accumulated backlog to
// the next sweep, ten seconds later, in batches of a hundred — a mass reclaim
// at the instant of restoration, which is precisely the failure the quiesce
// gate was built to prevent on a cold start.
//
// The off period is itself a control-plane gap in the reclaim path. It just is
// not one farm.control_plane_gap knows about, because the components kept
// heartbeating the whole time. So enabling here does what the reaper does on
// every gain of leadership, in the same transaction as the flip: it calls
// farm.reaper_arm, which records and refunds any real component outage AND
// sets quiesce_until to now() plus the longest live TTL. Every live holder then
// has a full TTL to renew before the first sweep can reclaim anything, and the
// response says exactly when that window closes.
//
// Enabling is therefore never instant, and that is the point. An operator who
// wants it instant does not want this endpoint; they want to explain why in the
// audit row, which is required.

// reaperArmComponents is left to farm.reaper_arm's own default
// (ARRAY['reaper','api','scheduler']). The set of components on the renewal
// path is a property of the schema, not of whoever is calling: naming it here
// would let an API deployment disagree with the reaper about what counts as a
// control-plane outage, and the disagreement would only ever be discovered
// during one.
const reaperArmSQL = `SELECT extract(epoch FROM farm.reaper_arm())::float8`

// reaperGapView is the most recent recorded control-plane outage.
type reaperGapView struct {
	Component string    `json:"component"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Seconds   float64   `json:"seconds"`

	// ShieldsReclaim reports that this gap is inside the six-hour window
	// farm.lease_reclaim consults, so a lease whose silence overlaps it cannot
	// be reclaimed at all. It is the difference between "we had an outage" and
	// "that outage is still protecting leases right now".
	ShieldsReclaim bool `json:"shields_reclaim"`
}

// reaperStateView is the answer to "is the reaper armed right now", which is
// the question an operator actually asks. `enabled` alone does not answer it:
// an enabled reaper inside a quiesce window reclaims nothing, and an enabled
// reaper whose process is dead reclaims nothing either.
type reaperStateView struct {
	Enabled bool `json:"enabled"`

	// Armed is enabled AND out of quiesce. It is the operational answer.
	Armed bool `json:"armed"`

	QuiesceUntil     time.Time `json:"quiesce_until"`
	QuiesceRemaining float64   `json:"quiesce_remaining_seconds"`
	ArmedAt          time.Time `json:"armed_at"`

	// Now is the server's clock. Every countdown above is computed by Postgres
	// against it, so a client never has to subtract its own clock from a
	// server timestamp to learn how long is left.
	Now time.Time `json:"now"`

	HeartbeatAt      *time.Time `json:"reaper_heartbeat_at,omitempty"`
	HeartbeatAgeSecs *float64   `json:"reaper_heartbeat_age_seconds,omitempty"`

	LiveLeases      int64 `json:"live_leases"`
	ProtectedLeases int64 `json:"protected_leases"`
	SuspectLeases   int64 `json:"suspect_leases"`

	// ReclaimableNow counts the leases the next sweep would take if the gate
	// opened this instant. It is the number worth reading before flipping the
	// switch back on, and it is computed with the same predicates
	// farm.lease_reclaim uses.
	ReclaimableNow int64 `json:"reclaimable_now"`

	RecentGap *reaperGapView `json:"recent_gap,omitempty"`

	Note string `json:"note,omitempty"`
}

// reaperChangeView is what a write returns: the state afterwards, in exactly
// the shape a read returns it, plus what the write itself did.
//
// The embedding is the point. A hand-built map for the write reply is a second
// definition of the same document, and the first field somebody adds to one and
// not the other is a client that reads a null where the GET has a value.
type reaperChangeView struct {
	reaperStateView

	PreviousEnabled bool `json:"previous_enabled"`
	Changed         bool `json:"changed"`

	// GapRefundSeconds is what farm.reaper_arm handed back to every live
	// lease. It is a pointer because zero and absent are different claims:
	// "there was no control-plane outage to refund" and "this call did not
	// arm at all", which is every disable.
	GapRefundSeconds *float64 `json:"gap_refund_seconds,omitempty"`

	ArmedNote    string `json:"armed_note,omitempty"`
	DisabledNote string `json:"disabled_note,omitempty"`
}

// reaperStateQuery answers everything in one row, against one now().
//
// Splitting it would let the census and the quiesce deadline come from
// different transaction timestamps, and the whole value of this endpoint is
// that the numbers in it describe one instant.
const reaperStateQuery = `
WITH st AS (
  SELECT enabled, quiesce_until, armed_at FROM farm.reaper_state WHERE singleton
), hb AS (
  SELECT beat_at FROM farm.component_heartbeat WHERE component = $1
), cen AS (
  SELECT count(*)                                        AS live,
         count(*) FILTER (WHERE l.protected)              AS protected,
         count(*) FILTER (WHERE l.state = 'suspect')      AS suspect,
         count(*) FILTER (
           WHERE l.state = 'suspect'
             AND l.reclaimable_at < now()
             AND l.protected = false
             AND (l.witness_at IS NULL OR l.witness_at < now() - l.grace)
             AND NOT EXISTS (
                   SELECT 1 FROM farm.control_plane_gap g
                    WHERE g.ended_at > l.heartbeat_at
                      AND g.ended_at > now() - interval '6 hours')
         )                                                AS reclaimable
    FROM farm.leases l
   WHERE l.state IN ('held','suspect')
), gap AS (
  SELECT component, started_at, ended_at
    FROM farm.control_plane_gap
   ORDER BY ended_at DESC
   LIMIT 1
)
SELECT st.enabled,
       st.quiesce_until,
       st.armed_at,
       now(),
       extract(epoch FROM greatest(st.quiesce_until - now(), interval '0'))::float8,
       hb.beat_at,
       extract(epoch FROM now() - hb.beat_at)::float8,
       cen.live, cen.protected, cen.suspect, cen.reclaimable,
       gap.component, gap.started_at, gap.ended_at,
       extract(epoch FROM gap.ended_at - gap.started_at)::float8,
       (gap.ended_at > now() - interval '6 hours')
  FROM st
  LEFT JOIN hb  ON true
  LEFT JOIN cen ON true
  LEFT JOIN gap ON true`

// readReaperState runs [reaperStateQuery] against q, which is the pool for a
// read and a transaction for a write that wants to see its own effect.
func readReaperState(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, component string) (*reaperStateView, error) {

	var (
		v        reaperStateView
		gapComp  *string
		gapStart *time.Time
		gapEnd   *time.Time
		gapSecs  *float64
		gapShiel *bool
	)
	err := q.QueryRow(ctx, reaperStateQuery, component).Scan(
		&v.Enabled, &v.QuiesceUntil, &v.ArmedAt, &v.Now, &v.QuiesceRemaining,
		&v.HeartbeatAt, &v.HeartbeatAgeSecs,
		&v.LiveLeases, &v.ProtectedLeases, &v.SuspectLeases, &v.ReclaimableNow,
		&gapComp, &gapStart, &gapEnd, &gapSecs, &gapShiel)
	if err != nil {
		return nil, err
	}
	v.Armed = v.Enabled && v.QuiesceRemaining <= 0
	if gapComp != nil && gapStart != nil && gapEnd != nil {
		g := reaperGapView{Component: *gapComp, StartedAt: *gapStart, EndedAt: *gapEnd}
		if gapSecs != nil {
			g.Seconds = *gapSecs
		}
		if gapShiel != nil {
			g.ShieldsReclaim = *gapShiel
		}
		v.RecentGap = &g
	}
	return &v, nil
}

// reaperNote is the sentence that goes with a state, in the words an operator
// needs rather than the words the column uses.
func reaperNote(v *reaperStateView) string {
	switch {
	case !v.Enabled:
		return fmt.Sprintf(
			"the reaper is OFF: farm.lease_reclaim returns without reclaiming anything, so no lease "+
				"will be ended by reclamation. %d live lease(s) are untouched by this — nothing here "+
				"released anything. This is NOT a freeze: a job that ends still releases its lease, "+
				"farm.jobs.max_runtime still expires one, and an operator revoke still works. "+
				"%d lease(s) would be reclaimed by the next sweep if this were turned back on now",
			v.LiveLeases, v.ReclaimableNow)
	case v.QuiesceRemaining > 0:
		return fmt.Sprintf(
			"the reaper is ON but QUIESCED for another %.0fs (until %s): it will reclaim nothing "+
				"before then, which gives every live holder time to renew. %d lease(s) are currently "+
				"eligible and would be swept after the window closes",
			v.QuiesceRemaining, v.QuiesceUntil.UTC().Format(time.RFC3339), v.ReclaimableNow)
	default:
		return fmt.Sprintf(
			"the reaper is ARMED: the next sweep reclaims leases whose holder stopped heartbeating "+
				"for ttl+grace with no witness, no protection and no control-plane gap over their "+
				"silence. %d lease(s) currently qualify",
			v.ReclaimableNow)
	}
}

// RegisterReaperAdmin mounts the kill switch.
//
// Every route is operator, the read included. The read is not a fleet
// statistic: it names the state of the mechanism that ends leases, and the
// people who need it are the same people allowed to change it. It is mounted
// from the parent rather than declared in the router's own table for the same
// reason the artifact routes are — see [Server.WithRoutes] — and the roles are
// chosen here, at registration, exactly as they are everywhere else in this
// package.
func (s *Server) RegisterReaperAdmin(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/reaper", s.requireRole(RoleOperator, http.HandlerFunc(s.handleReaperGet)))
	mux.Handle("POST /api/v1/reaper/disable", s.requireRole(RoleOperator, http.HandlerFunc(s.handleReaperDisable)))
	mux.Handle("POST /api/v1/reaper/enable", s.requireRole(RoleOperator, http.HandlerFunc(s.handleReaperEnable)))
}

// handleReaperGet serves GET /api/v1/reaper.
func (s *Server) handleReaperGet(w http.ResponseWriter, r *http.Request) {
	v, err := readReaperState(r.Context(), s.pool, reaperComponent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// farm.reaper_state is a singleton the migration inserts. No row
			// means the schema is not the one this binary was built against,
			// and reporting that as an empty state would read as "the reaper
			// is off", which is a claim about the farm rather than about the
			// database.
			s.fail(w, r, "read reaper state", errors.New("farm.reaper_state has no singleton row"))
			return
		}
		s.fail(w, r, "read reaper state", err)
		return
	}
	v.Note = reaperNote(v)
	writeJSON(w, http.StatusOK, v)
}

// reaperComponent is the farm.component_heartbeat key the reaper writes. It
// matches reaper.DefaultComponent, and it is spelled here rather than imported
// so that internal/api does not depend on the package that does the reclaiming
// — the one-way dependency is part of how "the API cannot release a device"
// stays true.
const reaperComponent = "reaper"

// handleReaperDisable serves POST /api/v1/reaper/disable.
func (s *Server) handleReaperDisable(w http.ResponseWriter, r *http.Request) {
	s.setReaperEnabled(w, r, false, "reaper.disable")
}

// handleReaperEnable serves POST /api/v1/reaper/enable.
func (s *Server) handleReaperEnable(w http.ResponseWriter, r *http.Request) {
	s.setReaperEnabled(w, r, true, "reaper.enable")
}

func (s *Server) setReaperEnabled(w http.ResponseWriter, r *http.Request, enabled bool, action string) {
	var req revokeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		badRequest(w, "reason is required: this switch controls the only automatic release path "+
			"in the farm, and the audit row must say why it was moved", nil)
		return
	}
	who := actor(r.Context())

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.fail(w, r, action+": begin", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.Context())) }()

	// The previous value comes from a CTE rather than a sub-SELECT in
	// RETURNING: a CTE is evaluated against the snapshot before the UPDATE by
	// definition, so "what was it before" cannot quietly become "what is it
	// now" under a different plan.
	var previous bool
	err = tx.QueryRow(r.Context(), `
WITH prev AS (SELECT enabled FROM farm.reaper_state WHERE singleton)
UPDATE farm.reaper_state SET enabled = $1
 WHERE singleton
RETURNING (SELECT enabled FROM prev)`, enabled).Scan(&previous)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.fail(w, r, action, errors.New("farm.reaper_state has no singleton row"))
			return
		}
		s.fail(w, r, action, err)
		return
	}

	// Arming is part of enabling, in the same transaction, and it happens
	// whether or not the previous value was already true: an operator who
	// re-sends enable is asking for the reaper to be safe to run, and a
	// no-op that leaves an accumulated backlog pointed at the next sweep is
	// not that. See the file comment for why the off period is itself a gap.
	var refundSeconds float64
	if enabled {
		if err := tx.QueryRow(r.Context(), reaperArmSQL).Scan(&refundSeconds); err != nil {
			s.fail(w, r, action+": arm", err)
			return
		}
	}

	// Read back inside the transaction, so the quiesce deadline in the reply is
	// the one this call just wrote and not one a concurrent arm moved.
	v, err := readReaperState(r.Context(), tx, reaperComponent)
	if err != nil {
		s.fail(w, r, action+": read back", err)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, r, action+": commit", err)
		return
	}

	detail := map[string]any{
		"previous_enabled": previous,
		"new_enabled":      enabled,
		"live_leases":      v.LiveLeases,
		"protected_leases": v.ProtectedLeases,
		"suspect_leases":   v.SuspectLeases,
		"reclaimable_now":  v.ReclaimableNow,
		"quiesce_until":    v.QuiesceUntil,
	}
	if enabled {
		detail["gap_refund_seconds"] = refundSeconds
	}
	// The switch has already moved. Its audit row must not depend on the
	// operator's connection outliving their own command: "who turned the
	// reaper off at 02:14, and why" is the question this row exists to answer.
	bookCtx, bookCancel := detachedCtx(r.Context())
	s.auditAction(bookCtx, who, action, "reaper", reason, detail)
	s.recordEvent(bookCtx, eventRow{Kind: strings.ReplaceAll(action, ".", "_"), Actor: who, Detail: detail})
	bookCancel()
	s.metrics.operatorActions.WithLabelValues(action, "ok").Inc()

	// Warn, not Info: this is a change to the mechanism that ends leases, and
	// it belongs in the same class of log line as a revoke.
	s.log.WarnContext(r.Context(), "reaper kill switch moved by operator",
		"action", action, "enabled", enabled, "previous", previous, "actor", who, "reason", reason,
		"live_leases", v.LiveLeases, "reclaimable_now", v.ReclaimableNow,
		"quiesce_until", v.QuiesceUntil, "gap_refund_seconds", refundSeconds)

	v.Note = reaperNote(v)
	body := reaperChangeView{
		reaperStateView: *v,
		PreviousEnabled: previous,
		Changed:         previous != enabled,
	}
	if enabled {
		body.GapRefundSeconds = &refundSeconds
		body.ArmedNote = fmt.Sprintf(
			"farm.reaper_arm ran with this change, exactly as it does when the reaper gains "+
				"leadership: %.0fs of control-plane outage was refunded to every live lease, and the "+
				"reaper is quiesced until %s so nothing is reclaimed at the instant of restoration. "+
				"Turning the reaper on is never instant, and that is deliberate",
			refundSeconds, v.QuiesceUntil.UTC().Format(time.RFC3339))
	} else {
		body.DisabledNote = "reclamation is stopped. Jobs still release their own leases, " +
			"max_runtime still expires them, and an operator revoke still works — this switch " +
			"only stops farm.lease_reclaim"
	}
	writeJSON(w, http.StatusOK, body)
}
