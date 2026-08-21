//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestUserNamespacesOnThisCluster measures threat-model gap E1 rather than arguing about it.
//
// Build pods run rootless BuildKit with allowPrivilegeEscalation, SETUID/SETGID and seccomp
// unconfined -- all four measured as necessary (ADR 0027), and all four widening the kernel surface
// reachable from code that came out of a git repository. `hostUsers: false` would remove the need
// for the escalation entirely by mapping the container's root to an unprivileged host uid.
//
// ADR 0027 recorded it as the destination and reported it not running on the cluster of the day.
// That measurement is now old: user namespaces went beta in 1.30 and this suite runs on 1.36. So
// this re-measures instead of citing.
//
// It does NOT fail when the feature is unavailable. A test that fails on an upstream capability
// this project cannot provide would be turned off, and the answer -- either way -- is what the
// threat model needs. It fails only if the pod ends up somewhere neither running nor rejected,
// which would mean the probe itself is wrong.
func TestUserNamespacesOnThisCluster(t *testing.T) {
	const pod = "userns-probe"
	_, _ = kubectl(t, "-n", namespace, "delete", "pod", pod, "--ignore-not-found")

	out, err := applyStdinAllowingFailure(t, `
apiVersion: v1
kind: Pod
metadata:
  name: `+pod+`
  namespace: `+namespace+`
spec:
  restartPolicy: Never
  hostUsers: false
  containers:
    - name: probe
      image: busybox:1.37
      command: ["sh", "-c"]
      args:
        - |
          set -e
          # Inside a user namespace, /proc/self/uid_map maps container uids onto a DIFFERENT host
          # range. Without one, the identity map "0 0 4294967295" is what appears.
          cat /proc/self/uid_map
          echo USERNS_PROBE_DONE
`)
	if err != nil {
		// The API server rejecting the field IS the answer: the feature gate is off.
		t.Logf("E1 MEASURED: the cluster refused hostUsers: false -- %s", strings.TrimSpace(out))
		t.Skip("user namespaces unavailable on this cluster; E1 stands as recorded in ADR 0027")
	}

	var phase string
	eventually(t, "the user-namespace probe to settle", func() error {
		p, err := kubectl(t, "-n", namespace, "get", "pod", pod, "-o", "jsonpath={.status.phase}")
		if err != nil {
			return fmt.Errorf("%v: %s", err, p)
		}
		phase = strings.TrimSpace(p)
		switch phase {
		case "Succeeded", "Failed":
			return nil
		default:
			events, _ := kubectl(t, "-n", namespace, "get", "events",
				"--field-selector", "involvedObject.name="+pod,
				"-o", "jsonpath={range .items[*]}{.reason}: {.message}{\"\n\"}{end}")
			return fmt.Errorf("phase=%s\n%s", phase, events)
		}
	})

	logs, _ := kubectl(t, "-n", namespace, "logs", pod)
	uidMap := strings.TrimSpace(logs)

	if phase != "Succeeded" {
		// Accepted by the API server and then not runnable by the node: the runtime does not
		// support it. Still a measurement, still not this project's failure.
		t.Logf("E1 MEASURED: hostUsers: false was accepted but the pod did not run -- %s", uidMap)
		t.Skip("the runtime does not support user namespaces; E1 stands as recorded in ADR 0027")
	}

	// It ran. Now check it actually got a namespace rather than the identity map, because a
	// silently-ignored hostUsers would be the worst outcome: it reads as mitigated and is not.
	if strings.Contains(uidMap, "0          0 4294967295") || strings.Contains(uidMap, "0 0 4294967295") {
		t.Fatalf("hostUsers: false was accepted and IGNORED -- uid_map is the identity map:\n%s", uidMap)
	}
	t.Logf("E1 MEASURED: user namespaces WORK on this cluster. uid_map:\n%s", uidMap)
	t.Log("ADR 0027's destination is now reachable; hostUsers: false on the build Job is the next step.")
}
