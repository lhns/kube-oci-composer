package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lhns/kube-oci-composer/internal/store"
)

// DefaultManifestMediaType is assumed when a stored manifest does not declare one. Everything
// this controller produces is an OCI image manifest.
const DefaultManifestMediaType = "application/vnd.oci.image.manifest.v1+json"

// SaveManifest records a manifest's bytes so the build can be replayed after a restart.
//
// go-containerregistry keeps manifests in an unexported in-memory map, and only the current build
// is republished at startup — so without this, every older build's digest reference and content
// tag return 404 once the process restarts, even though its blobs are still in the store. That
// affects the primary image-automation path, not only the hand-pinning fallback. See ADR 0013.
func (s *Server) SaveManifest(ctx context.Context, digest string, raw []byte) error {
	key, err := store.Key(store.NamespaceManifests, digest)
	if err != nil {
		return err
	}
	if err := s.Blobs.Write(ctx, key, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("saving manifest %s: %w", digest, err)
	}
	return nil
}

// LoadManifest reads back a saved manifest, or store.ErrNotFound.
func (s *Server) LoadManifest(ctx context.Context, digest string) ([]byte, error) {
	key, err := store.Key(store.NamespaceManifests, digest)
	if err != nil {
		return nil, err
	}
	rc, err := s.Blobs.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// PutManifest writes a manifest into the running registry under ref, which may be a tag or a
// digest.
//
// This goes over loopback HTTP rather than through a Go API because the registry's manifest store
// is unexported and has no seed hook — pushing to it IS the API. The blobs it references are
// already present, so this is a single small request per build.
func (s *Server) PutManifest(ctx context.Context, repoPath, ref string, raw []byte) error {
	url := fmt.Sprintf("http://127.0.0.1%s/v2/%s/manifests/%s", s.Addr, repoPath, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}
	req.Header.Set("Content-Type", manifestMediaType(raw))

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("putting manifest %s: %w", ref, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("putting manifest %s: unexpected status %s", ref, resp.Status)
	}
	return nil
}

// HasManifest reports whether the registry currently resolves ref.
func (s *Server) HasManifest(ctx context.Context, repoPath, ref string) bool {
	url := fmt.Sprintf("http://127.0.0.1%s/v2/%s/manifests/%s", s.Addr, repoPath, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// manifestMediaType reads the mediaType the manifest declares about itself.
//
// The registry stores whatever Content-Type it was pushed with and serves it back verbatim, so
// getting this wrong would hand clients a manifest labelled as something it is not.
func manifestMediaType(raw []byte) string {
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.MediaType != "" {
		return probe.MediaType
	}
	return DefaultManifestMediaType
}
