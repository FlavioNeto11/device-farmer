package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// `ctl quarantine` opens and closes quarantines through the API.
//
// The recovery ladder opens quarantines on its own evidence, at the scopes
// its evidence reaches. An operator who knows more than the ladder — that one
// port chews cables, that one ganged power switch browns out under load — had
// nowhere to say so except psql, which is an unaudited INSERT into a table
// three predicates read. Both verbs here go through the API like everything
// else in this package, so a name and a reason land in farm.audit_log beside
// the row, and the devices it covers leave allocation in the same transaction
// that opens it.
//
// Neither verb touches a lease. A quarantine stops the NEXT allocation; a
// device inside one that holds a live lease keeps it, and the server's reply
// says how many of those there are.

// quarantineOpenResponse is the shape of POST /api/v1/quarantines.
type quarantineOpenResponse struct {
	QuarantineID    int64   `json:"quarantine_id"`
	Scope           string  `json:"scope"`
	DeviceID        *string `json:"device_id"`
	SlotID          *int64  `json:"slot_id"`
	HubID           *int64  `json:"hub_id"`
	HostID          *string `json:"host_id"`
	PowerDomainID   *int64  `json:"power_domain_id"`
	Reason          string  `json:"reason"`
	DevicesCovered  int64   `json:"devices_covered"`
	DevicesFrozen   int64   `json:"devices_frozen"`
	DevicesDisabled int64   `json:"devices_disabled"`
	LiveLeases      int64   `json:"live_leases"`
	Note            string  `json:"note"`
}

// quarantineCloseResponse is the shape of POST /api/v1/quarantines/{id}/close.
type quarantineCloseResponse struct {
	QuarantineID     int64  `json:"quarantine_id"`
	Closed           bool   `json:"closed"`
	Scope            string `json:"scope"`
	OpenedFor        string `json:"opened_for"`
	DevicesReleased  int64  `json:"devices_released"`
	DevicesReenabled int64  `json:"devices_reenabled"`
}

const quarantineUsage = "usage: ctl quarantine open --scope s --id x --reason r  |  ctl quarantine close <id> --reason r"

func cmdQuarantine(ctx context.Context, s *session, args []string) error {
	if len(args) == 0 {
		return usageErrf(quarantineUsage)
	}
	switch args[0] {
	case "open":
		return quarantineOpen(ctx, s, args[1:])
	case "close":
		return quarantineClose(ctx, s, args[1:])
	case "-h", "--help":
		return usageErrf(quarantineUsage)
	}
	return usageErrf("ctl quarantine takes open or close, not %q", args[0])
}

func quarantineOpen(ctx context.Context, s *session, args []string) error {
	fs := newFlags("quarantine open", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	scope := fs.String("scope", "", "device, slot, power_domain, hub or host")
	subject := fs.String("id", "", "the subject: a device id or farm_uid, a slot, hub or power domain id, or a host id")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("usage: ctl quarantine open --scope s --id x --reason r")
	}
	*scope = strings.TrimSpace(*scope)
	*subject = strings.TrimSpace(*subject)
	if *scope == "" || *subject == "" {
		return usageErrf("usage: ctl quarantine open --scope s --id x --reason r (both --scope and --id are required)")
	}

	// The body is built here, before the server is asked anything, so a typo
	// in --scope is a usage error and not a round trip.
	body := map[string]any{"scope": *scope}
	switch *scope {
	case "device":
		body["device_id"] = *subject
	case "host":
		body["host_id"] = *subject
	case "slot", "hub", "power_domain":
		n, err := strconv.ParseInt(*subject, 10, 64)
		if err != nil {
			return usageErrf("--id for scope %s is the integer id the topology listing shows, not %q", *scope, *subject)
		}
		body[*scope+"_id"] = n
	default:
		return usageErrf("--scope must be device, slot, power_domain, hub or host, not %q", *scope)
	}

	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("quarantine open"); err != nil {
		return err
	}
	body["reason"] = e.reason

	// Preflight. For a device the listing can say where it is and whether it
	// is inside a run; for the wider scopes the server's reply carries the
	// count, because only it knows the topology at the instant it applies.
	f := &Fields{}
	f.Add("scope", *scope)
	f.Add("subject", *subject)
	if *scope == "device" {
		resp, _, readErr := fetch[deviceResponse](ctx, e.client, apiPrefix+"/devices/"+url.PathEscape(*subject), nil)
		if readErr != nil {
			e.warnf("could not read the device for the preflight (%v); the server remains the authority", readErr)
		} else {
			d := resp.Device
			f.Add("rack slot", rackSlotOf(d.RackSlot))
			f.Add("farm uid", d.FarmUID)
			f.Add("admin state", d.AdminState)
			f.Add("health", dash(d.Health))
			if d.Lease != nil {
				f.Addf("live lease", "%s — holder %s, job %s, expires %s; UNTOUCHED by this",
					d.Lease.ID, d.Lease.Holder, d.Lease.JobID, stamp(&d.Lease.ExpiresAt))
			}
		}
	}

	headline := fmt.Sprintf("About to QUARANTINE %s %s: every device it covers leaves allocation now\n"+
		"and stays out until the quarantine is closed. Live leases are NOT released by this and\n"+
		"run to completion.", strings.ReplaceAll(*scope, "_", " "), *subject)
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/quarantines", body)
	if err != nil {
		return e.unknownOutcome(err, "ctl recovery")
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	var res quarantineOpenResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Addf("quarantine", "%d", res.QuarantineID)
	out.Add("scope", quarantineSubjectLine(res))
	out.Addf("devices covered", "%d (%d frozen, %d taken out of allocation)",
		res.DevicesCovered, res.DevicesFrozen, res.DevicesDisabled)
	out.Addf("live leases inside", "%d — untouched", res.LiveLeases)
	if res.Note != "" {
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}

// quarantineSubjectLine names what a freshly opened quarantine covers.
func quarantineSubjectLine(r quarantineOpenResponse) string {
	host := ""
	if r.HostID != nil && r.Scope != "host" {
		host = " on host " + *r.HostID
	}
	switch r.Scope {
	case "device":
		return "device " + str(r.DeviceID) + host
	case "slot":
		return "slot " + dashInt64(r.SlotID) + host
	case "hub":
		return "hub " + dashInt64(r.HubID) + host
	case "power_domain":
		return "power domain " + dashInt64(r.PowerDomainID) + host
	case "host":
		return "host " + str(r.HostID)
	}
	return r.Scope
}

func quarantineClose(ctx context.Context, s *session, args []string) error {
	fs := newFlags("quarantine close", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl quarantine close <id> --reason r")
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return usageErrf("quarantine id must be an integer, not %q", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("quarantine close"); err != nil {
		return err
	}

	// Preflight from the open list, which is the only list there is: a closed
	// quarantine is not shown, so not finding it is itself information.
	f := &Fields{}
	f.Addf("quarantine", "%d", id)
	listing, _, listErr := fetch[recoveryResponse](ctx, e.client, apiPrefix+"/recovery", url.Values{"limit": {"1"}})
	if listErr != nil {
		e.warnf("could not read the open quarantines for the preflight (%v); the server remains the authority", listErr)
	} else {
		found := false
		for _, q := range listing.Quarantines {
			if q.ID != id {
				continue
			}
			found = true
			f.Add("scope", q.Scope)
			f.Add("where", quarantineWhere(q))
			f.Add("host", dash(q.HostID))
			f.Add("device", dash(q.FarmUID))
			f.Add("opened", ago(&q.OpenedAt))
			f.Addf("opened by automation", "%s", yesNo(q.Auto))
			f.Add("reason", q.Reason)
		}
		if !found {
			e.warnf("no OPEN quarantine has id %d; the request will be sent and the server will answer", id)
		}
	}

	headline := fmt.Sprintf("About to CLOSE quarantine %d. This claims the fault is fixed: its devices\n"+
		"return to allocation once the watchdog sees them healthy again. A device another open\n"+
		"quarantine still covers stays out, and no lease is touched either way.", id)
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/quarantines/"+strconv.FormatInt(id, 10)+"/close",
		map[string]string{"reason": e.reason})
	if err != nil {
		return e.unknownOutcome(err, "ctl recovery")
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	var res quarantineCloseResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Addf("quarantine", "%d", res.QuarantineID)
	out.Add("closed", yesNo(res.Closed))
	out.Add("scope", res.Scope)
	out.Add("was opened for", res.OpenedFor)
	out.Addf("devices released", "%d back to 'unknown' health, %d back to admin_state 'enabled'",
		res.DevicesReleased, res.DevicesReenabled)
	if res.DevicesReleased == 0 && res.DevicesReenabled == 0 {
		// Zero is an answer, and a surprising one deserves a sentence.
		out.Add("note", "nothing was released: either another open quarantine still covers these "+
			"devices, or none of them was in 'quarantined' health to begin with — `ctl recovery` lists what is still open")
	}
	return e.out.Fields(out)
}
