//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// outputLostMessage is the controller's wording for an attempt whose output the registry lost.
const outputLostMessage = "the build's output is gone from the registry"

// TestALostBuildOutputIsRebuilt -- a build's manifest is untagged until the controller names it. If
// the registry's collector takes it first, the build must not stay Pending until the Job's TTL: it
// is counted as a failed build and rebuilt (ADR 0054, amended).
//
// The loss is produced for real: the object is suspended while its Job runs, which keeps the
// controller from naming the output (Reconcile returns before it), and the repository lies outside
// keepalive-*, where zot's default policy reclaims untagged manifests after gcDelay.
func TestALostBuildOutputIsRebuilt(t *testing.T) {
	t.Parallel()
	const name = "e2e-lost-output"
	repository := "e2e/" + name

	applyBuild(t, name, "Dockerfile")
	// Not a build input, so the Job's name stays the same across the unsuspend.
	buildEventually(t, "the build to start", func() error {
		if mustKubectl(t, "-n", buildNamespace, "get", "imagebuild", name,
			"-o", "jsonpath={.status.buildRef.name}") == "" {
			return fmt.Errorf("no Job yet")
		}
		return nil
	})
	mustKubectl(t, "-n", buildNamespace, "patch", "imagebuild", name, "--type=merge",
		"-p", `{"spec":{"suspend":true}}`)

	job := mustKubectl(t, "-n", buildNamespace, "get", "imagebuild", name, "-o", "jsonpath={.status.buildRef.name}")
	var digest string
	buildEventually(t, "the Job to finish while the object is suspended", func() error {
		if mustKubectl(t, "-n", buildNamespace, "get", "job", job, "-o", "jsonpath={.status.succeeded}") != "1" {
			return fmt.Errorf("job %s not complete", job)
		}
		digest = strings.Trim(mustKubectl(t, "-n", buildNamespace, "get", "pods", "-l", "job-name="+job,
			"-o", `jsonpath={.items[0].status.containerStatuses[?(@.name=="build")].state.terminated.message}`), " \n")
		if !strings.Contains(digest, "sha256:") {
			return fmt.Errorf("no digest in the termination message yet: %q", digest)
		}
		return nil
	})
	// The message is buildctl's metadata JSON.
	digest = digest[strings.Index(digest, "sha256:"):][:len("sha256:")+64]

	if st := buildStatus(t, name); st.Artifact != nil {
		t.Skip("the controller named the output before the suspend landed, so there is no loss to observe")
	}

	deadline := time.Now().Add(time.Duration(collectionDeadline(t)) * time.Second)
	// Until the registry answers 404: a failed exec under load is not a collection, and moving on
	// early lets the controller name an output that was never lost.
	for !manifestGoneByDigestWithoutPulling(t, repository, digest) {
		if time.Now().After(deadline) {
			t.Fatalf("the registry never collected the untagged %s@%s\ntags: %s%s",
				repository, digest, tagsList(t, repository), registryLogs(t))
		}
		time.Sleep(interval)
	}

	mustKubectl(t, "-n", buildNamespace, "patch", "imagebuild", name, "--type=merge",
		"-p", `{"spec":{"suspend":false}}`)

	buildEventually(t, "the lost output to be reported as a failed build", func() error {
		events := mustKubectl(t, "-n", buildNamespace, "get", "events",
			"--field-selector", "involvedObject.name="+name+",reason=BuildFailed",
			"-o", "jsonpath={.items[*].message}")
		if !strings.Contains(events, outputLostMessage) {
			return fmt.Errorf("no BuildFailed event naming the loss yet: %q", events)
		}
		return nil
	})

	buildEventually(t, "the rebuild to publish an image the registry serves", func() error {
		st := buildStatus(t, name)
		if c := readyCondition(st); c == nil || c.Status != "True" || st.Artifact == nil {
			return fmt.Errorf("not Ready yet: %+v", readyCondition(st))
		}
		if st.Artifact.Digest == digest {
			return fmt.Errorf("published the digest that was lost")
		}
		if !manifestExists(t, repository, st.Artifact.Digest) {
			return fmt.Errorf("the registry does not serve %s", st.Artifact.Digest)
		}
		return nil
	})
}
