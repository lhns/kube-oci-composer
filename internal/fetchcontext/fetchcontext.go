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

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/lhns/kube-oci-composer/internal/archive"
	"github.com/lhns/kube-oci-composer/internal/build"
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
	// Dockerfile is a path inside the context whose FROM lines must all be digest-pinned. Empty
	// skips the check, which is what a Dockerfile from outside the context means -- the controller
	// checked those before this pod existed.
	Dockerfile string
}

// Run fetches, verifies and extracts.
//
// VERIFY BEFORE UNPACK, always. Checking afterwards would mean an archive with a wrong digest had
// already written files that a build might read, which makes the digest decorative. So the body is
// streamed to a temporary file while being hashed, and nothing is extracted until the hash matches.
func Run(ctx context.Context, opts Options) error {
	// An image is addressed by its own digest, so there is no separate blob to verify and nothing
	// to unpack from an archive -- the registry cannot serve other bytes under that reference.
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

	// The wrapper strip applies to a Flux artifact and to nothing else: source-controller wraps the
	// tree in one directory whose name is unpredictable. A fetched tarball is whatever the publisher
	// made it, and `subpath` is how a version-named wrapper is named there.
	if err := archive.Extract(f, archive.Mode(opts.Unpack), opts.Dest, opts.Subpath,
		opts.Kind == "sourceRef"); err != nil {
		return err
	}
	return checkDockerfile(opts)
}

// checkDockerfile refuses an unpinned FROM in a Dockerfile that came out of the context.
//
// Here rather than only in the controller because for an IMAGE context the controller cannot read
// it: doing so would mean giving a process shared by every namespace registry credentials for
// arbitrary user-named repositories, and pulling potentially gigabytes into it. Refusing the
// combination instead was the other option and it is worse -- it would mean `path`, the default and
// the thing most people want, silently not working with one context kind.
//
// Sound because THIS IS OUR BINARY, not user code: it runs before BuildKit, on user data, in a
// container the user cannot alter, and nothing writes to the tree between here and buildctl. What
// is lost against a controller-side check is that the failure arrives as a Job failure rather than
// a Stalled condition -- which is why jobFailureDetail had to learn to read init containers.
//
// Run for every kind, not only images. Once it is our binary it costs nothing, and it means the
// check happens on the bytes that are actually built rather than on bytes fetched a moment earlier.
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

	// Hashed as the bytes stream past, so nothing is buffered and the digest is over exactly what
	// was written.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return blob{}, fmt.Errorf("downloading %s: %w", url, err)
	}
	return blob{path: tmp.Name(), digest: "sha256:" + hex.EncodeToString(h.Sum(nil))}, nil
}

// image pulls a digest-pinned image and writes its flattened filesystem into dest.
//
// mutate.Extract rather than untarring each layer in turn, and that is not a shortcut: Extract
// APPLIES WHITEOUTS. A per-layer extraction gets an image whose upper layer deleted a file wrong,
// and the file reappears in the build context — which nobody notices until something reads it.
//
// The digest is the reference, so there is nothing to verify afterwards: the registry cannot serve
// other bytes under it. That is why this path has no equivalent of the download's hash check.
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
	// own platform -- the same refusal source.PullImage makes, for the same reason: the choice
	// belongs in the spec, not in whatever architecture the build happened to land on.
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

	// Through the same extractor as every other kind, so traversal refusal, symlink refusal and
	// subpath selection are one implementation rather than three.
	if err := archive.Extract(rc, archive.ModeTar, opts.Dest, opts.Subpath, false); err != nil {
		return err
	}
	return checkDockerfile(opts)
}
