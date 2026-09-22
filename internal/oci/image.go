package oci

import (
	"archive/tar"
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Image filesystems as layer content, placed at a path like any other content.
//
// The image is FLATTENED to a single layer: mutate.Extract applies whiteouts and yields an ordinary
// tar, so everything else is extractTar's. Splicing its layers instead would reinstate the "one
// entry, many layers" exception ADR 0016 removed; the cost is blob sharing, so this does not
// replace spec.base (ADR 0024).

// extractImage returns the flattened filesystem of img, rebased under target and filtered by
// subpath.
func extractImage(img v1.Image, target, subpath string, strip int) ([]tarEntry, error) {
	rc := mutate.Extract(img)
	defer rc.Close()

	entries, err := extractTar(tar.NewReader(rc), target, subpath, strip)
	if err != nil {
		return nil, fmt.Errorf("reading image filesystem: %w", err)
	}
	return entries, nil
}
