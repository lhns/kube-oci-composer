// Package cache provides a digest-keyed cache for fetched layer sources.
//
// The controller needs its inputs as local files, because assembly reads them lazily from disk.
// It also wants them to survive a restart, a reschedule onto another node, and to be shared
// between compositions that happen to use the same layer. Those pull in opposite directions:
// object storage gives durability but not a path, a local directory gives a path but not
// durability.
//
// Hence two tiers. A local directory is always present and is what assembly reads from; an
// optional remote Store sits behind it and is what makes a cold start cheap. Neither is required
// for correctness — everything here can be re-fetched from the origin — which is why a failure
// to write to either tier is logged and ignored rather than failing the build.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lhns/kube-oci-composer/internal/store"
)

// Origin fetches content that is in neither tier. It returns the path to a local file the cache
// takes ownership of, and must verify the content against the requested digest itself.
type Origin func(ctx context.Context, digest string) (path string, err error)

// Cache resolves a digest to a local file path.
type Cache struct {
	// Local is the tier assembly reads from. Required.
	Local *store.Disk

	// Remote is the durable tier. Optional; without it the cache is process-local and a restart
	// means re-fetching from the origin.
	Remote store.Store

	// Dir is where materialised files are written for the caller to read.
	Dir string
}

// New creates a Cache backed by a local directory, optionally fronting a remote Store.
func New(localDir string, remote store.Store) (*Cache, error) {
	local, err := store.NewDisk(localDir)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		return nil, fmt.Errorf("cache: creating %q: %w", localDir, err)
	}
	return &Cache{Local: local, Remote: remote, Dir: localDir}, nil
}

// Path returns a local file containing the content for digest, fetching it if necessary.
//
// The lookup order is by cost: the local tier, then the remote tier, then the origin. The
// returned path is owned by the cache and must not be removed by the caller — that is the point,
// since removing it would defeat the next lookup.
func (c *Cache) Path(ctx context.Context, digest string, origin Origin) (string, error) {
	logger := log.FromContext(ctx).WithValues("digest", digest)

	key, err := store.Key(store.NamespaceInputs, digest)
	if err != nil {
		return "", err
	}
	local := c.localPath(key)

	// Local hit. Trusted without re-hashing: content is verified on the way in, and re-verifying
	// on every reconcile would reintroduce a per-build cost over the whole artifact — a smaller
	// version of exactly the problem this cache exists to remove.
	if _, err := c.Local.Stat(ctx, key); err == nil {
		logger.V(1).Info("cache hit (local)")
		return local, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		// A broken local tier is worth reporting, but it is not fatal: the remote tier and the
		// origin are both still available.
		logger.Error(err, "local cache tier is unreadable; falling through")
	}

	// Remote hit. Verified on the way down, because the remote tier is shared and durable: it
	// may have been written by a different version of this controller, or corrupted in transit.
	if c.Remote != nil {
		if err := c.pullFromRemote(ctx, key, digest); err == nil {
			logger.V(1).Info("cache hit (remote)")
			return local, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			logger.Error(err, "could not read from the remote cache tier; falling through to origin")
		}
	}

	// Miss. The origin verifies the digest, so nothing unverified is ever admitted to either tier.
	logger.V(1).Info("cache miss; fetching from origin")
	fetched, err := origin(ctx, digest)
	if err != nil {
		return "", err
	}

	// The local tier is what the caller reads from, so its failure is the only one that changes
	// what is returned. A remote failure means the object store is unreachable; this build and
	// every other build on this node still get their content, and only durability across a
	// restart is lost. Treating that as fatal would turn an optimisation into a dependency.
	if err := writeFile(ctx, c.Local, key, fetched); err != nil {
		logger.Error(err, "could not write to the local cache; using the fetched copy directly")
		return fetched, nil
	}
	if c.Remote != nil {
		if err := writeFile(ctx, c.Remote, key, fetched); err != nil {
			logger.Error(err, "could not write to the remote cache tier; a restart will re-fetch")
		}
	}

	// Only now is the origin's temp file redundant. Removing it before this point would hand the
	// caller a path to a file that no longer exists.
	if err := os.Remove(fetched); err != nil && !os.IsNotExist(err) {
		logger.V(1).Info("could not remove the fetched temp file", "path", fetched, "err", err)
	}
	return local, nil
}

// localPath is where a key materialises on disk. It mirrors the disk store's own layout so the
// file the caller reads is the same file the store manages.
func (c *Cache) localPath(key string) string {
	return filepath.Join(c.Dir, filepath.FromSlash(key))
}

// pullFromRemote streams an object down into the local tier, verifying as it goes.
func (c *Cache) pullFromRemote(ctx context.Context, key, digest string) error {
	rc, err := c.Remote.Open(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	// Verified into a temporary file first. Writing straight into the local tier would publish
	// unverified bytes under a content-addressed key, which every later local hit then trusts
	// without checking.
	tmp, err := os.CreateTemp(c.Dir, ".pull-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), rc); err != nil {
		return fmt.Errorf("reading from remote cache: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(hasher.Sum(nil)); got != digest {
		// The remote tier is content-addressed, so this means it is corrupt or was written by
		// something that did not verify. Drop the object rather than serving it or looping on it.
		if delErr := c.Remote.Delete(ctx, key); delErr != nil {
			return fmt.Errorf("remote cache holds %s under key for %s, and removing it failed: %w",
				got, digest, delErr)
		}
		return fmt.Errorf("remote cache held %s under the key for %s; removed it", got, digest)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	f, err := os.Open(tmpName)
	if err != nil {
		return fmt.Errorf("reopening temp file: %w", err)
	}
	defer f.Close()

	if err := c.Local.Write(ctx, key, f); err != nil {
		return fmt.Errorf("writing to local cache: %w", err)
	}
	return nil
}

// writeFile copies a local file into a Store under key.
//
// The handle is opened and closed here rather than by the caller. Handing Store.Write a reader
// the caller forgot to close leaks a descriptor on every cache miss, and on Windows it also
// prevents the file from being deleted afterwards — which is how this was found.
func writeFile(ctx context.Context, s store.Store, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	if err := s.Write(ctx, key, f); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}

// Referenced reports whether digest is present in the local tier. Used by tests and by garbage
// collection reporting.
func (c *Cache) Referenced(ctx context.Context, digest string) bool {
	key, err := store.Key(store.NamespaceInputs, digest)
	if err != nil {
		return false
	}
	_, err = c.Local.Stat(ctx, key)
	return err == nil
}
