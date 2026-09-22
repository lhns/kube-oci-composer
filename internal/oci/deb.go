package oci

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Debian binary package extraction.
//
// A .deb is an ar archive of debian-binary, control.tar.* and data.tar.*. Only data.tar.* holds
// the package's files, as an ordinary tar (ADR 0022). ar is simple enough to parse here rather than
// take a dependency.

const (
	arMagic = "!<arch>\n"

	// Member header layout. mtime, uid, gid and mode, between name and size, are not read.
	arHeaderLen = 60
	arNameEnd   = 16 // name occupies [0, arNameEnd)
	arSizeStart = 48
	arSizeEnd   = 58 // the remaining two bytes are the "`\n" trailer
)

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

		// Names are space-padded and conventionally end in "/". GNU long names are not
		// supported (dpkg does not emit them).
		name := strings.TrimRight(strings.TrimSpace(string(hdr[:arNameEnd])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[arSizeStart:arSizeEnd])), 10, 64)
		if err != nil || size < 0 {
			return nil, noop, fmt.Errorf("unreadable size for ar member %q", name)
		}

		// dpkg picks the compressor; the member name's suffix names the codec, and decompress
		// rejects any it does not implement.
		if suffix, ok := strings.CutPrefix(name, "data.tar"); ok {
			dr, closeFn, err := decompress(io.LimitReader(r, size),
				compression(strings.TrimPrefix(suffix, ".")))
			if err != nil {
				return nil, noop, fmt.Errorf("data member: %w", err)
			}
			return dr, closeFn, nil
		}
		// Members are 2-aligned, so an odd-sized one is followed by a padding byte.
		if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
			return nil, noop, fmt.Errorf("skipping ar member %q: %w", name, err)
		}
	}

	return nil, noop, errors.New("no data member in the Debian package")
}

// extractDeb returns the package's payload, rebased under target and filtered by subpath exactly
// as any other tar layer is.
//
// Payload entries are named "./usr/lib/…"; extractTar's path.Clean drops the leading "./", so
// subpath and target are written without one.
func extractDeb(r io.Reader, target, subpath string, strip int) ([]tarEntry, error) {
	dr, closeFn, err := openDebData(r)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return extractTar(tar.NewReader(dr), target, subpath, strip)
}
