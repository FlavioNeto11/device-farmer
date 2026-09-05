# DeviceFarmerAPIDown

**Severity:** critical · **Group:** `device-farmer.control-plane`

```promql
up{job="<release>-api"} == 0 or absent(up{job="<release>-api"})    for 5m
```

## What fired

Prometheus cannot scrape the device-farmer API, or the target has disappeared
from service discovery entirely.

## What it means

Read this first, before doing anything fast:

**No lease ends because the API is down. No device is reallocated. No job is
aborted.** Holders cannot renew, so they burn TTL — and `farm.reaper_arm` adds
every second of that back to `expires_at` and `reclaimable_at` on every live
lease the moment the reaper next arms. The tenant pays nothing for our outage.

So the urgency here is about the farm being unusable, not about work being
destroyed. Do not take shortcuts that risk the database to bring the API back a
minute sooner.

Two secondary effects do matter:

- **Every other device-farmer alert is blind.** `templates/servicemonitor.yaml`
  scrapes the api Service and nothing else, so with the API down there is no
  device health, no lease census and no recovery data reaching Prometheus.
  Absence of other pages right now means nothing.
- **The API is in `FARM_REAPER_COMPONENTS` by default.** Its silence is
  accumulating a control-plane gap that will be refunded onto every live lease.
  Expect [control-plane-gap.md](control-plane-gap.md) to fire afterwards. That is
  the system working.

## What is NOT wrong

- **A rolling deploy.** With `api.replicaCount: 2` and a PodDisruptionBudget,
  one replica going down does not fire this — the alert is per target, so a
  single replica restarting fires for its own `instance` and clears. The 5m
  `for:` should swallow a normal rollout; a slow image pull can outlast it.
- **The dashboard being dark.** Cosmetic. It is served by the same process.
- **`farm_lease_suspect` climbing.** Holders that cannot renew get marked
  suspect. Marking a lease suspect **releases nothing**, and a heartbeat inside
  the grace band heals it at the same fence once the API returns.

## The false alarm worth knowing about

If this fires immediately after installing the chart and never clears, check
`prometheusRule.apiJob` before looking at the API. The rule matches on the `job`
label, which defaults to the api Service name; if your scrape config assigns
something else, the `absent()` half fires permanently. That is deliberate — a
selector that matches nothing announces itself rather than disarming the rule in
silence — but it is a configuration bug, not an outage.

```sh
# What job label does Prometheus actually use?
curl -s "$PROM_URL/api/v1/query?query=up" | jq -r '.data.result[].metric.job' | sort -u
```

## First three checks

**1. Is it the pod, or the scrape?**

```sh
kubectl -n <ns> get pods -l app.kubernetes.io/component=api
kubectl -n <ns> get endpoints <release>-api
kubectl -n <ns> logs -l app.kubernetes.io/component=api --tail=100
```

A `Running` pod with an empty Endpoints list is a readiness or selector problem,
not a crash. A `CrashLoopBackOff` gives you the reason in the logs — and farmd
refuses to start on a bad configuration rather than serving a broken farm, so
the message is usually a complete sentence naming the environment variable.

**2. Is it the database?** Everything in this control plane lives in one
Postgres; farmd verifies the connection at startup and will not boot without it.

```sh
kubectl -n <ns> logs -l app.kubernetes.io/component=api --tail=50 | grep -i -E 'connect|ping|dsn|database'
psql "$PGURL" -c "SELECT now()"
psql "$PGURL" -c "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()"
```

A connection count sitting at the server's `max_connections` is the classic
one — `config.db.maxConns` multiplied by every replica of every role.

**3. Is anything else still alive?** The rest of the control plane does not go
through the API; the reaper and scheduler talk to Postgres directly, so a dead
API does not stop reclaims.

```sh
psql "$PGURL" -c "
SELECT component, now() - beat_at AS age FROM farm.component_heartbeat ORDER BY age DESC"
```

If every component is stale, the problem is Postgres or the network to it, not
the API.

## While it is down

Nothing needs to be released, drained or revoked. Live jobs on phones keep
running; they will fail their renewals with `kind="transient"` (which proves
nothing and aborts nothing) and pick up where they left off.

`farmd ctl` is useless while the API is down — it is a pure HTTP client. Use
`psql` for read-only visibility.

## When to escalate

- **Postgres is unreachable.** That is a database incident, and every role in
  the farm is stopped behind it.
- **The API is crash-looping on a configuration error you did not make.** Check
  whether a Helm upgrade just landed; the chart refuses several
  half-configured combinations at render time, but not all of them.
- **It has been down long enough that jobs are approaching `max_runtime`.**
  `max_runtime` is a deadline the *user* wrote down and it is **not** refunded by
  the gap accounting — only TTL is. Long jobs can genuinely time out during a
  long API outage.
