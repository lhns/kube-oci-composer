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
// The registry is configured with a short window and frequent collection (see
// manifests/registry.yaml). A real deployment would use 30 days against an hourly refresh; the RATIO
// between the two is what makes it a guarantee rather than a race, and the ratio is what this file
// reproduces in miniature.
package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// retentionWindow mirrors pulledWithin in manifests/registry.yaml, for the failure messages. It is
// not read by the registry; the two are kept in step by hand, and the negative control is what
// catches them drifting apart.
const retentionWindow = "30s"

// keepaliveRepo scopes these tests to the repository prefix the retention policy applies to, so
// nothing else in the suite can be collected out from under it.
func keepaliveRepo(name string) string { return "keepalive-" + name }

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
	refreshed := keepaliveRepo("refreshed")
	abandoned := keepaliveRepo("abandoned")

	keptDigest := pushTinyImage(t, refreshed, "v1")
	pushTinyImage(t, abandoned, "v1")

	// Refreshed from INSIDE the cluster, in one shell loop, rather than by polling from the test.
	// Each kubectl exec costs seconds on a loaded runner, so a loop that sleeps between requests can
	// leave more time between pulls than the window it is trying to stay inside; the measurement
	// then reports on kubectl rather than on zot, and an earlier version of this file did exactly
	// that.
	//
	// BOTH references are pulled, and that is a measured requirement rather than belt-and-braces.
	// Pulling only the digest kept the content alive and let the TAG be collected: zot governs the
	// two by different rules (keepUntagged versus keepTags), so a refresh keeps alive precisely what
	// it asks for and nothing else. This is the shape the real refresh has to have.
	refreshBothFor(t, refreshed, "v1", keptDigest, 90)

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
	// unrefreshed tag is what this registry demonstrably collects inside a test's runtime.
	// Unrefreshed untagged CONTENT was measured surviving well past the window, so asserting on that
	// instead would fail for a reason that has nothing to do with the guarantee — see
	// TestUnrefreshedContentIsNotPromptlyReclaimed, which records that separately rather than
	// letting it quietly weaken this one.
	if manifestExists(t, abandoned, "v1") {
		t.Fatalf("%s:v1 survived 90s with no pulls against a %s window, so this suite cannot "+
			"observe a deletion at all and every retention assertion here is vacuous. Check "+
			"gcInterval, the keepTags patterns, and the repository glob.\ntags now: %s%s",
			abandoned, retentionWindow, tagsList(t, abandoned), registryLogs(t))
	}
}

// What is NOT true, recorded so that nobody builds on the assumption that it is.
//
// Untagged content is not reclaimed promptly once its window elapses: measured surviving 90s against
// a 30s window with no pulls at all, while tags in the same registry were collected in the same
// period. Whatever governs it is slower and less predictable than the tag-level policy.
//
// That is acceptable, for a stated reason rather than by shrugging. The guarantee is that live
// content survives, and expiry beyond it is explicitly best-effort (ADR 0031): leaking bytes is
// acceptable, losing live content is not. What would NOT be acceptable is quietly assuming storage
// is bounded by this policy when the measurement says it is not.
//
// Reported rather than asserted, because pinning "does not expire promptly" down as an expectation
// would make an improvement in zot show up here as a regression.
func TestUnrefreshedContentIsNotPromptlyReclaimed(t *testing.T) {
	repo := keepaliveRepo("cold")
	digest := pushTinyImage(t, repo, "v1")

	sleepInCluster(t, 90)

	t.Logf("after 90s with no pulls against a %s window: content alive=%v, tags now: %s",
		retentionWindow, manifestExistsByDigest(t, repo, digest), tagsList(t, repo))
}

// Untagged is not unreferenced. ADR 0010 makes referencing images BY DIGEST the recommended usage,
// so a manifest with no tag may be what a running workload pulls, and retention has to keep those
// alive on the same terms — which zot spells `keepUntagged`.
//
// Distinct from the guarantee test above, where the manifest keeps its tag throughout: here the tag
// is removed first, so the manifest is protected by `keepUntagged` alone.
func TestPullingByDigestKeepsAnUntaggedImageAlive(t *testing.T) {
	repo := keepaliveRepo("untagged")
	digest := pushTinyImage(t, repo, "temporary")

	deleteTag(t, repo, "temporary")
	refreshFor(t, repo, digest, 90)

	if !manifestExistsByDigest(t, repo, digest) {
		t.Fatalf("an untagged manifest was collected while being pulled by digest. ADR 0010 tells "+
			"users to reference digests, so this deletes content a rescheduled pod re-pulls. Set "+
			"deleteUntagged: false. (%s@%s)", repo, digest)
	}
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
	out, err := kubectl(t, "-n", buildNamespace, "logs", "deploy/e2e-registry", "--tail=120")
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

// pushTinyImage publishes a minimal image and returns its manifest digest.
//
// Built by hand rather than by running a build, because what is under test is the registry's
// retention behaviour and a real build would add several minutes and a dependency on BuildKit to a
// question that has nothing to do with either.
func pushTinyImage(t *testing.T, repository, tag string) string {
	t.Helper()

	// An empty config blob is a valid image config as far as the distribution spec is concerned, and
	// a manifest with no layers is a valid manifest. That is all retention operates on.
	const empty = "{}"
	configDigest := putBlob(t, repository, empty)

	manifest := fmt.Sprintf(`{"schemaVersion":2,`+
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":%d,"digest":"%s"},`+
		`"layers":[],"annotations":{"e2e":"%s"}}`, len(empty), configDigest, repository)

	out := registryRequest(t, "push-"+shortName(repository, tag), "PUT",
		fmt.Sprintf("/v2/%s/manifests/%s", repository, tag), manifest,
		"Content-Type: application/vnd.oci.image.manifest.v1+json")
	if !strings.Contains(out, "201") && !strings.Contains(out, "Created") {
		t.Fatalf("pushing %s:%s did not succeed:\n%s", repository, tag, out)
	}
	return contentDigest(t, repository, tag)
}

// putBlob uploads a blob and returns its digest.
func putBlob(t *testing.T, repository, body string) string {
	t.Helper()
	digest := sha256Of(t, body)
	out := registryRequest(t, "blob-"+shortName(repository, "cfg"), "POST",
		fmt.Sprintf("/v2/%s/blobs/uploads/?digest=%s", repository, digest), body,
		"Content-Type: application/octet-stream")
	if !strings.Contains(out, "201") && !strings.Contains(out, "Created") {
		t.Fatalf("uploading a config blob to %s failed:\n%s", repository, out)
	}
	return digest
}

// manifestExists fetches a manifest with GET, which is what a real pull is.
//
// Whether a HEAD would also renew recency is UNMEASURED and deliberately left that way. An early
// version of this file used HEAD here, and when the tagged case was collected the obvious reading
// was that HEAD does not count as a pull. That reading was wrong, and so was the next one; the
// causes were a retention policy that matched no tags and then a refresh that pulled the digest but
// not the tag. Both are recorded because they are the kind of plausible, tidy explanation worth
// being suspicious of.
//
// GET stays regardless: it is unambiguously a pull, the difference in cost is a few KB, and the
// refresh has no reason to economise on the one request the whole guarantee depends on.
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

func deleteTag(t *testing.T, repository, tag string) {
	t.Helper()
	registryRequest(t, "untag-"+shortName(repository, tag), "DELETE",
		fmt.Sprintf("/v2/%s/manifests/%s", repository, tag), "", "")
}

// contentDigest uses HEAD. Since it is unknown whether that renews recency, it is called only
// immediately after a push — never during a waiting period — so it cannot contaminate a negative
// control whichever way the answer falls.
func contentDigest(t *testing.T, repository, tag string) string {
	t.Helper()
	out := registryRequest(t, "digest-"+shortName(repository, tag), "HEAD",
		fmt.Sprintf("/v2/%s/manifests/%s", repository, tag), "", "")
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(k), "Docker-Content-Digest") {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no digest for %s:%s\n%s", repository, tag, out)
	return ""
}
