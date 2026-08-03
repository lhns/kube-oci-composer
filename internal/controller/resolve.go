package controller

import (
	"context"
	"errors"
	"fmt"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// resolveInputs turns spec layers into assembly inputs, resolving digests where they are not
// declared.
//
// Ordering is preserved exactly: layers are contributed in declaration order and nothing here
// reorders, groups or promotes any entry. See ADR 0003.
//
// url and image entries carry their digest in the spec. sourceRef and configMapRef do not, and
// their digest is resolved here — from the Flux source's status.artifact, or by hashing the
// ConfigMap's content. Resolution happens BEFORE the input hash is computed, so a change to a
// referenced source or ConfigMap changes the hash and triggers a rebuild, exactly as editing a
// declared digest would. See ADR 0002.
//
// Path is deliberately left empty: nothing is fetched until the short-circuit has decided a build
// is actually needed.
func (r *ImageCompositionReconciler) resolveInputs(ctx context.Context, obj *ociv1alpha1.ImageComposition, workDir string) ([]oci.LayerInput, error) {
	inputs := make([]oci.LayerInput, 0, len(obj.Spec.Layers))

	for _, l := range obj.Spec.Layers {
		in := oci.LayerInput{
			Name:   l.Name,
			Unpack: oci.UnpackMode(orDefault(string(l.Unpack), "none")),
			Target: orDefault(l.Target, "/"),
		}

		switch {
		case l.URLSource != nil:
			in.URL = l.URL
			in.Digest = l.Digest

		case l.Image != nil:
			// Only the repository is recorded here. The pull happens later, with the fetches,
			// so an unchanged spec still costs one HEAD rather than a registry round trip per
			// base image on every interval.
			in.ImageRepository = l.Image.Repository
			in.Digest = l.Digest

		case l.SourceRef != nil:
			art, err := r.resolveFluxSource(ctx, obj, l.SourceRef)
			if err != nil {
				return nil, err
			}
			in.URL = art.URL
			in.Digest = art.Digest
			// source-controller always publishes a gzipped tar, whatever the source kind. An
			// explicit unpack on the layer would be meaningless here, so the resolved value wins.
			in.Unpack = oci.UnpackTarGz
			in.Subpath = l.SourceRef.Path

		case l.ConfigMapRef != nil:
			resolved, err := source.ConfigMap(ctx, r.Client, obj.Namespace,
				l.ConfigMapRef.Name, l.ConfigMapRef.Optional, workDir)
			if err != nil {
				var nf *source.ErrNotFound
				if errors.As(err, &nf) {
					// Terminal: a missing ConfigMap that was not marked optional is a spec
					// problem, and retrying will not create it.
					return nil, terminal("layer %q: %s", l.Name, err)
				}
				return nil, fmt.Errorf("layer %q: %w", l.Name, err)
			}
			if resolved.Empty {
				// An optional ConfigMap that is absent, or one with no entries. Contributing an
				// empty layer would still change the output digest, so skip it entirely and let
				// the composition be exactly what it would have been without the entry.
				continue
			}
			in.Digest = resolved.Digest
			in.Path = resolved.Path
			in.Unpack = oci.UnpackTarGz

		default:
			// CEL already enforces the union, so this only fires for a source kind the CRD
			// permits and this build does not implement.
			return nil, terminal("layer %q: no supported source is set", l.Name)
		}

		inputs = append(inputs, in)
	}

	if len(inputs) == 0 {
		return nil, terminal("every layer resolved to nothing; there is no content to compose")
	}
	return inputs, nil
}

// resolveFluxSource reads the referenced source's published artifact.
func (r *ImageCompositionReconciler) resolveFluxSource(ctx context.Context, obj *ociv1alpha1.ImageComposition, ref *ociv1alpha1.SourceRef) (source.FluxArtifact, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = obj.Namespace
	}

	art, err := source.FluxSource(ctx, r.Client, ref.Kind, ns, ref.Name)
	if err != nil {
		var nf *source.ErrNotFound
		if errors.As(err, &nf) {
			// Terminal: the reference names something that does not exist.
			return source.FluxArtifact{}, terminal("source %s %s/%s not found", ref.Kind, ns, ref.Name)
		}
		// Everything else — including "no artifact yet" — is transient. source-controller may
		// simply not have finished its first reconcile.
		return source.FluxArtifact{}, err
	}
	return art, nil
}
