package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/lhns/kube-oci-composer/internal/store"
)

// PresignTTL bounds a redirect URL's validity. Long enough for a slow node to finish pulling a
// large layer, short enough that a leaked URL is not a durable credential.
const PresignTTL = 30 * time.Minute

// BlobHandler serves registry blobs out of a Store.
//
// It replaces go-containerregistry's disk handler for three reasons, in order of how much they
// matter:
//
//  1. Garbage collection needs deletion, and needs the blobs to live somewhere it can enumerate.
//  2. Object storage becomes possible, so a restart does not have to re-upload every layer.
//  3. Upstream's Stat re-reads and re-hashes the whole blob on every call, and Stat runs on every
//     HEAD and again before every GET — so a 200 MB artifact is read twice per pull to re-derive
//     a digest that was already verified when it was written.
//
// The repo argument is ignored throughout, exactly as upstream's disk handler ignores it. Blobs
// are content-addressed, so the same digest in two repositories is the same bytes; keying by repo
// would store them twice and break sharing between compositions.
type BlobHandler struct {
	Store store.Store

	// Presign, when set, redirects blob reads to a URL the client fetches directly, so the bytes
	// do not stream through the controller. Off unless the backend supports it AND it was asked
	// for, since it exposes the object-store endpoint to every pulling client.
	Presign store.Presigner
}

var (
	_ registry.BlobHandler       = (*BlobHandler)(nil)
	_ registry.BlobStatHandler   = (*BlobHandler)(nil)
	_ registry.BlobPutHandler    = (*BlobHandler)(nil)
	_ registry.BlobDeleteHandler = (*BlobHandler)(nil)
)

// NewBlobHandler wraps a Store. Presigned redirects are enabled only when the backend supports
// them and presign is true.
func NewBlobHandler(s store.Store, presign bool) *BlobHandler {
	h := &BlobHandler{Store: s}
	if presign {
		if p, ok := s.(store.Presigner); ok {
			h.Presign = p
		}
	}
	return h
}

func key(h v1.Hash) string {
	return store.NamespaceBlobs + "/" + h.Algorithm + "/" + h.Hex
}

// translate maps a store error into what pkg/registry expects. A miss must become
// registry.ErrNotFound so it is served as a 404; anything else surfaces as a 500, which is
// correct for a genuinely broken backend and wrong for an absent blob.
func translate(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return registry.ErrNotFound
	}
	return err
}

// Stat returns the blob's size.
//
// Deliberately does NOT redirect, even when presigning is enabled: this is what answers HEAD, and
// a client checking whether a blob exists should get a cheap definitive answer rather than a
// redirect it has to follow. GET is where the bytes are, so GET is where the redirect belongs.
func (b *BlobHandler) Stat(ctx context.Context, _ string, h v1.Hash) (int64, error) {
	info, err := b.Store.Stat(ctx, key(h))
	if err != nil {
		return 0, translate(err)
	}
	return info.Size, nil
}

// Get returns the blob's contents, or redirects to object storage when presigning is on.
func (b *BlobHandler) Get(ctx context.Context, _ string, h v1.Hash) (io.ReadCloser, error) {
	if b.Presign != nil {
		url, err := b.Presign.Presign(ctx, key(h), PresignTTL)
		switch {
		case err == nil:
			// 307 rather than 302: the method and body must be preserved, and some clients
			// downgrade a 302 to GET. Returned as a value, which is what pkg/registry matches on.
			return nil, registry.RedirectError{Location: url, Code: http.StatusTemporaryRedirect}
		case errors.Is(err, store.ErrNotFound):
			return nil, registry.ErrNotFound
		default:
			// Fall through and stream it ourselves. A presigning failure is a reason to be slow,
			// not a reason to fail a pull.
		}
	}

	rc, err := b.Store.Open(ctx, key(h))
	if err != nil {
		return nil, translate(err)
	}
	return rc, nil
}

// Put stores a blob. pkg/registry has already verified the content against the digest by the time
// this is called, so there is nothing to re-check here.
func (b *BlobHandler) Put(ctx context.Context, _ string, h v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()
	if err := b.Store.Write(ctx, key(h), rc); err != nil {
		return fmt.Errorf("storing blob %s: %w", h, err)
	}
	return nil
}

// Delete removes a blob. Used by garbage collection, which is why the Store interface requires
// deletion to be idempotent — a sweep must not fail because something else got there first.
func (b *BlobHandler) Delete(ctx context.Context, _ string, h v1.Hash) error {
	if err := b.Store.Delete(ctx, key(h)); err != nil {
		return translate(err)
	}
	return nil
}
