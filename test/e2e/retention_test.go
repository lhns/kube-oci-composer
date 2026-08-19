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
// The registry is configured with a 20-second window and collection every 10 (see
// manifests/registry.yaml). A real deployment would use 30 days against an hourly refresh; the
// margin is what makes it a guarantee rather than a race.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

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

	// Long enough that an unrefreshed image is well past its window and collection has run several
	// times over. The refreshed one is pulled throughout.
	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		if !manifestExists(t, refreshed, "v1") {
			t.Fatalf("%s:v1 was collected while being pulled every few seconds; a pull does NOT "+
				"reset the retention clock, and the entire registry-backed retention design is "+
				"inert. Nothing built on top of this measurement means anything.", refreshed)
		}
		time.Sleep(5 * time.Second)
	}

	// The negative control. Without this the test above proves only that zot deletes nothing.
	if manifestExists(t, abandoned, "v1") {
		t.Fatalf("%s:v1 survived %v with no pulls and a 20s window, so this suite cannot observe a "+
			"deletion at all. Every retention assertion here is vacuous until that is fixed — "+
			"check gcInterval, the retention policy and the repository pattern.",
			abandoned, 75*time.Second)
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

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if !manifestExistsByDigest(t, repo, digest) {
			t.Fatalf("an untagged manifest was collected while being pulled by digest. ADR 0010 "+
				"tells users to reference digests, so this would delete content a rescheduled pod "+
				"re-pulls. Set deleteUntagged: false. (%s@%s)", repo, digest)
		}
		time.Sleep(5 * time.Second)
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

// manifestExists fetches a manifest, and the choice of GET over HEAD is a MEASURED RESULT rather
// than a style preference.
//
// The first run of this file used HEAD here and GET in manifestExistsByDigest. The HEAD case was
// collected while being polled every five seconds; the GET case survived. A HEAD does not renew
// pull recency in zot — reasonably, since an existence check is not a pull.
//
// That is exactly the kind of fact this file exists to establish, and it constrains the refresh
// implementation directly: a refresh that HEADs is a refresh that does nothing, and the symptom
// arrives one retention window later as missing images.
func manifestExists(t *testing.T, repository, tag string) bool {
	t.Helper()
	return strings.Contains(
		registryRequest(t, "get-"+shortName(repository, tag), "GET",
			fmt.Sprintf("/v2/%s/manifests/%s", repository, tag), "", ""),
		"200 OK")
}

func manifestExistsByDigest(t *testing.T, repository, digest string) bool {
	t.Helper()
	// GET rather than HEAD, for the reason manifestExists now documents from measurement.
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

// contentDigest deliberately uses HEAD, which the measurement above shows does NOT renew recency.
// That matters: this is called on the abandoned repository too, and a call that refreshed would
// quietly destroy the negative control and make the whole file vacuous.
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
