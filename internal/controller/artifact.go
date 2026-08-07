package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/serve"
	"github.com/lhns/kube-oci-composer/internal/store"
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

func (a builtArtifact) isIndex() bool { return a.idx != nil }

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

// restoreChildren republishes the child manifests of a multi-platform build before its index.
//
// Returns false when a child could not be restored, which the caller treats as "skip this build".
// Publishing the index anyway would produce the worst available outcome: a reference that resolves,
// passes a HEAD, and reports as published, while every pull following its descriptors 404s.
//
// A single-platform build has no children and returns true immediately, so both replay paths can
// call this unconditionally.
func restoreChildren(ctx context.Context, srv *serve.Server, logger logr.Logger,
	repoPath string, h ociv1alpha1.BuildRecord) bool {
	for _, child := range h.Manifests {
		if srv.HasManifest(ctx, repoPath, child) {
			continue
		}
		raw, err := srv.LoadManifest(ctx, child)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Published before children were persisted, or already reclaimed. Either way the
				// index cannot be served, so say so once rather than restoring a broken one.
				logger.Info("skipping an index whose child manifest is not stored",
					"index", h.Digest, "child", child)
			} else {
				logger.Error(err, "could not read a stored child manifest",
					"index", h.Digest, "child", child)
			}
			return false
		}
		if err := srv.PutManifest(ctx, repoPath, child, raw); err != nil {
			logger.Error(err, "could not restore a child manifest",
				"index", h.Digest, "child", child)
			return false
		}
	}
	return true
}

// saveManifests persists the artifact so it can be replayed after a restart.
//
// For an index this stores the CHILDREN as well as the index itself. Storing only the index would
// restore a manifest whose children are absent from the registry, which does not fail at restore
// time — it fails much later, as a pull that 404s on a reference the status says is published.
func (a builtArtifact) saveManifests(ctx context.Context, save func(context.Context, string, []byte) error) error {
	if a.idx != nil {
		children, err := a.children()
		if err != nil {
			return err
		}
		for _, child := range children {
			d, err := child.Digest()
			if err != nil {
				return fmt.Errorf("child digest: %w", err)
			}
			raw, err := child.RawManifest()
			if err != nil {
				return fmt.Errorf("child manifest: %w", err)
			}
			if err := save(ctx, d.String(), raw); err != nil {
				return fmt.Errorf("saving child %s: %w", d, err)
			}
		}
	}
	d, err := a.Digest()
	if err != nil {
		return err
	}
	raw, err := a.RawManifest()
	if err != nil {
		return err
	}
	return save(ctx, d.String(), raw)
}
