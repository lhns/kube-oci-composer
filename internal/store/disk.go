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
// The layout is "<root>/<namespace>/<algorithm>/<hex>", which matches what
// go-containerregistry's own disk blob handler uses one level down, so an existing blob
// directory keeps working when it is moved under a namespace.
//
// This is the default backend and the one that needs no configuration. Backed by an emptyDir it
// is a pure cache; backed by a PVC it survives restarts. Neither is required for correctness,
// because everything here can be rebuilt from the spec.
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
// Keys are built by Key from CRD-supplied digests. Validating again here is deliberate
// belt-and-braces: this is the layer where a traversal would actually reach the filesystem, and
// a future caller that constructs a key by hand must not be able to slip past.
func (d *Disk) path(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty key")
	}

	// Validate the LOGICAL key first, before any conversion to an OS path. Doing it the other
	// way round is platform-dependent in a way that quietly disables the check: filepath.IsAbs
	// reports false for "/etc/passwd" on Windows because there is no drive letter, so an
	// absolute-looking key would sail past on one platform and be rejected on the other.
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
	// Belt and braces: even with the checks above, confirm the result is genuinely inside root.
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
	// Note what is NOT done here: the content is not re-hashed. go-containerregistry's disk
	// handler hashes the entire blob on every Stat, and Stat runs on every HEAD and before every
	// GET, so a large artifact gets read twice per pull. Content is verified when it is written;
	// verifying again on every read is a per-pull cost for no additional guarantee.
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
		// On Windows a rename over an existing file fails. The destination is content-addressed,
		// so if it is already there it already holds these exact bytes and there is nothing to do.
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

func (d *Disk) List(_ context.Context, prefix string) ([]Info, error) {
	base, err := d.path(prefix)
	if err != nil {
		return nil, err
	}

	var out []Info
	err = filepath.WalkDir(base, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // an empty namespace lists as empty, not as an error
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp-") {
			// A write in flight. Reporting it would let garbage collection delete a blob that is
			// moments away from being committed and referenced.
			return nil
		}
		fi, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // committed or removed underneath us
			}
			return err
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		out = append(out, Info{
			Key:     filepath.ToSlash(rel),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", prefix, err)
	}
	return out, nil
}
