package oci

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/netguard"
)

// These test that the fetcher installs the guard; its classification is tested in
// internal/netguard.

// TestPrivateAddressesAreReachableByDefault: an in-cluster artifact server on a private address is
// the ordinary layer source, so DenyPrivate is opt-in.
func TestPrivateAddressesAreReachableByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	f := NewFetcherWithGuard(netguard.DialGuard{})
	resp, err := f.Client.Get(srv.URL) //nolint:noctx // the client carries its own timeout
	if err != nil {
		t.Fatalf("a private (loopback) source must remain reachable by default: %v", err)
	}
	resp.Body.Close()
}

// TestDenyPrivateRefusesLoopback covers the opt-in half.

func TestDenyPrivateRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	f := NewFetcherWithGuard(netguard.DialGuard{DenyPrivate: true})
	_, err := f.Client.Get(srv.URL) //nolint:noctx // the client carries its own timeout
	if err == nil {
		t.Fatal("--fetch-deny-private must refuse a loopback source")
	}
	if !strings.Contains(err.Error(), "fetch-deny-private") {
		t.Fatalf("the error should name the flag that caused it: %v", err)
	}
}
