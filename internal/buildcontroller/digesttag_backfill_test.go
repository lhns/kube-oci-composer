package buildcontroller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestAConvergedBuildGainsItsDigestsOwnTagWithoutRebuilding: an ImageBuild published before ADR
// 0060 gains its digest's own tag (artifact and history) on the converged path, without a rebuild,
// which would move every digest in the cluster on upgrade.
func TestAConvergedBuildGainsItsDigestsOwnTagWithoutRebuilding(t *testing.T) {
	host := startRegistry(t)
	repo := host + "/team/app"

	_, current := pushByDigest(t, repo)
	tagAs(t, repo, "v1", current)
	_, older := pushByDigest(t, repo)
	// Content already gone: must neither fail the pass nor be marked as tagged.
	const gone = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"

	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push = &ociv1alpha1.Push{Repository: repo, Tags: []string{"v1"}}
		// What an object published before the digest tag existed looks like.
		b.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: current, Tags: []string{repo + ":v1"}}
		b.Status.History = []ociv1alpha1.BuildRecord{
			{Digest: current, Tags: []string{repo + ":v1"}},
			{Digest: older},
			{Digest: gone},
		}
	})
	r := harness(t, pinnedFrom, obj)
	r.JobConfig.InsecureRegistries = []string{host}
	r.Recorder = record.NewFakeRecorder(20)

	inputs, _, err := r.resolveInputs(context.Background(), obj)
	if err != nil {
		t.Fatalf("resolving inputs: %v", err)
	}
	obj.Status.InputHash = inputs.Hash()
	if err := r.Status().Update(context.Background(), obj); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	for pass := 1; pass <= 2; pass++ {
		if _, err := reconcileOnce(t, r, obj); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(obj.Namespace)); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("got %d build Jobs: an object whose only change is a missing digest tag was "+
			"rebuilt, which on upgrade moves the digest of every ImageBuild in the cluster", len(jobs.Items))
	}

	for _, d := range []string{current, older} {
		if got := tagResolvesTo(t, repo, recon.DigestTag(d)); got != d {
			t.Errorf("%s was not given its own tag (resolves to %q)", d, got)
		}
	}

	got := reload(t, r, obj)
	if !recon.HasDigestTag(got.Status.Artifact.Tags, current) {
		t.Errorf("status.artifact.tags %v does not record the digest's own tag, so every pass "+
			"will apply it again", got.Status.Artifact.Tags)
	}
	claimed := map[string]bool{}
	for _, rec := range got.Status.History {
		claimed[rec.Digest] = recon.HasDigestTag(rec.Tags, rec.Digest)
	}
	if !claimed[current] || !claimed[older] {
		t.Errorf("history does not record the tags it was given: %+v", got.Status.History)
	}
	if claimed[gone] {
		t.Error("a record whose content is gone claims a tag that was never applied")
	}
	// Applied once, not once per pass.
	var own int
	for _, tag := range got.Status.Artifact.Tags {
		if tag == repo+":"+recon.DigestTag(current) {
			own++
		}
	}
	if own != 1 {
		t.Errorf("the digest's own tag appears %d times in %v", own, got.Status.Artifact.Tags)
	}
}

// TestALostDigestTagIsALoss: once status records the digest's own tag, stillPublished checks it
// like any other tag.
func TestALostDigestTagIsALoss(t *testing.T) {
	srv := registryAnswering(t, 404, digestOfNothing, "v1")
	host := srv.URL[len("http://"):]

	obj := builtAndPublished(host, digestOfNothing)
	obj.Status.Artifact.Tags = []string{
		host + "/team/app:v1",
		host + "/team/app:" + recon.DigestTag(digestOfNothing),
	}

	r := reconcilerFor(t, srv)
	if r.stillPublished(context.Background(), obj) {
		t.Error("status claims the digest's own tag, the registry does not have it, and that was " +
			"called healthy")
	}
}

// preADR0060Build is an ImageBuild as a 0.5.x controller left it: published, with neither the
// artifact nor an older retained build carrying its own tag.
func preADR0060Build(t *testing.T, mutate func(*ociv1alpha1.ImageBuild)) (r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild, repo, current, older string) {
	t.Helper()
	host := startRegistry(t)
	repo = host + "/team/app"
	_, current = pushByDigest(t, repo)
	tagAs(t, repo, "v1", current)
	_, older = pushByDigest(t, repo)

	obj = buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push = &ociv1alpha1.Push{Repository: repo, Tags: []string{"v1"}}
		mutate(b)
		b.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: current, Tags: []string{repo + ":v1"}}
		b.Status.History = []ociv1alpha1.BuildRecord{{Digest: current, Tags: []string{repo + ":v1"}}, {Digest: older}}
	})
	r = harness(t, pinnedFrom, obj)
	r.JobConfig.InsecureRegistries = []string{host}
	r.Recorder = record.NewFakeRecorder(20)
	return r, obj, repo, current, older
}

func requireBuildBackfilled(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild, repo string, digests ...string) *ociv1alpha1.ImageBuild {
	t.Helper()
	for _, d := range digests {
		if got := tagResolvesTo(t, repo, recon.DigestTag(d)); got != d {
			t.Errorf("%s has no tag of its own (resolves to %q)", d, got)
		}
	}
	got := reload(t, r, obj)
	if a := got.Status.Artifact; a == nil || !recon.HasDigestTag(a.Tags, a.Digest) {
		t.Errorf("status.artifact does not claim its digest tag: %+v", got.Status.Artifact)
	}
	for _, rec := range got.Status.History {
		if !recon.HasDigestTag(rec.Tags, rec.Digest) {
			t.Errorf("history entry %s does not claim its digest tag: %v", rec.Digest, rec.Tags)
		}
	}
	return got
}

// Suspended builds are backfilled too: their content is as exposed as anyone's, and the upgrade
// procedure waits for every object to carry the tag.
func TestASuspendedBuildIsStillBackfilled(t *testing.T) {
	r, obj, repo, current, older := preADR0060Build(t, func(b *ociv1alpha1.ImageBuild) { b.Spec.Suspend = true })
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := requireBuildBackfilled(t, r, obj, repo, current, older)
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil || c.Reason != ociv1alpha1.ReasonSuspended {
		t.Errorf("the object is no longer reported suspended: %+v", c)
	}
}

// So are builds whose spec no longer reconciles: a stalled object still has published content.
func TestAStalledBuildIsStillBackfilled(t *testing.T) {
	// An invalid ref is terminal, and leaves the repository to publish to intact.
	r, obj, repo, current, older := preADR0060Build(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.Ref = "registry.example/app:not a tag"
	})
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := requireBuildBackfilled(t, r, obj, repo, current, older)
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil || c.Reason != ociv1alpha1.ReasonInvalidSpec {
		t.Errorf("the broken spec did not stall, so this test did not test a stalled object: %+v", c)
	}
}
