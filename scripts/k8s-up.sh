#!/usr/bin/env bash
# k8s-up.sh — a device-farmer evaluation cluster, from a checkout, in one
# command.
#
#   bash scripts/k8s-up.sh              simulated hardware; the dashboard has
#                                       56 devices in it
#   bash scripts/k8s-up.sh --farm       the seven control-plane roles and
#                                       nothing else; the fleet grid is empty
#                                       until real hosts enroll
#   bash scripts/k8s-down.sh            removes everything this made
#
# WHAT THIS IS. An evaluation cluster on the machine you are sitting at: a
# Postgres whose storage dies with its pod, an api token that is written down
# in this repository, and — by default — 56 phones that do not exist. It is the
# Kubernetes twin of `docker compose up`, and it is emphatically not a farm.
# deploy/local/README.md says what a farm needs instead, item by item.
#
# WHY IT IS NOT `helm install` ON ITS OWN. Three things stand between a
# checkout and a running release, and each of them fails in a way that names
# neither itself nor its cure:
#
#   1. the chart's default image, ghcr.io/flaviopadilha/device-farmer/farmd,
#      does not exist and nothing in this repository builds or publishes it.
#      A default install ImagePullBackOffs inside the pre-install migration
#      hook, and what you read is helm's five-minute timeout.
#   2. a locally built image is not automatically runnable by a local cluster,
#      and whether it is differs from one laptop to the next. This script
#      imports it and then checks that it landed, rather than finding out from
#      an ImagePullBackOff. See load_image for what is true where.
#   3. the api binds 0.0.0.0 and farmd refuses to serve an unauthenticated
#      control plane on a network listener, so an install with only a DSN comes
#      up CrashLoopBackOff. deploy/helm/local-values.yaml sets a token.
#
# Re-running it is safe: `helm upgrade --install` and `kubectl apply` both
# converge, and the image is rebuilt only if it is missing or you pass --build.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)

NS=${FARM_LOCAL_NAMESPACE:-df-local}
RELEASE=${FARM_LOCAL_RELEASE:-df}
IMAGE=${FARM_LOCAL_IMAGE:-device-farmer/farmd:local}
PORT=${FARM_LOCAL_PORT:-8420}
HOSTS=${FARM_LOCAL_HOSTS:-2}
DEVICES=${FARM_LOCAL_DEVICES:-56}
# Must match deploy/helm/local-values.yaml. It is repeated here rather than
# parsed out of the YAML because the script uses it to prove, on every run,
# that the api refuses the call without it and answers with it.
TOKEN=${FARM_LOCAL_TOKEN:-local-evaluation}

MODE=demo
FORWARD=1
BUILD=auto

usage() {
	cat <<'EOF'
k8s-up.sh — a device-farmer evaluation cluster, from a checkout, in one command.

  bash scripts/k8s-up.sh          simulated hardware; 56 devices in the fleet
  bash scripts/k8s-up.sh --farm   the control-plane roles alone; empty fleet
  bash scripts/k8s-down.sh        removes everything this made

It builds the image, side-loads it into the local cluster's node, brings up an
evaluation Postgres, installs the chart, and holds a port-forward open on the
dashboard. deploy/local/README.md is the long form, including what a farm needs
instead of each shortcut taken here.

Options
  --farm            deploy the control plane alone, with no simulated hardware
  --demo            the default: control plane plus `farmd demo`
  --build           rebuild the image even if it is already in the daemon
  --no-forward      do the whole install and the checks, then exit without
                    holding a port-forward open
  -h, --help        this

Environment
  FARM_LOCAL_NAMESPACE  (df-local)   FARM_LOCAL_RELEASE  (df)
  FARM_LOCAL_IMAGE      (device-farmer/farmd:local, and it must carry a tag)
  FARM_LOCAL_PORT       (8420)       FARM_LOCAL_TOKEN    (local-evaluation)
  FARM_LOCAL_HOSTS      (2)          FARM_LOCAL_DEVICES  (56)
  FARM_LOCAL_ALLOW_ANY_CONTEXT       install into a context this script does
                                     not recognise as a local one
EOF
}

die() {
	printf '\nk8s-up: %s\n' "$1" >&2
	exit 1
}
say() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is not on PATH, and this script cannot work without it."; }

while [ $# -gt 0 ]; do
	case "$1" in
	--farm) MODE=farm ;;
	--demo) MODE=demo ;;
	--build) BUILD=always ;;
	--no-forward) FORWARD=0 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument $1. Try --help." ;;
	esac
	shift
done

# The tag is not optional. With image.tag empty the chart falls back to
# .Chart.AppVersion — 0.1.0 — which nothing in this repository has ever built,
# so the pods would look for an image that is not on the node and not in any
# registry.
case "${IMAGE##*/}" in
*:*)
	IMAGE_REPO=${IMAGE%:*}
	IMAGE_TAG=${IMAGE##*:}
	;;
*) die "FARM_LOCAL_IMAGE=$IMAGE has no tag. Name one: device-farmer/farmd:local" ;;
esac

need docker
need kubectl
need helm

# ---------------------------------------------------------------------------
# Which cluster, and is it one we are allowed to do this to
# ---------------------------------------------------------------------------
CONTEXT=$(kubectl config current-context 2>/dev/null || true)
[ -n "$CONTEXT" ] || die "kubectl has no current context. Start a local cluster first — Docker Desktop's Kubernetes, kind, k3d, minikube or Rancher Desktop all work."
kubectl cluster-info >/dev/null 2>&1 ||
	die "kubectl cannot reach the cluster in context \"$CONTEXT\"."

# A guard, not a nicety. This script creates a Postgres whose storage dies with
# its pod and installs a release whose api token is published in a public git
# repository. Both are correct on a laptop and indefensible anywhere else, and
# the difference between the two is one stale kubeconfig context.
case "$CONTEXT" in
docker-desktop | kind-* | k3d-* | minikube | rancher-desktop | colima | orbstack) ;;
*)
	if [ "${FARM_LOCAL_ALLOW_ANY_CONTEXT:-0}" != "1" ]; then
		die "kubectl's current context is \"$CONTEXT\", which is not a name this script
recognises as a local cluster (docker-desktop, kind-*, k3d-*, minikube,
rancher-desktop, colima, orbstack).

It would install: a Postgres on emptyDir storage that loses every lease and job
on a pod restart, and an api token that is written down in this repository.

If that context really is your own machine:

  FARM_LOCAL_ALLOW_ANY_CONTEXT=1 bash scripts/k8s-up.sh"
	fi
	;;
esac

# ---------------------------------------------------------------------------
# The image
# ---------------------------------------------------------------------------
if [ "$BUILD" = always ] || ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	say "building $IMAGE"
	# The same build docker-compose.yml runs; the Dockerfile runs `go vet` and
	# the test suite inside it, so this is slow the first time and cached after.
	# VERSION reaches `farmd version` in the image (-X main.version), so a pod
	# on this cluster can be asked which of your builds it is. Note that
	# /api/v1/capabilities will still say "dev": it reads debug.ReadBuildInfo()
	# rather than that variable, and neither this script nor the chart can
	# change what it reports.
	docker build \
		--build-arg VERSION="$IMAGE_TAG" \
		--build-arg COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)" \
		-t "$IMAGE" "$ROOT" ||
		die "docker build failed. Nothing has been created in the cluster."
else
	echo "image $IMAGE is already in the daemon (--build rebuilds it)"
fi

# How a locally built image gets into a local cluster, and why that is done
# explicitly rather than left to whatever the machine happens to do.
#
# The received wisdom is that a local Kubernetes "just sees" the Docker
# daemon's images. It is not that simple, and it is not the same on two
# machines. Docker Desktop's kind-mode cluster does NOT hold them — `crictl
# images` on the node lists none of them — and a pod naming one still starts,
# because /etc/containerd/certs.d/_default/hosts.toml points every registry at
# http://registry-mirror:1273, the desktop-containerd-registry-mirror
# container, which serves this daemon's store. Measured: a tag invented
# seconds earlier and published nowhere pulls in about 40 ms.
#
# That is a Docker Desktop feature, not a Kubernetes one. A plain kind
# cluster, k3d and minikube ship no such mirror, and on those an unimported
# image really does end in ImagePullBackOff. So the image is imported: two
# seconds for 36 MB, the same behaviour on every local cluster, and no part of
# the path resting on a component whose presence is Docker Desktop's to change.
#
# Detect, never assume. The node is looked up by name and the daemon is asked
# whether a container by that name is running here — true for kind
# (`<cluster>-control-plane`), for Docker Desktop's kind mode
# (`desktop-control-plane`), for k3d (`k3d-<cluster>-server-0`) and for
# minikube's docker driver, and false for a remote cluster, where the honest
# answer is to refuse with instructions rather than to install something that
# cannot start.
#
# EVERY such node, not the first one. A multi-node kind cluster schedules pods
# wherever it likes, and an image on one node out of three is an
# ImagePullBackOff two thirds of the time — the intermittent kind of failure,
# which is the expensive kind.
NODE_CONTAINERS=""
LOADER=""
detect_loader() {
	local node
	for node in $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
		[ "$(docker inspect -f '{{.State.Running}}' "$node" 2>/dev/null)" = "true" ] || continue
		# MSYS_NO_PATHCONV is Windows-only and inert everywhere else. Git Bash
		# rewrites arguments that look like absolute paths into Windows paths
		# before the process sees them, which mangles anything passed to a
		# command running INSIDE a Linux container. Nothing below takes a path
		# today; the variable is here so that the first edit that does adds one
		# working line instead of one baffling bug.
		if MSYS_NO_PATHCONV=1 docker exec "$node" ctr --version >/dev/null 2>&1; then
			LOADER=ctr
			NODE_CONTAINERS="$NODE_CONTAINERS $node"
		elif [ -z "$LOADER" ] && MSYS_NO_PATHCONV=1 docker exec "$node" docker version >/dev/null 2>&1; then
			# minikube's docker driver with the docker runtime: one node, and
			# the kubelet reads the daemon inside it.
			LOADER=docker-in-node
			NODE_CONTAINERS=" $node"
		fi
	done
	[ -n "$LOADER" ] && return 0
	if command -v minikube >/dev/null 2>&1 && [ "${CONTEXT}" = minikube ]; then
		LOADER=minikube
		return 0
	fi
	return 1
}

load_image() {
	local node
	case "$LOADER" in
	ctr)
		# The kind recipe without needing kind installed: the node is an
		# ordinary container in this daemon, and it ships ctr. k8s.io is the
		# containerd namespace the kubelet reads; an import into the default
		# namespace lands nowhere the kubelet looks.
		for node in $NODE_CONTAINERS; do
			docker save "$IMAGE" |
				MSYS_NO_PATHCONV=1 docker exec -i "$node" ctr -n k8s.io images import -
		done
		;;
	docker-in-node)
		for node in $NODE_CONTAINERS; do
			docker save "$IMAGE" | MSYS_NO_PATHCONV=1 docker exec -i "$node" docker load
		done
		;;
	minikube) minikube image load "$IMAGE" ;;
	esac
}

say "loading $IMAGE into the cluster"
detect_loader || die "the cluster in context \"$CONTEXT\" is not one this script knows how to
side-load an image into: none of its nodes is a container running in this
Docker daemon, and minikube is not driving it.

That is not a failure you want to discover as ImagePullBackOff, so nothing has
been installed. Push the image somewhere the cluster can pull from, and point
this script at it:

  docker tag $IMAGE registry.example.com/$IMAGE_REPO:$IMAGE_TAG
  docker push registry.example.com/$IMAGE_REPO:$IMAGE_TAG
  FARM_LOCAL_IMAGE=registry.example.com/$IMAGE_REPO:$IMAGE_TAG bash scripts/k8s-up.sh"

case "$LOADER" in
ctr | docker-in-node) echo "node container(s):$NODE_CONTAINERS (via $LOADER)" ;;
minikube) echo "minikube image load" ;;
esac
load_image

if [ "$LOADER" = ctr ]; then
	# Verified, not assumed, and on every node it was written to. A silent
	# import failure is exactly the shape of bug this whole section exists to
	# prevent.
	for node in $NODE_CONTAINERS; do
		MSYS_NO_PATHCONV=1 docker exec "$node" ctr -n k8s.io images ls -q |
			grep -qF "$IMAGE" || die "the import into $node reported success and $IMAGE is not there."
	done
fi

# ---------------------------------------------------------------------------
# Namespace and the evaluation database
# ---------------------------------------------------------------------------
kubectl get namespace "$NS" >/dev/null 2>&1 || {
	say "creating namespace $NS"
	kubectl create namespace "$NS"
}

say "evaluation Postgres"
kubectl -n "$NS" apply -f "$ROOT/deploy/local/postgres.yaml"
# Before helm, not after. The chart migrates in a pre-install hook: if Postgres
# is not accepting connections yet, the hook fails, helm aborts the release,
# and the message names the Job rather than the database it could not reach.
kubectl -n "$NS" rollout status deployment/evaluation-postgres --timeout=300s

# ---------------------------------------------------------------------------
# The chart
# ---------------------------------------------------------------------------
say "installing the chart (release $RELEASE in namespace $NS)"
VALUES=(-f "$ROOT/deploy/helm/local-values.yaml")
[ "$MODE" = demo ] && VALUES+=(-f "$ROOT/deploy/local/demo-values.yaml")

if [ "$MODE" = farm ]; then
	# Before helm, not after, and this ordering is the whole point. Switching a
	# cluster that was in demo mode leaves the demo pod running, and the same
	# upgrade brings the jobrunner back — the one combination that must never
	# exist, because that jobrunner claims the simulation's jobs and dials
	# device endpoints that only resolve inside the demo pod. Removing the
	# hardware first means the two never overlap.
	kubectl -n "$NS" delete deployment device-farmer-demo --ignore-not-found --wait
fi

echo "this waits for the migration hook and then for every Deployment;"
echo "if it is still here in a minute:  kubectl -n $NS get pods"
# --timeout 16m outlives migrate.activeDeadlineSeconds (900s). Helm's default
# is 5 minutes, which expires first: the Job is still Running, its pod's real
# status is visible only to `kubectl describe`, and what helm prints is a
# timeout that names neither. The chart deliberately keeps a failed migration's
# pod so `kubectl logs` has something to show, and at the default timeout
# nobody is still watching when it gets there.
# The image id, so that rebuilding under an unmoved tag actually reaches the
# cluster. Kubernetes rolls a Deployment when its pod TEMPLATE changes, and
# `--set image.tag=local` renders the same template whatever the tag now points
# at: a rebuild imported cleanly, helm reported "deployed", and the pods went on
# running the binary from before the edit. Nothing said so — the only way to
# find out was to compare .status.containerStatuses[].imageID against
# `docker images --no-trunc`. Stamping the id into an annotation makes a changed
# image a changed template. An unchanged image stamps the same value and rolls
# nothing, so re-running this script stays free.
IMAGE_ID=$(docker images --no-trunc --format '{{.ID}}' "$IMAGE" | head -n 1)
[ -n "$IMAGE_ID" ] || die "built $IMAGE but docker cannot report its id, so a
rebuild could not be told from the image already running in the cluster."

helm upgrade --install "$RELEASE" "$ROOT/deploy/helm/device-farmer" \
	--namespace "$NS" \
	"${VALUES[@]}" \
	--set image.repository="$IMAGE_REPO" \
	--set image.tag="$IMAGE_TAG" \
	--set podAnnotations."device-farmer\\.io/image-id"="$IMAGE_ID" \
	--wait --timeout 16m

# Ask the cluster for the release's names rather than re-deriving them. The
# chart's fullname has a special case — a release name that already contains
# the chart name is not prefixed again — and a second implementation of that
# rule would be right until somebody names a release "device-farmer-eval".
API_SVC=$(kubectl -n "$NS" get svc \
	-l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=api" \
	-o jsonpath='{.items[0].metadata.name}')
[ -n "$API_SVC" ] || die "the release installed but has no api Service."
API_PORT=$(kubectl -n "$NS" get svc "$API_SVC" -o jsonpath='{.spec.ports[0].port}')
FULLNAME=${API_SVC%-api}

# ---------------------------------------------------------------------------
# Simulated hardware
# ---------------------------------------------------------------------------
if [ "$MODE" = demo ]; then
	say "simulated hardware: $DEVICES devices across $HOSTS hosts"
	sed -e "s|__IMAGE__|$IMAGE|g" \
		-e "s|__IMAGE_ID__|$IMAGE_ID|g" \
		-e "s|__CONFIGMAP__|$FULLNAME-config|g" \
		-e "s|__DB_SECRET__|$FULLNAME-db|g" \
		-e "s|__AUTH_SECRET__|$FULLNAME-auth|g" \
		-e "s|__HOSTS__|$HOSTS|g" \
		-e "s|__DEVICES__|$DEVICES|g" \
		"$ROOT/deploy/local/demo.yaml" | kubectl -n "$NS" apply -f -
	kubectl -n "$NS" rollout status deployment/device-farmer-demo --timeout=300s

	# Wait for the devices to exist, not just for the pod to be Running. The
	# deliverable is a dashboard with something in it, and "Running" is true
	# several seconds before the seed lands.
	printf 'seeding'
	n=0
	for _ in $(seq 1 60); do
		n=$(kubectl -n "$NS" exec deployment/evaluation-postgres -- \
			psql -U farm -d device_farmer -tAc 'SELECT count(*) FROM farm.devices' 2>/dev/null | tr -d '\r' || true)
		# Anything that is not a plain count — an exec that failed, a database
		# still starting — counts as zero rather than crashing the test below.
		case "$n" in '' | *[!0-9]*) n=0 ;; esac
		[ "$n" -ge "$DEVICES" ] && break
		printf '.'
		sleep 2
	done
	echo " $n devices in farm.devices"
fi

# ---------------------------------------------------------------------------
# Look at it
# ---------------------------------------------------------------------------
PF_PID=""
cleanup() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

say "port-forward 127.0.0.1:$PORT -> svc/$API_SVC:$API_PORT"
PF_LOG=$(mktemp)
kubectl -n "$NS" port-forward "svc/$API_SVC" "$PORT:$API_PORT" >"$PF_LOG" 2>&1 &
PF_PID=$!

for _ in $(seq 1 40); do
	curl -sf -o /dev/null "http://127.0.0.1:$PORT/healthz" && break
	kill -0 "$PF_PID" 2>/dev/null || {
		cat "$PF_LOG" >&2
		die "the port-forward died. Is 127.0.0.1:$PORT already taken? FARM_LOCAL_PORT picks another."
	}
	sleep 1
done

# Three facts, measured rather than claimed. The last two are the ones worth
# having: NOTES.txt says in as many words that a mounted FARM_API_TOKENS is not
# the same as authentication being on, because that is a property of the image,
# not of the values. So ask the running api.
health=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz")
dash=$(curl -s -o /dev/null -w '%{http_code} %{size_download}' "http://127.0.0.1:$PORT/")
anon=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/api/v1/capabilities")
auth=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
	"http://127.0.0.1:$PORT/api/v1/capabilities")

echo
kubectl -n "$NS" get pods
cat <<EOF

  healthz                          $health
  dashboard /                      $dash  (status, bytes)
  /api/v1/capabilities             $anon with no credential
  /api/v1/capabilities             $auth with the token

EOF
[ "$anon" = 401 ] || echo "  WARNING: an unauthenticated call was not refused. Read \"auth\" in the capability report before trusting this cluster with anything." >&2
[ "$auth" = 200 ] || echo "  WARNING: the token did not authenticate. kubectl -n $NS logs -l app.kubernetes.io/component=api" >&2

cat <<EOF
  DASHBOARD   http://127.0.0.1:$PORT/
  TOKEN       $TOKEN

The dashboard asks for that token: open it, use "Set API token", paste it. The
page is served without one — the API under it is not.

EOF

if [ "$MODE" = demo ]; then
	cat <<EOF
$DEVICES of those devices are simulated and no phone is attached. Watch the one
line the whole system exists for — a device drops off the USB bus mid-lease and
the lease does NOT end:

  kubectl -n $NS logs -f deployment/device-farmer-demo

Helm's notes above say NO WATCHDOGS WERE CREATED. True of the chart, and not the
whole story here: the demo runs one watchdog per host it invented, in the pod
where those hosts' ADB sockets exist. Same reason jobrunner and recovery are at
0/0 — deploy/local/demo-values.yaml explains it.

EOF
else
	cat <<EOF
ON A FRESH CLUSTER THE FLEET GRID IS EMPTY, and that is correct rather than
broken: --farm deploys the control plane and no hardware, so nothing has
registered a device. (Switched over from demo mode? Those simulated devices are
still in the database and their hosts are now unreachable. Tear it down and
start again for a clean farm.)

What there is to look at is what the api says it can do:

  curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:$PORT/api/v1/capabilities

Run \`farmd node\` on a machine with phones plugged into it to fill the grid; it
cannot run in the cluster, because it needs /dev/bus/usb. See deploy/helm/README.md.

EOF
fi

cat <<EOF
  remove all of it   bash scripts/k8s-down.sh

EOF

if [ "$FORWARD" = 1 ]; then
	echo "Ctrl-C closes the tunnel. The cluster keeps running; reopen it with"
	echo "  kubectl -n $NS port-forward svc/$API_SVC $PORT:$API_PORT"
	wait "$PF_PID"
else
	echo "reopen the tunnel with"
	echo "  kubectl -n $NS port-forward svc/$API_SVC $PORT:$API_PORT"
fi
