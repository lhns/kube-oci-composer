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

// An ImageBuild published before ADR 0060 must gain its digest's own tag on the converged path --
// and must NOT be rebuilt to get it.
//
// The rebuild is the trap. This kind is not reproducible, so a rebuild moves the digest, and every
// ImageBuild in the cluster would move at once on upgrade if the absent tag read as a loss. So the
// tag is added to what is already there, and status records it so the next pass asks nothing.
//
// History too: a retained record is what a rollback pulls, and what a rolling tag used to delete.
func TestAConvergedBuildGainsItsDigestsOwnTagWithoutRebuilding(t *testing.T) {
	host := startRegistry(t)
	repo := host + "/team/app"

	_, current := pushByDigest(t, repo)
	tagAs(t, repo, "v1", current)
	_, older := pushByDigest(t, repo)
	// A record whose content is already gone -- expired, or reclaimed. It must neither fail the
	// pass nor be marked as tagged.
	const gone = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"

	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push = &ociv1alpha1.Push{Repository: repo, Tags: []string{"v1"}}
		// Exactly what an object published before the digest tag existed looks like.
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
	// Once, not once per pass: the second pass found the artifact's tag recorded and asked nothing
	// about it, so status did not grow a duplicate.
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

// Once status claims the digest's own tag, losing it is a loss like any other tag's.
//
// The converse of the test above, and the reason the claim is recorded at all: stillPublished only
// checks the tag once status says it was applied, so this is the path where it IS checked.
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
