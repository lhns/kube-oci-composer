package controller

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// TestPlainHTTPPushesNeedTheOperatorToSaySo pins that plain HTTP is used exactly for hosts on
// --insecure-registry. It tests the decision rather than the transport because
// go-containerregistry treats loopback as insecure on its own, so every other unit test's
// httptest registry would pass regardless.
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
			// Assert the resulting scheme, not just that some option was returned.
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
