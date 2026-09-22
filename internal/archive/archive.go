// Package archive materialises a downloaded archive as a directory tree, for BuildKit's --local.
//
// Separate from internal/oci, which unpacks into memory for a layer tarball, so the build path
// does not import internal/oci (ADR 0025). Both share ONE path rule, Mapping (ADR 0023, ADR 0045).
package archive

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Mode is how a fetched blob becomes a directory tree.
//
// A subset of the API's Unpack: single-file modes cannot describe a tree, and the rest are not yet
// implemented. Anything else fails loudly rather than producing an empty context.
type Mode string

const (
	ModeTar   Mode = "tar"
	ModeTarGz Mode = "tar.gz"
)

// maxEntries bounds how many files an archive may contain: the emptyDir bounds bytes, but millions
// of empty files cost inodes.
const maxEntries = 500_000

// Extract writes the archive in r into dest.
//
// strip removes that many leading path components from every entry (zero suits a Flux artifact,
// whose entries are at the root). subpath, when set, then selects one directory and places its
// contents at dest. See Mapping.
func Extract(r io.Reader, mode Mode, dest, subpath string, strip int) error {
	tr, closeFn, err := reader(r, mode)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}

	w := NewWalk(strip, subpath)

	var written int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		written++
		if written > maxEntries {
			return fmt.Errorf("archive has more than %d entries", maxEntries)
		}

		// Refused rather than relativised: filepath.Join would quietly fold it under dest.
		if strings.HasPrefix(path.Clean(hdr.Name), "/") {
			return fmt.Errorf("archive entry %q is an absolute path, which escapes the layout the "+
				"archive describes", hdr.Name)
		}

		place := w.Map(hdr.Name)
		if !place.Selected {
			continue
		}
		if err := writeEntry(tr, hdr, dest, place.Dest); err != nil {
			return err
		}
	}

	return w.Err()
}

func reader(r io.Reader, mode Mode) (*tar.Reader, func(), error) {
	switch mode {
	case ModeTar:
		return tar.NewReader(r), func() {}, nil
	case ModeTarGz:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("reading gzip: %w", err)
		}
		return tar.NewReader(zr), func() { _ = zr.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unpack %q is not an archive mode this build can extract", mode)
	}
}

// writeEntry places one archive entry, refusing anything that would land outside dest.
//
// The traversal check is on the resolved path rather than the name, so any `..` form is caught.
func writeEntry(tr *tar.Reader, hdr *tar.Header, dest, name string) error {
	full := filepath.Join(dest, filepath.FromSlash(name))
	if !strings.HasPrefix(full, filepath.Clean(dest)+string(os.PathSeparator)) {
		return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(full, 0o755)

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
		if err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}
		// Bounded by the header's size, so one entry cannot fill the volume.
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil && err != io.EOF {
			_ = f.Close()
			return fmt.Errorf("writing %s: %w", name, err)
		}
		return f.Close()

	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		// A link leaving the tree is refused; relative links inside it are kept.
		if resolved := path.Join(path.Dir(name), hdr.Linkname); path.IsAbs(hdr.Linkname) ||
			strings.HasPrefix(resolved, "../") || resolved == ".." {
			return fmt.Errorf("archive entry %q is a symlink to %q, which leaves the context",
				hdr.Name, hdr.Linkname)
		}
		return os.Symlink(hdr.Linkname, full)

	default:
		// Devices, fifos, sockets and hard links are skipped: no use in a build context.
		return nil
	}
}
