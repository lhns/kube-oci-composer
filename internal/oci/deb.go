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

// Debian binary package extraction.
//
// A .deb is an ar archive of three members in order: debian-binary, control.tar.* and
// data.tar.*. Only data.tar.* holds the package's files, and it is an ordinary tar, so
// everything after decompression is extractTar's job. ADR 0022 has the reasoning.
//
// ar is parsed here rather than through a dependency: it is a magic string followed by fixed
// 60-byte headers, which is less code than auditing a library for it would be.

const (
	arMagic = "!<arch>\n"

	// Member header layout. The fields this does not read — mtime, uid, gid, mode — sit between
	// the name and the size and are skipped over.
	arHeaderLen = 60
	arNameEnd   = 16 // name occupies [0, arNameEnd)
	arSizeStart = 48
	arSizeEnd   = 58 // the remaining two bytes are the "`\n" trailer
)

// debDecompress wraps r according to a data.tar suffix, and returns a cleanup to call when done.
//
// dpkg picks the compressor, so a caller cannot know which to expect. bz2 has no writer in the
// standard library and is therefore not covered by a test — it is accepted because rejecting a
// valid package would be worse than accepting an untested path through two lines of stdlib.
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

// openDebData advances r to the package's data member and returns a decompressed reader for it.
func openDebData(r io.Reader) (io.Reader, func(), error) {
	noop := func() {}

	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, noop, fmt.Errorf("reading ar magic: %w", err)
	}
	if string(magic) != arMagic {
		return nil, noop, errors.New("not a Debian package: missing ar magic")
	}

	hdr := make([]byte, arHeaderLen)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, noop, errors.New("truncated ar header")
			}
			return nil, noop, fmt.Errorf("reading ar header: %w", err)
		}
		if string(hdr[arSizeEnd:arHeaderLen]) != "`\n" {
			return nil, noop, errors.New("malformed ar header")
		}

		// Names are space-padded and conventionally end in "/". GNU long names (a "//" string
		// table plus "/N" references) are refused rather than guessed at: dpkg does not emit
		// them, and a mis-read member name would produce a silently wrong layer.
		name := strings.TrimRight(strings.TrimSpace(string(hdr[:arNameEnd])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[arSizeStart:arSizeEnd])), 10, 64)
		if err != nil || size < 0 {
			return nil, noop, fmt.Errorf("unreadable size for ar member %q", name)
		}

		if suffix, ok := strings.CutPrefix(name, "data.tar"); ok {
			return debDecompress(io.LimitReader(r, size), suffix)
		}
		// Members are 2-aligned, so an odd-sized one is followed by a padding byte.
		if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
			return nil, noop, fmt.Errorf("skipping ar member %q: %w", name, err)
		}
	}

	return nil, noop, errors.New("Debian package contains no data member")
}

// extractDeb returns the package's payload, rebased under target and filtered by subpath exactly
// as any other tar layer is.
//
// Payload entries are named "./usr/lib/…"; extractTar's path.Clean drops the leading "./", so
// subpath and target are written without one.
func extractDeb(r io.Reader, target, subpath string) ([]tarEntry, error) {
	dr, closeFn, err := openDebData(r)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return extractTar(tar.NewReader(dr), target, subpath)
}
