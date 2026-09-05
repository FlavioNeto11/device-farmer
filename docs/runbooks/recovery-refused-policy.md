# DeviceFarmerRecoveryRefusedPolicy

**Severity:** warning · **Group:** `device-farmer.recovery`

```promql
sum by (host, hub, rack_slot, tier) (
  increase(farm_recovery_attempts_total{outcome="refused_policy"}[6h])
) > 5
```

## What fired

The recovery ladder wanted to run a rung on a broken device, and refused,
because a live lease in the blast radius carries a `disruption_policy` that does
not permit it.

## What it means

A device is stuck broken, and it is being kept broken **on purpose** by the
policy of the job that holds it (or, less often, a neighbour's).

The ladder's rungs are ranked by how much they disturb:

| Tier | Rung | Blast radius | Requires policy |
| --- | --- | --- | --- |
| 0 | `observe` | device | `no_disruption` |
| 1 | `adb_reconnect` | device | `no_disruption` |
| 2 | `transport_reset` | device | `allow_soft_reset` |
| 3 | `usb_reset` | device | `allow_soft_reset` |
| 4 | `port_power` | power domain | `allow_port_power_cycle` |
| 5 | `device_reboot` | device | `allow_port_power_cycle` |
| 6 | `quarantine` | device | `no_disruption` |
| 7 | `adb_restart` | host | `allow_port_power_cycle` |
| 8 | `host_drain` | host | `no_disruption` |

A job submitted with `disruption_policy = 'no_disruption'` gets rungs 0, 1, 6
and 8 and nothing else. If its device needs a USB reset, it will not get one
while that lease is live.

**This is the policy working.** A job that says "do not reset my phone under me"
means it, and honouring that is more valuable than fixing a device faster. The
alert exists because the *consequence* — a phone that stays broken while
somebody waits on it — is invisible otherwise.

The mirror image of this alert is
[recovery-refused-ganged.md](recovery-refused-ganged.md), where the objection
comes from a *neighbour* sharing a ganged power supply. This one is usually the
holder's own policy.

## What is NOT wrong

- **The refusal.** The device stays broken and the job keeps it. That is the
  contract.
- **A few refusals per hour.** The ladder retries on a cooldown, so a device
  stuck under a long job produces a steady trickle by design. The 6-hour window
  and the threshold are what decide this is worth a look.
- **The job.** It chose a conservative policy, which is usually the right choice
  for expensive work.
- **`quarantine` never being refused.** Tier 6 requires only `no_disruption`, so
  it is always permitted — the ladder can always stop scheduling to a slot, even
  when it cannot touch the hardware.

## First three checks

**1. Which slot, which tier, and what the refusal said.** The ladder records the
refusal text rather than skipping silently, so the UI can explain a gap instead
of showing one.

```sh
farmd ctl recovery
```

```sh
psql "$PGURL" -c "
SELECT ra.started_at, s.rack_slot, ra.tier, rt.name, rt.requires_policy,
       ra.outcome, ra.refusal
  FROM farm.recovery_attempts ra
  LEFT JOIN farm.recovery_tiers rt ON rt.tier = ra.tier
  LEFT JOIN farm.slots s ON s.id = ra.slot_id
 WHERE ra.outcome = 'refused' AND ra.started_at > now() - interval '6 hours'
 ORDER BY ra.started_at DESC LIMIT 30"
```

The `refusal` column is a full sentence naming the tier, the blast radius, the
policy required, the lease and the job.

**2. Who holds it, what policy, and when does it end.**

```sh
psql "$PGURL" -c "
SELECT f.rack_slot, f.farm_uid, f.health, f.health_since, f.ladder_tier,
       f.lease_id, f.job_id, f.tenant_id, f.protected, f.expires_at,
       j.disruption_policy, j.max_runtime, j.state,
       now() - f.acquired_at AS held_for
  FROM farm.v_fleet f
  LEFT JOIN farm.jobs j ON j.id = f.job_id
 WHERE f.rack_slot = '<rack_slot>'"
```

`max_runtime` is the answer to "how long will this go on?" — it is a deadline
the user wrote down and it is not extended by control-plane gaps.

**3. Is the job actually making progress, or is it wedged on a dead phone?**
This is the case worth finding: a job holding a device it can no longer talk to,
whose policy forbids the fix, waiting out its whole `max_runtime` for nothing.

```sh
farmd ctl job <job-id>
farmd ctl job steps <job-id>
farmd ctl job attempts <job-id>
```

A step that started an hour ago on a device whose `health` has been `offline`
for fifty minutes is a wedged job. A device that went `degraded` while its job
kept completing steps is a job that is fine and should be left alone.

## What to do

**If the job is progressing:** nothing. Note the slot, let the lease end, and
the ladder will fix the device on its next cycle. Consider silencing for the
job's remaining `max_runtime`.

**If the job is wedged on a dead device:** talk to the tenant. It is their work
and their policy. Two outcomes:

- they cancel the job themselves — `farmd ctl job cancel <job-id>` — which releases
  the lease with `reason='job_cancelled'` and frees the ladder; or
- you revoke, which destroys their work and needs a reason with your name on it:

  ```sh
  farmd ctl lease revoke <lease-id> --reason "device offline 50m, holder wedged, agreed with <tenant>"
  ```

  Prefer the tenant cancelling. Revoke is the operator's last resort, not the
  first convenient one.

**If this keeps happening to the same tenant:** their policy and their workload
disagree. A job that cannot tolerate a USB reset but runs for six hours on
hardware that needs one every day is going to lose a device a day. That is a
conversation about `disruption_policy`, not a nightly page.

## When to escalate

- **A `protected` lease is the one refusing.** Protected work is never reclaimed
  automatically, so this can persist indefinitely. It is also the work you least
  want to revoke. Get the tenant.
- **Refusals on many slots at once with the same tier.** More likely a fleet-wide
  policy default than many individually unlucky devices — check whether a
  template or a client library recently started submitting `no_disruption`.
- **The device is needed for capacity now.** That is a scheduling conversation,
  and draining the host is the honest lever, not revoking somebody's lease.
