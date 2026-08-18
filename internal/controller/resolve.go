package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// resolveInputs turns spec layers into assembly inputs, resolving digests where they are not
// declared.
//
// Ordering is preserved exactly: layers are contributed in declaration order and nothing here
// reorders, groups or promotes any entry.
//
// `fetch` carries its digest in the spec. `sourceRef` and `configMap` do not, and theirs is
// resolved here — from the Flux source's status.artifact, or by hashing the ConfigMap's content.
// Resolution happens BEFORE the input hash is computed, so a change to a referenced source or
// ConfigMap moves the hash and triggers a rebuild, exactly as editing a declared digest would.
// See ADR 0002.
//
// Path is deliberately left empty for remote sources: nothing is fetched until the short-circuit
// has decided a build is actually needed.
func (r *ImageCompositionReconciler) resolveInputs(ctx context.Context, obj *ociv1alpha1.ImageComposition, workDir string) ([]oci.LayerInput, error) {
	inputs := make([]oci.LayerInput, 0, len(obj.Spec.Layers))

	for _, l := range obj.Spec.Layers {
		in := oci.LayerInput{Name: l.Name, Target: l.To}

		if l.Owner != nil {
			in.UID, in.GID = l.Owner.UID, l.Owner.GID
		}
		if l.Mode != nil {
			var err error
			if in.FileMode, err = parseMode(l.Name, "file", l.Mode.File); err != nil {
				return nil, err
			}
			if in.DirMode, err = parseMode(l.Name, "dir", l.Mode.Dir); err != nil {
				return nil, err
			}
		}

		switch {
		case l.Fetch != nil:
			in.URL = l.Fetch.URL
			in.Digest = l.Fetch.Digest
			in.Unpack = oci.UnpackMode(orDefault(string(l.Fetch.Unpack), "none"))
			in.Subpath = l.Fetch.Subpath

		case l.SourceRef != nil:
			art, err := r.resolveFluxSource(ctx, obj, l.SourceRef)
			if err != nil {
				return nil, err
			}
			in.URL = art.URL
			in.Digest = art.Digest
			// source-controller always publishes a gzipped tar, whatever the source kind.
			in.Unpack = oci.UnpackTarGz
			in.Subpath = l.SourceRef.Subpath

		case l.ConfigMap != nil:
			resolved, err := source.ConfigMap(ctx, r.Client, obj.Namespace,
				l.ConfigMap.Name, l.ConfigMap.Optional, workDir)
			if err != nil {
				var nf *source.ErrNotFound
				if errors.As(err, &nf) {
					// Creating the ConfigMap fixes this, not editing the layer, so it waits
					// rather than stalls. ConfigMaps are watched, so the wait is usually over
					// the moment one appears.
					return nil, pending("layer %q: %s", l.Name, err)
				}
				return nil, fmt.Errorf("layer %q: %w", l.Name, err)
			}
			if resolved.Empty {
				// An optional ConfigMap that is absent, or one with no entries. Contributing an
				// empty layer would still change the output digest, so skip the entry entirely
				// and let the composition be exactly what it would have been without it.
				continue
			}
			in.Digest = resolved.Digest
			in.Path = resolved.Path
			in.Unpack = oci.UnpackTarGz

		case l.Image != nil:
			repository, digest := l.Image.Repository()

			opts, err := r.pullOptions(ctx, obj.Namespace, l.Image.SecretRef)
			if err != nil {
				return nil, fmt.Errorf("layer %q: %w", l.Name, err)
			}
			img, err := source.PullImage(ctx, repository, digest, opts...)
			if err != nil {
				var badRef *source.ErrBadReference
				if errors.As(err, &badRef) {
					// A malformed reference, or an index where a platform-specific manifest is
					// required. Editing the layer is what fixes it, so retrying is pointless.
					return nil, terminal("layer %q: %v", l.Name, err)
				}
				return nil, fmt.Errorf("layer %q: %w", l.Name, err)
			}

			// The manifest digest is the content address, so it is what reaches InputHash. The
			// pulled image is carried alongside exactly as a fetched file's Path is.
			in.Digest = digest
			in.Image = img
			in.Unpack = oci.UnpackImage
			in.Subpath = l.Image.Subpath

		case len(l.Remove) > 0:
			in.Remove = l.Remove

		default:
			// CEL already enforces the union, so this only fires for a verb the CRD permits and
			// this build does not implement.
			return nil, terminal("layer %q: no supported source is set", l.Name)
		}

		inputs = append(inputs, in)
	}

	if len(inputs) == 0 {
		return nil, terminal("every layer resolved to nothing; there is no content to compose")
	}
	return inputs, nil
}

// parseMode converts an octal string from the spec into a mode.
func parseMode(layer, which, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	mode, err := strconv.ParseInt(value, 8, 32)
	if err != nil {
		// Terminal: CEL already constrains the pattern, so reaching here means a spec that
		// somehow passed validation and still cannot be interpreted.
		return 0, terminal("layer %q: %s mode %q is not octal", layer, which, value)
	}
	return mode, nil
}

// resolveBase pulls the base image, if one is declared.
//
// Called after the short-circuit, so an unchanged spec costs one HEAD rather than a registry
// round trip per base image on every interval.
func (r *ImageCompositionReconciler) resolveBase(ctx context.Context, obj *ociv1alpha1.ImageComposition) (v1.Image, error) {
	if obj.Spec.Base == nil {
		return nil, nil
	}
	base := obj.Spec.Base

	opts, err := r.pullOptions(ctx, obj.Namespace, base.SecretRef)
	if err != nil {
		return nil, err
	}

	repository, digest := base.Repository()
	img, err := source.PullImage(ctx, repository, digest, opts...)
	if err != nil {
		var badRef *source.ErrBadReference
		if errors.As(err, &badRef) {
			// A malformed reference or a multi-architecture index needs a spec change, so
			// retrying would only repeat the same failure on an interval.
			return nil, terminal("base image: %v", err)
		}
		return nil, fmt.Errorf("base image: %w", err)
	}
	return img, nil
}

// resolveFluxSource reads the referenced source's published artifact.
func (r *ImageCompositionReconciler) resolveFluxSource(ctx context.Context, obj *ociv1alpha1.ImageComposition, ref *ociv1alpha1.SourceRefSource) (source.FluxArtifact, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = obj.Namespace
	}

	art, err := source.FluxSource(ctx, r.Client, ref.Kind, ns, ref.Name)
	if err != nil {
		var nf *source.ErrNotFound
		if errors.As(err, &nf) {
			// NOT terminal. Creating the source is what fixes this, and that does not bump
			// this object's generation — so stalling would wait forever for an event that
			// cannot come. Applying a composition and its GitRepository in one commit
			// routinely lands here for a second.
			return source.FluxArtifact{}, pending("source %s %s/%s not found yet", ref.Kind, ns, ref.Name)
		}
		// Everything else — including "no artifact yet" — is transient. source-controller may
		// simply not have finished its first reconcile.
		return source.FluxArtifact{}, err
	}
	return art, nil
}
