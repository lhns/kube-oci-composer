package oci

import (
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Stream decompression, shared by every unpack mode that has a compressed payload.
//
// This started inside the deb reader, because a .deb's data member can arrive under any of these
// and dpkg picks which. The same table serves `unpack: tar.gz` and its siblings, so it lives here
// rather than there — a codec is not a property of the container that happens to need it.

// compression names a stream codec.
//
// Values are the suffixes the API's unpack modes already use, so a mode maps onto a codec by
// inspection rather than through a translation table nobody can check.
type compression string

const (
	compNone  compression = ""
	compGzip  compression = "gz"
	compXz    compression = "xz"
	compZstd  compression = "zst"
	compBzip2 compression = "bz2"
)

// decompress wraps r in the named codec.
//
// The returned cleanup MUST be called: the zstd reader holds goroutines and buffers, and dropping
// it leaks both. compNone returns r with a no-op cleanup so that every caller can defer
// unconditionally rather than guarding the call.
//
// bz2 has no writer in the standard library and is therefore not covered by a round-trip test — it
// is accepted because rejecting a valid archive would be worse than accepting an untested path
// through two lines of stdlib.
func decompress(r io.Reader, c compression) (io.Reader, func(), error) {
	noop := func() {}
	switch c {
	case compNone:
		return r, noop, nil
	case compGzip:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, noop, fmt.Errorf("reading gzip: %w", err)
		}
		return zr, func() { _ = zr.Close() }, nil
	case compXz:
		zr, err := xz.NewReader(r)
		if err != nil {
			return nil, noop, fmt.Errorf("reading xz: %w", err)
		}
		return zr, noop, nil
	case compZstd:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, noop, fmt.Errorf("reading zstd: %w", err)
		}
		return zr, zr.Close, nil
	case compBzip2:
		return bzip2.NewReader(r), noop, nil
	default:
		return nil, noop, fmt.Errorf("unsupported compression %q", string(c))
	}
}

// tarCompression maps a tar-family unpack mode onto the codec wrapping its tar.
//
// Total by construction: collectEntries' switch is the only caller and lists exactly these modes,
// so the default is unreachable rather than a silent fallback to "uncompressed" — which would
// hand a compressed stream to the tar reader and report it as a corrupt archive.
func tarCompression(m UnpackMode) (compression, error) {
	switch m {
	case UnpackTar:
		return compNone, nil
	case UnpackTarGz:
		return compGzip, nil
	case UnpackTarXz:
		return compXz, nil
	case UnpackTarZstd:
		return compZstd, nil
	case UnpackTarBz2:
		return compBzip2, nil
	default:
		return compNone, fmt.Errorf("unpack mode %q is not a compressed tar", string(m))
	}
}
