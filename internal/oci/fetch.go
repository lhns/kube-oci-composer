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

	"github.com/lhns/kube-oci-composer/internal/netguard"
)

// ErrDigestMismatch is returned when fetched content does not match the declared digest. The
// caller maps it to a TERMINAL condition (Stalled): retrying cannot fix wrong bytes, and a retry
// loop would hide tampering.
type ErrDigestMismatch struct {
	Want string
	Got  string
	Ref  string
}

func (e *ErrDigestMismatch) Error() string {
	return fmt.Sprintf("digest mismatch for %s: declared %s, got %s", e.Ref, e.Want, e.Got)
}

// DefaultFetchTimeout bounds a single fetch.
const DefaultFetchTimeout = 10 * time.Minute

// Fetcher retrieves content addressed by digest.
type Fetcher struct {
	Client *http.Client
}

// NewFetcher returns a Fetcher with sane timeouts and the SSRF dial guard installed.
//
// The guard is not optional: without it a spec's URL could reach a metadata endpoint (I6).
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
// The content is streamed to disk and hashed on the way. The caller owns the returned file and
// must remove it.
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
		// Only leave the file behind on success.
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

// DialGuard is the SSRF guard. It lives in internal/netguard so the build path can use it without
// importing internal/oci (ADR 0025).
type DialGuard = netguard.DialGuard
