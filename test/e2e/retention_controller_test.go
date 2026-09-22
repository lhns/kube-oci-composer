//go:build e2e

// The retention guarantee driven by the CONTROLLER rather than the test.
//
// retention_test.go shows the registry renews on pull; this file shows the controller does the
// pulling, for the right objects, and stops when it should. The two fail independently. So these
// tests never touch the registry except to observe: anything they pulled would keep alive the very
// thing they ask about.
package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestALiveObjectKeepsItsImagesAlive -- an image a live object references is not reclaimed. The
// negative control is the same image after its object is deleted: everything else held constant, so
// the controller's refresh is the only explanation for the difference.
func TestALiveObjectKeepsItsImagesAlive(t *testing.T) {
	// Not parallel: it ends waiting for a real deletion, and concurrent tests add repositories that
	// slow the collector's rotation.
	repo := keepaliveRepo("live")
	digest := buildInto(t, "keepalive-live", repo, "v1")

	// Past the window with the test touching nothing; only the controller can have refreshed it.
	sleepInCluster(t, watchFor(t))

	if !manifestExists(t, repo, digest) {
		t.Fatalf("%s@%s was collected while a live ImageBuild still referenced it: a Ready, unchanged "+
			"object had its published content deleted.\ntags now: %s%s",
			repo, digest, tagsList(t, repo), registryLogs(t))
	}
	if !manifestExists(t, repo, "v1") {
		t.Errorf("%s:v1 was collected while its object was live; the refresh reaches the digest but "+
			"not the tag, so the reference an operator wrote down stops resolving.\ntags now: %s",
			repo, tagsList(t, repo))
	}

	// THE NEGATIVE CONTROL: without the object, the refreshing stops and the tag goes. Otherwise a
	// registry that never collects would pass the assertions above.
	mustKubectl(t, "-n", buildNamespace, "delete", "imagebuild", "keepalive-live")
	eventuallyUntagged(t, repo, "v1", collectionDeadline(t))
}

// TestTwoObjectsSharingADigestKeepItAliveIndependently -- two objects publishing one digest both
// refresh it, so it survives while EITHER lives (ADR 0031), with no cross-object bookkeeping.
func TestTwoObjectsSharingADigestKeepItAliveIndependently(t *testing.T) {
	// Not parallel: it ends waiting for a real deletion (see TestALiveObjectKeepsItsImagesAlive).

	repo := keepaliveRepo("shared")

	// Same context and Dockerfile, so the same digest (see
	// TestRebuildingTheSameContextReproducesTheDigest); different tags, so no tag conflict.
	digestA := buildInto(t, "keepalive-shared-a", repo, "a")
	digestB := buildInto(t, "keepalive-shared-b", repo, "b")

	if digestA != digestB {
		t.Skipf("the two builds produced different digests (%s vs %s), so this cannot test a SHARED "+
			"one. Reproducibility is a property of the Dockerfile, not a guarantee of the kind "+
			"(ADR 0025), so this is a skip rather than a failure.", digestA, digestB)
	}

	// The control is an image in its OWN repository with no object: seeing it collected proves a
	// collection ran during the test. Not tag `a`: B's refresh of the shared digest may keep every
	// tag on it alive, which is not what this test is about.
	control := keepaliveRepo("shared-control")
	pushTinyImage(t, control)

	mustKubectl(t, "-n", buildNamespace, "delete", "imagebuild", "keepalive-shared-a")
	eventuallyUntagged(t, control, "v1", collectionDeadline(t))

	// Recorded, not asserted: whether tag `a` outlives its object is registry behaviour this project
	// does not depend on.
	t.Logf("after deleting the object owning tag `a`, with the digest still refreshed by another "+
		"object: tags now %s", tagsList(t, repo))

	if !manifestExists(t, repo, digestA) {
		t.Fatalf("%s@%s was collected after ONE of the two objects referencing it was deleted: shared "+
			"content is reclaimed on the first eviction.\ntags now: %s%s",
			repo, digestA, tagsList(t, repo), registryLogs(t))
	}
	if !manifestExists(t, repo, "b") {
		t.Errorf("%s:b was collected while its own object is still live and Ready\ntags now: %s",
			repo, tagsList(t, repo))
	}
}

// TestAStalledObjectStillHasItsImagesRefreshed -- ADR 0031 condition 2: refresh follows
// status.history, never reconcile success. A Stalled object's images may still be running, and a
// success-gated refresh would delete them one window after the spec broke. internal/retention covers
// this against a fake client; this uses a real controller.
func TestAStalledObjectStillHasItsImagesRefreshed(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("stalled")
	digest := buildInto(t, "keepalive-stalled", repo, "v1")

	// A cross-namespace source is a tenancy violation, so terminal: Stalled, not retried.
	applyBuildSpec(t, "keepalive-stalled", "Dockerfile", buildRegistry+"/"+repo, "v1",
		"      namespace: someone-elses-namespace")

	buildEventually(t, "the object to go Stalled", func() error {
		st := buildStatus(t, "keepalive-stalled")
		ready := readyCondition(st)
		if ready == nil || ready.Status != "False" {
			return fmt.Errorf("Ready=%+v, want False", ready)
		}
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("the previous artifact was cleared; there is nothing left to refresh")
		}
		return nil
	})

	sleepInCluster(t, 90)

	if !manifestExists(t, repo, digest) {
		t.Fatalf("%s@%s was collected while its object was Stalled: refreshing is gated on a "+
			"successful reconcile, so a broken spec loses its published images one window later."+
			"\ntags now: %s%s", repo, digest, tagsList(t, repo), registryLogs(t))
	}
}

// buildInto publishes one image through the real build path and returns its digest.
func buildInto(t *testing.T, name, repository, tag string) string {
	t.Helper()

	applyBuildSpec(t, name, "Dockerfile", buildRegistry+"/"+repository, tag)
	buildEventually(t, name+" to publish", func() error {
		st := buildStatus(t, name)
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("no digest yet: %+v", readyCondition(st))
		}
		return nil
	})
	return buildStatus(t, name).Artifact.Digest
}

// applyBuildSpec is applyBuildTo with one chosen tag. extraContext is appended under
// spec.context.sourceRef, already indented, for the test that breaks the reference.
func applyBuildSpec(t *testing.T, name, dockerfile, repository, tag string, extraContext ...string) {
	t.Helper()
	applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h
  context:
    sourceRef:
      kind: GitRepository
      name: e2e-src
%s
  dockerfile: {path: %s}
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s
    tags: [%s]
`, name, buildNamespace, strings.Join(extraContext, "\n"), dockerfile, repository, tag))
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "imagebuild", name, "--ignore-not-found")
	})
}
