package oci

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"
)

// Zip archive extraction.
//
// A great deal of software is published only as a .zip — Kafka Connect plugin bundles, JVM
// distributions, Terraform providers, most cross-platform tool releases. Everything downstream of
// reading the entries is shared with the tar path: the collector rebases and filters, and
// buildLayerTarGz sorts and normalises, so a zip and a tarball of the same content produce the same
// layer.
//
// Zip differs from tar in four ways that all have to be handled here rather than downstream, and
// each one is a place where a plausible-looking implementation is quietly wrong:
//
//   - There is no typeflag. A symlink is an ordinary entry whose BODY is the link target, marked
//     only by a mode bit, so reading entries as files turns every symlink into a small text file
//     containing a path.
//   - Entry order is undefined and duplicate names are legal.
//   - Separators are specified as "/" but zips written on Windows do appear with "\".
//   - Unix permissions are present only if the writer recorded them.
//
// This takes an *os.File rather than a plain io.Reader, because the zip format puts its index at
// the end of the file and archive/zip therefore needs io.ReaderAt and a size. That is not a
// constraint in practice — fetched content is always streamed to a real file on disk before it gets
// here, see Fetcher.FetchURL and cache.Cache.Path — and *os.File is also the io.Reader the tar and
// deb readers take, so every extractor has the same shape.

// zipEncryptedFlag is general-purpose bit 0, set when an entry's data is encrypted.
const zipEncryptedFlag = 0x1

// extractZip reads a zip archive and rebases its entries under target, filtered by subpath.
func extractZip(f *os.File, target, subpath string) ([]tarEntry, error) {
	size, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sizing zip: %w", err)
	}
	zr, err := zip.NewReader(f, size.Size())
	if err != nil {
		return nil, fmt.Errorf("reading zip: %w", err)
	}

	c := newCollector(target, subpath)
	// Tracks emitted non-directory destinations, so a duplicate is refused rather than resolved.
	// Directories are excluded because repeats of those are ordinary and the collector absorbs them.
	seen := make(map[string]bool, len(zr.File))

	for _, e := range zr.File {
		if e.Flags&zipEncryptedFlag != 0 {
			// archive/zip supports neither ZipCrypto nor AES and does not check this flag, so
			// reading on would hand back ciphertext and, at best, fail a CRC check later. A layer
			// full of encrypted bytes is worse than a refusal.
			return nil, fmt.Errorf("zip entry %q is encrypted, which is not supported", e.Name)
		}
		if !utf8.ValidString(e.Name) {
			// Names are raw bytes when the UTF-8 flag is clear, historically CP437 or a local
			// codepage. Transcoding would mean carrying a charset table and a second way for two
			// builds to disagree, so this refuses instead. Deterministic either way; just not
			// something to guess at.
			return nil, fmt.Errorf("zip entry name is not valid UTF-8: %q", e.Name)
		}

		// Normalise separators BEFORE the traversal check, and the order is load-bearing: rebase
		// tests for "../", and path.Clean treats "a\b" as a single component, so checking first
		// would let "..\..\etc\passwd" through as one very strange filename instead of refusing it.
		name := strings.ReplaceAll(e.Name, "\\", "/")

		dest, ok, err := c.rebase(name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		mode := e.Mode()

		// Directory detection is deliberately two-sided. The trailing slash is what the format
		// specifies, but some writers set only the directory attribute, and others emit no
		// directory entries at all — which parentDirs already covers.
		if strings.HasSuffix(name, "/") || mode.IsDir() {
			c.addDir(dest)
			continue
		}

		if seen[dest] {
			return nil, fmt.Errorf("zip contains %q more than once", name)
		}
		seen[dest] = true

		switch {
		case mode&fs.ModeSymlink != 0:
			// Before the regular-file case, per the file header: a symlink IS a regular entry to
			// the container, so the wrong branch order fails silently.
			link, err := readZipEntry(e)
			if err != nil {
				return nil, err
			}
			c.addSymlink(dest, string(link))

		case mode.IsRegular():
			body, err := readZipEntry(e)
			if err != nil {
				return nil, err
			}
			c.addFile(dest, normaliseMode(int64(mode.Perm())), body)

		default:
			// Devices, fifos and sockets have no place in an artifact layer, matching extractTar.
			continue
		}
	}

	return c.done()
}

// readZipEntry returns one entry's decompressed contents.
//
// Reading to EOF is what makes archive/zip verify the entry's CRC32, so the error is propagated
// rather than tolerated: it is a free integrity check on every file, which tar does not offer.
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		// Store and Deflate are the only methods archive/zip registers. LZMA, bzip2 and zstd
		// inside a zip all exist in the wild — 7-Zip and some .NET writers emit them — and say so
		// here rather than surfacing a bare "unsupported compression".
		if errors.Is(err, zip.ErrAlgorithm) {
			return nil, fmt.Errorf("zip entry %q uses compression method %d, which is not supported",
				f.Name, f.Method)
		}
		return nil, fmt.Errorf("opening zip entry %q: %w", f.Name, err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading zip entry %q: %w", f.Name, err)
	}
	return body, nil
}
