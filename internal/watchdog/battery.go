package watchdog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// ---------------------------------------------------------------------------
// Battery and temperature: the level-triggered half of device health
// ---------------------------------------------------------------------------
//
// farm.device_runtime.battery_pct and .battery_temp_dc have existed since the
// first migration and, until this file, nothing in the control plane wrote
// them. The API serves them, the CLI renders them and the UI draws a battery
// meter from them; the only producer in the tree was the demo seeder. This is
// the producer.
//
// # Why it is a separate ticker and not part of the reconciler
//
// The reconciler in watchdog.go is EDGE-triggered: it reads the ADB server's
// track-devices stream, which speaks only when a device's connection state
// changes, and it deliberately skips a device whose state is unchanged (see
// the lastState cache). That is the right shape for a connection state, which
// only ever moves at an edge.
//
// A battery has no edges. It drifts continuously while the wire says nothing
// at all, so a rack that has been quiet for six hours — the normal state of a
// working farm — would produce not one battery reading. Hanging the poll off
// the state cache would also mean the only way to learn a device is at 3% is
// for it to first go offline, which is precisely the observation that arrives
// too late to be worth anything. So it gets its own clock.
//
// # This file OBSERVES. It does not decide
//
// Nothing here reads a battery in order to act on it: no charging is started
// or stopped, no device is taken out of the pool for being low, no health
// verdict is reached. That is deliberate and it is a boundary, not an
// omission — charge policy is somebody else's column (charge_gate) and this
// one must stay a measurement, so that the policy can be changed, argued with
// or turned off without changing what the fleet remembers about its hardware.
//
// The package's one rule holds unchanged: farm.leases is not read, not
// written, and not importable from here. A flat battery is not a reason to
// take a device away from the job that is holding it.
//
// # Absence of a reading is not a reading of zero
//
// An offline, unauthorized or wedged device cannot answer, and the honest
// record of that is no record at all. Both columns are nullable exactly so
// that NULL can mean "never observed", and a device that does not answer is
// left with whatever was last observed — nothing is written for it, so its
// row is not even touched. Writing a 0 would tell the fleet a phone is flat
// when the truth is that nobody could ask it.

// BatteryCommand is the single command this reader runs on a device.
//
// One round trip per device, not three: dumpsys prints level, scale and
// temperature together, so asking for them separately would triple the
// transports opened against devices that are mostly mid-job — the same
// reasoning that makes internal/enroll ask for eleven properties in one
// getprop loop.
//
// dumpsys rather than /sys/class/power_supply/*: the sysfs layout, its units
// and even the name of the battery node differ by SoC, while the framework
// service normalises all of it — level against scale, and temperature in
// decidegrees Celsius, which is exactly what battery_temp_dc stores.
//
// It is exported so a fake or a host agent can script the same string rather
// than a copy of it that drifts.
const BatteryCommand = "dumpsys battery"

const (
	// DefaultBatteryInterval is how often every attached device is asked.
	//
	// The cadence is set by what is being measured, not by what the loop could
	// manage. A phone under test moves about one percent of charge every one
	// to three minutes and its case temperature moves slower still, so a
	// one-minute sample is already finer than the signal: at five seconds —
	// the reconciler's cadence — a 56-device host would open sixty transports
	// every five seconds, competing with the jobs actually running on those
	// devices, to re-read a number that had not changed. One minute costs
	// under one shell call per second on the same host, bounds staleness at
	// one minute for whatever policy later consumes the column, and matches
	// DefaultResync, which is this loop's existing answer to "how often is a
	// quiet rack worth re-examining".
	DefaultBatteryInterval = 60 * time.Second

	// batteryProbeTimeout bounds ONE device-side call.
	//
	// It is deliberately smaller than Config.CallTimeout, which bounds
	// database statements. A wedged handset that accepts a transport and then
	// never answers must not be able to stretch a cycle past the next tick:
	// with batteryConcurrency in flight, a host whose every device is wedged
	// costs ceil(n/concurrency) * this, which for 56 devices is 35s — inside
	// one interval, so cycles cannot pile up on each other.
	batteryProbeTimeout = 5 * time.Second

	// batteryConcurrency is how many devices of one host are asked at once.
	// Sequential would be simplest but makes one wedged device delay every
	// device behind it; unbounded would open a transport per handset at the
	// same instant on a rack of sixty.
	//
	// The bound is per ADB SERVER, not per process, because the ADB server and
	// the USB bus behind it are the contended resource — the production shape
	// is one watchdog pod per host anyway. A single process supervising twenty
	// hosts (FARM_HOST_ID unset) therefore opens up to twenty times this many
	// sockets, spread over twenty different servers, and every rack still
	// finishes its cycle inside one interval. A process-wide cap would instead
	// let one busy host starve the other nineteen.
	batteryConcurrency = 8

	// batteryMaxOutput caps what one probe may print. A real dumpsys battery
	// is a few hundred bytes; the default 8 MiB cap is sized for logcat. The
	// small cap means a device answering with something that is not this
	// question is cut off early and reported as truncated instead of being
	// parsed.
	batteryMaxOutput = 32 << 10

	// Plausible battery temperatures, in decidegrees Celsius. A lithium cell
	// outside this range is not a cell, it is a sensor that is not there — the
	// common form being a device that reports 0 or a huge sentinel for a
	// thermistor it cannot read. Values outside the range are dropped rather
	// than clamped: clamping would invent an observation.
	//
	// migrations/00012_battery_temp_check.sql carries the same bounds as a
	// CHECK. This filter is the stricter of the two by construction, so the
	// constraint can never fire for a row written here — it is there to catch
	// a future writer, and to say in the schema what the unit is.
	minBatteryTempDC = -400
	maxBatteryTempDC = 1500
)

// batteryShell is the slice of the ADB host protocol the battery reader needs:
// run one command against one PHYSICAL POSITION and read what it printed.
// *adbwire.Client satisfies it.
//
// As in internal/enroll, the interface exists so the reader can be driven
// without an ADB server — not so that a different addressing scheme can be
// substituted. The method takes a devpath because two handsets can report the
// same OEM serial, and a serial-addressed probe would read one clone's battery
// and write it onto the other clone's row.
type batteryShell interface {
	Shell(ctx context.Context, devpath, command string) (*adbwire.ShellResult, error)
}

// batteryTarget is one device worth asking, resolved to the position it sits
// in and the ADB server that can reach it.
type batteryTarget struct {
	DeviceID string
	Devpath  string
	Host     string
	Endpoint string
}

// batteryReading is what one device said about itself. A nil field is a value
// the device did not report, or reported as nonsense; it is never a zero.
type batteryReading struct {
	DeviceID string
	Pct      *int32
	TempDC   *int32
}

// batteryPoller is the reader's own loop.
type batteryPoller struct {
	pool     *pgxpool.Pool
	hostID   string
	endpoint string // Config.ADBEndpoint override; "" means per-host
	interval time.Duration

	// callTimeout bounds a database statement; probeTimeout bounds one
	// device-side shell call. They are separate because a slow phone and a
	// slow database are different failures with different right answers.
	callTimeout  time.Duration
	probeTimeout time.Duration

	log *slog.Logger

	// lastHosts is the set of hosts the previous cycle published a target
	// count for, so a host that has lost every device can be zeroed instead of
	// leaving a gauge frozen at its last value. Touched only from run's own
	// goroutine, which is why it needs no lock.
	lastHosts map[string]struct{}

	// dial builds the shell client for one host's ADB server. It is a field so
	// a test can drive the reader against a fake, and it is the ONLY seam:
	// everything below it is the shipping code path.
	dial func(endpoint string) batteryShell
}

// newBatteryPoller wires a poller from the watchdog's config.
func (w *Watchdog) newBatteryPoller() *batteryPoller {
	p := &batteryPoller{
		pool:         w.cfg.Pool,
		hostID:       w.cfg.HostID,
		endpoint:     w.cfg.ADBEndpoint,
		interval:     DefaultBatteryInterval,
		callTimeout:  w.cfg.CallTimeout,
		probeTimeout: batteryProbeTimeout,
		log:          w.log.With("reader", "battery"),
	}
	p.dial = func(endpoint string) batteryShell {
		return adbwire.New(endpoint,
			adbwire.WithLogger(p.log),
			adbwire.WithCallTimeout(p.probeTimeout),
			adbwire.WithMaxOutput(batteryMaxOutput))
	}
	return p
}

// run polls until ctx is cancelled.
//
// The first cycle runs immediately rather than one interval in: a watchdog
// that has just started is exactly when the columns are most likely to be
// stale, and waiting a minute to find out a rack is flat helps nobody.
func (p *batteryPoller) run(ctx context.Context) {
	p.log.Info("battery reader starting", "interval", p.interval,
		"probe_timeout", p.probeTimeout, "command", BatteryCommand)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		p.cycle(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cycle asks every attached device once and writes what came back.
func (p *batteryPoller) cycle(ctx context.Context) {
	batteryCyclesTotal.Inc()
	start := time.Now()

	targets, err := p.targets(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("could not list the devices to read a battery from", "err", err)
		}
		return
	}

	byHost := make(map[string][]batteryTarget)
	for _, t := range targets {
		byHost[t.Host] = append(byHost[t.Host], t)
	}
	p.publishTargets(byHost)

	var readings []batteryReading
	if len(byHost) > 0 {
		readings = p.poll(ctx, byHost)
		if len(readings) > 0 {
			if err := p.write(ctx, readings); err != nil && ctx.Err() == nil {
				batteryWriteErrors.Inc()
				p.log.Warn("could not write battery observations",
					"devices", len(readings), "err", err)
			}
		}
	}

	// Published so an operator can see a cycle creeping toward the interval,
	// which is the shape of a rack full of handsets that accept a transport
	// and then never answer.
	batteryCycleSeconds.WithLabelValues(orAll(p.hostID)).Set(time.Since(start).Seconds())
	p.log.Debug("battery cycle complete",
		"asked", len(targets), "answered", len(readings), "took", time.Since(start))
}

// publishTargets sets the per-host target gauge.
//
// Per host and not per process: one farmd with FARM_HOST_ID unset runs a
// watchdog, and therefore a poller, for every registered host, and an
// unlabelled gauge would have all of them overwriting each other — the number
// on the dashboard would be whichever host wrote last.
//
// A host that has lost every device is set to zero rather than left alone. A
// gauge frozen at its last value reads as "sixty devices, all present" for a
// rack that has actually vanished, which is the mistake obs.SetDeviceHealth
// zero-fills to avoid.
func (p *batteryPoller) publishTargets(byHost map[string][]batteryTarget) {
	for _, host := range vanishedHosts(p.lastHosts, byHost) {
		batteryTargetsGauge.WithLabelValues(host).Set(0)
	}
	seen := make(map[string]struct{}, len(byHost))
	for host, list := range byHost {
		batteryTargetsGauge.WithLabelValues(host).Set(float64(len(list)))
		seen[host] = struct{}{}
	}
	p.lastHosts = seen
}

// vanishedHosts names the hosts that had targets last cycle and have none now.
// Separated from the gauge write so the decision can be tested without a
// metrics registry.
func vanishedHosts(last map[string]struct{}, now map[string][]batteryTarget) []string {
	var gone []string
	for host := range last {
		if _, still := now[host]; !still {
			gone = append(gone, host)
		}
	}
	return gone
}

// targets lists the devices worth asking.
//
// The gate is adb_state = 'device', which is the reconciler's own conclusion
// about the wire: only a transport the ADB server currently lists as usable
// can answer a shell, and asking the others would spend a five-second timeout
// per handset to learn what the health column already says. That also makes
// this reader inherit the reconciler's honesty about silence — a host whose
// socket dropped has no devices in state 'device' being invented for it.
//
// farm.leases is absent here, as it is from every query in this package, and
// farm.v_fleet is not used because it carries lease columns.
func (p *batteryPoller) targets(ctx context.Context) ([]batteryTarget, error) {
	const q = `
SELECT r.device_id::text, s.adb_devpath, h.id, h.adb_endpoint
  FROM farm.device_runtime r
  JOIN farm.devices d ON d.id = r.device_id
  JOIN farm.slots   s ON s.id = d.current_slot_id
  JOIN farm.hosts   h ON h.id = s.host_id
 WHERE r.adb_state = 'device'
   AND d.admin_state <> 'retired'
   AND h.admin_state <> 'disabled'
   AND ($1::text = '' OR h.id = $1::text)
 ORDER BY h.id, s.adb_devpath`

	cctx, cancel := context.WithTimeout(ctx, p.callTimeout)
	defer cancel()

	rows, err := p.pool.Query(cctx, q, p.hostID)
	if err != nil {
		return nil, fmt.Errorf("watchdog: list battery targets: %w", err)
	}
	defer rows.Close()

	var out []batteryTarget
	for rows.Next() {
		var t batteryTarget
		if err := rows.Scan(&t.DeviceID, &t.Devpath, &t.Host, &t.Endpoint); err != nil {
			return nil, fmt.Errorf("watchdog: list battery targets scan: %w", err)
		}
		if p.endpoint != "" {
			// A node-local deployment reaches its ADB server at 127.0.0.1:5037
			// whatever address the rest of the fleet uses; same override the
			// reconciler applies to farm.hosts.adb_endpoint.
			t.Endpoint = p.endpoint
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("watchdog: list battery targets: %w", err)
	}
	return out, nil
}

// poll asks every target, one host's devices at a time, and returns only the
// devices that actually answered.
func (p *batteryPoller) poll(ctx context.Context, byHost map[string][]batteryTarget) []batteryReading {
	total := 0
	for _, list := range byHost {
		total += len(list)
	}

	var (
		mu  sync.Mutex
		out = make([]batteryReading, 0, total)
		wg  sync.WaitGroup
	)

	for host, list := range byHost {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// One client per host per cycle. adbwire dials per call, so this
			// holds no socket between cycles and a host whose endpoint moved
			// is picked up on the next one without any bookkeeping.
			sh := p.dial(list[0].Endpoint)

			// The group is per host, not shared: a WaitGroup that is added to
			// after its counter has reached zero is a race, and a shared one
			// would also make a quick host wait on a slow one for nothing.
			var probes sync.WaitGroup
			defer probes.Wait()

			sem := make(chan struct{}, batteryConcurrency)
			for _, t := range list {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				probes.Add(1)
				go func() {
					defer probes.Done()
					defer func() { <-sem }()

					reading, reason := p.readOne(ctx, sh, t)
					if reason != "" {
						batteryReadings.WithLabelValues(host, reason).Inc()
						return
					}
					batteryReadings.WithLabelValues(host, "ok").Inc()
					mu.Lock()
					out = append(out, reading)
					mu.Unlock()
				}()
			}
		}()
	}
	wg.Wait()
	return out
}

// readOne runs the probe against one device.
//
// It never returns an error: a device that cannot be read is an ordinary,
// expected outcome and the whole point of this reader is that such a device
// produces no row change at all. The second return is the reason, empty when
// the device answered, and it is a closed set so it can label a counter.
func (p *batteryPoller) readOne(ctx context.Context, sh batteryShell, t batteryTarget) (batteryReading, string) {
	cctx, cancel := context.WithTimeout(ctx, p.probeTimeout)
	defer cancel()

	res, err := sh.Shell(cctx, t.Devpath, BatteryCommand)
	if err != nil {
		return batteryReading{}, batteryFailure(ctx, err)
	}
	if res == nil {
		// Unreachable through *adbwire.Client, which never returns a nil
		// result with a nil error. Probes run in their own goroutines, so a
		// batteryShell that did would take the process down with a nil
		// dereference instead of costing one reading.
		return batteryReading{}, "no_result"
	}
	if res.Truncated {
		// The tail was cut, and the cut can land inside a number: a dump
		// ending in "temperature: 29" would be read as 2.9 C. Half an answer
		// is not an answer.
		return batteryReading{}, "truncated"
	}
	if res.Exited && res.ExitCode != 0 {
		// dumpsys said it failed, so its stdout is not the answer to this
		// question. A stream that ended without an exit frame at all is
		// tolerated: shell v1 servers do not send one, and the values that
		// did arrive are still whole lines.
		return batteryReading{}, "nonzero_exit"
	}

	pct, tempDC := parseBatteryDump(res.Stdout)
	if pct == nil && tempDC == nil {
		// The device is up and the shell worked, but nothing usable came back
		// — a handset whose framework has not finished booting answers exactly
		// like this. Not a fault, and not something to write.
		return batteryReading{}, "no_value"
	}
	return batteryReading{DeviceID: t.DeviceID, Pct: pct, TempDC: tempDC}, ""
}

// batteryFailure classifies a failed probe for the counter.
//
// The distinction that matters is between "this device did not answer" and
// "we stopped asking": a cancelled probe is a fact about this process shutting
// down, and counting it as a device problem would put a redeploy into the
// record as evidence against a phone.
func batteryFailure(parent context.Context, err error) string {
	if parent.Err() != nil {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if te, ok := adbwire.AsTransport(err); ok && te.Timeout() {
		return "timeout"
	}
	if adbwire.IsNotFound(err) {
		// The transport went away between the listing and the probe. Common,
		// and about the wire rather than the phone.
		return "detached"
	}
	return "failed"
}

// parseBatteryDump reads level, scale and temperature out of a dumpsys battery
// dump. Either return is nil when the device did not report that value, or
// reported one that cannot be true.
//
// The dump is a flat list of "  key: value" lines under a header, and it is
// scanned in place rather than split so that a device answering this question
// with a novel costs one pass and no per-line allocation. Unknown keys are
// ignored: the set of things dumpsys prints grows with every Android release
// and a reader that fails on an unfamiliar line would stop working on the next
// one.
func parseBatteryDump(out []byte) (pct *int32, tempDC *int32) {
	var (
		level, scale, temp             int64
		haveLevel, haveScale, haveTemp bool
	)

	rest := string(out)
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		n, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			// Every key read here is numeric, so a value that is not a number
			// is either a different key ("technology: Li-ion") or a device
			// answering something else entirely. Both are the same to us.
			continue
		}
		switch key {
		case "level":
			level, haveLevel = n, true
		case "scale":
			scale, haveScale = n, true
		case "temperature":
			temp, haveTemp = n, true
		}
	}

	// level is a fraction of scale, not a percentage: the framework's default
	// scale is 100 and almost every device uses it, but the field exists
	// precisely because some do not, and reading level as a percent on a
	// device that scales to 255 would report a full battery as 39%.
	if haveLevel && level >= 0 {
		denom := int64(100)
		if haveScale && scale > 0 {
			denom = scale
		}
		// Rounded, not truncated: at scale 100 the two agree, and where they
		// do not, half a percent of error should not always fall the same way.
		if v := (level*100 + denom/2) / denom; v >= 0 && v <= 100 {
			// Out of range means level exceeded scale, which is nonsense and
			// is dropped rather than clamped to 100 — and rather than being
			// carried into a CHECK violation on battery_pct, which would take
			// down the whole cycle's write and with it every OTHER device's
			// reading.
			n := int32(v)
			pct = &n
		}
	}

	if haveTemp && temp >= minBatteryTempDC && temp <= maxBatteryTempDC {
		n := int32(temp)
		tempDC = &n
	}
	return pct, tempDC
}

// write records one cycle's readings in a single statement.
//
// One statement rather than one per device: a cycle produces up to a rackful
// of rows at once and they are independent, so there is nothing to be gained
// from sixty round trips. COALESCE on both columns is what keeps a partial
// answer from erasing a whole one — a device that reported a temperature but
// no level must not lose the level somebody read from it a minute ago.
//
// now() is the server's, as everywhere in this package: no clock reading from
// this process is ever written to the database.
func (p *batteryPoller) write(ctx context.Context, readings []batteryReading) error {
	const q = `
UPDATE farm.device_runtime r
   SET battery_pct     = COALESCE(v.pct, r.battery_pct),
       battery_temp_dc = COALESCE(v.temp_dc, r.battery_temp_dc),
       updated_at      = now()
  FROM unnest($1::text[], $2::int[], $3::int[]) AS v(device_id, pct, temp_dc)
 WHERE r.device_id = v.device_id::uuid`

	ids := make([]string, len(readings))
	pcts := make([]*int32, len(readings))
	temps := make([]*int32, len(readings))
	for i, r := range readings {
		ids[i], pcts[i], temps[i] = r.DeviceID, r.Pct, r.TempDC
	}

	cctx, cancel := context.WithTimeout(ctx, p.callTimeout)
	defer cancel()

	tag, err := p.pool.Exec(cctx, q, ids, pcts, temps)
	if err != nil {
		return fmt.Errorf("watchdog: write battery observations: %w", err)
	}
	batteryRowsWritten.Add(float64(tag.RowsAffected()))
	return nil
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	batteryCyclesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_cycles_total",
		Help: "Battery polling cycles started.",
	})

	batteryReadings = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_readings_total",
		Help: "Battery probes by outcome. Anything but result=ok means nothing was written " +
			"for that device, which is the point: a device that cannot answer is left with " +
			"its last observation rather than a fabricated zero.",
	}, []string{"host", "result"})

	batteryTargetsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_targets",
		Help: "Devices in adb_state 'device' that the last cycle asked for a battery reading. " +
			"Per host, because one process can supervise many.",
	}, []string{"host"})

	batteryRowsWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_rows_written_total",
		Help: "farm.device_runtime rows updated with an observed battery level or temperature.",
	})

	batteryWriteErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_write_errors_total",
		Help: "Cycles whose observations could not be written.",
	})

	batteryCycleSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "watchdog", Name: "battery_cycle_seconds",
		Help: "Duration of the last battery cycle, by the scope of the poller that ran it: a host " +
			"id, or (all) for a process watching every host. Approaching the poll interval means " +
			"devices are accepting transports and not answering.",
	}, []string{"scope"})
)

// batteryCollectors returns this reader's metrics, for Collectors.
func batteryCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		batteryCyclesTotal, batteryReadings, batteryTargetsGauge,
		batteryRowsWritten, batteryWriteErrors, batteryCycleSeconds,
	}
}
