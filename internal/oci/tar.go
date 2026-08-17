package oci

import (
	"archive/tar"
	"fmt"
	"io"
)

// Tar extraction, and the tar-shaped modes.
//
// A tar is the format everything else is normalised into: extractDeb hands its payload here, and
// buildLayerTarGz writes one. This file is only the READER — translating tar's typeflags into
// entries. Where an entry is allowed to land belongs to the collector in extract.go.

// extractTar reads an archive and rebases its entries under target.
//
// When subpath is set, only entries beneath it are taken, and the prefix is stripped so the
// selected directory's contents land at target rather than the directory itself. Everything about
// WHERE an entry lands is the collector's; this function only translates tar's typeflags.
func extractTar(tr *tar.Reader, target, subpath string) ([]tarEntry, error) {
	c := newCollector(target, subpath)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}

		// Names are passed through unchanged: a backslash in a tar entry is part of the filename,
		// not a separator, so the normalisation the zip path applies would corrupt it here.
		name, ok, err := c.rebase(hdr.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			c.addDir(name)
		case tar.TypeReg:
			body, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading %q: %w", hdr.Name, err)
			}
			c.addFile(name, normaliseMode(hdr.Mode), body)
		case tar.TypeSymlink:
			c.addSymlink(name, hdr.Linkname)
		default:
			// Devices, fifos and hard links have no place in an artifact layer.
			continue
		}
	}

	return c.done()
}
