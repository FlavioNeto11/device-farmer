# DeviceFarmerComponentBeatFailing

**Severity:** warning · **Group:** `device-farmer.roles` (off by default)

```promql
sum by (job, instance) (
  rate({__name__=~"farm_(reaper|scheduler|jobrunner|recovery|watchdog|janitor|node)_heartbeat_failures_total"}[10m])
) > 0                                                                    for 10m
```

## What fired

A control-plane role cannot write its row in `farm.component_heartbeat`.
`farm.component_beat` is failing.

## What it means

This is the **in-progress** form of a control-plane gap. It is the only alert in
the set that catches a component going quiet *while it is happening* rather than
after the reaper has discovered and refunded it.

The mechanism matters:

```sql
SELECT min(h.beat_at) FROM farm.component_heartbeat h
 WHERE h.component = ANY (p_components);
```

`farm.reaper_arm` takes the **oldest** beat across `FARM_REAPER_COMPONENTS`
(`reaper`, `api`, `scheduler` by default) and, if it is older than
`FARM_REAPER_GAP_FLOOR`, adds the whole interval back to `expires_at` and
`reclaimable_at` on **every live lease in the farm**.

So a single role in that list, silently failing to beat, is quietly extending
every tenant's deadline. Nothing is destroyed — the refund is in the safe
direction — but a device that should have been reclaimed an hour ago is still
held, and the effect scales with the whole fleet rather than with the broken
component.

## Which roles matter, and how much

| Role | In `FARM_REAPER_COMPONENTS`? | What its silence costs |
| --- | --- | --- |
| `reaper` | yes | Refunds every lease; also nothing is reclaimed at all |
| `api` | yes | Refunds every lease; holders cannot renew |
| `scheduler` | yes | Refunds every lease; nothing is placed |
| `watchdog` | **no, deliberately** | Health goes stale. Never moves a lease clock |
| `recovery` | **no** | Stuck devices stay stuck |
| `jobrunner` | **no** | Placed jobs are not picked up |
| `janitor`, `node` | **no** | Housekeeping; host-side actions |

The absences are the design, not an oversight. Listing `watchdog` in
`FARM_REAPER_COMPONENTS` would let device health move a lease deadline, fusing
two clocks this system keeps apart on purpose — and the Postgres role firewall
backs it up: the watchdog may not touch `farm.leases` at all.

## What is NOT wrong

- **A brief spike during a Postgres failover.** `component_beat` is a single
  `INSERT ... ON CONFLICT`; it fails while the database is unreachable and
  succeeds again immediately after. The 10-minute `for:` covers a failover.
- **The role itself being dead.** If the process is gone it writes no metrics at
  all, so this alert goes *quiet* rather than firing. Absence here is not
  reassurance — check `farm.component_heartbeat` ages, not this counter.
- **A `watchdog:<host>` component being stale.** Watchdogs beat per host, and a
  host being down does not extend anyone's lease.

## First three checks

**1. How stale is each component, and is anything already being refunded?**

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.roles'
psql "$PGURL" -c "
SELECT component, beat_at, now() - beat_at AS age
  FROM farm.component_heartbeat ORDER BY age DESC"
psql "$PGURL" -c "
SELECT component, started_at, ended_at, ended_at - started_at AS gap
  FROM farm.control_plane_gap ORDER BY started_at DESC LIMIT 5"
```

`FARM_HEARTBEAT_INTERVAL` defaults to 5s and `FARM_REAPER_GAP_FLOOR` to 60s, so
an age above a minute on `api`, `scheduler` or `reaper` means the next arm will
record a gap for the whole fleet.

**2. Is it the database, or this one role?**

```sh
psql "$PGURL" -c "SELECT now()"
psql "$PGURL" -c "
SELECT count(*) AS conns, max(now() - state_change) AS idlest
  FROM pg_stat_activity WHERE datname = current_database()"
psql "$PGURL" -c "SHOW max_connections"
kubectl -n <ns> logs deploy/<release>-<role> --tail=200 | grep -i -E 'beat|component_beat|pool|connect'
```

Every role beats through the same pool, so a connection ceiling hits all of them
at once. One role failing alone points at that role's pool: `config.db.maxConns`
is per pod, and the jobrunner wants at least 7 (one permanent claim session plus
one renewal per concurrent job).

**3. Is the role otherwise doing its work?** A role that beats but does nothing
is a different and worse problem than one that works but cannot beat.

```sh
psql "$PGURL" -c "SELECT singleton, armed_at, quiesce_until, enabled FROM farm.reaper_state"
farmd ctl jobs --state queued
farmd ctl leases --state suspect
```

A growing queue with a beating scheduler means leadership, not heartbeats — see
[reaper-not-leading.md](reaper-not-leading.md).

## What to do

- **Database unreachable** → that is the incident. Everything else here is a
  symptom.
- **Connection ceiling** → raise `config.db.maxConns`, or Postgres's
  `max_connections`, or reduce replicas. Count it out: every replica of every
  role holds up to `maxConns`.
- **One role, persistently** → restart it. Restarting a role releases no lease;
  SIGTERM drains, it does not release. Expect
  [control-plane-gap.md](control-plane-gap.md) afterwards when the reaper arms
  and refunds the gap. That is the system working.

## When to escalate

- **`reaper`, `api` or `scheduler` stale for longer than the shortest live
  TTL.** Leases are being marked suspect for reasons that are entirely ours.
  They will be refunded, but the farm is not placing or reclaiming work.
- **The failure count is climbing but ages are fine.** Some beats are landing
  and some are not — an intermittent database or network problem, which is
  harder to see and worth an engineer.
- **This is firing at all with `prometheusRule.roleScrape: false`.** It should
  be impossible; the rule is not rendered. Somebody has a stale rule object in
  the cluster.
