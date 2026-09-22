//go:build e2e

// Moving a rolling tag must not delete the digest it moved off.
//
// That digest is in the live object's status.history (ADR 0031) and is what a rescheduled pod or a
// rollback pulls (ADR 0010). Unlike the other retention files, this is about the PUSH destroying
// content, which no refresh can undo: zot v2.1.21 drops a manifest from the index when its LAST tag
// moves (zot#4444). ADR 0060's own digest-<hex> tag means a rolling tag is never the last one. The
// negative control is on record: without that tag, the same sequence failed (run 35760893931).
//
// Asserted PROMPTLY, well inside gcDelay and the window, so expiry cannot explain a failure.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// rollingTag is the tag the test moves.
const rollingTag = "main"

// TestMovingARollingTagKeepsThePreviousDigest -- one live object, built twice from different
// content under one rolling tag with onConflict: Overwrite (the documented setting for a moving
// tag). The previous digest must still resolve right after the move, and after a full window.
func TestMovingARollingTagKeepsThePreviousDigest(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("tagmove")
	name := keepaliveRepo("tagmove")

	applyRollingTagBuild(t, name, "Dockerfile", repo)
	first := awaitPublishedDigest(t, name, "")

	// It must have arrived, or "gone" proves nothing. HEAD, not GET: a GET is a pull and would renew
	// what it checks.
	if !manifestExistsByDigestWithoutPulling(t, repo, first) {
		t.Fatalf("%s@%s did not resolve immediately after its own build reported it published, "+
			"so this test has no subject.%s", repo, first, registryLogs(t))
	}

	// Different content, or the tag never moves and everything below passes vacuously.
	applyRollingTagBuild(t, name, "Dockerfile.other", repo)
	second := awaitPublishedDigest(t, name, first)
	published := time.Now()
	if second == first {
		t.Fatalf("both builds produced %s, so the rolling tag never moved and this test asks "+
			"nothing. The two Dockerfiles must differ in content.", first)
	}

	// Taken at once, the registry before the API server, so expiry cannot be the explanation.
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

	// The guarantee applies only if the object is Ready and still lists the digest.
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

	// It survived for ADR 0060's reason: it still carries its own tag. If a future zot keeps it
	// without one, this reports that the reason changed.
	if !strings.Contains(tags, `"`+ownTag(first)+`"`) {
		t.Errorf("%s@%s survived, but without its own tag %s -- so something other than ADR 0060 "+
			"kept it (did zot#4444 get fixed?). tags now: %s", repo, first, ownTag(first), tags)
	}
	if got := tagDigest(t, repo, rollingTag); got != second {
		t.Errorf("%s:%s resolves to %q, want the second build %s; the tag did not move, so this "+
			"test did not test a move", repo, rollingTag, got, second)
	}

	// Then the ordinary guarantee: with the object live, the refresher keeps the previous build
	// alive past a full window.
	sleepInCluster(t, watchFor(t))
	if !manifestExistsByDigestWithoutPulling(t, repo, first) {
		t.Fatalf("%s@%s survived the tag move but was collected within %ds while its object was "+
			"live and listed it in history. tags now: %s%s", repo, first, watchFor(t),
			tagsList(t, repo), registryLogs(t))
	}
}

// ownTag is ADR 0060's tag for a digest. Written out, not imported: the e2e checks the controller
// and should not borrow its definition.
func ownTag(digest string) string { return "digest-" + strings.TrimPrefix(digest, "sha256:") }

// applyRollingTagBuild creates or updates an ImageBuild publishing under the moving tag, with
// onConflict: Overwrite (under the default, Fail, the second build is refused -- tested elsewhere).
func applyRollingTagBuild(t *testing.T, name, dockerfile, repository string) {
	t.Helper()
	applyBuildToTagged(t, name, dockerfile, buildRegistry+"/"+repository, "["+rollingTag+"]",
		"    onConflict: Overwrite")
}

// awaitPublishedDigest waits for the object to report a published digest other than `previous`.
// Polled every second, since this file relies on asking the registry promptly after the push;
// polling the API server renews nothing.
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

// historyHasDigest reports whether status.history still names a digest.
func historyHasDigest(st imageBuildStatus, digest string) bool {
	for _, rec := range st.History {
		if rec.Digest == digest {
			return true
		}
	}
	return false
}

// historySummary renders status.history, tags included, for failure messages.
func historySummary(st imageBuildStatus) string {
	if len(st.History) == 0 {
		return "(empty)"
	}
	var parts []string
	for _, rec := range st.History {
		parts = append(parts, fmt.Sprintf("%s%v", rec.Digest, rec.Tags))
	}
	return strings.Join(parts, ", ")
}
