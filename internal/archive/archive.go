// Package archive materialises a downloaded archive as a directory tree.
//
// Separate from internal/oci, which unpacks archives into in-memory []tarEntry for a layer tarball;
// a build context has to be real files for BuildKit's `--local` to read. Same input, different
// sink. Keeping it neutral also keeps ADR 0025 true -- internal/oci contributes nothing to a
// build -- where an import from the build path into internal/oci would falsify it.
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
// stripWrapper handles what source-controller does and nothing else does: an artifact wraps the
// tree in one top-level directory whose name is unpredictable. It strips a segment only when there
// really is one such directory, so an archive whose files sit at the root still arrives intact.
// Deliberately the same rule as build.MatchesContextPath, which it once disagreed with.
func Extract(r io.Reader, mode Mode, dest, subpath string, stripWrapper bool) error {
	tr, closeFn, err := reader(r, mode)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}

	want := strings.Trim(path.Clean("/"+subpath), "/")
	if want == "." {
		want = ""
	}

	var written int
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

		name, selected, inSubpath := target(hdr.Name, want, stripWrapper)
		matched = matched || inSubpath
		if !selected {
			continue
		}
		if err := writeEntry(tr, hdr, dest, name); err != nil {
			return err
		}
	}

	// A subpath that selected nothing is a typo, and staying silent about it hands BuildKit an
	// empty context instead. Same refusal as the composer's collector.done.
	if want != "" && !matched {
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

// target maps an archive entry to its path under dest.
//
// selected is whether to write it; inSubpath is whether it lies within subpath at all. They differ
// for the subpath directory entry itself, which proves the subpath exists but contributes no file.
func target(entry, subpath string, stripWrapper bool) (name string, selected, inSubpath bool) {
	clean := strings.TrimPrefix(path.Clean(entry), "./")
	if clean == "." || clean == "/" {
		return "", false, false
	}
	if stripWrapper {
		_, rest, ok := strings.Cut(clean, "/")
		if !ok {
			// The wrapper directory itself. It becomes dest, so it contributes no entry.
			return "", false, false
		}
		clean = rest
	}
	if subpath == "" {
		return clean, clean != "", true
	}
	if clean == subpath {
		return "", false, true
	}
	rest, ok := strings.CutPrefix(clean, subpath+"/")
	if !ok || rest == "" {
		return "", false, false
	}
	return rest, true, true
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
