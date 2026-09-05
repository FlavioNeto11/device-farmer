package ctl

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// `ctl slot` is the operator's hand on the physical layer: what positions the
// farm knows about, giving one a name, putting a device in one, and writing
// the fleet's name for a phone onto the phone.
//
// Three of these change what an alert prints or where a recovery action lands,
// and every one of those goes through the API's refusals: a device holding a
// live lease keeps its slot and its brand, and this tool has no way past that.
// A re-slot and a rebrand additionally ask before acting, because "which
// socket is this phone in" is a fact a wrong answer to costs somebody a run.

type slot struct {
	SlotID        int64      `json:"slot_id"`
	HostID        string     `json:"host_id"`
	HubID         int64      `json:"hub_id"`
	HubPath       string     `json:"hub_path"`
	PortNumber    int        `json:"port_number"`
	USBPath       string     `json:"usb_path"`
	ADBDevpath    string     `json:"adb_devpath"`
	RackSlot      *string    `json:"rack_slot"`
	State         string     `json:"state"`
	RearmAt       *time.Time `json:"rearm_at"`
	PowerDomainID *int64     `json:"power_domain_id"`
	PowerKind     *string    `json:"power_kind"`
	PowerControl  *string    `json:"power_control"`
	DeviceID      *string    `json:"device_id"`
	FarmUID       *string    `json:"farm_uid"`
}

type slotListResponse struct {
	Slots     []slot `json:"slots"`
	Total     int    `json:"total"`
	Occupied  int    `json:"occupied"`
	Truncated bool   `json:"truncated"`
}

// slotRegisterRequest is the body of POST /slots.
type slotRegisterRequest struct {
	HostID     string `json:"host_id"`
	USBPath    string `json:"usb_path"`
	HubPath    string `json:"hub_path"`
	Port       int    `json:"port"`
	HubModel   string `json:"hub_model,omitempty"`
	Ports      int    `json:"ports,omitempty"`
	Switchable bool   `json:"switchable,omitempty"`
	RackSlot   string `json:"rack_slot,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type slotRegisterResponse struct {
	Slot    slot `json:"slot"`
	Created bool `json:"created"`
}

type slotLabelResponse struct {
	Slot             slot    `json:"slot"`
	PreviousRackSlot *string `json:"previous_rack_slot"`
}

type deviceReslotResponse struct {
	DeviceID   string  `json:"device_id"`
	FarmUID    string  `json:"farm_uid"`
	HostID     *string `json:"host_id"`
	FromSlotID *int64  `json:"from_slot_id"`
	ToSlotID   *int64  `json:"to_slot_id"`
	RackSlot   *string `json:"rack_slot"`
	ADBDevpath *string `json:"adb_devpath"`
	Moved      bool    `json:"moved"`
	Note       string  `json:"note"`
}

type deviceRebrandResponse struct {
	DeviceID         string  `json:"device_id"`
	FarmUID          string  `json:"farm_uid"`
	HostID           *string `json:"host_id"`
	ADBDevpath       string  `json:"adb_devpath"`
	PreviousUID      string  `json:"previous_uid"`
	PreviousDeviceID *string `json:"previous_device_id"`
	Outcome          string  `json:"outcome"`
	Note             string  `json:"note"`
}

// slotVerbs is the sub-verb table. Power is not here: cutting a port's power
// is a recovery action with a blast radius, and it has a top-level command of
// its own rather than a sub-verb of the listing.
var slotVerbs = map[string]func(context.Context, *session, []string) error{
	"list":     cmdSlotList,
	"register": cmdSlotRegister,
	"label":    cmdSlotLabel,
	"reslot":   cmdSlotReslot,
	"rebrand":  cmdSlotRebrand,
}

func slotVerbNames() string {
	names := make([]string, 0, len(slotVerbs))
	for v := range slotVerbs {
		names = append(names, v)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func cmdSlot(ctx context.Context, s *session, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageErrf("usage: ctl slot <%s> ...", strings.ReplaceAll(slotVerbNames(), ", ", "|"))
	}
	verb, ok := slotVerbs[args[0]]
	if !ok {
		return usageErrf("ctl slot takes %s, not %q", slotVerbNames(), args[0])
	}
	return verb(ctx, s, args[1:])
}

// ---------------------------------------------------------------------------
// slot list
// ---------------------------------------------------------------------------

func cmdSlotList(ctx context.Context, s *session, args []string) error {
	fs := newFlags("slot list", s.err)
	var g globals
	g.bind(fs)
	host := fs.String("host", "", "only slots on this host")
	hubFlag := fs.String("hub", "", "only slots on this hub (usb path or id)")
	limit := fs.Int("limit", 5000, "maximum slots to return")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("slot list takes no arguments; use --host or --hub to narrow it")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "host", *host)
	setIf(q, "hub", *hubFlag)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[slotListResponse](ctx, e.client, apiPrefix+"/slots", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	t := NewTable("RACK SLOT", "ID", "PORT", "DEVPATH", "STATE", "POWER", "DEVICE")
	group := ""
	for _, sl := range resp.Slots {
		if key := sl.HostID + " / hub " + sl.HubPath; key != group {
			group = key
			t.Section("%s", key)
		}
		power := "—"
		if sl.PowerKind != nil {
			power = *sl.PowerKind
			if sl.PowerControl != nil && *sl.PowerControl != "none" {
				power += " (" + *sl.PowerControl + ")"
			}
		}
		device := "(empty)"
		if sl.FarmUID != nil {
			device = *sl.FarmUID
		}
		t.Row(rackSlotOf(sl.RackSlot), strconv.FormatInt(sl.SlotID, 10), strconv.Itoa(sl.PortNumber),
			sl.ADBDevpath, sl.State, power, device)
	}
	if t.Len() == 0 {
		e.out.Text("no slot matched.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("%d slots, %d occupied. A slot is a position in the USB tree: it is what a "+
		"power switch controls and what an operator walks to.", resp.Total, resp.Occupied)
	if resp.Truncated {
		e.warnf("the listing hit its limit of %d slots and was cut there; narrow it with --host or --hub, "+
			"or raise --limit", *limit)
	}
	return nil
}

// ---------------------------------------------------------------------------
// slot register
// ---------------------------------------------------------------------------

func cmdSlotRegister(ctx context.Context, s *session, args []string) error {
	fs := newFlags("slot register", s.err)
	var g globals
	g.bind(fs)
	var req slotRegisterRequest
	fs.StringVar(&req.HostID, "host", "", "host the slot hangs from (required)")
	fs.StringVar(&req.USBPath, "usb-path", "", "the slot's USB position, e.g. 3-1.4 (required)")
	fs.StringVar(&req.HubPath, "hub-path", "", "the hub's USB position, e.g. 3-1 (required)")
	fs.IntVar(&req.Port, "port", 0, "port number on the hub, 1-32 (required)")
	fs.StringVar(&req.HubModel, "model", "", "hub model, recorded when the hub is new")
	fs.IntVar(&req.Ports, "ports", 0, "how many ports the hub has (default 7)")
	fs.BoolVar(&req.Switchable, "switchable", false, "the hub can switch VBUS per port")
	fs.StringVar(&req.RackSlot, "rack-slot", "", "human label, e.g. R1-U14-H2-P3")
	fs.StringVar(&req.Reason, "reason", "", "why; recorded in farm.audit_log")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 || req.HostID == "" || req.USBPath == "" || req.HubPath == "" || req.Port == 0 {
		return usageErrf("usage: ctl slot register --host h --usb-path p --hub-path q --port n " +
			"[--model m] [--ports n] [--switchable] [--rack-slot r]")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	res, raw, err := send[slotRegisterResponse](ctx, e.client, apiPrefix+"/slots", req)
	if err != nil {
		return e.unknownOutcome(err, "ctl slot list --host "+req.HostID)
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	f := slotFields(res.Slot)
	if res.Created {
		f.Add("result", "created")
	} else {
		f.Add("result", "already registered; the row was refreshed and any existing label kept")
	}
	return e.out.Fields(f)
}

// slotFields renders one slot, rack position first: it is the only line that
// tells an operator where to walk.
func slotFields(sl slot) *Fields {
	f := &Fields{}
	f.Add("rack slot", rackSlotOf(sl.RackSlot))
	f.Addf("slot id", "%d", sl.SlotID)
	f.Add("host", sl.HostID)
	f.Addf("hub", "%d (%s)", sl.HubID, sl.HubPath)
	f.Addf("port", "%d", sl.PortNumber)
	f.Add("usb path", sl.USBPath)
	f.Add("adb devpath", sl.ADBDevpath)
	f.Add("state", sl.State)
	if sl.PowerKind != nil {
		f.Addf("power", "%s via %s", *sl.PowerKind, dash(sl.PowerControl))
	}
	if sl.FarmUID != nil {
		f.Add("device", *sl.FarmUID)
	} else {
		f.Add("device", "none")
	}
	return f
}

// ---------------------------------------------------------------------------
// slot label
// ---------------------------------------------------------------------------

func cmdSlotLabel(ctx context.Context, s *session, args []string) error {
	fs := newFlags("slot label", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	label := fs.String("rack-slot", "", "the new label; an empty string clears it (required)")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl slot label <id> --rack-slot r --reason r")
	}
	if _, err := strconv.ParseInt(rest[0], 10, 64); err != nil {
		return usageErrf("slot id must be an integer, not %q", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("slot label"); err != nil {
		return err
	}

	res, raw, err := send[slotLabelResponse](ctx, e.client,
		apiPrefix+"/slots/"+url.PathEscape(rest[0])+"/label",
		map[string]string{"rack_slot": *label, "reason": e.reason})
	if err != nil {
		return e.unknownOutcome(err, "ctl slot list")
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	f := slotFields(res.Slot)
	f.Gap()
	f.Add("previous label", rackSlotOf(res.PreviousRackSlot))
	return e.out.Fields(f)
}

// ---------------------------------------------------------------------------
// slot reslot
// ---------------------------------------------------------------------------

func cmdSlotReslot(ctx context.Context, s *session, args []string) error {
	fs := newFlags("slot reslot", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	slotID := fs.Int64("slot", 0, "destination slot id")
	unslot := fs.Bool("unslot", false, "take the device out of its slot without placing it")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || (*slotID == 0) == !*unslot {
		return usageErrf("usage: ctl slot reslot <device> --slot <id> --reason r  |  " +
			"ctl slot reslot <device> --unslot --reason r")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("slot reslot"); err != nil {
		return err
	}
	path := apiPrefix + "/devices/" + url.PathEscape(rest[0])

	// The preflight is a read, so the operator sees where the device is now,
	// where it is going, and who is holding it BEFORE its address changes.
	target, _, err := fetch[deviceResponse](ctx, e.client, path, nil)
	if err != nil {
		return err
	}
	d := target.Device

	f := &Fields{}
	f.Add("rack slot", rackSlotOf(d.RackSlot))
	f.Add("farm uid", d.FarmUID)
	f.Add("host", dash(d.HostID))
	f.Add("current slot", dashInt64(d.SlotID))
	f.Add("adb devpath", dash(d.ADBDevpath))
	headline := fmt.Sprintf("About to take device %s OUT of its slot.\n"+
		"Until it is placed again nothing can be addressed to it: no exec, no recovery, no power cycle.",
		rackSlotOf(d.RackSlot))
	if !*unslot {
		f.Gap()
		f.Addf("destination slot", "%d", *slotID)
		if dest, ok := lookupSlot(ctx, e, str(d.HostID), *slotID); ok {
			f.Add("destination rack slot", rackSlotOf(dest.RackSlot))
			f.Add("destination devpath", dest.ADBDevpath)
			f.Add("destination state", dest.State)
			if dest.FarmUID != nil && (d.FarmUID != *dest.FarmUID) {
				f.Addf("destination occupant", "%s — the API will refuse this", *dest.FarmUID)
			}
		}
		headline = fmt.Sprintf("About to RE-SLOT device %s to slot %d.\n"+
			"Every recovery action, power cycle and exec for it will address the new position from now on.",
			rackSlotOf(d.RackSlot), *slotID)
	}
	if d.Lease != nil {
		f.Gap()
		// dash: tenant scoping leaves these nil for another tenant's lease.
		f.Add("live lease", dash(d.Lease.ID))
		f.Add("job", dash(d.Lease.JobID))
		f.Add("holder", dash(d.Lease.Holder))
		headline = fmt.Sprintf("Device %s holds a live lease. The API will REFUSE this; nothing here "+
			"can end a lease.", rackSlotOf(d.RackSlot))
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	body := map[string]any{"reason": e.reason}
	if *unslot {
		body["unslot"] = true
	} else {
		body["slot_id"] = *slotID
	}
	res, raw, err := send[deviceReslotResponse](ctx, e.client, path+"/reslot", body)
	if err != nil {
		return e.unknownOutcome(err, "ctl device "+rest[0])
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("rack slot", rackSlotOf(res.RackSlot))
	out.Add("farm uid", res.FarmUID)
	out.Add("from slot", dashInt64(res.FromSlotID))
	out.Add("to slot", dashInt64(res.ToSlotID))
	out.Add("adb devpath", dash(res.ADBDevpath))
	out.Add("moved", yesNo(res.Moved))
	if res.Note != "" {
		out.Gap()
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}

// lookupSlot finds one slot in a host's listing, for the preflight. A miss is
// reported rather than failed: the server remains the authority on whether
// the destination exists.
func lookupSlot(ctx context.Context, e *env, host string, id int64) (slot, bool) {
	q := url.Values{}
	setIf(q, "host", host)
	listing, _, err := fetch[slotListResponse](ctx, e.client, apiPrefix+"/slots", q)
	if err != nil {
		e.warnf("could not read the slot listing for the preflight (%v); the server remains the authority", err)
		return slot{}, false
	}
	for _, sl := range listing.Slots {
		if sl.SlotID == id {
			return sl, true
		}
	}
	e.warnf("slot %d is not in the listing for host %q; the request will be sent anyway and the server will answer",
		id, host)
	return slot{}, false
}

// ---------------------------------------------------------------------------
// slot rebrand
// ---------------------------------------------------------------------------

func cmdSlotRebrand(ctx context.Context, s *session, args []string) error {
	fs := newFlags("slot rebrand", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	previous := fs.String("previous-uid", "", "the uid the phone is expected to carry; anything else is refused")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl slot rebrand <device> --reason r [--previous-uid u]")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("slot rebrand"); err != nil {
		return err
	}
	path := apiPrefix + "/devices/" + url.PathEscape(rest[0])

	target, _, err := fetch[deviceResponse](ctx, e.client, path, nil)
	if err != nil {
		return err
	}
	d := target.Device

	f := &Fields{}
	f.Add("rack slot", rackSlotOf(d.RackSlot))
	f.Add("farm uid to write", d.FarmUID)
	f.Add("host", dash(d.HostID))
	f.Add("adb devpath", dash(d.ADBDevpath))
	if *previous != "" {
		f.Add("expected on the phone", *previous)
	}
	headline := fmt.Sprintf("About to REBRAND the phone at %s: whatever uid it carries will be replaced "+
		"with %s.\nThe identity it carried is abandoned. If another device row owns that uid, "+
		"that row will name no phone afterwards and must be retired.",
		rackSlotOf(d.RackSlot), d.FarmUID)
	if d.Lease != nil {
		f.Gap()
		// dash: tenant scoping leaves these nil for another tenant's lease.
		f.Add("live lease", dash(d.Lease.ID))
		f.Add("job", dash(d.Lease.JobID))
		f.Add("holder", dash(d.Lease.Holder))
		headline = fmt.Sprintf("Device %s holds a live lease. The API will REFUSE this: nothing is "+
			"written to a phone in the middle of somebody's run.", rackSlotOf(d.RackSlot))
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	body := map[string]string{"reason": e.reason}
	if *previous != "" {
		body["previous_uid"] = *previous
	}
	res, raw, err := send[deviceRebrandResponse](ctx, e.client, path+"/rebrand", body)
	if err != nil {
		return e.unknownOutcome(err, "ctl device "+rest[0])
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("farm uid", res.FarmUID)
	out.Add("adb devpath", res.ADBDevpath)
	out.Add("outcome", res.Outcome)
	if res.PreviousUID == "" {
		out.Add("previous uid", "none — the phone was unbranded")
	} else {
		out.Add("previous uid", res.PreviousUID)
	}
	if res.PreviousDeviceID != nil {
		out.Add("previous device row", *res.PreviousDeviceID)
	}
	if res.Note != "" {
		out.Gap()
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}
