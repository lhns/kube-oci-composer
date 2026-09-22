package buildcontroller

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// signBuild signs a build's output after the Job has terminated. SBOM and provenance are already in
// the pushed index from BuildKit; only the signature is added here, because the key never enters a
// build pod.
//
// Failure is not fatal: the image is pushed and pullable. It surfaces as a Warning Event.
func (r *ImageBuildReconciler) signBuild(ctx context.Context, obj *ociv1alpha1.ImageBuild, digest string) *ociv1alpha1.AttestationStatus {
	if r.Attestor == nil || r.Attestor.Key == nil {
		return nil
	}
	// Already signed for this digest, judging by status alone.
	if prev := obj.Status.Attestations; prev != nil && prev.Subject == digest && prev.Signature != "" {
		return prev
	}

	repoName := r.repositoryFor(obj)
	var refOpts []name.Option
	if recon.InsecureHost(repoName, r.JobConfig.InsecureRegistries) {
		refOpts = append(refOpts, name.Insecure)
	}
	repo, err := name.NewRepository(repoName, refOpts...)
	if err != nil {
		r.noteAttestationFailure(obj, fmt.Errorf("parsing %s: %w", repoName, err))
		return nil
	}
	h, err := v1.NewHash(digest)
	if err != nil {
		r.noteAttestationFailure(obj, fmt.Errorf("parsing the digest: %w", err))
		return nil
	}

	opts, err := r.remoteOptions(ctx, obj)
	if err != nil {
		r.noteAttestationFailure(obj, err)
		return nil
	}

	signed, existing, err := r.Attestor.Key.VerifiedSignatureExists(ctx, repo, h, opts)
	if err != nil {
		r.noteAttestationFailure(obj, err)
		return nil
	}
	if signed {
		return &ociv1alpha1.AttestationStatus{Subject: digest, Signature: existing.String()}
	}

	sig, err := r.Attestor.Key.SignArtifact(ctx, repo, h, opts)
	if err != nil {
		r.noteAttestationFailure(obj, err)
		return nil
	}
	return &ociv1alpha1.AttestationStatus{Subject: digest, Signature: sig.String()}
}

func (r *ImageBuildReconciler) noteAttestationFailure(obj *ociv1alpha1.ImageBuild, err error) {
	recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonAttestationFailed,
		fmt.Sprintf("the image is pushed, but signing it failed: %v", err))
}
