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

// Zip archive extraction. Everything after reading entries is shared with the tar path, so a zip
// and a tarball of the same content produce the same layer. Zip differs from tar in ways handled
// here:
//
//   - There is no typeflag: a symlink is an entry whose BODY is the link target, marked only by a
//     mode bit.
//   - Entry order is undefined and duplicate names are legal.
//   - Zips written on Windows may use "\" as a separator.
//   - Unix permissions are present only if the writer recorded them.
//
// It takes an *os.File because archive/zip needs io.ReaderAt and a size; fetched content is always
// on disk by now.

// zipEncryptedFlag is general-purpose bit 0, set when an entry's data is encrypted.
const zipEncryptedFlag = 0x1

// extractZip reads a zip archive and rebases its entries under target, filtered by subpath.
func extractZip(f *os.File, target, subpath string, strip int) ([]tarEntry, error) {
	size, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sizing zip: %w", err)
	}
	zr, err := zip.NewReader(f, size.Size())
	if err != nil {
		return nil, fmt.Errorf("reading zip: %w", err)
	}

	c := newCollector(target, subpath, strip)
	// Emitted non-directory destinations, so a duplicate is refused rather than resolved.
	seen := make(map[string]bool, len(zr.File))

	for _, e := range zr.File {
		if e.Flags&zipEncryptedFlag != 0 {
			// archive/zip does not check this flag and would hand back ciphertext.
			return nil, fmt.Errorf("zip entry %q is encrypted, which is not supported", e.Name)
		}
		if !utf8.ValidString(e.Name) {
			// Without the UTF-8 flag, names are in some legacy codepage; refused rather than
			// guessed at.
			return nil, fmt.Errorf("zip entry name is not valid UTF-8: %q", e.Name)
		}

		// Normalise separators BEFORE the traversal check, or "..\..\etc\passwd" would pass as a
		// single odd filename.
		name := strings.ReplaceAll(e.Name, "\\", "/")

		dest, ok, err := c.rebase(name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		mode := e.Mode()

		// The format specifies a trailing slash, but some writers set only the directory attribute.
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
			// Before the regular-file case (see the file comment).
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
// Reading to EOF makes archive/zip verify the entry's CRC32, so the error is propagated.
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		// archive/zip registers only Store and Deflate; name the method in the error.
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
