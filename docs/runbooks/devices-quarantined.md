# DeviceFarmerDevicesQuarantined / DeviceFarmerHubQuarantined

**Severity:** warning · **Group:** `device-farmer.devices`

```promql
sum(farm_device_health{state="quarantined"}) > 3                        for 15m
sum by (host, hub) (farm_device_health{state="quarantined"}) > 2        for 15m
```

Two alerts, one runbook, because they are the same fact at two scales — and the
scale is the diagnosis. Quarantine creep of one device on each of eight hubs is
a fleet wearing out. Four on one hub is one piece of broken hardware.

## What fired

Devices are sitting in `health = 'quarantined'`. The recovery ladder walked them
to its last rung, gave up, and stopped scheduling to those slots.

## What it means

Capacity has left the fleet, and **it will not come back on its own**. There is
no timer that reopens a quarantine. `farm.quarantines` rows are closed by an
operator, and until somebody does, the scheduler cannot see those devices at
all.

Quarantine is the ladder's honest admission of failure. It ran `observe`, then
`adb_reconnect`, `transport_reset`, `usb_reset`, `port_power`, `device_reboot`,
and none of them produced a healthy device — or it was refused every rung that
would have helped. The row in `farm.quarantines` says which.

**No lease was ended by any of this.** Quarantine stops *future* placement; a
device that still holds a live lease keeps holding it, and the job keeps running
until it ends by itself.

## What is NOT wrong

- **One or two quarantined devices in a large fleet.** Phones break. That is why
  the fleet is a fleet. The threshold, not the state, is what fired.
- **A quarantine that appeared right after somebody moved cables.** Discovery
  will re-slot the devices; close the quarantine after the move settles.
- **`scope = 'hub'` quarantines.** Those are the ladder correctly deciding that
  the hub, not the phones, is the fault — see
  [hub-correlated-failure.md](hub-correlated-failure.md). One hub quarantine is
  a much better outcome than twelve device quarantines, and it is what the
  correlation logic is for.
- **`auto = true`.** Almost all of them are. The ladder opens them.

## First three checks

**1. What is quarantined, where, and why.**

```sh
farmd ctl fleet --health quarantined
farmd ctl recovery
```

```sh
psql "$PGURL" -c "
SELECT q.id, q.scope, q.reason, q.opened_at, q.auto,
       f.rack_slot, f.farm_uid, f.host_id, f.hub_path, f.health, f.ladder_tier
  FROM farm.quarantines q
  LEFT JOIN farm.v_fleet f ON f.device_id = q.device_id
 WHERE q.closed_at IS NULL
 ORDER BY q.opened_at DESC"
```

`reason` is written by the ladder and usually names the rung that failed last.
`rack_slot` is where to walk.

**2. Is it clustered?** This is the question that decides whether you are
carrying a phone or a hub.

```sh
psql "$PGURL" -c "
SELECT f.host_id, f.hub_path, count(*) AS quarantined
  FROM farm.v_fleet f
 WHERE f.health = 'quarantined'
 GROUP BY 1, 2 ORDER BY 3 DESC"
```

Clustered on one hub → [hub-correlated-failure.md](hub-correlated-failure.md).
Clustered on one host → the host's ADB server or the machine itself. Spread
evenly → genuinely tired handsets, or a systemic problem in the ladder.

**3. What did the ladder actually try?** This is the check that catches the
worst failure mode: a ladder that quarantined healthy phones because it could
not perform a single physical action on them.

```sh
psql "$PGURL" -c "
SELECT ra.started_at, ra.tier, rt.name, ra.outcome, ra.refusal,
       s.rack_slot
  FROM farm.recovery_attempts ra
  LEFT JOIN farm.recovery_tiers rt ON rt.tier = ra.tier
  LEFT JOIN farm.slots s ON s.id = ra.slot_id
 WHERE ra.device_id = '<device-id>'
 ORDER BY ra.started_at DESC LIMIT 30"
```

If every hardware rung shows `outcome = 'refused'` with a refusal about an
unreachable host, or shows `failed` with a host error, **the phones are probably
fine and the host agent is missing** — go to
[recovery-failing.md](recovery-failing.md) before touching hardware.

If they show `refused` because of a live lease's `disruption_policy`, go to
[recovery-refused-policy.md](recovery-refused-policy.md).

## Closing a quarantine

Only after the underlying fault is fixed, or after you have decided the device
is fine.

```sh
curl -sS -X POST "$FARM_API_URL/api/v1/quarantines/<id>/close" \
  -H "Authorization: Bearer $FARM_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"replaced cable at R2-U14-H3.1.4-P5"}'
```

Closing the row is all you do. On its next cycle the recovery loop drops the
affected devices back to `health = 'unknown'` — not to `healthy`, deliberately:
nobody has looked at the device since, and `healthy` would be an assumption the
allocator would immediately act on. The watchdog re-observes it from scratch.
The ladder is also reset to rung zero, so a repair is not answered with an ADB
server restart.

## When to escalate

- **Quarantines rising steadily over days.** The fleet is shrinking. This is a
  capacity conversation and a hardware budget, not a nightly cleanup.
- **The same device quarantined repeatedly after being closed.** Retire it:
  ```sh
  psql "$PGURL" -c "SELECT id, farm_uid, model, failure_score, failure_score_at
                      FROM farm.devices WHERE id = '<device-id>'"
  ```
  `failure_score` is the fleet's memory of how much trouble a handset has been.
- **A quarantine you cannot explain from `farm.recovery_attempts`.** Something
  other than the ladder wrote it, and that is worth understanding before it
  writes more.
