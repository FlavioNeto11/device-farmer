# DeviceFarmerRecoveryFailing

**Severity:** warning · **Group:** `device-farmer.recovery`

```promql
sum by (host, hub, rack_slot, tier) (
  increase(farm_recovery_attempts_total{
    outcome="failed", tier=~"adb_reconnect|soft_reset|port_power_cycle"}[1h])
) > 5
```

## What fired

Recovery rungs that actually **ran** are not working. The ladder is spending its
budget on actions that produce no healthy device.

## What it means

The commonest cause is not a broken phone. It is that **the ladder cannot reach
the hardware at all** — `farmd node` is not running on the host, or `uhubctl` is
not installed there, or the node agent's endpoint is wrong. Every hardware rung
then reports the host unreachable, and the ladder walks a perfectly good handset
all the way to quarantine without a single physical action having happened.

`internal/recovery` works hard to keep that distinction: a *refusal* (this rung
is not permitted here) and an *unreachable host* (no rung will help until
somebody fixes the machine) are recorded separately in
`farm.recovery_attempts.refusal`, precisely so a dead host does not look like a
dead phone. Read that column before you carry anything anywhere.

## Why the tier filter is in the rule

`internal/recovery` folds every outcome except `recovered` onto the metric's
`failed`, and three rungs return `no_change` **when they succeed**:

- `observe` (tier 0) → obs tier `reprobe`
- `quarantine` (tier 6) → obs tier `quarantine`
- `host_drain` (tier 8) → obs tier `quarantine`

So an unfiltered `outcome="failed"` rule fires on the most frequent healthy
event in the whole ladder. The rule therefore only counts `adb_reconnect`,
`soft_reset` and `port_power_cycle` — the rungs where "failed" means something
actually failed. If you widen `prometheusRule.thresholds.recoveryActionTiers`,
you are re-introducing a permanent false alarm.

## What is NOT wrong

- **`outcome="failed"` on tier `reprobe` or `quarantine`.** See above. Normal,
  and deliberately excluded.
- **`outcome="aborted"`.** The context was cancelled — a shutdown, usually. It
  is not a hardware verdict, and it is not counted here.
- **A device that failed a rung and then recovered on the next one.** The ladder
  climbing is the ladder working. This alert needs sustained failure.
- **A single flaky phone.** One device failing `usb_reset` five times in an hour
  is a phone on its way to quarantine, which is the ladder doing its job. The
  alert groups by `rack_slot` so you can see whether it is one or many.

## First three checks

**1. Is the host agent alive?** Do this first; it is the cheapest and the most
often the answer.

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.roles[] | select(.component | startswith("node"))'
farmd ctl hosts
psql "$PGURL" -c "
SELECT id, adb_endpoint, node_endpoint, node_version, agent_version, host_epoch,
       now() - last_seen_at AS unseen
  FROM farm.hosts ORDER BY id"
psql "$PGURL" -c "
SELECT component, now() - beat_at AS age
  FROM farm.component_heartbeat WHERE component LIKE 'node%' ORDER BY age DESC"
```

A `node_endpoint` that is NULL or empty means the agent never registered — the
ladder has no way to reset a USB port on that machine and never had. A stale
heartbeat means it stopped.

`farmd node` runs on the bare-metal host, not in the cluster: it needs
`/dev/bus/usb` and `uhubctl` on the machine the phones are plugged into, so it
is not a workload the chart can deploy.

**2. Read the failures, with the reason.**

```sh
psql "$PGURL" -c "
SELECT ra.started_at, s.rack_slot, ra.host_id, ra.tier, rt.name,
       ra.outcome, ra.refusal, ra.detail->>'error' AS error,
       ra.detail->>'disposition' AS disposition
  FROM farm.recovery_attempts ra
  LEFT JOIN farm.recovery_tiers rt ON rt.tier = ra.tier
  LEFT JOIN farm.slots s ON s.id = ra.slot_id
 WHERE ra.finished_at > now() - interval '2 hours'
   AND ra.outcome IN ('failed','refused')
 ORDER BY ra.started_at DESC LIMIT 40"
```

`disposition` is the verdict the actuator gave. `unreachable` on every row is the
missing-agent case. A per-device `error` mentioning a timeout on a specific
serial is a real device problem.

**3. Is it one slot, one hub, or one host?**

```sh
psql "$PGURL" -c "
SELECT ra.host_id, h.usb_path AS hub, count(*) AS failures,
       count(DISTINCT ra.device_id) AS devices
  FROM farm.recovery_attempts ra
  LEFT JOIN farm.hubs h ON h.id = ra.hub_id
 WHERE ra.outcome = 'failed' AND ra.finished_at > now() - interval '6 hours'
 GROUP BY 1,2 ORDER BY 3 DESC"
```

- **One host, every hub** → the node agent, or the host's ADB server.
- **One hub** → [hub-correlated-failure.md](hub-correlated-failure.md).
- **One slot** → the phone, or that socket.

## What to do

**If the node agent is missing:** install and start it on the host. Until then,
tiers 2 through 5 and 7 cannot work anywhere on that machine, and the ladder
will keep quarantining healthy devices. Consider draining the host so it stops
accepting new work while it cannot be recovered:

```sh
farmd ctl host drain <host> --reason "node agent down; hardware recovery unavailable"
```

**If `uhubctl` is missing or the hub is not switchable:** `port_power` cannot
work there. Confirm:

```sh
psql "$PGURL" -c "
SELECT host_id, usb_path, model, vbus_switchable, port_count FROM farm.hubs ORDER BY 1,2"
```

`vbus_switchable = false` means the hardware cannot do it, and no amount of
software will change that.

**If it is genuinely one phone:** let the ladder finish. It will quarantine, and
[devices-quarantined.md](devices-quarantined.md) picks up from there.

**Before touching a slot by hand:** check whether it holds a live lease.

```sh
psql "$PGURL" -c "
SELECT rack_slot, lease_id, job_id, tenant_id, lease_state, protected
  FROM farm.v_fleet WHERE rack_slot = '<rack_slot>' AND lease_id IS NOT NULL"
```

Cutting power to a slot with a live lease destroys that job's work — the exact
thing the ladder refused to do on your behalf.

## When to escalate

- **Every host in the farm.** A common deployment step was missed, or the node
  agents cannot reach the API. Both are one fix, not many.
- **Devices being quarantined while `disposition = 'unreachable'`.** Healthy
  hardware is leaving the fleet because of a host-side problem. Drain the host
  before the ladder eats the rack.
- **`port_power` failing on a hub that reports `vbus_switchable = true`.** The
  topology says it should work and it does not. That is either bad discovery
  data or a failing hub, and both want a hardware look.
