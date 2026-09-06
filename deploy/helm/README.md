# device-farmer on Kubernetes

The control plane belongs in a cluster. The thing it controls does not.

This directory holds a Helm chart for the seven control-plane roles — `api`,
`scheduler`, `reaper`, `recovery`, `jobrunner`, `janitor`, `watchdog` — plus
the schema migration that has to succeed before any of them start. The eighth
role, `node`, belongs on the machine the phones are plugged into; the chart
renders it only if you ask for it, off by default, and the first section below
is the argument for leaving it off.

```
helm upgrade --install farm ./device-farmer \
  --namespace device-farmer --create-namespace \
  --set image.repository=registry.example.com/device-farmer/farmd \
  --set image.tag=1.4.2 \
  --set database.existingSecret=farm-postgres \
  --set auth.existingSecret=farm-api-tokens \
  --values my-hosts.yaml
```

Every knob is documented inline in [`device-farmer/values.yaml`](device-farmer/values.yaml);
this file covers the things a values comment cannot: the deployment split, the
bare-metal half, and what is not wired up yet.

---

## The split, and the DaemonSet that does not close it

`farmd node` is the agent that lives on a device host — the machine with the
USB hubs and the phones. **The shipped way to run it is the systemd unit in the
next section, on that machine.**

The chart can also run it as a DaemonSet — `node.enabled`, off by default, every
knob documented under `node:` in `values.yaml` — for a farm whose device hosts
*are* cluster nodes. That option exists because it was asked for. It does not
change which deployment this project stands behind, and it is not a packaging
preference: four things are true of the containerized shape, and turning it on
fixes none of them.

| | Why this is not the shipped path |
| --- | --- |
| **Tier 4 needs a second image** | `uhubctl` is another binary, with a libusb dependency and therefore a libc. This chart deploys the one distroless static image every role shares — OPS-01, *one static binary, every role a subcommand* — so in a pod tier 4 refuses with 501 and names uhubctl. Shipping a second image would put a second security posture on the machine that has physical control of the phones. |
| **Tier 3 costs `privileged: true`** | Kubernetes has no per-device cgroup API: nothing in a Pod spec says "allow `open()` on char major 189". The narrow form (`device_cgroup_rules`) exists only in compose. So `node.usbReset.enabled` is the whole device cgroup or none of it, which is why it is a second switch and not part of `node.enabled`. |
| **A restart is ordinary here, and it is not free** | Every agent start bumps `farm.hosts.host_epoch` on purpose: the new process cannot prove the local ADB server survived its absence, and a stale transport id is a wrong device away from a reset landing on somebody's running job. On bare metal a restart is a deploy. In a cluster it is also a drain, a preemption, an eviction and an image update — each one re-observing every device on that machine. |
| **Nothing on this path has met hardware** | No phone, no USB bus, no `USBDEVFS_RESET` ever issued and no VBUS ever cut, in a container or out of one — REQUIREMENTS.md REC-03 and HW-05. The DaemonSet in this chart has been rendered and never applied. |

What a pod *can* do is the software half, and that is the default shape when
the DaemonSet is enabled: discovery and enrollment need `/sys` and nothing
else. `internal/topo` only ever reads sysfs — down to the per-port test for
whether VBUS can be switched, which is a file-mode read — so the mount is
read-only, and a writable `/sys` inside a privileged container would be one of
the best-known container escapes bought for nothing.

That shape is only safe because the agent says so out loud. An agent with no
`/dev/bus/usb` refuses tier 3 with a 501 naming the missing mount, and one that
is denied by a file mode refuses with a 409 naming the udev rule; neither is
recorded as a rung that ran and failed. That distinction is the whole safety
argument, because a *failed* rung is the one the ladder answers by escalating —
to cutting VBUS, and then to quarantine, on a device the agent never touched.

Two things the chart cannot do for you in either deployment. Nothing in farmd
writes `farm.hosts.node_endpoint`, so no agent can be dialled until a human
runs `UPDATE farm.hosts SET node_endpoint = '<host>:8082' WHERE id = '<id>'`
once per machine. And the token is yours to place: `node.token.existingSecret`
must name the same Secret the api, recovery and chargepolicy roles read
`FARM_NODE_TOKEN` from, or every call answers 401.

> The `node.env` block in the next section does not set `FARM_NODE_TOKEN`,
> and the agent refuses to start without it. Add the line when you copy that
> file.

The consequence is visible rather than hidden. Without a node agent, recovery
tiers 3 and 4 are **refused with a reason naming what is missing**, not
silently skipped — see `recovery.NewADBActuator`. The dashboard's capability
report (`GET /api/v1/capabilities`) says the same thing.

### Identity, not serials

Every call that targets a physical device uses a `devpath`, never an ADB
serial. Duplicate OEM serials are real, and the `devpath` is what the node
agent discovers from the USB tree. That is another reason the agent must be on
the machine: nothing else can see the tree that gives a phone its identity.

---

## Installing the host half

On each machine with phones, as root. Nothing below is in the cluster.

### 1. Let the cluster reach the ADB server

`adb` binds `127.0.0.1` by default, which makes it invisible to the watchdog
and to the recovery ladder. Give it a routable listener:

```ini
# /etc/systemd/system/adb-server.service
[Unit]
Description=ADB server for the device farm
After=network-online.target

[Service]
# -a makes the server listen on all interfaces. Treat 5037 as privileged: a
# reachable ADB server is unauthenticated shell on every phone attached to it.
# Firewall it to the cluster's egress addresses.
ExecStart=/usr/bin/adb -a -P 5037 nodaemon server
Restart=always
RestartSec=2
User=farm

[Install]
WantedBy=multi-user.target
```

### 2. Run the node agent

```ini
# /etc/systemd/system/farmd-node.service
[Unit]
Description=device-farmer node agent (USB discovery, enrollment, hardware recovery)
After=adb-server.service network-online.target
Wants=adb-server.service

[Service]
Type=exec
EnvironmentFile=/etc/device-farmer/node.env
ExecStart=/usr/local/bin/farmd node

# SIGTERM MUST NOT RELEASE LEASES. farmd drains in-flight work and leaves
# every lease exactly where it is; a host agent restart is not evidence that
# the job on the phone died. Do not add a shutdown hook that "cleans up".
KillSignal=SIGTERM
TimeoutStopSec=60
Restart=always
RestartSec=5

# The agent needs the USB tree and hub power. This is precisely the privilege
# a cluster workload should not have, and precisely why it runs here.
DeviceAllow=/dev/bus/usb rw
SupplementaryGroups=plugdev

[Install]
WantedBy=multi-user.target
```

```sh
# /etc/device-farmer/node.env   (chmod 600 — it holds the DSN)
DATABASE_URL=postgres://farm:...@postgres.example.com:5432/device_farmer?sslmode=require

# Which physical host this is. It becomes farm.hosts.id, and every devpath is
# interpreted against it. Guessing it would attach a rack of phones to the
# wrong row.
FARM_HOST_ID=h01

# What the agent registers as farm.hosts.adb_endpoint. THE CLUSTER CONNECTS TO
# THIS STRING, so it must be routable from there — not 127.0.0.1.
FARM_ADB_ENDPOINT=10.20.0.11:5037

# These two MUST match the chart's config.lease.slotRearm and
# config.node.selfFenceTimeout. The relationship between them is the assertion
# that keeps one phone from being handed to two jobs: a reclaimed slot must
# stay parked until this agent has finished tearing down the previous holder's
# ADB sockets. farmd refuses to start if the numbers disagree.
FARM_NODE_SELF_FENCE_TIMEOUT=20s
FARM_SLOT_REARM=35s

# Optional: the fence proxy (docs/design/fence-proxy.md, section 14). With all
# three PEMs set — one or two refuses to start — this agent serves an mTLS
# listener in front of the adb server and registers THE PROXY as
# farm.hosts.adb_endpoint: FARM_FENCE_ADVERTISE, or FARM_ADB_ENDPOINT's host on
# the proxy's port. The self-fence timeout above becomes its staleness budget.
#FARM_FENCE_TLS_CERT=/etc/device-farmer/fence/tls.crt
#FARM_FENCE_TLS_KEY=/etc/device-farmer/fence/tls.key
#FARM_FENCE_TLS_CA=/etc/device-farmer/fence/ca.crt
#FARM_FENCE_LISTEN=:5038
#FARM_FENCE_ADVERTISE=10.20.0.11:5038
```

The agent needs a route to **Postgres**, not to the API: it upserts its own
`farm.hosts` row, registers the USB topology, and beats as `node:<host id>`.

### 3. Tell the chart about the host

```yaml
# my-hosts.yaml
hosts:
  - id: h01                          # must equal FARM_HOST_ID above
    adbEndpoint: farm-device-farmer-h01-adb.device-farmer.svc.cluster.local:5037
    service:
      enabled: true                  # renders the Service + EndpointSlice below
      address: 10.20.0.11
  - id: h02
    adbEndpoint: 10.20.0.12:5037     # or skip the Service and use the address
```

This produces one `watchdog` Deployment per host, each pinned with
`FARM_HOST_ID` and `FARM_ADB_ENDPOINT`. Health is per-host because the ADB
server is per-host: one stream, one epoch, one blast radius.

With `service.enabled`, the chart also renders a **selector-less Service plus
a hand-written EndpointSlice** for the machine's address. That is the standard
way to give an off-cluster endpoint a stable in-cluster name: `h01-adb.<ns>.svc`
means "whatever address that host has today", so re-imaging a machine is a
values change rather than an `UPDATE` on `farm.hosts`. A DNS name in
`service.address` renders an `ExternalName` Service instead.

These objects do **not** create rows in `farm.hosts`. The node agent upserts
its own row on every heartbeat with the `FARM_ADB_ENDPOINT` it was given, so
that variable and `hosts[].adbEndpoint` should name the same endpoint.

---

## What the chart deploys

| Object | Role | Shape |
| --- | --- | --- |
| Job (`pre-install`/`pre-upgrade` hook, weight -5) | `migrate` | Runs `farmd migrate up`. Helm aborts the release if it fails, so no workload is ever pointed at a schema it does not understand. Weight -10 is a hook-scoped Secret carrying the DSN, because ordinary release resources do not exist yet during a hook. |
| Deployment + Service + PDB | `api` | `/healthz` liveness (no database), `/readyz` readiness (pings Postgres), `/metrics`, and the dashboard at `/`. Spread across nodes; `maxUnavailable: 0`. |
| Deployment | `scheduler` | Single writer by **advisory-lock election**, not by replica count. Extra replicas idle. |
| Deployment | `reaper` | Same election. The only automatic release path in the system. On every gain of leadership it calls `farm.reaper_arm` before its first sweep, which is what makes rolling it safe. |
| Deployment | `janitor` | Same election. The only thing that closes a `farm.job_steps`, `job_attempts`, `bulk_targets` or `recovery_attempts` row whose process died. A step is an orphan when its **lease** is dead, never when it is slow; the two row kinds that carry no lease go by the run's own timeout and the ladder's own stale threshold. It cannot end a lease: the package does not import `internal/lease`. |
| Deployment | `recovery` | Serialised per device by a transaction-scoped advisory lock. |
| Deployment | `jobrunner` | **Scales.** Jobs are claimed with `SKIP LOCKED` plus a per-job advisory lock, and a lease is re-attached by `job_id`, so two replicas never fight over one device. |
| Deployment ×N | `watchdog` | One per entry in `hosts[]`, replicas pinned at 1: there is no election, and a second replica would only double the probe rate against that host's single ADB server. |
| Service + EndpointSlice ×N | — | Stable in-cluster names for the bare-metal ADB servers. |
| DaemonSet | `node` | **Off by default**, behind `node.enabled`, and the first section above is the argument for leaving it off. `hostNetwork`, a required `nodeSelector`, `/sys` read-only; `node.usbReset.enabled` adds `privileged: true` and `/dev/bus/usb` for tier 3 alone — tier 4 refuses in any pod, because uhubctl is not in this image. |
| ServiceMonitor | — | Optional, behind `serviceMonitor.enabled`. |

### Why most of these have no probes

Only `api` listens on a port. The other roles serve no HTTP, and the image is
distroless — there is no shell to `exec` a probe into. Their liveness is
`farm.component_heartbeat`, which is the same signal the reaper uses for gap
accounting and which the API reports at `GET /api/v1/capabilities`. Inventing
a second opinion about liveness here would mean a probe could restart a
process that is holding real work.

### terminationGracePeriodSeconds, and the thing it is not

`SIGTERM` drains in-flight work. It does **not** release leases, and nothing
in this chart can make it:

> A pod eviction is not evidence that the job on the phone died.

In Kubernetes, `SIGTERM` is the most ordinary event there is — a node drain, a
rolling update, a spot preemption, an OOM restart, a scale-down. A control
plane that "cleaned up after itself" on the way out would convert every one of
them into destroyed work. That is [DeviceFarmer/STF issue #663][663] with a
cluster trigger instead of a socket one: there, a ~90 minute `ECONNRESET`
releases a device mid-run.

So the grace periods here bound *draining*, nothing else. The replacement pod
calls `farm.lease_acquire` with the same `job_id` and gets the same lease, the
same device and the **same fence** back — the fence is not bumped precisely
because the evicted process's own work may still be attached to the phone.

Prove it on a live farm:

```sh
kubectl -n device-farmer rollout restart deploy/farm-device-farmer-jobrunner
kubectl -n device-farmer logs -l app.kubernetes.io/component=jobrunner --tail=50
```

The job keeps its device across the restart.

[663]: https://github.com/DeviceFarmer/stf/issues/663

---

## What the chart refuses to render

Every one of these is a values file that would have installed cleanly and then
misbehaved without an error to read. They fail at render time, before the
migration hook runs and before anything exists in the cluster.

| Refused | Because |
| --- | --- |
| Neither `database.dsn` nor `database.existingSecret` | There is no default DSN. A control plane silently pointed at the wrong database is worse than one that will not start. |
| **Both** of them | `existingSecret` wins and `dsn` renders nowhere. Every pod would read a database the operator did not name, while the one they typed appears in no object. |
| **Both** `auth.tokens` and `auth.existingSecret` | Same shape, worse direction: editing `auth.tokens` to *revoke* a leaked credential would look like a clean upgrade and change nothing. |
| `config.db.maxConns` < 2 | The scheduler, reaper and janitor each pin one connection for their leader-election lock. A pool of one leaves the elected leader with nothing to work with: it reports leadership, then places no job, reclaims no lease and closes no orphan, forever, silently. |
| `api.terminationGracePeriodSeconds` ≤ `config.shutdownGrace` | The kubelet would SIGKILL the api mid-drain. On every rolling deploy the request most likely to be cut off is a renewal one round trip from success. |
| A `hosts[].id` that is not a DNS-1123 label, or repeated | It becomes a Deployment name and a Service name. `farm.hosts.id` is unconstrained `text` in the schema, so an id that is legal in Postgres and illegal in Kubernetes is ordinary — and a repeated id means one machine silently loses its watchdog. |
| A `hosts[].service.address` carrying a port or a scheme | It becomes an EndpointSlice address or an `ExternalName`, and neither accepts one. |
| `DATABASE_URL`, `FARM_API_TOKENS` or `FARM_COMPONENT` in `config.extra` | The first two are credentials and belong in `database.*` / `auth.*`. The third is per-role identity: one shared `FARM_COMPONENT` would make the reaper's gap accounting blind to the role that is actually down, so its outage goes unrefunded and its leases are reclaimed on schedule. |
| Neither `auth.tokens` nor `auth.existingSecret`, on a listener the network can reach | The api refuses to serve an unauthenticated control plane on a non-loopback address, so every api pod would sit in `CrashLoopBackOff` while `helm install --wait` ran out its own timeout — five minutes, ending in a message that never mentions a token. Set one of the two, or opt into an open control plane on purpose. |
| A release name long enough to collapse the component suffix | `<fullname>-<component>` was truncated from the tail, and the tail is the only part that tells `reaper` from `scheduler`. Past a certain length every Deployment rendered under one name: at the quiet end one host's watchdog simply vanished, at the loud end nine workloads became one — after the migration hook had already run. |

`hosts[]` is validated from `templates/configmap.yaml`, which renders in every
possible release. That matters: the checks used to live in `watchdog.yaml`, so
`watchdog.enabled: false` let a malformed id through into a Service name and
failed the install halfway, *after* the migration hook had already run.
Validation that a values flag can switch off is not validation.

Warnings that do **not** block, printed by `NOTES.txt` because the right answer
depends on what you are doing: `config.db.maxConns` below 7 (the jobrunner
quietly stops picking work up once busy), a PDB that allows zero disruptions
because `api.replicaCount` is 1, a per-pod artifact store with more than one
replica, and no `hosts[]` at all.

---

## Secrets

- `DATABASE_URL` and `FARM_API_TOKENS` are Secrets. Everything else is a
  ConfigMap. `config.extra` **refuses** those two keys, and refuses
  `FARM_COMPONENT` as well: every role must beat under its own name, or the
  reaper's gap accounting goes blind to the role that is actually down.
- `database.existingSecret` / `auth.existingSecret` keep both out of your
  values file entirely. Setting an `existingSecret` *and* its inline twin is
  refused rather than resolved by precedence — see the table above.
- The migration hook gets its own short-lived copy of the DSN, deleted on
  success **and on failure**. The failed Job and its pod are kept so
  `kubectl logs` still has the goose output; a plaintext password has no
  diagnostic value and would otherwise be left in the namespace owned by no
  release, surviving `helm uninstall`.
- No ServiceAccount and no RBAC are created, and
  `automountServiceAccountToken: false` is set on every pod. farmd talks to
  Postgres, never to the Kubernetes API.

---

## Upgrades

`helm upgrade` runs the migration hook first. If it fails, the release is
aborted and the running control plane is untouched.

**`helm rollback` does not roll the schema back.** The migrations are additive
and a newer schema serves an older binary, so this is usually what you want.
When it is not, `farmd migrate down-to <version> -yes` is a deliberate,
manual, destructive operation — run it yourself, with a backup, and roll the
release back afterwards.

---

## What this chart still cannot do for you

Both entries that used to stand here were about call sites that no longer
exist, and they had been false since before the commit that wrote them. They
are recorded rather than deleted, because a reader who remembers being told the
API was open needs to be able to tell FIXED from NEVER TRUE.

| It used to say | What is true, and how it was checked |
|---|---|
| "`FARM_API_TOKENS` is mounted, but check that your build reads it" — `runAPI` passes `api.NewAllowAll(...)` unconditionally, so every caller is an operator no matter what the chart mounts | `cmd/farmd/roles.go` calls `api.AuthenticatorFor(cfg, log)`; the string `NewAllowAll` does not appear in the file. Measured on a live cluster: with `auth.tokens` set, `GET /api/v1/capabilities` answers **401 with no credential and 200 with the token**, and a wrong token answers 401. With `auth.tokens` empty the chart now refuses to render at all rather than installing an api that cannot start. |
| "`farmd node` cannot start in this build" — `runNode` never passes a `Token` and `node.New` refuses that combination | `cmd/farmd/roles.go` passes `Token: cfg.Node.Token`. `scripts/linux-acceptance.sh` starts the role against a real filesystem and asserts `/node/v1/health` answers **401 without a bearer token and 200 with one**; both checks pass on Linux 6.18 / PostgreSQL 18.6. |

What remains true, and is the reason `.auth.open` exists: **a chart cannot know
what image you deployed.** `auth.tokens` being set is not evidence that the
running binary honours it, and "tokens are configured" is exactly the sentence
that stops someone checking. So ask the server rather than the values file:

```sh
curl -s http://<api>/api/v1/capabilities | jq '.auth, .build'
# {"mode":"bearer","open":false, ...}
# {"version":"0.1.0","revision":"<sha>", ...}
```

`open: false` is the whole answer for authentication, and `build` is how you
know which image produced it — that field reported `"dev"` for every binary
ever built until the linker's stamp was passed through to the API, so a farm
you deployed last week may still answer `dev` if its image predates that fix.

The genuine gap is narrower and lives in this document: the `node.env` block
above does not set `FARM_NODE_TOKEN`, and the agent refuses to start without
it. Without a node agent, recovery tiers 3 and 4 are **refused with a reason
naming what is missing**, never silently skipped.

Neither of these affects the lease invariant. They affect who may talk to the
control plane, and whether recovery tiers 3 and 4 can act at all.

---

## Validating a change to this chart

```sh
# Both need a credential decision as well as a DSN: with neither auth.tokens
# nor auth.existingSecret the chart refuses, because the api would not start.
helm lint deploy/helm/device-farmer -f my-hosts.yaml
helm template farm deploy/helm/device-farmer -n device-farmer -f my-hosts.yaml   | kubectl apply --dry-run=server -f -
```

**`helm lint` alone is not a check on this chart.** Helm 4 reports a template
`fail` at INFO and then prints `0 chart(s) failed`, so a lint of a values file
the chart would REFUSE to install still exits 0. `helm template` is the one
that fails, which is why CI runs both and why the line above passes a values
file rather than a bare `--set`.

With **no** database configured, `helm template` (and `helm install`) fail on
purpose, with a message naming the value to set. A control plane that renders
cleanly while pointed at nothing is worse than one that refuses. `helm lint`
is more forgiving — Helm 4 logs the guard as `funcMap fail` at INFO and still
reports `0 chart(s) failed` — so lint the way you install it, with the values
you will actually use.

`.github/workflows/ci.yml` does exactly that on every push, with
[`ci-values.yaml`](ci-values.yaml) — every optional object switched on — then
renders the chart three ways, checks that each role's template still produces
a Deployment, and checks that the refusals above still refuse. `make ci-helm`
is the same run locally.

A stronger check than either, because it uses the real binary rather than a
schema: feed the rendered ConfigMap to `farmd` and see whether every role
accepts it. The cross-field assertions in `internal/config` — `FARM_SLOT_REARM`
against `FARM_NODE_SELF_FENCE_TIMEOUT`, the renewal-attempts-per-TTL floor, the
`FARM_REAPER_COMPONENTS` membership rule — are the ones a values typo trips,
and they fail a pod at startup rather than a template at render.

```sh
helm template farm deploy/helm/device-farmer -n device-farmer \
  --set database.dsn="$DATABASE_URL" \
| python -c 'import sys,yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d["kind"] == "ConfigMap":
        for k, v in d["data"].items(): print(f"{k}={v}")' > /tmp/cm.env

set -a; . /tmp/cm.env; set +a
for role in api scheduler reaper recovery jobrunner janitor; do farmd $role & sleep 2; kill %1; done
```

### Export `DATABASE_URL` when you run the test suite

`go test ./...` passes on a laptop with no Postgres — the SQL-backed tests skip
themselves — and that is a trap worth knowing about before you trust a green
run. The lease package's fencing tests are the ones this whole system rests on,
and only half of them survive the skip. Breaking `Store.Renew` in each
direction and re-running is how you find out:

| Break | Caught without a database? |
| --- | --- |
| Transient error reported as `ErrFenced` (a Postgres blip aborts a running job — STF #663) | **Yes.** `TestRenewErrorsThatAreNotFencing/unreachable_database` fails. |
| Zero rows reported as a transient error (a genuinely fenced holder keeps driving a phone that now belongs to another job) | **No.** Passes clean. With `DATABASE_URL` set it fails four ways: `TestRenewZeroRowsIsFencedAndTerminal` ×3, `TestAcquireReattachesAtTheSameFence`, and `TestHolderAgainstPostgres/an_operator_revoke_fences_the_holder`. |

So CI must export `DATABASE_URL`. Without it the suite is blind to the more
dangerous direction of the conflation. The tests create and drop a scratch
database per run and never touch the one in the DSN, so pointing it at a live
farm's Postgres is safe.
