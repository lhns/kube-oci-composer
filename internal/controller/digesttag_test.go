package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// A composition published before ADR 0060 must gain its digest's own tag on the cheap path --
// without being reassembled to get it.
//
// Reassembly is the cost being avoided: it downloads every layer. The cheap path only checks the
// digest's own tag once status claims it, so an object that predates the tag converges on its
// spec's tags as before, and the tag is applied to what is already there.
func TestAConvergedCompositionGainsItsDigestsOwnTagWithoutRepublishing(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("backfill", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)
	repo := host + "/default/backfill"

	art := build(t, r, obj, "first")

	// Turn it into what an object published before the tag existed looks like: no digest tag in
	// the registry, and none claimed in status.
	own := recon.DigestTag(art.Digest)
	deleteTag(t, repo, own)
	obj.Status.Artifact.Tags = []string{repo + ":main"}
	obj.Status.History[0].Tags = []string{"main"}

	// And an older retained build, published before it too.
	older := pushUntagged(t, repo)
	obj.Status.History = append(obj.Status.History, ociv1alpha1.BuildRecord{Digest: older})

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res.Record != nil {
		t.Fatal("the object was republished to add a tag; that is a reassembly of every layer, " +
			"for every composition, on upgrade")
	}
	if !recon.HasDigestTag(res.Artifact.Tags, art.Digest) {
		t.Errorf("status.artifact.tags %v does not record the digest's own tag", res.Artifact.Tags)
	}
	if !slices.Contains(res.DigestTagged, art.Digest) || !slices.Contains(res.DigestTagged, older) {
		t.Errorf("DigestTagged = %v, want both retained builds", res.DigestTagged)
	}
	for _, d := range []string{art.Digest, older} {
		if got := resolveTag(t, repo, recon.DigestTag(d)); got != d {
			t.Errorf("%s was not given its own tag (resolves to %q)", d, got)
		}
	}

	// What Reconcile does with it, and then the next pass asks nothing further.
	obj.Status.Artifact = res.Artifact
	markDigestTagged(obj.Status.History, res.DigestTagged)
	for _, rec := range obj.Status.History {
		if !recon.HasDigestTag(rec.Tags, rec.Digest) {
			t.Errorf("history record %s was not marked", rec.Digest)
		}
	}
	again, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if again.Record != nil || len(again.DigestTagged) != 0 {
		t.Errorf("a fully tagged object did work again: record=%v tagged=%v",
			again.Record != nil, again.DigestTagged)
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
