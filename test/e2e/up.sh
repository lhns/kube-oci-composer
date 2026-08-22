#!/usr/bin/env bash
# Creates the kind cluster the e2e tests run against, and loads the controller image into it.
set -euo pipefail

CLUSTER="${CLUSTER:-kube-oci-composer-e2e}"
# Image volumes need BOTH a kubelet that supports them and a runtime that implements the CRI side.
# The kubelet half is beta from 1.33; the runtime half landed in containerd 2.1, and the 1.33 node
# image ships 2.0.x. On that combination the pod is admitted and RUNS with nothing mounted, which
# reads as the composer having produced the wrong layout. 1.36 matches the cluster this operator
# is deployed to (containerd 2.3.x).
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.36.1}"
IMG="${IMG:-ghcr.io/lhns/kube-oci-composer:e2e}"
# The namespace the ImageBuild fixtures live in. Builds run in their object's namespace, so this
# is also where the build Jobs appear.
BUILD_NS="${BUILD_NS:-oci-builder-e2e}"

# The registry host baked into every published reference, and the NodePort the node actually reaches
# it on.
#
# This is the PUBLIC name only -- what a kubelet resolves, and what status.artifact.ref reports.
# The controllers never see it; they use the registry's in-cluster Service.
#
# Deliberately not a generic "oci.e2e": a cluster may well run other registries, and a name this
# specific cannot be mistaken for the only one.
REGISTRY_HOST="${REGISTRY_HOST:-oci-composer.e2e:5000}"
NODE_PORT="${NODE_PORT:-30500}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$HERE/kind-config.yaml"
fi

# Tell containerd where the registry is. Image volumes are pulled by the KUBELET using the NODE's
# resolver, so the .svc.cluster.local host in every reference resolves to nothing; this drop-in
# supplies the endpoint directly and containerd never looks the name up. Without it the pull fails
# with ErrImagePull even though the Service is perfectly healthy.
#
# Written after the node exists rather than baked in: certs.d is read per-pull, so no containerd
# restart is needed. Only `config_path` (in kind-config.yaml) has to be set at startup.
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
  docker exec -i "$node" tee "/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml" >/dev/null <<EOF
# Plain HTTP: this is a NodePort on the node itself, and containerd defaults to HTTPS for any
# host:port, so without the scheme here the pull dies in the TLS handshake.
[host."http://localhost:${NODE_PORT}"]
  capabilities = ["pull", "resolve"]
EOF
done

BUILDER_IMG="${BUILDER_IMG:-ghcr.io/lhns/kube-oci-builder:e2e}"

# Everything publishes here now: compositions and builds alike (ADR 0035). The drop-in above points
# this name at the registry's NodePort, so a Pod can actually pull what gets published -- which is
# what the image-volume tests exercise.
E2E_REGISTRY="$REGISTRY_HOST"

# BOTH images, before the single install that references them. imagePullPolicy is Never in the e2e,
# so an image that is not loaded is ErrImageNeverPull rather than a pull attempt.
make docker-build IMG="$IMG"
make docker-build-builder BUILDER_IMG="$BUILDER_IMG"
kind load docker-image "$IMG" --name "$CLUSTER"
kind load docker-image "$BUILDER_IMG" --name "$CLUSTER"

# CRDs are NOT applied here any more: the chart installs them from templates/ (ADR 0033), and Helm
# refuses to adopt a CRD it did not create. Letting the chart do it also means the e2e exercises
# that path rather than working around it.

# ONE chart, all three components (ADR 0033). The registry it installs is the one everything
# publishes to -- no hand-rolled fixture registry any more, so the e2e exercises the deployment an
# operator actually gets.
#
# The retention policy is compressed to a 30s window against a 1s refresh -- a margin of 30, where a
# deployment runs 30 days against 1h for 720. PROPORTIONATE, not merely small: the chart refuses to
# render a margin below 24 (threat D7), and it is right to, because the margin is the guarantee.
# Compressing the window without compressing the interval with it would be exactly the misconfigured
# state that check exists to catch, so the e2e must not be the first thing to work around it.
#
# SCOPED to keepalive-* repositories, because a repository matching no policy is never collected:
# that keeps every other test's images safe from a window measured in seconds while the retention
# tests still get to watch something expire.
# No defaultRegistry.insecure: the controllers never connect to the public name, and the in-cluster
# Service they DO connect to is marked insecure by the chart automatically.
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
  --set registry.retention.window=30s \
  --set registry.retention.gcInterval=5s \
  --set registry.retention.gcDelay=1s \
  --set registry.logLevel=debug \
  --set operator.retention.refreshInterval=1s \
  --set imageBuild.retention.refreshInterval=1s \
  --wait --timeout 5m

# NO CoreDNS entry, and its absence is the assertion.
#
# An earlier version taught cluster DNS about the public name, because the controllers were pointed
# at it and could not resolve it. They are not any more: --default-registry is the registry's own
# Service and --public-registry-host carries the node-resolvable name into status.artifact.ref and
# nowhere else. If this suite passes without the twenty lines that used to be here, the split is
# right; that is what this deletion tests.

kubectl -n oci-composer rollout status deploy/kube-oci-composer --timeout=5m
kubectl -n oci-composer rollout status deploy/kube-oci-composer-builder --timeout=5m

# --- ImageBuild fixtures ----------------------------------------------------------------------
#
# The controller is already installed above -- one chart, all components (ADR 0033). What is left is
# what a build NEEDS: a build context from a Flux source, which this cluster does not run. A minimal
# GitRepository CRD stands in and the harness publishes status.artifact itself, pointing at a tarball
# served from a ConfigMap, so this tests the controller's reading of the contract rather than testing
# Flux.

kubectl apply -f "$HERE/../crds/gitrepository.yaml"

kubectl create namespace "$BUILD_NS" --dry-run=client -o yaml | kubectl apply -f -

# The context tarball, built here so the Dockerfile lives in the repository as a readable file
# rather than as a base64 blob in a manifest. The wrapper directory mimics what source-controller
# produces, which both FetchDockerfile and the build pod's fetch script have to strip.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/src-e2e"
cp "$HERE/manifests/dockerfile" "$WORK/src-e2e/Dockerfile"
cp "$HERE/manifests/dockerfile.unpinned" "$WORK/src-e2e/Dockerfile.unpinned"
cp "$HERE/manifests/dockerfile.other" "$WORK/src-e2e/Dockerfile.other"
tar -czf "$WORK/context.tar.gz" -C "$WORK" src-e2e

kubectl -n "$BUILD_NS" create configmap e2e-context \
  --from-file=context.tar.gz="$WORK/context.tar.gz" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$BUILD_NS" apply -f "$HERE/manifests/context-server.yaml"
# The registry is part of the release now, so `helm --wait` above already waited for it.
# statefulset, not deploy: the registry became one so that clustering could never be a kind switch
# under a running install (ADR 0039).
kubectl -n oci-composer rollout status statefulset/kube-oci-composer-registry --timeout=3m
kubectl -n "$BUILD_NS" rollout status deploy/e2e-context --timeout=3m

# The source's published artifact. Nothing verifies this digest -- a real source-controller is what
# would have computed it -- but it must be STABLE, since the input hash is built from it and a
# changing value would rebuild on every reconcile.
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


