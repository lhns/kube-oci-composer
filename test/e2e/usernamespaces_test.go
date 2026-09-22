//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestUserNamespacesOnThisCluster measures threat-model gap E1: whether `hostUsers: false` works
// here, which would remove the privilege escalation rootless BuildKit needs (ADR 0027).
//
// It SKIPs when the feature is unavailable -- the answer either way is the measurement -- and fails
// only when the probe itself is broken or the setting is silently ignored.
func TestUserNamespacesOnThisCluster(t *testing.T) {
	// Parallel: it can spend ~90s probing, and shares no state.
	t.Parallel()

	const pod = "userns-probe"

	// Its OWN namespace, so a missing namespace cannot masquerade as a refused hostUsers.
	ns := namespace + "-userns"
	_, _ = kubectl(t, "create", "namespace", ns)
	t.Cleanup(func() { _, _ = kubectl(t, "delete", "namespace", ns, "--wait=false") })

	out, err := applyStdinAllowingFailure(t, `
apiVersion: v1
kind: Pod
metadata:
  name: `+pod+`
  namespace: `+ns+`
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
		// Only a rejection mentioning the field is evidence about it; anything else is a broken probe.
		if !strings.Contains(strings.ToLower(out), "hostuser") {
			t.Fatalf("the probe could not be applied, and not because of hostUsers: %s", strings.TrimSpace(out))
		}
		t.Logf("E1 MEASURED: the cluster refused hostUsers: false -- %s", strings.TrimSpace(out))
		t.Skip("user namespaces unavailable on this cluster; E1 stands as recorded in ADR 0027")
	}

	// Short on purpose: a node that cannot run this pod leaves it Pending, and that IS the answer.
	const settle = 90 * time.Second
	var phase string
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		p, err := kubectl(t, "-n", ns, "get", "pod", pod, "-o", "jsonpath={.status.phase}")
		if err == nil {
			phase = strings.TrimSpace(p)
		}
		if phase == "Succeeded" || phase == "Failed" {
			break
		}
		time.Sleep(interval)
	}

	events, _ := kubectl(t, "-n", ns, "get", "events", "--field-selector", "involvedObject.name="+pod)

	if phase != "Succeeded" && phase != "Failed" {
		t.Logf("E1 MEASURED: hostUsers: false was accepted by the API server and the pod never ran "+
			"(phase=%s after %s). The runtime does not support user namespaces.\n%s", phase, settle, events)
		t.Skip("the runtime does not support user namespaces; E1 stands as recorded in ADR 0027")
	}

	logs, _ := kubectl(t, "-n", ns, "logs", pod)
	uidMap := strings.TrimSpace(logs)

	if phase != "Succeeded" {
		t.Logf("E1 MEASURED: the pod ran and failed -- %s\n%s", uidMap, events)
		t.Skip("the probe did not complete; E1 stands as recorded in ADR 0027")
	}

	// It ran: the identity map would mean hostUsers was silently ignored -- mitigated on paper only.
	if strings.Contains(uidMap, "0          0 4294967295") || strings.Contains(uidMap, "0 0 4294967295") {
		t.Fatalf("hostUsers: false was accepted and IGNORED -- uid_map is the identity map:\n%s", uidMap)
	}
	t.Logf("E1 MEASURED: user namespaces WORK on this cluster. uid_map:\n%s", uidMap)
	t.Log("ADR 0027's destination is now reachable; hostUsers: false on the build Job is the next step.")
}
