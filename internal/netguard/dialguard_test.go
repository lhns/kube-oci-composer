package netguard

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// TestTheMetadataEndpointIsAlwaysRefused: link-local (the cloud metadata endpoint, which hands out
// credentials) is refused even with DenyPrivate off.
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

		// 100.64.0.0/10 (CGNAT), which net.IP.IsPrivate does not cover.
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
