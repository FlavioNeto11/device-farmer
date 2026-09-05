# DeviceFarmerRecoveryRefusedGanged

**Severity:** warning · **Group:** `device-farmer.recovery`

```promql
sum by (host, hub, rack_slot, tier) (
  increase(farm_recovery_attempts_total{outcome="refused_ganged"}[1h])
) > 3
```

## What fired

The recovery ladder wanted to cut USB power to a broken device, and refused,
because the socket shares a **ganged** power domain with a neighbour that is
holding a live lease.

## What it means

**Nothing is broken about the ladder. This alert means buy hardware.**

A ganged power domain is a hub whose VBUS switching is per-hub rather than
per-port: you cannot power-cycle socket 5 without also power-cycling sockets 1
through 8. So when the ladder reaches `port_power` on a stuck phone, it first
asks what else is in the blast radius — and if a neighbour is running somebody's
six-hour test under a `disruption_policy` that does not permit being cut, the
ladder declines.

Declining is correct. The alternative is a watchdog that destroys live work in
order to fix a device nobody is waiting for. That is precisely the fusion this
system exists to prevent.

The cost of being correct is that the broken device stays broken until its
neighbours are idle. On a busy rack with ganged hubs, that can be never — which
is why a *rising rate* is worth a ticket even though every individual refusal
was the right call.

The fix is per-port power switching on that hub. There is no software fix, and
attempts to make one are how this system acquires the bug it was built to avoid.

## What is NOT wrong

- **The refusal.** Say it twice, because the instinct at 03:00 is to override it.
  Every refusal here protected a running job.
- **A handful of refusals in an hour.** The ladder retries on a cooldown; a
  device stuck next to a long job will refuse a few times per hour by design.
  The threshold, not the event, is what fired.
- **`outcome="refused_policy"` on the same slot.** Different cause — the
  *holder's own* policy rather than a neighbour's — see
  [recovery-refused-policy.md](recovery-refused-policy.md).
- **The neighbour's job.** It is doing nothing wrong by declining to be
  power-cycled.

## First three checks

**1. Which slot, which power domain, and what is on it.**

```sh
farmd ctl recovery
```

```sh
psql "$PGURL" -c "
SELECT s.rack_slot, s.port_number, s.power_domain_id,
       pd.kind, pd.control, pd.control_addr,
       h.usb_path AS hub, h.vbus_switchable
  FROM farm.slots s
  JOIN farm.hubs h ON h.id = s.hub_id
  LEFT JOIN farm.power_domains pd ON pd.id = s.power_domain_id
 WHERE s.rack_slot = '<rack_slot>'"
```

`pd.kind = 'ganged'` is the confirmation. `vbus_switchable = false` on the hub is
worse: nothing on it can be power-cycled at all.

**2. Who is in the blast radius, and for how long.**

```sh
psql "$PGURL" -c "
SELECT f.rack_slot, f.farm_uid, f.health, f.lease_id, f.job_id, f.tenant_id,
       f.protected, f.expires_at, j.disruption_policy, j.max_runtime
  FROM farm.v_fleet f
  LEFT JOIN farm.jobs j ON j.id = f.job_id
 WHERE f.slot_id IN (
   SELECT id FROM farm.slots
    WHERE power_domain_id = (SELECT power_domain_id FROM farm.slots WHERE rack_slot = '<rack_slot>'))
 ORDER BY f.rack_slot"
```

This is the list of jobs that would have been destroyed. `max_runtime` tells you
when the domain will next be free.

**3. How much of the rack is like this?** One ganged domain is a nuisance; a
whole rack of them is a purchasing decision.

```sh
psql "$PGURL" -c "
SELECT h.id AS host, pd.kind, count(DISTINCT pd.id) AS domains, count(s.id) AS slots
  FROM farm.power_domains pd
  JOIN farm.hosts h ON h.id = pd.host_id
  LEFT JOIN farm.slots s ON s.power_domain_id = pd.id
 GROUP BY 1,2 ORDER BY 1,2"
psql "$PGURL" -c "
SELECT host_id, usb_path, model, vbus_switchable, port_count
  FROM farm.hubs ORDER BY host_id, usb_path"
```

## What to do now

Short term, pick one — in this order of preference:

1. **Wait.** When the neighbour's lease ends, the ladder's next cycle will get
   its rung. If `max_runtime` is an hour away, that is often the whole answer.
2. **Recover the device without cutting power.** The lower rungs have a blast
   radius of one device: `adb_reconnect`, `transport_reset`, `usb_reset`. If the
   ladder has not tried them recently, the device may not need power at all.
   `farmd ctl resets --profile <p>` shows exactly what each tier will run before
   it runs.
3. **Move the phone** to a slot on a per-port-switchable hub, if you have one
   free. Discovery re-registers the position on the next scan.
4. **Ask the tenant** whether their job can carry a more permissive
   `disruption_policy`. Only sensible for work that is cheap to retry.

Do **not** power-cycle the domain by hand with `uhubctl` to clear the alert. The
refusal exists because somebody's job is on the other sockets, and doing by hand
what the ladder refused to do automatically is the same destruction with a name
attached to it.

Long term: replace the hub with one that has per-port VBUS switching, and
re-run discovery so `farm.power_domains.kind` becomes `per_port`.

## When to escalate

- **The same slot refusing for days.** The device is effectively out of the
  fleet. Either move it or accept the capacity loss explicitly, in a ticket.
- **Many slots across many domains.** This is the rack's design, not an
  incident. Take it to whoever buys hubs, with the query above as the evidence.
- **Anyone proposes relaxing the blast-radius check.** Escalate that, not the
  hardware. The check is the reason live work survives a watchdog.
