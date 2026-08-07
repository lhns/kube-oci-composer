package source

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ErrBadReference marks a pull failure that a retry cannot fix — a malformed reference, or a
// multi-architecture index where a platform-specific digest is required. Typed rather than
// string-matched, so the caller can map it to Stalled without inspecting the message.
type ErrBadReference struct{ Reason string }

func (e *ErrBadReference) Error() string { return e.Reason }

// PullImage fetches a digest-pinned image whose layers become part of the composition.
//
// The reference is always built from the digest, never a tag, so what is pulled is exactly what
// the spec named. That is the same rule every other source follows (ADR 0002), and here it also
// means the pull is cacheable by the registry and reproducible across reconciles.
//
// Layers are contributed as they are rather than being unpacked and repacked. They are already
// content-addressed; rebuilding them would change their digests, break sharing with anything else
// using the same base, and force a re-upload of content the registry already has.
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

	// A multi-architecture index is refused rather than resolved — WHEN the spec names no
	// platforms, which is when this function is called.
	//
	// go-containerregistry would happily pick a platform for us, which is precisely the problem:
	// the choice would be made by the controller's own defaults rather than by the spec, so the
	// same ImageComposition could produce different output on different builds. Naming the
	// platform-specific digest keeps the output a pure function of the spec, and the error says
	// how to find it.
	//
	// The refusal was always conditional on the platform list not existing; it now says so, and
	// points at the other way out. With spec.platforms set the choice comes FROM the spec, so an
	// index base is correct and PullImageIndex is used instead.
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
// platform.
//
// This is the multi-platform counterpart of PullImage, and the reason ADR 0015's refusal is
// conditional rather than absolute: the platform list comes from the spec, so selecting a child is
// spec-driven and the output stays a pure function of the spec.
//
// A single-platform manifest is accepted too, and satisfies exactly one requested platform — the
// one it declares. Asking for two platforms from a base that is not an index is an error rather
// than silently reusing the same child, which would produce an index whose children lie about what
// they contain.
//
// A requested platform the base does not offer is an ErrBadReference: the spec asked for something
// that does not exist, and substituting a near-match is how you end up shipping an amd64 binary to
// an arm node.
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
