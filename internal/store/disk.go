package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Disk stores objects as files under a root directory.
//
// The layout is "<root>/<namespace>/<algorithm>/<hex>". This is the default backend and needs no
// configuration.
type Disk struct {
	root string
}

var _ Store = (*Disk)(nil)

// NewDisk creates a Disk rooted at dir, creating it if necessary.
func NewDisk(dir string) (*Disk, error) {
	if dir == "" {
		return nil, errors.New("disk store: directory must not be empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("disk store: resolving %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("disk store: creating %q: %w", abs, err)
	}
	return &Disk{root: abs}, nil
}

// path maps a key to a filesystem path, refusing anything that escapes the root.
//
// Key already validates CRD-supplied digests; this re-checks at the layer that touches the
// filesystem, in case a caller builds a key by hand.
func (d *Disk) path(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty key")
	}

	// Validate the LOGICAL key before converting it to an OS path: filepath.IsAbs reports false
	// for "/etc/passwd" on Windows.
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) {
		return "", fmt.Errorf("key %q must be relative", key)
	}
	if filepath.VolumeName(key) != "" {
		return "", fmt.Errorf("key %q must not name a volume", key)
	}
	for _, elem := range strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '\\' }) {
		if elem == ".." || elem == "." {
			return "", fmt.Errorf("key %q escapes the store root", key)
		}
	}

	full := filepath.Join(d.root, filepath.FromSlash(key))
	// Confirm the result is inside root regardless.
	if !strings.HasPrefix(full, d.root+string(filepath.Separator)) {
		return "", fmt.Errorf("key %q escapes the store root", key)
	}
	return full, nil
}

func (d *Disk) Stat(_ context.Context, key string) (Info, error) {
	p, err := d.path(key)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("stat %s: %w", key, err)
	}
	// Not re-hashed: content is verified when written.
	return Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (d *Disk) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	return f, nil
}

// Write stores the object via a temporary file and a rename, so a reader never sees a partial
// object and two concurrent writers of identical content cannot corrupt each other.
func (d *Disk) Write(_ context.Context, key string, r io.Reader) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// The temp file goes in the destination directory so the rename stays on one filesystem.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", key, err)
	}

	if err := os.Rename(tmpName, p); err != nil {
		// On Windows a rename over an existing file fails. The key is content-addressed, so an
		// existing destination already holds these bytes.
		if _, statErr := os.Stat(p); statErr == nil {
			return nil
		}
		return fmt.Errorf("committing %s: %w", key, err)
	}
	return nil
}

func (d *Disk) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}
