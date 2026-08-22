package reconciler

import "testing"

// TestThePublicHostRewritesOnlyTheOperatorsOwnRegistry is the boundary this split has to hold.
//
// PublicHost says "this is the name a workload should use for MY registry". It says nothing about
// anyone else's. An object that named its own repository somewhere else must be reported back
// exactly as written — rewriting its host would put an address in status that names a registry the
// operator runs and the object never published to, which is a lie in the one field a workload
// reads.
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
			// The case that matters most: a host chosen by a tenant must never be reported as the
			// operator's public name.
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

// TestAnUnsetPublicHostChangesNothing keeps the split invisible to everyone who does not need it —
// an external registry, or an ingress whose name resolves everywhere, is one name and must stay
// one name.
func TestAnUnsetPublicHostChangesNothing(t *testing.T) {
	d := DefaultRegistry{Host: "ghcr.io/example"}

	if got := d.PublicRepository("ghcr.io/example/team-a/app"); got != "ghcr.io/example/team-a/app" {
		t.Fatalf("PublicRepository rewrote a reference with no public host set: %q", got)
	}
	internal := d.RepositoryFor("team-a", "app")
	public := d.PublicRepositoryFor("team-a", "app")
	if internal != public {
		t.Fatalf("with no public host the two must agree: %q vs %q", internal, public)
	}
}

// TestPublicRepositoryForIsNamespaceQualifiedToo — the public name is derived from the same rule as
// the internal one, so a bare object name cannot collide across namespaces in one and not the other.
func TestPublicRepositoryForIsNamespaceQualifiedToo(t *testing.T) {
	d := DefaultRegistry{
		Host:       "registry.svc:5000",
		PublicHost: "oci-composer.internal:30500",
	}
	if got, want := d.PublicRepositoryFor("team-a", "app"), "oci-composer.internal:30500/team-a/app"; got != want {
		t.Fatalf("PublicRepositoryFor = %q, want %q", got, want)
	}
	// A trailing slash on the configured host must not double up.
	d.PublicHost = "oci-composer.internal:30500/"
	if got, want := d.PublicRepositoryFor("team-a", "app"), "oci-composer.internal:30500/team-a/app"; got != want {
		t.Fatalf("a trailing slash produced %q, want %q", got, want)
	}
}

// TestTheCredentialRuleIgnoresThePublicHost is the security check on this change.
//
// CredentialFor compares against Host, the address the controller actually connects to. Comparing
// against PublicHost instead — or as well — would mean a tenant who named the operator's PUBLIC
// name in their own spec could be handed the operator's credential for a connection the operator
// never verified. The public name is documentation, not authority.
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

	// The public name is a different host as far as this rule is concerned, and that is deliberate.
	if name, _ := d.CredentialFor("team-a", "", "oci-composer.internal:30500/team-a/app"); name != "" {
		t.Fatalf("the public host must not authorise the operator's credential; got %q", name)
	}
}
