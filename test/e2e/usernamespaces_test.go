//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
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
// threat model needs. It fails only when the probe itself is wrong, which has already happened
// twice: see the comments below, both of which are there because the probe reported something it
// had not measured.
func TestUserNamespacesOnThisCluster(t *testing.T) {
	const pod = "userns-probe"

	// Its OWN namespace. The first version borrowed the image-volume test's, which by then had
	// been deleted -- so the probe reported "the cluster refused hostUsers: false" when what the
	// cluster had actually said was "no such namespace". A probe that can report the wrong answer
	// is worse than no probe.
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
		// Only a rejection that MENTIONS the field is evidence about the field. Anything else is
		// the probe being broken, and must fail rather than quietly become a measurement.
		if !strings.Contains(strings.ToLower(out), "hostuser") {
			t.Fatalf("the probe could not be applied, and not because of hostUsers: %s", strings.TrimSpace(out))
		}
		t.Logf("E1 MEASURED: the cluster refused hostUsers: false -- %s", strings.TrimSpace(out))
		t.Skip("user namespaces unavailable on this cluster; E1 stands as recorded in ADR 0027")
	}

	// A short deadline on purpose. If the node cannot run this pod it sits Pending indefinitely,
	// and that IS the measurement -- spending the suite's full five-minute timeout to learn it
	// would add five minutes to every run for an answer available in one.
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

	// Plain -o wide rather than a jsonpath range. The jsonpath this started with was copied from
	// another file and mangled in transit, and an unterminated-quote error from kubectl replaced
	// the diagnostic it was supposed to produce.
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

	// It ran. Now check it actually got a namespace rather than the identity map, because a
	// silently-ignored hostUsers would be the worst outcome: it reads as mitigated and is not.
	if strings.Contains(uidMap, "0          0 4294967295") || strings.Contains(uidMap, "0 0 4294967295") {
		t.Fatalf("hostUsers: false was accepted and IGNORED -- uid_map is the identity map:\n%s", uidMap)
	}
	t.Logf("E1 MEASURED: user namespaces WORK on this cluster. uid_map:\n%s", uidMap)
	t.Log("ADR 0027's destination is now reachable; hostUsers: false on the build Job is the next step.")
}
