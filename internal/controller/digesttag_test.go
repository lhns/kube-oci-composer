package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/attest"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// preADR0060 publishes obj, then puts registry and status back to how a 0.5.x controller left them:
// no digest tag on the artifact, on an older retained build, or on the attestations of either.
// Returns the digests involved: the artifact, the older build, and the two attestations.
func preADR0060(t *testing.T, r *ImageCompositionReconciler, obj *ociv1alpha1.ImageComposition, repo string) (art, older, artSBOM, olderSBOM string) {
	t.Helper()
	published := build(t, r, obj, "first publish")
	art = published.Digest
	older = pushUntagged(t, repo)
	artSBOM = attachUntagged(t, repo, art)
	olderSBOM = attachUntagged(t, repo, older)
	deleteTag(t, repo, recon.DigestTag(art))

	var latest ociv1alpha1.ImageComposition
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), &latest); err != nil {
		t.Fatalf("reading: %v", err)
	}
	latest.Status.Artifact = published.DeepCopy()
	latest.Status.Artifact.Tags = []string{repo + ":main"}
	latest.Status.InputHash = obj.Status.InputHash
	latest.Status.History = []ociv1alpha1.BuildRecord{
		{Digest: art, Tags: []string{"main"}},
		{Digest: older},
		{Digest: goneDigest},
	}
	if err := r.Status().Update(context.Background(), &latest); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	return art, older, artSBOM, olderSBOM
}

// requireBackfilled asserts every digest carries its own tag in the registry, and that status claims
// the artifact's and every history entry's.
func requireBackfilled(t *testing.T, r *ImageCompositionReconciler, obj *ociv1alpha1.ImageComposition, repo string, digests ...string) {
	t.Helper()
	for _, d := range digests {
		if got := resolveTag(t, repo, recon.DigestTag(d)); got != d {
			t.Errorf("%s has no tag of its own (resolves to %q)", d, got)
		}
	}
	got := reload(t, r, obj)
	if a := got.Status.Artifact; a == nil || !recon.HasDigestTag(a.Tags, a.Digest) {
		t.Errorf("status.artifact does not claim its digest tag: %+v", got.Status.Artifact)
	}
	for _, rec := range got.Status.History {
		switch {
		case rec.Digest == goneDigest:
			// Content 0.5.x already lost: marked, not retried forever, and no longer counted as
			// waiting -- or the upgrade procedure's step 2 never finishes.
			if rec.Lost == nil || recon.NeedsDigestTag(rec) {
				t.Errorf("history entry %s, which the registry no longer serves, is not marked Lost: %+v",
					rec.Digest, rec)
			}
		case !recon.HasDigestTag(rec.Tags, rec.Digest):
			t.Errorf("history entry %s does not claim its digest tag: %v", rec.Digest, rec.Tags)
		case rec.Lost != nil:
			t.Errorf("history entry %s is served, and was marked Lost", rec.Digest)
		}
	}
}

// goneDigest is a history entry whose content the registry no longer has.
const goneDigest = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"

// A composition published before ADR 0060 gains its digest's own tag -- on the artifact, on retained
// history (what a rollback pulls), and on the attestations of both, which were pushed untagged and
// are collected once keepUntagged is off -- and is not reassembled to get it.
func TestAConvergedCompositionIsBackfilledWithoutRepublishing(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("backfill", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)
	repo := host + "/default/backfill"
	art, older, artSBOM, olderSBOM := preADR0060(t, r, obj, repo)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireBackfilled(t, r, obj, repo, art, older, artSBOM, olderSBOM)
	if got := reload(t, r, obj); got.Status.Artifact.Digest != art || len(got.Status.History) != 3 {
		t.Errorf("the object was republished to add a tag (artifact %s, history %d); that reassembles "+
			"every layer of every composition on upgrade", got.Status.Artifact.Digest, len(got.Status.History))
	}
}

// Suspended objects are backfilled too. Their content is as exposed as anyone's, and an operator
// waiting for every object to carry the tag before dropping keepUntagged would wait forever.
func TestASuspendedCompositionIsStillBackfilled(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("suspended", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)
	repo := host + "/default/suspended"
	art, older, artSBOM, olderSBOM := preADR0060(t, r, obj, repo)

	suspend(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireBackfilled(t, r, obj, repo, art, older, artSBOM, olderSBOM)
	if c := readyOf(reload(t, r, obj)); c == nil || c.Reason != ociv1alpha1.ReasonSuspended {
		t.Errorf("the object is no longer reported suspended: %+v", c)
	}
}

// So are objects whose spec no longer reconciles: a stalled object keeps its published content, and
// that content needs its name as much as any other.
func TestAStalledCompositionIsStillBackfilled(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("stalled", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)
	repo := host + "/default/stalled"
	art, older, artSBOM, olderSBOM := preADR0060(t, r, obj, repo)

	// A digest mismatch is terminal: the spec, not a retry, has to change.
	var latest ociv1alpha1.ImageComposition
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), &latest); err != nil {
		t.Fatalf("reading: %v", err)
	}
	latest.Spec.Layers[0].Fetch.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := r.Update(context.Background(), &latest); err != nil {
		t.Fatalf("breaking the spec: %v", err)
	}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireBackfilled(t, r, obj, repo, art, older, artSBOM, olderSBOM)
	if c := readyOf(reload(t, r, obj)); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("the broken spec did not stall, so this test did not test a stalled object: %+v", c)
	}
}

// Once status claims the digest's own tag, it is checked with the rest: a lost one is restored by
// republishing, the same as a lost spec tag.
func TestALostDigestTagIsRepublished(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("restore", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)
	repo := host + "/default/restore"

	art := build(t, r, obj, "first")
	own := recon.DigestTag(art.Digest)
	if !recon.HasDigestTag(art.Tags, art.Digest) {
		t.Fatalf("a fresh publish does not claim its own tag: %v", art.Tags)
	}
	deleteTag(t, repo, own)

	build(t, r, obj, "after losing the tag")
	if got := resolveTag(t, repo, own); got != art.Digest {
		t.Fatalf("the digest's own tag was not restored (resolves to %q)", got)
	}
}

func suspend(t *testing.T, r *ImageCompositionReconciler, obj *ociv1alpha1.ImageComposition) {
	t.Helper()
	var latest ociv1alpha1.ImageComposition
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), &latest); err != nil {
		t.Fatalf("reading: %v", err)
	}
	latest.Spec.Suspend = true
	if err := r.Update(context.Background(), &latest); err != nil {
		t.Fatalf("suspending: %v", err)
	}
}

func readyOf(obj *ociv1alpha1.ImageComposition) *metav1.Condition {
	for i := range obj.Status.Conditions {
		if obj.Status.Conditions[i].Type == ociv1alpha1.ReadyCondition {
			return &obj.Status.Conditions[i]
		}
	}
	return nil
}

// attachUntagged attaches an SBOM to subject the way 0.5.x did: a referrer with no tag of its own.
func attachUntagged(t *testing.T, repo, subject string) string {
	t.Helper()
	r, err := name.NewRepository(repo, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s: %v", repo, err)
	}
	h, err := v1.NewHash(subject)
	if err != nil {
		t.Fatalf("parsing %s: %v", subject, err)
	}
	desc, err := remote.Head(r.Digest(subject))
	if err != nil {
		t.Fatalf("reading %s: %v", subject, err)
	}
	desc.Digest = h
	sbom, err := attest.Push(r, *desc, attest.PredicateSPDX, []byte(`{"spdxVersion":"SPDX-2.3"}`), false, nil)
	if err != nil {
		t.Fatalf("attaching an SBOM to %s: %v", subject, err)
	}
	deleteTag(t, repo, attest.OwnTag(sbom))
	return sbom.String()
}

func deleteTag(t *testing.T, repo, tag string) {
	t.Helper()
	ref, err := name.NewTag(repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s:%s: %v", repo, tag, err)
	}
	if err := remote.Delete(ref); err != nil {
		t.Fatalf("deleting %s: %v", ref, err)
	}
	if got := resolveTag(t, repo, tag); got != "" {
		t.Fatalf("%s still resolves after deleting it; this test cannot simulate a missing tag", ref)
	}
}

func resolveTag(t *testing.T, repo, tag string) string {
	t.Helper()
	ref, err := name.NewTag(repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s:%s: %v", repo, tag, err)
	}
	desc, err := remote.Head(ref)
	if err != nil {
		return ""
	}
	return desc.Digest.String()
}

func pushUntagged(t *testing.T, repo string) string {
	t.Helper()
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatalf("building an image: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digesting: %v", err)
	}
	ref, err := name.NewDigest(repo+"@"+d.String(), name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("pushing: %v", err)
	}
	return d.String()
}
