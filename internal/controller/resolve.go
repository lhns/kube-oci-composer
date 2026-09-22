package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// imagePulls maps an input's index to the spec entry describing how to pull it, for the entries
// whose content is another image. Only the fetch phase uses it.
type imagePulls map[int]*ociv1alpha1.ImageSource

// resolveInputs turns spec layers into assembly inputs, in declaration order, resolving digests
// where they are not declared.
//
// `sourceRef` and `configMap` digests are resolved here, BEFORE the input hash, so a change to the
// referenced object moves the hash exactly as editing a declared digest would (ADR 0002). Path is
// left empty for remote sources: nothing is fetched until the short-circuit decides a build is
// needed.
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
			in.StripComponents = l.Fetch.StripComponents

		case l.SourceRef != nil:
			art, err := r.resolveFluxSource(ctx, obj, l.SourceRef)
			if err != nil {
				return nil, nil, err
			}
			in.URL = art.URL
			in.Digest = art.Digest
			// The revision identifies the CONTENT; the tarball digest moves whenever
			// source-controller re-packs.
			in.Identity = art.Revision
			// source-controller always publishes a gzipped tar, whatever the source kind.
			in.Unpack = oci.UnpackTarGz
			in.Subpath = l.SourceRef.Subpath

		case l.ConfigMap != nil:
			resolved, err := source.ConfigMap(ctx, r.Client, obj.Namespace,
				l.ConfigMap.Name, l.ConfigMap.Optional, workDir)
			if err != nil {
				var nf *source.ErrNotFound
				if errors.As(err, &nf) {
					// Pending: creating the ConfigMap (which is watched) fixes it.
					return nil, nil, recon.Pending("layer %q: %s", l.Name, err)
				}
				return nil, nil, fmt.Errorf("layer %q: %w", l.Name, err)
			}
			if resolved.Empty {
				// Absent-and-optional, or empty: skipped entirely, since even an empty layer
				// would change the output digest.
				continue
			}
			in.Digest = resolved.Digest
			in.Path = resolved.Path
			in.Unpack = oci.UnpackTarGz

		case l.Image != nil:
			// Not pulled here: the digest is declared, so the hash needs nothing from the
			// registry. The pull is recorded for the fetch phase.
			repository, digest := l.Image.Repository()
			in.URL = repository
			in.Digest = digest
			in.Unpack = oci.UnpackImage
			in.Subpath = l.Image.Subpath
			pulls[len(inputs)] = l.Image

		case len(l.Remove) > 0:
			in.Remove = l.Remove

		default:
			// CEL enforces the union; this fires only for a verb this build does not implement.
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
		return nil, pullFailure(fmt.Sprintf("layer %q", in.Name), err)
	}
	return img, nil
}

// pullFailure classifies an image pull error. A bad reference (malformed, or an index where a
// platform manifest is required) is Terminal, because only a spec edit fixes it; anything else is
// transient.
func pullFailure(what string, err error) error {
	var badRef *source.ErrBadReference
	if errors.As(err, &badRef) {
		return recon.Terminal("%s: %v", what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// parseMode converts an octal string from the spec into a mode.
func parseMode(layer, which, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	mode, err := strconv.ParseInt(value, 8, 32)
	if err != nil {
		// CEL already constrains the pattern; retrying cannot reinterpret it.
		return 0, recon.Terminal("layer %q: %s mode %q is not octal", layer, which, value)
	}
	return mode, nil
}

// resolveBase pulls the base image, if one is declared.
//
// Called after the short-circuit, so an unchanged spec never pulls its base.
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
		return nil, pullFailure("base image", err)
	}
	return img, nil
}

// resolveFluxSource reads the referenced source's published artifact.
func (r *ImageCompositionReconciler) resolveFluxSource(ctx context.Context, obj *ociv1alpha1.ImageComposition, ref *ociv1alpha1.SourceRefSource) (source.FluxArtifact, error) {
	// Pinning is optional by design (ADR 0026), unless the operator requires it cluster-wide (T1).
	if ref.Revision == "" {
		// Terminal, unlike the revision MISMATCH below: an absent pin is fixed by editing this spec.
		if r.RequirePinnedSources {
			return source.FluxArtifact{}, recon.Terminal(
				"layer source %s/%s names no revision, and this controller runs with "+
					"--require-pinned-sources: add `revision:` to pin what this layer consumes",
				ref.Kind, ref.Name)
		}
		warnUnpinnedUnderFail(r.Recorder, obj, ref)
	}

	// Same namespace only: the controller's RBAC is cluster-wide, so otherwise a tenant could bake
	// any namespace's source into an image they can read.
	ns := obj.Namespace
	if ref.Namespace != "" && ref.Namespace != obj.Namespace {
		return source.FluxArtifact{}, recon.Terminal(
			"layer source %s/%s is in namespace %q: a source must be in the same namespace as the "+
				"ImageComposition that consumes it", ref.Kind, ref.Name, ref.Namespace)
	}

	art, err := source.FluxSource(ctx, r.Client, ref.Kind, ns, ref.Name)
	if err != nil {
		var nf *source.ErrNotFound
		if errors.As(err, &nf) {
			// Pending: creating the source does not bump this generation.
			return source.FluxArtifact{}, recon.Pending("source %s %s/%s not found yet", ref.Kind, ns, ref.Name)
		}
		var nr *source.ErrNotReady
		if errors.As(err, &nr) {
			// The source's status describes a superseded spec. Building now would publish the
			// PREVIOUS revision under the current tag, which no guard catches on a first publish
			// (ADR 0026). The source is watched, so the wait usually ends within seconds.
			return source.FluxArtifact{}, recon.Pending("%s", err)
		}
		// Everything else, including "no artifact yet", is transient.
		return source.FluxArtifact{}, err
	}
	// A pinned revision that has not arrived yet is Pending, not terminal: the SOURCE catching up
	// fixes it, which bumps no generation here (ADR 0009).
	if !ociv1alpha1.RevisionMatches(ref.Revision, art.Revision) {
		return source.FluxArtifact{}, recon.Pending(
			"source %s/%s is at revision %q, waiting for %q",
			ref.Kind, ref.Name, art.Revision, ref.Revision)
	}

	return art, nil
}

// warnUnpinnedUnderFail says so when an unpinned layer feeds tags that cannot move. The caller has
// already established that the layer is unpinned.
//
// A digest-only publish is silent, since it collides with nothing. Repeats aggregate into one event
// with a rising count.
func warnUnpinnedUnderFail(
	recorder record.EventRecorder, obj *ociv1alpha1.ImageComposition, ref *ociv1alpha1.SourceRefSource,
) {
	if obj.Spec.Push.ResolveConflictPolicy() != ociv1alpha1.ConflictFail {
		return
	}
	// An unusable ref fails the reconcile later with a better message.
	tags, err := recon.EffectiveTags(obj.Spec.Push.GetTags(), obj.Spec.Push.GetRef())
	if err != nil || len(tags) == 0 {
		return
	}

	recon.Event(recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonUnpinnedSource,
		fmt.Sprintf("layer source %s/%s names no revision while %s publishes tags under "+
			"onConflict: %s. The tag is fixed and the source is not, so a build that starts "+
			"before the source catches up publishes the previous revision under it and the tag "+
			"cannot be corrected afterwards. Add `revision:` to this sourceRef, or use a "+
			"conflict policy that tolerates a moving source.",
			ref.Kind, ref.Name, strings.Join(tags, ","), ociv1alpha1.ConflictFail))
}
