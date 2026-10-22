// Package fetchcontext materialises an ImageBuild's context inside the build pod.
//
// No SSRF dial guard, unlike the controller's fetcher: that guard stops the controller being used
// as a proxy (ADR 0036, threat I6), while this runs in a build pod that is about to execute
// arbitrary code with the pod network's reach anyway.
package fetchcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// ExitDigestMismatch is the exit code for fetched bytes that do not match the declared digest, so
// the controller can tell a spec problem from a failed build.
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
	// Subpath selects one directory out of the archive, after Strip has been applied.
	Subpath string
	// Strip is how many leading path components to remove from every entry. Zero for a Flux
	// artifact.
	Strip int
	// Dest is where the tree is written.
	Dest string
	// Dockerfile is a path inside the context whose FROM lines must all be digest-pinned. Empty
	// means the Dockerfile came from outside the context and the controller already checked it.
	Dockerfile string
	// Token authenticates this build to the controller's context endpoint. Empty when the URL
	// needs no credential.
	Token string
}

// Run fetches, verifies and extracts. Always verify before unpack, or a build could read files the
// digest never vouched for: the body is hashed into a staged file and extracted only on a match.
func Run(ctx context.Context, opts Options) error {
	// An image is addressed by its own digest: no separate blob to verify, no archive to unpack.
	if opts.Kind == "image" {
		return image(ctx, opts)
	}

	if opts.Digest == "" {
		return fmt.Errorf("no digest for the %s context: refusing to build content nothing addresses", opts.Kind)
	}

	blob, err := download(ctx, opts.URL, opts.Dest, opts.Token)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(blob.stage) }()

	if blob.digest != opts.Digest {
		return &MismatchError{URL: opts.URL, Want: opts.Digest, Got: blob.digest}
	}

	f, err := os.Open(blob.path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Strip depth comes from the spec, never from the kind: source-controller does not wrap its
	// tree. ADR 0045.
	if err := archive.Extract(f, archive.Mode(opts.Unpack), opts.Dest, opts.Subpath,
		opts.Strip); err != nil {
		return err
	}
	return checkDockerfile(opts)
}

// checkDockerfile refuses an unpinned FROM in a Dockerfile that came out of the context, on the
// bytes actually built. It is the only check for an image context, which the controller cannot
// read. Sound because this binary runs before BuildKit in a container the user cannot alter; a
// failure surfaces as a Job failure (hence jobFailureDetail reads init containers).
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
	// stage is the directory holding path, removed wholesale by the caller.
	stage string
}

func download(ctx context.Context, url, dest, token string) (blob, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return blob{}, fmt.Errorf("creating %s: %w", dest, err)
	}
	// Staged inside dest: the container root is not writable by uid 1000, and dest's emptyDir
	// sizeLimit bounds the download. A directory of its own, so removing it cannot delete an
	// extracted entry of the same name.
	stage, err := os.MkdirTemp(dest, ".fetch-")
	if err != nil {
		return blob{}, fmt.Errorf("staging the download: %w", err)
	}
	tmp, err := os.CreateTemp(stage, "context-*.blob")
	if err != nil {
		return blob{stage: stage}, fmt.Errorf("staging the download: %w", err)
	}
	defer tmp.Close()

	client := &http.Client{Timeout: 10 * time.Minute}
	delay := fetchBaseDelay
	var lastErr error

	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		digest, err := fetchInto(ctx, client, tmp, url, token)
		if err == nil {
			return blob{path: tmp.Name(), digest: digest, stage: stage}, nil
		}
		lastErr = err

		var permanent *permanentError
		if errors.As(err, &permanent) || ctx.Err() != nil {
			break
		}
		if attempt == fetchAttempts {
			break
		}
		fmt.Fprintf(os.Stderr, "fetching the build context (attempt %d/%d): %v; retrying in %s\n",
			attempt, fetchAttempts, err, delay)
		select {
		case <-ctx.Done():
			return blob{stage: stage}, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return blob{stage: stage}, lastErr
}

// fetchAttempts and fetchBaseDelay bound the retry: about fifteen seconds in total. The first dial
// happens as the pod starts, before the CNI has programmed NetworkPolicy (showing as `connection
// refused`), and the Job has BackoffLimit: 0. Also covers source-controller restarts and 5xx.
const (
	fetchAttempts  = 6
	fetchBaseDelay = 500 * time.Millisecond
)

// permanentError marks a response not worth repeating.
type permanentError struct{ error }

// fetchInto makes one attempt, returning the digest of what it wrote. It truncates first, so a
// failed partial attempt cannot corrupt the digest.
func fetchInto(ctx context.Context, client *http.Client, tmp *os.File, url, token string) (string, error) {
	if err := tmp.Truncate(0); err != nil {
		return "", &permanentError{fmt.Errorf("resetting the staged download: %w", err)}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", &permanentError{fmt.Errorf("resetting the staged download: %w", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", &permanentError{err}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Dial, DNS, reset, timeout: transient.
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return "", fmt.Errorf("fetching %s: %s", url, resp.Status)
	default:
		// Other 4xx: retrying will not help.
		return "", &permanentError{fmt.Errorf("fetching %s: %s", url, resp.Status)}
	}

	// Hashed while streaming, over exactly what was written.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// image pulls a digest-pinned image and writes its flattened filesystem into dest.
//
// mutate.Extract applies whiteouts, which per-layer extraction would not. Nothing to verify
// afterwards: the registry cannot serve other bytes under the digest.
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
	// An index is refused rather than resolved by this process's platform (as source.PullImage
	// does): the choice belongs in the spec.
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

	// The same extractor as every other kind, so path rules are one implementation.
	if err := archive.Extract(rc, archive.ModeTar, opts.Dest, opts.Subpath, opts.Strip); err != nil {
		return err
	}
	return checkDockerfile(opts)
}
