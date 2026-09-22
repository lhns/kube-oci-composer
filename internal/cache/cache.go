// Package cache provides a digest-keyed cache for fetched layer sources.
//
// Two tiers: a local directory, which assembly reads lazily from, and an optional remote Store
// that makes a cold start (restart, reschedule) cheap. Neither is required for correctness, since
// everything can be re-fetched from the origin, so a failure to write either tier is only logged.
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

	// Remote is the durable tier. Optional.
	Remote store.Store

	// Dir is where materialised files are written for the caller to read.
	Dir string
}

// New creates a Cache backed by a local directory, optionally fronting a remote Store.
func New(localDir string, remote store.Store) (*Cache, error) {
	// NewDisk creates localDir.
	local, err := store.NewDisk(localDir)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	return &Cache{Local: local, Remote: remote, Dir: localDir}, nil
}

// Path returns a local file containing the content for digest, fetching it if necessary.
//
// Lookup order is local tier, remote tier, origin. The returned path is owned by the cache and
// must not be removed by the caller.
func (c *Cache) Path(ctx context.Context, digest string, origin Origin) (string, error) {
	logger := log.FromContext(ctx).WithValues("digest", digest)

	key, err := store.Key(store.NamespaceInputs, digest)
	if err != nil {
		return "", err
	}
	local := c.localPath(key)

	// Local hit, trusted without re-hashing: content is verified on the way in.
	if _, err := c.Local.Stat(ctx, key); err == nil {
		logger.V(1).Info("cache hit (local)")
		return local, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		// Not fatal: the remote tier and the origin remain.
		logger.Error(err, "local cache tier is unreadable; falling through")
	}

	// Remote hit, verified on the way down: the tier is shared and may hold bytes this process
	// never verified.
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

	// Only a local-tier failure changes what is returned; a remote failure just loses durability.
	if err := writeFile(ctx, c.Local, key, fetched); err != nil {
		logger.Error(err, "could not write to the local cache; using the fetched copy directly")
		return fetched, nil
	}
	if c.Remote != nil {
		if err := writeFile(ctx, c.Remote, key, fetched); err != nil {
			logger.Error(err, "could not write to the remote cache tier; a restart will re-fetch")
		}
	}

	// Only now is the origin's temp file redundant.
	if err := os.Remove(fetched); err != nil && !os.IsNotExist(err) {
		logger.V(1).Info("could not remove the fetched temp file", "path", fetched, "err", err)
	}
	return local, nil
}

// localPath is where the local disk store keeps a key, so the caller reads the managed file.
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

	// Verified in a temp file first: local hits are trusted without checking.
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
		// Corrupt: drop it rather than serve it or fail on it every lookup.
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
// It owns the file handle so it is always closed (an open handle also blocks deletion on Windows).
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
