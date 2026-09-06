#!/usr/bin/env bash
# k8s-down.sh — remove everything scripts/k8s-up.sh made.
#
#   bash scripts/k8s-down.sh
#
# In order: the helm release, the namespace with the evaluation Postgres and
# the demo in it, and the copy of the image this machine pushed into the
# cluster's node. What is deliberately left behind is named at the end — a
# built image in your Docker daemon is not something a teardown should throw
# away without saying so.
#
# It removes a namespace, so it first checks that the namespace is one of ours.
# The check is not paranoia about this script: FARM_LOCAL_NAMESPACE is an
# environment variable, and the shell that ran `up` is often not the shell that
# runs `down`.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)

NS=${FARM_LOCAL_NAMESPACE:-df-local}
RELEASE=${FARM_LOCAL_RELEASE:-df}
IMAGE=${FARM_LOCAL_IMAGE:-device-farmer/farmd:local}

KEEP_IMAGE=0
while [ $# -gt 0 ]; do
	case "$1" in
	--keep-image) KEEP_IMAGE=1 ;;
	-h | --help)
		cat <<'EOF'
k8s-down.sh — remove everything scripts/k8s-up.sh made.

Options
  --keep-image   leave the image in the cluster node's containerd store, so
                 the next `up` does not have to copy it in again
  -h, --help     this

Environment
  FARM_LOCAL_NAMESPACE (df-local)   FARM_LOCAL_RELEASE (df)
  FARM_LOCAL_IMAGE     (device-farmer/farmd:local)
EOF
		exit 0
		;;
	*)
		echo "k8s-down: unknown argument $1. Try --help." >&2
		exit 1
		;;
	esac
	shift
done

die() {
	printf '\nk8s-down: %s\n' "$1" >&2
	exit 1
}
say() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

command -v kubectl >/dev/null 2>&1 || die "kubectl is not on PATH."
kubectl cluster-info >/dev/null 2>&1 ||
	die "kubectl cannot reach a cluster; nothing to tear down from here."

if ! kubectl get namespace "$NS" >/dev/null 2>&1; then
	echo "namespace $NS does not exist — nothing to remove."
else
	# Ours, or somebody else's? A namespace with neither the release nor the
	# evaluation Postgres in it was not created by scripts/k8s-up.sh, and
	# deleting a namespace takes everything in it with no way back.
	MINE=0
	kubectl -n "$NS" get deployment evaluation-postgres >/dev/null 2>&1 && MINE=1
	if command -v helm >/dev/null 2>&1; then
		helm -n "$NS" status "$RELEASE" >/dev/null 2>&1 && MINE=1
	fi
	[ "$MINE" = 1 ] || die "namespace \"$NS\" holds neither the release \"$RELEASE\" nor the
deployment evaluation-postgres, so scripts/k8s-up.sh did not create it and this
script will not delete it. If you meant a different one:

  FARM_LOCAL_NAMESPACE=<ns> bash scripts/k8s-down.sh"

	if command -v helm >/dev/null 2>&1 && helm -n "$NS" status "$RELEASE" >/dev/null 2>&1; then
		# Before the namespace, so helm records the uninstall instead of
		# discovering later that its release secret vanished underneath it.
		say "uninstalling release $RELEASE"
		helm -n "$NS" uninstall "$RELEASE" --wait --timeout 5m || true
	fi

	case "$NS" in
	default | kube-system | kube-public | kube-node-lease)
		# Somebody pointed the whole thing at a namespace that predates it.
		# Take back what we applied and leave the namespace alone.
		say "removing this project's objects from $NS (the namespace itself is not ours to delete)"
		kubectl -n "$NS" delete -f "$ROOT/deploy/local/postgres.yaml" --ignore-not-found
		kubectl -n "$NS" delete deployment device-farmer-demo --ignore-not-found
		;;
	*)
		say "deleting namespace $NS"
		kubectl delete namespace "$NS" --wait --timeout=300s
		;;
	esac
fi

# The image this machine pushed into the node's containerd store. It is a copy,
# not the original: `docker images` still has what was built.
if [ "$KEEP_IMAGE" = 0 ] && command -v docker >/dev/null 2>&1; then
	for node in $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
		[ "$(docker inspect -f '{{.State.Running}}' "$node" 2>/dev/null)" = "true" ] || continue
		# MSYS_NO_PATHCONV is Windows-only and inert elsewhere; see the same
		# note in scripts/k8s-up.sh.
		MSYS_NO_PATHCONV=1 docker exec "$node" ctr --version >/dev/null 2>&1 || continue
		# The tag AND the digest. `ctr images import` registers the tag and a
		# reference by digest, and removing only the tag leaves the second one
		# pinning 36 MB on the node for every build ever loaded. Removing a
		# digest reference cannot take content another tag is using: containerd
		# counts references, so content still named by a tag survives.
		refs=$(MSYS_NO_PATHCONV=1 docker exec "$node" ctr -n k8s.io images ls -q 2>/dev/null | grep -F "$IMAGE" || true)
		digest=$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)
		if [ -n "$digest" ]; then
			refs="$refs
$(MSYS_NO_PATHCONV=1 docker exec "$node" ctr -n k8s.io images ls -q 2>/dev/null | grep -F "@$digest" || true)"
		fi
		for ref in $refs; do
			say "removing $ref from node $node"
			MSYS_NO_PATHCONV=1 docker exec "$node" ctr -n k8s.io images rm "$ref" >/dev/null || true
		done
	done
fi

cat <<EOF

Gone. What is still on this machine, deliberately:

  the image $IMAGE in your Docker daemon — build output, and deleting it would
  make the next \`up\` rebuild from scratch. Reclaim it with:
      docker rmi $IMAGE

  nothing else. There is no PersistentVolume to clean up: the evaluation
  Postgres ran on an emptyDir, which is the whole reason it was safe to ship.
EOF
