# DeviceFarmerBatteryAnomaly

**Severity:** critical · **Group:** `device-farmer.devices`

```promql
farm_battery_anomaly == 1
  or on() (farm_battery_anomalies > 0)                                   for 0m
```

## What fired

A phone's battery is doing one of two things a healthy cell does not do, and
the watchdog's swell detector has said so: it wrote a `battery_anomaly` row in
`farm.events`, set `farm_battery_anomaly{host,rack_slot,kind}` to 1, and logged
a line with the same labels.

**`rack_slot` is the whole message.** Walk there. This is the one alert in the
file whose first step is physical and whose clock is minutes, which is why it
has no `for:` and why it is critical.

## What it means

The physics, from [docs/siting.md §2](../siting.md): a clean-agent suppression
system does not stop a lithium event. In peer-reviewed testing, Novec 1230 at
8.5 vol% "failed to suppress flaming combustion and did not prevent propagation
of thermal runaway through the array," and a pure-nitrogen baseline — no oxygen,
no flame — still propagated. Runaway is the cell's own chemistry decomposing; it
carries its own oxidiser and heats its neighbours by conduction. The mitigations
that work are containment, spacing, charge limiting and **early detection**,
because runaway announces itself — off-gassing, swelling, heat — before it is
visible. This alert is the early-detection half. Everything it buys you is time,
and it is measured from the moment you read this.

**No lease has ended, and none will.** The watchdog is the health plane; it
cannot read `farm.leases` and it did not try. If the device is mid-job, the job
is still running on it. You are going to interrupt that job by unplugging the
phone, and that is correct: a tenant loses a test run, or a rack loses a shelf.
Say so in the incident channel and do not wait for the job to finish.

### The kinds

The `kind` label says which rule fired. Two of them are thermal; one is charge.

| `kind` | Rule | What it usually is |
| --- | --- | --- |
| `temp_rise` | Case temperature climbing faster than `FARM_BATTERY_TEMP_RISE_DC_PER_MIN` (default 20 = 2.0 °C/min), fitted over the last five minutes of readings | **Walk now.** A CPU under test warms a case at tenths of a degree a minute. Two full degrees a minute is heat coming from inside the cell, at whatever temperature it happens to be passing through. |
| `temp_max` | Newest reading above `FARM_BATTERY_TEMP_MAX_DC` (default 450 = 45.0 °C) | **Walk now.** The top of every cell vendor's charging band, and where Android's own thermal service starts throttling. A case above it on an open shelf is not being warmed by the room. |
| `drain` | An **idle** device — no lease now and none ended in the last thirty minutes — on a port whose `charge_gate` is not `off`, losing charge faster than `FARM_BATTERY_DRAIN_PCT_PER_HOUR` (default 15 points/h) | A cell that cannot hold what it is given. Not a runaway; this is the phone that swells in a drawer. Same first move, with a slower clock. |

`value`, `threshold` and `unit` are in the event detail together, so the row
still means something after the thresholds have been retuned.

## The first five minutes

1. **Read `rack_slot`.** `R2-U14-H3.1.4-P5` is rack 2, shelf 14, the hub on USB
   path 3-1.4, socket 5. If the alert arrived without one — the `or on()` half
   of the rule, which fires from the fleet count when no per-position series
   exists — get it from the ledger:

   ```sh
   psql "$PGURL" -c "
   SELECT at, detail->>'rack_slot' AS rack_slot, detail->>'kind' AS kind,
          detail->>'value' AS value, detail->>'threshold' AS threshold,
          detail->>'unit' AS unit, detail->>'host' AS host
     FROM farm.events WHERE kind = 'battery_anomaly'
    ORDER BY at DESC LIMIT 10"
   ```

   A slot nobody has labelled still gets a position, derived the way the
   topology labeller does it for a host without rack coordinates:
   `h07-H3.1.4-P5` is host `h07`, the hub on USB path `3-1.4`, socket 5, and
   the rack and shelf come from `farm.hosts` (see [README](README.md), "Reading
   a rack_slot"). The row cannot be written without one: the schema refuses a
   `battery_anomaly` whose detail carries no `rack_slot`.

2. **Walk there.** Do not first try to make the phone cooler through software:
   nothing in the control plane cuts a port because of this alert. The charge
   limiter, where one is deployed, holds a port off for charge policy and not
   for a hot cell, and the only other VBUS control that exists is the recovery
   ladder's power cycle, which arms a power-ON before it cuts anything. Nothing
   here can hold this port dark. A person can.

3. **Unplug the USB cable at the phone end.** That removes the charge current,
   which is the energy input you control. If the case is hot to the touch,
   swollen, hissing, or smells sweet or of solvent, do not pick it up bare —
   use whatever the lab keeps for handling a venting cell, and if it keeps
   nothing at the shelf, that is a finding to add to
   [docs/siting.md §7](../siting.md) tomorrow. Stop reading this until the
   phone is out of the rack.

4. **Isolate it.** The siting document is explicit that the failure mode is
   cell-to-cell heating, so the barrier that matters is *distance from the
   next cell*: the containment row of its mitigation table exists because "a
   barrier interrupts it," and the spacing row because "conduction and
   radiation both fall off with distance." Put the phone on a non-combustible
   surface, away from the rack and from the drawer of spares — a metal tray on
   a concrete floor is enough — and **not** back on the shelf, not in a box
   with other phones. Purpose-built containment at handset scale is one of the
   things §7 of that document says research did not establish; use whatever
   the lab settled on locally, and if nothing was settled, distance is the
   part that is known to work.

5. **Power it off** once it is somewhere it can burn without taking anything
   with it. Then, and only then, tell the control plane: park the device so
   the recovery ladder does not spend the next ten minutes trying to bring
   back a port you unplugged on purpose.

   ```sh
   curl -sS -X POST "$FARM_API_URL/api/v1/devices/<device-id>/park" \
     -H "Authorization: Bearer $FARM_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"reason":"battery anomaly, pulled from R2-U14-H3.1.4-P5"}'
   ```

Two or more alerts on one hub at once, all `temp_rise`: treat the hub as the
patient. Unplug every phone on it — one cell heating its neighbours is exactly
the propagation the paper measured — and read
[hub-correlated-failure.md](hub-correlated-failure.md) afterwards, not before.

## What is NOT wrong

- **A phone at 42 °C under a heavy test.** Below the ceiling and, if the climb
  was slow, below the rate. The detector fits a slope through five minutes of
  readings; a test that warms a case by half a degree a minute does not fire
  it. If it *did* fire, the number in `value` tells you by how much, and the
  answer to "is 2.0 °C/min too sensitive for our lab" is a manifest change to
  `FARM_BATTERY_TEMP_RISE_DC_PER_MIN`, made in daylight, not a silence.
- **A `drain` on a phone that just finished a job.** It should not happen — the
  idle rule requires `farm.devices.last_released_at` to be older than the
  window — but a job that ran on the device *without* a lease (nothing in this
  system does that) would look like this.
- **A `drain` while the charge limiter holds the port off.** Excluded:
  `charge_gate = 'off'` exempts the device. A gate that reads `unknown` does
  not, because a port nobody has gated is a port that is powering its phone.
- **One thermistor stutter.** A single wild reading in a flat run moves a fitted
  slope by a fraction of its error, and a reading outside `[-40, 150] °C` is
  dropped before it is ever written. The rule needs three points across at
  least two minutes.
- **Anything about the lease.** It is still held. The job may still be
  running. That is not a fault; it is the point.

## Where the evidence is

The detector fits its slopes over `farm.battery_readings`, one row per reading
per device, kept for seven days:

```sh
psql "$PGURL" -c "
SELECT b.at, b.pct, b.temp_dc / 10.0 AS temp_c
  FROM farm.battery_readings b
 WHERE b.device_id = '<device-id>'
 ORDER BY b.at DESC LIMIT 40"
```

If that table is empty for a device that is `adb_state = 'device'`, readings
are not flowing and the detector is blind for it — check
`farm_watchdog_battery_history_rows_total` is climbing alongside
`farm_watchdog_battery_rows_written_total`, and read the watchdog's log for
"could not write battery observations".

## How it clears

The gauge is re-evaluated every battery cycle (one minute) from the last thirty
minutes of readings. It goes to **0** — a real zero, not a vanished series — when
the newest readings no longer satisfy the rule, or when the device has stopped
producing readings and its last ones have aged out of the window. An unplugged
phone therefore clears itself within about thirty minutes of being pulled.
There is nothing to acknowledge in the database.

The `farm.events` row is permanent. That is what it is for: a week later, when
the phone is on a bench, the row says what the detector saw and against which
threshold.

If the same phone fires again after it has been returned to the rack, retire
it. A cell that has done this once has told you what it is.

## When to escalate

- **Anything you saw, smelled or touched in step 3.** A cell that was
  swollen, venting or hot enough to be uncomfortable is an incident report and
  a call to whoever owns the building's fire policy, tonight — see
  [docs/siting.md §3](../siting.md) for why that conversation cannot wait for
  the morning.
- **More than one device on one hub or one shelf.** Propagation, or a charger
  fault common to the shelf. Nobody puts phones back on that hub until a person
  has looked at the hub's power supply.
- **A `drain` that clears and returns on the same device across days.** The
  cell is failing slowly. Retire the device before it becomes the first kind.
- **The count and the per-position series disagree** — `farm_battery_anomalies`
  is above zero and no `farm_battery_anomaly` child is at 1. The exporter
  publishes both from the same check, so this is a metrics bug, not a phone;
  the ledger query in step 1 still has the position.
