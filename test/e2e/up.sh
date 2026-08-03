#!/usr/bin/env bash
# Creates the kind cluster the e2e tests run against, and loads the controller image into it.
set -euo pipefail

CLUSTER="${CLUSTER:-kube-oci-composer-e2e}"
# Image volumes need a kubelet that supports them. Pinned rather than "latest" so that a failure
# means the feature is broken, not that the node image drifted.
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.33.1}"
IMG="${IMG:-ghcr.io/lhns/kube-oci-composer:e2e}"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE"
fi

make docker-build IMG="$IMG"
kind load docker-image "$IMG" --name "$CLUSTER"

kubectl apply -f config/crd/bases

helm upgrade --install kube-oci-composer charts/kube-oci-composer \
  --namespace oci-composer --create-namespace \
  --set image.repository="${IMG%:*}" \
  --set image.tag="${IMG##*:}" \
  --set image.pullPolicy=Never \
  --set operator.servingHost=kube-oci-composer.oci-composer.svc.cluster.local:5000 \
  --wait --timeout 5m

kubectl -n oci-composer rollout status deploy/kube-oci-composer --timeout=5m
