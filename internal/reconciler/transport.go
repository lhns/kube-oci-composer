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
// Lives here beside InsecureHost and CredentialFor for the same reason those do: all four places
// that talk to a registry — composing, pulling a base image, building, refreshing — have to agree
// about what is trusted, and a fourth copy of the decision is how they stop agreeing.
//
// ADDITIVE, never replacing. The pool starts from x509.SystemCertPool and the file is appended to
// it. An empty base pool would work perfectly for the operator's own registry and break every pull
// from ghcr.io, Docker Hub and any registry an object named for itself — and it would break them
// only for the installs that set this flag, which is to say only in production.
func Transport(caFile string) (http.RoundTripper, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading registry CA: %w", err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		// Deliberately fatal rather than falling back to an empty pool. "At least the private CA
		// works" would silently distrust the whole public internet, and the symptom — base image
		// pulls failing with x509 while pushes succeed — points nowhere near the cause.
		return nil, fmt.Errorf("reading system CA pool: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate found in %s", caFile)
	}

	// Cloned from ggcr's default so its connection pooling and timeouts survive; only the TLS
	// config is ours. Building an http.Transport from scratch here would silently drop that tuning.
	base, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("go-containerregistry's default transport is no longer an *http.Transport")
	}
	t := base.Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return t, nil
}
