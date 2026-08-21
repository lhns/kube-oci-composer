//go:build e2e

// Package e2e runs against a real cluster.
//
// This is the only test that proves the point of the project: compose an artifact, mount it as an
// image volume, and confirm the files land where the spec said. Everything else verifies that the
// controller does what it intends; this verifies that what it intends is useful.
//
// It also doubles as the check that the kubelet honours image volumes at all. The API accepting
// spec.volumes[].image does not prove the feature is enabled, and if it is not, the entire
// approach is moot — so that failure must be loud rather than skipped.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	namespace = "oci-composer-e2e"
	// The registry every reference names, mapped to its NodePort by a containerd drop-in on each
	// node. There is no serving endpoint any more (ADR 0035): compositions publish to the registry
	// like everything else, and this is the name a kubelet resolves.
	registryHost = "oci.e2e:5000"
	timeout      = 5 * time.Minute
	interval     = 5 * time.Second
)

func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(t, args...)
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func applyStdin(t *testing.T, manifest string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply failed: %v\n%s\n%s", err, out, manifest)
	}
}

// applyStdinAllowingFailure is applyStdin for the cases where a REJECTION is the result, not an
// error -- probing whether the cluster supports a field at all.
func applyStdinAllowingFailure(t *testing.T, manifest string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// eventually polls until fn succeeds. On timeout it reports the last error AND dumps the
// controller's logs, because "timed out waiting for Ready" on its own tells you nothing.
func eventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(interval)
	}
	logs, _ := kubectl(t, "-n", "oci-composer", "logs",
		"deploy/kube-oci-composer", "--tail=100")
	t.Fatalf("timed out waiting for %s: %v\n\ncontroller logs:\n%s", what, last, logs)
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		fmt.Fprintln(os.Stderr, "kubectl not found; run 'make e2e-up' first")
		os.Exit(1)
	}
	out, err := exec.Command("kubectl", "get", "crd", "imagecompositions.oci.lhns.de").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "CRD not installed; run 'make e2e-up' first:\n%s\n", out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestComposedArtifactMountsAsAnImageVolume is the whole point.
func TestComposedArtifactMountsAsAnImageVolume(t *testing.T) {
	mustKubectl(t, "create", "namespace", namespace, "--dry-run=client", "-o", "yaml")
	_, _ = kubectl(t, "create", "namespace", namespace)
	t.Cleanup(func() { _, _ = kubectl(t, "delete", "namespace", namespace, "--wait=false") })

	// A ConfigMap source, so the test needs no network access to an external artifact. The
	// digest is resolved by the controller from the content.
	applyStdin(t, `
apiVersion: v1
kind: ConfigMap
metadata:
  name: plugin-files
  namespace: `+namespace+`
data:
  first.properties: "level=INFO"
  second.properties: "retries=3"
`)

	applyStdin(t, `
apiVersion: oci.lhns.de/v1alpha1
kind: ImageComposition
metadata:
  name: e2e-artifact
  namespace: `+namespace+`
spec:
  interval: 1m
  layers:
    - name: config
      configMap:
        name: plugin-files
      to: /plugins
`)

	eventually(t, "the ImageComposition to become Ready", func() error {
		out, err := kubectl(t, "-n", namespace, "get", "imagecomposition", "e2e-artifact",
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		if strings.TrimSpace(out) != "True" {
			msg, _ := kubectl(t, "-n", namespace, "get", "imagecomposition", "e2e-artifact",
				"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].message}")
			return fmt.Errorf("Ready=%q: %s", strings.TrimSpace(out), msg)
		}
		return nil
	})

	// status.artifact.ref, not a reference reassembled from the digest. It is what the controller
	// says a consumer should pull, so using it means the test fails if that answer is wrong --
	// which is the whole contract an image volume depends on.
	ref := strings.TrimSpace(mustKubectl(t, "-n", namespace, "get", "imagecomposition",
		"e2e-artifact", "-o", "jsonpath={.status.artifact.ref}"))
	if ref == "" {
		t.Fatal("status.artifact.ref is empty; nothing can pull this")
	}
	// A tag alone would let a stale image satisfy this test. ADR 0010: pull the digest.
	if !strings.Contains(ref, "@sha256:") {
		t.Fatalf("status.artifact.ref is not digest-pinned: %q", ref)
	}
	if !strings.HasPrefix(ref, registryHost+"/") {
		t.Fatalf("published to %q, not to the default registry %q", ref, registryHost)
	}
	t.Logf("published %s", ref)

	// Reference the DIGEST, exactly as a workload should. See ADR 0010.
	applyStdin(t, `
apiVersion: v1
kind: Pod
metadata:
  name: consumer
  namespace: `+namespace+`
spec:
  restartPolicy: Never
  containers:
    - name: check
      image: busybox:1.37
      command:
        - sh
        - -c
        - |
          # Listed BEFORE asserting: a failing 'test -f' prints nothing, so without this a
          # failure arrives as an empty log saying only that the pod exited non-zero.
          echo "--- what actually mounted ---"
          ls -laR /mnt || true
          echo "-----------------------------"
          set -e
          test -f /mnt/plugins/first.properties
          test -f /mnt/plugins/second.properties
          grep -q 'level=INFO' /mnt/plugins/first.properties
          grep -q 'retries=3' /mnt/plugins/second.properties
          echo IMAGE_VOLUME_OK
      volumeMounts:
        - name: plugins
          # The image ROOT is mounted here, so the assertions above are against
          # /mnt + the layer's own 'to: /plugins' -- which is precisely what this test exists to
          # pin down: that 'to:' places content at that path INSIDE the artifact.
          #
          # Deliberately not subPath. subPath would let the test assert /plugins directly, but it
          # is a kubelet feature whose behaviour on image volumes is not uniform across versions
          # (it works on 1.36; on kind's 1.33 here the mount simply did not appear), and this test
          # is about the composer's output, not about subPath.
          mountPath: /mnt
          readOnly: true
  volumes:
    - name: plugins
      image:
        reference: `+ref+`
        pullPolicy: IfNotPresent
`)

	eventually(t, "the consumer pod to finish", func() error {
		phase, err := kubectl(t, "-n", namespace, "get", "pod", "consumer",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return fmt.Errorf("%v: %s", err, phase)
		}
		switch strings.TrimSpace(phase) {
		case "Succeeded":
			return nil
		case "Failed":
			logs, _ := kubectl(t, "-n", namespace, "logs", "consumer")
			state, _ := kubectl(t, "-n", namespace, "get", "pod", "consumer",
				"-o", "jsonpath={.status.containerStatuses[0].state}")
			t.Fatalf("consumer pod failed:\nstate: %s\nlogs:\n%s", strings.TrimSpace(state), logs)
			return nil
		default:
			// Surface the pull error rather than just the phase: if image volumes are not
			// supported, this is where it says so, and that is the single most useful line in
			// the whole run.
			events, _ := kubectl(t, "-n", namespace, "get", "events",
				"--field-selector", "involvedObject.name=consumer",
				"-o", "jsonpath={range .items[*]}{.reason}: {.message}{\"\\n\"}{end}")
			return fmt.Errorf("phase=%s\n%s", strings.TrimSpace(phase), events)
		}
	})

	logs := mustKubectl(t, "-n", namespace, "logs", "consumer")
	if !strings.Contains(logs, "IMAGE_VOLUME_OK") {
		t.Fatalf("the consumer did not confirm the files:\n%s", logs)
	}
}

// TestChangingTheSourceRebuilds — editing the ConfigMap must produce a new digest, because the
// controller resolves the digest from its content rather than from a declared one.
func TestChangingTheSourceRebuilds(t *testing.T) {
	ns := namespace + "-rebuild"
	_, _ = kubectl(t, "create", "namespace", ns)
	t.Cleanup(func() { _, _ = kubectl(t, "delete", "namespace", ns, "--wait=false") })

	apply := func(value string) {
		applyStdin(t, `
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: `+ns+`
data:
  app.conf: "`+value+`"
`)
	}
	apply("level=INFO")

	applyStdin(t, `
apiVersion: oci.lhns.de/v1alpha1
kind: ImageComposition
metadata:
  name: rebuild
  namespace: `+ns+`
spec:
  interval: 10s
  layers:
    - name: settings
      configMap:
        name: settings
      to: /config
`)

	digestOf := func() string {
		var digest string
		eventually(t, "a published digest", func() error {
			out, err := kubectl(t, "-n", ns, "get", "imagecomposition", "rebuild",
				"-o", "jsonpath={.status.artifact.digest}")
			if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "sha256:") {
				return fmt.Errorf("no digest yet: %s", out)
			}
			digest = strings.TrimSpace(out)
			return nil
		})
		return digest
	}

	first := digestOf()

	apply("level=DEBUG")
	eventually(t, "the digest to change after editing the ConfigMap", func() error {
		out, _ := kubectl(t, "-n", ns, "get", "imagecomposition", "rebuild",
			"-o", "jsonpath={.status.artifact.digest}")
		if strings.TrimSpace(out) == first {
			return fmt.Errorf("digest is still %s", first)
		}
		return nil
	})

	// The old build must remain pullable: that is what retention is for.
	history := mustKubectl(t, "-n", ns, "get", "imagecomposition", "rebuild",
		"-o", "jsonpath={.status.history[*].digest}")
	if !strings.Contains(history, first) {
		t.Fatalf("the previous build is not in status.history: %s", history)
	}
}
