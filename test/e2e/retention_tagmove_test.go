//go:build e2e

// Moving a rolling tag must not delete the digest it moved off.
//
// ADR 0031 says an image named by a live object's retained status.history is never deleted, by
// anything. ADR 0010 tells workloads to reference digests, so the digest a rolling tag USED to point
// at is not a historical curiosity: it is what a running pod re-pulls when it is rescheduled, and
// what a rollback resolves to.
//
// retention_test.go and retention_controller_test.go establish that nothing EXPIRES out from under
// a live object. This file is about a different mechanism, and the distinction is the point of it:
// whether publishing the NEXT build destroys the previous one as a side effect of the push itself --
// before any retention decision is taken, and therefore where no amount of refreshing can help. The
// refresher pulls, and a pull cannot resurrect content the registry has already dropped.
//
// It did, and this file was written to find out. zot v2.1.21 drops a manifest from the repository
// index when its LAST tag moves to another digest: CheckIfIndexNeedsUpdate splices out the
// descriptor carrying the tag and does not re-add the old digest untagged (zot#4444). The first
// version of this test, run through the real controller with a single rolling tag, saw the previous
// digest 404 about five seconds after the next push, with no retention decision logged for it at
// all -- run 35760893931. That run is this test's negative control, on record: the same sequence,
// without the protection below, fails.
//
// The protection is ADR 0060. Every manifest the controllers publish is also named after its own
// digest (digest-<hex>), so a rolling tag is never a manifest's last tag, and moving it takes
// nothing with it. A second, per-build tag in the spec had the same effect in that run's control,
// which is what localised the cause to the tag descriptor and made this the fix.
//
// Everything here is asserted PROMPTLY -- seconds after the second build publishes, well inside
// gcDelay and far inside the retention window -- so that "it expired" is never available as an
// explanation for a failure.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// rollingTag is the name the test moves. Named once, because the failure message quotes it.
const rollingTag = "main"

// TestMovingARollingTagKeepsThePreviousDigest is the guarantee, through the real controller.
//
// One object, built twice from different content, publishing under one rolling tag with
// onConflict: Overwrite -- what the CRD documents as the setting for a tag meant to move. So this is
// not an exotic configuration; it is the one the API tells people to use.
//
// The object is never deleted. A live object is precisely the condition ADR 0031's guarantee is
// stated over, and the controller is refreshing both of its history entries throughout.
func TestMovingARollingTagKeepsThePreviousDigest(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("tagmove")
	name := keepaliveRepo("tagmove")

	applyRollingTagBuild(t, name, "Dockerfile", repo)
	first := awaitPublishedDigest(t, name, "")

	// It has to have been there, or "it is gone" is indistinguishable from "it never arrived".
	//
	// HEAD, not GET: a GET stamps the digest's lastPullTimestamp, and a check that renews what it is
	// checking makes the check itself the protection.
	if !manifestExistsByDigestWithoutPulling(t, repo, first) {
		t.Fatalf("%s@%s did not resolve immediately after its own build reported it published, "+
			"so this test has no subject.%s", repo, first, registryLogs(t))
	}

	// A different Dockerfile rather than a nudged annotation: if the two builds produce the same
	// digest the tag never moves and everything below passes vacuously. Asserted, not assumed.
	applyRollingTagBuild(t, name, "Dockerfile.other", repo)
	second := awaitPublishedDigest(t, name, first)
	published := time.Now()
	if second == first {
		t.Fatalf("both builds produced %s, so the rolling tag never moved and this test asks "+
			"nothing. The two Dockerfiles must differ in content.", first)
	}

	// THE ASSERTION, taken at once -- the registry before the API server, so every second spent
	// reading status is not a second in which expiry becomes an available explanation.
	alive := manifestExistsByDigestWithoutPulling(t, repo, first)
	elapsed := time.Since(published)
	tags := tagsList(t, repo)

	st := buildStatus(t, name)
	ready := readyCondition(st)
	inHistory := historyHasDigest(st, first)

	t.Logf("%s: first=%s second=%s; first alive=%v, checked %s after the second publish "+
		"(gcDelay=%s, window=%s); Ready=%+v, first in history=%v; tags now: %s",
		repo, first, second, alive, elapsed.Round(time.Millisecond), deployedGCDelay(t),
		retentionWindow, ready, inHistory, tags)

	// The object's own state, so a failure cannot be dismissed as the object having gone: a live,
	// Ready object still listing the digest is what makes the registry's answer mean anything.
	if ready == nil || ready.Status != "True" {
		t.Errorf("%s is not Ready after its second build (%+v), so the guarantee under test is not "+
			"in force and the registry's answer below is about something else", name, ready)
	}
	if !inHistory {
		t.Errorf("%s no longer lists %s in status.history, so ADR 0031 does not cover it and this "+
			"test is measuring the wrong thing. history: %s", name, first, historySummary(st))
	}

	if !alive {
		t.Fatalf(`%s@%s stopped resolving %s after a LATER BUILD OF THE SAME OBJECT moved %q off it.

Publishing a new build under a rolling tag DELETED content a live, Ready object still lists in
status.history. A workload pinned to that digest -- what ADR 0010 tells workloads to do -- gets a
404 when it is rescheduled, and a rollback cannot resolve its image.

Not expiry: the check was taken %s after the push, while the controller was refreshing every
digest in history. zot drops a manifest whose LAST tag moves (zot#4444), and ADR 0060 answers that
by naming every published manifest after its own digest, so the rolling tag is never the last one.
If the digest's own tag %q is missing from the tags below, that is where this broke.

  first:     %s
  second:    %s
  history:   %s
  tags now:  %s%s`,
			repo, first, elapsed.Round(time.Millisecond), rollingTag,
			elapsed.Round(time.Millisecond), ownTag(first),
			first, second, historySummary(st), tags, registryLogs(t))
	}

	// And it survived for the reason ADR 0060 says, not by luck: the previous build still carries a
	// name of its own while the rolling tag has moved on. If a future zot keeps the digest without
	// it, this is what reports that the reason changed.
	if !strings.Contains(tags, `"`+ownTag(first)+`"`) {
		t.Errorf("%s@%s survived, but without its own tag %s -- so something other than ADR 0060 "+
			"kept it (did zot#4444 get fixed?). tags now: %s", repo, first, ownTag(first), tags)
	}
	if got := tagDigest(t, repo, rollingTag); got != second {
		t.Errorf("%s:%s resolves to %q, want the second build %s; the tag did not move, so this "+
			"test did not test a move", repo, rollingTag, got, second)
	}

	// Then the ordinary half of the guarantee: with the object live, the refresher keeps the
	// previous build -- digest AND its own tag -- alive past a full window. The own tag is renewed
	// by the refresher's pulls like any other name it records.
	sleepInCluster(t, watchFor(t))
	if !manifestExistsByDigestWithoutPulling(t, repo, first) {
		t.Fatalf("%s@%s survived the tag move but was collected within %ds while its object was "+
			"live and listed it in history. tags now: %s%s", repo, first, watchFor(t),
			tagsList(t, repo), registryLogs(t))
	}
}

// ownTag is ADR 0060's tag for a digest, as the registry lists it. Written out rather than imported:
// the e2e checks the controller's behaviour, so it should not borrow the controller's definition.
func ownTag(digest string) string { return "digest-" + strings.TrimPrefix(digest, "sha256:") }

// applyRollingTagBuild creates or updates an ImageBuild that publishes under the MOVING tag.
//
// onConflict: Overwrite is not a convenience for this test -- it is what the CRD documents for a
// tag meant to move. Under the default (Fail) the second build is refused before it runs, which is a
// different behaviour and already tested.
func applyRollingTagBuild(t *testing.T, name, dockerfile, repository string) {
	t.Helper()
	applyBuildToTagged(t, name, dockerfile, buildRegistry+"/"+repository, "["+rollingTag+"]",
		"    onConflict: Overwrite")
}

// awaitPublishedDigest waits for the object to report a published digest that is not `previous`.
//
// Polled at one second rather than the suite's five. The value of this file rests on asking the
// registry PROMPTLY after the push, and kubectl talks to the API server, never to the registry, so
// polling it cannot renew anything.
func awaitPublishedDigest(t *testing.T, name, previous string) string {
	t.Helper()

	what := "the build " + name + " to publish"
	if previous != "" {
		what += " something other than " + previous
	}

	var digest string
	buildEventuallyPolling(t, buildTimeout, time.Second, what,
		func() error {
			st := buildStatus(t, name)
			if st.Artifact == nil || st.Artifact.Digest == "" {
				return fmt.Errorf("no digest yet: %+v", readyCondition(st))
			}
			if st.Artifact.Digest == previous {
				return fmt.Errorf("still publishing %s; the rebuild has not landed", previous)
			}
			digest = st.Artifact.Digest
			return nil
		})
	return digest
}

// historyHasDigest reports whether the retained history still names a digest, which is the exact
// wording of the guarantee under test.
func historyHasDigest(st dockerBuildStatus, digest string) bool {
	for _, rec := range st.History {
		if rec.Digest == digest {
			return true
		}
	}
	return false
}

// historySummary renders status.history for a failure message, tags included: the tags each record
// carries are what decide whether a descriptor still names the digest.
func historySummary(st dockerBuildStatus) string {
	if len(st.History) == 0 {
		return "(empty)"
	}
	var parts []string
	for _, rec := range st.History {
		parts = append(parts, fmt.Sprintf("%s%v", rec.Digest, rec.Tags))
	}
	return strings.Join(parts, ", ")
}
