package ctl

import (
	"context"
	"flag"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// `ctl endings` — how leases ended, without a database session.
//
// A lease ends when the job says so, when a deadline the user wrote down
// elapses, or when a human takes it back. This is the command that checks it.
//
// The record has existed since migration 00007_lease_events.sql: a trigger on
// farm.leases writes one 'lease_ended' row inside the same transaction as
// every state change, and farm.v_lease_endings is the view that turns its
// jsonb into columns. What did not exist was any way to read it that was not
// psql, so every runbook in docs/runbooks told a person at 3am to open one —
// against the production database, during an incident, to answer the first
// question of the review. This verb and GET /api/v1/leases/endings behind it
// are that answer as one command.
//
// Two shapes, because there are two questions. With a lease id it is "how did
// THIS one end", which is what an alert names. Without one it is "what has
// been ending", which is what a burst of fences or a mass reclaim looks like
// from the outside.

// leaseEnding is one row of farm.v_lease_endings as the API serves it. Every
// nullable column is a pointer because the ledger is allowed to be honest
// about what it does not know — release_reason is NULL for exactly the ending
// nobody recorded a reason for, which is the failure the whole system exists
// to prevent and is reported as ended_by "unaccounted".
type leaseEnding struct {
	EndedAt       time.Time `json:"ended_at"`
	LeaseID       *string   `json:"lease_id"`
	DeviceID      *string   `json:"device_id"`
	SlotID        *int64    `json:"slot_id"`
	JobID         *string   `json:"job_id"`
	TenantID      *string   `json:"tenant_id"`
	Fence         *int64    `json:"fence"`
	ReleaseReason *string   `json:"release_reason"`
	EndedBy       *string   `json:"ended_by"`
	HeldSeconds   *float64  `json:"held_seconds"`
	HeartbeatAgeS *float64  `json:"heartbeat_age_s"`
	Holder        *string   `json:"holder"`
	Protected     *bool     `json:"protected"`
	Backfilled    bool      `json:"backfilled"`
}

type endingsResponse struct {
	Endings []leaseEnding `json:"endings"`
	// The four summaries are computed by the server over the rows it returned,
	// not over the whole window the filters describe. That is what makes
	// Truncated worth printing loudly: on a cut page they are a sample, and the
	// judgement they exist for — one holder dominating a list of reclaims, or
	// none — is exactly the one a sample can get wrong.
	Counts      map[string]int `json:"counts"`
	Reasons     map[string]int `json:"reasons"`
	Holders     map[string]int `json:"holders"`
	Unaccounted int            `json:"unaccounted"`
	// Limit is the page size the server actually applied, which is not
	// necessarily the one that was asked for: it clamps. Telling somebody to
	// raise a --limit that is already at the server's ceiling is advice that
	// cannot work, given at the moment they are trying to see further back.
	Limit     int  `json:"limit"`
	Truncated bool `json:"truncated"`
}

type endingResponse struct {
	LeaseID    string       `json:"lease_id"`
	Ended      bool         `json:"ended"`
	LeaseState *string      `json:"lease_state"`
	Ending     *leaseEnding `json:"ending"`
	Note       string       `json:"note"`
}

// endedByOrder is the order the summary line prints the classes in: the three
// endings the axiom permits, then the one the control plane performs, then the
// one that means nobody recorded anything.
var endedByOrder = []string{"job", "deadline", "operator", "reaper", "unaccounted"}

func cmdEndings(ctx context.Context, s *session, args []string) error {
	fs := newFlags("endings", s.err)
	var g globals
	g.bind(fs)
	reason := fs.String("reason", "", "only endings with this release reason "+
		"(completed, failed, job_cancelled, max_runtime, operator_revoked, holder_expired, device_retired)")
	endedBy := fs.String("ended-by", "", "only endings of this class: job, deadline, operator, reaper or unaccounted")
	dev := fs.String("device", "", "only endings on this device id or farm uid")
	jobFlag := fs.String("job", "", "only endings of this job's leases")
	holder := fs.String("holder", "", "only endings whose holder was this")
	tenantFlag := fs.String("tenant", "", "only this tenant's endings (ignored for a tenant-scoped token, "+
		"which is already confined to its own)")
	since := fs.String("since", "", "only endings within this long ago, e.g. 30m, 6h, 24h")
	// Smaller than the API's own default of 200: this is a table a person
	// reads on one screen during an incident, not a page a script consumes.
	// --limit reaches further back and -o json carries whatever it returns.
	limit := fs.Int("limit", 50, "maximum endings to return")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usageErrf("usage: ctl endings [<lease id>] [--reason r] [--ended-by b] [--since d]")
	}
	if *since != "" {
		if _, err := time.ParseDuration(*since); err != nil {
			return usageErrf("--since %q is not a duration (want e.g. 30m, 6h, 24h)", *since)
		}
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if len(rest) == 1 {
		// A lease id and a filter are two different questions, and answering
		// the first while silently dropping the second is how a listing lies:
		// `ctl endings <id> --ended-by reaper` would print that lease's ending
		// whoever ended it, and be read as confirmation that the reaper did.
		if stray := filterFlags(fs); len(stray) > 0 {
			return usageErrf("a lease id asks about ONE lease and %s narrow a listing; drop one "+
				"or the other. `ctl endings %s` answers how that lease ended, whatever ended it.",
				strings.Join(stray, ", "), rest[0])
		}
		return e.oneEnding(ctx, rest[0])
	}

	q := url.Values{}
	setIf(q, "reason", *reason)
	setIf(q, "ended_by", *endedBy)
	setIf(q, "device", *dev)
	setIf(q, "job", *jobFlag)
	setIf(q, "holder", *holder)
	setIf(q, "tenant", *tenantFlag)
	setIf(q, "since", *since)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[endingsResponse](ctx, e.client, apiPrefix+"/leases/endings", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	t := NewTable("ENDED", "ENDED BY", "REASON", "HELD", "BEAT AGE", "HOLDER", "TENANT", "JOB", "LEASE")
	backfilled := 0
	for _, v := range resp.Endings {
		if v.Backfilled {
			backfilled++
		}
		class := dash(v.EndedBy)
		if v.Protected != nil && *v.Protected {
			class += "*"
		}
		t.Row(stamp(&v.EndedAt), class, dash(v.ReleaseReason),
			seconds(v.HeldSeconds), seconds(v.HeartbeatAgeS),
			dash(v.Holder), dash(v.TenantID), shortID(str(v.JobID)), dash(v.LeaseID))
	}
	if t.Len() == 0 {
		e.out.Text("no lease ending matched. The ledger records every ending since " +
			"migration 00007; an empty result here means none matched the filters, not that none happened.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Blank()
	// "listed", not "endings": every count below is over the rows on this page.
	// With --limit reached they are a sample of the window, and the warning
	// further down says so.
	e.out.Text("%d endings listed: %s", len(resp.Endings), countsLine(resp.Counts, endedByOrder...))
	e.out.Text("reasons: %s", countsLine(resp.Reasons, "completed", "failed", "job_cancelled",
		"max_runtime", "operator_revoked", "holder_expired", "device_retired"))
	if len(resp.Holders) > 1 {
		// One holder dominating a page of reclaims is a broken supervisor;
		// many holders means it is the control plane, which is a different
		// runbook. With one holder the table already says so.
		e.out.Text("by holder: %s", countsLine(resp.Holders))
	}
	e.out.Text("job = the job said so; deadline = a max_runtime the user wrote down elapsed; " +
		"operator = a human revoked it; reaper = the holder stopped beating. * marks a lease that " +
		"was protected — the reaper never takes one of those back on its own.")
	if backfilled > 0 {
		e.warnf("%d of these rows were reconstructed from farm.leases by the 30-day backfill in "+
			"migration 00007 rather than written when the lease ended; their beat age is measured "+
			"against the last beat the lease ever recorded. -o json flags each one.", backfilled)
	}
	if resp.Unaccounted > 0 {
		e.warnf("%d lease(s) here ended with NO release reason recorded. A lease ends when its job "+
			"says so, when a deadline elapses, or when a human takes it back — an ending that names "+
			"none of those is the failure this system exists to prevent. Escalate.", resp.Unaccounted)
	}
	if resp.Truncated {
		// The server's own limit, because it clamps: asking for 5000 yields
		// 2000 rows, and "raise --limit past 5000" would be a wrong number AND
		// impossible advice. A server that echoes none leaves this at what was
		// asked for, which is the best that can be said then.
		cut := resp.Limit
		if cut <= 0 {
			cut = *limit
		}
		e.warnf("the listing hit its limit of %d endings and was cut there; older endings were not "+
			"searched, and the counts above cover only these %d rows. Raise --limit or narrow with --since.",
			cut, len(resp.Endings))
	}
	return nil
}

// filterFlags names the listing filters the caller actually set, so a lease id
// given alongside one can be refused rather than quietly losing to it. fs.Visit
// walks only the flags that were passed, which is what separates a "--limit 50"
// somebody typed from the default of the same value.
func filterFlags(fs *flag.FlagSet) []string {
	var set []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "reason", "ended-by", "device", "job", "holder", "tenant", "since", "limit":
			set = append(set, "--"+f.Name)
		}
	})
	return set
}

// oneEnding answers "how did THIS lease end" for a single id.
//
// It exits 0 for all three answers the server can give — it ended, it has not
// ended, or it is terminal with nothing in the ledger — because each is an
// answer to the question. The third is an incident, and it is reported as a
// warning on stderr rather than as a failed command: a tool that exits
// non-zero while explaining an outage is a tool somebody stops trusting during
// one.
func (e *env) oneEnding(ctx context.Context, leaseID string) error {
	resp, raw, err := fetch[endingResponse](ctx, e.client,
		apiPrefix+"/leases/"+url.PathEscape(leaseID)+"/ending", nil)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	f := &Fields{}
	f.Add("lease", resp.LeaseID)
	f.Add("ended", yesNo(resp.Ended))
	if v := resp.Ending; v != nil {
		f.Addf("ended at", "%s (%s)", stamp(&v.EndedAt), ago(&v.EndedAt))
		f.Add("ended by", dash(v.EndedBy))
		f.Add("release reason", dash(v.ReleaseReason))
		f.Gap()
		f.Add("held for", seconds(v.HeldSeconds))
		f.Add("heartbeat age at the end", seconds(v.HeartbeatAgeS))
		f.Add("holder", dash(v.Holder))
		f.Add("protected", yesNo(v.Protected != nil && *v.Protected))
		f.Gap()
		f.Add("tenant", dash(v.TenantID))
		f.Add("job", dash(v.JobID))
		f.Add("device", dash(v.DeviceID))
		f.Add("slot", dashInt64(v.SlotID))
		f.Add("fence", dashInt64(v.Fence))
		source := "the ledger, written when the lease ended"
		if v.Backfilled {
			source = "reconstructed from farm.leases by migration 00007's backfill, " +
				"not written at the time"
		}
		f.Add("source", source)
	} else {
		f.Add("lease state", dash(resp.LeaseState))
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}
	if resp.Note != "" {
		e.out.Blank()
		e.out.Text("%s", resp.Note)
	}

	// The two findings worth waking somebody for, on stderr so a pipeline
	// cannot swallow them.
	if v := resp.Ending; v != nil && v.EndedBy != nil && *v.EndedBy == "unaccounted" {
		e.warnf("this lease ended with no release reason recorded. That names none of the three " +
			"ways a lease may end, and it is the failure this system exists to prevent. Escalate.")
	}
	if !resp.Ended && resp.LeaseState != nil &&
		*resp.LeaseState != "held" && *resp.LeaseState != "suspect" {
		e.warnf("lease %s is %s and the ledger has no row for it. Every ending writes one in the "+
			"same transaction as the state change, so this means the row or the state was changed "+
			"out of band. See docs/runbooks/lease-fenced.md.", resp.LeaseID, *resp.LeaseState)
	}
	return nil
}

// seconds renders one of the ledger's fractional-second durations.
//
// The view rounds to three decimal places, so a lease that lived under a
// second — a job that failed on its first step — has a real value there and
// printing it as "0s" would erase the only number that distinguishes it from
// one that never started.
func seconds(p *float64) string {
	if p == nil {
		return "—"
	}
	return millis(int64(*p * 1000))
}
