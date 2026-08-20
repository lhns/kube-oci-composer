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

# The registry host baked into every composed reference, and the NodePort the node actually
# reaches it on. They differ on purpose: the host is what appears in image references and resolves
# nowhere; the port is where containerd is told to go. Same split as the real deployment.
SERVING_HOST="${SERVING_HOST:-kube-oci-composer.oci-composer.svc.cluster.local:5000}"
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
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/${SERVING_HOST}"
  docker exec -i "$node" tee "/etc/containerd/certs.d/${SERVING_HOST}/hosts.toml" >/dev/null <<EOF
# Plain HTTP: this is a NodePort on the node itself, and containerd defaults to HTTPS for any
# host:port, so without the scheme here the pull dies in the TLS handshake.
[host."http://localhost:${NODE_PORT}"]
  capabilities = ["pull", "resolve"]
EOF
done

make docker-build IMG="$IMG"
kind load docker-image "$IMG" --name "$CLUSTER"

kubectl apply -f config/crd/bases

helm upgrade --install kube-oci-composer charts/kube-oci-composer \
  --namespace oci-composer --create-namespace \
  --set image.repository="${IMG%:*}" \
  --set image.tag="${IMG##*:}" \
  --set image.pullPolicy=Never \
  --set operator.servingHost="$SERVING_HOST" \
  --set service.type=NodePort \
  --set service.nodePort="$NODE_PORT" \
  --wait --timeout 5m

kubectl -n oci-composer rollout status deploy/kube-oci-composer --timeout=5m

# --- ImageBuild ------------------------------------------------------------------------------
#
# The builder is a SECOND component with its own chart and RBAC (ADR 0004), so it is installed
# separately here exactly as an operator would install it.
#
# Its prerequisites are the interesting part. A build pushes to a registry, so one runs in the
# cluster; it is plain HTTP, so the builder is told to allow that host and only that host. And the
# build context comes from a Flux source, which this cluster does not run -- so a minimal
# GitRepository CRD stands in and the harness publishes status.artifact itself, pointing at a
# tarball served from a ConfigMap. That tests this controller's reading of the contract rather than
# testing Flux.

BUILDER_IMG="${BUILDER_IMG:-ghcr.io/lhns/kube-oci-builder:e2e}"
E2E_REGISTRY="e2e-registry.${BUILD_NS}.svc.cluster.local:5000"

make docker-build-builder BUILDER_IMG="$BUILDER_IMG"
kind load docker-image "$BUILDER_IMG" --name "$CLUSTER"

kubectl apply -f config/crd/bases/oci.lhns.de_imagebuilds.yaml
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

kubectl -n "$BUILD_NS" apply -f "$HERE/manifests/registry.yaml"
kubectl -n "$BUILD_NS" apply -f "$HERE/manifests/context-server.yaml"
kubectl -n "$BUILD_NS" rollout status deploy/e2e-registry --timeout=3m
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

# refreshInterval is 5s against the registry's 30s window (manifests/registry.yaml), a margin of 6
# where a deployment runs 1h against 30 days for 720. The RATIO is what is reproduced here, not
# the numbers: the retention tests need the controller to visibly keep something alive inside a
# test run, and to visibly stop once the object naming it is gone.
helm upgrade --install kube-oci-builder charts/kube-oci-builder \
  --namespace oci-builder --create-namespace \
  --set image.repository="${BUILDER_IMG%:*}" \
  --set image.tag="${BUILDER_IMG##*:}" \
  --set image.pullPolicy=Never \
  --set builder.insecureRegistry="$E2E_REGISTRY" \
  --set builder.retention.refreshInterval=5s \
  --wait --timeout 5m

kubectl -n oci-builder rollout status deploy/kube-oci-builder --timeout=5m
