package controller

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/attest"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// attestSources maps what the composer resolved into the neutral shape internal/attest takes.
//
// Identity rather than Digest where the two differ, for the reason InputHash gives: a Flux
// artifact's tarball digest moves when source-controller re-packs, while the revision it describes
// does not. The revision answers "what produced this"; the tarball digest does not.
func attestSources(inputs []oci.LayerInput) []attest.Source {
	out := make([]attest.Source, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, attest.Source{
			Name:    in.Name,
			URI:     in.URL,
			Digest:  in.Digest,
			Version: in.Identity,
			Target:  in.Target,
		})
	}
	return out
}

// attestPublished attaches the SBOM, provenance and signature to an artifact that has just been
// published, or confirms what is already there.
//
// FAILURE IS NOT FATAL, and that follows the precedent already in this file for the manifest
// record: the artifact is published and pullable right now, so reporting a build that succeeded as
// failed would be the larger error. It surfaces as a Warning event and Ready stays true.
func (r *ImageCompositionReconciler) attestPublished(
	ctx context.Context,
	obj *ociv1alpha1.ImageComposition,
	tgt target,
	digest v1.Hash,
	inputs []oci.LayerInput,
	baseDigest string,
	refOpts []name.Option,
	opts []remote.Option,
) *ociv1alpha1.AttestationStatus {
	if !r.Attestor.Enabled() {
		return nil
	}

	repo, err := name.NewRepository(tgt.writeRepo, refOpts...)
	if err != nil {
		r.noteAttestationFailure(obj, fmt.Errorf("parsing %s: %w", tgt.writeRepo, err))
		return nil
	}

	desc, err := remote.Head(repo.Digest(digest.String()), opts...)
	if err != nil {
		r.noteAttestationFailure(obj, fmt.Errorf("describing the artifact: %w", err))
		return nil
	}

	var base *attest.Source
	if baseDigest != "" {
		base = &attest.Source{Name: "base", Digest: baseDigest}
	}

	rec, err := r.Attestor.Ensure(ctx, repo, *desc, attest.Payloads{
		BuildType: attest.BuildTypeComposition,
		// The SAME field set the input hash covers. Provenance narrower than the hash would claim
		// less than the artifact actually depends on, which is the shape of ADR 0026's incident.
		External: externalParameters(obj),
		Internal: map[string]any{"assemblyVersion": oci.AssemblyVersion},
		Base:     base,
		Sources:  attestSources(inputs),
	}, opts)
	if err != nil {
		r.noteAttestationFailure(obj, err)
		return nil
	}
	if rec == nil {
		return nil
	}
	return &ociv1alpha1.AttestationStatus{
		Subject:    rec.Subject,
		SBOM:       rec.SBOM,
		Provenance: rec.Provenance,
		Signature:  rec.Signature,
	}
}

// externalParameters is the resolved spec, as SLSA's buildDefinition.externalParameters.
func externalParameters(obj *ociv1alpha1.ImageComposition) map[string]any {
	layers := make([]map[string]any, 0, len(obj.Spec.Layers))
	for _, l := range obj.Spec.Layers {
		entry := map[string]any{"name": l.Name}
		if l.To != "" {
			entry["to"] = l.To
		}
		if len(l.Remove) > 0 {
			entry["remove"] = l.Remove
		}
		layers = append(layers, entry)
	}
	out := map[string]any{"layers": layers}
	if len(obj.Spec.Platforms) > 0 {
		out["platforms"] = obj.Spec.Platforms
	}
	if obj.Spec.Base != nil {
		out["base"] = map[string]any{"image": obj.Spec.Base.Image, "digest": obj.Spec.Base.Digest}
	}
	return out
}

func (r *ImageCompositionReconciler) noteAttestationFailure(obj *ociv1alpha1.ImageComposition, err error) {
	recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonAttestationFailed,
		fmt.Sprintf("the artifact is published, but attaching supply-chain material failed: %v", err))
}

// attestRecord converts the API shape to the one internal/attest reasons about. Two types rather
// than one because internal/attest must not depend on the API package: it is used by two
// controllers and shared with neither's types.
func attestRecord(st *ociv1alpha1.AttestationStatus) *attest.Record {
	if st == nil {
		return nil
	}
	return &attest.Record{
		Subject:    st.Subject,
		SBOM:       st.SBOM,
		Provenance: st.Provenance,
		Signature:  st.Signature,
	}
}
