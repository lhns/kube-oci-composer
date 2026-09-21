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
			}
		})
	}
}

// TestADigestOnlyPublishNeverConflicts — the name IS the content, so nothing can be remeaned.
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
}
