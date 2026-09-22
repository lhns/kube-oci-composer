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
func PullImage(ctx context.Context, repository, digest string, opts ...remote.Option) (v1.Image, error) {
	ref, err := name.NewDigest(repository + "@" + digest)
	if err != nil {
		return nil, &ErrBadReference{
			Reason: fmt.Sprintf("invalid image reference %s@%s: %v", repository, digest, err),
		}
	}

	desc, err := remote.Get(ref, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", ref, err)
	}

	// Without spec.platforms an index is refused rather than resolved by the controller's own
	// defaults, which would make the output depend on more than the spec. With spec.platforms,
	// PullImageIndex is used instead.
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		return nil, &ErrBadReference{Reason: fmt.Sprintf(
			"%s is a multi-architecture index; either set spec.platforms to select from it, or "+
				"pin a platform-specific digest (crane digest --platform linux/amd64 %s)",
			ref, repository)}
	}

	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("reading image %s: %w", ref, err)
	}
	return img, nil
}

// PullImageIndex fetches a digest-pinned base and returns the child image for each requested
// platform (spec.platforms, ADR 0015).
//
// A single-platform manifest satisfies only the one platform it declares. A requested platform the
// base does not offer is an ErrBadReference, never a near-match substitute.
func PullImageIndex(ctx context.Context, repository, digest string, platforms []v1.Platform,
	opts ...remote.Option) (map[string]v1.Image, error) {
	ref, err := name.NewDigest(repository + "@" + digest)
	if err != nil {
		return nil, &ErrBadReference{
			Reason: fmt.Sprintf("invalid image reference %s@%s: %v", repository, digest, err),
		}
	}

	desc, err := remote.Get(ref, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", ref, err)
	}

	out := make(map[string]v1.Image, len(platforms))

	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("reading index %s: %w", ref, err)
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("reading index manifest %s: %w", ref, err)
		}
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
					"%s has no %s manifest; the base index does not offer that platform",
					ref, platformKey(want))}
			}
		}
		return out, nil
	}

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
