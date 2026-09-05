package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// `ctl power` is tier 4 of the recovery ladder — port power — asked for by a
// human instead of by the ladder.
//
// It is the one recovery rung that is not per-device. On a hub without
// per-port switching every port shares one power domain, so "cycle this slot"
// is "cycle these seven phones", and the API REFUSES with 409 when any live
// lease anywhere in that domain carries a disruption policy that forbids it.
// That refusal is the whole design: an operator with a dead handset reaches
// for the power switch, and the control plane is what stands between that
// reflex and somebody else's six-hour run. Exit 3 is the refusal reaching a
// script intact.
//
// The API does not itself toggle VBUS. It decides whether the action is
// permitted, records the decision as a farm.recovery_attempts row, and the
// host agent acts on it. Whether that hand-off is reported as "requested" or
// waited on and reported with its outcome is the server's choice, and the
// reply is rendered from what it says rather than from a shape this file
// insists on, so a server that answers either way reads well here.
//
// A power cycle ends no lease. A lease that permits it keeps its device, its
// fence and its deadline throughout; the holder sees a USB disconnect and a
// reconnect, which is the disruption its policy said it could bear.

// powerFieldOrder is the order the reply's keys are printed in when they
// are present: what happened, where, how big the blast radius was, and then
// the ladder's own terms for it. Anything the server adds beyond these is
// printed after, in name order, so nothing it says is dropped.
var powerFieldOrder = []string{
	"attempt_id", "state", "outcome", "refusal",
	"slot_id", "rack_slot", "host_id", "hub_id", "usb_path", "slot_state",
	"power_domain_id", "power_kind", "power_control", "slots_in_domain", "live_leases",
	"tier", "tier_name", "blast_radius", "requires_policy",
	"duration_ms", "note",
}

func cmdSlotPower(ctx context.Context, s *session, args []string) error {
	fs := newFlags("power", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl power <slot id> --reason r")
	}
	slotID, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || slotID <= 0 {
		return usageErrf("power takes a slot id — the integer `ctl device <id>` prints as slot_id — not %q", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("power"); err != nil {
		return err
	}

	// The preflight comes from the fleet listing, which is the one read that
	// carries a slot id beside the rack position, the hub, and the lease. The
	// server computes the real blast radius from farm.power_domains; what is
	// shown here is the hub, which on ganged hardware is the same thing.
	f := &Fields{}
	f.Addf("slot", "%d", slotID)
	fleet, _, listErr := fetch[fleetResponse](ctx, e.client, apiPrefix+"/fleet", url.Values{"limit": {"5000"}})
	if listErr != nil {
		e.warnf("could not read the fleet for the preflight (%v); the server remains the authority", listErr)
	} else {
		var occupant *device
		for i := range fleet.Devices {
			if d := &fleet.Devices[i]; d.SlotID != nil && *d.SlotID == slotID {
				occupant = d
				break
			}
		}
		if occupant == nil {
			e.warnf("no device in the fleet listing sits in slot %d; the request will be sent and the "+
				"server will answer", slotID)
		} else {
			powerPreflight(f, fleet, occupant)
		}
	}

	headline := fmt.Sprintf("About to POWER-CYCLE slot %d — tier 4 of the recovery ladder, by hand.\n"+
		"Every device in this slot's power domain loses VBUS; on a hub without per-port switching that\n"+
		"is the whole hub. The server refuses (exit 3) if any live lease in the domain forbids it. A lease\n"+
		"that permits it keeps its device, its fence and its deadline; nothing here releases anything.", slotID)
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/slots/"+strconv.FormatInt(slotID, 10)+"/power",
		map[string]string{"reason": e.reason})
	if err != nil {
		// A permitted cycle is a recovery_attempts row before it is anything
		// else, so the attempts listing settles whether this one was filed.
		return e.unknownOutcome(err, "ctl recovery --tier 4 --limit 20")
	}
	if e.format == FormatJSON {
		if err := e.out.RawJSON(raw); err != nil {
			return err
		}
		return powerOutcome(raw)
	}
	out, err := powerFields(raw)
	if err != nil {
		return e.out.RawJSON(raw)
	}
	if err := e.out.Fields(out); err != nil {
		return err
	}
	return powerOutcome(raw)
}

// powerPreflight fills in what the fleet listing knows about the slot's
// occupant and its neighbours.
func powerPreflight(f *Fields, fleet fleetResponse, d *device) {
	f.Add("rack slot", rackSlotOf(d.RackSlot))
	f.Add("farm uid", d.FarmUID)
	f.Add("host", dash(d.HostID))
	f.Add("slot state", dash(d.SlotState))
	f.Add("health", dash(d.Health))
	if d.HubID != nil {
		hubDevices := 0
		for _, other := range fleet.Devices {
			if other.HubID != nil && *other.HubID == *d.HubID {
				hubDevices++
			}
		}
		switching := "unknown"
		if d.VbusSwitchable != nil {
			if *d.VbusSwitchable {
				switching = "per port — the domain is this slot"
			} else {
				switching = "GANGED — every port on the hub shares one power domain"
			}
		}
		f.Addf("hub", "%d (%s), %d device(s) on it", *d.HubID, dash(d.HubPath), hubDevices)
		f.Add("vbus switching", switching)
	}
	if d.Lease != nil {
		f.Gap()
		// dash: after tenant scoping these are nil when the lease is another
		// tenant's, and the point of the block is that a lease is THERE — the
		// identifiers are what gets masked, not the fact.
		f.Add("live lease", dash(d.Lease.ID))
		f.Add("job", dash(d.Lease.JobID))
		f.Add("tenant", dash(d.Lease.TenantID))
		f.Add("holder", dash(d.Lease.Holder))
		f.Addf("protected", "%s", yesNo(d.Lease.Protected))
		f.Add("note", "the server decides from every lease in the power domain, not only this one")
	}
}

// powerFields renders the reply generically: the keys this package knows
// first, in a fixed order, then whatever else the server said. The endpoint
// is changing from an accepted-and-handed-off 202 to a synchronous outcome,
// and a renderer that insisted on one shape would print nothing useful for
// the other.
func powerFields(raw json.RawMessage) (*Fields, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	f := &Fields{}
	seen := map[string]bool{}
	for _, k := range powerFieldOrder {
		v, ok := body[k]
		if !ok {
			continue
		}
		seen[k] = true
		f.Add(strings.ReplaceAll(k, "_", " "), jsonCell(v))
	}
	var extra []string
	for k := range body {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 && f.Len() > 0 {
		f.Gap()
	}
	for _, k := range extra {
		f.Add(strings.ReplaceAll(k, "_", " "), jsonCell(body[k]))
	}
	return f, nil
}

// jsonCell renders one JSON value for a Fields row without deciding what
// type it ought to have been.
func jsonCell(v json.RawMessage) string {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return "—"
	}
	switch v[0] {
	case '"':
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s
		}
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(v, &b); err == nil {
			return yesNo(b)
		}
	case '{', '[':
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err == nil {
			return buf.String()
		}
	}
	return string(v)
}

// powerOutcome turns a reply the server sent as success into this package's
// exit code.
//
// A 2xx says the request was permitted and acted on; it does not say the
// hardware came back. A server that waits for the host agent reports that in
// outcome, using farm.recovery_attempts' own vocabulary, and a cycle that
// finished 'failed' or 'aborted' is a failure of the action even though the
// transport and the policy check both worked. A server that hands off and
// answers 'requested' has nothing to fail yet.
func powerOutcome(raw json.RawMessage) error {
	var body struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	switch body.Outcome {
	case "failed", "aborted":
		return fmt.Errorf("the power cycle finished with outcome %q; no lease was affected", body.Outcome)
	}
	return nil
}
