# DeviceFarmerControlPlaneGap / DeviceFarmerControlPlaneGapBudget

**Severity:** warning — a ticket, not a page · **Group:** `device-farmer.control-plane`

```promql
sum by (component) (increase(farm_control_plane_gap_seconds_count[30m])) > 0
sum(increase(farm_control_plane_gap_seconds_sum[1h])) > 600
```

Two alerts, one response. The first says a gap happened; the second says they
are adding up.

## What fired

`farm.reaper_arm` found the oldest heartbeat across `FARM_REAPER_COMPONENTS`
(`reaper`, `api`, `scheduler` by default) older than `FARM_REAPER_GAP_FLOOR`,
recorded a row in `farm.control_plane_gap`, and **added that interval back to
`expires_at` and `reclaimable_at` on every live lease in the farm**.

## What it means

A component on the lease-renewal path stopped beating. While it was gone,
holders could not renew — and the reaper, when it came back, refused to charge
them for it.

**This is not an emergency and it must not be treated as one.** No work was
destroyed. That is the whole design:

```
UPDATE farm.leases
   SET expires_at     = expires_at + v_gap,
       reclaimable_at = reclaimable_at + v_gap
 WHERE state IN ('held','suspect');
```

and then the reaper quiesces for the longest TTL it could have missed, so it
cannot mass-reclaim at the moment of restoration.

The reason `internal/obs/doc.go` insists this is a ticket rather than a page is
specific and worth internalising at 03:00: if this wakes people up, somebody
eventually "fixes" it by disabling the quiesce gate — and a reaper that sweeps
immediately after a blind period is exactly how a control-plane restart
mass-reclaims a farm. The alert that was meant to protect tenants becomes the
mechanism that robs them.

## Timing, which is counter-intuitive

`farm.reaper_arm` runs **once per gain of leadership**, not once per sweep. So:

- This alert fires when the gap is *discovered and refunded* — usually when the
  reaper itself restarts — not while the outage is happening.
- A component that is down **right now** does not fire this. It fires
  `DeviceFarmerComponentBeatFailing` (if the roles group is enabled) and shows
  up in `/api/v1/capabilities`.
- A very long gap can therefore surface hours after the outage that caused it.

## What is NOT wrong

- **`farm_lease_suspect` having spiked during the gap.** Suspect releases
  nothing. Leases that were marked go back to `held` at the same fence on the
  next heartbeat.
- **A gap the size of a rolling deploy.** With `FARM_REAPER_GAP_FLOOR` at 60s
  and `FARM_HEARTBEAT_INTERVAL` at 5s, a slow image pull on the last replica of
  a component can cross the floor. One a week is noise.
- **`component` naming a role you did not restart.** The label is whichever
  component had the *oldest* beat, which is the one that was down longest, not
  necessarily the one you were working on.
- **`watchdog`, `recovery`, `jobrunner` or `node` being down.** They are
  deliberately **not** in `FARM_REAPER_COMPONENTS`. Listing the watchdog there
  would let device health move a lease deadline, which fuses two clocks this
  system keeps apart on purpose. Their outages are real problems; they are not
  this one.

## First three checks

**1. What was down, for how long, and when.**

```sh
psql "$PGURL" -c "
SELECT component, started_at, ended_at, ended_at - started_at AS gap
  FROM farm.control_plane_gap
 ORDER BY started_at DESC LIMIT 20"
```

The `started_at` is the last successful beat, not the moment anything noticed.

**2. Is it beating now?**

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.roles'
psql "$PGURL" -c "
SELECT component, beat_at, now() - beat_at AS age
  FROM farm.component_heartbeat ORDER BY age DESC"
```

Anything with an `age` above a minute or two is still down. `age` above
`FARM_REAPER_GAP_FLOOR` means the next arm will record another gap.

**3. Confirm the refund actually landed, and that nothing was reclaimed
anyway.**

```sh
farmd ctl reaper
farmd ctl endings --ended-by reaper --since 6h
```

`ctl reaper` reads `farm.reaper_state`; `ctl endings` reads the ledger over the
API. Without a token, both from the database:

```sh
psql "$PGURL" -c "SELECT singleton, armed_at, quiesce_until, enabled FROM farm.reaper_state"
psql "$PGURL" -c "
SELECT ended_at, lease_id, job_id, release_reason, heartbeat_age_s
  FROM farm.v_lease_endings
 WHERE release_reason = 'holder_expired' AND ended_at > now() - interval '6 hours'
 ORDER BY ended_at DESC"
```

`quiesce_until` should be in the future by roughly the longest live TTL. If the
endings listing returns rows whose `ended_at` sits inside or just after a recorded
gap, **the refund did not protect them** — that is a real incident, and it is
[lease-reclaimed.md](lease-reclaimed.md), not this file.

## Then find the cause

The gap is a symptom; the component is the story.

```sh
kubectl -n <ns> get pods -l app.kubernetes.io/part-of=device-farmer
kubectl -n <ns> logs deploy/<release>-<component> --previous --tail=200
kubectl -n <ns> get events --sort-by=.lastTimestamp | tail -30
```

Usual causes, in order of how often they turn out to be it:

1. **A rolling deploy or node drain** that took the last replica of a
   single-replica role. `reaper`, `scheduler` and `janitor` are leader-elected
   and normally run one replica; there is no PodDisruptionBudget shielding them
   because a second replica would just stand by.
2. **Postgres unavailable** — a failover, a connection-limit ceiling, a
   `statement_timeout`. Every role beats through the same pool.
3. **Pool exhaustion in one role.** `config.db.maxConns` below 7 quietly stops
   the jobrunner picking up work; below 2 the leader-elected roles deadlock on
   their own advisory-lock session. The chart refuses below 2 at render time and
   warns below 7.
4. **OOMKill.** Check `lastState.terminated.reason` on the pod.

## When to escalate

- **`DeviceFarmerControlPlaneGapBudget` firing repeatedly.** Ten minutes of
  refunded deadline an hour, week after week, is a control plane that keeps
  falling over — and every refunded second is a device held for longer than
  anybody chose. That is a capacity and reliability problem worth an engineer.
- **A gap larger than six hours.** `farm.lease_reclaim` only considers gaps
  ended within the last six hours when deciding what to shield. Past that
  window, a recorded gap no longer protects any lease from reclamation.
- **Any `holder_expired` reclaim inside the gap window.** Straight to
  [lease-reclaimed.md](lease-reclaimed.md); the invariant failed.

## What NOT to do

Do not touch `farm.reaper_state.quiesce_until` to "get the reaper going again".
That delay is derived from the longest TTL the reaper could have missed, and
shortening it is precisely the change that turns a control-plane restart into a
fleet-wide reclaim.
