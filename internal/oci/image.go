package oci

import (
	"archive/tar"
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Image filesystems as layer content.
//
// An image published by someone's CI is a perfectly good bag of files, and until now the only way
// one could enter a composition was as spec.base — which allows exactly one, and always underneath
// everything. This places one at a path, like any other content.
//
// The image is FLATTENED to a single layer. mutate.Extract walks the image's layers in order and
// applies whiteouts, producing the filesystem a runtime would actually see, and that stream is an
// ordinary tar — so subpath selection, rebasing, mode normalisation, symlink handling, traversal
// refusal and deterministic ordering are all extractTar's, unchanged.
//
// Flattening rather than splicing is a constraint, not a shortcut. ADR 0016 hoisted the base out of
// the layer list because "an image entry contributes many layers where every other entry
// contributes exactly one"; carrying the source image's layers through here would reinstate the
// exact exception that removed. It costs the blob sharing spec.base gets by reusing a base's layers
// verbatim (ADR 0015) — this re-packs the bytes — which is the price of choosing where the content
// lands, and the reason this does not replace spec.base for building on top of something.

// extractImage returns the flattened filesystem of img, rebased under target and filtered by
// subpath.
func extractImage(img v1.Image, target, subpath string) ([]tarEntry, error) {
	rc := mutate.Extract(img)
	defer rc.Close()

	entries, err := extractTar(tar.NewReader(rc), target, subpath)
	if err != nil {
		return nil, fmt.Errorf("reading image filesystem: %w", err)
	}
	return entries, nil
}
