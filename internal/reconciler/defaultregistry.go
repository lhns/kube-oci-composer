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

// CredentialFor decides which Secret authenticates a push, and it is a SECURITY boundary rather
// than a convenience.
//
// The operator's credential is used only for the default registry -- that is, only when the object
// did not choose where its content goes. An object that names its own repository gets its own
// secretRef or nothing.
//
// Without that rule, any tenant able to create an ImageComposition could set
// `push.repository: attacker.example/x` and have the controller authenticate to it with the
// operator's registry password. The credential would be handed to a host the tenant picked, which
// is credential exfiltration dressed up as a feature.
//
// Returns the secret name and the namespace to read it from; an empty name means anonymous.
func (d DefaultRegistry) CredentialFor(objectNamespace string, ownSecretRef string, usesDefault bool) (name, namespace string) {
	if ownSecretRef != "" {
		return ownSecretRef, objectNamespace
	}
	if usesDefault && d.SecretName != "" {
		return d.SecretName, d.Namespace
	}
	return "", ""
}
