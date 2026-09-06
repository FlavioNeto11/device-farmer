# DeviceFarmerLeaseRenewalsFailing

**Severity:** warning · **Group:** `device-farmer.roles` (off by default)

```promql
sum by (job, instance) (
  rate(farm_jobrunner_renewals_total{outcome="transient"}[6m])
) > 0
unless
sum by (job, instance) (
  rate(farm_jobrunner_renewals_total{outcome=~"ok|self_healed"}[6m])
) > 0                                                                     for 4m
```

## What fired

One jobrunner replica has been attempting lease renewals and **not one of them
has landed** for the length of the window. `farm.lease_renew` is being called
and is not answering.

The window and the `for:` add up, so this is roughly **ten minutes** after the
last renewal that landed on this replica.

## What it means

**Nothing has been lost, and nothing is being taken away.** This is the only
alert in the set that fires while the work can still be saved. Everything else
about a lease going wrong — [lease-reclaimed.md](lease-reclaimed.md),
[lease-fenced.md](lease-fenced.md) — is a tombstone written after the fact.

A failed renewal proves nothing about a lease. `internal/lease` classifies zero
rows from `farm.lease_renew` as **fencing** and every other failure as
**transient**, and a transient failure leaves the lease exactly as it was: the
deadline has not moved, the device is still the job's, the fence is unchanged,
and the holder retries on a jittered backoff without cancelling anything. That
classification is the whole design; getting it backwards is DeviceFarmer/STF
#663 rebuilt on the control plane instead of on an ADB socket.

So what this alert reports is a **budget being spent**, not work being
destroyed. Each affected lease is now living on:

1. `ttl + grace`, counted from its last successful renewal. **45 minutes** at
   this chart's defaults (`FARM_LEASE_TTL` 15m, `FARM_LEASE_GRACE` 30m) and
   never less than 15, because `farm.jobs` CHECKs a floor of 10m and 5m. At the
   shipped 90s renewal interval that is ten consecutive failed renewals before
   a default lease is even marked *suspect* — and suspect releases nothing; a
   single landed renewal inside the grace band heals it at the same fence.
2. **The control-plane gap refund.** If the jobrunner is in
   `FARM_REAPER_COMPONENTS` — it is, by default — its silence is recorded by
   `farm.reaper_arm` and added back to `expires_at` and `reclaimable_at` on
   every live lease. You are not charged for our outage.
3. **The witness.** Every placement writes a marker on its own device and
   presents it as evidence; `farm.lease_reclaim` skips any lease whose
   `witness_at` is younger than one grace period. A job that can still prove it
   is working keeps its device even while its heartbeats are not landing.

You have all three. Use the time.

## What is NOT wrong

- **The holders.** They are doing the correct thing: retrying, holding, and not
  cancelling. Do not restart them to "clear" this alert — see below.
- **The jobs.** They are still running on their devices. A renewal is a question
  about the *supervisor process*, never about the device or the work.
- **`farm_jobrunner_renewals_total{outcome="transient"}` climbing on its own.**
  That alone is not this alert and is deliberately not alertable. A database
  blip is a database blip. This rule fires only when the successes have gone to
  **zero**, which is a renewal path that is down rather than one that stuttered.
- **A replica with no leases.** It attempts no renewals, so it cannot fire this.
  Silence here from an idle replica is not coverage — check
  `farm_jobrunner_running`.
- **`outcome="self_healed"` appearing.** That is a renewal landing on a lease
  the sweeper had already marked suspect. It means this incident is *ending*
  with nothing lost, and it disarms this rule.

## The one thing not to do

**Do not restart the jobrunner to make it go away.** It will not help and it
costs something:

- SIGTERM stops renewal *without releasing*, which is correct — the lease, the
  device and the fence survive the process and the replacement re-attaches at
  the same fence. So a restart does not free anything.
- But the replacement starts renewing from wherever the pool is, and if the
  pool is the problem it inherits it, having thrown away the checkpointed
  progress of whatever step was mid-flight.
- And two restarts in quick succession are two more control-plane gaps to
  refund, which is [control-plane-gap.md](control-plane-gap.md)'s subject.

Restart only after you know the cause is process-local — a wedged pool, a
goroutine leak — and know that restarting fixes it.

## First three checks

**1. Is it this replica, or is it Postgres?** The instance label in the alert
names one pod. Ask whether the others are renewing.

```sh
kubectl -n <ns> get pods -l app.kubernetes.io/component=jobrunner
psql "$PGURL" -c "SELECT now()"
psql "$PGURL" -c "
SELECT component, beat_at, now() - beat_at AS age
  FROM farm.component_heartbeat ORDER BY age DESC"
```

If every jobrunner pod is firing and `farm.component_heartbeat` is stale across
the board, this is a database or network incident and everything below is a
symptom of it. If exactly one pod is firing while the others are fine, it is
that pod's pool.

**2. Pool exhaustion is the usual single-pod cause.** The jobrunner needs one
permanent claim session plus one renewal connection per concurrent job.

```sh
psql "$PGURL" -c "
SELECT count(*) AS conns, state, max(now() - state_change) AS idlest
  FROM pg_stat_activity WHERE datname = current_database()
 GROUP BY state ORDER BY conns DESC"
psql "$PGURL" -c "SHOW max_connections"
kubectl -n <ns> logs <pod> --tail=200 | grep -i 'lease renewal failed'
```

The holder's own log line carries everything the metric cannot: the lease, the
device, the job, the fence, the consecutive-failure count, the backoff, and the
`expires_at` it is spending. `FARM_DB_MAX_CONNS` is per pod; count it out
against `max_connections` across every replica of every role.

**3. How much budget is actually left?** This is the number that decides whether
you have an hour or ten minutes.

```sh
farmd ctl leases --state suspect
psql "$PGURL" -c "
SELECT id, job_id, device_id, state,
       now() - heartbeat_at AS silent_for,
       expires_at - now()   AS ttl_left,
       reclaimable_at - now() AS until_reclaimable,
       witness_at, witness_extensions, protected
  FROM farm.leases
 WHERE released_at IS NULL
 ORDER BY reclaimable_at"
curl -s -H "Authorization: Bearer $FARM_API_TOKEN" "$FARM_API_URL/api/v1/reaper" \
  | jq '{armed, enabled, suspect_leases, reclaimable_now}'
```

`until_reclaimable` in the future on every row means nothing is at risk yet. A
non-null `witness_at` inside one grace period means that lease is covered by
device-side evidence regardless of what the renewals do. `reclaimable_now: 0`
means the sweep, right now, would take nothing.

## Reading the counter

```promql
sum by (instance) (rate(farm_jobrunner_renewals_total[5m]))            # attempts
sum by (instance, outcome) (rate(farm_jobrunner_renewals_total[5m]))   # split
farm_jobrunner_witness_total{outcome="accepted"}                       # what is carrying them
farm_lease_renew_failures_total{kind="fenced"}                         # must stay flat
```

| `outcome` | Means |
| --- | --- |
| `ok` | The renewal landed. |
| `self_healed` | It landed on a lease the sweeper had marked suspect. An incident ended with no work lost and no fence moved. |
| `transient` | The call did not complete. Proves **nothing** about the lease; retried on a backoff. |
| `fenced` | `farm.lease_renew` returned zero rows. The lease is gone and the job aborted — a different alert, [lease-fenced.md](lease-fenced.md). |

`transient` and `fenced` are counted apart here and again, fleet-wide, in
`farm_lease_renew_failures_total{kind}`. `internal/lease` makes the distinction
once and nothing downstream re-decides it. **If `fenced` is climbing during
this incident, this is not the page you are on** — stop here and go to
[lease-fenced.md](lease-fenced.md).

## What to do

- **Database unreachable or overloaded** → that is the incident. Fix it; the
  holders re-converge on their own and you will see `outcome="self_healed"` for
  any lease that reached suspect. Expect
  [control-plane-gap.md](control-plane-gap.md) afterwards when the reaper arms
  and refunds the outage. That is the system working.
- **Connection ceiling** → raise `FARM_DB_MAX_CONNS` (`config.db.maxConns`), or
  Postgres's `max_connections`, or lower `concurrency` per jobrunner replica.
- **One pod, persistently, with the database healthy** → restart *that* pod,
  having read the section above on why that is not the first move.
- **A lease you are about to lose anyway** → there is deliberately no operator
  action that extends one. Deadlines are the server's, computed against
  Postgres' `now()`, and nothing outside the database moves them. The only
  in-flight lever is the reaper's kill switch:

  ```sh
  farmd ctl reaper disable --reason "renewals down on jobrunner-1, holding leases"
  ```

  It stops `farm.lease_reclaim` for the **whole farm** and nothing else — jobs
  still release their own leases, `max_runtime` still expires them, an operator
  revoke still works. Re-enabling is not instant: `farm.reaper_arm` runs,
  refunds the outage to every live lease and quiesces, which is the point.
  Treat it as a written-down decision, not a reflex.

  A job whose work must never be reclaimed is submitted `protected`, which
  `farm.lease_reclaim` skips permanently — but that is a property of the job
  and cannot be granted to a lease that is already in trouble. See
  [protected-lease-suspect.md](protected-lease-suspect.md).

## When to escalate

- **`reclaimable_now` above 0 in `GET /api/v1/reaper`.** The sweep would now
  take a lease. Everything above has run out. This becomes
  [lease-reclaimed.md](lease-reclaimed.md) within one reaper interval.
- **`farm_jobrunner_witness_total{outcome="skipped"}` climbing alongside.** The
  renewals are failing *and* the placements cannot reach their devices to
  refresh evidence. Both of a lease's ways of saying "I am still here" are gone
  and only `ttl + grace` and the gap refund are left.
- **Every jobrunner replica firing at once for longer than the shortest live
  TTL.** The farm is one refused gap refund away from mass reclamation. Confirm
  the reaper is armed and that `farm.control_plane_gap` is recording, and if it
  is not, `farmd ctl reaper disable` buys time at the cost of nothing being
  reclaimed at all. That is a deliberate, reversible decision and it must be
  written down.
- **This is firing at all with `prometheusRule.roleScrape: false`.** It should
  be impossible; the rule is not rendered. Somebody has a stale rule object in
  the cluster.
