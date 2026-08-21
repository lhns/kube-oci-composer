package controller

import (
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// builtArtifact is what a build produced: either one image manifest, or an index over one child
// per platform.
//
// The two are kept behind a single type rather than branching at every call site because the
// places that must handle both are exactly the places where forgetting one is silent — publishing
// (an index needs its children written first), persistence (an index restored without its children
// serves 404), and garbage collection (children that nothing records get swept out from under a
// retained index). A type with one method each makes those three the same shape.
type builtArtifact struct {
	// exactly one of these is set
	img v1.Image
	idx v1.ImageIndex
}

func singleArtifact(img v1.Image) builtArtifact     { return builtArtifact{img: img} }
func indexArtifact(idx v1.ImageIndex) builtArtifact { return builtArtifact{idx: idx} }

func (a builtArtifact) Digest() (v1.Hash, error) {
	if a.idx != nil {
		return a.idx.Digest()
	}
	return a.img.Digest()
}

func (a builtArtifact) RawManifest() ([]byte, error) {
	if a.idx != nil {
		return a.idx.RawManifest()
	}
	return a.img.RawManifest()
}

// write publishes to a reference. remote.Write handles an image; an index needs remote.WriteIndex,
// which also uploads every child manifest and its blobs.
func (a builtArtifact) write(ref name.Reference, opts ...remote.Option) error {
	if a.idx != nil {
		return remote.WriteIndex(ref, a.idx, opts...)
	}
	return remote.Write(ref, a.img, opts...)
}

// children returns the per-platform images of an index, or the single image itself.
func (a builtArtifact) children() ([]v1.Image, error) {
	if a.idx == nil {
		return []v1.Image{a.img}, nil
	}
	im, err := a.idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading the index manifest: %w", err)
	}
	out := make([]v1.Image, 0, len(im.Manifests))
	for _, desc := range im.Manifests {
		child, err := a.idx.Image(desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("reading child %s: %w", desc.Digest, err)
		}
		out = append(out, child)
	}
	return out, nil
}

// childDigests returns the child manifest digests of an index, and nothing for a single image —
// where the manifest digest is already the artifact digest.
func (a builtArtifact) childDigests() ([]string, error) {
	if a.idx == nil {
		return nil, nil
	}
	im, err := a.idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading the index manifest: %w", err)
	}
	out := make([]string, 0, len(im.Manifests))
	for _, desc := range im.Manifests {
		out = append(out, desc.Digest.String())
	}
	return out, nil
}
