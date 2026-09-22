//go:build e2e

// Does a refresh actually keep an image alive?
//
// This file exists before any retention code does, and deliberately so. The registry-backed design
// rests on one assumption — that touching an image resets the clock the registry expires it by — and
// if that assumption is false then every later test passes vacuously, because a registry that never
// deletes anything satisfies "live images are not deleted" trivially.
//
// So the guarantee test carries a NEGATIVE CONTROL: something not refreshed, asserted to actually
// disappear. Without it, "the image is still there" is not evidence of anything.
//
// The registry is configured with a short window and frequent collection by the chart install in
// up.sh. A real deployment would use 30 days against an hourly refresh; the RATIO between the two
// is what makes it a guarantee rather than a race, and the ratio is what this file reproduces in
// miniature.
package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// retentionWindow mirrors E2E_WINDOW in up.sh, for the failure messages. It is not read by the
// registry; the two are kept in step by hand, and the negative control is what catches them
// drifting apart.
const retentionWindow = "30s"

// windowSeconds is retentionWindow as a number, for the tests that scale their wait by it.
func windowSeconds(t *testing.T) int {
	t.Helper()
	d, err := time.ParseDuration(retentionWindow)
	if err != nil {
		t.Fatalf("retentionWindow %q is not a duration: %v", retentionWindow, err)
	}
	return int(d.Seconds())
}

// collectionDeadline is how long a negative control waits for something to actually be collected.
//
// Far longer than the 30s window, and deliberately so. zot walks repositories on a rotation, so the
// gap between visits to any ONE repository is not bounded by gcInterval -- it grows with the number
// of repositories in the registry, and the bundled registry now holds every image the whole suite
// produces, build caches included. TestExpiryIsNotPrompt records the same thing from the other side.
//
// A deadline for "did it happen at all", not a measurement of when: overshooting costs nothing,
// because the poll returns as soon as the tag goes, while undershooting fails the suite and reads
// like a retention bug.
//
// A computed term over a floor. The term lengthens this automatically when a test adds a
// repository, instead of spending margin somebody else was relying on -- which is how the old
// constant of 420 was outgrown by ONE added ImageBuild. The floor exists because the model behind
// the term, that a repository is reached every (repositories x gcInterval), is not trustworthy: a
// run with a one-second sweep failed at the 121s that model predicted was ample.
func collectionDeadline(t *testing.T) int {
	t.Helper()
	const floor = 600
	if s := int((deployedGCDelay(t) + 4*deployedRotation(t)).Seconds()); s > floor {
		return s
	}
	return floor
}

// keepaliveRepo scopes these tests to the repository prefix the retention policy applies to, so
// nothing else in the suite can be collected out from under it.
func keepaliveRepo(name string) string { return "keepalive-" + name }

// watchFor is how long a survival test watches, derived from what the registry was deployed with
// rather than picked.
//
// Two things have to happen before content can die, so both are in it. Nothing younger than
// gcDelay is a candidate at all -- watch for less and "it survived" is a statement about the clock,
// not about retention. And being a candidate is not being collected: the collector has to reach
// THIS repository, which it does on a rotation, not on every sweep.
//
// An earlier version used gcInterval for the second term, which is the sweep and not the rotation.
// It was right only while the two were close.
func watchFor(t *testing.T) int {
	t.Helper()
	// CAPPED, because the rotation estimate is both expensive and wrong. Uncapped it reached 348s
	// against a flat 90s before -- 33 repositories at a 5s sweep -- and after that full wait the
	// control repository STILL had its tag, so the term did not buy the visit it exists to
	// guarantee. Paying linearly per repository for a guarantee that does not hold is the worst of
	// both.
	//
	// What makes the shorter wait safe is not this number: it is that every test using it now ends
	// in a negative control, which observes a real collection rather than predicting one.
	rotation := min(deployedRotation(t), 30*time.Second)
	return int((2*deployedGCDelay(t) + 2*rotation).Seconds())
}

// The load-bearing measurement: a PULL resets the retention clock.
//
// This is the fact the whole design turns on, and it is what makes the guarantee affordable. If
// staying alive required a PUSH, the controller would need a write credential to every repository it
// keeps alive — which is exactly what must not exist ("nothing should be able to push into the
// composer registry"). A pull needs no credential at all, moves no blobs, and cannot corrupt what it
// is protecting.
//
// zot's own vocabulary for this is `pulledWithin`. That it is documented is not evidence; this is.
func TestPullingAnImageKeepsItFromExpiring(t *testing.T) {
	// NOT t.Parallel(), unlike the survival-only tests in this package. This one ends in a negative
	// control that waits for a real deletion, and concurrent tests put more repositories in the
	// registry at once -- which is the thing the collector's rotation is slowest at. Tried, and it
	// failed: see the note on E2E_GC_FACTOR in up.sh.
	refreshed := keepaliveRepo("refreshed")
	abandoned := keepaliveRepo("abandoned")

	keptDigest := pushTinyImage(t, refreshed)
	pushTinyImage(t, abandoned)

	// Refreshed from INSIDE the cluster, in one shell loop, rather than by polling from the test.
	// Each kubectl exec costs seconds on a loaded runner, so a loop that sleeps between requests can
	// leave more time between pulls than the window it is trying to stay inside; the measurement
	// then reports on kubectl rather than on zot, and an earlier version of this file did exactly
	// that.
	//
	// BOTH references are pulled, and the honest reason is that it is unambiguously safe rather than
	// that it is known to be necessary.
	//
	// An earlier run concluded that pulling only the digest let the TAG be collected, and that
	// conclusion is withdrawn: it was drawn while the fixture was a hand-built manifest zot could
	// not track at all, which is the same defect that produced every other wrong answer here. Later
	// evidence points the other way — a tag survived while only its digest was being refreshed, which
	// is what one would expect if recency is tracked per manifest.
	//
	// So the refresh asks for everything it wants kept, because the cost is one small request and
	// the alternative is depending on which of those two readings is right.
	seconds := watchFor(t)
	requireCollectionPossible(t, time.Duration(seconds)*time.Second, "a refreshed image")
	refreshBothFor(t, refreshed, "v1", keptDigest, seconds)

	// THE GUARANTEE: content named by a live object is still there.
	if !manifestExistsByDigest(t, refreshed, keptDigest) {
		t.Fatalf("%s@%s was collected while being pulled every two seconds for 90s against a %s "+
			"window. A pull does NOT renew recency, and the registry-backed retention design is "+
			"inert -- nothing built on top of this measurement means anything.\ntags now: %s%s",
			refreshed, keptDigest, retentionWindow, tagsList(t, refreshed), registryLogs(t))
	}

	// And the name it was published under still resolves, which is what an operator expects.
	if !manifestExists(t, refreshed, "v1") {
		t.Fatalf("%s:v1 was collected while the tag itself was being pulled every two seconds, so "+
			"tags are not retainable by this mechanism and the design has to say so.\ntags now: %s%s",
			refreshed, tagsList(t, refreshed), registryLogs(t))
	}

	// THE NEGATIVE CONTROL, and it is deliberately about the TAG rather than about content.
	//
	// Something has to be observed dying, or "it is still there" is not evidence of anything. An
	// unrefreshed tag is what this registry demonstrably collects; unrefreshed CONTENT was measured
	// surviving far past its window, so asserting on that instead would fail for a reason with
	// nothing to do with the guarantee.
	//
	// Waited for rather than checked once. The first version checked immediately after the refresh
	// loop and passed — but a separate test that pushed an image and waited the same 90s saw its tag
	// still present, which is the same scenario with the opposite result. That is what a marginal
	// assertion looks like from the outside: green, and one slow runner away from red.
	eventuallyUntagged(t, abandoned, "v1", collectionDeadline(t))
}

// eventuallyUntagged waits for a tag to be collected, and fails loudly if it never is.
//
// Polls the TAGS LIST rather than the manifest, and that is the whole trick: fetching the manifest
// to ask whether it still exists would renew its recency and keep alive the very thing this is
// waiting to see die. A negative control that refreshes its own subject can never fire.
func eventuallyUntagged(t *testing.T, repository, tag string, maxSeconds int) {
	t.Helper()

	for waited := 0; waited < maxSeconds; waited += 10 {
		if !strings.Contains(tagsList(t, repository), `"`+tag+`"`) {
			return
		}
		sleepInCluster(t, 10)
	}

	t.Fatalf("%s:%s survived %ds with no pulls against a %s window, so this suite cannot observe a "+
		"deletion at all and every retention assertion here is vacuous. Check gcInterval, the "+
		"keepTags patterns, and the repository glob.\ntags now: %s%s",
		repository, tag, maxSeconds, retentionWindow, tagsList(t, repository), registryLogs(t))
}

// What is NOT true, recorded so that nobody builds on the assumption that it is.
//
// Expiry is not prompt. Measured: content untouched for 90s against a 30s window was still there,
// tag included, while an equivalent image in the guarantee test had been collected by then. Whatever
// schedules collection is coarser and less predictable than the window suggests.
//
// That is acceptable, for a stated reason rather than by shrugging. The guarantee is that live
// content survives, and expiry beyond it is explicitly best-effort (ADR 0031): leaking bytes is
// acceptable, losing live content is not. What would NOT be acceptable is quietly assuming storage
// is bounded by this policy on any particular schedule when the measurement says it is not.
//
// Reported rather than asserted. Pinning "does not expire promptly" down as an expectation would
// make an improvement in zot show up here as a regression — and asserting the opposite is what made
// the negative control marginal in the first place.
func TestExpiryIsNotPrompt(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("cold")
	digest := pushTinyImage(t, repo)

	// Three windows, not watchFor. This test asserts NOTHING -- it reports -- so it has no reason
	// to pay for a margin that exists to make an assertion safe, and it was the critical path of
	// the parallel group while doing it.
	waited := 3 * windowSeconds(t)
	sleepInCluster(t, waited)

	// The duration is read back rather than written in, because it is derived now -- the message
	// said "90s" for a while after it had stopped waiting 90s.
	t.Logf("after %ds with no pulls against a %s window: content alive=%v, tags now: %s",
		waited, retentionWindow, manifestExistsByDigest(t, repo, digest), tagsList(t, repo))
}

// A digest-only publication is kept while it is pulled, and reclaimed -- layers included -- once
// nothing pulls it. The end-to-end case for ADR 0060, and the only test here that watches bytes
// actually leave the registry.
//
// ADR 0010 makes referencing images BY DIGEST the recommended usage, so `push.tags: []` is a real
// mode and what it publishes may be what a running workload pulls. Two things are asserted about it,
// and both used to be false:
//
//   - It is NAMED, after its own digest (sha256-<hex>). Left untagged it was protected only by
//     keepUntagged, and configuring keepUntagged is what made zot keep every retired manifest
//     forever: removing a manifest's last tag deletes the digest's statistics, and zot then retains
//     a statistics-less untagged manifest without evaluating it. The layers it referenced were never
//     freed -- measured, 252 blob sweeps reclaimed 9 blobs.
//   - So once its own tag expires it is reclaimed. The registry runs WITHOUT keepUntagged here, and
//     requireKeepUntaggedOff refuses to run otherwise: with it configured the control below is
//     pinned and can never fire.
//
// The subject survives on its own tag's pull recency, renewed by pulls BY DIGEST -- which is what
// the controller's refresher issues, and so the property the whole scheme rests on. The control is
// the same kind of publication, left alone.
func TestADigestOnlyPublicationIsKeptWhilePulledAndReclaimedOnceRetired(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("digestonly")
	abandoned := keepaliveRepo("digestonly-control")

	requireUntaggedCollection(t, repo)
	requireKeepUntaggedOff(t)
	seconds := watchFor(t)
	requireCollectionPossible(t, time.Duration(seconds)*time.Second, "a digest-only publication")

	// THE CONTROL, FIRST, because it is the one that has to age: a build takes minutes, and every one
	// of them is time it spends untouched. From a DIFFERENT Dockerfile, so it has its own digest and
	// a layer no other image in this repository shares.
	abandonedDigest := pushUntaggedImageFrom(t, abandoned, "Dockerfile.other")

	// It has to have been there, or "it is gone" is the same observation as "it never arrived".
	// HEAD, so the check is not itself the pull that starts the control's clock over.
	if !manifestExistsByDigestWithoutPulling(t, abandoned, abandonedDigest) {
		t.Fatalf("%s@%s was not in the registry immediately after being published, so it cannot "+
			"serve as a control.%s", abandoned, abandonedDigest, registryLogs(t))
	}
	// And it was named after its own digest -- the first half of ADR 0060, on the mode it matters
	// most for.
	if tags := tagsList(t, abandoned); !strings.Contains(tags, `"`+ownTag(abandonedDigest)+`"`) {
		t.Fatalf("a push.tags: [] publication was left untagged (tags: %s). Untagged content is what "+
			"a collector reclaims by age, whoever is pulling it by digest.", tags)
	}
	// The layer to watch go. Its top layer is the one this Dockerfile adds, so nothing else in the
	// repository holds it. Read with a GET -- a pull -- deliberately while the control is minutes old:
	// its push recency covers it for the same window anyway, so this moves its clock by seconds.
	layer := topLayer(t, abandoned, abandonedDigest)

	// LAST before the refresh: until the first pull, the subject's own tag is protected only by push
	// recency, for one window.
	digest := pushUntaggedImage(t, repo)
	if abandonedDigest == digest {
		t.Fatalf("the control and its subject are the same content (%s); a control that cannot be "+
			"told apart from what it is controlling for is not worth having", digest)
	}

	// Pulled BY DIGEST only, never by its tag -- as the refresher does.
	refreshFor(t, repo, digest, seconds)

	if !manifestExistsByDigest(t, repo, digest) {
		t.Fatalf("a digest-only publication was collected while being pulled by digest. ADR 0010 "+
			"tells users to reference digests, so this deletes content a rescheduled pod re-pulls. "+
			"(%s@%s)\ntags now: %s%s", repo, digest, tagsList(t, repo), registryLogs(t))
	}
	if tags := tagsList(t, repo); !strings.Contains(tags, `"`+ownTag(digest)+`"`) {
		t.Fatalf("%s@%s survived, but its own tag did not: pulls by digest did not renew it, so a "+
			"digest-only publication is kept alive only until the tag's window runs out -- by the "+
			"refresher as much as by this test. tags now: %s%s", repo, digest, tags, registryLogs(t))
	}

	// THE NEGATIVE CONTROL, and it is the leak's test as much as the guarantee's: the manifest has to
	// GO once nothing pulls it, and then the bytes it held have to go with it.
	eventuallyGone(t, abandoned, abandonedDigest, collectionDeadline(t))
	eventuallyBlobGone(t, abandoned, layer, collectionDeadline(t))
}

// eventuallyGone waits for a manifest to actually be collected.
//
// The digest-side counterpart of eventuallyUntagged: a tag going is not its manifest going, and a
// control that only watches tags cannot speak for the content -- or for the bytes behind it.
//
// HEAD, and that is the whole of this control's correctness -- for the reason eventuallyUntagged
// states and this function ignored. A GET is a pull: zot's GetManifest handler calls
// meta.OnGetManifest, which calls MetaDB.UpdateStatsOnDownload(repo, reference) and stamps the
// digest's lastPullTimestamp. keepUntagged.pulledWithin is the retention window, and this polls
// every 10s, so every GET renewed the control's recency inside its own window and it could never
// become a candidate. That is not a hypothesis: the control survived 709s of polling, to the
// second, on repeated runs, which is a deadline being waited out rather than a collection being
// missed.
//
// zot's CheckManifest handler (HEAD) resolves the manifest through the same getImageManifest and
// returns the same 200/404 -- and records nothing. So HEAD asks exactly the question this needs,
// "is it still there", without being the answer's cause. Verified in zot v2.1.21: OnGetManifest is
// called from GetManifest and from nowhere else.
func eventuallyGone(t *testing.T, repository, digest string, maxSeconds int) {
	t.Helper()

	for waited := 0; waited < maxSeconds; waited += 10 {
		if !manifestExistsByDigestWithoutPulling(t, repository, digest) {
			return
		}
		sleepInCluster(t, 10)
	}

	t.Fatalf("%s@%s survived %ds unpulled -- polled with HEAD, so this wait did not renew it -- and "+
		"so this suite cannot observe a manifest being collected at all, which is the only thing "+
		"that makes the assertion above evidence of anything. Check deleteUntagged and the "+
		"repository glob against %q -- and keepUntagged: if it is configured, zot keeps a manifest "+
		"whose last tag expired forever, logging `untagged manifest statistics not found`.\n"+
		"tags now: %s%s",
		repository, digest, maxSeconds, repository, tagsList(t, repository), registryLogs(t))
}

// eventuallyBlobGone waits for a blob to leave a repository once no manifest there references it.
//
// The part of retention anyone actually pays for. A manifest is a few kilobytes; its layers are the
// image. zot frees a blob only once nothing references it and it is older than gcDelay, so this is
// asked AFTER the manifest is gone, and only of a layer nothing else in the repository holds.
// A HEAD on a blob records nothing, so polling cannot keep it alive.
func eventuallyBlobGone(t *testing.T, repository, digest string, maxSeconds int) {
	t.Helper()

	for waited := 0; waited < maxSeconds; waited += 10 {
		if !blobExists(t, repository, digest) {
			return
		}
		sleepInCluster(t, 10)
	}

	t.Fatalf("%s's layer %s survived %ds after the only manifest referencing it was collected. The "+
		"registry is reclaiming manifests and keeping their bytes, which is the leak ADR 0060 was "+
		"written against in a different shape.%s", repository, digest, maxSeconds, registryLogs(t))
}

func blobExists(t *testing.T, repository, digest string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "head-"+shortName(repository, "blob"), "HEAD",
			fmt.Sprintf("/v2/%s/blobs/%s", repository, digest), "", ""),
		"200 OK")
}

// topLayer is the last layer of a single-platform manifest: the one its own Dockerfile added.
func topLayer(t *testing.T, repository, digest string) string {
	t.Helper()
	out := registryRequest(t, "get-"+shortName(repository, "layers"), "GET",
		fmt.Sprintf("/v2/%s/manifests/%s", repository, digest), "", "")
	i := strings.Index(out, "{")
	if i < 0 {
		t.Fatalf("no manifest body for %s@%s:\n%s", repository, digest, out)
	}
	var mf struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal([]byte(out[i:]), &mf); err != nil || len(mf.Layers) == 0 {
		t.Fatalf("%s@%s is not a single-platform manifest with layers (%v); this test needs one to "+
			"watch a layer go:\n%s", repository, digest, err, out[i:])
	}
	return mf.Layers[len(mf.Layers)-1].Digest
}

// refreshBothFor pulls a tag AND a digest every two seconds for the given number of seconds,
// in-cluster.
//
// The exec blocks for the whole duration, which is the point: the loop IS the refresh, and nothing
// about the test's own latency can get between two pulls.
func refreshBothFor(t *testing.T, repository, tag, digest string, seconds int) {
	t.Helper()
	ensureCurlPod(t)

	base := "http://" + buildRegistry + "/v2/" + repository + "/manifests/"
	script := fmt.Sprintf(
		"i=0; while [ $i -lt %d ]; do "+
			"curl -sS -o /dev/null -H 'Accept: application/vnd.oci.image.manifest.v1+json' %s; "+
			"curl -sS -o /dev/null -H 'Accept: application/vnd.oci.image.manifest.v1+json' %s; "+
			"sleep 2; i=$((i+2)); done",
		seconds, base+tag, base+digest)

	// No timeout wrapper: the exec is expected to block for the whole duration, and go test's own
	// timeout is the outer bound.
	if out, err := kubectl(t, "-n", buildNamespace, "exec", curlPod, "--",
		"sh", "-c", script); err != nil {
		t.Fatalf("refreshing %s: %v\n%s", repository, err, out)
	}
}

// refreshFor pulls one reference, for the case where there is only one to keep alive.
func refreshFor(t *testing.T, repository, ref string, seconds int) {
	t.Helper()
	refreshBothFor(t, repository, ref, ref, seconds)
}

// sleepInCluster waits without touching the registry, so a cold repository stays cold.
func sleepInCluster(t *testing.T, seconds int) {
	t.Helper()
	ensureCurlPod(t)
	if out, err := kubectl(t, "-n", buildNamespace, "exec", curlPod, "--",
		"sleep", fmt.Sprint(seconds)); err != nil {
		t.Fatalf("waiting: %v\n%s", err, out)
	}
}

// registryLogs returns what the registry itself says it decided.
//
// Included in every retention failure because the alternative is guessing, and guessing cost several
// runs here: three plausible explanations for a failure were wrong in a row, and the registry knew
// the answer the whole time.
func registryLogs(t *testing.T) string {
	t.Helper()
	// Selected by label, not by `statefulset/...`, which picks ONE arbitrary pod. With read
	// replicas the collector that deleted the image is very likely a different pod, so naming the
	// StatefulSet would print a pod that did nothing -- worse than no logs, because it misleads.
	// --prefix is not optional once more than one pod can answer.
	out, err := kubectl(t, "-n", operatorNamespace, "logs",
		"-l", "oci-composer.lhns.de/registry-role=serve", "--prefix", "--tail=120")
	if err != nil {
		return "\n\n(registry logs unavailable: " + err.Error() + ")"
	}
	return "\n\nregistry logs:\n" + out
}

// tagsList reports what tags a repository currently has, for failure messages. A retention question
// answered with "it is gone" and nothing else is not much of an answer.
func tagsList(t *testing.T, repository string) string {
	t.Helper()
	out := registryRequest(t, "tags-"+shortName(repository, "list"), "GET",
		"/v2/"+repository+"/tags/list", "", "")
	if i := strings.LastIndex(out, "{"); i >= 0 {
		return out[i:]
	}
	return strings.TrimSpace(out)
}

// pushTinyImage publishes a REAL image, by running a build, and returns its manifest digest.
//
// It started as a hand-crafted manifest — an empty config, no layers — which is valid per the
// distribution spec and was accepted with a 201. That cost six runs. zot could not derive image
// metadata from it, so on every pull it logged
//
//	failed to update stats on download image ... error: image meta not found
//
// and recorded nothing. `pulledWithin` then had nothing to match on and the tag expired however
// often it was fetched, which reads exactly like "a pull does not renew recency" — a conclusion
// about the registry drawn from a defect in the fixture. Fleshing the manifest out did not help;
// what settled it was that the error appeared ONLY for the fixture, never for images the builder had
// actually pushed.
//
// So the fixture is now the real thing. It costs a build per repository, which is the price of
// measuring the registry's behaviour on the images this project actually produces rather than on a
// reduction of them that the registry treats differently.
//
// The general lesson is worth more than the fix: a fixture pared down until it is minimal for the
// code under test can quietly stop being valid input for the system AROUND it, and the resulting
// failure looks like a finding rather than like a bug.
func pushTinyImage(t *testing.T, repository string) string {
	t.Helper()
	return pushTinyImageFrom(t, repository, "Dockerfile")
}

// pushTinyImageFrom publishes a fixture built from a NAMED Dockerfile, so a caller can get content
// whose digest differs from everything else in the suite.
//
// A digest-level control keeps using this, but the reason given for it is WITHDRAWN. It was
// introduced on the reading that zot keys retention statistics by digest alone, so that two
// repositories holding identical content share one clock and pulling either renews both. That is
// not what zot does: statistics live in the per-repository metadata (BoltDB repoMeta.Statistics,
// keyed by digest WITHIN a repository), so two repositories are independent. The control was not
// being renewed by its subject's pulls; it was being retained because untagging it had deleted its
// statistics -- the defect ADR 0060 describes, and that keepUntagged turns into a permanent leak.
//
// Distinct content is kept anyway -- it costs one build, the test still asserts the digests differ,
// and a control that cannot be confused with its subject is worth that much on its own.
func pushTinyImageFrom(t *testing.T, repository, dockerfile string) string {
	t.Helper()
	return publishFixture(t, repository, dockerfile, applyBuildTo)
}

// pushUntaggedImage publishes a fixture with push.tags empty -- the digest-only mode.
//
// Not untagged any more, and that is the point: the controller names it after its own digest (ADR
// 0060), so the only name it carries is one no other content can take.
func pushUntaggedImage(t *testing.T, repository string) string {
	t.Helper()
	return pushUntaggedImageFrom(t, repository, "Dockerfile")
}

func pushUntaggedImageFrom(t *testing.T, repository, dockerfile string) string {
	t.Helper()
	return publishFixture(t, repository, dockerfile, applyBuildToUntagged)
}

// publishFixture runs one build into the named repository and returns the manifest digest, with
// `apply` deciding whether the result gets a tag.
func publishFixture(t *testing.T, repository, dockerfile string,
	apply func(t *testing.T, name, dockerfile, repository string, extraSpec ...string),
) string {
	t.Helper()

	// The repository is <host>/<name>; the object is named after the name half.
	name := repository
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}

	apply(t, name, dockerfile, buildRegistry+"/"+repository)
	buildEventually(t, "the retention fixture "+name+" to publish", func() error {
		st := buildStatus(t, name)
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("no digest yet: %+v", readyCondition(st))
		}
		return nil
	})
	digest := buildStatus(t, name).Artifact.Digest

	// The object is deleted the moment it has published, and this is not tidying up.
	//
	// The controller now refreshes every live ImageBuild, which is the whole point of
	// retention_controller_test.go — and it would silently destroy every assertion in THIS file. The
	// negative controls here wait for something to be collected; a live object would keep it alive
	// forever, so they would hang until their deadline and then report that the registry collects
	// nothing, which is the exact false conclusion this file exists to rule out.
	//
	// So the two files divide the question cleanly. This one measures the REGISTRY with no
	// controller involvement: the images here are orphans, and only what the test pulls keeps them
	// alive. The other measures the CONTROLLER, and touches the registry not at all.
	mustKubectl(t, "-n", buildNamespace, "delete", "imagebuild", name)
	return digest
}

// manifestExists fetches a manifest with GET, which is what a real pull is.
//
// Whether HEAD also renews recency is no longer unmeasured: it does not. zot records a download --
// MetaDB.UpdateStatsOnDownload, which is what sets the lastPullTimestamp `pulledWithin` reads --
// only from meta.OnGetManifest, and only GetManifest calls that. CheckManifest, the HEAD handler,
// resolves the manifest and returns without touching the metadata database (zot v2.1.21).
//
// An early version of this file used HEAD here, and when the tagged case was collected anyway the
// obvious reading was that HEAD does not count as a pull. That reading happened to be true and was
// still wrong as an inference: the causes were a retention policy that matched no tags and then a
// refresh that pulled the digest but not the tag. Recorded because it is the kind of plausible,
// tidy explanation worth being suspicious of even when it later turns out to hold.
//
// GET stays HERE regardless: these callers are asserting that something SURVIVED, so renewing it is
// harmless, it is unambiguously a pull, and the refresh has no reason to economise on the one
// request the whole guarantee depends on. A caller waiting for something to DIE must use
// manifestExistsByDigestWithoutPulling instead.
func manifestExists(t *testing.T, repository, tag string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "get-"+shortName(repository, tag), "GET",
			fmt.Sprintf("/v2/%s/manifests/%s", repository, tag), "", ""),
		"200 OK")
}

func manifestExistsByDigest(t *testing.T, repository, digest string) bool {
	t.Helper()
	// GET rather than HEAD, for the reason manifestExists gives.
	return strings.Contains(
		registryRequest(t, "get-"+shortName(repository, "digest"), "GET",
			fmt.Sprintf("/v2/%s/manifests/%s", repository, digest), "", ""),
		"200 OK")
}

// manifestExistsByDigestWithoutPulling answers the same question as manifestExistsByDigest without
// being a pull, for the negative controls.
//
// A control that renews its own subject can never fire, and the renewal is invisible in the result:
// every poll answers "still there", which is exactly what a broken registry would also answer. The
// tag-side control avoids this by reading the tags list, which names no manifest; the digest side
// has no listing to read -- an untagged manifest appears in no API but the one that fetches it --
// so it asks with HEAD instead, which zot resolves identically and records nothing against.
func manifestExistsByDigestWithoutPulling(t *testing.T, repository, digest string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "head-"+shortName(repository, "digest"), "HEAD",
			fmt.Sprintf("/v2/%s/manifests/%s", repository, digest), "", ""),
		"200 OK")
}

// There is no deleteTag helper any more, and its absence is worth a line.
//
// Removing a tag looked like the obvious way to produce an untagged manifest, and it produces
// something else: in zot v2.1.21 the digest's statistics go with the last tag that named it, after
// which the collector retains the manifest unconditionally and never evaluates it against any rule.
// A fixture made that way cannot be collected and cannot be protected -- it is inert, and so is
// every assertion about it. With keepUntagged configured, it is also what every tag
// EXPIRING did to the manifest behind it -- which is what ADR 0060 came out of.
