# device-farmer

Control plane for a self-hosted Android device farm: 50+ physical handsets
across a few bare-metal hosts, running long jobs continuously, operated from
Kubernetes.

## The one thing this project is about

**A lease ends when the job says so, when a deadline the user wrote down
elapses, or when a human takes it back. Nothing else.**

Not a socket error. Not a probe timeout. Not a device going offline. Not a pod
being evicted.

The reference implementation in this space, [DeviceFarmer/STF][stf], has an
issue that has been [open and unanswered since 2023][663]: a device running
automation on a Kubernetes-hosted STF gets an `ECONNRESET` after about ninety
minutes and is **automatically released** mid-run, destroying hours of work.
Its lease model assumes an interactive human with a timeout.

Here that failure is unrepresentable rather than handled:

```sql
-- farm.leases.release_reason
CHECK (release_reason IN ('completed','failed','job_cancelled','max_runtime',
                          'operator_revoked','holder_expired','device_retired'))
```

There is no connectivity value in that domain, so "released because the
transport dropped" cannot be written down. Three clocks are kept apart —
**lease liveness**, **job liveness**, and **device health** — and the role that
performs reclamation has `SELECT` on the health table revoked, so no future
edit can make releasing a device depend on whether it is reachable.

[stf]: https://github.com/DeviceFarmer/stf
[663]: https://github.com/DeviceFarmer/stf/issues/663

## Try it in one command, with no phones

```bash
make demo
```

or, on Windows without Docker:

```powershell
.\scripts\dev-up.ps1
```

or with Docker:

```bash
docker compose up -d && open http://localhost:8420
```

Any of these brings up 56 simulated devices across two in-process fake ADB
servers — and the **real** scheduler, lease store, reaper, recovery ladder and
job runner running against them. The fake is only the hardware; a demo that
stubbed the logic would prove nothing about the logic.

Watch the log for:

```
transport failure inside a step; retrying INSIDE the lease (job NOT failed)
```

That is the whole thesis. The device went away mid-step and the job kept its
device.

## Architecture

The USB tier cannot move into the cluster, and that is a physical constraint
rather than a limitation of the design. Everything else can.

```
┌──────────────────────────── KUBERNETES ────────────────────────────┐
│  api + dashboard   scheduler   jobrunner ×N   reaper   watchdog    │
│  recovery ladder                            PostgreSQL (state)     │
└────────────────────────────────┬───────────────────────────────────┘
                                 │  ADB over TCP :5037
                ┌────────────────┴────────────────┐
┌───────────────▼──────────────┐  ┌───────────────▼──────────────┐
│ HOST 01   farmd node         │  │ HOST 02   farmd node         │
│ adbd --privileged            │  │ adbd --privileged            │
│ 4 × powered 7-port hub       │  │ 4 × powered 7-port hub       │
│ 28 handsets                  │  │ 28 handsets                  │
└──────────────────────────────┘  └──────────────────────────────┘
```

Twenty-eight devices per host is STF's own reference build — four powered
seven-port hubs on a dedicated PCIe USB 3.0 card. Plan two hosts for 50–60
handsets, three if you want to drain one for maintenance without stopping.

### Roles

`farmd` is one static binary; every role is a subcommand, so a reaper can never
be running a different commit than the scheduler it races with.

| Role | What it does |
|---|---|
| `migrate` | Applies the embedded schema. Carries its own SQL. |
| `api` | HTTP API and the embedded dashboard. |
| `scheduler` | Matches queued jobs to free devices via `farm.lease_acquire`. |
| `jobrunner` | Runs job specs on leased devices. Re-attaches after an eviction. |
| `reaper` | Suspect sweep, and the **only** automatic release path in the system. |
| `watchdog` | Device health only. It can never touch a lease. |
| `recovery` | The recovery ladder. Acts for a holder that keeps its device. |
| `janitor` | Closes rows whose process died. A step is an orphan when its **lease** is dead, never when it is slow. It cannot end a lease — the package does not import `internal/lease`. |
| `chargepolicy` | Holds idle devices inside a 40-80% charge band. Acts only on a device with no live lease, and can never end one. It is the one fire mitigation software can reach. |
| `node` | Host agent: USB discovery, enrollment, and the hardware rungs. |
| `ctl` | Operator CLI, over the HTTP API rather than the database. |
| `all` | Every control-plane role in one process — laptop or single node. |
| `demo` | Simulated hardware plus the real control plane. |

### Why devices are addressed by USB path, never by serial

Cheap OEMs ship duplicate serials; STF's own README mentions a handset whose
serial is `0123456789ABCDEF`. A serial-addressed reset can land on the wrong
clone and sever a healthy device that is hours into someone's job — worse than
the fault being recovered from. Every call that targets a physical position
uses a devpath (`usb:3-1.4.2`), and `internal/adbwire` has a test that fails the
build if allocation vocabulary appears anywhere in it.

Identity is **observed, then resolved, then branded**: a `farm_uid` we wrote to
the device, then a hardware fingerprint, then a serial *and* the same slot, then
adopt as new. `farm.resolve_device` is the only place that rule lives.

## Operating a real farm

### 1. Each host runs an ADB server and a node agent

```bash
docker run -d --name adbd --privileged --restart unless-stopped \
  -p 5037:5037 -v /dev/bus/usb:/dev/bus/usb devicefarmer/adb:latest

FARM_HOST_ID=h01 FARM_ADB_ENDPOINT=127.0.0.1:5037 \
DATABASE_URL=postgres://... farmd node
```

The node agent reads `/sys/bus/usb/devices`, registers the USB tree through
`farm.register_slot`, enrolls whatever is plugged in, and performs the two
recovery rungs ADB cannot reach: `USBDEVFS_RESET` and per-port VBUS power.
**Linux only, kernel 6.0 or newer** — below that the kernel silently re-powers a
disabled port, so a cycle that looks successful did nothing.

Without a node agent the farm still works; tiers 3 and 4 are refused with a
reason naming what is missing, rather than reported as ineffective.

### 2. Bring up the control plane

```bash
docker compose --profile farm up -d
```

Scale `jobrunner` freely: jobs are claimed with `SKIP LOCKED` and a lease is
re-attached by `job_id`, so two replicas never fight over one device. The
`reaper` elects itself with a Postgres advisory lock; a second replica idles.

**PostgreSQL 14 or newer.** `migrations/00001_core.sql` refuses an older server
before it creates anything, rather than failing three migrations later on a
missing feature. `docker-compose.yml`, the Helm chart and CI all run 17, which
is the version the assertion suites are checked against.

### 3. Submit work

A job is an ordered list of typed steps against an abstract device. The
vocabulary is closed and stored in `farm.step_kinds`, so a spec written today
still means the same thing when it resumes tomorrow.

```json
{
  "version": 1,
  "default_timeout": "30s",
  "steps": [
    {"id": "install", "kind": "install",
     "install": {"sha256": "<64 hex>", "reinstall": true}},
    {"id": "soak", "kind": "shell_detached",
     "shell_detached": {"command": "sh /data/local/tmp/soak.sh",
                        "result_path": "/data/local/tmp/.farm/soak.result",
                        "handle": "soak"}},
    {"id": "await", "kind": "wait_for", "timeout": "6h",
     "wait_for": {"handle": "soak", "interval": "30s", "timeout": "6h"}},
    {"id": "collect", "kind": "pull",
     "pull": {"path": "/data/local/tmp/.farm/soak.result", "artifact": "result"}}
  ]
}
```

```bash
# ctl defaults to http://127.0.0.1:8080, which is where a locally run `farmd
# api` listens. Compose publishes the same listener on 8420, so point ctl at it
# — otherwise these commands reach whatever else is on 8080, which on a
# developer's machine is rarely nothing.
export FARM_API_URL=http://127.0.0.1:8420

farmd ctl validate -f spec.json     # every problem, not the first
farmd ctl submit  -f spec.json --pool default --queue soak --tenant acme --expect-duration 6h
farmd ctl jobs
```

Long work runs **detached** on the device, with its output and exit code going
to a device-side file. That is what lets a six-hour job survive a ten-minute
partition: no socket is the source of truth for anything.

The `wait_for` above names the detached step's `handle` rather than probing for
its result file, and the difference is the job's verdict. `test -f …result` is
true the instant the wrapper publishes a status, and it publishes `137` exactly
as eagerly as `0`; `cat …result` exits 0 whatever the file says. Naming the
handle makes the runner read that status and compare it, so a soak killed at
hour four fails the job instead of passing it.

### Reset tiers

`none`, `soft`, `medium`, `hard` expand into real step lists from a profile's
package list, so reset is generic — the farm does not need to know what a job
installed. `medium` uninstalls every package *not* in the profile.

```bash
farmd ctl recovery              # the ladder, attempts, quarantines
curl .../api/v1/specs/resets    # exactly what 'medium' will run, before it runs
```

## The recovery ladder

Stored in `farm.recovery_tiers`, cheapest rung first, each with a blast radius
and the minimum lease disruption policy it requires. **A rung whose blast radius
exceeds what live leases permit is refused and the refusal is recorded**, not
quietly downgraded:

```
tier 4 (port power) REFUSED — the live lease on this device carries
disruption_policy="no_disruption", which forbids a tier-4 power cycle
```

A hub without per-port switching gets a single *ganged* power domain, which is
what stops the ladder from cycling seven devices to fix one. When most of a
hub's devices fail together the hub is quarantined once, not each device
independently — the dashboard says so in as many words:

> 5 of 7 devices on hub 4-2 are unhealthy; suspect the hub, its cable or its
> power domain before the phones

## Development

```bash
make test              # vet, gofmt, and the Go suite
make assertions        # the lease-protocol assertions against a live Postgres
make build-linux       # the static binaries the images ship
make linux-acceptance  # the whole control plane, on Linux, end to end
```

`make linux-acceptance` is the one check that has to run somewhere other than a
developer's laptop, and it exists because three things cannot be seen from here:

- **The binary starts.** Every role builds a metrics registry at startup, and
  nothing in the Go suite calls that function. A duplicate registration once
  made every role panic before serving anything while `go build`, `go vet`,
  `gofmt` and the whole suite stayed green — and only on Linux, because
  prometheus's process collector describes nothing on other platforms.
- **`topo.Sysfs` reads the USB tree.** All eighteen tests in `internal/topo`
  hand `FromFS` an `fstest.MapFS`. The shipped binary calls `Sysfs`, which
  refuses on any GOOS but Linux and then reads through `os.DirFS`, and it takes
  a hub's VBUS switchability from the file MODE on each port's `disable` — a
  fact a MapFS can only assert into being.
- **The schema runs on the server you deploy**, not the one on the laptop.

It migrates an empty database, runs every assertion suite, starts the control
plane, checks the routes and the authorisation, asserts the founding invariant
against the leases the run actually produced, and then runs `farmd node` against
a generated sysfs tree and checks what discovery wrote. From Windows:

```bash
wsl -d Ubuntu -- bash -c 'cd /mnt/c/git/device-farmer && make linux-acceptance'
```

On a farm host with no Go toolchain, hand it the binary instead:
`FARMD=/usr/local/bin/farmd scripts/linux-acceptance.sh`.

It does not prove there is a phone. The ADB servers are `test/fakeadb` and the
USB tree is written by a script, so `USBDEVFS_RESET` and `uhubctl` against real
hardware stay open — `REC-03` and `HW-05` in `REQUIREMENTS.md`.

The two are complements, not alternatives: `test/e2e` proves that `cmd/farmd`
wires the packages together correctly, on whatever platform you are on;
`make linux-acceptance` proves that the result runs on the platform it ships
for. Between them they cover the gap that no in-process test can reach — and
each of them has already found a defect the other could not have.

`test/assertions.sql` is the protocol's specification in executable form. It
proves, against a real PostgreSQL, that a connectivity release reason is
rejected, that a pod eviction re-attaches at the same fence, that a device going
offline leaves the lease untouched, that a control-plane outage is refunded
rather than charged to the tenant, and that device health is unreadable to the
role reclamation runs as.

`test/fakeadb` is a complete in-process ADB server — host protocol and sync —
with fault injection for `FAIL` replies, mid-stream resets and hangs, plus a
duplicate-serial fixture.

`test/e2e` is the acceptance harness. It builds the shipped binary, creates and
migrates a scratch database per scenario with `farmd migrate up`, seeds the
physical tree, starts a `fakeadb` server per host, and then runs the roles the
scenario asked for as **real processes** on ports it picks itself — so it fails
when `cmd/farmd` wires the packages together wrongly, which is the one thing no
in-process test can do. It needs a database and skips without one:

```bash
DATABASE_URL="postgres://farm@127.0.0.1:55432/postgres?sslmode=disable" \
  go test -count=1 -v ./test/e2e/
```

## Status

Working and exercised end to end against simulated hardware: enrollment,
topology discovery, scheduling, leasing, job execution with checkpoint and
resume, file transfer, the recovery ladder, bulk exec, the dashboard and the
CLI.

All three things this section used to name as missing have since shipped, and
the paragraph outlived them. What is true at HEAD:

- **Authentication is wired.** `api.AuthenticatorFor` builds a static bearer
  list from `FARM_API_TOKENS` with role levels and a constant-time compare, and
  it REFUSES to start an open control plane on a listener the network can
  reach. Open is reachable on loopback, or by saying so — that is what the
  packaged demo does. Ask the running server which authenticator it installed
  rather than trusting a values file: `GET /api/v1/capabilities` reports
  `auth.open`.
- **A Helm chart ships**, at `deploy/helm/device-farmer`: eight control-plane
  roles, the migration as a pre-install hook, a ServiceMonitor, a
  PrometheusRule with runbook links, and a values file that refuses nine
  combinations it cannot install correctly. `deploy/helm/README.md` is the
  operator's page; `bash scripts/k8s-up.sh` is the evaluation cluster.
- **The fence proxy's host half is integrated**, not unbuilt: `internal/fenceproxy`
  serves it and `farmd node` wires it. What remains is the CLIENT half
  (`FARM_FENCE_CLIENT_*`), which is why a farm without it still enforces the
  fence in PostgreSQL and relies on the holder to honour it at the socket.

What is genuinely not done is `REC-03`: recovery tiers 3 and 4 —
`USBDEVFS_RESET` and cutting VBUS — have never run against a handset. Nothing
in this repository has met real hardware.

`internal/node` and `internal/topo` are Linux-only by nature and have not been
exercised against real hardware. They compile everywhere and refuse clearly
where they cannot work.
