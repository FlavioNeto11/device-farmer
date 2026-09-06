# device-farmer on a local Kubernetes

An evaluation cluster on the machine you are sitting at, in one command, and
gone in one more.

```
bash scripts/k8s-up.sh          # or: make k8s-up
bash scripts/k8s-down.sh        # or: make k8s-down
```

It builds the image, imports it into the local cluster's node, brings up a
Postgres whose storage dies with its pod, installs the chart from
`deploy/helm/device-farmer`, starts 56 simulated devices, and holds a
port-forward open on <http://127.0.0.1:8420/>. Measured on Docker Desktop's
Kubernetes v1.36.1, from an already-built image: about a minute.

```
NAME                                             READY   STATUS      RESTARTS
device-farmer-demo-67df488b88-k422h              1/1     Running     0
df-device-farmer-api-588f494bd-266sn             1/1     Running     0
df-device-farmer-chargepolicy-685b8d79d8-l6kzv   1/1     Running     0
df-device-farmer-janitor-c884c4dd7-76dfp         1/1     Running     0
df-device-farmer-migrate-nk27c                   0/1     Completed   0
df-device-farmer-reaper-56ff5bf697-6qr4h         1/1     Running     0
df-device-farmer-scheduler-5698678887-kb2jg      1/1     Running     0
evaluation-postgres-6bc57b8f49-v8nq2             1/1     Running     0

  healthz                          200
  dashboard /                      200 16485  (status, bytes)
  /api/v1/capabilities             401 with no credential
  /api/v1/capabilities             200 with the token
```

**This is an evaluation cluster, not a farm.** Its database loses everything on
a pod restart, its api token is published in this repository, and every phone in
it is imaginary. Each of those is a deliberate choice with a farm's answer
beside it, below.

## The two shapes

| | what runs | what the fleet grid shows |
|---|---|---|
| `scripts/k8s-up.sh` | the chart plus `farmd demo` | 56 simulated devices, jobs moving |
| `scripts/k8s-up.sh --farm` | the chart alone | nothing, until real hosts enroll |

`--farm` is the shape you would actually deploy: the control-plane roles and no
simulated hardware. Its empty fleet grid is correct rather than broken —
nothing has registered a device — and `farmd node`, the process that would,
cannot run in a cluster because it needs `/dev/bus/usb` and `uhubctl` on the
machine the phones are plugged into. See `deploy/helm/README.md`.

Useful flags: `--build` forces a rebuild, `--no-forward` does the whole install
and the checks and then exits without holding the tunnel open. Everything else
is an environment variable, listed by `scripts/k8s-up.sh --help`.

## The three things between a checkout and a running release

**1. The chart's default image does not exist.** `image.repository` is
`ghcr.io/flaviopadilha/device-farmer/farmd` and nothing in this repository
builds it, pushes it, or names it anywhere else. On this cluster:

```
Failed to pull image "ghcr.io/flaviopadilha/device-farmer/farmd:0.1.0":
  failed to fetch anonymous token: unexpected status from GET request to
  https://ghcr.io/token?...&service=ghcr.io: 403 Forbidden
```

Inside the chart's pre-install migration hook that is worse than it looks: helm
waits on the hook, and its default `--timeout` is five minutes, so what you get
is a timeout that names neither the image nor the registry.
`deploy/helm/local-values.yaml` points the release at a locally built
`device-farmer/farmd:local` instead, and `scripts/k8s-up.sh` passes
`--timeout 16m` so that helm outlives `migrate.activeDeadlineSeconds` (900s)
and reports the Job's own verdict rather than its own impatience.

**2. A locally built image is not automatically runnable.** What is true here
is narrower than the usual advice, in both directions:

- Docker Desktop's kind-mode node does **not hold** your daemon's images.
  `crictl images` on `desktop-control-plane` lists none of them.
- It can still **fetch** them. `/etc/containerd/certs.d/_default/hosts.toml`
  points every registry at `http://registry-mirror:1273` — the
  `desktop-containerd-registry-mirror` container, which serves this daemon's
  store. A tag invented seconds ago and published nowhere pulls in ~40 ms.

That mirror is a Docker Desktop feature. A plain kind cluster, k3d and minikube
have nothing like it, and there an unimported image really does end in
ImagePullBackOff. So the script imports explicitly and then checks that the
image landed:

```
docker save device-farmer/farmd:local |
  docker exec -i <node-container> ctr -n k8s.io images import -
```

The node container is found, not assumed: every node name is looked up in your
own Docker daemon, which matches `desktop-control-plane` (Docker Desktop),
`<cluster>-control-plane` (kind), `k3d-<cluster>-server-0` (k3d) and `minikube`.
`k8s.io` is the containerd namespace the kubelet reads; an import into the
default namespace lands somewhere nothing looks. If no node is a local
container and minikube is not driving the cluster, the script **refuses before
creating anything** and tells you to push the image somewhere the cluster can
pull from — an honest stop instead of an ImagePullBackOff twenty seconds later.

*On Windows* the script prefixes those `docker exec` calls with
`MSYS_NO_PATHCONV=1`. Git Bash rewrites arguments that look like absolute paths
into Windows paths before the process sees them, which mangles anything handed
to a command running inside a Linux container. Nothing in the current commands
takes a path, so it changes nothing today; it is there so the first edit that
adds one is not a baffling bug. *On Linux and macOS* it is an unused
environment variable and the rest is identical — there is no other
platform-conditional line in either script.

**3. The api refuses to run open.** The chart binds `0.0.0.0` — a container
that binds `127.0.0.1` answers its own probes and nothing else — and farmd
will not serve an unauthenticated control plane on a network listener. An
install with only a DSN therefore comes up `CrashLoopBackOff` on every api pod,
and helm reports `Available: 0/2`. `local-values.yaml` sets one token:

```
auth.tokens: "local-evaluation:operator:local-evaluation"
```

The dashboard page is served without a credential; the API under it is not. Open
it, click **Set API token**, paste `local-evaluation`. The script proves on every
run that this is really happening — `/api/v1/capabilities` answers 401 without
the token and 200 with it, while `/healthz`, which is not behind the gate,
answers 200 either way.

## Two questions this path answers differently from a farm

**Postgres.** The chart deliberately ships none, and `Chart.yaml` argues why: a
farm whose database arrived as a convenience subchart is one `helm uninstall`
away from losing the record of which phone is holding whose six-hour job. That
argument is about a farm. A laptop has no record worth keeping, so
`deploy/local/postgres.yaml` says yes to a bundled Postgres and then makes it
impossible to mistake for a real one — the storage is an `emptyDir`, so there is
no PVC anybody could "just make bigger" and no upgrade path from that file to a
farm's database. For anything real, bring your own and point `database.dsn` or
`database.existingSecret` at it.

**Simulated hardware.** A correct install with no hardware is an empty
dashboard, and nothing on the screen distinguishes that from a broken one. So
the default mode adds `deploy/local/demo.yaml`: one pod running `farmd demo`,
which starts fake ADB servers in its own process and runs the real scheduler,
lease store, reaper and recovery ladder against them.

Because those servers bind `127.0.0.1`, the endpoint the demo writes to
`farm.hosts.adb_endpoint` means nothing in any other pod:

```
h01|127.0.0.1:44837
h02|127.0.0.1:35275
```

Everything that dials a device therefore has to run in that pod, and
`deploy/local/demo-values.yaml` scales the chart's `jobrunner` and `recovery`
Deployments to zero while the demo is up. This is not a guess. Scaling the
jobrunner back to 1 on a running evaluation cluster produces, within a minute:

```
level=WARN msg="transport failure inside a step; retrying INSIDE the lease
  (job NOT failed)" ... err="adbwire: dial: dial endpoint=127.0.0.1:44837:
  dial tcp 127.0.0.1:44837: connect: connection refused"
```

and five jobs in `failed` where there had been none. The roles are scaled to
zero rather than removed, so `kubectl -n df-local get deploy` shows them at
`0/0` — a visible fact about this cluster rather than a farm that never had
them. For the same reason `watchdog.superviseAllHosts` stays off: a watchdog
pod would dial its own loopback and mark all 56 devices unhealthy. The demo
runs one watchdog per host it invented.

Helm's install notes will say `NO WATCHDOGS WERE CREATED — hosts[] is empty`.
That is correct about the chart, and in demo mode it is not the whole story:
health is being probed, from inside the demo pod.

## What is not here

`farmd node` (it needs real USB), an Ingress, TLS, a fence proxy, a shared
artifact volume, backups, NetworkPolicies, and any reason to keep the data.
Jobs that install an artifact will fail their install step, because the api and
the jobrunner have separate `emptyDir` stores and there is no ReadWriteMany
claim; the demo's own job specs deliberately use none.

## If it goes wrong

```
kubectl -n df-local get pods
kubectl -n df-local logs -l app.kubernetes.io/component=api --tail=50
kubectl -n df-local logs -f deployment/device-farmer-demo
kubectl -n df-local describe pod <the one that is not Running>
```

`ImagePullBackOff` means the import did not happen or the tag does not match:
compare `docker exec <node> ctr -n k8s.io images ls -q | grep farmd` with the
`image:` on the pod. A helm timeout at exactly five minutes means something
overrode `--timeout`; the migration Job's own deadline is fifteen. `CrashLoop`
on the api with `FARM_API_TOKENS is empty` means the release lost
`-f deploy/helm/local-values.yaml`.

Capacity is not the constraint, whatever Docker Desktop's settings dialog
reports: the whole release requests about 0.65 CPU and 1 GiB, plus 100m/256Mi
for the evaluation Postgres. A single-node cluster on a developer machine is
not tight, and there is no reason to start tuning replica counts to fit it.

## Files

| | |
|---|---|
| `scripts/k8s-up.sh` | build, import, install, seed, verify, forward |
| `scripts/k8s-down.sh` | uninstall, delete the namespace, remove the imported image |
| `deploy/helm/local-values.yaml` | the three chart defaults a laptop has to change |
| `deploy/local/demo-values.yaml` | the two roles that must not run beside the demo |
| `deploy/local/postgres.yaml` | the evaluation database |
| `deploy/local/demo.yaml` | the simulated hardware |
