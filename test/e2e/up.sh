#!/usr/bin/env bash
# Creates the kind cluster the e2e tests run against, loads the images and installs the chart.
set -euo pipefail

CLUSTER="${CLUSTER:-kube-oci-composer-e2e}"
# Image volumes need kubelet support AND containerd >= 2.1; on older runtimes the pod runs with
# nothing mounted. 1.36 matches production (containerd 2.3.x).
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.36.1}"
IMG="${IMG:-ghcr.io/lhns/kube-oci-composer:e2e}"
# Namespace of the ImageBuild fixtures, and so of the build Jobs.
BUILD_NS="${BUILD_NS:-oci-builder-e2e}"

# The PUBLIC registry name (what a kubelet resolves and status.artifact.ref reports) and the
# NodePort behind it. The controllers never use it; they use the in-cluster Service.
REGISTRY_HOST="${REGISTRY_HOST:-oci-composer.e2e:5000}"
NODE_PORT="${NODE_PORT:-30500}"

# The retention clock, compressed. Only the window, the refresh and the build poll are chosen; the
# chart derives the rest and the tests read back what was deployed.
E2E_WINDOW="${E2E_WINDOW:-30s}"

# Set explicitly: the chart refuses to DERIVE an interval under 30s, and the explicit value is the
# escape hatch. The 24x margin check still applies.
E2E_REFRESH="${E2E_REFRESH:-1s}"
E2E_REFRESH_FACTOR="${E2E_REFRESH_FACTOR:-30}"

# gcInterval = window / this: a five-second sweep. gcInterval is not what bounds collection
# latency; see E2E_GC_MAX_SCHEDULER_DELAY.
E2E_GC_FACTOR="${E2E_GC_FACTOR:-6}"

# zot delays each repository's collection task by a random amount up to this, so a pass takes
# roughly repositories x delay / 2: ~500s at the 30s default over this suite's ~33 repositories,
# ~20s at 1s. Content still has to expire first, so no assertion is weakened.
E2E_GC_MAX_SCHEDULER_DELAY="${E2E_GC_MAX_SCHEDULER_DELAY:-1s}"

# gcDelay is never derived below 3x this (a build is untagged until named, ADR 0054); at the
# default 15s that is 45s, more than the 30s window allows.
E2E_BUILD_POLL="${E2E_BUILD_POLL:-3s}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$HERE/kind-config.yaml"
fi

# Point containerd at the registry's NodePort for the public name: the kubelet pulls with the NODE's
# resolver, which does not know it. certs.d is read per pull, so no restart is needed; only
# `config_path` (kind-config.yaml) must be set at startup.
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
  docker exec -i "$node" tee "/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml" >/dev/null <<EOF
# Plain HTTP: containerd assumes HTTPS for any host:port.
[host."http://localhost:${NODE_PORT}"]
  capabilities = ["pull", "resolve"]
EOF
done

BUILDER_IMG="${BUILDER_IMG:-ghcr.io/lhns/kube-oci-builder:e2e}"

# Everything publishes here (ADR 0035); the drop-in above makes it pullable by pods.
E2E_REGISTRY="$REGISTRY_HOST"

# Load both images before the install (pullPolicy is Never). CI passes a prebuilt archive; run
# directly, the images are built here. A set-but-missing archive is an error, not a fallback.
E2E_IMAGE_ARCHIVE="${E2E_IMAGE_ARCHIVE:-}"
if [ -n "$E2E_IMAGE_ARCHIVE" ]; then
  if [ ! -f "$E2E_IMAGE_ARCHIVE" ]; then
    echo "E2E_IMAGE_ARCHIVE=$E2E_IMAGE_ARCHIVE does not exist" >&2
    exit 1
  fi
  kind load image-archive "$E2E_IMAGE_ARCHIVE" --name "$CLUSTER"
else
  make docker-build IMG="$IMG"
  make docker-build-builder BUILDER_IMG="$BUILDER_IMG"
  kind load docker-image "$IMG" --name "$CLUSTER"
  kind load docker-image "$BUILDER_IMG" --name "$CLUSTER"
fi

# Fail now rather than as a helm --wait timeout (ErrImageNeverPull) five minutes later.
for node in $(kind get nodes --name "$CLUSTER"); do
  have="$(docker exec "$node" crictl images -o json)"
  for want in "$IMG" "$BUILDER_IMG"; do
    if ! printf '%s' "$have" | grep -q "\"$want\"\|\"docker.io/$want\""; then
      echo "$want is not on node $node after loading" >&2
      exit 1
    fi
  done
done

# One chart, all components, CRDs included (ADR 0033), so the e2e runs what an operator installs.
#
# Retention: a 30s window against a 1s refresh keeps the margin (30) above the chart's floor of 24
# (threat D7) instead of working around it.
#
# Scoped to keepalive-* so the short window only touches the retention tests' images. Scoping does
# NOT protect other repositories' untagged manifests -- zot collects those by default -- so gcDelay
# must clear the naming gap (ADR 0054). deleteUntagged stays true, or the digest-only retention test
# would prove nothing. keepUntagged stays off, the default (ADR 0060); requireKeepUntaggedOff
# refuses to run that test otherwise.
#
# No defaultRegistry.insecure: the controllers only use the in-cluster Service, which the chart
# marks insecure itself.
helm upgrade --install kube-oci-composer charts/kube-oci-composer \
  --namespace oci-composer --create-namespace \
  --set image.repository="${IMG%:*}" \
  --set image.tag="${IMG##*:}" \
  --set image.pullPolicy=Never \
  --set imageBuild.image.repository="${BUILDER_IMG%:*}" \
  --set imageBuild.image.tag="${BUILDER_IMG##*:}" \
  --set imageBuild.image.pullPolicy=Never \
  --set registry.service.type=NodePort \
  --set registry.service.nodePort="$NODE_PORT" \
  --set registry.publish.mode=nodePort \
  --set registry.host="$E2E_REGISTRY" \
  --set 'registry.retention.repositories={keepalive-*,keepalive-**}' \
  --set "retention.window=$E2E_WINDOW" \
  --set "retention.refreshFactor=$E2E_REFRESH_FACTOR" \
  --set "retention.refreshInterval=$E2E_REFRESH" \
  --set "registry.retention.gcFactor=$E2E_GC_FACTOR" \
  --set "registry.retention.gcMaxSchedulerDelay=$E2E_GC_MAX_SCHEDULER_DELAY" \
  --set "imageBuild.buildPollInterval=$E2E_BUILD_POLL" \
  --set registry.logLevel=debug \
  --wait --timeout 5m

# Deliberately NO CoreDNS entry for the public name: the controllers must not need it, and this
# suite passing without one is what shows it.

kubectl -n oci-composer rollout status deploy/kube-oci-composer --timeout=5m
kubectl -n oci-composer rollout status deploy/kube-oci-composer-builder --timeout=5m

# --- ImageBuild fixtures ----------------------------------------------------------------------
#
# Flux is not installed. A minimal GitRepository CRD stands in, and its status.artifact points at a
# tarball served from a ConfigMap, so the controller's reading of the contract is what is tested.

kubectl apply -f "$HERE/../crds/gitrepository.yaml"

kubectl create namespace "$BUILD_NS" --dry-run=client -o yaml | kubectl apply -f -

# The context tarball, built from the readable Dockerfiles in manifests/.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/src-e2e"
cp "$HERE/manifests/dockerfile" "$WORK/src-e2e/Dockerfile"
cp "$HERE/manifests/dockerfile.unpinned" "$WORK/src-e2e/Dockerfile.unpinned"
cp "$HERE/manifests/dockerfile.other" "$WORK/src-e2e/Dockerfile.other"
# Archived from INSIDE src-e2e, so entries sit at the root ("./", "Dockerfile") as in a real
# source-controller artifact. A wrapper directory here once hid a fetcher bug (ADR 0045).
tar -czf "$WORK/context.tar.gz" -C "$WORK/src-e2e" .

kubectl -n "$BUILD_NS" create configmap e2e-context \
  --from-file=context.tar.gz="$WORK/context.tar.gz" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$BUILD_NS" apply -f "$HERE/manifests/context-server.yaml"
kubectl -n oci-composer rollout status statefulset/kube-oci-composer-registry --timeout=3m
kubectl -n "$BUILD_NS" rollout status deploy/e2e-context --timeout=3m

# The source's artifact. Nothing verifies the digest, but it must be STABLE: it feeds the input
# hash, and a changing value would rebuild on every reconcile.
CONTEXT_URL="http://e2e-context.${BUILD_NS}.svc.cluster.local:8080/context.tar.gz"
CONTEXT_DIGEST="sha256:$(sha256sum "$WORK/context.tar.gz" | cut -d' ' -f1)"

kubectl -n "$BUILD_NS" apply -f - <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: e2e-src
spec: {}
status:
  artifact:
    url: ${CONTEXT_URL}
    digest: ${CONTEXT_DIGEST}
    revision: main@sha1:e2e
EOF
