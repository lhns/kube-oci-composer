// Package serve provides the built-in read-only OCI distribution endpoint.
//
// This is what makes a registry optional. Workloads pull artifacts straight from the
// controller, so a small cluster installs one component and is done: no registry to run, no
// push credentials anywhere, and no node configuration — an ordinary Service behind the
// cluster's existing ingress and certificate is enough for containerd to pull over HTTPS.
//
// Storage is deliberately disposable. Composition is deterministic, so the controller can
// always re-assemble an artifact from its spec; losing the blob directory costs a rebuild, not
// data. That is why there is no PVC here and nothing to back up. A registry has to persist
// bytes; a deterministic composer does not.
//
// UPSTREAM CAVEAT, stated rather than buried: go-containerregistry describes pkg/registry as
// aimed at tests, with production use invited but not claimed. It is used here because it is a
// standards-compliant implementation of the distribution spec — including the Referrers API —
// and reimplementing that would be worse. The exposure is bounded: the endpoint is read-only
// to the network, serves reproducible content, and Spegel absorbs its unavailability for
// anything already pulled. Anyone uncomfortable with that should set spec.push and use a real
// registry.
package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

// Server hosts the read-only OCI endpoint plus its backing blob store.
type Server struct {
	// Host is the externally reachable name workloads pull from, e.g. "oci.example.com".
	// It is used to build status.artifact.ref, so it must match how the endpoint is exposed.
	Host string

	// Addr is the listen address, e.g. ":5000".
	Addr string

	// dir backs the blob store.
	dir string

	handler http.Handler
}

// New creates a Server storing blobs under dir. The directory is created if missing and may be
// ephemeral: it is a cache of reproducible content, not a system of record.
func New(host, addr, dir string) (*Server, error) {
	if host == "" {
		return nil, fmt.Errorf("serving host must be set: it is what status.artifact.ref is built from")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating blob directory %q: %w", dir, err)
	}

	h := registry.New(
		registry.WithBlobHandler(registry.NewDiskBlobHandler(filepath.Clean(dir))),
		// Referrers support is on so SBOM, provenance and signature artifacts can attach to a
		// manifest by subject, the same way they would on a real registry.
		registry.WithReferrersSupport(true),
	)

	return &Server{Host: host, Addr: addr, dir: dir, handler: h}, nil
}

// Handler exposes the distribution endpoint.
//
// Writes arrive only from the controller itself over loopback; the Service exposes this for
// pulls. Restricting writes at the network layer is the deployment's job, not this handler's —
// the chart binds the write path to localhost.
func (s *Server) Handler() http.Handler { return s.handler }

// LocalRef returns the loopback reference the controller pushes to, e.g.
// "127.0.0.1:5000/name:tag". Assembly writes here; the same store then serves external pulls.
func (s *Server) LocalRef(name, tag string) string {
	return fmt.Sprintf("127.0.0.1%s/%s:%s", s.Addr, name, tag)
}

// PublicRef returns the reference a workload should use, e.g. "oci.example.com/name:tag".
func (s *Server) PublicRef(name, tag string) string {
	return fmt.Sprintf("%s/%s:%s", s.Host, name, tag)
}

// Start runs the endpoint until ctx is cancelled, satisfying manager.Runnable so the
// controller-runtime manager owns its lifecycle. A listener failure therefore takes the whole
// process down rather than leaving a controller that reports Ready for artifacts nothing can
// pull.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
