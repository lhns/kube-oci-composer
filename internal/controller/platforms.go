package controller

import (
	"context"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// declaredPlatforms parses spec.platforms. Empty means the spec does not say, which is resolved
// later from the base or the controller — see assemble.
func declaredPlatforms(obj *ociv1alpha1.ImageComposition) ([]oci.Platform, error) {
	if len(obj.Spec.Platforms) == 0 {
		return nil, nil
	}
	out := make([]oci.Platform, 0, len(obj.Spec.Platforms))
	seen := make(map[string]struct{}, len(obj.Spec.Platforms))
	for _, s := range obj.Spec.Platforms {
		p, err := oci.ParsePlatform(s)
		if err != nil {
			// The CRD pattern already rejects this; retrying cannot reparse it.
			return nil, recon.Terminal("spec.platforms: %v", err)
		}
		if _, dup := seen[p.String()]; dup {
			// Two identical children would make the index's descriptors ambiguous.
			return nil, recon.Terminal("spec.platforms: %s is listed twice", p)
		}
		seen[p.String()] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

// assemble builds the artifact: one image when the spec names at most one platform, an index over
// per-platform children when it names several.
//
// The single-platform path must keep producing the same digest as before multi-platform support,
// or every existing artifact would churn.
func (r *ImageCompositionReconciler) assemble(ctx context.Context, obj *ociv1alpha1.ImageComposition,
	declared []oci.Platform, inputs []oci.LayerInput, cfg oci.Config, workDir string) (builtArtifact, error) {

	if len(declared) > 1 {
		bases, err := r.resolveBases(ctx, obj, declared)
		if err != nil {
			return builtArtifact{}, err
		}
		idx, err := oci.AssembleIndex(bases, inputs, cfg, declared, workDir)
		if err != nil {
			return builtArtifact{}, recon.Terminal("assembling: %v", err)
		}
		return indexArtifact(idx), nil
	}

	base, err := r.resolveBase(ctx, obj)
	if err != nil {
		return builtArtifact{}, err
	}

	// Exactly one platform named: a single manifest, with the platform from the spec.
	if len(declared) == 1 {
		img, err := oci.AssembleAs(base, inputs, cfg, declared[0], workDir)
		if err != nil {
			return builtArtifact{}, recon.Terminal("assembling: %v", err)
		}
		return singleArtifact(img), nil
	}

	img, err := oci.Assemble(base, inputs, cfg, workDir)
	if err != nil {
		return builtArtifact{}, recon.Terminal("assembling: %v", err)
	}
	return singleArtifact(img), nil
}

// resolveBases pulls the base once and returns the child selected for each platform.
//
// Returns nil for a base-less composition, which AssembleIndex reads as "scratch for every
// platform" — the common case for a bundle of files that is only ever mounted.
func (r *ImageCompositionReconciler) resolveBases(ctx context.Context, obj *ociv1alpha1.ImageComposition,
	platforms []oci.Platform) (map[oci.Platform]v1.Image, error) {

	if obj.Spec.Base == nil {
		return nil, nil
	}
	base := obj.Spec.Base

	opts, err := r.pullOptions(ctx, obj.Namespace, base.SecretRef)
	if err != nil {
		return nil, err
	}

	want := make([]v1.Platform, 0, len(platforms))
	for _, p := range platforms {
		want = append(want, v1.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant})
	}

	repository, digest := base.Repository()
	byKey, err := source.PullImageIndex(ctx, repository, digest, want, opts...)
	if err != nil {
		// A platform the base does not offer is a bad reference: only a spec change fixes it.
		return nil, pullFailure("base image", err)
	}

	out := make(map[oci.Platform]v1.Image, len(platforms))
	for _, p := range platforms {
		img, ok := byKey[p.String()]
		if !ok {
			return nil, recon.Terminal("base image: no %s manifest", p)
		}
		out[p] = img
	}
	return out, nil
}
