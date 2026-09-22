//go:build e2e

// Does pulling an image keep it from expiring in the registry?
//
// The registry-backed retention design (ADR 0031) rests on a pull resetting the registry's expiry
// clock. A registry that never deletes anything satisfies "live images survive" trivially, so every
// survival assertion here is paired with a NEGATIVE CONTROL: something not refreshed, observed to
// actually disappear.
//
// This file measures the REGISTRY alone: its fixtures' objects are deleted once published, and only
// the test's own pulls keep anything alive. retention_controller_test.go measures the controller.
// up.sh deploys a compressed window with the production ratio of window to refresh interval.
package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// retentionWindow mirrors E2E_WINDOW in up.sh, for failure messages. Kept in step by hand; the
// negative controls catch drift.
const retentionWindow = "30s"

// windowSeconds is retentionWindow in seconds.
func windowSeconds(t *testing.T) int {
	t.Helper()
	d, err := time.ParseDuration(retentionWindow)
	if err != nil {
		t.Fatalf("retentionWindow %q is not a duration: %v", retentionWindow, err)
	}
	return int(d.Seconds())
}

// collectionDeadline is how long a negative control waits for a collection to happen at all.
//
// Far above the window: zot visits repositories in rotation, so the wait grows with the number of
// repositories in the registry. The computed term tracks that as tests add repositories; the floor
// is there because the rotation model has underestimated before. Overshooting costs nothing, since
// the poll returns as soon as the subject goes.
func collectionDeadline(t *testing.T) int {
	t.Helper()
	const floor = 600
	if s := int((deployedGCDelay(t) + 4*deployedRotation(t)).Seconds()); s > floor {
		return s
	}
	return floor
}

// keepaliveRepo prefixes a repository into the scope the retention policy governs.
func keepaliveRepo(name string) string { return "keepalive-" + name }

// watchFor is how long a survival test watches, derived from the deployed registry. Content must
// first outlive gcDelay to be a candidate at all, and then the collector must reach its repository.
//
// The rotation term is capped at 30s: uncapped it grew linearly with repositories without reliably
// buying a collector visit. What makes a short watch meaningful is the negative control each test
// ends with, which observes a real collection.
func watchFor(t *testing.T) int {
	t.Helper()
	rotation := min(deployedRotation(t), 30*time.Second)
	return int((2*deployedGCDelay(t) + 2*rotation).Seconds())
}

// TestPullingAnImageKeepsItFromExpiring -- a PULL resets the retention clock. The design depends on
// it: a pull needs no write credential and cannot corrupt what it protects.
func TestPullingAnImageKeepsItFromExpiring(t *testing.T) {
	// Not parallel: it ends waiting for a real deletion, and concurrent tests add repositories that
	// slow the collector's rotation.
	refreshed := keepaliveRepo("refreshed")
	abandoned := keepaliveRepo("abandoned")

	keptDigest := pushTinyImage(t, refreshed)
	pushTinyImage(t, abandoned)

	// Refreshed in one in-cluster shell loop: a test-side loop of kubectl execs can leave gaps
	// longer than the window. Both the tag and the digest are pulled -- cheap, and safe whichever
	// way zot tracks recency.
	seconds := watchFor(t)
	requireCollectionPossible(t, time.Duration(seconds)*time.Second, "a refreshed image")
	refreshBothFor(t, refreshed, "v1", keptDigest, seconds)

	// THE GUARANTEE: pulled content is still there.
	if !manifestExists(t, refreshed, keptDigest) {
		t.Fatalf("%s@%s was collected while being pulled every two seconds against a %s window: a "+
			"pull does NOT renew recency, so registry-backed retention is inert.\ntags now: %s%s",
			refreshed, keptDigest, retentionWindow, tagsList(t, refreshed), registryLogs(t))
	}
	if !manifestExists(t, refreshed, "v1") {
		t.Fatalf("%s:v1 was collected while the tag itself was being pulled every two seconds, so "+
			"tags are not retainable by this mechanism.\ntags now: %s%s",
			refreshed, tagsList(t, refreshed), registryLogs(t))
	}

	// THE NEGATIVE CONTROL: the unrefreshed TAG goes. Waited for rather than checked once, since
	// collection timing is not prompt (TestExpiryIsNotPrompt).
	eventuallyUntagged(t, abandoned, "v1", collectionDeadline(t))
}

// eventuallyUntagged waits for a tag to be collected, and fails if it never is.
//
// It polls the TAGS LIST, not the manifest: fetching the manifest would be a pull, renewing the very
// thing this waits to see die.
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

// TestExpiryIsNotPrompt records, without asserting, that untouched content can outlive its window
// by a wide margin. Expiry is best-effort (ADR 0031); asserting either way would turn a zot change
// into a failure.
func TestExpiryIsNotPrompt(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("cold")
	digest := pushTinyImage(t, repo)

	// Three windows; this only reports, so it needs no safety margin.
	waited := 3 * windowSeconds(t)
	sleepInCluster(t, waited)

	t.Logf("after %ds with no pulls against a %s window: content alive=%v, tags now: %s",
		waited, retentionWindow, manifestExists(t, repo, digest), tagsList(t, repo))
}

// TestADigestOnlyPublicationIsKeptWhilePulledAndReclaimedOnceRetired -- ADR 0060 end to end, and the
// only test watching bytes leave the registry.
//
// A `push.tags: []` publication (the mode ADR 0010 recommends consuming) must be NAMED after its own
// digest, survive while pulled by digest (as the refresher does), and once unpulled be reclaimed
// together with its layers. Runs only with keepUntagged off (requireKeepUntaggedOff): with it on,
// zot keeps retired manifests forever and the control could never fire.
func TestADigestOnlyPublicationIsKeptWhilePulledAndReclaimedOnceRetired(t *testing.T) {
	t.Parallel()

	repo := keepaliveRepo("digestonly")
	abandoned := keepaliveRepo("digestonly-control")

	requireUntaggedCollection(t, repo)
	requireKeepUntaggedOff(t)
	seconds := watchFor(t)
	requireCollectionPossible(t, time.Duration(seconds)*time.Second, "a digest-only publication")

	// The control first, so it ages during the subject's build. A different Dockerfile gives it its
	// own digest and a layer nothing else in its repository holds.
	abandonedDigest := pushUntaggedImageFrom(t, abandoned, "Dockerfile.other")

	// It must have arrived, or "gone" proves nothing. HEAD, so the check is not a pull.
	if !manifestExistsByDigestWithoutPulling(t, abandoned, abandonedDigest) {
		t.Fatalf("%s@%s was not in the registry immediately after being published, so it cannot "+
			"serve as a control.%s", abandoned, abandonedDigest, registryLogs(t))
	}
	if tags := tagsList(t, abandoned); !strings.Contains(tags, `"`+ownTag(abandonedDigest)+`"`) {
		t.Fatalf("a push.tags: [] publication was left untagged (tags: %s). Untagged content is what "+
			"a collector reclaims by age, whoever is pulling it by digest.", tags)
	}
	// The layer to watch go. Read with a GET (a pull) while the control is still young enough that
	// its push recency covers it anyway.
	layer := topLayer(t, abandoned, abandonedDigest)

	// Pushed last: until its first pull the subject is protected only by push recency.
	digest := pushUntaggedImage(t, repo)
	if abandonedDigest == digest {
		t.Fatalf("the control and its subject are the same content (%s); a control that cannot be "+
			"told apart from what it is controlling for is not worth having", digest)
	}

	// Pulled BY DIGEST only, as the refresher does.
	refreshFor(t, repo, digest, seconds)

	if !manifestExists(t, repo, digest) {
		t.Fatalf("a digest-only publication was collected while being pulled by digest. ADR 0010 "+
			"tells users to reference digests, so this deletes content a rescheduled pod re-pulls. "+
			"(%s@%s)\ntags now: %s%s", repo, digest, tagsList(t, repo), registryLogs(t))
	}
	if tags := tagsList(t, repo); !strings.Contains(tags, `"`+ownTag(digest)+`"`) {
		t.Fatalf("%s@%s survived, but its own tag did not: pulls by digest did not renew it, so a "+
			"digest-only publication is kept alive only until the tag's window runs out. "+
			"tags now: %s%s", repo, digest, tags, registryLogs(t))
	}

	// THE NEGATIVE CONTROL: the unpulled manifest goes, and then its bytes.
	eventuallyGone(t, abandoned, abandonedDigest, collectionDeadline(t))
	eventuallyBlobGone(t, abandoned, layer, collectionDeadline(t))
}

// eventuallyGone waits for a manifest to be collected.
//
// Polls with HEAD, which is what makes this control work: a GET is a pull (zot's GetManifest stamps
// lastPullTimestamp via MetaDB.UpdateStatsOnDownload), so polling with GET kept the control alive
// indefinitely. HEAD (CheckManifest) returns the same 200/404 and records nothing (zot v2.1.21).
func eventuallyGone(t *testing.T, repository, digest string, maxSeconds int) {
	t.Helper()

	for waited := 0; waited < maxSeconds; waited += 10 {
		if !manifestExistsByDigestWithoutPulling(t, repository, digest) {
			return
		}
		sleepInCluster(t, 10)
	}

	t.Fatalf("%s@%s survived %ds unpulled (polled with HEAD, which does not renew it), so this suite "+
		"cannot observe a manifest being collected and the assertion above is not evidence. Check "+
		"deleteUntagged and the repository glob against %q -- and keepUntagged: if configured, zot "+
		"keeps a manifest whose last tag expired forever, logging `untagged manifest statistics not "+
		"found`.\ntags now: %s%s",
		repository, digest, maxSeconds, repository, tagsList(t, repository), registryLogs(t))
}

// eventuallyBlobGone waits for a blob to leave a repository once no manifest there references it.
// Ask only after the manifest is gone, and only about a layer nothing else in the repository holds.
// A blob HEAD records nothing, so polling cannot keep it alive.
func eventuallyBlobGone(t *testing.T, repository, digest string, maxSeconds int) {
	t.Helper()

	for waited := 0; waited < maxSeconds; waited += 10 {
		if !blobExists(t, repository, digest) {
			return
		}
		sleepInCluster(t, 10)
	}

	t.Fatalf("%s's layer %s survived %ds after the only manifest referencing it was collected: the "+
		"registry reclaims manifests but keeps their bytes (ADR 0060).%s",
		repository, digest, maxSeconds, registryLogs(t))
}

func blobExists(t *testing.T, repository, digest string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "HEAD", fmt.Sprintf("/v2/%s/blobs/%s", repository, digest), "", ""),
		"200 OK")
}

// topLayer is the last layer of a single-platform manifest: the one its own Dockerfile added.
func topLayer(t *testing.T, repository, digest string) string {
	t.Helper()
	out := registryRequest(t, "GET", fmt.Sprintf("/v2/%s/manifests/%s", repository, digest), "", "")
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

// refreshBothFor pulls a tag AND a digest every two seconds for the given duration, in one
// in-cluster loop, so the test's own latency never falls between two pulls. The exec blocks for the
// whole duration; go test's timeout is the outer bound.
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

	if out, err := kubectl(t, "-n", buildNamespace, "exec", curlPod, "--",
		"sh", "-c", script); err != nil {
		t.Fatalf("refreshing %s: %v\n%s", repository, err, out)
	}
}

// refreshFor pulls a single reference.
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

// registryLogs returns what the registry says it decided, for every retention failure message.
func registryLogs(t *testing.T) string {
	t.Helper()
	// By label with --prefix, not statefulset/...: with read replicas the pod that collected may
	// not be the one a workload selector would pick.
	out, err := kubectl(t, "-n", operatorNamespace, "logs",
		"-l", "oci-composer.lhns.de/registry-role=serve", "--prefix", "--tail=120")
	if err != nil {
		return "\n\n(registry logs unavailable: " + err.Error() + ")"
	}
	return "\n\nregistry logs:\n" + out
}

// tagsList reports a repository's current tags, for assertions and failure messages. Listing tags
// names no manifest, so it is not a pull.
func tagsList(t *testing.T, repository string) string {
	t.Helper()
	out := registryRequest(t, "GET", "/v2/"+repository+"/tags/list", "", "")
	if i := strings.LastIndex(out, "{"); i >= 0 {
		return out[i:]
	}
	return strings.TrimSpace(out)
}

// pushTinyImage publishes a REAL image, by running a build, and returns its manifest digest.
//
// Not a hand-made minimal manifest: zot cannot derive image metadata from one ("image meta not
// found"), records no pulls for it, and the result looks exactly like "a pull does not renew
// recency".
func pushTinyImage(t *testing.T, repository string) string {
	t.Helper()
	return publishFixture(t, repository, "Dockerfile", applyBuildTo)
}

// pushUntaggedImage publishes a fixture with push.tags empty (digest-only). The controller still
// names it after its own digest (ADR 0060).
func pushUntaggedImage(t *testing.T, repository string) string {
	t.Helper()
	return pushUntaggedImageFrom(t, repository, "Dockerfile")
}

func pushUntaggedImageFrom(t *testing.T, repository, dockerfile string) string {
	t.Helper()
	return publishFixture(t, repository, dockerfile, applyBuildToUntagged)
}

// publishFixture runs one build into the repository and returns the manifest digest; `apply`
// decides the tags.
func publishFixture(t *testing.T, repository, dockerfile string,
	apply func(t *testing.T, name, dockerfile, repository string, extraSpec ...string),
) string {
	t.Helper()

	// The object is named after the last path segment.
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

	// Deleted at once, so the controller stops refreshing it: a live object would keep every
	// negative control in this file alive until its deadline.
	mustKubectl(t, "-n", buildNamespace, "delete", "imagebuild", name)
	return digest
}

// manifestExists fetches a manifest with GET -- a real pull, which renews recency. Use it only to
// assert that something SURVIVED; to wait for something to die, use
// manifestExistsByDigestWithoutPulling.
func manifestExists(t *testing.T, repository, ref string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "GET", fmt.Sprintf("/v2/%s/manifests/%s", repository, ref), "", ""),
		"200 OK")
}

// manifestExistsByDigestWithoutPulling asks with HEAD, which zot resolves like GET but records
// nothing against, for the negative controls: a control that renews its own subject never fires.
// A digest-only manifest appears in no listing, so there is nothing else to ask.
func manifestExistsByDigestWithoutPulling(t *testing.T, repository, digest string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "HEAD", fmt.Sprintf("/v2/%s/manifests/%s", repository, digest), "", ""),
		"200 OK")
}

// There is deliberately no deleteTag helper: in zot v2.1.21 removing a manifest's last tag deletes
// its statistics, after which the collector never evaluates it, so such a fixture is inert.
