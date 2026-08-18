package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
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
// has decided a build is actually needed — including an image layer's manifest, whose digest is
// declared in the spec and so needs nothing from a registry to hash.

// imagePulls maps an input's index to the spec entry describing how to pull it, for the entries
// whose content is another image. The fetch phase uses it; nothing else does.
type imagePulls map[int]*ociv1alpha1.ImageSource

func (r *ImageCompositionReconciler) resolveInputs(ctx context.Context, obj *ociv1alpha1.ImageComposition, workDir string) ([]oci.LayerInput, imagePulls, error) {
	inputs := make([]oci.LayerInput, 0, len(obj.Spec.Layers))
	pulls := imagePulls{}

	for _, l := range obj.Spec.Layers {
		in := oci.LayerInput{Name: l.Name, Target: l.To}

		if l.Owner != nil {
			in.UID, in.GID = l.Owner.UID, l.Owner.GID
		}
		if l.Mode != nil {
			var err error
			if in.FileMode, err = parseMode(l.Name, "file", l.Mode.File); err != nil {
				return nil, nil, err
			}
			if in.DirMode, err = parseMode(l.Name, "dir", l.Mode.Dir); err != nil {
				return nil, nil, err
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
				return nil, nil, err
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
					return nil, nil, recon.Pending("layer %q: %s", l.Name, err)
				}
				return nil, nil, fmt.Errorf("layer %q: %w", l.Name, err)
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
			// Not pulled here. The digest is declared in the spec, so the hash needs nothing from
			// the registry — and pulling would make every reconcile of an image layer cost a
			// manifest round trip even when the short-circuit is about to decide there is nothing
			// to do. The pull is recorded for the fetch phase, exactly as Path is left empty for
			// every other remote source.
			repository, digest := l.Image.Repository()
			in.URL = repository
			in.Digest = digest
			in.Unpack = oci.UnpackImage
			in.Subpath = l.Image.Subpath
			pulls[len(inputs)] = l.Image

		case len(l.Remove) > 0:
			in.Remove = l.Remove

		default:
			// CEL already enforces the union, so this only fires for a verb the CRD permits and
			// this build does not implement.
			return nil, nil, recon.Terminal("layer %q: no supported source is set", l.Name)
		}

		inputs = append(inputs, in)
	}

	if len(inputs) == 0 {
		return nil, nil, recon.Terminal("every layer resolved to nothing; there is no content to compose")
	}
	return inputs, pulls, nil
}

// pullImageLayer fetches the manifest for an image layer, once a build is known to be needed.
func (r *ImageCompositionReconciler) pullImageLayer(ctx context.Context, obj *ociv1alpha1.ImageComposition,
	in oci.LayerInput, src *ociv1alpha1.ImageSource) (v1.Image, error) {

	opts, err := r.pullOptions(ctx, obj.Namespace, src.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("layer %q: %w", in.Name, err)
	}
	img, err := source.PullImage(ctx, in.URL, in.Digest, opts...)
	if err != nil {
		var badRef *source.ErrBadReference
		if errors.As(err, &badRef) {
			// A malformed reference, or an index where a platform-specific manifest is required.
			// Editing the layer is what fixes it, so retrying would repeat the same failure.
			return nil, recon.Terminal("layer %q: %v", in.Name, err)
		}
		return nil, fmt.Errorf("layer %q: %w", in.Name, err)
	}
	return img, nil
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
		return 0, recon.Terminal("layer %q: %s mode %q is not octal", layer, which, value)
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
			return nil, recon.Terminal("base image: %v", err)
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
			return source.FluxArtifact{}, recon.Pending("source %s %s/%s not found yet", ref.Kind, ns, ref.Name)
		}
		var nr *source.ErrNotReady
		if errors.As(err, &nr) {
			// Pending for the same reason and one degree more sharply: the source exists, but its
			// status describes a spec that is no longer the one in the cluster. Waiting is the only
			// safe answer — building from that artifact publishes the PREVIOUS revision's content
			// under the tag the current revision was supposed to get, and a tag's first publish has
			// nothing for the immutability guard to refuse. See ADR 0026.
			//
			// Not terminal: source-controller catching up bumps no generation here, and the source
			// is watched, so the wait normally ends within seconds.
			return source.FluxArtifact{}, recon.Pending("%s", err)
		}
		// Everything else — including "no artifact yet" — is transient. source-controller may
		// simply not have finished its first reconcile.
		return source.FluxArtifact{}, err
	}
	return art, nil
}
