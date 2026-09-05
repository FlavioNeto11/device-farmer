// Package enroll turns a phone somebody just plugged into a rack into a
// schedulable member of the fleet, with nobody editing a database.
//
// # Observe, resolve, brand
//
// Every cycle asks each host's ADB server what is attached to it, and for each
// physical position does three things in this order:
//
//	OBSERVE   read the brand and the properties off the device and write one
//	          row into farm.identity_observations recording exactly that,
//	          BEFORE any conclusion is drawn about which device it is
//	RESOLVE   hand that evidence to farm.resolve_device, which decides — by
//	          branded uid, then hardware fingerprint, then serial together
//	          with the slot, then adoption — and returns the device it means
//	BRAND     write the farm uid onto a device that was just adopted, or onto
//	          a known device that came back from a factory reset carrying no
//	          brand, so the strongest signal exists next time
//
// The order is the point. The sighting is recorded before the conclusion, so a
// wrong adoption can be explained afterwards from the record instead of
// reconstructed from guesses. The decision itself lives in one place —
// farm.resolve_device — and is not re-implemented here; a second copy of the
// identity ladder in Go would drift from the one in SQL, and the two would
// disagree about which phone this is.
//
// # An ADB serial is evidence, not an identifier
//
// Duplicate OEM serials are real: STF's own README documents a device shipping
// with serial "0123456789ABCDEF", and two of those in one rack are
// indistinguishable by serial. So every address in this package is a devpath —
// a position in the USB tree — and the serial is recorded as an observation
// and used only as one component of a hardware fingerprint, where two clones
// colliding is the honest answer rather than a silent mix-up.
//
// # What this package will not do
//
// It does not import internal/lease and it never ends, shortens or examines a
// lease. Enrollment is about which device this is; a lease is ended by the job,
// by a deadline a user wrote down, or by a human, and by nothing else — the
// failure mode of DeviceFarmer/STF issue #663, where a transport error freed a
// device in the middle of a multi-hour run, has no path through this code.
//
// It also refuses two things that would look like progress:
//
//   - It never overwrites a different farm uid on a device (see [Brander]).
//     That would merge two devices' histories into one row.
//   - It never clears another device row's slot pointer to make room for a
//     newcomer. That row may belong to a phone that is holding a live lease
//     right now, and enrollment is not allowed to have an opinion about that.
//     The collision is recorded as a conflict for a human instead.
package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// Defaults.
const (
	// DefaultComponent names this loop in logs and in farm.events.actor.
	DefaultComponent = "enroll"

	// DefaultInterval is how often the fleet is re-enumerated. Enrollment is
	// how long a human waits between plugging a phone in and seeing it in the
	// pool, so it is frequent; it is also mostly a no-op, because a fleet
	// whose devices are all branded resolves every one of them with a single
	// index probe.
	DefaultInterval = 30 * time.Second

	// DefaultCallTimeout bounds one database statement.
	DefaultCallTimeout = 10 * time.Second

	// DefaultProbeTimeout bounds one device's answer. A phone that is booting,
	// thermally throttled, or busy running somebody's job can take seconds to
	// return a getprop, and a device that exceeds this is recorded as
	// unreadable rather than guessed at.
	DefaultProbeTimeout = 15 * time.Second

	// DefaultConcurrency is how many devices on one host are probed at once.
	// A host may carry forty phones and one wedged phone must not hold up the
	// other thirty-nine for a whole timeout.
	DefaultConcurrency = 4

	// DefaultPoolID is the farm.pools row newly adopted devices join.
	DefaultPoolID = "default"

	// maxProbeOutput caps what one identity read may return. A probe asks for
	// a dozen short lines and a thirty-five byte uid; adbwire's 8 MiB default
	// is a sensible ceiling for a job's log and an absurd one for this. A
	// device with something large sitting at the brand path would otherwise
	// hand this loop megabytes to parse and store, once per device per cycle.
	maxProbeOutput = 64 << 10
)

// Resolution values, mirroring the CHECK constraint on
// farm.identity_observations.resolution. Restated here so a typo is a compile
// error on this side and not a constraint violation at 3am.
const (
	ResolutionPending       = "pending"
	ResolutionBrandedUID    = "branded_uid"
	ResolutionHWFingerprint = "hw_fingerprint"
	ResolutionSerialAndSlot = "serial_and_slot"
	ResolutionAdoptedNew    = "adopted_new"
	ResolutionAmbiguous     = "ambiguous"
	ResolutionConflict      = "conflict"
	ResolutionUnreadable    = "unreadable"
)

// SQLSTATEs this loop reacts to.
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
	sqlStateNoDataFound         = "P0002"
	sqlStateUndefinedFunction   = "42883"
)

// Host is the slice of one ADB server this loop uses: what is attached, and
// what one of those devices says about itself.
type Host interface {
	Shell
	Devices(ctx context.Context) (adbwire.Snapshot, error)
}

// Config is the enrollment loop's wiring. Pool is required.
type Config struct {
	// Pool writes farm.identity_observations and farm.events, and calls
	// farm.resolve_device. It is never used to read or write farm.leases.
	Pool *pgxpool.Pool

	// Component names this loop in logs and in farm.events.actor.
	Component string

	// HostID limits this process to one host, as in a per-node DaemonSet.
	// Empty means every host in farm.hosts that is not administratively
	// disabled, which is how a single central enroller runs.
	HostID string

	// ADBEndpoint overrides farm.hosts.adb_endpoint for every host this
	// process enrolls. A node-local deployment reaches its ADB server at
	// 127.0.0.1:5037 whatever address the rest of the fleet uses.
	ADBEndpoint string

	// PoolID is the farm.pools row a newly adopted device joins. It must
	// exist: farm.devices.pool_id is NOT NULL with ON DELETE RESTRICT, so an
	// unknown pool makes every adoption fail loudly rather than putting
	// devices somewhere nobody asked for.
	PoolID string

	Interval     time.Duration
	CallTimeout  time.Duration
	ProbeTimeout time.Duration
	Concurrency  int

	// SkipBranding stops this loop writing anything to any device. Resolution
	// still runs, so devices are still adopted — they simply stay unbranded
	// and keep being recognised by the weaker rungs of the ladder. Useful when
	// an operator wants enrollment to be strictly read-only against the
	// phones; not a good steady state.
	SkipBranding bool

	// Connect opens a client for one ADB server. Defaults to adbwire.
	Connect func(endpoint string) (Host, error)

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Component == "" {
		c.Component = DefaultComponent
	}
	if c.PoolID == "" {
		c.PoolID = DefaultPoolID
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = DefaultProbeTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Connect == nil {
		probeTimeout := c.ProbeTimeout
		log := c.Logger
		c.Connect = func(endpoint string) (Host, error) {
			return adbwire.New(endpoint,
				adbwire.WithCallTimeout(probeTimeout),
				adbwire.WithMaxOutput(maxProbeOutput),
				adbwire.WithLogger(log)), nil
		}
	}
}

// Enroller is the loop.
type Enroller struct {
	cfg Config
	log *slog.Logger

	// last is the previous cycle's summary line, so a quiet fleet logs at
	// debug and a fleet that changed logs at info.
	last string
}

// New validates cfg and returns an Enroller.
func New(cfg Config) (*Enroller, error) {
	if cfg.Pool == nil {
		return nil, errors.New("enroll: Config.Pool is required")
	}
	cfg.applyDefaults()
	return &Enroller{cfg: cfg, log: cfg.Logger.With("component", cfg.Component)}, nil
}

// Run enrolls continuously until ctx is cancelled, and returns nil when it is.
//
// Stopping enrollment freezes the fleet's membership at what it already knows.
// Nothing is un-adopted, no device is marked anything, and no lease is touched:
// a phone plugged in while this loop is down is simply picked up on the next
// cycle after it comes back.
func (e *Enroller) Run(ctx context.Context) error {
	e.log.Info("enrollment loop starting",
		"host", hostLabel(e.cfg.HostID), "interval", e.cfg.Interval,
		"pool", e.cfg.PoolID, "branding", !e.cfg.SkipBranding)

	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()

	for {
		sum, err := e.EnrollOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			cycleErrors.Inc()
			e.log.Error("enrollment cycle failed", "err", err)
		case err == nil:
			e.report(sum)
		}

		select {
		case <-ctx.Done():
			e.log.Info("enrollment loop stopping; fleet membership stands as it is")
			return nil
		case <-ticker.C:
		}
	}
}

// EnrollOnce runs exactly one pass over every host in scope and reports what it
// found, by resolution.
//
// It returns an error only when the pass could not be attempted at all — the
// host list was unreadable. A host whose ADB server is unreachable, or a single
// device that would not answer, is counted and logged, not returned: one bad
// phone must not stop the other thirty-nine from being enrolled.
func (e *Enroller) EnrollOnce(ctx context.Context) (Summary, error) {
	cyclesTotal.Inc()

	hosts, err := e.hosts(ctx)
	if err != nil {
		// An initialised map even on the error path: a caller that reports
		// this summary should never have to guard against a nil one.
		return newSummary(), err
	}

	sum := newSummary()
	if len(hosts) == 0 {
		// Enrollment cannot invent a host: farm.identity_observations.host_id
		// is a foreign key, and topology registration is a different job.
		e.log.Warn("no host to enroll from; topology has no matching row in farm.hosts",
			"host", hostLabel(e.cfg.HostID))
		return sum, nil
	}
	sum.Hosts = len(hosts)

	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		e.enrollHost(ctx, h, &sum)
	}
	return sum, nil
}

// report logs a cycle. A fleet where nothing changed says so at debug; a cycle
// that adopted, conflicted or failed is always at info or above, because those
// are the lines an operator is waiting for.
func (e *Enroller) report(sum Summary) {
	line := sum.String()
	notable := sum.ByResolution[ResolutionAdoptedNew] > 0 ||
		sum.ByResolution[ResolutionConflict] > 0 ||
		sum.ByResolution[ResolutionAmbiguous] > 0 ||
		sum.BrandConflicts > 0 || sum.Errors > 0
	switch {
	case notable || line != e.last:
		e.log.Info("enrollment cycle", "summary", line)
	default:
		e.log.Debug("enrollment cycle", "summary", line)
	}
	e.last = line
}

// ---------------------------------------------------------------------------
// One host
// ---------------------------------------------------------------------------

type hostRow struct {
	ID       string
	Endpoint string
	// Epoch is farm.hosts.host_epoch, which increments on every adb server
	// restart. A transport_id is a small integer the server reuses across a
	// restart, so it is recorded together with the epoch it was minted in or
	// not at all.
	Epoch int64
}

func (e *Enroller) hosts(ctx context.Context) ([]hostRow, error) {
	// A draining host is included: it is still full of phones, a human may
	// still be re-cabling it, and knowing what is physically attached to it
	// is exactly what draining is for. Only 'disabled' drops out.
	const q = `
SELECT h.id, h.adb_endpoint, h.host_epoch
  FROM farm.hosts h
 WHERE h.admin_state <> 'disabled'
   AND ($1::text = '' OR h.id = $1::text)
 ORDER BY h.id`

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	rows, err := e.cfg.Pool.Query(cctx, q, e.cfg.HostID)
	if err != nil {
		return nil, fmt.Errorf("enroll: read hosts: %w", err)
	}
	defer rows.Close()

	var out []hostRow
	for rows.Next() {
		var h hostRow
		if err := rows.Scan(&h.ID, &h.Endpoint, &h.Epoch); err != nil {
			return nil, fmt.Errorf("enroll: read hosts scan: %w", err)
		}
		if e.cfg.ADBEndpoint != "" {
			h.Endpoint = e.cfg.ADBEndpoint
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enroll: read hosts: %w", err)
	}
	return out, nil
}

// enrollHost enumerates one host and folds every position it reports into the
// record.
func (e *Enroller) enrollHost(ctx context.Context, h hostRow, sum *Summary) {
	log := e.log.With("host", h.ID)

	client, err := e.cfg.Connect(h.Endpoint)
	if err != nil {
		sum.Errors++
		hostErrors.WithLabelValues(h.ID).Inc()
		log.Error("could not open a client for this host's ADB server",
			"endpoint", h.Endpoint, "err", err)
		return
	}

	lctx, cancel := context.WithTimeout(ctx, e.cfg.ProbeTimeout)
	snap, err := client.Devices(lctx)
	cancel()
	if err != nil {
		// An unreachable ADB server tells us nothing about the phones behind
		// it. Nothing is recorded, nothing is concluded, and the fleet stands
		// as it was — silence is not evidence.
		if ctx.Err() == nil {
			sum.Errors++
			hostErrors.WithLabelValues(h.ID).Inc()
			log.Warn("could not list this host's devices, so nothing about its phones was recorded "+
				"or concluded; check that an ADB server is listening at this endpoint",
				"endpoint", h.Endpoint, "err", err)
		}
		return
	}
	attachedGauge.WithLabelValues(h.ID).Set(float64(len(snap.Devices)))

	if dupes := snap.AmbiguousSerials(); len(dupes) > 0 {
		// Not an error and not a reason to skip anything: it is the collision
		// this whole design assumes. Say it once per cycle so an operator
		// reading the identity_observations rows knows why two devices look
		// alike.
		log.Info("this host reports duplicate serials; identity is resolved by position and brand, not by serial",
			"serials", dupes)
	}

	slots, err := e.slots(ctx, h.ID)
	if err != nil {
		sum.Errors++
		hostErrors.WithLabelValues(h.ID).Inc()
		log.Error("could not read this host's registered slots, so no device on it can be placed "+
			"this cycle; the sightings are skipped rather than recorded against unknown positions",
			"err", err)
		return
	}

	// Positions only. A device the server lists without a devpath is an
	// emulator or a network-attached target: it has no place in a rack, it
	// cannot be power-cycled, and farm.identity_observations.usb_path could
	// only be filled with a lie.
	addressable := make([]adbwire.Device, 0, len(snap.Devices))
	unaddressable := 0
	for _, d := range snap.Devices {
		if d.Devpath == "" {
			unaddressable++
			sum.Unaddressable++
			log.Info("ignoring a device with no USB position; it cannot be placed in a rack",
				"serial", truncate(d.Serial, 64), "state", string(d.State))
			continue
		}
		addressable = append(addressable, d)
	}
	unaddressableGauge.WithLabelValues(h.ID).Set(float64(unaddressable))

	ids := e.probeAll(ctx, client, addressable)
	brander := NewBrander(client, e.cfg.ProbeTimeout, log)

	unslotted := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		_, hasSlot := slots[id.USBPath]
		if !hasSlot {
			unslotted++
		}
		e.record(ctx, h, id, hasSlot, brander, sum, log)
	}
	unslottedGauge.WithLabelValues(h.ID).Set(float64(unslotted))
}

// slots reads the positions topology discovery has registered on this host.
//
// It is read once per cycle rather than relied upon inside
// farm.resolve_device, because that function RAISES for an unknown position
// and an exception would take down the transaction that was recording the
// sighting. A position with no slot is a normal state of a growing rack, not
// an error, and it must still be recorded.
func (e *Enroller) slots(ctx context.Context, host string) (map[string]struct{}, error) {
	const q = `SELECT s.usb_path FROM farm.slots s WHERE s.host_id = $1::text`

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	rows, err := e.cfg.Pool.Query(cctx, q, host)
	if err != nil {
		return nil, fmt.Errorf("enroll: read slots of %s: %w", host, err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("enroll: read slots scan: %w", err)
		}
		out[p] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enroll: read slots of %s: %w", host, err)
	}
	return out, nil
}

// probeAll gathers identity from every position, a few at a time.
//
// The bound matters more than the parallelism: a phone that is booting can sit
// on a probe for the whole timeout, and a rack of forty with one such phone
// must not take forty timeouts to enumerate. Results keep the listing's order
// so a cycle's log lines are stable.
func (e *Enroller) probeAll(ctx context.Context, sh Shell, devs []adbwire.Device) []Identity {
	out := make([]Identity, len(devs))
	sem := make(chan struct{}, e.cfg.Concurrency)
	var wg sync.WaitGroup

	for i, d := range devs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = Identity{
					Devpath: d.Devpath, USBPath: USBPath(d.Devpath), Serial: d.Serial,
					State: d.State, RawState: d.RawState, TransportID: d.TransportID,
					Props: map[string]string{}, Unreadable: "cancelled",
				}
				return
			}
			defer func() { <-sem }()
			out[i] = Probe(ctx, sh, d, e.cfg.ProbeTimeout)
			probeDuration.Observe(out[i].ProbeDuration.Seconds())
		}()
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------------------
// One device
// ---------------------------------------------------------------------------

// record writes the sighting, resolves it, and brands what needs branding.
//
// The observation row is written FIRST and in its own statement, so that a
// failure in resolution — an unknown position, a slot whose previous occupant
// is still recorded in it, a database that rejects the call — still leaves
// behind a durable record of what was physically seen. That record is the only
// thing that can explain, weeks later, why a device was adopted or why it was
// not.
func (e *Enroller) record(ctx context.Context, h hostRow, id Identity, hasSlot bool,
	brander *Brander, sum *Summary, log *slog.Logger) {

	log = log.With("devpath", id.Devpath)

	// A probe this process abandoned on its way out says nothing about the
	// phone, and there is no live connection left to write it with anyway.
	if id.Unreadable == "cancelled" {
		return
	}

	detail := map[string]any{
		"adb_state":     string(id.State),
		"adb_raw_state": id.RawState,
		"probe_ms":      id.ProbeDuration.Milliseconds(),
		"endpoint":      h.Endpoint,
		"branded":       id.FarmUID != "",
	}
	if len(id.Props) > 0 {
		detail["props"] = id.Props
	}
	if len(id.FingerprintKeys) > 0 {
		detail["fingerprint_keys"] = id.FingerprintKeys
	}
	if id.Unreadable != "" {
		// Recorded even when the row ends up 'pending' for a different
		// reason, so an operator never has to guess whether an unregistered
		// position also held a phone that would not answer.
		detail["unreadable"] = id.Unreadable
	}
	if id.Err != nil {
		detail["probe_error"] = truncate(id.Err.Error(), 300)
	}
	if id.MalformedUID != "" {
		// Something that is not this system wrote to the brand path. Recorded
		// verbatim (bounded), never acted on.
		detail["malformed_uid"] = truncate(id.MalformedUID, 128)
		log.Error("the brand file on this device does not hold a farm uid, so it is ignored as "+
			"identity and this device will not be branded while it is there; remove that file on "+
			"the device to let enrollment claim it",
			"content", truncate(id.MalformedUID, 64), "path", BrandPath)
	}

	// The resolution the row is born with. It stays this way unless
	// farm.resolve_device answers.
	resolution := ResolutionPending
	resolve := true

	switch ok, why := id.Resolvable(); {
	case !hasSlot:
		// Not dropped, and not adopted either: adoption needs a slot, and a
		// slot is topology's to register. The sighting is the evidence a human
		// needs in order to register it.
		resolve = false
		detail["reason"] = "no_slot_registered"
		log.Warn("a device is attached at a position topology discovery has not registered; recording the sighting and adopting nothing",
			"host", h.ID, "usb_path", id.USBPath, "serial", truncate(id.Serial, 64),
			"state", string(id.State))
	case !ok:
		// An unauthorized phone, a phone that would not answer in time, or a
		// phone that answered with nothing that could tell it apart from
		// another unit. All three are sightings; none of them is an identity.
		resolve = false
		resolution = ResolutionUnreadable
		detail["reason"] = why
		log.Info("a device could not be identified; recording the sighting and adopting nothing",
			"reason", why, "serial", truncate(id.Serial, 64), "state", string(id.State))
	}

	obsID, err := e.observe(ctx, h, id, resolution, detail)
	if err != nil {
		sum.Errors++
		observeErrors.Inc()
		log.Error("could not record an identity observation", "err", err)
		return
	}

	if !resolve {
		sum.count(resolution)
		resolutionsTotal.WithLabelValues(resolution).Inc()
		return
	}

	devID, resolved, slotID, err := e.resolve(ctx, h, id)
	if err != nil {
		resolution, detail = e.classifyResolveError(err, h, id, log)
		sum.Errors++
		resolveErrors.Inc()
		e.setResolution(ctx, obsID, resolution, "", detail, log)
		sum.count(resolution)
		resolutionsTotal.WithLabelValues(resolution).Inc()
		return
	}

	resolution = resolved
	e.setResolution(ctx, obsID, resolution, devID,
		map[string]any{"resolved": true, "slot_id": slotID}, log)
	sum.count(resolution)
	resolutionsTotal.WithLabelValues(resolution).Inc()

	if devID == "" {
		// farm.resolve_device answered without naming a device. Everything
		// downstream — abis, branding, the event — needs one, so stop here
		// rather than acting on a device that was not identified.
		log.Warn("resolution named no device", "resolution", resolution)
		return
	}

	// farm.resolve_device carries every property it stores except this one.
	// Leaving it empty would make a real adopted device look, to any selector
	// that filters on ABI, like a device that runs no native code at all.
	e.writeABIs(ctx, devID, id.ABIs, log)

	// A device joining the fleet is a timeline event whether or not the
	// evidence it joined on was clean.
	//
	// farm.resolve_device returns 'ambiguous' ONLY from its adoption branch
	// (see migrations/00011_resolve_ambiguous.sql): every rung that can detect
	// a collision — a fingerprint or a serial claimed by more than one device —
	// names no device by definition, so the row is created either way and the
	// word describes the evidence, not a half-finished write. Gating this on
	// 'adopted_new' alone meant a tray of clone-serial handsets — duplicate OEM
	// serials being real, which is why farm.slots addresses by devpath — was
	// genuinely adopted, branded and put to work while /api/v1/events said no
	// device had joined and farm_enroll_adopted_total under-counted the fleet.
	if resolution == ResolutionAdoptedNew || resolution == ResolutionAmbiguous {
		contested := resolution == ResolutionAmbiguous
		adoptedTotal.Inc()
		if contested {
			log.Warn("ADOPTED a device on CONTESTED evidence: another device in this fleet claims "+
				"the same serial or hardware fingerprint, so this row was created rather than "+
				"matched; a human should confirm the two are really different handsets",
				"device", devID, "usb_path", id.USBPath, "serial", truncate(id.Serial, 64),
				"model", truncate(id.Model, 64), "fingerprint_keys", id.FingerprintKeys)
		} else {
			log.Info("ADOPTED a new device: nothing the fleet already knew matched it",
				"device", devID, "usb_path", id.USBPath, "serial", truncate(id.Serial, 64),
				"model", truncate(id.Model, 64), "fingerprint_keys", id.FingerprintKeys)
		}
		e.event(ctx, "device_adopted", devID, slotID, map[string]any{
			"host": h.ID, "usb_path": id.USBPath,
			"serial": truncate(id.Serial, 64), "model": truncate(id.Model, 64),
			"manufacturer":    truncate(id.Manufacturer, 64),
			"had_fingerprint": len(id.HWFingerprint) > 0,
			// The observation row carries the full evidence; this flag is what
			// makes the collision visible on the timeline itself.
			"ambiguous": contested,
		}, log)
	}

	// Brand a device that carries no brand: the one just adopted, and equally
	// the known device that came back from a factory reset with /data wiped.
	// Restoring the brand is what stops that phone depending on the weaker
	// rungs of the ladder for the rest of its life.
	if id.FarmUID == "" && !e.cfg.SkipBranding {
		e.brandDevice(ctx, brander, devID, id.Devpath, slotID, sum, obsID, log)
	}
}

// observe writes the sighting. Every column is what was seen; nothing here is
// a conclusion, and observed_at is the server's clock, never this process's.
func (e *Enroller) observe(ctx context.Context, h hostRow, id Identity,
	resolution string, detail map[string]any) (int64, error) {

	const q = `
INSERT INTO farm.identity_observations (
  host_id, host_epoch, usb_path, adb_devpath, transport_id, adb_serial,
  farm_uid, hw_fingerprint, manufacturer, model, product, device_codename,
  android_release, sdk_int, abis, build_fingerprint, resolution, detail)
VALUES ($1::text, $2::bigint, $3::text, $4::text, $5::bigint, nullif($6::text,''),
        nullif($7::text,''), $8::bytea, nullif($9::text,''), nullif($10::text,''),
        nullif($11::text,''), nullif($12::text,''), nullif($13::text,''), $14::int,
        $15::text[], nullif($16::text,''), $17::text, $18::jsonb)
RETURNING id`

	var transport *int64
	if id.TransportID > 0 {
		t := id.TransportID
		transport = &t
	}
	var sdk *int32
	if id.SDKInt > 0 {
		v := int32(id.SDKInt)
		sdk = &v
	}

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	var obsID int64
	err := e.cfg.Pool.QueryRow(cctx, q,
		h.ID, h.Epoch, id.USBPath, id.Devpath, transport, id.Serial,
		id.FarmUID, id.HWFingerprint, id.Manufacturer, id.Model, id.Product, id.Codename,
		id.AndroidRelease, sdk, id.ABIs, id.BuildFingerprint, resolution, jsonDetail(detail),
	).Scan(&obsID)
	if err != nil {
		return 0, fmt.Errorf("enroll: record observation at %s/%s: %w", h.ID, id.USBPath, err)
	}
	return obsID, nil
}

// resolve asks the database which device this is.
//
// The ladder — branded uid, hardware fingerprint, serial together with the
// slot, adopt — lives entirely inside farm.resolve_device. This function only
// carries evidence in and an answer out.
func (e *Enroller) resolve(ctx context.Context, h hostRow, id Identity) (
	deviceID, resolution string, slotID int64, err error) {

	const q = `
SELECT r.device_id::text, r.resolution, r.slot_id
  FROM farm.resolve_device($1::text, $2::text, nullif($3::text,''), $4::bytea,
                           nullif($5::text,''), $6::text, $7::jsonb) r`

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	var dev *string
	var res *string
	var slot *int64
	if err := e.cfg.Pool.QueryRow(cctx, q,
		h.ID, id.USBPath, id.FarmUID, id.HWFingerprint, id.Serial,
		e.cfg.PoolID, jsonDetail(id.resolveProps()),
	).Scan(&dev, &res, &slot); err != nil {
		return "", "", 0, fmt.Errorf("enroll: resolve %s/%s: %w", h.ID, id.USBPath, err)
	}

	if dev != nil {
		deviceID = *dev
	}
	if res != nil {
		resolution = *res
	}
	if slot != nil {
		slotID = *slot
	}
	if resolution == "" {
		resolution = ResolutionPending
	}
	return deviceID, resolution, slotID, nil
}

// classifyResolveError turns a failed resolution into the resolution value the
// observation should carry, plus the detail explaining it.
func (e *Enroller) classifyResolveError(err error, h hostRow, id Identity, log *slog.Logger) (
	string, map[string]any) {

	detail := map[string]any{"resolve_error": truncate(err.Error(), 300)}

	switch {
	case isSQLState(err, sqlStateNoDataFound):
		// The slot existed when this cycle read topology and does not now.
		// Same outcome as never having had one: recorded, not adopted.
		detail["reason"] = "no_slot_registered"
		log.Warn("the slot for this position disappeared while it was being resolved",
			"host", h.ID, "usb_path", id.USBPath)
		return ResolutionPending, detail

	case isSQLState(err, sqlStateUniqueViolation):
		// A different phone is now in a slot whose previous occupant still
		// points at it. Adoption would need that pointer cleared, and this
		// loop will not clear it: the departing device may be holding a live
		// lease this second, and enrollment has no standing to decide that.
		detail["reason"] = "slot_occupied_by_another_device"
		log.Error("a device was found in a slot that another device row still occupies; "+
			"enrollment will not move that row, a human must re-slot or retire it",
			"host", h.ID, "usb_path", id.USBPath, "serial", truncate(id.Serial, 64),
			"err", err)
		return ResolutionConflict, detail

	case isSQLState(err, sqlStateForeignKeyViolation):
		// Adoption inserts a row that must reference a pool and a host. The
		// pool is the one an operator chooses and therefore the one they can
		// get wrong: farm.devices.pool_id is NOT NULL with ON DELETE RESTRICT,
		// so a pool that does not exist fails every adoption on the farm and
		// nothing else explains why. Verified against a live server: an
		// unknown pool comes back as 23503 on devices_pool_id_fkey.
		detail["reason"] = "referenced_row_missing"
		detail["constraint"] = sqlConstraint(err)
		log.Error("a device could not be adopted because a row it must reference does not exist; "+
			"if the constraint below names the pool, create it (INSERT INTO farm.pools) or point "+
			"this enroller at a pool that exists",
			"pool", e.cfg.PoolID, "constraint", sqlConstraint(err),
			"host", h.ID, "usb_path", id.USBPath, "err", err)
		return ResolutionPending, detail

	case isSQLState(err, sqlStateUndefinedFunction):
		detail["reason"] = "resolve_device_unavailable"
		log.Error("farm.resolve_device could not execute on this server: a function it calls "+
			"does not exist here; check the PostgreSQL version against migrations/00004_operate.sql",
			"host", h.ID, "usb_path", id.USBPath, "err", err)
		return ResolutionPending, detail

	default:
		log.Error("could not resolve a device; the sighting stands, no conclusion was drawn",
			"host", h.ID, "usb_path", id.USBPath, "err", err)
		return ResolutionPending, detail
	}
}

// setResolution records what resolution concluded, on the row that already
// holds what was seen. The detail is merged, never replaced: the observation's
// own evidence must survive the conclusion.
func (e *Enroller) setResolution(ctx context.Context, obsID int64, resolution, deviceID string,
	detail map[string]any, log *slog.Logger) {

	const q = `
UPDATE farm.identity_observations
   SET resolution = $2::text,
       device_id  = nullif($3::text,'')::uuid,
       detail     = detail || $4::jsonb
 WHERE id = $1::bigint`

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	if _, err := e.cfg.Pool.Exec(cctx, q, obsID, resolution, deviceID, jsonDetail(detail)); err != nil {
		if ctx.Err() == nil {
			observeErrors.Inc()
			log.Warn("could not attach the resolution to its observation",
				"observation", obsID, "resolution", resolution, "err", err)
		}
	}
}

// writeABIs stores the device's ABI list.
//
// farm.resolve_device writes every property it is given except this one, and
// farm.devices.abis defaults to an empty array — which any future selector
// would read as "runs nothing". The write is skipped when the value has not
// changed, so a steady fleet does no work here.
func (e *Enroller) writeABIs(ctx context.Context, deviceID string, abis []string, log *slog.Logger) {
	if len(abis) == 0 {
		return
	}
	const q = `
UPDATE farm.devices d
   SET abis = $2::text[], updated_at = now()
 WHERE d.id = $1::uuid AND d.abis IS DISTINCT FROM $2::text[]`

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	if _, err := e.cfg.Pool.Exec(cctx, q, deviceID, abis); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not store the device's ABI list", "device", deviceID, "err", err)
		}
	}
}

// deviceUID reads the farm uid of a device row. It is the value that gets
// branded onto the phone, and it is read back from the database rather than
// generated here: farm.resolve_device mints it, and a uid this process invented
// would name a row that does not exist.
func (e *Enroller) deviceUID(ctx context.Context, deviceID string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	var uid string
	err := e.cfg.Pool.QueryRow(cctx,
		`SELECT d.farm_uid FROM farm.devices d WHERE d.id = $1::uuid`, deviceID).Scan(&uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("enroll: device %s has no row to take a uid from", deviceID)
		}
		return "", fmt.Errorf("enroll: read farm_uid of %s: %w", deviceID, err)
	}
	return uid, nil
}

// brandDevice writes the fleet's name for this phone onto the phone.
//
// Failure here is not failure of enrollment: the device is already resolved and
// schedulable. It simply keeps being recognised by the weaker rungs of the
// ladder until a later cycle manages to brand it, which is why this retries
// implicitly — an unbranded device is observed as unbranded again next time.
func (e *Enroller) brandDevice(ctx context.Context, brander *Brander, deviceID, devpath string,
	slotID int64, sum *Summary, obsID int64, log *slog.Logger) {

	uid, err := e.deviceUID(ctx, deviceID)
	if err != nil {
		sum.Errors++
		brandsTotal.WithLabelValues(string(BrandFailed)).Inc()
		log.Warn("could not read the uid to brand with", "device", deviceID, "err", err)
		return
	}

	outcome, err := brander.Brand(ctx, devpath, uid)
	brandsTotal.WithLabelValues(string(outcome)).Inc()

	switch outcome {
	case BrandWritten:
		sum.Branded++
		log.Info("branded a device", "device", deviceID, "uid", uid)
		e.setResolutionDetail(ctx, obsID, map[string]any{"brand": string(outcome), "uid": uid}, log)

	case BrandAlready:
		// The device carried the brand all along and the probe read it as
		// empty — a slow or partial read, not an identity problem.
		e.setResolutionDetail(ctx, obsID, map[string]any{"brand": string(outcome)}, log)

	case BrandConflict:
		sum.BrandConflicts++
		var ce *ConflictError
		have := ""
		if errors.As(err, &ce) {
			have = ce.Have
		}
		// The loudest thing this package says. Two rows in farm.devices both
		// believe they are this phone, and overwriting the brand would fuse
		// their histories beyond recovery.
		log.Error("REFUSING to rebrand: this device already carries a different farm uid",
			"device", deviceID, "on_device_uid", have, "database_uid", uid,
			"devpath", devpath)
		e.setResolutionDetail(ctx, obsID,
			map[string]any{"brand": string(outcome), "on_device_uid": have, "database_uid": uid}, log)
		e.event(ctx, "identity_conflict", deviceID, slotID, map[string]any{
			"reason":        "device carries a different farm uid than the row it resolved to",
			"on_device_uid": have, "database_uid": uid, "devpath": devpath,
		}, log)

	default:
		sum.Errors++
		log.Warn("could not brand a device; it stays resolvable by fingerprint and slot",
			"device", deviceID, "devpath", devpath, "err", err)
		e.setResolutionDetail(ctx, obsID, map[string]any{
			"brand": string(BrandFailed), "brand_error": truncate(fmt.Sprint(err), 300)}, log)
	}
}

// setResolutionDetail merges detail into an observation without touching its
// resolution or device.
func (e *Enroller) setResolutionDetail(ctx context.Context, obsID int64,
	detail map[string]any, log *slog.Logger) {

	const q = `UPDATE farm.identity_observations SET detail = detail || $2::jsonb WHERE id = $1::bigint`

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	if _, err := e.cfg.Pool.Exec(cctx, q, obsID, jsonDetail(detail)); err != nil {
		if ctx.Err() == nil {
			log.Debug("could not extend an observation's detail", "observation", obsID, "err", err)
		}
	}
}

// event writes the operator-facing timeline row.
//
// Only the notable outcomes get one: an adoption and a conflict. A cycle that
// recognises forty known devices writes forty observation rows and no events,
// because a timeline that scrolls past at one line per device per cycle is a
// timeline nobody reads.
func (e *Enroller) event(ctx context.Context, kind, deviceID string, slotID int64,
	detail map[string]any, log *slog.Logger) {

	const q = `
INSERT INTO farm.events (kind, device_id, slot_id, actor, detail)
VALUES ($1::text, nullif($2::text,'')::uuid, $3::bigint, $4::text, $5::jsonb)`

	var slot *int64
	if slotID > 0 {
		s := slotID
		slot = &s
	}

	cctx, cancel := context.WithTimeout(ctx, e.cfg.CallTimeout)
	defer cancel()

	if _, err := e.cfg.Pool.Exec(cctx, q, kind, deviceID, slot, e.cfg.Component,
		jsonDetail(detail)); err != nil {
		if ctx.Err() == nil {
			// Warn, not Error: farm.events is the forensic trail and the fact
			// itself is already in farm.identity_observations.
			log.Warn("could not record an event", "kind", kind, "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

// Summary is one pass's counts, by resolution. It is what an operator reads
// after plugging a phone in: "42 known, 3 adopted, 1 unreadable".
type Summary struct {
	// Hosts is how many hosts were in scope.
	Hosts int
	// ByResolution counts every recorded sighting under the resolution it
	// ended with. The keys are the Resolution* constants.
	ByResolution map[string]int
	// Branded counts devices this pass wrote a farm uid onto.
	Branded int
	// BrandConflicts counts devices carrying a different uid than the row they
	// resolved to. Every one of these needs a human.
	BrandConflicts int
	// Unaddressable counts listed devices with no USB position — emulators and
	// network-attached targets, which cannot be members of a rack.
	Unaddressable int
	// Errors counts what failed: a host that could not be listed, an
	// observation that could not be written, a resolution that raised.
	Errors int
}

func newSummary() Summary { return Summary{ByResolution: map[string]int{}} }

func (s *Summary) count(resolution string) {
	if s.ByResolution == nil {
		s.ByResolution = map[string]int{}
	}
	s.ByResolution[resolution]++
}

// Seen is how many sightings were recorded.
func (s Summary) Seen() int {
	n := 0
	for _, v := range s.ByResolution {
		n += v
	}
	return n
}

// Known is how many sightings matched a device the fleet already had, by any
// rung of the ladder.
func (s Summary) Known() int {
	return s.ByResolution[ResolutionBrandedUID] +
		s.ByResolution[ResolutionHWFingerprint] +
		s.ByResolution[ResolutionSerialAndSlot]
}

// String renders the operator's one-line answer to "what happened".
func (s Summary) String() string {
	parts := []string{
		strconv.Itoa(s.Known()) + " known",
		strconv.Itoa(s.ByResolution[ResolutionAdoptedNew]) + " adopted",
	}
	// Everything else is mentioned only when it happened, so an ordinary
	// cycle reads as one short sentence and an unusual one stands out.
	for _, r := range []string{ResolutionUnreadable, ResolutionPending,
		ResolutionAmbiguous, ResolutionConflict} {
		if n := s.ByResolution[r]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+r)
		}
	}
	// Any resolution value the database grows that this build does not know
	// about still gets counted rather than silently dropped.
	var extra []string
	for r, n := range s.ByResolution {
		switch r {
		case ResolutionBrandedUID, ResolutionHWFingerprint, ResolutionSerialAndSlot,
			ResolutionAdoptedNew, ResolutionUnreadable, ResolutionPending,
			ResolutionAmbiguous, ResolutionConflict:
			continue
		}
		if n > 0 {
			extra = append(extra, strconv.Itoa(n)+" "+r)
		}
	}
	sort.Strings(extra)
	parts = append(parts, extra...)

	if s.Branded > 0 {
		parts = append(parts, strconv.Itoa(s.Branded)+" branded")
	}
	if s.BrandConflicts > 0 {
		parts = append(parts, strconv.Itoa(s.BrandConflicts)+" brand conflicts")
	}
	if s.Unaddressable > 0 {
		parts = append(parts, strconv.Itoa(s.Unaddressable)+" without a position")
	}
	if s.Errors > 0 {
		parts = append(parts, strconv.Itoa(s.Errors)+" errors")
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func jsonDetail(m map[string]any) string {
	if len(m) == 0 {
		return `{}`
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"detail_marshal_error": ` + strconv.Quote(err.Error()) + `}`
	}
	return string(b)
}

// isSQLState reports whether err is a Postgres error with the given SQLSTATE.
func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// sqlConstraint names the constraint a Postgres error came from, so a message
// about a violated reference can say WHICH reference.
func sqlConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func hostLabel(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	cyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "cycles_total",
		Help: "Enrollment passes over the fleet.",
	})

	cycleErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "cycle_errors_total",
		Help: "Passes that could not be attempted, i.e. the host list was unreadable.",
	})

	hostErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "host_errors_total",
		Help: "Hosts whose ADB server or topology could not be read this pass. Nothing is " +
			"concluded about the devices behind them: silence is not evidence.",
	}, []string{"host"})

	attachedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "devices_attached",
		Help: "Devices in the last listing from each host's ADB server.",
	}, []string{"host"})

	unslottedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "devices_unslotted",
		Help: "Attached devices at a USB position with no registered slot. A topology gap: the " +
			"sighting is recorded and the device is not adopted.",
	}, []string{"host"})

	unaddressableGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "devices_unaddressable",
		Help: "Listed devices with no USB position — emulators and network targets, which cannot " +
			"be placed in a rack.",
	}, []string{"host"})

	resolutionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "resolutions_total",
		Help: "Sightings by how they were resolved. adopted_new is a device joining the fleet; " +
			"ambiguous and conflict need a human.",
	}, []string{"resolution"})

	adoptedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "adopted_total",
		Help: "Devices adopted into farm.devices because nothing known matched them. Counts the " +
			"adoptions made on contested evidence too (resolutions_total{resolution=\"ambiguous\"}), " +
			"since those are devices joining the fleet as much as any other.",
	})

	brandsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "brands_total",
		Help: "Branding attempts by outcome. conflict means the device carries a different farm " +
			"uid than the row it resolved to, and nothing was overwritten.",
	}, []string{"outcome"})

	observeErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "observation_errors_total",
		Help: "Identity observations that could not be written or updated.",
	})

	resolveErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "resolve_errors_total",
		Help: "farm.resolve_device calls that failed. The sighting survives; no conclusion is drawn.",
	})

	probeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "farm", Subsystem: "enroll", Name: "probe_seconds",
		Help:    "How long a device took to answer the identity probe.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20},
	})
)

// Collectors returns this package's metrics for registration by the binary.
func Collectors() []prometheus.Collector {
	// Pre-create the label values an alert would be written against, so the
	// series exist before the first adoption rather than appearing only once
	// something has already gone wrong.
	for _, r := range []string{ResolutionBrandedUID, ResolutionHWFingerprint,
		ResolutionSerialAndSlot, ResolutionAdoptedNew, ResolutionPending,
		ResolutionAmbiguous, ResolutionConflict, ResolutionUnreadable} {
		resolutionsTotal.WithLabelValues(r)
	}
	for _, o := range []BrandOutcome{BrandWritten, BrandAlready, BrandConflict, BrandFailed} {
		brandsTotal.WithLabelValues(string(o))
	}
	return []prometheus.Collector{
		cyclesTotal, cycleErrors, hostErrors, attachedGauge, unslottedGauge,
		unaddressableGauge, resolutionsTotal, adoptedTotal, brandsTotal,
		observeErrors, resolveErrors, probeDuration,
	}
}
