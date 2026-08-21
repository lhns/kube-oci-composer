package oci

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheMetadataEndpointIsAlwaysRefused is the point of the whole file.
//
// 169.254.169.254 is the cloud metadata endpoint on AWS, GCP, Azure and Hetzner, and it hands
// credentials to anything that asks. `fetch.url` comes from a spec, so without this a tenant
// chooses that address and the controller makes the request from its own network position.
func TestTheMetadataEndpointIsAlwaysRefused(t *testing.T) {
	for _, addr := range []string{
		"169.254.169.254:80",
		"169.254.169.254:443",
		"[fe80::1]:80",
	} {
		t.Run(addr, func(t *testing.T) {
			// The zero value: no --fetch-deny-private, nothing opted into. It must still refuse.
			_, err := DialGuard{}.DialContext(context.Background(), "tcp", addr)
			var blocked *ErrBlockedAddress
			if !errors.As(err, &blocked) {
				t.Fatalf("dialing %s must be refused as a blocked address; got %v", addr, err)
			}
			if !strings.Contains(blocked.Reason, "metadata") {
				t.Fatalf("the refusal should say why: %q", blocked.Reason)
			}
		})
	}
}

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

	f := NewFetcherWithGuard(DialGuard{})
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

	f := NewFetcherWithGuard(DialGuard{DenyPrivate: true})
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
func TestTheGuardClassifiesAddressesCorrectly(t *testing.T) {
	cases := []struct {
		ip          string
		denyPrivate bool
		refused     bool
	}{
		// Always, whatever the flag says.
		{"169.254.169.254", false, true},
		{"169.254.169.254", true, true},
		{"0.0.0.0", false, true},

		// A public address is a layer source under either setting.
		{"140.82.121.4", false, false}, // github.com
		{"140.82.121.4", true, false},

		// Private: allowed by default, refused on request.
		{"10.0.0.5", false, false},
		{"10.0.0.5", true, true},
		{"192.168.1.10", true, true},
		{"172.16.0.1", true, true},
		{"127.0.0.1", true, true},
		{"fd00::1", true, true},

		// 100.64.0.0/10 -- CGNAT, where several managed providers put node networks. net has no
		// IsPrivate for it, which is exactly why it is worth a case.
		{"100.64.0.1", true, true},
		{"100.127.255.254", true, true},
		// 100.128.0.0 is outside the /10 and is ordinary public space.
		{"100.128.0.1", true, false},
	}
	for _, tc := range cases {
		g := DialGuard{DenyPrivate: tc.denyPrivate}
		got := g.blocked(net.ParseIP(tc.ip)) != ""
		if got != tc.refused {
			t.Errorf("%s with DenyPrivate=%v: refused=%v, want %v", tc.ip, tc.denyPrivate, got, tc.refused)
		}
	}
}
