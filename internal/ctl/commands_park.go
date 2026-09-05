package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// `ctl park` and `ctl unpark` are the operator surface for a deliberate hold.
//
// farm.device_park and farm.device_unpark (migration 00008) are the only
// supported way in and out of admin_state 'parked', and until now the only
// way to reach them was a psql session. A person shelving a handset for the
// afternoon with a database shell and a deadline reaches for UPDATE
// farm.devices instead, and the guards in 00008 refuse them with a HINT naming
// a function they cannot call from where they are. These two verbs go through
// the API, so the hold is opened under the operator's token name with a
// reason, and reversed the same way.
//
// A park never touches a lease. The functions run as a role with no privilege
// on farm.leases at all; a parked device with a live lease keeps it, and the
// preflight below shows that lease so nobody expects the device back sooner.

// parkResponse is the shape of both routes' replies.
type parkResponse struct {
	DeviceID    string       `json:"device_id"`
	FarmUID     string       `json:"farm_uid"`
	RackSlot    *string      `json:"rack_slot"`
	ParkID      int64        `json:"park_id"`
	Parked      bool         `json:"parked"`
	AdminState  string       `json:"admin_state"`
	OpenedBy    string       `json:"opened_by"`
	OpenedAt    *time.Time   `json:"opened_at"`
	Reason      string       `json:"reason"`
	ClosedBy    *string      `json:"closed_by"`
	ClosedAt    *time.Time   `json:"closed_at"`
	CloseReason *string      `json:"close_reason"`
	LiveLease   *deviceLease `json:"live_lease"`
	Note        string       `json:"note"`
}

func cmdPark(ctx context.Context, s *session, args []string) error {
	return setParked(ctx, s, args, true)
}

func cmdUnpark(ctx context.Context, s *session, args []string) error {
	return setParked(ctx, s, args, false)
}

func setParked(ctx context.Context, s *session, args []string, park bool) error {
	verb := "unpark"
	if park {
		verb = "park"
	}
	fs := newFlags(verb, s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		if park {
			return usageErrf("usage: ctl park <id|farm_uid> --reason r")
		}
		return usageErrf("usage: ctl unpark <id|farm_uid> [--reason r]")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	// Unparking may go without a reason — the function records it as NULL —
	// because "the hold is over" is what the verb already says. Parking may
	// not: a parked device with no reason is indistinguishable from a fault.
	if park {
		if err := e.requireReason("park"); err != nil {
			return err
		}
	}
	dev := rest[0]

	f := &Fields{}
	f.Add("device", dev)
	resp, _, readErr := fetch[deviceResponse](ctx, e.client, apiPrefix+"/devices/"+url.PathEscape(dev), nil)
	if readErr != nil {
		e.warnf("could not read the device for the preflight (%v); the server remains the authority", readErr)
	} else {
		d := resp.Device
		f.Add("rack slot", rackSlotOf(d.RackSlot))
		f.Add("farm uid", d.FarmUID)
		f.Add("model", dash(d.Model))
		f.Add("admin state", d.AdminState)
		f.Add("health", dash(d.Health))
		if d.Lease != nil {
			f.Addf("live lease", "%s — holder %s, job %s, expires %s; UNTOUCHED by this",
				d.Lease.ID, d.Lease.Holder, d.Lease.JobID, stamp(&d.Lease.ExpiresAt))
		}
		if park && d.AdminState != "enabled" {
			e.warnf("this device is %s, not enabled; the server will refuse to park it, because that "+
				"state is already somebody's decision about it", d.AdminState)
		}
		if !park && d.AdminState != "parked" {
			e.warnf("this device is %s, not parked; there is nothing for the server to reverse", d.AdminState)
		}
	}

	headline := fmt.Sprintf("About to PARK device %s: no new lease will be placed on it until it is unparked.\n"+
		"A live lease is NOT released by this and runs to completion; the recovery ladder leaves\n"+
		"a parked device alone.", dev)
	if !park {
		headline = fmt.Sprintf("About to UNPARK device %s: it becomes eligible for allocation once the\n"+
			"watchdog sees it healthy again.", dev)
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/devices/"+url.PathEscape(dev)+"/"+verb,
		map[string]string{"reason": e.reason})
	if err != nil {
		return e.unknownOutcome(err, "ctl device "+dev)
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	var res parkResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("rack slot", rackSlotOf(res.RackSlot))
	out.Add("farm uid", res.FarmUID)
	out.Add("device", res.DeviceID)
	out.Add("admin state", res.AdminState)
	out.Addf("park", "%d, opened by %s %s: %s", res.ParkID, res.OpenedBy, ago(res.OpenedAt), res.Reason)
	if res.ClosedAt != nil {
		out.Addf("closed", "by %s %s%s", str(res.ClosedBy), ago(res.ClosedAt), closeReasonSuffix(res.CloseReason))
	}
	if res.LiveLease != nil {
		out.Addf("live lease", "%s — job %s, %s; untouched", res.LiveLease.ID, res.LiveLease.JobID, res.LiveLease.State)
	}
	if res.Note != "" {
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}

func closeReasonSuffix(p *string) string {
	if p == nil || *p == "" {
		return ""
	}
	return ": " + *p
}
