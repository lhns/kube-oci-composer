package oci

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/netguard"
)

// The guard reached THROUGH the fetcher. The guard's own classification is tested in
// internal/netguard; these two are about the fetcher installing it, which is the part that would
// silently stop being true if a constructor changed.

// TestPrivateAddressesAreReachableByDefault is the other half, and the reason DenyPrivate exists
// as a flag rather than as the default.
//
// An artifact server on a private address in the same cluster is this project's most ordinary
// layer source. A guard that refused it would be turned off, and then it would protect nothing.
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

// TestTheGuardClassifiesAddressesCorrectly exercises the ranges directly, because reaching some of
// them from a test would mean making the connections this code exists to prevent.
