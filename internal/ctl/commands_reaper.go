package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// `ctl reaper` reads and moves the kill switch for automatic reclamation.
//
// farm.reaper_state.enabled gates farm.lease_reclaim, which is the only
// automatic release path in the system. Before this command the only way to
// move it was a psql session against the production database — an unaudited
// UPDATE, typed by whoever was most under pressure, on a table with no history.
// Everything here goes through the API like every other command in this
// package, so the switch cannot be moved without a name and a reason landing in
// farm.audit_log.
//
// The read is the more common use. "Is the reaper armed right now" is not the
// same question as "is enabled true": an enabled reaper inside its quiesce
// window reclaims nothing, and an enabled reaper whose process died reclaims
// nothing either. The listing below shows all three facts on their own lines.

// reaperStateResponse is the shape of GET /api/v1/reaper, and of the state
// half of the enable and disable replies.
type reaperStateResponse struct {
	Enabled          bool       `json:"enabled"`
	Armed            bool       `json:"armed"`
	QuiesceUntil     *time.Time `json:"quiesce_until"`
	QuiesceRemaining float64    `json:"quiesce_remaining_seconds"`
	ArmedAt          *time.Time `json:"armed_at"`
	Now              *time.Time `json:"now"`

	HeartbeatAt      *time.Time `json:"reaper_heartbeat_at"`
	HeartbeatAgeSecs *float64   `json:"reaper_heartbeat_age_seconds"`

	LiveLeases      int64 `json:"live_leases"`
	ProtectedLeases int64 `json:"protected_leases"`
	SuspectLeases   int64 `json:"suspect_leases"`
	ReclaimableNow  int64 `json:"reclaimable_now"`

	RecentGap *struct {
		Component      string     `json:"component"`
		StartedAt      *time.Time `json:"started_at"`
		EndedAt        *time.Time `json:"ended_at"`
		Seconds        float64    `json:"seconds"`
		ShieldsReclaim bool       `json:"shields_reclaim"`
	} `json:"recent_gap"`

	Note string `json:"note"`
}

// reaperChangeResponse adds what the write said about itself.
type reaperChangeResponse struct {
	reaperStateResponse
	PreviousEnabled bool     `json:"previous_enabled"`
	Changed         bool     `json:"changed"`
	GapRefundSecs   *float64 `json:"gap_refund_seconds"`
	ArmedNote       string   `json:"armed_note"`
	DisabledNote    string   `json:"disabled_note"`
}

func cmdReaper(ctx context.Context, s *session, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "disable", "enable":
			return reaperSet(ctx, s, args[0], args[1:])
		case "-h", "--help":
		default:
			// A flag is a flag; anything else that is not a known verb is a
			// typo, and guessing which of the two directions it meant is not a
			// guess this command gets to make.
			if args[0][0] != '-' {
				return usageErrf("ctl reaper takes disable or enable, not %q", args[0])
			}
		}
	}
	return reaperShow(ctx, s, args)
}

func reaperShow(ctx context.Context, s *session, args []string) error {
	fs := newFlags("reaper", s.err)
	var g globals
	g.bind(fs)
	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("usage: ctl reaper  |  ctl reaper disable|enable --reason r")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	state, raw, err := fetch[reaperStateResponse](ctx, e.client, apiPrefix+"/reaper", nil)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	return e.out.Fields(reaperFields(&state))
}

// reaperFields renders the state.
//
// Every countdown printed here was computed by Postgres against its own now()
// and arrives already counted down. None of it is derived by subtracting this
// machine's clock from a server timestamp: an operator deciding whether the
// reaper is about to sweep must not be reading a number that depends on their
// laptop's clock being right.
func reaperFields(v *reaperStateResponse) *Fields {
	f := &Fields{}
	f.Add("enabled", yesNo(v.Enabled))
	switch {
	case !v.Enabled:
		f.Add("armed", "no — the switch is off, nothing is reclaimed")
	case v.QuiesceRemaining > 0:
		f.Addf("armed", "no — quiesced for another %s", duration(int64(v.QuiesceRemaining)))
	default:
		f.Add("armed", "YES — the next sweep can reclaim")
	}
	f.Add("quiesce until", stamp(v.QuiesceUntil))
	f.Add("last armed", stamp(v.ArmedAt))

	if v.HeartbeatAt == nil {
		// Worth a line of its own: an enabled reaper that is not running is a
		// farm where nothing is being reclaimed no matter what this switch
		// says, and the two look identical from the switch alone.
		f.Add("reaper heartbeat", "none — no reaper process has ever beaten here")
	} else if v.HeartbeatAgeSecs != nil {
		f.Addf("reaper heartbeat", "%s ago (%s)", duration(int64(*v.HeartbeatAgeSecs)), stamp(v.HeartbeatAt))
	} else {
		f.Add("reaper heartbeat", stamp(v.HeartbeatAt))
	}

	f.Gap()
	f.Addf("live leases", "%d, of which %d protected", v.LiveLeases, v.ProtectedLeases)
	f.Addf("suspect leases", "%d", v.SuspectLeases)
	f.Addf("reclaimable now", "%d — what the next sweep would take if it ran unquiesced", v.ReclaimableNow)

	if g := v.RecentGap; g != nil {
		shield := ""
		if g.ShieldsReclaim {
			shield = " — still inside the 6h window, so leases silent across it cannot be reclaimed"
		}
		f.Addf("recent control-plane gap", "%s, %s, ended %s%s",
			g.Component, duration(int64(g.Seconds)), stamp(g.EndedAt), shield)
	}
	if v.Note != "" {
		f.Gap()
		f.Add("note", v.Note)
	}
	return f
}

func reaperSet(ctx context.Context, s *session, action string, args []string) error {
	fs := newFlags("reaper "+action, s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("usage: ctl reaper %s --reason r", action)
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("reaper " + action); err != nil {
		return err
	}

	// Preflight. Both directions have a blast radius worth printing before the
	// question: disabling leaves a farm that never takes a device back, and
	// enabling points the next sweep at whatever accumulated while it was off.
	f := &Fields{}
	before, _, readErr := fetch[reaperStateResponse](ctx, e.client, apiPrefix+"/reaper", nil)
	if readErr != nil {
		e.warnf("could not read the reaper state for the preflight (%v); the server remains the authority", readErr)
	} else {
		f.Add("currently enabled", yesNo(before.Enabled))
		f.Addf("live leases", "%d, of which %d protected", before.LiveLeases, before.ProtectedLeases)
		f.Addf("suspect leases", "%d", before.SuspectLeases)
		f.Addf("reclaimable now", "%d", before.ReclaimableNow)
		if before.Enabled == (action == "enable") {
			f.Add("note", "this is already the state; the call is still audited and, for enable, still re-arms")
		}
	}

	headline := "About to DISABLE the reaper: farm.lease_reclaim will stop reclaiming.\n" +
		"Nothing is released by this and no live lease is touched. Jobs still release their own\n" +
		"leases, max_runtime still expires them, and revoke still works — but a holder that dies\n" +
		"while this is off keeps its device until a human takes it back."
	if action == "enable" {
		headline = "About to ENABLE the reaper: automatic reclamation resumes.\n" +
			"It does not resume instantly. The server re-arms in the same transaction, which sets a\n" +
			"quiesce window of the longest live TTL, so every holder gets a full TTL to renew before\n" +
			"the first sweep. The leases counted above are what that sweep would then consider."
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/reaper/"+action, map[string]string{"reason": e.reason})
	if err != nil {
		return e.unknownOutcome(err, "ctl reaper")
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	var res reaperChangeResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := reaperFields(&res.reaperStateResponse)
	out.Gap()
	changed := ""
	if res.Changed {
		changed = " (changed)"
	}
	out.Addf("previously enabled", "%s%s", yesNo(res.PreviousEnabled), changed)
	if res.GapRefundSecs != nil {
		out.Addf("gap refunded", "%s added to every live lease", duration(int64(*res.GapRefundSecs)))
	}
	if res.ArmedNote != "" {
		out.Add("armed", res.ArmedNote)
	}
	if res.DisabledNote != "" {
		out.Add("stopped", res.DisabledNote)
	}
	if err := e.out.Fields(out); err != nil {
		return err
	}
	if action == "disable" {
		// Said on stderr so it survives being piped into jq, and said at all
		// because the failure mode of this switch is forgetting it is off.
		e.warnf("\n%s", fmt.Sprintf(
			"the reaper is now OFF. Nothing will reclaim a lease until it is turned back on with "+
				"`ctl reaper enable --reason ...`; devices held by dead holders will accumulate."))
	}
	return nil
}
