#!/usr/bin/env bash
# Upgrade from the last release to this checkout, and check nothing published is lost or rebuilt.
#
# The unit tests and the e2e both start from a fresh install. This runs the upgrade an operator runs:
# install the previous release's published chart and images, publish with it, then `helm upgrade` to
# this tree and assert that
#
#   - no object rebuilds or stalls, and every digest is unchanged -- judged only after the upgraded
#     controller has fully reconciled each object, which a requestedAt annotation proves;
#   - every artifact, history entry and SBOM referrer gains its digest- tag, or is marked lost if the
#     previous release had already lost it (ADR 0060), suspended objects included;
#   - NOTES warns on the upgrade that stops keeping untagged content;
#   - with keepUntagged off and a compressed clock, a deleted object's content IS collected (the
#     control) and everything live still resolves.
#
# Expects a kind cluster (CLUSTER), helm, kubectl, jq and docker. PREVIOUS is the release to start
# from. The images for this tree are built here unless COMPOSER_IMG/BUILDER_IMG are already present.
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
fail() {
  printf '\nFAIL: %s\n' "$*" >&2
  kubectl -n "$APP" get imagecompositions -o yaml >&2 || true
  kubectl -n "$NS" logs deploy/"$RELEASE" --tail=80 >&2 || true
  exit 1
}

# The same values on every install and upgrade, so the only change is the chart and images.
COMMON=(--namespace "$NS" --set registry.publish.mode=internalOnly --set operator.supplyChain.sbom=true)
NEW_IMAGES=(--set image.repository="${COMPOSER_IMG%:*}" --set image.tag="${COMPOSER_IMG##*:}"
            --set image.pullPolicy=Never
            --set imageBuild.image.repository="${BUILDER_IMG%:*}"
            --set imageBuild.image.tag="${BUILDER_IMG##*:}" --set imageBuild.image.pullPolicy=Never)

composition() { # name, tags (YAML list), configmap, extra push fields
  kubectl apply -f - <<EOF
apiVersion: oci.lhns.de/v1alpha1
kind: ImageComposition
metadata: {name: $1, namespace: $APP}
spec:
  interval: 1m
  push: {tags: $2${4:+, $4}}
  layers:
    - {name: files, configMap: {name: $3}, to: /files}
EOF
}

# One pod, kept running, that asks the registry things from inside the cluster.
curl_pod() {
  kubectl -n "$APP" get pod curl >/dev/null 2>&1 && return
  kubectl -n "$APP" run curl --restart=Never \
    --image=curlimages/curl:8.19.0@sha256:c03110c736db81bbe1be0296f1f1608c81b954b01626bdfb0a8f84e5bd00ff3c \
    --command -- sleep 3600 >/dev/null
  kubectl -n "$APP" wait --for=condition=Ready pod/curl --timeout=120s >/dev/null
}
# The HTTP status of a HEAD on repo's manifest REFERENCE. HEAD records no pull, so asking cannot keep
# anything alive.
resolves() { # $1 = repo (host/path), $2 = tag or digest
  local host="${1%%/*}" path="${1#*/}"
  kubectl -n "$APP" exec curl -- curl -s -o /dev/null -w '%{http_code}' -I \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json' \
    "http://$host/v2/$path/manifests/$2"
}
state() {
  kubectl -n "$APP" get imagecompositions -o json | jq -S '[.items[] | {
    name: .metadata.name, digest: .status.artifact.digest,
    repo: (.status.artifact.ref | split("@")[0] | sub(":[^/:]*$"; "")),
    sbom: .status.attestations.sbom, history: [.status.history[]?.digest],
    lost: [.status.history[]? | select(.lost) | .digest],
    ready: ([.status.conditions[]? | select(.type == "Ready")][0] | {status, reason}),
    stalled: ([.status.conditions[]? | select(.type == "Stalled" and .status == "True")] | length)
  }]'
}
missing_digest_tags() {
  kubectl get imagecompositions,imagebuilds -A -o json | jq -r '.items[]
    | select([.status.artifact // empty] + (.status.history // [])
             | any(.digest and (.lost | not) and ((.tags // []) | any(test(":digest-|^digest-")) | not)))
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
}
# Ask every object for a full reconcile and wait until each reports finishing it, so what status says
# next is the upgraded controller's complete pass -- not just its backfill's early status patch.
reconciled_by_this_controller() {
  local token; token="upgrade-$(date +%s)"
  kubectl -n "$APP" annotate imagecomposition --all --overwrite "reconcile.fluxcd.io/requestedAt=$token" >/dev/null
  for _ in $(seq 60); do
    [ "$(kubectl -n "$APP" get imagecompositions -o json |
         jq --arg t "$token" '[.items[] | select(.status.lastHandledReconcileAt != $t)] | length')" = 0 ] && return
    sleep 3
  done
  fail "the upgraded controller did not finish reconciling every object"
}
same_digests() { # $1 before, $2 after, $3 when
  diff <(echo "$1" | jq -S '[.[] | {name, digest}]') <(echo "$2" | jq -S '[.[] | {name, digest}]') \
    || fail "a digest moved $3: an unchanged spec must not be reassembled"
  [ "$(echo "$2" | jq '[.[] | select(.stalled > 0)] | length')" = 0 ] || fail "an object stalled $3"
}
# Everything live resolves, and carries its own tag. The SBOM's own tag is checked only with
# $2=backfill: zot keeps a referrer while its subject exists but records no pulls of one, so after a
# window that tag lapses -- harmlessly, and the SBOM itself must still resolve.
check_registry() { # $1 state, $2 "backfill" to also require the SBOM's own tag
  echo "$1" | jq -c '.[]' | while read -r o; do
    repo="$(echo "$o" | jq -r .repo)" digest="$(echo "$o" | jq -r .digest)" sbom="$(echo "$o" | jq -r .sbom)"
    refs=("$digest" "digest-${digest#sha256:}" "$sbom")
    [ "${2:-}" = backfill ] && refs+=("digest-${sbom#sha256:}")
    for ref in "${refs[@]}"; do
      [ "$(resolves "$repo" "$ref")" = 200 ] || fail "$repo does not serve $ref"
    done
  done
}

step "install $PREVIOUS"
helm install "$RELEASE" "oci://ghcr.io/lhns/charts/kube-oci-composer" --version "$PREVIOUS" \
  --create-namespace "${COMMON[@]}" --wait --timeout 5m

step "publish with $PREVIOUS"
kubectl create namespace "$APP"
kubectl -n "$APP" create configmap files --from-literal=hello.txt=hello
kubectl -n "$APP" create configmap rolling-files --from-literal=hello.txt=first
kubectl -n "$APP" create configmap retired-files --from-literal=hello.txt=retired
composition spechash "[s1a2b3c4]" files                            # a spec-hash tag under Fail
composition digestonly "[]" files                                  # named only by its digest
composition suspended "[s5d6e7f8]" files                           # suspended below
composition rolling "[main]" rolling-files "onConflict: Overwrite" # republished below
composition retired "[r1]" retired-files                           # deleted later: the control
kubectl -n "$APP" wait --for=condition=Ready imagecomposition --all --timeout=5m

# A second build under the rolling tag, so history holds an older digest -- the rollback case. On a
# release with zot#4444 the tag move drops that digest from the registry, and it must then be marked
# lost rather than hold step 2 open for ever.
first_rolling="$(kubectl -n "$APP" get imagecomposition rolling -o jsonpath='{.status.artifact.digest}')"
kubectl -n "$APP" create configmap rolling-files --from-literal=hello.txt=second \
  --dry-run=client -o yaml | kubectl apply -f -
for _ in $(seq 60); do
  [ "$(kubectl -n "$APP" get imagecomposition rolling -o jsonpath='{.status.artifact.digest}')" != "$first_rolling" ] && break
  sleep 3
done

kubectl -n "$APP" patch imagecomposition suspended --type=merge -p '{"spec":{"suspend":true}}'
sleep 5
BEFORE="$(state)"
echo "$BEFORE"
[ "$(echo "$BEFORE" | jq '[.[] | select(.sbom == null or .sbom == "")] | length')" = 0 ] \
  || fail "$PREVIOUS published no SBOM, so this cannot check the referrer backfill"
[ "$(echo "$BEFORE" | jq -r '.[] | select(.name == "rolling") | .history | length')" -ge 2 ] \
  || fail "the rolling object's history has one entry, so no older digest is being tested"
retired="$(echo "$BEFORE" | jq -c '.[] | select(.name == "retired")')"

step "build this tree's images"
if ! docker image inspect "$COMPOSER_IMG" >/dev/null 2>&1; then
  docker build -t "$COMPOSER_IMG" --build-arg CMD=oci-composer "$HERE/../.."
  docker build -t "$BUILDER_IMG" --build-arg CMD=oci-builder "$HERE/../.."
fi
kind load docker-image "$COMPOSER_IMG" "$BUILDER_IMG" --name "$CLUSTER"

step "NOTES warns on the upgrade that removes keepUntagged"
helm upgrade "$RELEASE" "$CHART" "${COMMON[@]}" "${NEW_IMAGES[@]}" --dry-run=server \
  | grep -q "stops the registry keeping untagged" || fail "no upgrade warning on the upgrade from $PREVIOUS"

step "step 1: upgrade, keeping untagged content until the backfill is done"
helm upgrade "$RELEASE" "$CHART" "${COMMON[@]}" "${NEW_IMAGES[@]}" \
  --set registry.retention.keepUntagged=true --wait --timeout 5m
kubectl -n "$NS" rollout status deploy/"$RELEASE" --timeout=5m

step "step 2: every object gains its digest- tag, or is marked lost"
for _ in $(seq 60); do [ -z "$(missing_digest_tags)" ] && break; sleep 5; done
[ -z "$(missing_digest_tags)" ] || fail "still missing digest- tags: $(missing_digest_tags)"
reconciled_by_this_controller
AFTER="$(state)"
echo "$AFTER"
same_digests "$BEFORE" "$AFTER" "on upgrade"
[ "$(echo "$AFTER" | jq -r '.[] | select(.name == "suspended") | .ready.reason')" = Suspended ] \
  || fail "the suspended object is no longer reported suspended"
echo "rolling's history: $(echo "$AFTER" | jq -c '.[] | select(.name == "rolling") | {history, lost}')"

curl_pod
check_registry "$AFTER" backfill

step "step 3: stop keeping untagged content, compress the clock, retire one object"
kubectl -n "$APP" delete imagecomposition retired --wait
helm upgrade "$RELEASE" "$CHART" "${COMMON[@]}" "${NEW_IMAGES[@]}" \
  --set retention.window=30s --set retention.refreshFactor=30 --set retention.refreshInterval=1s \
  --set registry.retention.gcFactor=6 --set imageBuild.buildPollInterval=3s \
  --set registry.retention.gcMaxSchedulerDelay=1s --wait --timeout 5m

# The control: nothing refreshes the retired object's content, so it must actually go. Otherwise
# "everything live still resolves" below would hold just as well for a registry collecting nothing.
retired_repo="$(echo "$retired" | jq -r .repo)" retired_digest="$(echo "$retired" | jq -r .digest)"
for _ in $(seq 60); do [ "$(resolves "$retired_repo" "$retired_digest")" = 404 ] && break; sleep 5; done
[ "$(resolves "$retired_repo" "$retired_digest")" = 404 ] \
  || fail "the retired object's content was never collected, so this cannot tell whether collection ran"

reconciled_by_this_controller
FINAL="$(state)"
same_digests "$(echo "$AFTER" | jq '[.[] | select(.name != "retired")]')" "$FINAL" "after collection ran"
check_registry "$FINAL"

step "OK: upgraded from $PREVIOUS with no rebuild, no stall, every object backfilled, nothing live lost"
