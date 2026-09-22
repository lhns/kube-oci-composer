package reconciler

import "testing"

// PublicHost renames the operator's registry only. A repository elsewhere must be reported as
// written; rewriting it would put an address in status the object never published to.
func TestThePublicHostRewritesOnlyTheOperatorsOwnRegistry(t *testing.T) {
	d := DefaultRegistry{
		Host:       "kube-oci-composer-registry.oci.svc.cluster.local:5000",
		PublicHost: "oci-composer.internal:30500",
	}

	cases := []struct {
		name string
		repo string
		want string
	}{
		{
			name: "the operator's registry is rewritten",
			repo: "kube-oci-composer-registry.oci.svc.cluster.local:5000/team-a/app",
			want: "oci-composer.internal:30500/team-a/app",
		},
		{
			// The path survives whole, including a prefix the operator configured.
			name: "a deeper path keeps every segment",
			repo: "kube-oci-composer-registry.oci.svc.cluster.local:5000/prefix/team-a/app",
			want: "oci-composer.internal:30500/prefix/team-a/app",
		},
		{
			name: "someone else's registry is left alone",
			repo: "ghcr.io/example/app",
			want: "ghcr.io/example/app",
		},
		{
			name: "a host a tenant chose is left alone",
			repo: "attacker.example/x",
			want: "attacker.example/x",
		},
		{
			// A lookalike is a different host. Same rule as InsecureHost and CredentialFor.
			name: "a lookalike host is not the operator's registry",
			repo: "kube-oci-composer-registry.oci.svc.cluster.local.evil.example/x",
			want: "kube-oci-composer-registry.oci.svc.cluster.local.evil.example/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.PublicRepository(tc.repo); got != tc.want {
				t.Fatalf("PublicRepository(%q) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}

// TestAnUnsetPublicHostChangesNothing: with no public host, internal and public names are the same.
func TestAnUnsetPublicHostChangesNothing(t *testing.T) {
	d := DefaultRegistry{Host: "ghcr.io/example"}

	if got := d.PublicRepository("ghcr.io/example/team-a/app"); got != "ghcr.io/example/team-a/app" {
		t.Fatalf("PublicRepository rewrote a reference with no public host set: %q", got)
	}
	internal := d.RepositoryFor("team-a", "app")
	if public := d.PublicRepository(internal); internal != public {
		t.Fatalf("with no public host the two must agree: %q vs %q", internal, public)
	}
}

// TestThePublicNameIsNamespaceQualifiedToo — the public name keeps the internal one's path, so
// object names cannot collide across namespaces in one and not the other. ADR 0048.
func TestThePublicNameIsNamespaceQualifiedToo(t *testing.T) {
	d := DefaultRegistry{
		Host:       "registry.svc:5000",
		PublicHost: "oci-composer.internal:30500",
	}
	internal := d.RepositoryFor("team-a", "app")
	if got, want := d.PublicRepository(internal), "oci-composer.internal:30500/team-a/app"; got != want {
		t.Fatalf("PublicRepository(%q) = %q, want %q", internal, got, want)
	}
	// A trailing slash on the configured host must not double up.
	d.PublicHost = "oci-composer.internal:30500/"
	if got, want := d.PublicRepository(internal), "oci-composer.internal:30500/team-a/app"; got != want {
		t.Fatalf("a trailing slash produced %q, want %q", got, want)
	}
}

// TestTheCredentialRuleIgnoresThePublicHost: CredentialFor matches Host, the address actually
// dialled. Matching PublicHost would hand the operator's credential to a tenant who named it.
func TestTheCredentialRuleIgnoresThePublicHost(t *testing.T) {
	d := DefaultRegistry{
		Host:       "registry.svc:5000",
		PublicHost: "oci-composer.internal:30500",
		SecretName: "operator-push",
		Namespace:  "oci",
	}

	name, ns := d.CredentialFor("team-a", "", "registry.svc:5000/team-a/app")
	if name != "operator-push" || ns != "oci" {
		t.Fatalf("the operator's own registry must get the credential; got %q/%q", ns, name)
	}

	if name, _ := d.CredentialFor("team-a", "", "oci-composer.internal:30500/team-a/app"); name != "" {
		t.Fatalf("the public host must not authorise the operator's credential; got %q", name)
	}
}
