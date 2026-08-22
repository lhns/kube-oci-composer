// Package store provides content-addressed blob storage with interchangeable backends.
//
// One caller today: the layer cache, holding fetched layer sources keyed by their declared digest
// (internal/cache). It is disposable -- anything lost is refetched from upstream -- and it should
// be able to live on a volume or in object storage without the caller knowing which, which is what
// the interface is for.
//
// It once also backed the serving endpoint's blob store, which is why the design is namespaced and
// backend-agnostic rather than simply a directory. That endpoint is gone (ADR 0035); the shape it
// left behind is still the right one for a cache that may be local or remote.
//
// Keys are opaque strings, conventionally "<namespace>/sha256/<hex>". Callers build them with
// Key; nothing in here parses them.
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
// backend-specific error, because callers routinely treat a miss as ordinary control flow: a cache
// miss is not a failure, it is a refetch.
var ErrNotFound = errors.New("not found")

// Namespaces keep unrelated content apart within one backend. A constant rather than a literal
// because a typo would silently create a parallel set of objects that nothing ever reads.
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
// Implementations must be safe for concurrent use. Write must be atomic in the sense that a
// reader never observes a partially written object: either the whole thing is there or the key
// is absent. Two writers racing on the same key is fine and expected, because the key is the
// content digest and both are writing identical bytes.
type Store interface {
	// Stat reports the object's metadata, or ErrNotFound.
	Stat(ctx context.Context, key string) (Info, error)

	// Open returns the object's contents, or ErrNotFound. The caller closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)

	// Write stores the object. Overwriting an existing key with identical content is a no-op
	// that must succeed, since that is the normal outcome of two reconciles racing.
	Write(ctx context.Context, key string, r io.Reader) error

	// Delete removes the object. Deleting a key that is already gone must succeed: garbage
	// collection is not a transaction, and a concurrent sweep must not fail the whole cycle.
	Delete(ctx context.Context, key string) error

	// List returns every object under prefix. Used only by garbage collection, so backends may
	// make it slow, but it must be complete — a partial listing would make the collector treat
	// live objects as absent and, on the next pass, unreferenced ones as still present.
	List(ctx context.Context, prefix string) ([]Info, error)
}

// Key builds the canonical key for a digest within a namespace, e.g.
// "blobs/sha256/deadbeef...". The digest is expected in "<algo>:<hex>" form.
func Key(namespace, digest string) (string, error) {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hex == "" {
		return "", fmt.Errorf("malformed digest %q: want <algorithm>:<hex>", digest)
	}
	// Reject anything that could climb out of the namespace or, on a disk backend, out of the
	// directory. These values come from a CRD field, so they are user input.
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
