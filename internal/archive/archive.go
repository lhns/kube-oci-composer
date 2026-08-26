// Package archive materialises a downloaded archive as a directory tree.
//
// Separate from internal/oci, which unpacks archives into in-memory []tarEntry for a layer tarball;
// a build context has to be real files for BuildKit's `--local` to read. Same input, different
// sink. Keeping it neutral also keeps ADR 0025 true -- internal/oci contributes nothing to a
// build -- where an import from the build path into internal/oci would falsify it.
//
// Different sinks, ONE path rule: Mapping decides where an entry lands and internal/oci calls it
// too. Two copies of that rule is what ADR 0023 forbade and what ADR 0045 was written about.
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
// A smaller set than the API's Unpack: a build context is a tree, so the single-file modes cannot
// describe one, and the rest are not implemented yet rather than refused on principle. Anything
// unknown fails loudly here rather than producing an empty context.
type Mode string

const (
	ModeTar   Mode = "tar"
	ModeTarGz Mode = "tar.gz"
)

// maxEntries bounds how many files an archive may contain. Not a size bound -- the emptyDir has
// its own -- but a bound on what that does not catch: millions of empty files cost inodes, not
// bytes.
const maxEntries = 500_000

// Extract writes the archive in r into dest.
//
// subpath, when set, selects one directory out of the archive and strips its prefix, so dest ends
// up holding that directory's contents rather than the directory itself.
//
// strip removes that many leading path components from every entry, before subpath is considered.
// Zero leaves the archive's own paths alone, which is what a Flux artifact wants: its entries are
// already at the root. See Mapping, which both this and the composer's assembler share.
func Extract(r io.Reader, mode Mode, dest, subpath string, strip int) error {
	tr, closeFn, err := reader(r, mode)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}

	m := NewMapping(strip, subpath)

	var written, survivors int
	var matched bool
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

		// Refused rather than relativised or skipped. filepath.Join folds "/etc/passwd" back under
		// the destination, so it does not escape -- but it lands somewhere the archive did not say,
		// quietly. Same refuse-rather-than-sanitise rule as the ConfigMap source's key check.
		if strings.HasPrefix(path.Clean(hdr.Name), "/") {
			return fmt.Errorf("archive entry %q is an absolute path, which escapes the layout the "+
				"archive describes", hdr.Name)
		}

		place := m.Map(hdr.Name)
		if place.Survived {
			survivors++
		}
		matched = matched || place.InSubpath
		if !place.Selected {
			continue
		}
		if err := writeEntry(tr, hdr, dest, place.Dest); err != nil {
			return err
		}
	}

	// Both of these refuse a silently empty tree, which is the failure that produced this rule:
	// an empty context reads as a broken build somewhere else entirely.
	if strip > 0 && survivors == 0 {
		return fmt.Errorf("stripComponents %d removed every entry in the archive", strip)
	}
	if m.Subpath() != "" && !matched {
		return fmt.Errorf("subpath %q is not present in the archive", subpath)
	}
	return nil
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
// The traversal check is on the resolved path rather than the name, so `..` segments, absolute
// paths and escaping symlinks are caught by one rule.
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
		// Bounded by the header's own size: copying to exhaustion would let one entry fill the
		// volume.
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil && err != io.EOF {
			_ = f.Close()
			return fmt.Errorf("writing %s: %w", name, err)
		}
		return f.Close()

	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		// Refused rather than followed: a build context has no legitimate need for a symlink that
		// leaves the tree. Relative links inside it are kept.
		if resolved := path.Join(path.Dir(name), hdr.Linkname); path.IsAbs(hdr.Linkname) ||
			strings.HasPrefix(resolved, "../") || resolved == ".." {
			return fmt.Errorf("archive entry %q is a symlink to %q, which leaves the context",
				hdr.Name, hdr.Linkname)
		}
		return os.Symlink(hdr.Linkname, full)

	default:
		// Devices, fifos, sockets and hard links: no use in a build context, and each is a way to
		// surprise whatever reads the tree.
		return nil
	}
}
