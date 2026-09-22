package reconciler

import "testing"

// The operator's credential must reach the operator's registry and nothing else. Otherwise a tenant
// setting `push: {repository: attacker.example/x}` gets the controller to authenticate there with
// the operator's password: exfiltration that looks like a feature.
func TestTheOperatorCredentialNeverReachesATenantChosenRegistry(t *testing.T) {
	d := DefaultRegistry{
		Host:       "registry.internal:5000",
		SecretName: "operator-push",
		Namespace:  "oci-composer",
	}

	for _, tc := range []struct {
		name      string
		ownSecret string
		target    string
		wantName  string
		wantNS    string
	}{
		{
			// The case that matters: a host the tenant picked.
			name:     "a registry the tenant chose gets nothing",
			target:   "attacker.example/x",
			wantName: "", wantNS: "",
		},
		{
			name:      "a registry the tenant chose uses the tenant's own secret",
			ownSecret: "tenant-creds",
			target:    "attacker.example/x",
			wantName:  "tenant-creds", wantNS: "team-a",
		},
		{
			name:     "the operator's registry, path chosen by the operator",
			target:   "registry.internal:5000/team-a/app",
			wantName: "operator-push", wantNS: "oci-composer",
		},
		{
			// A path inside the operator's registry is not a different registry. Denying the
			// credential here would force the operator to hand their password to every tenant.
			name:     "the operator's registry, path chosen by the object",
			target:   "registry.internal:5000/somewhere/else",
			wantName: "operator-push", wantNS: "oci-composer",
		},
		{
			name:      "an explicit secret wins even on the operator's registry",
			ownSecret: "tenant-creds",
			target:    "registry.internal:5000/team-a/app",
			wantName:  "tenant-creds", wantNS: "team-a",
		},
		{
			// A host that merely starts with the operator's is a different host.
			name:     "a lookalike host gets nothing",
			target:   "registry.internal:5000.attacker.example/x",
			wantName: "", wantNS: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, ns := d.CredentialFor("team-a", tc.ownSecret, tc.target)
			if name != tc.wantName || ns != tc.wantNS {
				t.Errorf("CredentialFor = (%q, %q), want (%q, %q)", name, ns, tc.wantName, tc.wantNS)
			}
		})
	}
}

// The credential is read from the controller's namespace, never the object's; otherwise a tenant
// could create a Secret of that name and have the controller push with it.
func TestTheDefaultCredentialComesFromTheControllersNamespace(t *testing.T) {
	d := DefaultRegistry{Host: "r:5000", SecretName: "operator-push", Namespace: "oci-composer"}

	_, ns := d.CredentialFor("some-tenant", "", "r:5000/some-tenant/app")
	if ns != "oci-composer" {
		t.Errorf("namespace = %q, want the controller's own; a tenant namespace here means a "+
			"tenant can supply the credential the controller pushes with", ns)
	}
}

// Namespace-qualified, because one registry is shared by the whole cluster: two namespaces each
// with an "app" must not publish to the same repository.
func TestTheDefaultRepositoryIsNamespaceQualified(t *testing.T) {
	d := DefaultRegistry{Host: "registry.internal:5000"}

	a := d.RepositoryFor("team-a", "app")
	b := d.RepositoryFor("team-b", "app")
	if a == b {
		t.Fatalf("two namespaces share the repository %q", a)
	}
	if want := "registry.internal:5000/team-a/app"; a != want {
		t.Errorf("RepositoryFor = %q, want %q", a, want)
	}

	// A host carrying a path prefix is a supported way to share one registry between clusters.
	withPrefix := DefaultRegistry{Host: "registry.internal:5000/staging/"}
	if got, want := withPrefix.RepositoryFor("team-a", "app"),
		"registry.internal:5000/staging/team-a/app"; got != want {
		t.Errorf("with a prefix = %q, want %q", got, want)
	}
}

// An unconfigured default must not half-work: an empty host would read as a Docker Hub reference.
func TestAnUnconfiguredDefaultIsNotUsable(t *testing.T) {
	var d DefaultRegistry
	if d.Configured() {
		t.Error("an empty DefaultRegistry reports itself configured")
	}
	name, _ := d.CredentialFor("team-a", "", "anything/at/all")
	if name != "" {
		t.Errorf("credential %q offered with no registry configured", name)
	}
}
