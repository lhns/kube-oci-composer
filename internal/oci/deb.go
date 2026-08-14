package oci

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// A Debian binary package is an ar archive holding exactly three members, in order:
// debian-binary, control.tar.*, data.tar.*. Only the last one carries files, so that is all this
// reads: the control member describes an installation that is not happening here, and no
// maintainer script is ever executed. See ADR 0022.
//
// The ar container is parsed inline rather than through a dependency. The format is a magic
// string and a run of fixed 60-byte headers, and a third-party parser would be more code to audit
// than the fifty lines below.
const arMagic = "!<arch>\n"

// arHeader is the fixed-width member header. Only the fields this needs are named; the rest
// (mtime, uid, gid, mode) describe a filesystem the artifact will not inherit.
const (
	arHeaderSize    = 60
	arNameOffset    = 0
	arNameEnd       = 16
	arSizeOffset    = 48
	arSizeEnd       = 58
	arTrailerOffset = 58
)

// debDataMember reports whether name is the package payload, and returns the compression suffix.
//
// Debian has shipped data as gz, bz2, xz and zst over the years, and which one a package uses is
// the packager's choice rather than anything the caller can know. All four are handled so that
// `unpack: deb` means "a .deb" and not "a .deb that happens to be compressed the way we expected".
func debDataMember(name string) (suffix string, ok bool) {
	if !strings.HasPrefix(name, "data.tar") {
		return "", false
	}
	return strings.TrimPrefix(name, "data.tar"), true
}

// debDecompress wraps r according to the data member's suffix.
func debDecompress(r io.Reader, suffix string) (io.Reader, func(), error) {
	noop := func() {}
	switch suffix {
	case "":
		return r, noop, nil
	case ".gz":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, noop, fmt.Errorf("reading gzip: %w", err)
		}
		return zr, func() { _ = zr.Close() }, nil
	case ".xz":
		zr, err := xz.NewReader(r)
		if err != nil {
			return nil, noop, fmt.Errorf("reading xz: %w", err)
		}
		return zr, noop, nil
	case ".zst":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, noop, fmt.Errorf("reading zstd: %w", err)
		}
		return zr, zr.Close, nil
	case ".bz2":
		return bzip2.NewReader(r), noop, nil
	default:
		return nil, noop, fmt.Errorf("unsupported compression %q in data member", suffix)
	}
}

// extractDeb reads a .deb and returns its payload rebased under target, honouring subpath.
//
// Entries in a Debian payload are prefixed "./"; that is stripped here so subpath and target can
// be written the way they read in the package listing ("usr/lib/..."), not with a leading dot.
func extractDeb(r io.Reader, target, subpath string) ([]tarEntry, error) {
	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("reading ar magic: %w", err)
	}
	if string(magic) != arMagic {
		return nil, errors.New("not a Debian package: missing ar magic")
	}

	hdr := make([]byte, arHeaderSize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("truncated ar header")
			}
			return nil, fmt.Errorf("reading ar header: %w", err)
		}
		if string(hdr[arTrailerOffset:arHeaderSize]) != "`\n" {
			return nil, errors.New("malformed ar header")
		}

		// Names are space-padded and conventionally end in "/". GNU long names (a "//" string
		// table plus "/N" references) are not handled: dpkg never emits them, and silently
		// mis-reading a name would be worse than refusing one.
		name := strings.TrimRight(strings.TrimSpace(string(hdr[arNameOffset:arNameEnd])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[arSizeOffset:arSizeEnd])), 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("unreadable size for ar member %q", name)
		}

		suffix, isData := debDataMember(name)
		if !isData {
			// Skip the member and the padding byte that keeps members 2-aligned.
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return nil, fmt.Errorf("skipping ar member %q: %w", name, err)
			}
			continue
		}

		dr, closeFn, err := debDecompress(io.LimitReader(r, size), suffix)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return extractTar(tar.NewReader(dr), target, subpath)
	}

	return nil, errors.New("Debian package contains no data member")
}
