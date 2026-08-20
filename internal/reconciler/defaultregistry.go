package reconciler

import (
	"fmt"
	"strings"
)

// DefaultRegistry is the registry objects publish to when they do not name one themselves.
//
// It exists so an operator can configure the registry ONCE, in the chart, instead of pasting a
// hostname into every object. With the bundled registry that is the whole of the configuration: a
// default install publishes somewhere real without anyone editing a spec.
type DefaultRegistry struct {
	// Host is "registry.example:5000" or "registry.example:5000/prefix". Empty disables the whole
	// mechanism, and an object that names no repository then has nowhere to go.
	Host string

	// SecretName is a dockerconfigjson Secret in the CONTROLLER's own namespace -- not the
	// object's. The credential belongs to the operator who installed the chart, not to the tenant
	// who created the object.
	SecretName string

	// Namespace is the controller's namespace, from POD_NAMESPACE.
	Namespace string
}

// Configured reports whether a default target exists.
func (d DefaultRegistry) Configured() bool { return d.Host != "" }

// RepositoryFor is where an object that named no repository publishes.
//
// Namespace-qualified, and that is not decoration. One registry is now shared by every object in
// the cluster, so a bare object name collides the moment two namespaces both have an "app" -- and
// the collision would be silent, resolved by whichever object reconciled last, under a tag policy
// that would read it as a legitimate conflict.
func (d DefaultRegistry) RepositoryFor(namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(d.Host, "/"), namespace, name)
}

// CredentialFor decides which Secret authenticates a request, and it is a SECURITY boundary rather
// than a convenience.
//
// The rule is about the HOST, not about whether the object named a repository:
//
//	the operator's credential is sent to the operator's registry, and nowhere else.
//
// An object may name its own path inside that registry and still be authenticated -- that is the
// ordinary case, and the first version of this rule got it wrong by keying on "did the object name a
// repository", which denied the credential to every object that wanted a specific path in the
// operator's own registry. The e2e caught it as a 401. The consequence of leaving it that way would
// have been worse than the bug: the workaround is handing the operator's password to every tenant.
//
// What the rule still prevents is the whole point. A tenant able to create an ImageComposition
// writes `push: {repository: attacker.example/x}` and, under a host-blind rule, the controller
// connects to a host they chose and presents the operator's registry password. Nothing about the
// request looks wrong -- it is a well-formed push to a well-formed reference, and it succeeds.
//
// Returns the secret name and the namespace to read it from; an empty name means anonymous.
func (d DefaultRegistry) CredentialFor(objectNamespace, ownSecretRef, targetRepository string) (name, namespace string) {
	if ownSecretRef != "" {
		return ownSecretRef, objectNamespace
	}
	if d.SecretName != "" && d.Owns(targetRepository) {
		return d.SecretName, d.Namespace
	}
	return "", ""
}

// Owns reports whether a repository lives in the operator's own registry.
//
// Compared on HOST alone. A path prefix in DefaultRegistry.Host is a convention for organising one
// registry, not a security boundary: anyone who can reach the registry can reach every path in it,
// so pretending otherwise here would suggest a guarantee the registry does not make.
func (d DefaultRegistry) Owns(repository string) bool {
	if d.Host == "" || repository == "" {
		return false
	}
	return hostOf(repository) == hostOf(d.Host)
}

// hostOf returns the registry host of a reference, which is everything before the first slash.
func hostOf(reference string) string {
	host, _, _ := strings.Cut(reference, "/")
	return host
}
