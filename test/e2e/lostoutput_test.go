//go:build e2e

package e2e

import (
	"encoding/json"
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
	var digest, firstPod string
	var started time.Time
	buildEventually(t, "the Job to finish while the object is suspended", func() error {
		if mustKubectl(t, "-n", buildNamespace, "get", "job", job, "-o", "jsonpath={.status.succeeded}") != "1" {
			return fmt.Errorf("job %s not complete", job)
		}
		// The push cannot be older than the Job.
		at := mustKubectl(t, "-n", buildNamespace, "get", "job", job, "-o", "jsonpath={.status.startTime}")
		var err error
		if started, err = time.Parse(time.RFC3339, at); err != nil {
			return fmt.Errorf("job %s startTime %q: %v", job, at, err)
		}
		firstPod = mustKubectl(t, "-n", buildNamespace, "get", "pods", "-l", "job-name="+job,
			"-o", "jsonpath={.items[0].metadata.name}")
		msg := mustKubectl(t, "-n", buildNamespace, "get", "pods", "-l", "job-name="+job,
			"-o", `jsonpath={.items[0].status.containerStatuses[?(@.name=="build")].state.terminated.message}`)
		// The message is buildctl's whole metadata file, which names other digests first: the build
		// cache's manifest (cache.manifest) and the config blob. Read the key the controller reads.
		var md struct {
			Digest string `json:"containerimage.digest"`
		}
		if err := json.Unmarshal([]byte(msg), &md); err != nil || md.Digest == "" {
			return fmt.Errorf("no manifest digest in the termination message yet (%v): %q", err, msg)
		}
		digest = md.Digest
		return nil
	})

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

	// Nothing younger than gcDelay is collected, and the manifest is no older than its Job. A 404
	// sooner than that means the digest asked about was never a manifest there, and unsuspending now
	// would name an output that was not lost.
	if age, delay := time.Since(started), deployedGCDelay(t); age < delay {
		t.Fatalf("the registry answered 404 for %s@%s only %s after the Job started, sooner than "+
			"gcDelay=%s allows a collection; this test asked about something that was never there",
			repository, digest, age.Round(time.Second), delay)
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
		// The fixture is reproducible, so the rebuild publishes the SAME digest. That it was built
		// again shows in the pod status names.
		pod := mustKubectl(t, "-n", buildNamespace, "get", "imagebuild", name,
			"-o", "jsonpath={.status.lastAttempt.podName}")
		if pod == "" || pod == firstPod {
			return fmt.Errorf("status still names the first build's pod (%q), so nothing was rebuilt", pod)
		}
		if !manifestExists(t, repository, st.Artifact.Digest) {
			return fmt.Errorf("the registry does not serve %s", st.Artifact.Digest)
		}
		return nil
	})
}
