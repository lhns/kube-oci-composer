//go:build e2e

// Does moving a rolling tag delete the digest it moved off?
//
// ADR 0031 says an image named by a live object's retained status.history is never deleted, by
// anything. ADR 0010 tells workloads to reference digests, so the digest a rolling tag USED to
// point at is not a historical curiosity: it is what a running pod re-pulls when it is rescheduled,
// and what a rollback resolves to.
//
// retention_test.go and retention_controller_test.go together establish that nothing EXPIRES out
// from under a live object. This file asks a different question, and the distinction is the whole
// point of it: whether publishing the NEXT build destroys the previous one as a side effect of the
// push itself -- before any retention decision is ever taken, and therefore where no amount of
// refreshing can help. The refresher pulls (internal/retention/refresher.go: `remote.Image`, a GET
// and never a push), and a pull cannot resurrect content the registry has already dropped.
//
// Everything here is therefore asserted PROMPTLY -- seconds after the second build publishes, well
// inside gcDelay and far inside the retention window -- so that "it expired" is not available as an
// explanation for a failure. That promptness is not decoration; it is what separates this file's
// question from the two files next to it.
//
// Established by hand against zot v2.1.21 with this chart's rendered policy, which is what these
// tests exist to confirm or refute through the REAL controller path:
//
//   - Moving a tag from digest A to digest B makes A stop resolving within about a second of the
//     push, with no retention decision logged for it at all. zot's CheckIfIndexNeedsUpdate
//     (pkg/storage/common/common.go) splices the descriptor carrying the tag out of
//     index.Manifests and appends the new one, without re-adding the old digest as an untagged
//     entry.
//   - A digest carrying a SECOND, unique tag survived, because a separate descriptor still names
//     it. The spec-hash-tag pattern this project recommends is therefore protective -- but nothing
//     enforces it: push.tags is entirely user-supplied and no controller adds a per-build tag.
//
// The two tests below are that pair. If they behave as the harness predicts, the first documents
// the defect and the second documents the workaround.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// rollingTag is the name both tests move. Named once, because the failure message quotes it and a
// message naming a different tag from the one the fixture pushed would be worse than no message.
const rollingTag = "main"

// TestMovingARollingTagKeepsThePreviousDigest is the question, stated as the guarantee.
//
// One object, built twice from different content, publishing under one rolling tag with
// onConflict: Overwrite -- which is what the CRD documents as the correct setting for a tag meant
// to move ("A tag meant to MOVE therefore conflicts under Fail ... and wants Overwrite instead").
// So this is not an exotic configuration; it is the configuration the API tells people to use.
//
// The object is NEVER deleted, unlike the fixtures in retention_test.go. That is deliberate: a live
// object is precisely the condition ADR 0031's guarantee is stated over, and the controller is
// refreshing both of its history entries throughout.
func TestMovingARollingTagKeepsThePreviousDigest(t *testing.T) {
	t.Parallel()
	rollingTagMoveCase(t, "tagmove", "[main]", "[main]", "")
}

// TestASecondUniqueTagSurvivesTheRollingTagMoving is the same sequence with the recommended
// workaround applied, and it earns its place by being the CONTROL for the test above.
//
// If both behave as the hand-driven harness predicts -- this one green, the one above red -- then
// the difference between them is exactly one extra tag, which localises the mechanism to the tag
// descriptor rather than to retention, to the controller, or to anything about the content. If they
// behave the SAME way, whichever way, then the explanation is somewhere else and the first test's
// failure message is wrong about the cause.
//
// The unique tag is per-build, which is what a spec-hash tag would be in a real deployment.
func TestASecondUniqueTagSurvivesTheRollingTagMoving(t *testing.T) {
	t.Parallel()
	rollingTagMoveCase(t, "tagmove-unique", "[main, build-a]", "[main, build-b]", "build-a")
}

// rollingTagMoveCase runs the whole sequence: publish, republish different content under the same
// rolling tag, then ask -- at once -- whether the first digest is still there.
//
// survivingTag is the unique tag the first build carried, or "" when it carried only the rolling
// one. It is reported rather than asserted on its own: the digest is what a pinned workload names,
// and a tag that resolves while the digest does not would be a stranger finding than either
// outcome this is looking for.
func rollingTagMoveCase(t *testing.T, suffix, firstTags, secondTags, survivingTag string) {
	t.Helper()

	repo := keepaliveRepo(suffix)
	name := keepaliveRepo(suffix)

	// FIRST BUILD. Dockerfile, under the rolling tag.
	applyRollingTagBuild(t, name, "Dockerfile", repo, firstTags)
	first := awaitPublishedDigest(t, name, "")

	// It has to have been there, or "it is gone" is indistinguishable from "it never arrived" --
	// the trap the untagged control in retention_test.go fell into.
	//
	// HEAD, not GET, and for a reason that matters more here than anywhere else in this suite: a
	// GET goes through zot's OnGetManifest -> UpdateStatsOnDownload and stamps the digest's
	// lastPullTimestamp. A check that renews what it is checking makes the check itself protective,
	// and a test whose own probes are the reason the subject survives measures nothing.
	if !manifestExistsByDigestWithoutPulling(t, repo, first) {
		t.Fatalf("%s@%s did not resolve immediately after its own build reported it published, "+
			"so this test has no subject. Either the push did not land or status.artifact.digest "+
			"names something the registry does not have.%s", repo, first, registryLogs(t))
	}

	// SECOND BUILD, same object, different content, same rolling tag.
	//
	// A DIFFERENT Dockerfile rather than a nudged annotation, because if the two builds produce the
	// same digest the tag never moves and every assertion below passes vacuously. This suite has
	// already paid for that mistake once, with a negative control that shared a digest with its
	// subject, so the digests are asserted to differ rather than assumed to.
	applyRollingTagBuild(t, name, "Dockerfile.other", repo, secondTags)
	second := awaitPublishedDigest(t, name, first)
	published := time.Now()

	if second == first {
		t.Fatalf("both builds produced %s, so the rolling tag never moved and this test asks "+
			"nothing. The two Dockerfiles must differ in content.", first)
	}

	// THE ASSERTION, taken at once.
	//
	// Order matters: the registry is asked before the API server, because every second spent
	// reading status is a second in which expiry becomes a marginally more available explanation
	// for a 404. The object's state is gathered afterwards, purely to close off "the object went
	// away" as a reading of the result.
	alive := manifestExistsByDigestWithoutPulling(t, repo, first)
	elapsed := time.Since(published)

	st := buildStatus(t, name)
	ready := readyCondition(st)
	inHistory := historyHasDigest(st, first)

	gcDelay := deployedGCDelay(t)
	t.Logf("%s: first=%s second=%s; first digest alive=%v, checked %s after the second publish "+
		"(gcDelay=%s, window=%s); Ready=%+v, first digest in history=%v; tags now: %s",
		repo, first, second, alive, elapsed.Round(time.Millisecond), gcDelay, retentionWindow,
		ready, inHistory, tagsList(t, repo))

	// The object's own state, so a failure below cannot be dismissed as the object having gone.
	// Checked whether or not the digest survived: a live, Ready object still listing the digest is
	// the precondition that makes the registry's answer mean anything.
	if ready == nil || ready.Status != "True" {
		t.Errorf("%s is not Ready after its second build (%+v), so the guarantee this test is "+
			"about -- an image named by a LIVE object's history -- is not in force and the "+
			"registry's answer below is about something else", name, ready)
	}
	if !inHistory {
		t.Errorf("%s no longer lists %s in status.history after the second build, so ADR 0031's "+
			"guarantee does not cover it and this test is measuring the wrong thing. history now: "+
			"%s", name, first, historySummary(st))
	}

	if alive {
		// It survived the prompt check, which is the one that isolates the mechanism. Whether it
		// then expires is a different question -- the one retention_controller_test.go answers --
		// so it is reported and not asserted.
		if survivingTag != "" {
			t.Logf("the unique tag %s:%s resolves: %v", repo, survivingTag,
				strings.Contains(tagsList(t, repo), `"`+survivingTag+`"`))
		}
		sleepInCluster(t, windowSeconds(t))
		t.Logf("after a further %s (one full retention window) with the object still live, "+
			"%s@%s alive=%v; tags now: %s", retentionWindow, repo, first,
			manifestExistsByDigestWithoutPulling(t, repo, first), tagsList(t, repo))
		return
	}

	t.Fatalf(`%s@%s stopped resolving %s after a LATER BUILD OF THE SAME OBJECT moved %q off it.

What that means, plainly: publishing a new build under a rolling tag DELETED content that a live,
Ready object still lists in status.history. A workload that pinned that digest -- which is what
ADR 0010 tells workloads to do -- gets a 404 the next time it is rescheduled, and a rollback to the
previous build cannot resolve its image.

This is not expiry, and the timings are here so that cannot be argued. The check above was taken %s
after the second build published. The object was live and Ready across the whole sequence, so the
controller was refreshing every digest in status.history with a GET throughout -- which is exactly
what resets the recency the %s window is measured against. For expiry to explain this, the window
would have had to lapse inside that %s gap, on a digest that was being pulled.

(gcDelay=%s is reported for completeness rather than as part of the argument: it floors how YOUNG
content can be collected, and this digest is minutes old, so it bounds nothing here.)

If it were expiry the registry would have logged a retention decision for this digest. The logs
below are included so that can be read rather than guessed at.

It is a side effect of the PUSH, and that is why the refresher cannot defend against it. The
refresher GETs %s@<digest> to renew recency (internal/retention/refresher.go). Renewing recency on
content the registry has already removed from the index is not something a pull can do, so this
failure is invisible to every mechanism ADR 0031 relies on.

  first build:  %s  tags %s
  second build: %s  tags %s
  Ready:        %+v
  history:      %s
  tags now:     %s%s`,
		repo, first, elapsed.Round(time.Millisecond), rollingTag,
		elapsed.Round(time.Millisecond), retentionWindow,
		elapsed.Round(time.Millisecond), gcDelay, repo,
		first, firstTags, second, secondTags, ready, historySummary(st),
		tagsList(t, repo), registryLogs(t))
}

// applyRollingTagBuild creates or updates an ImageBuild that publishes under a MOVING tag.
//
// onConflict: Overwrite is not a workaround for this test's convenience -- it is what the CRD
// documents for a tag meant to move, and under the default (Fail) the second build would be refused
// before it ever ran, which is a different and already-tested behaviour.
func applyRollingTagBuild(t *testing.T, name, dockerfile, repository, tags string) {
	t.Helper()
	applyBuildToTagged(t, name, dockerfile, buildRegistry+"/"+repository, tags,
		"    onConflict: Overwrite")
}

// awaitPublishedDigest waits for the object to report a published digest that is not `previous`.
//
// Polled at one second rather than at the suite's five, and locally rather than in-cluster. The
// whole value of this file rests on asking the registry PROMPTLY after the push, and a five-second
// poll would put most of gcDelay between the push and the question for no reason. kubectl talks to
// the API server and never to the registry, so polling cannot renew anything.
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

// historySummary renders status.history for a failure message. "It is gone" with nothing else is
// not much of an answer, and the tags each record carries are what decide whether a descriptor
// still names the digest.
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
