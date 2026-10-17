package source

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ErrBadReference marks a pull failure that a retry cannot fix: a malformed reference, or an index
// where a platform-specific digest is required. The caller maps it to Stalled.
type ErrBadReference struct{ Reason string }

func (e *ErrBadReference) Error() string { return e.Reason }

// PullImage fetches a digest-pinned image whose layers become part of the composition (ADR 0002).
// Layers are used as they are, never repacked, so their digests and registry sharing are kept.
// An index is refused: see PullImageFor.
func PullImage(ctx context.Context, repository, digest string, opts ...remote.Option) (v1.Image, error) {
	img, _, err := PullImageFor(ctx, repository, digest, nil, opts...)
	return img, err
}

// PullImageFor is PullImage for a spec that may name platforms (spec.platforms, ADRs 0015 and 0024).
// Exactly one result is non-nil.
//
// A single manifest is returned as it is, with no platform check: as an image layer it is plain
// content, and as a base under one declared platform it has always been taken on trust.
//
// An index needs a platform to be chosen, and the spec must be the one choosing, never the
// controller's defaults, or the output would depend on more than the spec. With platforms it yields
// the child for each, keyed by platformKey; without, it is refused.
func PullImageFor(ctx context.Context, repository, digest string, platforms []v1.Platform,
	opts ...remote.Option) (v1.Image, map[string]v1.Image, error) {
	ref, desc, err := getPinned(ctx, repository, digest, opts)
	if err != nil {
		return nil, nil, err
	}

	if isIndex(desc) {
		if len(platforms) == 0 {
			return nil, nil, &ErrBadReference{Reason: fmt.Sprintf(
				"%s is a multi-architecture index; either set spec.platforms to select from it, or "+
					"pin a platform-specific digest (crane digest --platform linux/amd64 %s)",
				ref, repository)}
		}
		children, err := selectChildren(ref, desc, platforms)
		return nil, children, err
	}

	img, err := desc.Image()
	if err != nil {
		return nil, nil, fmt.Errorf("reading image %s: %w", ref, err)
	}
	return img, nil, nil
}

func isIndex(desc *remote.Descriptor) bool {
	return desc.MediaType == types.OCIImageIndex || desc.MediaType == types.DockerManifestList
}

// selectChildren picks the child of an index for each requested platform. A platform the index does
// not offer is an ErrBadReference, never a near-match substitute.
func selectChildren(ref name.Digest, desc *remote.Descriptor, platforms []v1.Platform) (map[string]v1.Image, error) {
	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("reading index %s: %w", ref, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading index manifest %s: %w", ref, err)
	}
	out := make(map[string]v1.Image, len(platforms))
	for _, want := range platforms {
		var found bool
		for _, m := range im.Manifests {
			if m.Platform == nil || !platformMatches(*m.Platform, want) {
				continue
			}
			child, err := idx.Image(m.Digest)
			if err != nil {
				return nil, fmt.Errorf("reading child %s of %s: %w", m.Digest, ref, err)
			}
			out[platformKey(want)] = child
			found = true
			break
		}
		if !found {
			return nil, &ErrBadReference{Reason: fmt.Sprintf(
				"%s offers no %s manifest", ref, platformKey(want))}
		}
	}
	return out, nil
}

// PullImageIndex fetches a digest-pinned base and returns the child image for each requested
// platform (spec.platforms, ADR 0015).
//
// A single-platform manifest satisfies only the one platform it declares.
func PullImageIndex(ctx context.Context, repository, digest string, platforms []v1.Platform,
	opts ...remote.Option) (map[string]v1.Image, error) {
	ref, desc, err := getPinned(ctx, repository, digest, opts)
	if err != nil {
		return nil, err
	}

	if isIndex(desc) {
		return selectChildren(ref, desc, platforms)
	}

	out := make(map[string]v1.Image, len(platforms))

	// Not an index. Usable only if it is the one platform asked for.
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("reading image %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading config of %s: %w", ref, err)
	}
	have := v1.Platform{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant}
	for _, want := range platforms {
		if !platformMatches(have, want) {
			return nil, &ErrBadReference{Reason: fmt.Sprintf(
				"%s is a single %s manifest but %s was requested; pin a multi-architecture index "+
					"as the base to build for several platforms",
				ref, platformKey(have), platformKey(want))}
		}
		out[platformKey(want)] = img
	}
	return out, nil
}

// getPinned fetches the descriptor of repository@digest. A malformed reference is an
// ErrBadReference.
func getPinned(ctx context.Context, repository, digest string, opts []remote.Option) (name.Digest, *remote.Descriptor, error) {
	ref, err := name.NewDigest(repository + "@" + digest)
	if err != nil {
		return name.Digest{}, nil, &ErrBadReference{
			Reason: fmt.Sprintf("invalid image reference %s@%s: %v", repository, digest, err),
		}
	}
	desc, err := remote.Get(ref, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return name.Digest{}, nil, fmt.Errorf("pulling %s: %w", ref, err)
	}
	return ref, desc, nil
}

// platformMatches compares os/arch, and variant only when the request names one. A base child
// tagged linux/arm64/v8 satisfies a request for linux/arm64, which is how registries treat it.
func platformMatches(have, want v1.Platform) bool {
	if have.OS != want.OS || have.Architecture != want.Architecture {
		return false
	}
	return want.Variant == "" || have.Variant == want.Variant
}

func platformKey(p v1.Platform) string {
	if p.Variant != "" {
		return p.OS + "/" + p.Architecture + "/" + p.Variant
	}
	return p.OS + "/" + p.Architecture
}
