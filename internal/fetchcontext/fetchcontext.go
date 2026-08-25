// Package fetchcontext materialises an ImageBuild's context inside the build pod.
//
// It replaces a shell script that did `wget -qO- URL | tar -xzf -`. That script could not grow into
// this: it verified NOTHING, not even the Flux artifact digest the controller already held; busybox
// ships no unzip; and its wrapper-stripping rule was a second copy of build.MatchesContextPath that
// once disagreed with it, so an unpinned FROM was correctly refused and every build that passed the
// check then failed inside BuildKit. A rule that exists twice, in two languages, is a rule that
// eventually differs.
//
// Deliberately NO SSRF dial guard, unlike the controller's fetcher. The guard exists to stop the
// CONTROLLER being used as a proxy into the cluster (ADR 0036, threat I6). This runs in the build
// pod, which is about to execute arbitrary code from a Dockerfile and can already reach anything the
// pod network allows -- a guard here would block legitimate in-cluster registries while preventing
// nothing.
package fetchcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lhns/kube-oci-composer/internal/archive"
)

// ExitDigestMismatch is returned when the fetched bytes are not what the spec declared.
//
// A distinct exit code rather than a generic failure, so the controller can report a mismatch as a
// spec problem rather than as "the build failed". Without it FetchSource.Digest's promise -- that a
// mismatch is terminal -- would be quietly weaker on this kind than on the composer.
const ExitDigestMismatch = 3

// Options is one context fetch.
type Options struct {
	// Kind is which member of the context union this is: "sourceRef" or "fetch".
	Kind string
	// URL is where the archive comes from.
	URL string
	// Digest is the sha256 the bytes must have. Required for both kinds -- the Flux path had none
	// before and verified nothing, which was a gap rather than a design.
	Digest string
	// Unpack is the archive mode.
	Unpack string
	// Subpath selects one directory out of the archive.
	Subpath string
	// Dest is where the tree is written.
	Dest string
}

// Run fetches, verifies and extracts.
//
// VERIFY BEFORE UNPACK, always. Checking afterwards would mean an archive with a wrong digest had
// already written files that a build might read, which makes the digest decorative. So the body is
// streamed to a temporary file while being hashed, and nothing is extracted until the hash matches.
func Run(ctx context.Context, opts Options) error {
	if opts.Digest == "" {
		return fmt.Errorf("no digest for the %s context: refusing to build content nothing addresses", opts.Kind)
	}

	blob, err := download(ctx, opts.URL, opts.Dest)
	if err != nil {
		return err
	}
	defer os.Remove(blob.path)

	if blob.digest != opts.Digest {
		return &MismatchError{URL: opts.URL, Want: opts.Digest, Got: blob.digest}
	}

	f, err := os.Open(blob.path)
	if err != nil {
		return err
	}
	defer f.Close()

	// The wrapper strip applies to a Flux artifact and to nothing else: source-controller wraps the
	// tree in one directory whose name is unpredictable. A fetched tarball is whatever the publisher
	// made it, and `subpath` is how a version-named wrapper is named there.
	return archive.Extract(f, archive.Mode(opts.Unpack), opts.Dest, opts.Subpath, opts.Kind == "sourceRef")
}

// MismatchError is a digest mismatch, distinguishable so the caller can exit with
// ExitDigestMismatch.
type MismatchError struct {
	URL, Want, Got string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s served content with digest %s, but the spec declares %s", e.URL, e.Got, e.Want)
}

type blob struct {
	path   string
	digest string
}

func download(ctx context.Context, url, dest string) (blob, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return blob{}, fmt.Errorf("creating %s: %w", dest, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "context-*.blob")
	if err != nil {
		return blob{}, fmt.Errorf("staging the download: %w", err)
	}
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return blob{}, err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return blob{}, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return blob{}, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}

	// Hashed as the bytes stream past, so nothing is buffered and the digest is over exactly what
	// was written.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return blob{}, fmt.Errorf("downloading %s: %w", url, err)
	}
	return blob{path: tmp.Name(), digest: "sha256:" + hex.EncodeToString(h.Sum(nil))}, nil
}
