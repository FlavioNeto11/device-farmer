# DeviceFarmerJobsFailing

**Severity:** warning · **Group:** `device-farmer.roles` (off by default)

```promql
sum(rate(farm_jobrunner_outcomes_total{state="failed"}[30m]))
  / clamp_min(sum(rate(farm_jobrunner_outcomes_total[30m])), 0.001) > 0.25
                                                                    for 30m
```

## What fired

More than a quarter of job attempts are ending in `failed`.

## What it means

Something is wrong, and **this metric cannot tell you what**. That is on purpose:
it carries no `job_id` and no `device_id`, because those are unbounded label
dimensions and `internal/obs` refuses them — metrics answer "is something wrong,
and where", they are not an audit log with worse retention.

The question worth answering is the one the ratio hides:

> **Is one job failing on many devices, or are many jobs failing on one device?**

- **One job, many devices → a job problem.** A bad APK, a spec that references a
  missing artifact, a test that assumes an Android version the pool does not
  have. The fleet is fine.
- **Many jobs, one device → a device problem.** A phone that installs and then
  wedges, a full `/data`, a handset that reboots under load. One slot is eating
  the queue.

`farm.job_attempts` exists to tell those apart. It records every placement: job,
attempt number, device, lease, fence, outcome and error. The two queries below
are the whole diagnosis.

## What is NOT wrong

- **A quiet farm with two failures.** The rule is a *ratio* of all outcomes, not
  a rate, so two failures out of four attempts is 50% and will fire. The 30m
  `for:` helps; a genuinely idle farm is still the most likely false alarm here.
- **`state="cancelled"`.** A tenant cancelling their own work. Counted in the
  denominator, not the numerator.
- **`state="abandoned"`.** The janitor closing an attempt whose runner
  disappeared. Worth its own look, but it is not a test failing.
- **`farm_jobrunner_fenced_total` climbing alongside.** That is
  [lease-fenced.md](lease-fenced.md); jobs that lost their lease mid-flight did
  not fail, they were stopped.

## First three checks

**1. Is it one job (or one spec) failing everywhere?**

```sh
psql "$PGURL" -c "
SELECT a.job_id, j.tenant_id, j.pool_id,
       count(*) AS attempts,
       count(DISTINCT a.device_id) AS distinct_devices,
       count(*) FILTER (WHERE a.outcome = 'failed') AS failed,
       max(a.error) AS last_error
  FROM farm.job_attempts a
  JOIN farm.jobs j ON j.id = a.job_id
 WHERE a.started_at > now() - interval '6 hours'
 GROUP BY 1,2,3
HAVING count(*) FILTER (WHERE a.outcome = 'failed') > 1
 ORDER BY failed DESC, distinct_devices DESC
 LIMIT 20"
```

**`distinct_devices` is the discriminator.** A job with 5 failed attempts on 5
different devices is the job. A job with 5 failed attempts on 1 device is
`max_attempts` retrying onto the same pinned phone — look at the device.

**2. Is it one device failing everything?**

```sh
psql "$PGURL" -c "
SELECT a.device_id, f.rack_slot, f.host_id, f.hub_path, f.model, f.health,
       count(DISTINCT a.job_id) AS distinct_jobs,
       count(*) FILTER (WHERE a.outcome = 'failed') AS failed,
       count(*) AS attempts
  FROM farm.job_attempts a
  LEFT JOIN farm.v_fleet f ON f.device_id = a.device_id
 WHERE a.started_at > now() - interval '6 hours' AND a.device_id IS NOT NULL
 GROUP BY 1,2,3,4,5,6
HAVING count(*) FILTER (WHERE a.outcome = 'failed') > 1
 ORDER BY failed DESC, distinct_jobs DESC
 LIMIT 20"
```

**`distinct_jobs` is the discriminator here.** A device with 6 failures across 6
different jobs is the device, and `rack_slot` is where to walk. A device with 6
failures from 1 job is the job again.

Note `a.device_id` can be NULL: `farm.job_attempts` keeps the placement and drops
the device reference when a handset is retired, so a NULL there is history, not
a bug.

**3. Which step is failing, and with what?** Once you know *which* job or device,
the steps say why.

```sh
farmd ctl job <job-id>
farmd ctl job steps <job-id>
farmd ctl job attempts <job-id>
```

```sh
psql "$PGURL" -c "
SELECT s.step_index, s.kind, s.state, s.exit_code,
       left(coalesce(s.error, s.output), 300) AS detail
  FROM farm.job_steps s
 WHERE s.job_id = '<job-id>' AND s.attempt = (SELECT max(attempt) FROM farm.job_steps WHERE job_id = '<job-id>')
 ORDER BY s.step_index"
```

A failure at an `install` step across many devices is almost always the APK or
the artifact store. A failure at the *same* test step on one device is the phone.

## Cross-cutting causes worth ruling out

```sh
# Placements refused because the device had no USB position or no adb endpoint.
# Never resolved by serial, deliberately — so a topology gap fails loudly.
curl -s "$PROM_URL/api/v1/query?query=farm_jobrunner_unaddressable_total" | jq '.data.result'

# Artifacts: a shared store that is not actually shared is the classic.
farmd ctl artifacts
```

If `artifacts.persistence.enabled` is false in a multi-replica install, the api
accepts an upload into its own `emptyDir` and a jobrunner on another node cannot
find the blob — every job with an install step then fails, on every device, and
it looks exactly like a fleet-wide hardware problem. That needs a
`ReadWriteMany` claim, not a rack visit.

## What to do

- **A job problem** → it is the tenant's. Give them the failing step and the
  error. Cancel the job if it is burning devices on retries:
  ```sh
  farmd ctl job cancel <job-id> --reason "failing install on every device; artifact missing"
  ```
  Cancelling releases the lease with `reason='job_cancelled'` — an ordinary
  ending, not lost work.
- **A device problem** → let the recovery ladder have it, or take it out
  yourself if it is clearly bad. Check its history first:
  ```sh
  farmd ctl device <device-id>
  psql "$PGURL" -c "SELECT id, farm_uid, model, failure_score, failure_score_at, admin_state
                      FROM farm.devices WHERE id = '<device-id>'"
  ```
- **Neither, and it is everything** → look at the artifact store and at
  `farm_jobrunner_unaddressable_total` before anything else.

## When to escalate

- **The ratio is high and `distinct_devices` is high for many jobs.** Something
  common to all of them changed — an image, the artifact store, a step kind.
- **One device is failing jobs but its `health` is `healthy`.** The watchdog and
  the jobs disagree, and the watchdog is the one that gates scheduling. That is
  worth an engineer: it means devices that break in ways ADB cannot see are
  being handed out.
- **`abandoned` outcomes climbing.** Runners are disappearing mid-attempt. That
  is a jobrunner or a node-eviction problem, not a test problem.
