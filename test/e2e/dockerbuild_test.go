//go:build e2e

// DockerBuild against a real cluster.
//
// This is the first thing that runs the builder end to end, and it exists because two pieces of it
// could not be verified any other way. The digest comes back out of the build through the pod's
// termination message, which needs a real kubelet to populate; and the FROM check reads a real
// context over HTTP. Both were previously only unit-tested against fakes.
//
// It also answers ADR 0025's second spike question — whether rootless BuildKit runs on the target
// nodes at all. If it does not, that is a real answer and the ADR lists it as grounds to abandon,
// so the failure must be loud rather than skipped.
package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	buildNamespace = "oci-builder-e2e"
	buildRegistry  = "e2e-registry.oci-builder-e2e.svc.cluster.local:5000"
)

// buildEventually is eventually() with the BUILDER's logs on timeout, plus the build pod's, since a
// failure is usually inside the build rather than in the controller.
func buildEventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(interval)
	}

	ctrl, _ := kubectl(t, "-n", "oci-builder", "logs", "deploy/kube-oci-builder", "--tail=120")
	jobs, _ := kubectl(t, "-n", buildNamespace, "get", "jobs,pods", "-o", "wide")
	pods, _ := kubectl(t, "-n", buildNamespace, "logs", "-l", "job-name", "--tail=120", "--all-containers")
	obj, _ := kubectl(t, "-n", buildNamespace, "get", "dockerbuild", "-o", "yaml")
	t.Fatalf("timed out waiting for %s: %v\n\nbuilder logs:\n%s\n\nobjects:\n%s\n\nbuild logs:\n%s\n\ndockerbuilds:\n%s",
		what, last, ctrl, jobs, pods, obj)
}

// dockerBuildStatus is the part of status this test reads.
type dockerBuildStatus struct {
	InputHash string `json:"inputHash"`
	Artifact  *struct {
		Digest string `json:"digest"`
		Ref    string `json:"ref"`
	} `json:"artifact"`
	Conditions []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
}

func buildStatus(t *testing.T, name string) dockerBuildStatus {
	t.Helper()
	out := mustKubectl(t, "-n", buildNamespace, "get", "dockerbuild", name,
		"-o", "jsonpath={.status}")
	var st dockerBuildStatus
	if strings.TrimSpace(out) == "" {
		return st
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("parsing status of %s: %v\n%s", name, err, out)
	}
	return st
}

func conditionIs(st dockerBuildStatus, condType, status string) bool {
	for _, c := range st.Conditions {
		if c.Type == condType {
			return c.Status == status
		}
	}
	return false
}

func applyBuild(t *testing.T, name, dockerfile string) {
	t.Helper()
	applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: DockerBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h
  context:
    kind: GitRepository
    name: e2e-src
  dockerfile: %s
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s/e2e/%s
    tags: [v1]
`, name, buildNamespace, dockerfile, buildRegistry, name))
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "dockerbuild", name, "--ignore-not-found")
	})
}

// TestDockerBuildProducesAnImage is the whole point of the kind.
//
// It proves the three things only a cluster can: rootless BuildKit runs, the built digest makes it
// back through the pod's termination message, and the image is actually in the registry afterwards.
func TestDockerBuildProducesAnImage(t *testing.T) {
	applyBuild(t, "e2e-build", "Dockerfile")

	buildEventually(t, "the build to become Ready", func() error {
		st := buildStatus(t, "e2e-build")
		if !conditionIs(st, "Ready", "True") {
			for _, c := range st.Conditions {
				if c.Type == "Ready" {
					return fmt.Errorf("Ready=%s (%s): %s", c.Status, c.Reason, c.Message)
				}
			}
			return fmt.Errorf("no Ready condition yet")
		}
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("Ready but no artifact digest recorded")
		}
		return nil
	})

	st := buildStatus(t, "e2e-build")
	if !strings.HasPrefix(st.Artifact.Digest, "sha256:") {
		t.Fatalf("digest %q is not a sha256 reference", st.Artifact.Digest)
	}
	if st.InputHash == "" {
		t.Error("no input hash recorded, so the short-circuit has nothing to compare")
	}

	// The digest must name something the registry actually has. This is what proves the push
	// happened rather than the controller merely believing it did.
	manifest := mustKubectl(t, "-n", buildNamespace, "run", "verify-digest",
		"--rm", "-i", "--restart=Never", "--image=busybox:1.37", "--quiet", "--command", "--",
		"wget", "-qO-", "--header", "Accept: application/vnd.oci.image.manifest.v1+json",
		fmt.Sprintf("http://%s/v2/e2e/e2e-build/manifests/%s", buildRegistry, st.Artifact.Digest))
	if !strings.Contains(manifest, "layers") {
		t.Fatalf("the registry does not serve a manifest at the recorded digest:\n%s", manifest)
	}
}

// TestDockerBuildIsIdempotent — the input hash is the whole cost model. A second reconcile of an
// unchanged object must not build again, which is visible as the digest and hash both holding still
// while no new Job appears.
func TestDockerBuildIsIdempotent(t *testing.T) {
	applyBuild(t, "e2e-idempotent", "Dockerfile")

	buildEventually(t, "the first build to finish", func() error {
		if st := buildStatus(t, "e2e-idempotent"); st.Artifact != nil && st.Artifact.Digest != "" {
			return nil
		}
		return fmt.Errorf("no artifact yet")
	})

	first := buildStatus(t, "e2e-idempotent")
	jobsBefore := mustKubectl(t, "-n", buildNamespace, "get", "jobs",
		"-o", "jsonpath={.items[*].metadata.name}")

	// Force a reconcile without changing an input.
	mustKubectl(t, "-n", buildNamespace, "annotate", "dockerbuild", "e2e-idempotent",
		fmt.Sprintf("reconcile.fluxcd.io/requestedAt=%d", time.Now().Unix()), "--overwrite")
	time.Sleep(20 * time.Second)

	second := buildStatus(t, "e2e-idempotent")
	if second.InputHash != first.InputHash {
		t.Errorf("input hash moved without an input changing: %q then %q",
			first.InputHash, second.InputHash)
	}
	if second.Artifact == nil || second.Artifact.Digest != first.Artifact.Digest {
		t.Errorf("digest changed on an unchanged spec: %+v then %+v", first.Artifact, second.Artifact)
	}

	jobsAfter := mustKubectl(t, "-n", buildNamespace, "get", "jobs",
		"-o", "jsonpath={.items[*].metadata.name}")
	if len(strings.Fields(jobsAfter)) > len(strings.Fields(jobsBefore)) {
		t.Errorf("an unchanged reconcile started another build:\nbefore: %s\nafter:  %s",
			jobsBefore, jobsAfter)
	}
}

// TestDockerBuildRefusesAnUnpinnedFrom — the one rule the controller enforces on a Dockerfile's
// CONTENT, and the reason it is enforced before a Job exists: an unpinned base means an unchanged
// spec can silently build on something else.
func TestDockerBuildRefusesAnUnpinnedFrom(t *testing.T) {
	applyBuild(t, "e2e-unpinned", "Dockerfile.unpinned")

	buildEventually(t, "the unpinned FROM to be refused", func() error {
		st := buildStatus(t, "e2e-unpinned")
		for _, c := range st.Conditions {
			if c.Type == "Ready" && c.Status == "False" &&
				strings.Contains(c.Message, "pinned by digest") {
				return nil
			}
		}
		if st.Artifact != nil {
			return fmt.Errorf("an unpinned FROM produced an artifact: %+v", st.Artifact)
		}
		return fmt.Errorf("not refused yet: %+v", st.Conditions)
	})

	// And no Job may exist for it — the check has to happen before anything executes.
	jobs := mustKubectl(t, "-n", buildNamespace, "get", "jobs",
		"-o", "jsonpath={.items[*].metadata.name}")
	if strings.Contains(jobs, "e2e-unpinned") {
		t.Errorf("a Job was created for an unpinned FROM: %s", jobs)
	}
}
