//go:build e2e

// Package e2e runs against a real cluster (see up.sh).
//
// The image-volume test is the one that proves the project useful: compose an artifact, mount it as
// an image volume, and confirm the files land where the spec said. It also checks that the kubelet
// honours image volumes at all -- the API accepting spec.volumes[].image does not prove it -- so that
// failure is loud rather than skipped.
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
	// The PUBLIC registry name: what a kubelet resolves (via the containerd drop-in) and what
	// status.artifact.ref reports. The controllers never use it.
	registryHost = "oci-composer.e2e:5000"
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
	if out, err := applyStdinAllowingFailure(t, manifest); err != nil {
		t.Fatalf("apply failed: %v\n%s\n%s", err, out, manifest)
	}
}

// applyStdinAllowingFailure returns a rejection instead of failing, for probing whether the cluster
// supports a field at all.
func applyStdinAllowingFailure(t *testing.T, manifest string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// eventually polls until fn succeeds. On timeout it reports the last error and the controller's
// logs.
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

// TestComposedArtifactMountsAsAnImageVolume is the whole point: a composed artifact, pulled by the
// reference status reports, mounts with each layer's files under its `to:` path.
func TestComposedArtifactMountsAsAnImageVolume(t *testing.T) {
	mustKubectl(t, "create", "namespace", namespace, "--dry-run=client", "-o", "yaml")
	_, _ = kubectl(t, "create", "namespace", namespace)
	t.Cleanup(func() { _, _ = kubectl(t, "delete", "namespace", namespace, "--wait=false") })

	// A ConfigMap source, so no external artifact is needed.
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
  # Tagged so the manifest stays reachable under the suite's compressed GC; the pod below still
  # references the DIGEST (ADR 0010).
  push:
    tags: [main]
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

	// status.artifact.ref as published, not a reference rebuilt from the digest: it is the
	// controller's answer to "what should a consumer pull", and this test checks that answer.
	ref := strings.TrimSpace(mustKubectl(t, "-n", namespace, "get", "imagecomposition",
		"e2e-artifact", "-o", "jsonpath={.status.artifact.ref}"))
	if ref == "" {
		t.Fatal("status.artifact.ref is empty; nothing can pull this")
	}
	// A tag alone would let a stale image satisfy this test (ADR 0010).
	if !strings.Contains(ref, "@sha256:") {
		t.Fatalf("status.artifact.ref is not digest-pinned: %q", ref)
	}
	if !strings.HasPrefix(ref, registryHost+"/") {
		t.Fatalf("published to %q, not to the default registry %q", ref, registryHost)
	}
	t.Logf("published %s", ref)

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
          # Listed first: a failing 'test -f' prints nothing.
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
          # The image ROOT, so the assertions check that 'to: /plugins' placed content at that path
          # inside the artifact. Not subPath: its behaviour on image volumes varies by version.
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
			// The pod's events carry the pull error, e.g. when image volumes are unsupported.
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

// TestChangingTheSourceRebuilds -- editing the ConfigMap must produce a new digest, because the
// controller resolves the digest from content, and the previous build must stay in history.
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
