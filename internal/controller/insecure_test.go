package controller

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// TestPlainHTTPPushesNeedTheOperatorToSaySo is the regression test for a defect the unit suite was
// structurally incapable of catching.
//
// Removing the embedded serving endpoint (ADR 0035) removed the only plaintext push path this
// controller had: pushes were previously either loopback -- always HTTP -- or to a real registry
// over HTTPS, so there was no third case and the controller never consulted --insecure-registry.
// The default case is now a bundled registry on a Service or a NodePort, neither of which has a
// certificate, so every publish failed with "server gave HTTP response to HTTPS client".
//
// The whole suite stayed green through that, because go-containerregistry treats localhost and
// 127.0.0.1 as insecure on its own and every unit test's registry is an httptest server on
// loopback. Testing the DECISION rather than the transport is what makes this checkable here.
func TestPlainHTTPPushesNeedTheOperatorToSaySo(t *testing.T) {
	cases := []struct {
		name     string
		insecure []string
		repo     string
		want     bool
	}{
		{
			name:     "the operator's own bundled registry, named on the flag",
			insecure: []string{"kube-oci-composer-registry.oci:5000"},
			repo:     "kube-oci-composer-registry.oci:5000/team-a/app",
			want:     true,
		},
		{
			// The failure that reached a cluster: the flag rendered, and nothing read it.
			name:     "nothing configured",
			insecure: nil,
			repo:     "oci.internal:5000/team-a/app",
			want:     false,
		},
		{
			name:     "a real registry, while an internal one is listed",
			insecure: []string{"oci.internal:5000"},
			repo:     "ghcr.io/me/app",
			want:     false,
		},
		{
			// Host, not prefix. A prefix match would downgrade a lookalike an attacker controls.
			name:     "a lookalike host that merely starts the same way",
			insecure: []string{"oci.internal"},
			repo:     "oci.internal.evil.example/me/app",
			want:     false,
		},
		{
			// Ports are part of the host. Listing the NodePort must not downgrade port 443.
			name:     "the same name on a different port",
			insecure: []string{"oci.internal:5000"},
			repo:     "oci.internal/me/app",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &ImageCompositionReconciler{InsecureRegistries: tc.insecure}
			got := len(r.refOptions(tc.repo)) > 0
			if got != tc.want {
				t.Fatalf("plain HTTP allowed = %v, want %v for %q", got, tc.want, tc.repo)
			}
			// And the option really produces an http scheme, so this asserts the outcome rather
			// than the presence of an opaque value in a slice.
			if tc.want {
				repo, err := name.NewRepository(tc.repo, r.refOptions(tc.repo)...)
				if err != nil {
					t.Fatalf("parsing %q: %v", tc.repo, err)
				}
				if repo.Scheme() != "http" {
					t.Fatalf("scheme is %q, want http", repo.Scheme())
				}
			}
		})
	}
}
