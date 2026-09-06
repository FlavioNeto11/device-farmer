# DeviceFarmerLeaseReclaimed

**Severity:** critical · **Group:** `device-farmer.lost-work`

```promql
sum(increase(farm_lease_reaped_total{reason="holder_expired"}[15m])) > 0
```

## What fired

`farm.lease_reclaim` ended one or more leases because their holders stopped
renewing for `ttl + grace`, measured across every control-plane component.

## What it means

Work was destroyed, and we destroyed it.

`holder_expired` is the only one of the seven release reasons that means this.
`completed`, `failed`, `job_cancelled`, `max_runtime`, `operator_revoked` and
`device_retired` all describe work that ended — by its own hand, by a deadline
the user wrote down, or by a person. `holder_expired` describes work that was
still going as far as anybody knew, whose supervisor went quiet long enough that
the control plane took the phone back.

In a healthy farm this counter is flat at zero. It is pre-created at zero on
process start precisely so that this rule is armed from the first scrape rather
than from the first casualty.

**This is a tombstone.** The devices are already back in the pool, the fences
are already burned, and nothing you do now un-destroys the run. The job of this
page is to find out why, before it happens to twenty leases instead of one.

## What is NOT wrong

- **The reclaim itself.** It is the deadline working. `ttl + grace` is the only
  automatic release deadline in the system that a user did not write down, and
  it exists so a dead supervisor cannot hold a phone forever.
- **A single reclaim after a genuinely dead holder.** One CI pod OOM-killed
  mid-run, missed its whole grace band, and lost its device. Sad, correct, and
  not an incident — unless it is the third one this week from the same tenant.
- **`reason="max_runtime"` alongside it.** That is a deadline the *user* wrote
  down in `farm.jobs.max_runtime`. Different thing entirely; it does not fire
  this alert.

## What IS wrong, in descending order of how bad it is

1. **A control-plane gap that was not refunded.** If the API was down, holders
   could not renew, and the reclaim charged them for our outage. This is the
   failure `farm.reaper_arm` exists to prevent, and it means the gap accounting
   did not do its job.
2. **A reaper that came up and swept before arming.** The reaper stands down
   rather than sweep unarmed, so this should be impossible — but it is worth
   confirming rather than assuming.
3. **TTL and grace too tight for the workload.** A job that legitimately goes
   quiet for 45 minutes under a 15m/30m TTL/grace will be reclaimed every time.
4. **A genuinely dead holder.** The ordinary case.

## First three checks

**1. What exactly was ended, and how silent was it?**

```sh
farmd ctl endings --ended-by reaper --limit 20
```

That reads `farm.v_lease_endings` over the API, so it needs a token and not a
database session. Without one, the same rows:

```sh
psql "$PGURL" -c "
SELECT ended_at, lease_id, job_id, tenant_id, holder, protected,
       held_seconds, heartbeat_age_s, ended_by
  FROM farm.v_lease_endings
 WHERE release_reason = 'holder_expired'
 ORDER BY ended_at DESC LIMIT 20"
```

`heartbeat_age_s` — the `BEAT AGE` column — is the number that matters. Compare it to `ttl + grace`
(defaults: 15m + 30m = 2700s). An age barely over the threshold means the
deadline was too tight or the outage was ours. An age of many hours means the
holder really was gone.

**2. Was the control plane up for the whole silence?**

```sh
psql "$PGURL" -c "
SELECT component, started_at, ended_at, ended_at - started_at AS gap
  FROM farm.control_plane_gap
 WHERE ended_at > now() - interval '6 hours'
 ORDER BY started_at DESC"
psql "$PGURL" -c "SELECT * FROM farm.reaper_state"
```

If a gap overlaps the reclaimed leases' silence and they were reclaimed anyway,
**stop and escalate**: the refund did not protect the leases it was supposed to
protect, and every long-running job in the farm is exposed to the same thing.
`reaper_state.quiesce_until` should have been in the future for the longest TTL
after the gap ended.

**3. Who was holding them, and are they still failing?**

```sh
farmd ctl endings --ended-by reaper --since 24h --limit 200
farmd ctl jobs --state failed
```

The `by holder:` line under the table is the count per holder **over the rows
listed**, not over the whole 24 hours. If `stderr` says the listing was cut,
those counts are a sample and this judgement — one holder, or many — is exactly
the one a sample gets wrong; raise `--limit` until the warning stops, or use the
SQL below, which groups over the window. To confirm a single holder,
`farmd ctl endings --ended-by reaper --since 24h --holder <name>`. Without a
token:

```sh
psql "$PGURL" -c "
SELECT holder, count(*), max(ended_at)
  FROM farm.v_lease_endings
 WHERE release_reason = 'holder_expired' AND ended_at > now() - interval '24 hours'
 GROUP BY holder ORDER BY 2 DESC"
```

One holder dominating the list is a broken supervisor. Many holders means it is
us.

## When to escalate

- **Immediately, if a `control_plane_gap` row overlaps the silence.** Leases
  were charged for our downtime. That is the core invariant of this system
  failing, and it needs an engineer, not an operator.
- **More than a few in one sweep.** `farm.lease_reclaim` is `LIMIT`-bounded and
  uses `SKIP LOCKED`, so a batch of reclaims is a batch of genuinely silent
  holders — which almost never happens for unrelated reasons at the same moment.
- **Any `protected` lease appears in the list.** It should be impossible: the
  reclaim sweep skips protected leases. If one is there, the skip is broken and
  every protected lease in the farm is at risk.

## Prevention, once the incident is over

- If `heartbeat_age_s` is clustered just over the threshold, raise
  `FARM_LEASE_TTL` / `FARM_LEASE_GRACE` (chart: `config.lease.ttl`,
  `config.lease.grace`) or have the tenant renew more often.
- If a specific job's work is expensive enough that it should never be discarded
  automatically, the tenant should submit it `protected`. It will then hold and
  page ([protected-lease-suspect.md](protected-lease-suspect.md)) instead of
  being reclaimed.
