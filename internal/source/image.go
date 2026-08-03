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

	// A multi-architecture index is refused rather than resolved.
	//
	// go-containerregistry would happily pick a platform for us, which is precisely the problem:
	// the choice would be made by the controller's own defaults rather than by the spec, so the
	// same ImageComposition could produce different output on different builds. Naming the
	// platform-specific digest keeps the output a pure function of the spec, and the error says
	// how to find it.
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		return nil, &ErrBadReference{Reason: fmt.Sprintf(
			"%s is a multi-architecture index; pin a platform-specific digest instead "+
				"(crane digest --platform linux/amd64 %s)", ref, repository)}
	}

	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("reading image %s: %w", ref, err)
	}
	return img, nil
}
