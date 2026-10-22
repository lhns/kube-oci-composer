package reconciler

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Transport returns a RoundTripper that trusts the system roots PLUS the PEM bundle in caFile.
//
// Shared so every place that talks to a registry agrees about what is trusted.
//
// ADDITIVE, never replacing: an empty base pool would work for the operator's own registry and
// break every pull from public registries, only on the installs that set this flag.
func Transport(caFile string) (http.RoundTripper, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading registry CA: %w", err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		// Fatal rather than falling back to an empty pool, which would silently distrust every
		// public registry.
		return nil, fmt.Errorf("reading system CA pool: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate found in %s", caFile)
	}

	// Cloned from ggcr's default so its connection pooling and timeouts survive.
	base, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("go-containerregistry's default transport is no longer an *http.Transport")
	}
	t := base.Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return t, nil
}
