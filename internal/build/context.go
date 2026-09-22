package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/lhns/kube-oci-composer/internal/archive"
)

// Reading one file out of a build context. The FROM check runs before a Job exists, so the
// controller streams the context tarball until it finds the Dockerfile: never to disk (read-only
// root filesystem), and bounded, since the URL is digest-addressed but not necessarily small.

const (
	// maxDockerfileBytes bounds one entry; a Dockerfile is kilobytes.
	maxDockerfileBytes = 1 << 20

	// maxContextScan bounds how much of the tarball is walked looking for the entry.
	maxContextScan = 64 << 20

	fetchTimeout = 2 * time.Minute
)

// FetchDockerfile returns the named file from a gzipped-tar build context.
func FetchDockerfile(ctx context.Context, client *http.Client, url, subpath, dockerfile string,
	strip int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the build context: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the build context: %s returned %s", url, resp.Status)
	}

	zr, err := gzip.NewReader(io.LimitReader(resp.Body, maxContextScan))
	if err != nil {
		return nil, fmt.Errorf("reading the build context: %w", err)
	}
	defer zr.Close()

	// Matched through the extractor's own path mapping; see matchesContextPath.
	want := path.Join(subpath, dockerfile)

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%q is not present in the build context", want)
		}
		if err != nil {
			return nil, fmt.Errorf("reading the build context: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !matchesContextPath(hdr.Name, want, strip) {
			continue
		}

		body, err := io.ReadAll(io.LimitReader(tr, maxDockerfileBytes))
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", want, err)
		}
		return body, nil
	}
}

// matchesContextPath reports whether an archive entry is the file being looked for, using the same
// mapping as the extractor so the file checked is the file built. ADR 0045.
func matchesContextPath(entry, want string, strip int) bool {
	place := archive.NewMapping(strip, "").Map(entry)
	return place.Selected && place.Dest == want
}
