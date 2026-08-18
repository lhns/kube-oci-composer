package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// Reading one file out of a build context.
//
// The context is a source-controller artifact: a gzipped tar at a digest-addressed URL. The build
// itself never passes through this process — an init container fetches the same URL into the build
// pod — but the FROM check has to happen BEFORE a Job exists, which means the controller needs the
// Dockerfile and only the Dockerfile.
//
// So this streams the tarball and stops at the entry it wants. It never writes to disk (the
// controller's root filesystem is read-only by design) and it bounds what it will read, because the
// URL is trusted to be digest-addressed but not to be small.

const (
	// maxDockerfileBytes bounds one entry. A Dockerfile is kilobytes; anything approaching this is
	// not one, and reading it into a controller shared by every namespace would be a way to make
	// that controller someone else's problem.
	maxDockerfileBytes = 1 << 20

	// maxContextScan bounds how much of the tarball is walked looking for the entry. A context can
	// legitimately be hundreds of megabytes, and the Dockerfile is usually near the front, but
	// "usually" is not a bound.
	maxContextScan = 64 << 20

	fetchTimeout = 2 * time.Minute
)

// FetchDockerfile returns the named file from a gzipped-tar build context.
func FetchDockerfile(ctx context.Context, client *http.Client, url, subpath, dockerfile string) ([]byte, error) {
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

	// source-controller wraps everything in one top-level directory whose name is not predictable,
	// so the match is on the path's tail rather than the whole thing.
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
		if !matchesContextPath(hdr.Name, want) {
			continue
		}

		body, err := io.ReadAll(io.LimitReader(tr, maxDockerfileBytes))
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", want, err)
		}
		return body, nil
	}
}

// matchesContextPath reports whether a tar entry is the file being looked for, ignoring the
// unpredictable top-level directory source-controller adds.
func matchesContextPath(entry, want string) bool {
	clean := strings.TrimPrefix(path.Clean(entry), "./")
	if clean == want {
		return true
	}
	// Strip one leading path segment — the wrapper directory — and compare again.
	if _, rest, ok := strings.Cut(clean, "/"); ok {
		return rest == want
	}
	return false
}
