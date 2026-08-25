// Package archive materialises a downloaded archive as a directory tree.
//
// Deliberately separate from internal/oci, which also unpacks archives and cannot be reused here.
// Its extractors return []tarEntry destined for a layer tarball, entirely in memory; a build
// context has to be real files on a real filesystem for BuildKit's `--local` to read. Same input,
// different sink, so the shared part is the archive reading and not the unpacking.
//
// That separation also keeps ADR 0025 true: internal/oci contributes nothing to a build. A neutral
// package both sides may depend on is the honest version of that claim, where an import from the
// build path into internal/oci would quietly falsify it.
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
// A deliberately smaller set than the API's Unpack: a build context is a TREE, so the single-file
// modes cannot describe one, and the archive modes beyond these are not implemented yet rather than
// refused on principle. The caller validates; this refuses anything it does not know rather than
// guessing, so an unimplemented mode fails loudly at the extraction rather than producing an empty
// context that looks like a build with nothing in it.
type Mode string

const (
	ModeTar   Mode = "tar"
	ModeTarGz Mode = "tar.gz"
)

// maxEntries bounds how many files an archive may contain.
//
// Not a size bound -- the disk is an emptyDir with its own limit -- but a bound on the thing an
// emptyDir limit does not catch: an archive of a hundred million empty files costs inodes and time
// rather than bytes.
const maxEntries = 500_000

// Extract writes the archive in r into dest.
//
// subpath, when set, selects one directory out of the archive and strips its prefix, so dest ends
// up holding that directory's contents rather than the directory itself.
//
// stripWrapper handles the thing source-controller does and nothing else does: an artifact wraps
// the tree in one top-level directory whose name is unpredictable. Only ever strips a segment when
// there really is exactly one such directory, so an archive whose files sit at the root still
// arrives intact rather than being silently emptied. Deliberately the same rule as
// build.MatchesContextPath -- when the two disagreed, an unpinned FROM was correctly refused and
// every build that passed the check then failed inside BuildKit.
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

		// An absolute entry is REFUSED rather than relativised, and rather than skipped.
		// filepath.Join would fold "/etc/passwd" back under the destination, so it does not
		// escape -- but it would land somewhere the archive did not say, and quietly. Same rule as
		// the ConfigMap source's refusal of keys containing separators: refusing rather than
		// sanitising means a surprising name never silently arrives somewhere unexpected. An error
		// rather than a skip, because dropping content without saying so is the other half of the
		// same problem.
		if strings.HasPrefix(path.Clean(hdr.Name), "/") {
			return fmt.Errorf("archive entry %q is an absolute path, which escapes the layout the "+
				"archive describes", hdr.Name)
		}

		name, ok := target(hdr.Name, want, stripWrapper)
		if !ok {
			continue
		}
		if err := writeEntry(tr, hdr, dest, name); err != nil {
			return err
		}
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

// target maps an archive entry to its path under dest, or reports that it is not wanted.
func target(entry, subpath string, stripWrapper bool) (string, bool) {
	clean := strings.TrimPrefix(path.Clean(entry), "./")
	if clean == "." || clean == "/" {
		return "", false
	}
	if stripWrapper {
		if _, rest, ok := strings.Cut(clean, "/"); ok {
			clean = rest
		} else {
			// The wrapper directory itself. It becomes dest, so it contributes no entry.
			return "", false
		}
	}
	if subpath == "" {
		return clean, clean != ""
	}
	if clean == subpath {
		return "", false
	}
	rest, ok := strings.CutPrefix(clean, subpath+"/")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// writeEntry places one archive entry, refusing anything that would land outside dest.
//
// The traversal check is on the RESOLVED path rather than the name, so `..` segments, absolute
// paths and a symlink pointing out of the tree are all caught by the same rule. A build context is
// attacker-influenced by definition -- it is whatever the referenced archive contains.
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
		// Copy bounded by the header's own size: a tar whose body outruns its header is malformed,
		// and copying to exhaustion would let one entry fill the volume.
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil && err != io.EOF {
			_ = f.Close()
			return fmt.Errorf("writing %s: %w", name, err)
		}
		return f.Close()

	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		// Refused rather than followed: a symlink to /etc or to the host's filesystem is how an
		// archive reaches outside the context, and a build context has no legitimate need for one
		// that leaves the tree. Relative links inside it are kept.
		if resolved := path.Join(path.Dir(name), hdr.Linkname); path.IsAbs(hdr.Linkname) ||
			strings.HasPrefix(resolved, "../") || resolved == ".." {
			return fmt.Errorf("archive entry %q is a symlink to %q, which leaves the context",
				hdr.Name, hdr.Linkname)
		}
		return os.Symlink(hdr.Linkname, full)

	default:
		// Devices, fifos, sockets and hard links. A build context has no use for any of them, and
		// each is a way to surprise whatever reads the tree afterwards.
		return nil
	}
}
