// Package store provides content-addressed blob storage with interchangeable backends.
//
// Its caller is the layer cache (internal/cache), which is disposable: anything lost is refetched
// from upstream. The interface lets it live on a volume or in object storage.
//
// Keys are opaque strings, conventionally "<namespace>/sha256/<hex>", built with Key.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrNotFound is returned when a key does not exist. Backends must return this rather than a
// backend-specific error: a cache miss is ordinary control flow.
var ErrNotFound = errors.New("not found")

// Namespaces keep unrelated content apart within one backend.
const (
	// NamespaceInputs holds fetched layer sources, keyed by the digest declared in the spec.
	NamespaceInputs = "inputs"
)

// Info describes a stored object.
type Info struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Store is a flat content-addressed key/value store.
//
// Implementations must be safe for concurrent use. Write must be atomic: a reader never observes a
// partially written object. Two writers racing on one key is expected, and harmless because the
// key is the content digest.
type Store interface {
	// Stat reports the object's metadata, or ErrNotFound.
	Stat(ctx context.Context, key string) (Info, error)

	// Open returns the object's contents, or ErrNotFound. The caller closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)

	// Write stores the object. Overwriting a key with identical content must succeed.
	Write(ctx context.Context, key string, r io.Reader) error

	// Delete removes the object. Deleting a key that is already gone must succeed.
	Delete(ctx context.Context, key string) error
}

// Key builds the canonical key for a digest within a namespace, e.g.
// "inputs/sha256/deadbeef...". The digest is expected in "<algo>:<hex>" form.
func Key(namespace, digest string) (string, error) {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hex == "" {
		return "", fmt.Errorf("malformed digest %q: want <algorithm>:<hex>", digest)
	}
	// Digests come from a CRD field: reject anything that could climb out of the namespace.
	if strings.ContainsAny(algo, "/\\.") || strings.ContainsAny(hex, "/\\.") {
		return "", fmt.Errorf("malformed digest %q: unexpected path characters", digest)
	}
	return namespace + "/" + algo + "/" + hex, nil
}

// MustKey is Key for callers that have already validated the digest. It panics on a malformed
// digest, which is a programming error rather than a runtime condition.
func MustKey(namespace, digest string) string {
	k, err := Key(namespace, digest)
	if err != nil {
		panic(err)
	}
	return k
}
