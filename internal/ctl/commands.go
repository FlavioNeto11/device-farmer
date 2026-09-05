package ctl

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// ---------------------------------------------------------------------------
// Wire types
//
// These mirror the JSON internal/api emits, field for field, and carry only
// what a rendering reads. They are deliberately NOT shared with the api
// package: ctl is a client, and a client that compiles against the server's
// internal structs stops being able to detect a response it no longer
// understands. Anything not decoded here still reaches the operator, because
// -o json passes the server's own body through untouched.
// ---------------------------------------------------------------------------

type deviceLease struct {
	ID            string    `json:"id"`
	Fence         int64     `json:"fence"`
	State         string    `json:"state"`
	Protected     bool      `json:"protected"`
	JobID         string    `json:"job_id"`
	TenantID      string    `json:"tenant_id"`
	Holder        string    `json:"holder"`
	AcquiredAt    time.Time `json:"acquired_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	ReclaimableAt time.Time `json:"reclaimable_at"`
}

type device struct {
	DeviceID        string  `json:"device_id"`
	FarmUID         string  `json:"farm_uid"`
	ADBSerial       *string `json:"adb_serial"`
	SerialAmbiguous bool    `json:"serial_ambiguous"`
	Model           *string `json:"model"`
	Manufacturer    *string `json:"manufacturer"`
	AndroidRelease  *string `json:"android_release"`
	SDKInt          *int    `json:"sdk_int"`
	Pool            string  `json:"pool"`
	AdminState      string  `json:"admin_state"`
	FailureScore    float64 `json:"failure_score"`
	FenceFloor      int64   `json:"fence_floor"`

	SlotID     *int64     `json:"slot_id"`
	RackSlot   *string    `json:"rack_slot"`
	USBPath    *string    `json:"usb_path"`
	ADBDevpath *string    `json:"adb_devpath"`
	SlotState  *string    `json:"slot_state"`
	RearmAt    *time.Time `json:"rearm_at"`

	HubID           *int64     `json:"hub_id"`
	HubPath         *string    `json:"hub_path"`
	VbusSwitchable  *bool      `json:"vbus_switchable"`
	HostID          *string    `json:"host_id"`
	ADBEndpoint     *string    `json:"adb_endpoint"`
	HostAdminState  *string    `json:"host_admin_state"`
	ADBState        *string    `json:"adb_state"`
	Health          *string    `json:"health"`
	HealthSince     *time.Time `json:"health_since"`
	BatteryPct      *int       `json:"battery_pct"`
	BatteryTempDeci *int       `json:"battery_temp_dc"`
	ConsecBad       *int       `json:"consec_bad"`
	LadderTier      *int       `json:"ladder_tier"`
	LastSeenAt      *time.Time `json:"last_seen_at"`

	Lease *deviceLease `json:"lease"`

	QuarantineID     *int64  `json:"quarantine_id"`
	QuarantineReason *string `json:"quarantine_reason"`
}

type hub struct {
	HubID          int64      `json:"hub_id"`
	HostID         string     `json:"host_id"`
	USBPath        string     `json:"usb_path"`
	Model          *string    `json:"model"`
	VbusSwitchable bool       `json:"vbus_switchable"`
	Devices        int        `json:"devices"`
	Healthy        int        `json:"healthy"`
	Unhealthy      int        `json:"unhealthy"`
	WorstSince     *time.Time `json:"worst_since"`
	Correlated     bool       `json:"correlated"`
}

type fleetResponse struct {
	Devices []device `json:"devices"`
	Hubs    []hub    `json:"hubs"`
	Counts  struct {
		Total       int            `json:"total"`
		Health      map[string]int `json:"health"`
		Host        map[string]int `json:"host"`
		LeaseState  map[string]int `json:"lease_state"`
		Leased      int            `json:"leased"`
		Free        int            `json:"free"`
		Unhealthy   int            `json:"unhealthy"`
		Quarantined int            `json:"quarantined"`
		Protected   int            `json:"protected"`
	} `json:"counts"`
	Truncated bool `json:"truncated"`
}

type recoveryAttempt struct {
	ID          int64      `json:"id"`
	DeviceID    *string    `json:"device_id"`
	FarmUID     *string    `json:"farm_uid"`
	SlotID      *int64     `json:"slot_id"`
	RackSlot    *string    `json:"rack_slot"`
	HubID       *int64     `json:"hub_id"`
	HostID      *string    `json:"host_id"`
	Tier        int        `json:"tier"`
	TierName    string     `json:"tier_name"`
	BlastRadius string     `json:"blast_radius"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	Outcome     *string    `json:"outcome"`
	Refusal     *string    `json:"refusal"`
}

type deviceResponse struct {
	Device   device            `json:"device"`
	Hub      *hub              `json:"hub"`
	Recovery []recoveryAttempt `json:"recovery"`
}

type execResponse struct {
	DeviceID   string `json:"device_id"`
	Devpath    string `json:"adb_devpath"`
	Endpoint   string `json:"adb_endpoint"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"`
	Stderr     string `json:"stderr"`
	Exited     bool   `json:"exited"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	LeaseID    string `json:"lease_id"`
}

type jobLease struct {
	ID        string `json:"id"`
	Fence     int64  `json:"fence"`
	State     string `json:"state"`
	DeviceID  string `json:"device_id"`
	Protected bool   `json:"protected"`
}

type job struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	QueueID           string          `json:"queue_id"`
	PoolID            string          `json:"pool_id"`
	State             string          `json:"state"`
	Spec              json.RawMessage `json:"spec"`
	Selector          json.RawMessage `json:"selector"`
	PinDevice         *string         `json:"pin_device"`
	Protected         bool            `json:"protected"`
	DisruptionPolicy  string          `json:"disruption_policy"`
	ExpectedDurationS *int64          `json:"expected_duration_s"`
	MaxRuntimeS       *int64          `json:"max_runtime_s"`
	TTLSeconds        int64           `json:"ttl_s"`
	GraceSeconds      int64           `json:"grace_s"`
	CreatedBy         *string         `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at"`
	FinishedAt        *time.Time      `json:"finished_at"`
	Lease             *jobLease       `json:"lease"`
}

type jobListResponse struct {
	Jobs      []job          `json:"jobs"`
	Counts    map[string]int `json:"counts"`
	Truncated bool           `json:"truncated"`
}

type jobHistory struct {
	ID            string     `json:"id"`
	Fence         int64      `json:"fence"`
	State         string     `json:"state"`
	DeviceID      string     `json:"device_id"`
	Holder        string     `json:"holder"`
	AcquiredAt    time.Time  `json:"acquired_at"`
	ReleasedAt    *time.Time `json:"released_at"`
	ReleaseReason *string    `json:"release_reason"`
}

type jobResponse struct {
	Job    job          `json:"job"`
	Leases []jobHistory `json:"leases"`
}

type lease struct {
	ID               string     `json:"id"`
	Fence            int64      `json:"fence"`
	State            string     `json:"state"`
	DeviceID         string     `json:"device_id"`
	FarmUID          string     `json:"farm_uid"`
	SlotID           *int64     `json:"slot_id"`
	RackSlot         *string    `json:"rack_slot"`
	HostID           *string    `json:"host_id"`
	JobID            string     `json:"job_id"`
	TenantID         string     `json:"tenant_id"`
	QueueID          string     `json:"queue_id"`
	Holder           string     `json:"holder"`
	Protected        bool       `json:"protected"`
	DisruptionPolicy string     `json:"disruption_policy"`
	AcquiredAt       time.Time  `json:"acquired_at"`
	HeartbeatAt      time.Time  `json:"heartbeat_at"`
	ExpiresInS       int64      `json:"expires_in_s"`
	ReclaimableInS   int64      `json:"reclaimable_in_s"`
	WitnessExts      int        `json:"witness_extensions"`
	ReleasedAt       *time.Time `json:"released_at"`
	ReleaseReason    *string    `json:"release_reason"`
}

type leaseListResponse struct {
	Leases           []lease        `json:"leases"`
	Counts           map[string]int `json:"counts"`
	ProtectedSuspect int            `json:"protected_suspect"`
	Truncated        bool           `json:"truncated"`
}

type host struct {
	ID              string     `json:"id"`
	RackID          *string    `json:"rack_id"`
	RackUnit        *int       `json:"rack_unit"`
	HostEpoch       int64      `json:"host_epoch"`
	ADBEndpoint     string     `json:"adb_endpoint"`
	AdminState      string     `json:"admin_state"`
	KernelRelease   *string    `json:"kernel_release"`
	AgentVersion    *string    `json:"agent_version"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	Devices         int        `json:"devices"`
	Healthy         int        `json:"healthy"`
	Unhealthy       int        `json:"unhealthy"`
	LiveLeases      int        `json:"live_leases"`
	ProtectedLeases int        `json:"protected_leases"`
}

type hostListResponse struct {
	Hosts []host `json:"hosts"`
}

type quarantine struct {
	ID       int64     `json:"id"`
	Scope    string    `json:"scope"`
	DeviceID *string   `json:"device_id"`
	FarmUID  *string   `json:"farm_uid"`
	SlotID   *int64    `json:"slot_id"`
	RackSlot *string   `json:"rack_slot"`
	HubID    *int64    `json:"hub_id"`
	HostID   *string   `json:"host_id"`
	Reason   string    `json:"reason"`
	OpenedAt time.Time `json:"opened_at"`
	Auto     bool      `json:"auto"`
}

type recoveryTier struct {
	Tier           int    `json:"tier"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	BlastRadius    string `json:"blast_radius"`
	RequiresPolicy string `json:"requires_policy"`
	CooldownS      int64  `json:"cooldown_s"`
	MaxPerHour     int    `json:"max_per_hour"`
	Enabled        bool   `json:"enabled"`
}

type recoveryResponse struct {
	Attempts    []recoveryAttempt `json:"attempts"`
	Quarantines []quarantine      `json:"quarantines"`
	Tiers       []recoveryTier    `json:"tiers"`
}

type bulkRun struct {
	ID         string     `json:"id"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	Command    string     `json:"command"`
	MaxPerHub  int        `json:"max_per_hub"`
	TimeoutS   int64      `json:"timeout_s"`
	State      string     `json:"state"`
	FinishedAt *time.Time `json:"finished_at"`
	Targets    int        `json:"targets"`
	Pending    int        `json:"pending"`
	Running    int        `json:"running"`
	OK         int        `json:"ok"`
	Errors     int        `json:"errors"`
	Skipped    int        `json:"skipped"`
}

type bulkTarget struct {
	DeviceID   string     `json:"device_id"`
	FarmUID    string     `json:"farm_uid"`
	RackSlot   *string    `json:"rack_slot"`
	HostID     *string    `json:"host_id"`
	HubID      *int64     `json:"hub_id"`
	State      string     `json:"state"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ExitCode   *int       `json:"exit_code"`
	Output     *string    `json:"output"`
	Error      *string    `json:"error"`
}

type bulkGetResponse struct {
	Run     bulkRun      `json:"run"`
	Targets []bulkTarget `json:"targets"`
}

type bulkCreateResponse struct {
	RunID     string `json:"run_id"`
	State     string `json:"state"`
	Targets   int    `json:"targets"`
	Skipped   int    `json:"skipped"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note"`
}

type artifact struct {
	SHA256      string    `json:"sha256"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Size        int64     `json:"size_bytes"`
	Package     *string   `json:"package"`
	VersionCode *int64    `json:"version_code"`
	URL         *string   `json:"url"`
	UploadedBy  *string   `json:"uploaded_by"`
	CreatedAt   time.Time `json:"created_at"`

	// DevicesPresent counts the devices whose ledger says these exact bytes
	// are already on them — the number that decides whether a push step costs
	// 200 MB over USB or nothing at all. It is a pointer because the upload
	// reply does not carry it, and a zero there would claim the build is on no
	// device rather than that this response never said.
	DevicesPresent *int64 `json:"devices_present"`
}

type artifactListResponse struct {
	Artifacts []artifact `json:"artifacts"`
}

// ---------------------------------------------------------------------------
// fleet
// ---------------------------------------------------------------------------

func cmdFleet(ctx context.Context, s *session, args []string) error {
	fs := newFlags("fleet", s.err)
	var g globals
	g.bind(fs)
	host := fs.String("host", "", "only devices on this host")
	hubFlag := fs.String("hub", "", "only devices on this hub (usb path or id)")
	health := fs.String("health", "", "health state, or \"unhealthy\" for everything that is not fine")
	pool := fs.String("pool", "", "only devices in this pool")
	query := fs.String("q", "", "substring match on uid, serial, model, rack slot or holder")
	limit := fs.Int("limit", 1000, "maximum devices to return")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("fleet takes no arguments; did you mean `ctl device %s`?", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "host", *host)
	setIf(q, "hub", *hubFlag)
	setIf(q, "health", *health)
	setIf(q, "pool", *pool)
	setIf(q, "q", *query)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[fleetResponse](ctx, e.client, apiPrefix+"/fleet", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	// Hubs are indexed so a group heading can carry the correlation verdict.
	// Devices that fail together almost always share a hub or a power domain,
	// and a listing that presents six of them as six unrelated faults is how an
	// operator spends an hour rebooting phones that were never the problem.
	hubs := make(map[string]hub, len(resp.Hubs))
	for _, h := range resp.Hubs {
		hubs[h.HostID+"\x00"+h.USBPath] = h
	}

	t := NewTable("RACK SLOT", "UID", "MODEL", "ANDROID", "HEALTH", "ADB", "ADMIN", "LEASE", "JOB", "HOLDER", "BATT")
	group := ""
	for _, d := range resp.Devices {
		hostID, hubPath := str(d.HostID), str(d.HubPath)
		if hostID == "" {
			hostID = "(unassigned)"
		}
		if hubPath == "" {
			hubPath = "(no hub)"
		}
		if key := hostID + " / " + hubPath; key != group {
			group = key
			t.Section("%s", fleetGroupHeading(hostID, hubPath, hubs[str(d.HostID)+"\x00"+str(d.HubPath)]))
		}

		leaseCell, jobCell, holderCell := "free", "—", "—"
		if d.Lease != nil {
			leaseCell = d.Lease.State
			if d.Lease.Protected {
				leaseCell += "*"
			}
			jobCell = shortID(d.Lease.JobID)
			holderCell = d.Lease.Holder
		}
		health := dash(d.Health)
		if d.QuarantineID != nil {
			health += " Q"
		}
		t.Row(
			rackSlotOf(d.RackSlot),
			d.FarmUID,
			dash(d.Model),
			androidOf(d.AndroidRelease, d.SDKInt),
			health,
			dash(d.ADBState),
			d.AdminState,
			leaseCell,
			jobCell,
			holderCell,
			batteryOf(d.BatteryPct, d.BatteryTempDeci),
		)
	}

	if t.Len() == 0 {
		e.out.Text("no device matched.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}

	c := resp.Counts
	e.out.Blank()
	e.out.Text("%d devices: %d free, %d leased, %d unhealthy, %d quarantined, %d protected",
		c.Total, c.Free, c.Leased, c.Unhealthy, c.Quarantined, c.Protected)
	e.out.Text("health: %s", countsLine(c.Health, "healthy", "degraded", "unhealthy", "unknown", "retired"))
	if len(c.LeaseState) > 0 {
		e.out.Text("leases: %s", countsLine(c.LeaseState, "held", "suspect"))
	}
	e.out.Text("* on a lease state marks a protected lease: it is never reclaimed automatically. " +
		"Q marks an open quarantine. Job ids are shortened; -o json carries them in full.")
	if resp.Truncated {
		e.warnf("the listing hit its limit of %d devices and was cut there; "+
			"narrow it with --host, --hub or --pool, or raise --limit", *limit)
	}
	return nil
}

func fleetGroupHeading(hostID, hubPath string, h hub) string {
	head := hostID + " / hub " + hubPath
	if h.Devices == 0 {
		return head
	}
	head += fmt.Sprintf("  (%d devices, %d unhealthy", h.Devices, h.Unhealthy)
	if h.Correlated {
		head += "; CORRELATED — suspect the hub, its cable or its power domain before the phones"
	}
	return head + ")"
}

// ---------------------------------------------------------------------------
// device, device exec
// ---------------------------------------------------------------------------

func cmdDevice(ctx context.Context, s *session, args []string) error {
	if len(args) > 0 && args[0] == "exec" {
		return cmdDeviceExec(ctx, s, args[1:])
	}
	fs := newFlags("device", s.err)
	var g globals
	g.bind(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl device <id|farm_uid>  |  ctl device exec <id> -- <command>")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	resp, raw, err := fetch[deviceResponse](ctx, e.client, apiPrefix+"/devices/"+url.PathEscape(rest[0]), nil)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}
	return renderDevice(e, resp)
}

func renderDevice(e *env, resp deviceResponse) error {
	d := resp.Device
	f := &Fields{}
	// Rack slot first, always: it is the only field that tells an operator
	// where to walk.
	f.Add("rack slot", rackSlotOf(d.RackSlot))
	f.Add("farm uid", d.FarmUID)
	f.Add("device id", d.DeviceID)
	f.Add("pool", d.Pool)
	f.Add("admin state", d.AdminState)
	f.Gap()

	f.Add("model", strings.TrimSpace(str(d.Manufacturer)+" "+str(d.Model)))
	f.Add("android", androidOf(d.AndroidRelease, d.SDKInt))
	// The serial is evidence, never an address. Duplicate OEM serials are real
	// hardware, so it is printed as a fact about the device and never used to
	// target one; every call ctl makes carries the device id, and every call the
	// server makes carries the devpath.
	serial := dash(d.ADBSerial)
	if d.SerialAmbiguous {
		serial += "  (AMBIGUOUS: another device reports this same serial — evidence only, never an address)"
	}
	f.Add("adb serial", serial)
	f.Gap()

	f.Add("host", dash(d.HostID))
	f.Add("host admin state", dash(d.HostAdminState))
	f.Add("adb endpoint", dash(d.ADBEndpoint))
	f.Add("adb devpath", dash(d.ADBDevpath))
	f.Add("usb path", dash(d.USBPath))
	if d.HubID != nil {
		f.Addf("hub", "%d (%s)", *d.HubID, dash(d.HubPath))
	}
	f.Add("slot state", dash(d.SlotState))
	if d.RearmAt != nil && d.RearmAt.After(time.Now()) {
		f.Addf("slot rearm", "%s — nothing is scheduled here until it elapses", stamp(d.RearmAt))
	}
	f.Gap()

	f.Add("health", dash(d.Health))
	f.Add("health since", stamp(d.HealthSince))
	f.Add("adb state", dash(d.ADBState))
	f.Add("battery", batteryOf(d.BatteryPct, d.BatteryTempDeci))
	f.Add("consecutive bad probes", dashInt(d.ConsecBad))
	f.Add("recovery ladder tier", dashInt(d.LadderTier))
	f.Addf("failure score", "%.2f", d.FailureScore)
	f.Add("last seen", ago(d.LastSeenAt))
	f.Addf("fence floor", "%d", d.FenceFloor)
	if d.QuarantineID != nil {
		f.Addf("quarantine", "#%d %s", *d.QuarantineID, dash(d.QuarantineReason))
	}

	if l := d.Lease; l != nil {
		f.Gap()
		f.Add("lease", l.ID)
		f.Addf("lease state", "%s%s", l.State, protectedNote(l.Protected))
		f.Addf("fence", "%d", l.Fence)
		f.Add("job", l.JobID)
		f.Add("tenant", l.TenantID)
		f.Add("holder", l.Holder)
		f.Add("acquired", stamp(&l.AcquiredAt))
		f.Add("expires", stamp(&l.ExpiresAt))
		f.Add("reclaimable", stamp(&l.ReclaimableAt))
	} else {
		f.Gap()
		f.Add("lease", "none — this device holds no live lease")
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if h := resp.Hub; h != nil && h.Unhealthy > 1 {
		e.out.Blank()
		e.out.Text("hub %s on %s: %d of %d devices unhealthy — a correlated fault. "+
			"Suspect the hub, its cable or its power domain before this phone.",
			h.USBPath, h.HostID, h.Unhealthy, h.Devices)
	}

	if len(resp.Recovery) > 0 {
		e.out.Blank()
		e.out.Text("recovery history:")
		t := NewTable("STARTED", "TIER", "NAME", "BLAST RADIUS", "OUTCOME", "REFUSAL")
		for _, a := range resp.Recovery {
			t.Row(stamp(&a.StartedAt), strconv.Itoa(a.Tier), a.TierName, a.BlastRadius,
				dash(a.Outcome), dash(a.Refusal))
		}
		if err := e.out.Table(t); err != nil {
			return err
		}
	}
	return nil
}

func protectedNote(protected bool) string {
	if !protected {
		return ""
	}
	return " (protected: never reclaimed automatically — it holds and pages instead)"
}

// execRequest is the body of POST /devices/{id}/exec.
type execRequest struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	Force     bool   `json:"force,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func cmdDeviceExec(ctx context.Context, s *session, args []string) error {
	head, tail, found := splitCommand(args)
	if !found || len(tail) == 0 {
		return usageErrf("usage: ctl device exec <id> --reason r -- <command>")
	}
	fs := newFlags("device exec", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	force := fs.Bool("force", false, "run even though the device holds a live lease")
	timeout := fs.Duration("exec-timeout", 30*time.Second, "how long the command may run on the device")

	rest, err := parseArgs(fs, head)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl device exec <id> --reason r -- <command>")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("device exec"); err != nil {
		return err
	}

	command := strings.Join(tail, " ")
	path := apiPrefix + "/devices/" + url.PathEscape(rest[0])

	// The preflight is a read, so an operator sees the rack position and the
	// current holder BEFORE anything runs on the hardware.
	target, _, err := fetch[deviceResponse](ctx, e.client, path, nil)
	if err != nil {
		return err
	}
	d := target.Device

	f := &Fields{}
	f.Add("rack slot", rackSlotOf(d.RackSlot))
	f.Add("farm uid", d.FarmUID)
	f.Add("host", dash(d.HostID))
	f.Add("adb devpath", dash(d.ADBDevpath))
	f.Add("command", command)
	headline := fmt.Sprintf("About to run a shell command on 1 device at %s.", rackSlotOf(d.RackSlot))
	if d.Lease != nil {
		f.Gap()
		f.Add("live lease", d.Lease.ID)
		f.Add("job", d.Lease.JobID)
		f.Add("tenant", d.Lease.TenantID)
		f.Add("holder", d.Lease.Holder)
		f.Addf("protected", "%s", yesNo(d.Lease.Protected))
		if *force {
			headline = fmt.Sprintf("About to run a shell command on %s WHILE SOMEBODY'S JOB IS USING IT.\n"+
				"This can corrupt that run, and unlike a revoke the holder gets no signal that it happened.",
				rackSlotOf(d.RackSlot))
		} else {
			headline = fmt.Sprintf("Device %s holds a live lease. Without --force the API will refuse this.",
				rackSlotOf(d.RackSlot))
		}
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	res, raw, err := send[execResponse](ctx, e.client, path+"/exec", execRequest{
		Command:   command,
		TimeoutMS: int(timeout.Milliseconds()),
		Force:     *force,
		Reason:    e.reason,
	})
	if err != nil {
		return err
	}

	if e.format == FormatJSON {
		// The body is emitted and THEN the exit code is honoured. A script
		// asking for -o json is the one most likely to be reading the exit
		// status rather than the document, and rendering a failed command as
		// a successful invocation would hide every failure from it.
		if err := e.out.RawJSON(raw); err != nil {
			return err
		}
		return execOutcome(res)
	}

	// The device's own bytes go to stdout verbatim, so `ctl device exec ... |
	// grep` behaves the way the same command piped from adb would.
	if res.Output != "" {
		fmt.Fprint(e.session.out, res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			fmt.Fprintln(e.session.out)
		}
	}
	if res.Stderr != "" {
		fmt.Fprint(e.err, res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			fmt.Fprintln(e.err)
		}
	}
	if res.Truncated {
		e.warnf("output was truncated by the server's per-command cap")
	}
	if !res.Exited {
		e.warnf("the command did not report an exit status; it may still be running on the device. "+
			"No lease was affected (devpath %s).", res.Devpath)
	}
	e.warnf("exit %d in %s at devpath %s", res.ExitCode, millis(res.DurationMS), res.Devpath)
	return execOutcome(res)
}

// execOutcome mirrors the device's exit code onto ctl's.
//
// The transport worked and the API answered; the command on the phone failed.
// Reporting that as ctl's own failure is what lets a script read it the way it
// would read a local command's status — and it says nothing about any lease,
// which is why the message names the command and not the device.
func execOutcome(res execResponse) error {
	if res.ExitCode == 0 {
		return nil
	}
	return fmt.Errorf("the command exited %d on the device", res.ExitCode)
}

// ---------------------------------------------------------------------------
// hosts, host drain/undrain
// ---------------------------------------------------------------------------

func cmdHosts(ctx context.Context, s *session, args []string) error {
	fs := newFlags("hosts", s.err)
	var g globals
	g.bind(fs)
	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("hosts takes no arguments; did you mean `ctl host drain %s`?", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	resp, raw, err := fetch[hostListResponse](ctx, e.client, apiPrefix+"/hosts", nil)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	t := NewTable("RACK", "HOST", "ADMIN", "ENDPOINT", "DEVICES", "HEALTHY", "UNHEALTHY", "LIVE LEASES", "PROTECTED", "AGENT", "LAST SEEN")
	for _, h := range resp.Hosts {
		t.Row(rackPositionOf(h), h.ID, h.AdminState, h.ADBEndpoint,
			strconv.Itoa(h.Devices), strconv.Itoa(h.Healthy), strconv.Itoa(h.Unhealthy),
			strconv.Itoa(h.LiveLeases), strconv.Itoa(h.ProtectedLeases),
			dash(h.AgentVersion), ago(h.LastSeenAt))
	}
	if t.Len() == 0 {
		e.out.Text("no hosts are registered.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("LIVE LEASES is what a drain would have to wait out. Draining stops new " +
		"placement and releases nothing.")
	return nil
}

// rackPositionOf renders a host's physical position, which is what a rack slot
// is for a device: the thing an operator walks to.
func rackPositionOf(h host) string {
	switch {
	case h.RackID == nil && h.RackUnit == nil:
		return unslotted
	case h.RackUnit == nil:
		return *h.RackID
	case h.RackID == nil:
		return fmt.Sprintf("u%d", *h.RackUnit)
	}
	return fmt.Sprintf("%s u%d", *h.RackID, *h.RackUnit)
}

func cmdHost(ctx context.Context, s *session, args []string) error {
	if len(args) == 0 {
		return usageErrf("usage: ctl host drain <id> --reason r  |  ctl host undrain <id> --reason r")
	}
	action := args[0]
	switch action {
	case "drain", "undrain":
	default:
		return usageErrf("ctl host takes drain or undrain, not %q", action)
	}

	fs := newFlags("host "+action, s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl host %s <id> --reason r", action)
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("host " + action); err != nil {
		return err
	}
	hostID := rest[0]

	// Preflight from the hosts listing: the number that matters is how many
	// live leases are on the box, because that is the work a drain makes
	// somebody wait for — and, if this is misread as "evacuate", the work
	// somebody would destroy.
	f := &Fields{}
	f.Add("host", hostID)
	listing, _, listErr := fetch[hostListResponse](ctx, e.client, apiPrefix+"/hosts", nil)
	found := false
	if listErr != nil {
		e.warnf("could not read the host list for the preflight (%v); the server remains the authority", listErr)
	} else {
		for _, h := range listing.Hosts {
			if h.ID != hostID {
				continue
			}
			found = true
			f.Add("rack position", rackPositionOf(h))
			f.Add("current admin state", h.AdminState)
			f.Addf("devices", "%d (%d healthy, %d unhealthy)", h.Devices, h.Healthy, h.Unhealthy)
			f.Addf("live leases", "%d, of which %d protected", h.LiveLeases, h.ProtectedLeases)
		}
		if !found {
			e.warnf("no host %q is in the listing; the request will be sent anyway and the server will answer", hostID)
		}
	}

	headline := fmt.Sprintf("About to DRAIN host %s: no new lease will be placed on it.\n"+
		"Live leases are NOT released by this and will run to completion.", hostID)
	if action == "undrain" {
		headline = fmt.Sprintf("About to UNDRAIN host %s: it becomes eligible for new placement again.", hostID)
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/hosts/"+url.PathEscape(hostID)+"/"+action,
		map[string]string{"reason": e.reason})
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	var res struct {
		HostID          string `json:"host_id"`
		AdminState      string `json:"admin_state"`
		PreviousState   string `json:"previous_state"`
		LiveLeases      int    `json:"live_leases"`
		ProtectedLeases int    `json:"protected_leases"`
		Note            string `json:"note"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("host", res.HostID)
	out.Addf("admin state", "%s (was %s)", res.AdminState, res.PreviousState)
	out.Addf("live leases", "%d, of which %d protected — untouched", res.LiveLeases, res.ProtectedLeases)
	if res.Note != "" {
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}

// ---------------------------------------------------------------------------
// jobs, job, job cancel
// ---------------------------------------------------------------------------

func cmdJobs(ctx context.Context, s *session, args []string) error {
	fs := newFlags("jobs", s.err)
	var g globals
	g.bind(fs)
	state := fs.String("state", "", "all, live, terminal, queued, allocating, running, succeeded, failed or cancelled")
	pool := fs.String("pool", "", "only jobs in this pool")
	queue := fs.String("queue", "", "only jobs in this queue")
	tenant := fs.String("tenant", "", "only jobs for this tenant")
	limit := fs.Int("limit", 200, "maximum jobs to return")

	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("jobs takes no arguments; did you mean `ctl job %s`?", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "state", *state)
	setIf(q, "pool", *pool)
	setIf(q, "queue", *queue)
	setIf(q, "tenant", *tenant)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[jobListResponse](ctx, e.client, apiPrefix+"/jobs", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	// A job's rack slot is a property of the device its lease holds, and the
	// job listing carries only a device id. One extra read turns a column of
	// uuids into a column of places an operator can walk to, which is the whole
	// point of putting it first.
	racks := e.racksByJob(ctx, resp.Jobs)

	t := NewTable("RACK SLOT", "JOB", "STATE", "POOL", "QUEUE", "TENANT", "MAX RUNTIME", "AGE")
	for _, j := range resp.Jobs {
		t.Row(racks[j.ID], j.ID, jobStateCell(j), j.PoolID, j.QueueID, j.TenantID,
			secondsCell(j.MaxRuntimeS), ago(&j.CreatedAt))
	}
	if t.Len() == 0 {
		e.out.Text("no job matched.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("%d jobs: %s", len(resp.Jobs), countsLine(resp.Counts,
		"queued", "allocating", "running", "succeeded", "failed", "cancelled"))
	e.out.Text("* marks a protected job: its lease is never reclaimed automatically. " +
		"MAX RUNTIME is the only user-supplied clock that may end a lease.")
	if resp.Truncated {
		e.warnf("the listing hit its limit of %d jobs and was cut there", *limit)
	}
	return nil
}

func jobStateCell(j job) string {
	if j.Protected || (j.Lease != nil && j.Lease.Protected) {
		return j.State + "*"
	}
	return j.State
}

func secondsCell(p *int64) string {
	if p == nil {
		return "—"
	}
	return duration(*p)
}

// racksByJob maps job ids onto rack positions using the live lease listing.
//
// A failure here degrades the column to "(unknown)" and says so on stderr. It
// never degrades to "(unslotted)": reporting a lookup failure as a fact about
// the hardware is how a listing sends somebody to the wrong rack.
func (e *env) racksByJob(ctx context.Context, jobs []job) map[string]string {
	out := make(map[string]string, len(jobs))
	need := false
	for _, j := range jobs {
		if j.Lease != nil {
			need = true
		}
		out[j.ID] = unallocated
	}
	if !need {
		return out
	}
	resp, _, err := fetch[leaseListResponse](ctx, e.client, apiPrefix+"/leases",
		url.Values{"state": {"live"}, "limit": {"2000"}})
	if err != nil {
		e.warnf("could not resolve rack positions for these jobs (%v)", err)
		for _, j := range jobs {
			if j.Lease != nil {
				out[j.ID] = unknownSlot
			}
		}
		return out
	}
	byJob := make(map[string]lease, len(resp.Leases))
	for _, l := range resp.Leases {
		byJob[l.JobID] = l
	}
	for _, j := range jobs {
		if j.Lease == nil {
			continue
		}
		if l, ok := byJob[j.ID]; ok {
			out[j.ID] = rackSlotOf(l.RackSlot)
		} else {
			out[j.ID] = unknownSlot
		}
	}
	return out
}

func cmdJob(ctx context.Context, s *session, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "cancel":
			return cmdJobCancel(ctx, s, args[1:])
		// A job that failed on four devices is a job problem; the same failure
		// four times on one device is a device problem. These two subcommands
		// are how an operator tells them apart.
		case "steps":
			return cmdJobSteps(ctx, s, args[1:])
		case "attempts":
			return cmdJobAttempts(ctx, s, args[1:])
		}
	}
	fs := newFlags("job", s.err)
	var g globals
	g.bind(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl job <id>  |  ctl job cancel <id> --reason r  |  ctl job steps <id> [--attempt n]  |  ctl job attempts <id>")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	resp, raw, err := fetch[jobResponse](ctx, e.client, apiPrefix+"/jobs/"+url.PathEscape(rest[0]), nil)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	j := resp.Job
	f := &Fields{}
	f.Add("rack slot", e.rackForJob(ctx, j))
	f.Add("job", j.ID)
	f.Add("state", j.State)
	f.Add("tenant", j.TenantID)
	f.Add("queue", j.QueueID)
	f.Add("pool", j.PoolID)
	f.AddOpt("created by", j.CreatedBy)
	f.Add("created", stamp(&j.CreatedAt))
	f.Add("started", stamp(j.StartedAt))
	f.Add("finished", stamp(j.FinishedAt))
	f.Gap()

	f.Addf("protected", "%s%s", yesNo(j.Protected), protectedNote(j.Protected))
	f.Add("disruption policy", j.DisruptionPolicy)
	f.Addf("expected duration", "%s%s", secondsCell(j.ExpectedDurationS), protectionHint(j.ExpectedDurationS))
	f.Addf("max runtime", "%s  (the only user-supplied clock that may end this lease)", secondsCell(j.MaxRuntimeS))
	f.Addf("ttl / grace", "%s / %s", duration(j.TTLSeconds), duration(j.GraceSeconds))
	f.AddOpt("pinned device", j.PinDevice)

	if l := j.Lease; l != nil {
		f.Gap()
		f.Add("live lease", l.ID)
		f.Addf("fence", "%d", l.Fence)
		f.Add("lease state", l.State)
		f.Add("device", l.DeviceID)
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if len(resp.Leases) > 0 {
		e.out.Blank()
		e.out.Text("lease history — 'holder_expired' here is work that was taken away, " +
			"'completed' is work that finished:")
		t := NewTable("LEASE", "FENCE", "STATE", "HOLDER", "ACQUIRED", "RELEASED", "REASON")
		for _, h := range resp.Leases {
			t.Row(h.ID, strconv.FormatInt(h.Fence, 10), h.State, h.Holder,
				stamp(&h.AcquiredAt), stamp(h.ReleasedAt), dash(h.ReleaseReason))
		}
		if err := e.out.Table(t); err != nil {
			return err
		}
	}
	if len(j.Spec) > 0 && !isEmptyJSON(j.Spec) {
		e.out.Blank()
		e.out.Text("spec: %s", clip(string(j.Spec), 240))
	}
	return nil
}

// protectionHint explains the threshold in migrations/00002_lease.sql rather
// than making an operator remember it: farm.lease_acquire marks a lease
// protected when expected_duration exceeds thirty minutes.
func protectionHint(expected *int64) string {
	if expected == nil || *expected <= int64(protectionThreshold.Seconds()) {
		return ""
	}
	return "  (over 30m, so this job's lease is protected and is never reclaimed automatically)"
}

// protectionThreshold mirrors farm.lease_acquire's
// `expected_duration > interval '30 minutes'`. The database is the authority;
// this copy exists only so ctl can explain what the operator is about to see.
const protectionThreshold = 30 * time.Minute

func (e *env) rackForJob(ctx context.Context, j job) string {
	if j.Lease == nil {
		return unallocated
	}
	d, _, err := fetch[deviceResponse](ctx, e.client,
		apiPrefix+"/devices/"+url.PathEscape(j.Lease.DeviceID), nil)
	if err != nil {
		e.warnf("could not resolve the rack position of device %s (%v)", j.Lease.DeviceID, err)
		return unknownSlot
	}
	return rackSlotOf(d.Device.RackSlot)
}

func cmdJobCancel(ctx context.Context, s *session, args []string) error {
	fs := newFlags("job cancel", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl job cancel <id> --reason r")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("job cancel"); err != nil {
		return err
	}
	jobID := rest[0]

	f := &Fields{}
	f.Add("job", jobID)
	target, _, readErr := fetch[jobResponse](ctx, e.client, apiPrefix+"/jobs/"+url.PathEscape(jobID), nil)
	if readErr != nil {
		e.warnf("could not read the job for the preflight (%v); the server remains the authority", readErr)
	} else {
		j := target.Job
		f.Add("rack slot", e.rackForJob(ctx, j))
		f.Add("state", j.State)
		f.Add("tenant", j.TenantID)
		f.Add("queue", j.QueueID)
		f.AddOpt("created by", j.CreatedBy)
		f.Add("running for", ago(j.StartedAt))
		if l := j.Lease; l != nil {
			f.Gap()
			f.Add("live lease", l.ID)
			f.Addf("fence", "%d", l.Fence)
			f.Add("device", l.DeviceID)
			f.Addf("protected", "%s", yesNo(l.Protected))
		}
	}

	headline := "About to CANCEL this job. Its live lease, if it has one, is released with " +
		"reason 'job_cancelled', the device's fence floor is raised, and the slot is held " +
		"unschedulable for its rearm window. Work in progress on the device is lost."
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/jobs/"+url.PathEscape(jobID)+"/cancel",
		map[string]string{"reason": e.reason})
	if err != nil {
		return e.unknownOutcome(err, "ctl job "+jobID)
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	var res struct {
		JobID         string `json:"job_id"`
		State         string `json:"state"`
		LeaseID       string `json:"lease_id"`
		LeaseReleased bool   `json:"lease_released"`
		DeviceID      string `json:"device_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("job", res.JobID)
	out.Add("state", res.State)
	if res.LeaseID != "" {
		out.Add("lease", res.LeaseID)
		out.Add("lease released", yesNo(res.LeaseReleased))
		out.Add("device", res.DeviceID)
	} else {
		out.Add("lease", "none — the job held no device")
	}
	return e.out.Fields(out)
}

// ---------------------------------------------------------------------------
// submit, validate
// ---------------------------------------------------------------------------

// jobCreateRequest is the body of POST /jobs.
type jobCreateRequest struct {
	Pool              string          `json:"pool"`
	Queue             string          `json:"queue"`
	Tenant            string          `json:"tenant"`
	Spec              json.RawMessage `json:"spec,omitempty"`
	Selector          json.RawMessage `json:"selector,omitempty"`
	ExpectedDurationS int64           `json:"expected_duration_s,omitempty"`
	MaxRuntimeS       int64           `json:"max_runtime_s,omitempty"`
}

func cmdSubmit(ctx context.Context, s *session, args []string) error {
	fs := newFlags("submit", s.err)
	var g globals
	g.bind(fs)
	file := fs.String("f", "", "path to the job spec, or - for stdin")
	pool := fs.String("pool", "", "pool to allocate from (required)")
	queue := fs.String("queue", "", "queue to file under (required)")
	tenant := fs.String("tenant", "", "tenant that owns the job (required)")
	profile := fs.String("profile", "", "device profile the job expects, carried in the selector")
	expect := fs.Duration("expect-duration", 0, "how long the job is expected to take; over 30m the lease is protected")
	maxRuntime := fs.Duration("max-runtime", 0, "hard deadline; the only user-supplied clock that may end the lease")

	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("submit takes no positional arguments; pass the spec with -f %s", rest[0])
	}
	if strings.TrimSpace(*file) == "" {
		return usageErrf("submit needs -f <spec.json>")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(*pool) == "":
		return usageErrf("submit needs --pool")
	case strings.TrimSpace(*queue) == "":
		return usageErrf("submit needs --queue")
	case strings.TrimSpace(*tenant) == "":
		return usageErrf("submit needs --tenant")
	}
	// A negative max runtime is a deadline in the past. farm.jobs.max_runtime
	// is an unconstrained interval and the reaper expires a lease once
	// now() > acquired_at + max_runtime, so `--max-runtime -1h` files a job
	// whose lease dies on the first pass after it is acquired — and it dies
	// with release_reason 'max_runtime', which reads as the deadline working.
	// The only user-supplied clock that may end a lease has to be a clock the
	// user meant, so a typed minus sign is refused here rather than turned
	// into a job that cannot run.
	if *maxRuntime < 0 {
		return usageErrf("--max-runtime is %s: a negative deadline is already in the past, and "+
			"this job's lease would be expired as soon as it was acquired", *maxRuntime)
	}
	if *expect < 0 {
		return usageErrf("--expect-duration is %s; it is how long the job is expected to take", *expect)
	}

	raw, err := readSpecFile(*file, s.in)
	if err != nil {
		return err
	}
	// Validated here, before anything is filed. The server validates too and is
	// the authority; doing it locally first means a spec with nine defects
	// produces nine messages and no half-created job.
	spec, err := jobspec.Parse(raw)
	if err != nil {
		reportSpecProblems(e, err)
		return fmt.Errorf("%s did not validate; nothing was submitted", *file)
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("re-encode spec: %w", err)
	}

	req := jobCreateRequest{
		Pool:              strings.TrimSpace(*pool),
		Queue:             strings.TrimSpace(*queue),
		Tenant:            strings.TrimSpace(*tenant),
		Spec:              canonical,
		ExpectedDurationS: int64(expect.Seconds()),
		MaxRuntimeS:       int64(maxRuntime.Seconds()),
	}
	if p := strings.TrimSpace(*profile); p != "" {
		sel, err := json.Marshal(map[string]string{"profile": p})
		if err != nil {
			return fmt.Errorf("encode selector: %w", err)
		}
		req.Selector = sel
	}

	body, err := e.client.Post(ctx, apiPrefix+"/jobs", req)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(body)
	}

	var res struct {
		Job job    `json:"job"`
		ID  string `json:"id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return e.out.RawJSON(body)
	}
	id := res.Job.ID
	if id == "" {
		id = res.ID
	}
	f := &Fields{}
	f.Add("job", id)
	f.Add("state", firstNonEmpty(res.Job.State, "queued"))
	f.Add("pool", req.Pool)
	f.Add("queue", req.Queue)
	f.Add("tenant", req.Tenant)
	f.Addf("steps", "%d", len(spec.Steps))
	f.Addf("spec total timeout", "%s", spec.TotalTimeout())
	if *expect > 0 {
		note := ""
		if *expect > protectionThreshold {
			note = "  (over 30m: the lease will be protected and is never reclaimed automatically)"
		}
		f.Addf("expected duration", "%s%s", *expect, note)
	}
	if *maxRuntime > 0 {
		f.Addf("max runtime", "%s  (this deadline, and nothing else, may end the lease on a clock)", *maxRuntime)
	} else {
		f.Add("max runtime", "none — this lease ends when the job ends or a human revokes it")
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("nothing is scheduled yet: the scheduler turns this row into a lease.")
	e.out.Text("follow it with: ctl job %s", id)
	return nil
}

func cmdValidate(ctx context.Context, s *session, args []string) error {
	_ = ctx // validation is local; no request is made and none is needed

	fs := newFlags("validate", s.err)
	var g globals
	g.bind(fs)
	file := fs.String("f", "", "path to the job spec, or - for stdin")
	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("validate takes no positional arguments; pass the spec with -f %s", rest[0])
	}
	if strings.TrimSpace(*file) == "" {
		return usageErrf("validate needs -f <spec.json>")
	}
	format, err := ParseFormat(g.output)
	if err != nil {
		return err
	}
	// This command opens no connection, so it deliberately does not build a
	// client: `ctl validate` has to work on a laptop with no farm in reach.
	out := NewPrinter(s.out, format)
	e := &env{session: s, out: out, format: format}

	raw, err := readSpecFile(*file, s.in)
	if err != nil {
		return err
	}
	spec, parseErr := jobspec.Parse(raw)
	if parseErr != nil {
		if format == FormatJSON {
			// The report is emitted and THEN the command fails. A validation
			// gate in CI reads the exit code, not the document, so rendering a
			// list of defects and exiting 0 would let a broken spec through the
			// one check that exists to stop it.
			if err := out.JSON(map[string]any{
				"valid":    false,
				"problems": specProblems(parseErr),
				"error":    parseErr.Error(),
			}); err != nil {
				return err
			}
		} else {
			reportSpecProblems(e, parseErr)
		}
		return fmt.Errorf("%s is not a valid job spec", *file)
	}

	if format == FormatJSON {
		return out.JSON(map[string]any{
			"valid":         true,
			"version":       spec.Version,
			"steps":         len(spec.Steps),
			"total_timeout": spec.TotalTimeout().String(),
			"artifacts":     specArtifacts(spec),
		})
	}

	t := NewTable("#", "STEP", "KIND", "TIMEOUT", "ON ERROR", "DETAIL")
	for i, st := range spec.Steps {
		onErr := "stop"
		if st.ContinueOnError {
			onErr = "continue"
		}
		t.Row(strconv.Itoa(i), st.ID, string(st.Kind()), spec.StepTimeout(st).String(), onErr, stepDetail(st))
	}
	if err := out.Table(t); err != nil {
		return err
	}
	out.Blank()
	out.Text("%s is valid: version %d, %s, total timeout %s",
		*file, spec.Version, plural(len(spec.Steps), "step", "steps"), spec.TotalTimeout())
	if refs := specArtifacts(spec); len(refs) > 0 {
		out.Text("artifacts referenced: %s", strings.Join(refs, ", "))
		out.Text("each must already be in the store, by sha256, before this job runs: ctl artifacts")
	}
	return nil
}

// stepDetail renders the one field of a step an operator scanning a spec cares
// about. The closed vocabulary is what makes this a type switch rather than a
// reflective walk over an open map.
func stepDetail(st jobspec.Step) string {
	switch p := st.Payload.(type) {
	case jobspec.Push:
		return p.SHA256[:min(12, len(p.SHA256))] + " -> " + p.Dest
	case jobspec.Install:
		return p.SHA256[:min(12, len(p.SHA256))]
	case jobspec.Uninstall:
		return p.Package
	case jobspec.Shell:
		return p.Command
	case jobspec.ShellDetached:
		return p.Command + "  (handle " + p.Handle + ")"
	case jobspec.WaitFor:
		return p.Probe
	case jobspec.Pull:
		return p.Path + " -> artifact " + p.Artifact
	case jobspec.Assert:
		return p.Probe + " " + string(p.Operator) + " " + p.Value
	case jobspec.Reset:
		return "tier " + string(p.Tier)
	case jobspec.Sleep:
		return p.Duration.String()
	case nil:
		return "(no payload)"
	}
	return ""
}

// specArtifacts lists every content hash and artifact name a spec depends on.
func specArtifacts(spec jobspec.Spec) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, st := range spec.Steps {
		switch p := st.Payload.(type) {
		case jobspec.Push:
			add(p.SHA256)
		case jobspec.Install:
			add(p.SHA256)
		case jobspec.Pull:
			add("(produces) " + p.Artifact)
		}
	}
	return out
}

// specProblems extracts every defect from a validation failure, or the single
// message when the document did not even decode.
func specProblems(err error) []jobspec.Problem {
	var ve *jobspec.ValidationError
	if errors.As(err, &ve) {
		return ve.Problems
	}
	return []jobspec.Problem{{Path: "(document)", Message: err.Error()}}
}

// reportSpecProblems prints every defect at once. A person fixing a spec should
// need one edit, not ten round trips, which is the entire reason
// jobspec.ValidationError carries all of them.
func reportSpecProblems(e *env, err error) {
	problems := specProblems(err)
	fmt.Fprintf(e.err, "%s\n\n", plural(len(problems), "problem", "problems"))
	t := NewTable("WHERE", "PROBLEM")
	t.MaxCell(120)
	for _, p := range problems {
		t.Row(p.Path, p.Message)
	}
	_ = t.Render(e.err)
}

func readSpecFile(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read spec from stdin: %w", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read spec: %w", err)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// leases, lease revoke
// ---------------------------------------------------------------------------

func cmdLeases(ctx context.Context, s *session, args []string) error {
	fs := newFlags("leases", s.err)
	var g globals
	g.bind(fs)
	state := fs.String("state", "", "live (default), all, held, suspect, released or expired")
	host := fs.String("host", "", "only leases on this host")
	dev := fs.String("device", "", "only leases on this device id or farm uid")
	jobFlag := fs.String("job", "", "only the lease for this job")
	limit := fs.Int("limit", 200, "maximum leases to return")

	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("leases takes no arguments; did you mean `ctl lease revoke %s`?", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "state", *state)
	setIf(q, "host", *host)
	setIf(q, "device", *dev)
	setIf(q, "job", *jobFlag)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[leaseListResponse](ctx, e.client, apiPrefix+"/leases", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	t := NewTable("RACK SLOT", "UID", "STATE", "FENCE", "HOLDER", "TENANT", "JOB", "EXPIRES IN", "RECLAIMABLE IN", "LEASE")
	for _, l := range resp.Leases {
		state := l.State
		if l.Protected {
			state += "*"
		}
		t.Row(rackSlotOf(l.RackSlot), l.FarmUID, state, strconv.FormatInt(l.Fence, 10),
			l.Holder, l.TenantID, shortID(l.JobID),
			duration(l.ExpiresInS), duration(l.ReclaimableInS), l.ID)
	}
	if t.Len() == 0 {
		e.out.Text("no lease matched.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("%d leases: %s", len(resp.Leases), countsLine(resp.Counts,
		"held", "suspect", "released", "expired"))
	e.out.Text("countdowns come from the database clock, not from this machine's. " +
		"A suspect lease is still LIVE: it has been marked for alerting and nothing has been released.")
	if resp.ProtectedSuspect > 0 {
		e.warnf("%d protected lease(s) are suspect. The reaper will NOT take these back: "+
			"they are waiting for a human.", resp.ProtectedSuspect)
	}
	if resp.Truncated {
		e.warnf("the listing hit its limit of %d leases and was cut there", *limit)
	}
	return nil
}

func cmdLease(ctx context.Context, s *session, args []string) error {
	if len(args) == 0 || args[0] != "revoke" {
		return usageErrf("usage: ctl lease revoke <id> --reason r")
	}
	fs := newFlags("lease revoke", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl lease revoke <id> --reason r")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("lease revoke"); err != nil {
		return err
	}
	leaseID := rest[0]

	// Only a live lease can be revoked, so the preflight reads the live set and
	// finds this one in it. Not finding it is not fatal — the server decides —
	// but it changes what the operator is told before they answer.
	f := &Fields{}
	f.Add("lease", leaseID)
	// Kept past the preflight so that, if the reply to the revoke never
	// arrives, the operator can be pointed at the job whose lease history
	// settles whether it happened.
	heldByJob := ""
	live, _, listErr := fetch[leaseListResponse](ctx, e.client, apiPrefix+"/leases",
		url.Values{"state": {"live"}, "limit": {"2000"}})
	switch {
	case listErr != nil:
		e.warnf("could not read the live leases for the preflight (%v); the server remains the authority", listErr)
	default:
		found := false
		for _, l := range live.Leases {
			if l.ID != leaseID {
				continue
			}
			found = true
			heldByJob = l.JobID
			f.Add("rack slot", rackSlotOf(l.RackSlot))
			f.Add("device", l.FarmUID)
			f.Add("host", dash(l.HostID))
			f.Add("state", l.State)
			f.Addf("fence", "%d", l.Fence)
			f.Add("holder", l.Holder)
			f.Add("tenant", l.TenantID)
			f.Add("job", l.JobID)
			f.Add("held for", ago(&l.AcquiredAt))
			f.Addf("expires in", "%s", duration(l.ExpiresInS))
			f.Addf("protected", "%s", yesNo(l.Protected))
		}
		if !found {
			e.warnf("no LIVE lease has id %s. A revoke only applies to a live lease; "+
				"the request will be sent and the server will answer.", leaseID)
		}
	}

	headline := "About to REVOKE this lease. This ENDS SOMEBODY'S RUN NOW: the holder is fenced " +
		"out at the host proxy, the device's fence floor is raised past the revoked fence, and " +
		"the slot is unschedulable until its rearm window elapses. The holder gets no warning."
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Post(ctx, apiPrefix+"/leases/"+url.PathEscape(leaseID)+"/revoke",
		map[string]string{"reason": e.reason})
	if err != nil {
		check := "ctl leases --state all --limit 2000"
		if heldByJob != "" {
			// The job's lease history carries release_reason, which says
			// whether this revoke landed and under what reason.
			check = "ctl job " + heldByJob
		}
		return e.unknownOutcome(err, check)
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	var res struct {
		LeaseID       string `json:"lease_id"`
		Released      bool   `json:"released"`
		Reason        string `json:"reason"`
		DeviceID      string `json:"device_id"`
		JobID         string `json:"job_id"`
		RevokedFence  int64  `json:"revoked_fence"`
		NewFenceFloor int64  `json:"new_fence_floor"`
		Note          string `json:"note"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("lease", res.LeaseID)
	out.Add("released", yesNo(res.Released))
	out.Add("release reason", res.Reason)
	out.Add("device", res.DeviceID)
	out.Add("job", res.JobID)
	out.Addf("revoked fence", "%d", res.RevokedFence)
	out.Addf("new fence floor", "%d", res.NewFenceFloor)
	if res.Note != "" {
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}

// ---------------------------------------------------------------------------
// recovery
// ---------------------------------------------------------------------------

func cmdRecovery(ctx context.Context, s *session, args []string) error {
	fs := newFlags("recovery", s.err)
	var g globals
	g.bind(fs)
	dev := fs.String("device", "", "only attempts against this device")
	host := fs.String("host", "", "only attempts on this host")
	limit := fs.Int("limit", 100, "maximum attempts to return")
	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("recovery takes no arguments")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "device", *dev)
	setIf(q, "host", *host)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[recoveryResponse](ctx, e.client, apiPrefix+"/recovery", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	e.out.Text("the ladder — a tier runs only when the live lease's disruption policy permits it:")
	tiers := NewTable("TIER", "NAME", "BLAST RADIUS", "REQUIRES POLICY", "COOLDOWN", "MAX/HOUR", "ENABLED", "DESCRIPTION")
	for _, t := range resp.Tiers {
		tiers.Row(strconv.Itoa(t.Tier), t.Name, t.BlastRadius, t.RequiresPolicy,
			duration(t.CooldownS), strconv.Itoa(t.MaxPerHour), yesNo(t.Enabled), t.Description)
	}
	if err := e.out.Table(tiers); err != nil {
		return err
	}

	e.out.Blank()
	e.out.Text("recent attempts:")
	att := NewTable("RACK SLOT", "UID", "HOST", "TIER", "NAME", "BLAST RADIUS", "STARTED", "OUTCOME", "REFUSAL")
	for _, a := range resp.Attempts {
		att.Row(rackSlotOf(a.RackSlot), dash(a.FarmUID), dash(a.HostID),
			strconv.Itoa(a.Tier), a.TierName, a.BlastRadius,
			stamp(&a.StartedAt), dash(a.Outcome), dash(a.Refusal))
	}
	if att.Len() == 0 {
		e.out.Text("  none.")
	} else if err := e.out.Table(att); err != nil {
		return err
	}

	e.out.Blank()
	e.out.Text("open quarantines — nothing is scheduled onto these:")
	qt := NewTable("RACK SLOT", "SCOPE", "UID", "HOST", "HUB", "REASON", "OPENED", "AUTO")
	for _, q := range resp.Quarantines {
		qt.Row(quarantineWhere(q), q.Scope, dash(q.FarmUID), dash(q.HostID),
			dashInt64(q.HubID), q.Reason, ago(&q.OpenedAt), yesNo(q.Auto))
	}
	if qt.Len() == 0 {
		e.out.Text("  none.")
		return nil
	}
	if err := e.out.Table(qt); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("a REFUSAL is the ladder declining to act because a live lease's disruption " +
		"policy forbids that blast radius. It is the system working, not a fault.")
	return nil
}

// quarantineWhere renders a quarantine's position.
//
// A hub- or host-scoped quarantine covers no single rack slot, and printing
// "(unslotted)" for it would claim something false about hardware — that a
// device is off the rack — when the row is not about a device at all. It names
// the blast radius instead, which is also the thing an operator needs to see:
// one quarantine covering a whole hub is six phones nobody can schedule.
func quarantineWhere(q quarantine) string {
	if q.RackSlot != nil && strings.TrimSpace(*q.RackSlot) != "" {
		return *q.RackSlot
	}
	switch q.Scope {
	case "hub":
		return "(whole hub)"
	case "host":
		return "(whole host)"
	}
	return unslotted
}

// ---------------------------------------------------------------------------
// bulk
// ---------------------------------------------------------------------------

type bulkSelector struct {
	Pool      string   `json:"pool,omitempty"`
	Host      string   `json:"host,omitempty"`
	Hub       string   `json:"hub,omitempty"`
	Health    string   `json:"health,omitempty"`
	Model     string   `json:"model,omitempty"`
	DeviceIDs []string `json:"device_ids,omitempty"`

	IncludeLeased bool `json:"include_leased,omitempty"`
	// The server excludes devices that are not healthy, and devices under an
	// open quarantine, before it addresses anything. Both are mirrored here so
	// the CLI can express what the API can: an operator waking a shelf of
	// offline handsets needs include-unhealthy, and without a flag for it the
	// only interface that could reach them would be curl.
	IncludeUnhealthy   bool `json:"include_unhealthy,omitempty"`
	IncludeQuarantined bool `json:"include_quarantined,omitempty"`
}

type bulkCreateRequest struct {
	Selector  bulkSelector `json:"selector"`
	Command   string       `json:"command"`
	MaxPerHub int          `json:"max_per_hub,omitempty"`
	TimeoutMS int          `json:"timeout_ms,omitempty"`
	Reason    string       `json:"reason,omitempty"`
}

// bulkPollInterval is how often a running bulk run is re-read while it is being
// followed. It is a poll rather than a subscription because the run's target
// rows are the authority on what happened, and they are what the operator will
// still be able to read tomorrow.
const bulkPollInterval = time.Second

func cmdBulk(ctx context.Context, s *session, args []string) error {
	head, tail, found := splitCommand(args)
	if !found || len(tail) == 0 {
		return usageErrf("usage: ctl bulk --selector k=v [--selector k=v] --reason r -- <command>")
	}
	fs := newFlags("bulk", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	var selectors repeatable
	fs.Var(&selectors, "selector", "k=v; repeatable. Keys: pool, host, hub, health, model, device, "+
		"include-leased, include-unhealthy, include-quarantined")
	maxPerHub := fs.Int("max-per-hub", 4, "how many devices on one hub may answer at once")
	execTimeout := fs.Duration("exec-timeout", 30*time.Second, "how long each command may run on a device")
	follow := fs.Bool("follow", true, "stream results until the run finishes")

	if rest, err := parseArgs(fs, head); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("bulk takes no positional arguments; put the command after --, not before it")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("bulk"); err != nil {
		return err
	}
	if len(selectors) == 0 {
		return usageErrf("bulk needs at least one --selector; running a command on the whole farm " +
			"by default is not a mistake this tool will make for you")
	}
	sel, err := parseSelectors(selectors)
	if err != nil {
		return err
	}
	command := strings.Join(tail, " ")

	// The preflight asks the fleet endpoint the same question the selector asks
	// the server, so an operator sees the actual rack positions before sixty
	// phones are told to do something.
	f := &Fields{}
	f.Add("command", command)
	f.Add("selector", selectors.String())
	f.Addf("max per hub", "%d  (a hub answering all at once browns out its power domain and "+
		"manufactures the incident you are investigating)", *maxPerHub)
	preview, leased, excluded, truncated := e.previewSelector(ctx, sel)
	if truncated {
		f.Addf("matches", "AT LEAST %d device(s) — the preview hit its own limit, so the real "+
			"blast radius is larger than this", len(preview))
	} else {
		f.Addf("matches", "%d device(s)", len(preview))
	}
	if excluded > 0 {
		// Named rather than silently subtracted. A device that is offline or
		// mid-recovery is not addressable work, but an operator who expected
		// forty and is being shown three must be told which rule took the rest
		// — and which selector key gives them back.
		f.Addf("excluded", "%d device(s) the selector matched are not healthy or are under an "+
			"open quarantine, and are NOT addressed. Add --selector include-unhealthy=true or "+
			"--selector include-quarantined=true to reach them", excluded)
	}
	if len(preview) == 0 && excluded > 0 {
		return fmt.Errorf("every device this selector matched is excluded: %d not healthy or "+
			"quarantined. Nothing would run. Add --selector include-unhealthy=true or "+
			"--selector include-quarantined=true if you meant to reach them", excluded)
	}
	if sel.IncludeQuarantined {
		f.Add("quarantined", "INCLUDED — the recovery ladder may be power-cycling these devices "+
			"while your command runs on them")
	}
	if sel.IncludeLeased {
		f.Addf("of which leased", "%d — include-leased is ON, so these WILL be disturbed mid-run", leased)
	} else {
		f.Addf("of which leased", "%d — these will be SKIPPED and recorded as skipped", leased)
	}
	if len(preview) > 0 {
		f.Add("rack slots", slotList(preview))
	}

	scale := plural(len(preview), "device", "devices")
	if truncated {
		scale = "at least " + scale
	}
	headline := fmt.Sprintf("About to run one shell command across %s.", scale)
	if sel.IncludeLeased && leased > 0 {
		headline += fmt.Sprintf("\n%d of them are in the middle of somebody's job and will be disturbed.", leased)
	}
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	created, raw, err := send[bulkCreateResponse](ctx, e.client, apiPrefix+"/bulk", bulkCreateRequest{
		Selector:  sel,
		Command:   command,
		MaxPerHub: *maxPerHub,
		TimeoutMS: int(execTimeout.Milliseconds()),
		Reason:    e.reason,
	})
	if err != nil {
		return err
	}
	if created.RunID == "" {
		return e.out.RawJSON(raw)
	}
	e.warnf("run %s started: %d targets, %d skipped", created.RunID, created.Targets, created.Skipped)
	if created.Note != "" {
		e.warnf("%s", created.Note)
	}
	if !*follow {
		e.out.Text("run %s", created.RunID)
		e.out.Text("follow it with: ctl bulk-status is not a command; re-run with --follow, or read /api/v1/bulk/%s", created.RunID)
		return nil
	}
	return e.followBulk(ctx, created.RunID)
}

// followBulk polls the run and reports each target as it finishes.
//
// Progress lines go to stderr and the final table to stdout: a run being
// watched on a terminal shows both, and `ctl bulk ... -o json | jq` still
// receives nothing but the run's JSON.
func (e *env) followBulk(ctx context.Context, runID string) error {
	path := apiPrefix + "/bulk/" + url.PathEscape(runID)
	reported := map[string]bool{}
	ticker := time.NewTicker(bulkPollInterval)
	defer ticker.Stop()

	for {
		resp, raw, err := fetch[bulkGetResponse](ctx, e.client, path, nil)
		if err != nil {
			// Losing the view is not losing the run. The server owns the run
			// and its target rows; this poll failing says nothing about
			// whether the command is still executing, and nothing here
			// cancelled anything or released any lease.
			e.warnf("could not read run %s; it is still running on the server and nothing was "+
				"cancelled. Read it later at %s", runID, apiPrefix+"/bulk/"+runID)
			return err
		}
		for _, t := range resp.Targets {
			if reported[t.DeviceID] || t.State == "pending" || t.State == "running" {
				continue
			}
			reported[t.DeviceID] = true
			// Fixed widths, not a measured table: a streaming line cannot be
			// aligned against rows that have not arrived yet, and re-printing
			// the whole grid on every result would make the transcript
			// unreadable. The aligned view is the summary below.
			fmt.Fprintf(e.err, "%-14s %-10s %-8s %s\n",
				clip(rackSlotOf(t.RackSlot), 14), clip(t.FarmUID, 10),
				t.State, clip(bulkTargetDetail(t), 100))
		}
		if resp.Run.State != "running" {
			if e.format == FormatJSON {
				// As with device exec: the run's own body first, then the
				// outcome. A run where nine of sixty devices failed is not a
				// successful invocation just because it was asked for as JSON.
				if err := e.out.RawJSON(raw); err != nil {
					return err
				}
				return bulkOutcome(resp.Run)
			}
			return e.renderBulkRun(resp)
		}
		select {
		case <-ctx.Done():
			// The run keeps going on the server; only this view stops. Nothing
			// here cancels work, and nothing here releases a lease.
			e.warnf("stopped following run %s; it is still running. Read it later at /api/v1/bulk/%s",
				runID, runID)
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *env) renderBulkRun(resp bulkGetResponse) error {
	t := NewTable("RACK SLOT", "UID", "HOST", "STATE", "EXIT", "TOOK", "RESULT")
	for _, tg := range resp.Targets {
		took := "—"
		if tg.StartedAt != nil && tg.FinishedAt != nil {
			took = duration(int64(tg.FinishedAt.Sub(*tg.StartedAt).Seconds()))
		}
		t.Row(rackSlotOf(tg.RackSlot), tg.FarmUID, dash(tg.HostID), tg.State,
			dashInt(tg.ExitCode), took, bulkTargetDetail(tg))
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	r := resp.Run
	e.out.Blank()
	e.out.Text("run %s finished in state %q: %d targets — %d ok, %d error, %d skipped",
		r.ID, r.State, r.Targets, r.OK, r.Errors, r.Skipped)
	if r.Skipped > 0 {
		e.out.Text("skipped targets held a live lease and were recorded as skipped rather than " +
			"quietly dropped. Nothing about them was disturbed.")
	}
	if r.Errors > 0 {
		// An ADB failure says something about the wire and nothing about any
		// lease. Saying so here keeps a wave of transport errors from being
		// read as a wave of dead jobs.
		e.out.Text("errors above are transport or command failures. No lease was affected by any of them.")
	}
	return bulkOutcome(r)
}

// bulkOutcome fails the invocation when any target failed, in every output
// format. A target's failure is a command or transport failure on one phone
// and never an allocation event: the run's own rows record which, and nothing
// about a failed target releases that device.
func bulkOutcome(r bulkRun) error {
	if r.Errors == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d targets failed", r.Errors, r.Targets)
}

func bulkTargetDetail(t bulkTarget) string {
	if t.Error != nil && *t.Error != "" {
		return *t.Error
	}
	if t.Output != nil {
		return strings.TrimSpace(*t.Output)
	}
	return ""
}

// maxListedSlots bounds how many rack positions the blast-radius block names.
//
// A selector can match a thousand devices, and a thousand comma-separated
// slots on one line is a wall of text an operator scrolls past — so the block
// stops being read at exactly the size where reading it matters most. The
// count above it is always exact; this list is the sample that tells somebody
// which part of the building they are about to touch.
const maxListedSlots = 24

func slotList(slots []string) string {
	if len(slots) <= maxListedSlots {
		return strings.Join(slots, ", ")
	}
	return strings.Join(slots[:maxListedSlots], ", ") +
		fmt.Sprintf(", … and %d more", len(slots)-maxListedSlots)
}

// parseSelectors turns repeated k=v flags into the server's selector object.
func parseSelectors(raw []string) (bulkSelector, error) {
	var sel bulkSelector
	for _, item := range raw {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			return sel, usageErrf("--selector takes k=v, not %q", item)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if value == "" {
			// An empty value is dropped by the server's selector, and a
			// selector with every field empty matches the whole farm — which
			// is exactly what the "at least one --selector" rule below exists
			// to prevent. In practice this is `--selector host=$HOST` in a
			// script where HOST never got set, and the result would be a
			// reboot across every device in the building.
			return sel, usageErrf("--selector %s= has no value. An empty selector matches the "+
				"whole farm, so it is refused rather than expanded", key)
		}
		switch key {
		case "pool":
			sel.Pool = value
		case "host":
			sel.Host = value
		case "hub":
			sel.Hub = value
		case "health":
			sel.Health = value
		case "model":
			sel.Model = value
		case "device", "device_id":
			sel.DeviceIDs = append(sel.DeviceIDs, value)
		case "include-leased", "include_leased":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return sel, usageErrf("--selector include-leased takes true or false, not %q", value)
			}
			sel.IncludeLeased = b
		case "include-unhealthy", "include_unhealthy":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return sel, usageErrf("--selector include-unhealthy takes true or false, not %q", value)
			}
			sel.IncludeUnhealthy = b
		case "include-quarantined", "include_quarantined":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return sel, usageErrf("--selector include-quarantined takes true or false, not %q", value)
			}
			sel.IncludeQuarantined = b
		default:
			return sel, usageErrf("unknown selector key %q; use pool, host, hub, health, model, "+
				"device, include-leased, include-unhealthy or include-quarantined", key)
		}
	}
	return sel, nil
}

// bulkExcludes reports whether the server will refuse to address this device
// for the given selector, and why in operator words.
//
// It mirrors the two exclusions in the API's own selector expansion. Keeping
// the preflight in step matters more than it looks: the operator approves a
// blast radius from the number in front of them, and a preview that counted
// devices the server then declined would over-report every run against a shelf
// that had gone offline — or promise seven devices and touch three.
func bulkExcludes(sel bulkSelector, health *string, quarantineID *int64) (bool, string) {
	if quarantineID != nil && !sel.IncludeQuarantined {
		return true, "quarantined"
	}
	// Naming a health explicitly is itself the opt-in, exactly as on the
	// server; a device whose health was never observed is not known good.
	if sel.Health == "" && !sel.IncludeUnhealthy {
		if health == nil || *health != "healthy" {
			return true, "not healthy"
		}
	}
	return false, ""
}

// previewSelector resolves the selector against the fleet so the confirmation
// can name rack positions rather than a count.
//
// It is best effort and says so: the server expands the selector itself, and a
// preview that disagreed would not change what the run does. What it must never
// do is under-report, so a failure here is reported rather than swallowed.
//
// excluded counts the devices the selector matched and the server will not
// address, so an operator whose run is about to be empty is told which rule
// emptied it rather than reading "matched no addressable devices" and guessing.
func (e *env) previewSelector(ctx context.Context, sel bulkSelector) (racks []string, leased, excluded int, truncated bool) {
	q := url.Values{}
	setIf(q, "pool", sel.Pool)
	setIf(q, "host", sel.Host)
	setIf(q, "hub", sel.Hub)
	setIf(q, "health", sel.Health)
	setIf(q, "q", sel.Model)
	q.Set("limit", "1000")

	if len(sel.DeviceIDs) > 0 {
		for _, id := range sel.DeviceIDs {
			d, _, err := fetch[deviceResponse](ctx, e.client,
				apiPrefix+"/devices/"+url.PathEscape(id), nil)
			if err != nil {
				e.warnf("could not resolve device %s for the preflight (%v)", id, err)
				racks = append(racks, unknownSlot)
				continue
			}
			// Naming a device by id is not an override: the server applies the
			// same exclusions however the device was selected.
			if skip, _ := bulkExcludes(sel, d.Device.Health, d.Device.QuarantineID); skip {
				excluded++
				continue
			}
			racks = append(racks, rackSlotOf(d.Device.RackSlot))
			if d.Device.Lease != nil {
				leased++
			}
		}
		return racks, leased, excluded, false
	}

	resp, _, err := fetch[fleetResponse](ctx, e.client, apiPrefix+"/fleet", q)
	if err != nil {
		e.warnf("could not preview the selector (%v); the server will expand it and you will "+
			"be told the count before anything runs", err)
		return nil, 0, 0, false
	}
	for _, d := range resp.Devices {
		if skip, _ := bulkExcludes(sel, d.Health, d.QuarantineID); skip {
			excluded++
			continue
		}
		racks = append(racks, rackSlotOf(d.RackSlot))
		if d.Lease != nil {
			leased++
		}
	}
	// Truncation is carried out rather than swallowed. A preview that says
	// "1000 devices" when the selector matches four thousand is the one
	// failure mode this preflight must not have: an operator approves a blast
	// radius on the number in front of them.
	return racks, leased, excluded, resp.Truncated
}

// ---------------------------------------------------------------------------
// artifacts, push
// ---------------------------------------------------------------------------

func cmdArtifacts(ctx context.Context, s *session, args []string) error {
	fs := newFlags("artifacts", s.err)
	var g globals
	g.bind(fs)
	kind := fs.String("kind", "", "apk, file, script or bundle")
	pkg := fs.String("package", "", "only APKs of this package")
	limit := fs.Int("limit", 200, "maximum artifacts to return")
	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("artifacts takes no arguments; did you mean `ctl push %s`?", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	q := url.Values{}
	setIf(q, "kind", *kind)
	setIf(q, "package", *pkg)
	q.Set("limit", strconv.Itoa(*limit))

	resp, raw, err := fetch[artifactListResponse](ctx, e.client, apiPrefix+"/artifacts", q)
	if err != nil {
		return err
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	// A sha256 is 64 characters and is the artifact's identity — it is what a
	// spec references, so it is the one column in this tool that must never be
	// abbreviated. The default clip would take four characters off the end and
	// hand the operator a hash that resolves to nothing.
	t := NewTable("SHA256", "KIND", "NAME", "SIZE", "PACKAGE", "VERSION", "DEVICES", "UPLOADED BY", "CREATED")
	t.MaxCell(72)
	for _, a := range resp.Artifacts {
		t.Row(a.SHA256, a.Kind, a.Name, bytesCell(a.Size), dash(a.Package),
			dashInt64(a.VersionCode), dashInt64(a.DevicesPresent), dash(a.UploadedBy), stamp(&a.CreatedAt))
	}
	if t.Len() == 0 {
		e.out.Text("the artifact store is empty.")
		return nil
	}
	if err := e.out.Table(t); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("a spec references an artifact by its full sha256, never by name: " +
		"the hash is the identity and the name is a label.")
	e.out.Text("DEVICES counts the devices already holding these exact bytes; a push step to " +
		"one of those costs nothing.")
	return nil
}

func cmdPush(ctx context.Context, s *session, args []string) error {
	fs := newFlags("push", s.err)
	var g globals
	g.bind(fs)
	kindFlag := fs.String("kind", "", "apk, file, script or bundle (default: inferred from the extension)")
	nameFlag := fs.String("name", "", "name to file it under (default: the file's base name)")

	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl push <file> [--kind k] [--name n]")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	path := rest[0]

	kind := artifacts.Kind(strings.TrimSpace(*kindFlag))
	if kind == "" {
		kind = inferKind(path)
	}
	if !kind.Valid() {
		return usageErrf("--kind takes apk, file, script or bundle, not %q", *kindFlag)
	}
	name := strings.TrimSpace(*nameFlag)
	if name == "" {
		name = filepath.Base(path)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat artifact: %w", err)
	}
	if info.IsDir() {
		return usageErrf("%s is a directory; push takes one file", path)
	}

	// The digest is computed BEFORE anything is sent and declared in the
	// request, because the API verifies a declared digest inside the stream
	// and aborts the store on a mismatch — no blob published, no row written.
	// Hashing afterwards instead would mean discovering a corrupted transfer
	// only once the store had already filed it under a name its bytes do not
	// hash to, and an artifact's sha256 IS its identity: a spec references it
	// by hash and a job would install whatever was filed there.
	local, err := digestOf(f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind %s after hashing it: %w", path, err)
	}
	e.warnf("uploading %s (%s, kind %s, sha256 %s) to %s",
		name, bytesCell(info.Size()), kind, local, e.client.BaseURL())

	// kind, name and the declared digest travel in the query string and the
	// file is the body itself: the store writes exactly the bytes that arrive,
	// so any envelope around them would become the artifact.
	q := url.Values{}
	q.Set("kind", string(kind))
	q.Set("name", name)
	q.Set("sha256", local)

	raw, err := e.client.Upload(ctx, apiPrefix+"/artifacts", q, f, info.Size())
	if err != nil {
		return err
	}

	var res struct {
		Artifact      artifact `json:"artifact"`
		Deduplicated  bool     `json:"deduplicated"`
		ManifestError string   `json:"manifest_error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		e.warnf("the reply did not decode; these bytes hashed to %s", local)
		return e.out.RawJSON(raw)
	}
	switch {
	case res.Artifact.SHA256 == "":
		e.warnf("the store did not name the digest it filed. It verified %s against the bytes "+
			"it read, so the content is right, but this reply is not the one this build expects", local)
	case res.Artifact.SHA256 != local:
		return fmt.Errorf("the bytes that arrived are not the bytes that left: "+
			"this machine hashed %s and the store filed %s. Nothing here is safe to reference "+
			"from a spec until that is explained", local, res.Artifact.SHA256)
	}
	if res.ManifestError != "" {
		// The artifact is stored and is pushable; only the APK metadata is
		// missing. Reporting it as a failure would refuse every repackaged or
		// obfuscated build in the fleet.
		e.warnf("the APK manifest did not parse (%s); the content is stored and pushable, "+
			"but package and versionCode are unknown to the store", res.ManifestError)
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	out := &Fields{}
	out.Add("sha256", firstNonEmpty(res.Artifact.SHA256, local))
	out.Add("kind", firstNonEmpty(res.Artifact.Kind, string(kind)))
	out.Add("name", firstNonEmpty(res.Artifact.Name, name))
	out.Add("size", bytesCell(info.Size()))
	if res.Artifact.Package != nil {
		out.Addf("package", "%s%s", *res.Artifact.Package, versionSuffix(res.Artifact.VersionCode))
	}
	if res.Deduplicated {
		out.Add("note", "the content was already in the store; this upload only refreshed its metadata")
	}
	if err := e.out.Fields(out); err != nil {
		return err
	}
	e.out.Blank()
	e.out.Text("reference it from a spec by that sha256.")
	return nil
}

// digestOf hashes a reader's contents in full.
//
// push reads the file twice — once here and once to upload it — rather than
// hashing as it streams, so the digest can be stated up front. That is what
// lets the server refuse a corrupted transfer before it stores anything
// instead of storing it and being told afterwards.
func digestOf(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("hash artifact: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inferKind guesses from the extension. It is only a default: --kind wins, and
// the schema's CHECK constraint is the authority on what the four words are.
func inferKind(path string) artifacts.Kind {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".apk":
		return artifacts.KindAPK
	case ".sh", ".bash":
		return artifacts.KindScript
	case ".zip", ".obb", ".tar", ".tgz":
		return artifacts.KindBundle
	}
	return artifacts.KindFile
}

func versionSuffix(code *int64) string {
	if code == nil {
		return ""
	}
	return fmt.Sprintf(" versionCode %d", *code)
}

// bytesCell renders a byte count.
//
// The prefix index is clamped rather than trusted. The number reaching here is
// a size the server reported, and an int64 reaches into exbibytes: letting it
// walk off the end of the prefix string would turn one absurd row — a corrupt
// column, a response from something that is not this API — into a panic that
// takes the whole listing down with it.
func bytesCell(n int64) string {
	const unit = 1024
	const prefixes = "KMGTPE"
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < len(prefixes)-1; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), prefixes[exp])
}

// ---------------------------------------------------------------------------
// watch
// ---------------------------------------------------------------------------

// sseFieldLimit bounds one Server-Sent Events line. The snapshot frame carries
// the whole fleet, so this has to be generous; it exists to stop a wedged
// server from growing this process without bound, not to trim ordinary frames.
const sseFieldLimit = 32 << 20

func cmdWatch(ctx context.Context, s *session, args []string) error {
	fs := newFlags("watch", s.err)
	var g globals
	g.bind(fs)
	if rest, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usageErrf("watch takes no arguments")
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	body, err := e.client.Stream(ctx, apiPrefix+"/stream", nil)
	if err != nil {
		return err
	}
	defer body.Close()

	e.warnf("following %s%s — Ctrl-C to stop. Nothing here changes anything.",
		e.client.BaseURL(), apiPrefix+"/stream")

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), sseFieldLimit)

	var name string
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// A blank line ends a frame.
			if data.Len() > 0 {
				if err := e.emitEvent(name, data.String()); err != nil {
					return err
				}
			}
			name, data = "", strings.Builder{}
		case strings.HasPrefix(line, ":"):
			// A comment frame: the server's heartbeat, keeping an idle proxy
			// from closing a stream that is working perfectly.
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "retry:"), strings.HasPrefix(line, "id:"):
			// Reconnection hints for a browser's EventSource; ctl reconnects
			// only when a human re-runs it.
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("event stream ended: %w", err)
	}
	e.warnf("the server closed the event stream")
	return nil
}

// emitEvent renders one SSE frame.
//
// In JSON mode the output is newline-delimited objects, one per frame, because
// a stream has no end and therefore cannot be one JSON document.
func (e *env) emitEvent(name, data string) error {
	if name == "" {
		name = "message"
	}
	if e.format == FormatJSON {
		// A frame whose data is not JSON is passed through as a string rather
		// than dropped or fatal. One malformed frame — a proxy injecting a
		// notice, a server bug — must not end a stream an operator is watching
		// a fleet through, and it must not vanish either.
		payload := json.RawMessage(data)
		if !json.Valid(payload) {
			quoted, err := json.Marshal(data)
			if err != nil {
				return err
			}
			payload = quoted
		}
		line, err := json.Marshal(map[string]any{
			"event": name,
			"data":  payload,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(e.session.out, string(line))
		return err
	}

	stamp := time.Now().Format("15:04:05")
	var payload struct {
		Snapshot bool `json:"snapshot"`
		Devices  []struct {
			RackSlot string `json:"rack_slot"`
		} `json:"devices"`
		Changed     []json.RawMessage `json:"changed"`
		Removed     []json.RawMessage `json:"removed"`
		Leases      []json.RawMessage `json:"leases"`
		Jobs        []json.RawMessage `json:"jobs"`
		Attempts    []json.RawMessage `json:"attempts"`
		Quarantines []json.RawMessage `json:"quarantines"`
		Alerts      []struct {
			Kind     string `json:"kind"`
			RackSlot string `json:"rack_slot"`
			HostID   string `json:"host_id"`
			USBPath  string `json:"usb_path"`
			Message  string `json:"message"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		fmt.Fprintf(e.session.out, "%s  %-9s %s\n", stamp, name, clip(data, 160))
		return nil
	}

	// Alerts are the frames worth interrupting somebody for, so they are
	// printed in full rather than counted.
	for _, a := range payload.Alerts {
		// A hub-correlation alert is about a hub and carries no rack slot,
		// because it is not about one phone. Naming the hub is the same
		// answer to "where do I go" that a rack slot is for a device, and it
		// is a great deal more useful than a dash.
		where := firstNonEmpty(a.RackSlot, strings.TrimSpace(a.HostID+" "+a.USBPath), "-")
		fmt.Fprintf(e.session.out, "%s  ALERT     %s [%s] %s\n", stamp, a.Kind, where, a.Message)
	}
	if len(payload.Alerts) > 0 {
		return nil
	}

	kind := "delta"
	if payload.Snapshot {
		kind = "snapshot"
	}
	counts := []string{}
	addCount := func(label string, n int) {
		if n > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", label, n))
		}
	}
	addCount("devices", len(payload.Devices))
	addCount("changed", len(payload.Changed))
	addCount("removed", len(payload.Removed))
	addCount("leases", len(payload.Leases))
	addCount("jobs", len(payload.Jobs))
	addCount("attempts", len(payload.Attempts))
	addCount("quarantines", len(payload.Quarantines))
	if len(counts) == 0 {
		// A frame this build does not recognise still gets a line. Rendering
		// nothing would make a stream that is working look like a stream that
		// has stopped, and would hide exactly the event a newer server added.
		fmt.Fprintf(e.session.out, "%s  %-9s %-8s %s\n", stamp, name, kind, clip(data, 120))
		return nil
	}
	fmt.Fprintf(e.session.out, "%s  %-9s %-8s %s\n", stamp, name, kind, strings.Join(counts, ", "))
	return nil
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// setIf adds a query parameter only when it has a value, so an unset filter is
// absent from the URL rather than present and empty — the API treats those the
// same, but a request log should show what was actually asked.
func setIf(q url.Values, key, value string) {
	if v := strings.TrimSpace(value); v != "" {
		q.Set(key, v)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isEmptyJSON(b json.RawMessage) bool {
	s := strings.TrimSpace(string(b))
	return s == "" || s == "{}" || s == "null"
}
