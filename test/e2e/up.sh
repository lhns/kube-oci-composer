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
