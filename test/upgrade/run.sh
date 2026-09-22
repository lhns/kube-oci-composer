#!/usr/bin/env bash
# Upgrade from the last release to this checkout, and check nothing published is lost or rebuilt.
#
# The unit tests and the e2e both start from a fresh install. This is the one check that runs the
# upgrade an operator runs: install the previous release's chart and images, publish with it, then
# `helm upgrade` to this tree and assert that
#
#   - no object rebuilds or stalls, and every digest is unchanged (an input-hash change would
#     reassemble every composition, and a spec-hash tag under onConflict: Fail then stalls for good);
#   - every artifact, history entry and SBOM referrer gains its digest- tag (ADR 0060's backfill),
#     suspended objects included;
#   - NOTES warns on the upgrade that stops keeping untagged content;
#   - after collection has had a chance to run with keepUntagged off, everything live still resolves.
#
# Expects a kind cluster (CLUSTER), helm, kubectl, jq and docker. PREVIOUS is the release to start
# from. The images for this tree are built here unless COMPOSER_IMG/BUILDER_IMG are already loaded.
set -euo pipefail

PREVIOUS="${PREVIOUS:-0.5.1}"
CLUSTER="${CLUSTER:-kube-oci-composer-upgrade}"
NS=oci-composer
APP=upgrade-test
RELEASE=kube-oci-composer
COMPOSER_IMG="${COMPOSER_IMG:-ghcr.io/lhns/kube-oci-composer:upgrade}"
BUILDER_IMG="${BUILDER_IMG:-ghcr.io/lhns/kube-oci-builder:upgrade}"
HERE="$(cd "$(dirname "$0")" && pwd)"
CHART="$HERE/../../charts/kube-oci-composer"

step() { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; kubectl -n "$APP" get imagecompositions -o yaml >&2 || true
         kubectl -n "$NS" logs deploy/"$RELEASE" --tail=80 >&2 || true; exit 1; }

# The same values on every install and upgrade, so the only change is the chart and images.
COMMON=(--namespace "$NS" --set registry.publish.mode=internalOnly --set operator.supplyChain.sbom=true)
NEW_IMAGES=(--set image.repository="${COMPOSER_IMG%:*}" --set image.tag="${COMPOSER_IMG##*:}"
            --set image.pullPolicy=Never
            --set imageBuild.image.repository="${BUILDER_IMG%:*}"
            --set imageBuild.image.tag="${BUILDER_IMG##*:}" --set imageBuild.image.pullPolicy=Never)

# One pod, kept running, that asks the registry things from inside the cluster.
curl_pod() {
  kubectl -n "$APP" get pod curl >/dev/null 2>&1 && return
  kubectl -n "$APP" run curl --restart=Never --image=curlimages/curl:8.19.0@sha256:c03110c736db81bbe1be0296f1f1608c81b954b01626bdfb0a8f84e5bd00ff3c --command -- sleep 3600 >/dev/null
  kubectl -n "$APP" wait --for=condition=Ready pod/curl --timeout=120s >/dev/null
}
# resolves REF -> prints the HTTP status of a HEAD on host/repo's manifest REFERENCE.
resolves() { # $1 = repo (host/path), $2 = tag or digest
  local host="${1%%/*}" path="${1#*/}"
  kubectl -n "$APP" exec curl -- curl -s -o /dev/null -w '%{http_code}' -I \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json' \
    "http://$host/v2/$path/manifests/$2"
}
state() {
  kubectl -n "$APP" get imagecompositions -o json | jq -S '[.items[] | {
    name: .metadata.name, digest: .status.artifact.digest, repo: (.status.artifact.ref | split("@")[0] | sub(":[^/:]*$"; "")),
    sbom: .status.attestations.sbom, history: [.status.history[]?.digest],
    ready: ([.status.conditions[]? | select(.type == "Ready")][0] | {status, reason}),
    stalled: ([.status.conditions[]? | select(.type == "Stalled" and .status == "True")] | length)
  }]'
}
missing_digest_tags() {
  kubectl get imagecompositions,imagebuilds -A -o json | jq -r '.items[]
    | select([.status.artifact // empty] + (.status.history // [])
             | any(.digest and ((.tags // []) | any(test(":digest-|^digest-")) | not)))
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
}

step "install $PREVIOUS"
helm install "$RELEASE" "oci://ghcr.io/lhns/charts/kube-oci-composer" --version "$PREVIOUS" \
  --create-namespace "${COMMON[@]}" --wait --timeout 5m

step "publish with $PREVIOUS: a spec-hash tag under onConflict: Fail, a digest-only one, a suspended one"
kubectl create namespace "$APP"
kubectl -n "$APP" create configmap files --from-literal=hello.txt=hello
for spec in "spechash:[s1a2b3c4]" "digestonly:[]" "suspended:[s5d6e7f8]"; do
  name="${spec%%:*}" tags="${spec#*:}"
  kubectl apply -f - <<EOF
apiVersion: oci.lhns.de/v1alpha1
kind: ImageComposition
metadata: {name: $name, namespace: $APP}
spec:
  interval: 1m
  push: {tags: $tags}
  layers:
    - {name: files, configMap: {name: files}, to: /files}
EOF
done
kubectl -n "$APP" wait --for=condition=Ready imagecomposition --all --timeout=5m
kubectl -n "$APP" patch imagecomposition suspended --type=merge -p '{"spec":{"suspend":true}}'
sleep 5
BEFORE="$(state)"
echo "$BEFORE"
[ "$(echo "$BEFORE" | jq '[.[] | select(.sbom == null or .sbom == "")] | length')" = 0 ] \
  || fail "$PREVIOUS published no SBOM, so this cannot check the referrer backfill"

step "build this tree's images"
if ! docker image inspect "$COMPOSER_IMG" >/dev/null 2>&1; then
  docker build -t "$COMPOSER_IMG" --build-arg CMD=oci-composer "$HERE/../.."
  docker build -t "$BUILDER_IMG" --build-arg CMD=oci-builder "$HERE/../.."
fi
kind load docker-image "$COMPOSER_IMG" "$BUILDER_IMG" --name "$CLUSTER"

step "NOTES warns on the upgrade that removes keepUntagged"
helm upgrade "$RELEASE" "$CHART" "${COMMON[@]}" "${NEW_IMAGES[@]}" --dry-run=server \
  | grep -q "stops the registry keeping untagged" || fail "no upgrade warning on the 0.5.x upgrade"

step "upgrade, keeping untagged content until the backfill is done (step 1 of the procedure)"
helm upgrade "$RELEASE" "$CHART" "${COMMON[@]}" "${NEW_IMAGES[@]}" \
  --set registry.retention.keepUntagged=true --wait --timeout 5m
kubectl -n "$NS" rollout status deploy/"$RELEASE" --timeout=5m

step "every object gains its digest- tag (step 2)"
for _ in $(seq 60); do [ -z "$(missing_digest_tags)" ] && break; sleep 5; done
[ -z "$(missing_digest_tags)" ] || fail "still missing digest- tags: $(missing_digest_tags)"

AFTER="$(state)"
echo "$AFTER"
diff <(echo "$BEFORE" | jq -S '[.[] | {name, digest, history}]') \
     <(echo "$AFTER" | jq -S '[.[] | {name, digest, history}]') \
  || fail "a digest moved on upgrade: an unchanged spec must not be reassembled"
[ "$(echo "$AFTER" | jq '[.[] | select(.stalled > 0)] | length')" = 0 ] || fail "an object stalled on upgrade"
[ "$(echo "$AFTER" | jq -r '.[] | select(.name == "suspended") | .ready.reason')" = Suspended ] \
  || fail "the suspended object is no longer reported suspended"

curl_pod
check_registry() { # everything live resolves, and carries its own tag
  echo "$1" | jq -c '.[]' | while read -r o; do
    repo="$(echo "$o" | jq -r .repo)" digest="$(echo "$o" | jq -r .digest)" sbom="$(echo "$o" | jq -r .sbom)"
    for ref in "$digest" "digest-${digest#sha256:}" "$sbom" "digest-${sbom#sha256:}"; do
      [ "$(resolves "$repo" "$ref")" = 200 ] || fail "$repo does not serve $ref"
    done
  done
}
check_registry "$AFTER"

step "stop keeping untagged content (step 3), compress the clock, and let collection run"
helm upgrade "$RELEASE" "$CHART" "${COMMON[@]}" "${NEW_IMAGES[@]}" \
  --set retention.window=30s --set retention.refreshFactor=30 --set retention.refreshInterval=1s \
  --set registry.retention.gcFactor=6 --set imageBuild.buildPollInterval=3s \
  --set registry.retention.gcMaxSchedulerDelay=1s --wait --timeout 5m
sleep 120
check_registry "$(state)"

step "OK: upgraded from $PREVIOUS with no rebuild, no stall, every object backfilled, nothing lost"
