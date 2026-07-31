// Package store provides content-addressed blob storage with interchangeable backends.
//
// Two things in this controller need to keep bytes around, and they want the same operations:
//
//   - the input cache, holding fetched layer sources keyed by their declared digest;
//   - the served blob store, holding assembled layers and configs for workloads to pull.
//
// Both are content-addressed, both are disposable in the sense that anything lost can be rebuilt
// from the spec, and both should be able to live on a volume or in object storage without either
// caller knowing which. Hence one interface and two key namespaces rather than two subsystems.
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
// backend-specific error, because callers routinely treat a miss as ordinary control flow — a
// cache miss is not a failure, and the serving endpoint has to turn it into a 404 rather than a
// 500.
var ErrNotFound = errors.New("not found")

// Namespaces. Kept as constants because a typo would silently create a parallel universe of
// objects that nothing reads and garbage collection would then delete as unreferenced.
const (
	// NamespaceInputs holds fetched layer sources, keyed by the digest declared in the spec.
	NamespaceInputs = "inputs"
	// NamespaceBlobs holds assembled layers and configs, served to workloads.
	NamespaceBlobs = "blobs"
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

// Presigner is an optional extension for backends that can hand out a URL a client may fetch
// directly. When available, the serving endpoint can redirect blob pulls straight to object
// storage instead of streaming every byte through the controller.
type Presigner interface {
	// Presign returns a temporary URL for reading key, or ErrNotFound.
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
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
