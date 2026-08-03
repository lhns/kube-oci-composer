#!/usr/bin/env bash
set -euo pipefail
CLUSTER="${CLUSTER:-kube-oci-composer-e2e}"
kind delete cluster --name "$CLUSTER"
