# DeviceFarmerAlertingBlind

**Severity:** warning · **Group:** `device-farmer.coverage`

```promql
(count(farm_device_health{host!="unknown"}) or vector(0)) == 0
  and
(count(farm_api_auth_open) or vector(0)) > 0                    for 1h
```

## What fired

Prometheus is scraping a device-farmer API (`farm_api_*` is present), but
`farm_device_health` contains only its zero-filled placeholder series — the one
with `host="unknown"`. Nobody is publishing a real device census into anything
Prometheus can see.

## What it means

**Several of the pages in this rule set cannot fire.** Not "are quiet" — cannot
fire, structurally, no matter what happens to the farm.

`internal/obs` is registered in every farmd process, but each of its gauges is
*published* by exactly one role:

| Series | Published by |
| --- | --- |
| `farm_device_health` | watchdog |
| `farm_lease_held`, `farm_lease_suspect`, `farm_slot_rearm_pending` | reaper |
| `farm_control_plane_gap_seconds` | reaper |
| `farm_recovery_attempts_total` | recovery |
| `farm_lease_reaped_total`, `farm_lease_renew_failures_total` | api, jobrunner, scheduler, reaper |
| `farm_api_*` | api |

The chart's `ServiceMonitor` scrapes the **api Service and nothing else**,
because the other roles serve no HTTP: `FARM_METRICS_ADDR` is read by
`internal/config` and listened on by nobody. In a split deployment — this
chart's default, one Deployment per role — the api's registry contains those
gauges but never publishes into them, so they sit at their zero-filled seeds
forever and every alert over them evaluates to a permanent "no".

That is why this rule exists and why it uses the watchdog's census as its
witness. `obs.SetDeviceHealth` publishes one row per `(host, hub)`
unconditionally, every cycle, on any farm with a device in it; `obs.zeroFill`
seeds exactly one series with `host="unknown"`. So "no series with a real host"
is a reliable signal that nobody is publishing.

Silence in the other alerts right now means nothing at all. This alert is what
stops that silence being mistaken for good news.

## The legitimate false alarm

**A farm that has been installed but has no devices enrolled yet.** The watchdog
has nothing to publish, so the census is genuinely empty and this fires. That is
the reason for the 1-hour `for:` — long enough for a first enrollment cycle,
short enough to find a genuinely blind farm the same day.

Check it in one command before doing anything else:

```sh
farmd ctl fleet
psql "$PGURL" -c "SELECT count(*) FROM farm.devices"
```

Zero devices → the alert is correct and harmless. Silence it until the rack is
plugged in.

## What is NOT wrong

- **The api.** It is being scraped; that is half the rule's condition.
- **The watchdog necessarily being down.** It may be running perfectly and
  simply not scraped. Check `farm.component_heartbeat` before restarting
  anything — see below.
- **The `host="unknown"` series existing.** That is `obs.zeroFill` doing its job.
  A gauge that springs into existence at the moment it first goes bad is a gauge
  whose alert was armed by the incident it was supposed to warn about.

## First three checks

**1. Are the roles actually running?** This distinguishes "not scraped" from
"not running", and they need completely different fixes.

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.roles'
psql "$PGURL" -c "
SELECT component, beat_at, now() - beat_at AS age
  FROM farm.component_heartbeat ORDER BY age DESC"
```

Fresh heartbeats from `watchdog`, `reaper` and `recovery` mean the farm is
healthy and only the *observability* is broken. Stale ones mean go to
[control-plane-gap.md](control-plane-gap.md) instead.

**2. What is Prometheus actually scraping?**

```sh
kubectl -n <ns> get servicemonitor -l app.kubernetes.io/part-of=device-farmer
kubectl -n <ns> get svc -l app.kubernetes.io/part-of=device-farmer
curl -s "$PROM_URL/api/v1/targets?state=active" \
  | jq -r '.data.activeTargets[] | select(.labels.job|test("device-farmer|-api")) | "\(.labels.job) \(.labels.instance) \(.health)"'
```

Exactly one job, pointing at the api Service, is the expected — and insufficient
— picture.

**3. Confirm the seeds are all that is there.**

```sh
curl -s "$PROM_URL/api/v1/query?query=count%20by%20(host)(farm_device_health)" | jq '.data.result'
```

A single result with `host="unknown"` is this alert, verbatim.

## The fixes, in order of how much they cost

**1. Run one process with every role in it.** `farmd all` registers everything
into one registry, and the api serves it. The whole rule set then works with the
ServiceMonitor exactly as shipped. This is the right answer for small and
mid-size farms and costs nothing but a values change; it gives up independent
scaling and independent restarts per role.

**2. Scrape the other pods.** They do not serve HTTP today, so this needs the
non-api roles to listen on `FARM_METRICS_ADDR` and a `ServiceMonitor` (or a
`PodMonitor`) per role. Until that lands in the binary, this option does not
exist — and the honest thing is to say so rather than to add a ServiceMonitor
pointing at a port nothing is bound to.

**3. Accept it, deliberately and in writing.** If the farm is small enough that
a human looks at the dashboard daily, turn off the alerts that cannot fire
rather than leaving them installed and dead. A rule that returns no data reads
as coverage on a status page and is not.

Whatever you choose, do not silence *this* alert while leaving the others
enabled. That converts a known gap into an unknown one.

## Also check, while you are here

The same class of gap exists inside the process. `cmd/farmd` calls
`obs.RegisterAll(reg, log)` — and if it is called without each role's
`Collectors()`, nine of the ten packages that export them register nowhere, and
every `farm_reaper_*`, `farm_scheduler_*`, `farm_jobrunner_*` and
`farm_watchdog_*` series is absent regardless of scraping. The startup log says
so plainly:

```sh
kubectl -n <ns> logs deploy/<release>-api | grep -i -E 'metrics registered|could not be registered|gaps'
```

`farm metrics registered with gaps` in that log is the same disease as this
alert, one layer down. `prometheusRule.roleScrape` must stay off until both are
fixed.

## When to escalate

- **The heartbeats are stale too.** Then this is not an observability problem;
  the control plane is down. [control-plane-gap.md](control-plane-gap.md).
- **This has been firing for weeks.** Somebody has been treating a
  known-uncovered farm as a covered one. That is worth saying out loud in a
  review, not just fixing quietly.
