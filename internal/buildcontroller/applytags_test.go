package buildcontroller

import (
	"context"
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestOnConflictIsExactOnceTheDigestExists is what this change is for.
//
// The Job uploads by digest and names nothing, so the controller holds the NEW digest when it
// decides whether a tag may take it. Before, the check ran ahead of the build against
// status.artifact.digest -- a stand-in for a value that did not yet exist -- and the substitution
// had a one-directional hole: a tag holding this object's OWN previous digest was exempt, so an
// object remeaning its own tag was never a conflict. ADR 0054.
func TestOnConflictIsExactOnceTheDigestExists(t *testing.T) {
	for _, tc := range []struct {
		name string
		// holder decides what the tag points at before the build: nothing, this object's previous
		// digest, or somebody else's content.
		holder   string
		policy   ociv1alpha1.TagConflictPolicy
		wantErr  bool
		wantKeep bool
		wantTag  bool
	}{
		{name: "free tag is taken", holder: "none", wantTag: true},
		// The case the old check could not see. Same object, same tag, new content.
		{name: "own previous digest, Fail", holder: "ours", policy: ociv1alpha1.ConflictFail, wantErr: true},
		{name: "own previous digest, Overwrite", holder: "ours", policy: ociv1alpha1.ConflictOverwrite, wantTag: true},
		{name: "own previous digest, Keep", holder: "ours", policy: ociv1alpha1.ConflictKeep, wantKeep: true},
		{name: "another digest, Fail", holder: "other", policy: ociv1alpha1.ConflictFail, wantErr: true},
		{name: "another digest, Keep", holder: "other", policy: ociv1alpha1.ConflictKeep, wantKeep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := startRegistry(t)
			repo := host + "/team-a/app"

			// What the build produced.
			_, built := pushByDigest(t, repo)

			obj := sampleBuild()
			obj.Spec.Push = &ociv1alpha1.Push{
				Repository: repo, Tags: []string{"v1"}, OnConflict: tc.policy,
			}

			switch tc.holder {
			case "ours":
				_, prev := pushByDigest(t, repo)
				tagAs(t, repo, "v1", prev)
				obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: prev}
			case "other":
				_, other := pushByDigest(t, repo)
				tagAs(t, repo, "v1", other)
			}
			before := tagResolvesTo(t, repo, "v1")

			cfg := sampleConfig()
			cfg.InsecureRegistries = []string{host}
			r := &ImageBuildReconciler{JobConfig: cfg}

			conflict, err := r.applyTags(context.Background(), obj, built)

			switch {
			case tc.wantErr:
				if err == nil {
					t.Fatal("onConflict: Fail accepted a tag taking on new content")
				}
				if !recon.IsTerminal(err) {
					t.Errorf("must be terminal -- the spec is what fixes it; got %v", err)
				}
				if now := tagResolvesTo(t, repo, "v1"); now != before {
					t.Errorf("the tag moved despite Fail: %s -> %s", before, now)
				}
				// Refused content must stay untagged, so the registry reclaims it. Naming it after
				// its own digest would make it permanent -- the leak ADR 0060 exists to close.
				if own := tagResolvesTo(t, repo, recon.DigestTag(built)); own != "" {
					t.Errorf("Fail gave the refused digest its own tag, so nothing will reclaim it")
				}
			case tc.wantKeep:
				if err != nil {
					t.Fatalf("Keep returned an error: %v", err)
				}
				if conflict == nil {
					t.Fatal("Keep recorded no conflict, so the divergence is invisible")
				}
				// The improvement ADR 0029 had to forgo: a REAL dropped digest, because the
				// content existed before the decision.
				if conflict.Dropped != built {
					t.Errorf("dropped = %q, want the digest this build produced (%s)",
						conflict.Dropped, built)
				}
				if now := tagResolvesTo(t, repo, "v1"); now != before {
					t.Errorf("Keep moved the tag: %s -> %s", before, now)
				}
				if own := tagResolvesTo(t, repo, recon.DigestTag(built)); own != "" {
					t.Errorf("Keep gave the dropped digest its own tag, so nothing will reclaim it")
				}
			case tc.wantTag:
				if err != nil {
					t.Fatalf("tagging: %v", err)
				}
				if conflict != nil {
					t.Errorf("unexpected conflict: %+v", conflict)
				}
				if now := tagResolvesTo(t, repo, "v1"); now != built {
					t.Errorf("tag resolves to %q, want the built digest %s", now, built)
				}
				if own := tagResolvesTo(t, repo, recon.DigestTag(built)); own != built {
					t.Errorf("the digest's own tag resolves to %q, want %s (ADR 0060)", own, built)
				}
			}
		})
	}
}

// TestADigestOnlyPublishNeverConflicts — the name IS the content, so nothing can be remeaned.
//
// And it is still named after its own digest (ADR 0060). Left untagged, it is exactly what a
// registry's collector reclaims by age, whoever is pulling it by digest.
func TestADigestOnlyPublishNeverConflicts(t *testing.T) {
	host := startRegistry(t)
	repo := host + "/team-a/app"
	_, built := pushByDigest(t, repo)

	obj := sampleBuild()
	obj.Spec.Push = &ociv1alpha1.Push{Repository: repo, OnConflict: ociv1alpha1.ConflictFail}

	cfg := sampleConfig()
	cfg.InsecureRegistries = []string{host}
	r := &ImageBuildReconciler{JobConfig: cfg}

	conflict, err := r.applyTags(context.Background(), obj, built)
	if err != nil || conflict != nil {
		t.Fatalf("a digest-only publish reported a conflict: conflict=%+v err=%v", conflict, err)
	}
	if own := tagResolvesTo(t, repo, recon.DigestTag(built)); own != built {
		t.Fatalf("a digest-only publish was left untagged (own tag resolves to %q); a collector "+
			"deleting untagged content by age would take it from under a digest-pinned workload", own)
	}
}

// TestTheDigestsOwnTagSurvivesARollingTagMoving is the property ADR 0060 is for, one level down.
//
// This registry keeps a manifest addressable when its only tag moves, so it cannot reproduce the
// zot behaviour (zot#4444) -- test/e2e/retention_tagmove_test.go does, against the real thing.
// What it can pin is the mechanism: after the rolling tag moves, the previous build still has a
// name of its own, which is what keeps zot from dropping it out of the index.
func TestTheDigestsOwnTagSurvivesARollingTagMoving(t *testing.T) {
	host := startRegistry(t)
	repo := host + "/team-a/app"

	cfg := sampleConfig()
	cfg.InsecureRegistries = []string{host}
	r := &ImageBuildReconciler{JobConfig: cfg}

	obj := sampleBuild()
	obj.Spec.Push = &ociv1alpha1.Push{
		Repository: repo, Tags: []string{"main"}, OnConflict: ociv1alpha1.ConflictOverwrite,
	}

	_, first := pushByDigest(t, repo)
	if _, err := r.applyTags(context.Background(), obj, first); err != nil {
		t.Fatalf("first build: %v", err)
	}
	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: first}

	_, second := pushByDigest(t, repo)
	if first == second {
		t.Fatal("the two builds share a digest, so the tag never moved and this proves nothing")
	}
	if _, err := r.applyTags(context.Background(), obj, second); err != nil {
		t.Fatalf("second build: %v", err)
	}

	if now := tagResolvesTo(t, repo, "main"); now != second {
		t.Fatalf("the rolling tag did not move (resolves to %q); this test is not testing a move", now)
	}
	if own := tagResolvesTo(t, repo, recon.DigestTag(first)); own != first {
		t.Fatalf("after the rolling tag moved, the previous build has no name of its own (%q) -- "+
			"zot drops a manifest whose last tag moves, under anything pinned to it", own)
	}
}
