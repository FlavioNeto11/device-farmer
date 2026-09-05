# DeviceFarmerReaperNotLeading / DeviceFarmerSchedulerNotLeading

**Severity:** critical · **Group:** `device-farmer.roles` (off by default)

```promql
sum(farm_reaper_leader)    != 1    for 10m
sum(farm_scheduler_leader) != 1    for 10m
```

## What fired

No replica of the reaper (or the scheduler) holds its leader-election lock.

Both gauges sum to exactly 1 in a healthy farm. Leadership is a Postgres
`pg_try_advisory_lock` held on a dedicated session, so **two leaders is not
possible** — in practice this alert only ever means zero.

## What it means

### Reaper

The reaper is the **only automatic release path in the system**. With no leader:

- nothing marks an expired lease `suspect`;
- nothing reclaims a lease whose holder is gone — an abandoned device is held
  forever;
- `farm.reaper_arm` never runs, so no control-plane gap is detected and no
  refund is applied;
- `farm.lease_expire_max_runtime` never runs, so a deadline the *user* wrote
  down is not enforced.

**Nothing is lost.** Live jobs keep running, leases keep renewing, the API keeps
serving. Devices are simply never handed back. The farm slowly fills up with
phones nobody is using, and the first visible symptom is usually a queue that
stops moving because there is nothing free to place onto.

### Scheduler

Queued jobs are never placed. Running jobs are untouched and no lease is
affected. Less urgent than the reaper — the damage is a stalled queue, not a
shrinking pool — but the causes are the same.

## What is NOT wrong

- **A rolling deploy.** Leadership moves. There is a window with zero leaders
  while the old holder's session closes and the new pod takes the lock; the
  10-minute `for:` is sized well past it.
- **`replicaCount: 1`.** Normal for these roles — a second replica only stands
  by. It does mean every restart is a leadership gap.
- **A standby replica reporting 0.** Expected. The alert is on the *sum*.
- **Devices sitting in `suspect`.** Suspect releases nothing and heals itself on
  the next heartbeat.

## First three checks

**1. Is the process there, and does it think it is leading?**

```sh
kubectl -n <ns> get pods -l app.kubernetes.io/component=reaper
kubectl -n <ns> logs deploy/<release>-reaper --tail=200 | grep -i -E 'leader|advisory|arm|quiesce'
psql "$PGURL" -c "
SELECT component, now() - beat_at AS age FROM farm.component_heartbeat
 WHERE component IN ('reaper','scheduler')"
```

A pod that is `Running` and beating but never logs acquiring leadership is stuck
trying to take the lock.

**2. Who holds the advisory lock, and is the pool big enough to use it?** This
is the classic self-inflicted deadlock: the leader pins **one connection for the
entire life of the process** — the session carrying the lock — so with a pool of
one, the elected leader has nothing left to work with. It logs that it took
leadership and then reclaims nothing, forever, with no error anywhere.

```sh
psql "$PGURL" -c "
SELECT l.locktype, l.objid, l.granted, a.pid, a.application_name,
       a.client_addr, a.state, now() - a.state_change AS since
  FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE l.locktype = 'advisory'"
kubectl -n <ns> get cm <release>-config -o jsonpath='{.data.FARM_DB_MAX_CONNS}{"\n"}'
```

The chart refuses `config.db.maxConns` below 2 at render time for exactly this
reason, but a pod running with an overridden environment can still get there.

A lock held by a `pid` whose `state_change` is hours old and whose pod no longer
exists is a **stale session**: the old leader's TCP connection was never torn
down, so Postgres still thinks it is alive and the new pod cannot take the lock.

**3. What has it cost so far?**

```sh
psql "$PGURL" -c "SELECT singleton, armed_at, quiesce_until, enabled FROM farm.reaper_state"
psql "$PGURL" -c "
SELECT state, count(*), min(expires_at) AS oldest_deadline
  FROM farm.leases WHERE state IN ('held','suspect') GROUP BY state"
psql "$PGURL" -c "
SELECT count(*) FROM farm.leases WHERE state = 'suspect' AND reclaimable_at < now()"
```

That last count is leases that *should* have been reclaimed and were not. It is
the size of the backlog waiting for the reaper to come back.

Also check `reaper_state.enabled`. If it is `false`, somebody turned the reaper
off deliberately; find out who and why before turning it back on.

## What to do

- **Pod not running** → get it running. Ordinary Kubernetes work.
- **Pool too small** → raise `config.db.maxConns` to at least 2 (7 if this pod
  also runs a jobrunner) and roll the deployment.
- **Stale advisory lock** → terminate the orphaned backend:
  ```sh
  psql "$PGURL" -c "SELECT pg_terminate_backend(<pid>)"
  ```
  Confirm the pod that owned it is really gone first. Terminating the *live*
  leader's session just moves leadership, which is harmless but pointless.
- **`reaper_state.enabled = false`** → understand why before changing it.

## What NOT to do

**Do not shorten `quiesce_until` to make the reaper "catch up" faster.** When
the reaper regains leadership it arms, refunds the gap, and then deliberately
stands down for the longest TTL it could have missed. That delay is what stops a
restored reaper from mass-reclaiming every lease that went suspect while it was
blind. Removing it is how a control-plane restart destroys a whole farm's work
in one sweep — which is the failure this entire system is built around
preventing.

The backlog will drain on its own once the quiesce window passes. Wait.

## When to escalate

- **Leases past `reclaimable_at` in significant numbers.** The pool is
  effectively shrinking; capacity planning and tenant communication follow.
- **The lock cannot be taken even with no competing holder.** That is a Postgres
  problem, not a device-farmer one.
- **Both reaper and scheduler at once.** Look one layer down —
  [component-beat-failing.md](component-beat-failing.md) and the database.
