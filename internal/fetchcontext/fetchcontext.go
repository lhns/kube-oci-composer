// Package fetchcontext materialises an ImageBuild's context inside the build pod.
//
// It replaces a shell script that verified nothing -- not even the Flux artifact digest the
// controller already held -- and that carried a second copy of build.MatchesContextPath which once
// disagreed with the original.
//
// Deliberately no SSRF dial guard, unlike the controller's fetcher. That guard stops the CONTROLLER
// being used as a proxy into the cluster (ADR 0036, threat I6). This runs in the build pod, which
// is about to execute arbitrary code and can already reach anything the pod network allows.
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

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/lhns/kube-oci-composer/internal/archive"
	"github.com/lhns/kube-oci-composer/internal/build"
)

// ExitDigestMismatch is returned when the fetched bytes are not what the spec declared.
//
// A distinct exit code, so the controller reports a mismatch as a spec problem rather than as "the
// build failed" -- which is what keeps FetchSource.Digest's terminality promise true here.
const ExitDigestMismatch = 3

// Options is one context fetch.
type Options struct {
	// Kind is which member of the context union this is: "sourceRef" or "fetch".
	Kind string
	// URL is where the archive comes from.
	URL string
	// Digest is the sha256 the bytes must have. Required for both kinds.
	Digest string
	// Unpack is the archive mode.
	Unpack string
	// Subpath selects one directory out of the archive.
	Subpath string
	// Dest is where the tree is written.
	Dest string
	// Dockerfile is a path inside the context whose FROM lines must all be digest-pinned. Empty
	// means the Dockerfile came from outside the context and the controller already checked it.
	Dockerfile string
}

// Run fetches, verifies and extracts.
//
// VERIFY BEFORE UNPACK, always: checking afterwards would leave files a build might read already
// written, which makes the digest decorative. The body streams to a temporary file while being
// hashed, and nothing is extracted until the hash matches.
func Run(ctx context.Context, opts Options) error {
	// An image is addressed by its own digest: no separate blob to verify, no archive to unpack.
	if opts.Kind == "image" {
		return image(ctx, opts)
	}

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

	// The wrapper strip applies to a Flux artifact and nothing else: source-controller wraps the
	// tree in one directory whose name is unpredictable. In a fetched tarball, `subpath` names it.
	if err := archive.Extract(f, archive.Mode(opts.Unpack), opts.Dest, opts.Subpath,
		opts.Kind == "sourceRef"); err != nil {
		return err
	}
	return checkDockerfile(opts)
}

// checkDockerfile refuses an unpinned FROM in a Dockerfile that came out of the context.
//
// Here rather than only in the controller because for an IMAGE context the controller cannot read
// it cheaply -- that would mean registry credentials for arbitrary user-named repositories in a
// process shared by every namespace. Refusing the combination was the alternative, and it is worse:
// `path` is the default, and it would silently not work with one context kind.
//
// Sound because this is our binary, not user code: it runs before BuildKit, in a container the user
// cannot alter, and nothing writes to the tree between here and buildctl. The cost is that the
// failure arrives as a Job failure rather than Stalled, which is why jobFailureDetail reads init
// containers.
//
// Run for every kind, so the check happens on the bytes that are actually built.
func checkDockerfile(opts Options) error {
	if opts.Dockerfile == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(opts.Dest, filepath.FromSlash(opts.Dockerfile)))
	if err != nil {
		return fmt.Errorf("reading %s from the context: %w", opts.Dockerfile, err)
	}
	defer f.Close()
	return build.CheckPinnedBases(f)
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

	// Hashed as the bytes stream past: nothing is buffered, and the digest covers what was written.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return blob{}, fmt.Errorf("downloading %s: %w", url, err)
	}
	return blob{path: tmp.Name(), digest: "sha256:" + hex.EncodeToString(h.Sum(nil))}, nil
}

// image pulls a digest-pinned image and writes its flattened filesystem into dest.
//
// mutate.Extract rather than untarring each layer, because Extract APPLIES WHITEOUTS. Per-layer
// extraction resurrects files an upper layer deleted, which nobody notices until something reads
// one. Nothing to verify afterwards: the registry cannot serve other bytes under the digest.
func image(ctx context.Context, opts Options) error {
	ref, err := name.NewDigest(opts.URL)
	if err != nil {
		return fmt.Errorf("the image reference %q is not digest-pinned: %w", opts.URL, err)
	}

	remoteOpts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	desc, err := remote.Get(ref, remoteOpts...)
	if err != nil {
		return fmt.Errorf("pulling %s: %w", opts.URL, err)
	}
	// An index names several images and picks none. Refused rather than resolved by this process's
	// platform, as source.PullImage also does: the choice belongs in the spec, not in where the
	// build landed.
	if desc.MediaType.IsIndex() {
		return fmt.Errorf("%s is a multi-platform index: name a platform-specific digest, because "+
			"resolving one here would make the context depend on where the build ran", opts.URL)
	}
	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("reading %s: %w", opts.URL, err)
	}

	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", opts.Dest, err)
	}
	rc := mutate.Extract(img)
	defer rc.Close()

	// The same extractor as every other kind, so traversal, symlink and subpath rules are one
	// implementation rather than three.
	if err := archive.Extract(rc, archive.ModeTar, opts.Dest, opts.Subpath, false); err != nil {
		return err
	}
	return checkDockerfile(opts)
}
