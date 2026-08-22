package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrDigestMismatch is returned when fetched content does not match the declared digest.
// It is deliberately a distinct error type: the caller maps it to a TERMINAL condition
// (Stalled), never a retry, because retrying cannot make wrong bytes right and a silent retry
// loop would hide tampering.
type ErrDigestMismatch struct {
	Want string
	Got  string
	Ref  string
}

func (e *ErrDigestMismatch) Error() string {
	return fmt.Sprintf("digest mismatch for %s: declared %s, got %s", e.Ref, e.Want, e.Got)
}

// DefaultFetchTimeout bounds a single fetch. Artifacts here are tens of megabytes; a fetch
// that has not finished well inside this is stuck rather than slow.
const DefaultFetchTimeout = 10 * time.Minute

// Fetcher retrieves content addressed by digest.
type Fetcher struct {
	Client *http.Client
}

// NewFetcher returns a Fetcher with sane timeouts and the SSRF dial guard installed.
//
// The guard is in the default constructor rather than an option, because a fetcher built without
// it is one an attacker-supplied URL can point at a metadata endpoint (I6), and that should not be
// the thing a caller has to remember.
func NewFetcher() *Fetcher {
	return NewFetcherWithGuard(DialGuard{})
}

// NewFetcherWithGuard returns a Fetcher whose transport applies g.
func NewFetcherWithGuard(g DialGuard) *Fetcher {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = g.DialContext
	return &Fetcher{Client: &http.Client{Timeout: DefaultFetchTimeout, Transport: tr}}
}

// FetchURL downloads url into a temporary file, verifying that its content matches wantDigest.
//
// The content is streamed to disk rather than buffered: these artifacts run to tens or hundreds
// of megabytes and a controller holding several of them in memory at once is a memory limit
// waiting to be hit.
//
// The digest is verified over the bytes as they stream past, so a mismatch is caught without a
// second pass. The caller owns the returned file and must remove it.
func (f *Fetcher) FetchURL(ctx context.Context, url, wantDigest string) (path string, err error) {
	if !strings.HasPrefix(wantDigest, "sha256:") {
		return "", fmt.Errorf("unsupported digest algorithm in %q: only sha256 is supported", wantDigest)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp("", "oci-composer-fetch-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer func() {
		tmp.Close()
		// Only leave the file behind on success; a partial download is never useful.
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()

	hasher := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}

	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if got != wantDigest {
		err = &ErrDigestMismatch{Want: wantDigest, Got: got, Ref: url}
		return "", err
	}

	return tmp.Name(), nil
}
