package oci

import (
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Stream decompression, shared by every unpack mode with a compressed payload (and a .deb's data
// member).

// compression names a stream codec. Values are the suffixes the API's unpack modes use.
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
// The returned cleanup MUST be called (the zstd reader holds goroutines); it is never nil, so
// callers can defer it unconditionally. bz2 has no stdlib writer, so it has no round-trip test.
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

// tarCompressions is the ONE list of unpack modes that are a tar under a codec, and which codec.
// collectEntries dispatches by lookup here, so the modes and codecs cannot disagree.
var tarCompressions = map[UnpackMode]compression{
	UnpackTar:     compNone,
	UnpackTarGz:   compGzip,
	UnpackTarXz:   compXz,
	UnpackTarZstd: compZstd,
	UnpackTarBz2:  compBzip2,
}
