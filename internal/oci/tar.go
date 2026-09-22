package oci

import (
	"archive/tar"
	"fmt"
	"io"
)

// extractTar reads an archive and rebases its entries under target, taking only entries beneath
// subpath when set. It only translates tar's typeflags; where an entry lands is the collector's
// (extract.go).
func extractTar(tr *tar.Reader, target, subpath string, strip int) ([]tarEntry, error) {
	c := newCollector(target, subpath, strip)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}

		// Names are passed through unchanged: in a tar, a backslash is part of the filename.
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
