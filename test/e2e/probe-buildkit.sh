#!/usr/bin/env bash
# Which security context lets BuildKit actually start?
#
# TEMPORARY diagnostic, not part of the suite. The first real e2e run answered ADR 0025's spike
# question 2 with a hard no: rootless BuildKit could not map UIDs under the build pod's hardened
# context, because `newuidmap` is setuid-root and the pod sets both allowPrivilegeEscalation:false
# (NO_NEW_PRIVS, which makes the kernel ignore the setuid bit) and drop:ALL (which empties the
# bounding set so CAP_SETUID cannot be acquired either).
#
# The question this answers is which of the ways out actually works on this cluster, and the reason
# it is a script rather than four successive pipeline runs is that each of those costs ~15 minutes
# to learn one bit. Every candidate runs at once and reports the same fact: did buildkitd come up.
#
# `buildctl debug workers` is the gate. It needs buildkitd started and connectable, which is exactly
# what failed, and it needs no build context, no registry and no network.
set -uo pipefail

NS="${BUILD_NS:-oci-builder-e2e}"
ROOTLESS="${ROOTLESS_IMAGE:-moby/buildkit:v0.17.2-rootless@sha256:5b45405a38c579692f6fcd47ceef2002fe4fa61bb04ef0c2c644cf74cbbd57b8}"
STANDARD="${STANDARD_IMAGE:-moby/buildkit:v0.17.2}"

# Each candidate is a name, an image, and the pod spec fragments that distinguish it.
#
#   rootless-userns-noesc  the ideal: the kubelet makes the namespace, nothing needs setuid.
#   rootless-userns-esc    same, but setuid permitted INSIDE the namespace — any escalation is
#                          confined to a userns whose root maps to an unprivileged host uid.
#   rootless-esc           no userns; setuid permitted on the host's user table. This is what
#                          upstream documents, and the weakest of the three.
#   standard-userns-caps   no rootlesskit at all: uid 0 inside the kubelet's namespace, with the
#                          mount capabilities buildkitd needs — namespaced, so not host power.
#   standard-userns-nocaps the same without capabilities, to find out whether they are needed
#                          rather than assuming it.
probe() {
  local name="$1" image="$2" hostusers="$3" runas="$4" nonroot="$5" allowesc="$6" caps="$7"

  kubectl -n "$NS" delete pod "probe-$name" --ignore-not-found --wait=false >/dev/null 2>&1

  # An empty caps argument omits the field rather than sending an empty one: "no capabilities
  # stanza" is itself a candidate, because it is what upstream BuildKit's Kubernetes examples ship.
  local capline=""
  [ -n "$caps" ] && capline="      capabilities: $caps"

  cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: probe-$name
  namespace: $NS
spec:
  restartPolicy: Never
  hostUsers: $hostusers
  automountServiceAccountToken: false
  containers:
  - name: probe
    image: $image
    command: ["sh", "-c", "buildctl-daemonless.sh debug workers && echo PROBE_OK"]
    env:
    - name: BUILDKITD_FLAGS
      value: "--oci-worker-no-process-sandbox"
    securityContext:
      runAsUser: $runas
      runAsGroup: $runas
      runAsNonRoot: $nonroot
      allowPrivilegeEscalation: $allowesc
      privileged: false
      seccompProfile: {type: Unconfined}
      appArmorProfile: {type: Unconfined}
$capline
EOF
}

DROP_ALL='{drop: ["ALL"]}'
SETUID_CAPS='{drop: ["ALL"], add: ["SETUID", "SETGID"]}'
MOUNT_CAPS='{drop: ["ALL"], add: ["SYS_ADMIN", "SETUID", "SETGID", "SYS_CHROOT", "MKNOD"]}'

echo "===== applying probes ====="
probe rootless-userns-noesc  "$ROOTLESS" false 1000 true  false "$DROP_ALL"
probe rootless-userns-esc    "$ROOTLESS" false 1000 true  true  "$SETUID_CAPS"
probe rootless-esc-caps      "$ROOTLESS" true  1000 true  true  "$SETUID_CAPS"
probe rootless-esc-nocaps    "$ROOTLESS" true  1000 true  true  ""
probe standard-userns-caps   "$STANDARD" false 0    false false "$MOUNT_CAPS"
probe standard-userns-nocaps "$STANDARD" false 0    false false "$DROP_ALL"

NAMES="rootless-userns-noesc rootless-userns-esc rootless-esc-caps rootless-esc-nocaps standard-userns-caps standard-userns-nocaps"

# A pod that cannot be admitted at all never reaches a terminal phase, so this waits on the clock
# rather than on kubectl wait, and reports whatever each pod managed to become.
echo "===== waiting ====="
for _ in $(seq 1 30); do
  pending=0
  for n in $NAMES; do
    phase=$(kubectl -n "$NS" get pod "probe-$n" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    case "$phase" in
      Succeeded|Failed) ;;
      *) pending=$((pending + 1)) ;;
    esac
  done
  [ "$pending" -eq 0 ] && break
  sleep 10
done

echo
echo "===== RESULTS ====="
for n in $NAMES; do
  phase=$(kubectl -n "$NS" get pod "probe-$n" -o jsonpath='{.status.phase}' 2>/dev/null || echo "<absent>")
  logs=$(kubectl -n "$NS" logs "probe-$n" --tail=40 2>&1)

  if printf '%s' "$logs" | grep -q PROBE_OK; then
    verdict="WORKS"
  else
    verdict="fails"
  fi

  echo
  echo "--- $n: $verdict (phase=$phase)"
  # Admission rejections leave nothing in the log, so the pod's own status is the only record of
  # a config the cluster refused outright.
  if [ "$phase" = "<absent>" ] || [ -z "$logs" ]; then
    kubectl -n "$NS" get pod "probe-$n" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason} {.message}{"
"}{end}' 2>&1 | head -6
    kubectl -n "$NS" describe pod "probe-$n" 2>&1 | sed -n '/Events:/,$p' | head -20
  fi
  printf '%s\n' "$logs" | head -25
done

echo
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "## BuildKit security-context probe"
    echo
    echo "| candidate | verdict |"
    echo "|---|---|"
    for n in $NAMES; do
      if kubectl -n "$NS" logs "probe-$n" --tail=40 2>&1 | grep -q PROBE_OK; then
        echo "| \`$n\` | **WORKS** |"
      else
        echo "| \`$n\` | fails |"
      fi
    done
  } >> "$GITHUB_STEP_SUMMARY"
fi

echo "===== node support ====="
# If every userns candidate failed, this is the difference between "the feature is off" and
# "the feature is on and something else is wrong", which are opposite next steps.
kubectl get nodes -o wide
kubectl get --raw '/api/v1/nodes' 2>/dev/null | grep -o '"kubeletVersion":"[^"]*"' | head -1

# The probe reports findings; a candidate failing is a result, not a broken step.
exit 0
