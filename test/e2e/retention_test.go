//go:build e2e

// Does a refresh actually keep an image alive?
//
// This file exists before any retention code does, and deliberately so. The registry-backed design
// rests on one assumption — that touching an image resets the clock the registry expires it by — and
// if that assumption is false then every later test passes vacuously, because a registry that never
// deletes anything satisfies "live images are not deleted" trivially.
//
// So each test here has a NEGATIVE CONTROL: something that is not refreshed, asserted to actually
// disappear. Without it, "the image is still there" is not evidence of anything.
//
// The registry is configured with a short window and frequent collection (see
// manifests/registry.yaml). A real deployment would use 30 days against an hourly refresh; the
// RATIO between the two is what makes it a guarantee rather than a race, and the ratio is what this
// file reproduces in miniature.
package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// retentionWindow mirrors pulledWithin in manifests/registry.yaml, for the failure messages. It is
// not read by the registry; the two are kept in step by hand and the negative control is what
// catches them drifting apart.
const retentionWindow = "30s"

// keepaliveRepo scopes these tests to the repository prefix the retention policy applies to, so
// nothing else in the suite can be collected out from under it.
func keepaliveRepo(name string) string { return "keepalive-" + name }

// The load-bearing measurement: a PULL resets the retention clock.
//
// This is the fact the whole design turns on, and it is what makes the guarantee affordable. If
// staying alive requires a PUSH, the controller needs a write credential to every repository it
// keeps alive — which is exactly what must not exist ("nothing should be able to push into the
// composer registry"). A pull needs no credential at all, moves no blobs, and cannot corrupt what
// it is protecting.
//
// zot's own vocabulary for this is `pulledWithin`. That it is documented is not evidence; this is.
func TestPullingAnImageKeepsItFromExpiring(t *testing.T) {
	refreshed := keepaliveRepo("refreshed")
	abandoned := keepaliveRepo("abandoned")

	pushTinyImage(t, refreshed, "v1")
	pushTinyImage(t, abandoned, "v1")

	// Refreshed from INSIDE the cluster, in one shell loop, rather than by polling from the test.
	//
	// This is not a tidiness preference. Each kubectl exec costs seconds on a loaded runner, so a
	// loop that sleeps 5s between requests can easily leave 20s between pulls -- longer than the
	// retention window it is trying to stay inside. The measurement then reports that pulls do not
	// renew recency, when what it actually measured was kubectl.
	//
	// One exec, a tight loop, and the assertions afterwards keeps the harness out of the result.
	refreshFor(t, refreshed, "v1", 90)

	if !manifestExists(t, refreshed, "v1") {
		t.Fatalf("%s:v1 was collected while being pulled every two seconds for 90s against a %s "+
			"window; a pull does NOT renew recency, and the registry-backed retention design is "+
			"inert. Nothing built on top of this measurement means anything.",
			refreshed, retentionWindow)
	}

	// The negative control. Without it the assertion above proves only that zot deletes nothing.
	if manifestExists(t, abandoned, "v1") {
		t.Fatalf("%s:v1 survived 90s with no pulls against a %s window, so this suite cannot "+
			"observe a deletion at all and every retention assertion here is vacuous. Check "+
			"gcInterval, the keepTags patterns, and the repository glob.", abandoned, retentionWindow)
	}
}

// refreshFor pulls a manifest every two seconds for the given number of seconds, in-cluster.
//
// The exec blocks for the whole duration, which is the point: the loop IS the refresh, and nothing
// about the test's own latency can get between two pulls.
func refreshFor(t *testing.T, repository, tag string, seconds int) {
	t.Helper()
	ensureCurlPod(t)

	url := "http://" + buildRegistry + "/v2/" + repository + "/manifests/" + tag
	script := fmt.Sprintf(
		"i=0; while [ $i -lt %d ]; do curl -sS -o /dev/null "+
			"-H 'Accept: application/vnd.oci.image.manifest.v1+json' %s; sleep 2; i=$((i+2)); done",
		seconds, url)

	// No timeout wrapper: the exec is expected to block for the whole duration, and go test's own
	// timeout is the outer bound.
	if out, err := kubectl(t, "-n", buildNamespace, "exec", curlPod, "--", "sh", "-c", script); err != nil {
		t.Fatalf("refreshing %s:%s: %v\n%s", repository, tag, err, out)
	}
}

// Untagged is not unreferenced. ADR 0010 makes referencing images BY DIGEST the recommended usage,
// so a manifest with no tag may be what a running workload pulls. Retention has to keep those alive
// on the same terms, which zot spells `keepUntagged`.
//
// If this fails, the correct configuration is `deleteUntagged: false` — leaking untagged bytes is
// acceptable, deleting content a rescheduled pod re-pulls is not.
func TestPullingByDigestKeepsAnUntaggedImageAlive(t *testing.T) {
	repo := keepaliveRepo("untagged")
	digest := pushTinyImage(t, repo, "temporary")

	// Remove the tag, leaving the manifest reachable only by digest.
	deleteTag(t, repo, "temporary")

	refreshFor(t, repo, digest, 90)

	if !manifestExistsByDigest(t, repo, digest) {
		t.Fatalf("an untagged manifest was collected while being pulled by digest. ADR 0010 tells "+
			"users to reference digests, so this deletes content a rescheduled pod re-pulls. Set "+
			"deleteUntagged: false. (%s@%s)", repo, digest)
	}
}

// pushTinyImage publishes a minimal image and returns its manifest digest.
//
// Built by hand rather than by running a build, because what is under test is the registry's
// retention behaviour and a real build would add several minutes and a dependency on BuildKit to
// a question that has nothing to do with either.
func pushTinyImage(t *testing.T, repository, tag string) string {
	t.Helper()

	// An empty config blob is a valid image config as far as the distribution spec is concerned,
	// and a manifest with no layers is a valid manifest. That is all retention operates on.
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
// was that HEAD does not count as a pull. That reading was wrong — the cause was a retention policy
// that matched no tags at all (see manifests/registry.yaml) — and it is recorded here because it is
// the kind of plausible, tidy explanation that is worth being suspicious of.
//
// GET stays regardless: it is unambiguously a pull, the difference in cost is a few KB, and the
// refresh has no reason to economise on the one request that the whole guarantee depends on.
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
// immediately after a push -- never during a polling loop -- so it cannot contaminate the negative
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
