//go:build e2e

// The retention guarantee, driven by the CONTROLLER rather than by the test.
//
// retention_test.go establishes the registry's behaviour: that pulling an image renews its recency,
// with a negative control proving the registry does collect things. This file is the other half —
// that the controller actually does that pulling, for the right objects, and stops when it should.
//
// The distinction matters because the two can fail independently. A registry that renews on pull
// proves nothing if the controller never pulls; a controller that pulls diligently proves nothing if
// the registry ignores it. Everything here therefore does NOTHING to the registry itself: the test
// creates objects, waits, and looks. Anything it pulled would keep alive the very thing it is asking
// about, which is how a test of this shape passes while the feature is inert.
package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// The whole guarantee, end to end: an image a live object still references is not reclaimed.
//
// The negative control is the same image after its object is DELETED. That is a sharper control than
// retention_test.go's, because it holds everything constant except the one thing under test — the
// existence of a live object naming the image. If both halves pass, the controller's refresh is the
// only thing that can explain the difference.
func TestALiveObjectKeepsItsImagesAlive(t *testing.T) {
	repo := keepaliveRepo("live")
	digest := buildInto(t, "keepalive-live", repo, "v1")

	// Well past the 30s window, with several collection passes in between, and the test touching
	// nothing. If the object's images are still here, something refreshed them, and the only
	// candidate is the controller.
	sleepInCluster(t, 90)

	if !manifestExistsByDigest(t, repo, digest) {
		t.Fatalf("%s@%s was collected while a live ImageBuild still referenced it. The retention "+
			"guarantee is not being kept: an object that is Ready, unchanged and running had the "+
			"content it published deleted underneath it.\ntags now: %s%s",
			repo, digest, tagsList(t, repo), registryLogs(t))
	}
	if !manifestExists(t, repo, "v1") {
		t.Errorf("%s:v1 was collected while its object was live; the refresh reaches the digest but "+
			"not the tag, so the reference an operator wrote down stops resolving.\ntags now: %s",
			repo, tagsList(t, repo))
	}

	// THE NEGATIVE CONTROL. Delete the object and the refreshing stops with it.
	//
	// Without this the assertions above are satisfied by a registry that never collects anything,
	// which is indistinguishable from a guarantee that works.
	mustKubectl(t, "-n", buildNamespace, "delete", "imagebuild", "keepalive-live")
	eventuallyUntagged(t, repo, "v1", 240)
}

// Two objects publishing the same digest need no coordination: both refresh it, and it survives
// while EITHER lives (ADR 0031).
//
// This is the case that makes delete-on-eviction the wrong mechanism. There, one object's eviction
// destroys the other's content unless something tracks cross-object references — a distributed
// mark-and-sweep, with all of its failure modes. Here it falls out of the design, and this is the
// test that says so rather than the ADR merely claiming it.
func TestTwoObjectsSharingADigestKeepItAliveIndependently(t *testing.T) {
	repo := keepaliveRepo("shared")

	// Same context and Dockerfile, so both builds produce the same digest — which
	// TestRebuildingTheSameContextReproducesTheDigest already establishes. Different tags, so
	// neither trips the other's tag-conflict policy.
	digestA := buildInto(t, "keepalive-shared-a", repo, "a")
	digestB := buildInto(t, "keepalive-shared-b", repo, "b")

	if digestA != digestB {
		t.Skipf("the two builds produced different digests (%s vs %s), so this cannot test a SHARED "+
			"one. Reproducibility is a property of the Dockerfile, not a guarantee of the kind "+
			"(ADR 0025), so this is a skip rather than a failure.", digestA, digestB)
	}

	// One object goes away. Its tag stops being refreshed; the digest does not, because the other
	// object still names it.
	mustKubectl(t, "-n", buildNamespace, "delete", "imagebuild", "keepalive-shared-a")

	// Waiting for tag `a` to be collected is what proves time actually passed for this repository —
	// otherwise the assertions below would hold simply because nothing had been collected yet.
	eventuallyUntagged(t, repo, "a", 240)

	if !manifestExistsByDigest(t, repo, digestA) {
		t.Fatalf("%s@%s was collected after ONE of the two objects referencing it was deleted. "+
			"Shared content is being reclaimed on the first eviction, which is the failure mode "+
			"delete-on-eviction was rejected for.\ntags now: %s%s",
			repo, digestA, tagsList(t, repo), registryLogs(t))
	}
	if !manifestExists(t, repo, "b") {
		t.Errorf("%s:b was collected while its own object is still live and Ready\ntags now: %s",
			repo, tagsList(t, repo))
	}
}

// CONDITION 2 of ADR 0031, and the mistake it names as most likely: refresh must be driven by
// status.history, never by a successful reconcile.
//
// An object Stalled on a spec error has images that may be running right now, and stalling is
// exactly when nobody is watching the object. A refresh gated on reconcile success would delete the
// images of every broken object one retention window after it broke — silently, and long after the
// change that caused it.
//
// internal/retention covers this against a fake client. This covers it against a real controller,
// where the object genuinely goes Stalled and the refresher genuinely has to ignore that.
func TestAStalledObjectStillHasItsImagesRefreshed(t *testing.T) {
	repo := keepaliveRepo("stalled")
	digest := buildInto(t, "keepalive-stalled", repo, "v1")

	// Break the spec in a way the controller refuses outright rather than retries. A source in
	// another namespace is a tenancy violation and therefore terminal, which is what Stalled means.
	applyBuildSpec(t, "keepalive-stalled", "Dockerfile", buildRegistry+"/"+repo, "v1",
		"    namespace: someone-elses-namespace")

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

	if !manifestExistsByDigest(t, repo, digest) {
		t.Fatalf("%s@%s was collected while its object was Stalled. Refreshing is gated on a "+
			"successful reconcile, so every object with a broken spec loses the images it already "+
			"published -- one retention window after the spec broke, with nothing connecting the "+
			"two.\ntags now: %s%s", repo, digest, tagsList(t, repo), registryLogs(t))
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

// applyBuildSpec is applyBuildTo with the tag chosen too, which the retention tests need in order to
// point two objects at one repository without them colliding on a tag.
//
// extraContext is appended under spec.context, already indented, for the one test that has to break
// the reference deliberately.
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
    kind: GitRepository
    name: e2e-src
%s
  dockerfile: %s
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
