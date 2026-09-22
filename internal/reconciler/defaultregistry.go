package reconciler

import (
	"fmt"
	"strings"
)

// DefaultRegistry is the registry objects publish to when they do not name one themselves,
// configured once in the chart.
type DefaultRegistry struct {
	// Host is where the CONTROLLERS reach the registry -- "registry.example:5000" or
	// "registry.example:5000/prefix". Empty disables the mechanism. With the bundled registry this
	// is the in-cluster Service name, which a kubelet cannot resolve; see PublicHost.
	Host string

	// PublicHost is what a WORKLOAD is told to pull from. The controllers resolve the registry
	// through cluster DNS, the kubelet through the node's resolver, and one name rarely works for
	// both. Empty means "same as Host", right when one name works from both places.
	PublicHost string

	// SecretName is a dockerconfigjson Secret in the CONTROLLER's own namespace, not the object's:
	// the credential belongs to the operator, not the tenant.
	SecretName string

	// Namespace is the controller's namespace, from POD_NAMESPACE.
	Namespace string
}

// Configured reports whether a default target exists.
func (d DefaultRegistry) Configured() bool { return d.Host != "" }

// RepositoryFor is where an object that named no repository publishes.
//
// Namespace-qualified because one registry is shared by the whole cluster, and two namespaces'
// "app" objects would otherwise silently overwrite each other.
func (d DefaultRegistry) RepositoryFor(namespace, name string) string {
	return repositoryAt(d.Host, namespace, name)
}

// PublicRepository maps a repository the controller wrote to onto the name a workload should pull.
//
// Only the HOST is rewritten, and only for the operator's own registry.
//
// The result is for status.artifact.ref and .tags only. A pod generally cannot resolve it, so
// anything that dials a registry (including the retention refresh) must build its reference from
// RepositoryFor rather than read one back out of status. ADR 0048.
func (d DefaultRegistry) PublicRepository(repository string) string {
	if d.PublicHost == "" || repository == "" || !d.Owns(repository) {
		return repository
	}
	_, path, found := strings.Cut(repository, "/")
	if !found {
		return strings.TrimSuffix(d.PublicHost, "/")
	}
	return strings.TrimSuffix(d.PublicHost, "/") + "/" + path
}

func repositoryAt(host, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(host, "/"), namespace, name)
}

// CredentialFor decides which Secret authenticates a request. It is a SECURITY boundary:
//
//	the operator's credential is sent to the operator's registry, and nowhere else.
//
// The rule keys on the target HOST, not on whether the object named a repository: an object may
// pick its own path inside the operator's registry and still be authenticated, while a tenant
// naming `attacker.example/x` must never be handed the operator's password.
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
// Compared on HOST alone: a path prefix in Host organises one registry but is not a security
// boundary, since anyone who can reach the registry can reach every path in it.
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

// InsecureHost reports whether a repository's host is in the operator's plain-HTTP list.
//
// Matched on exact HOST, not prefix: a prefix match on "oci.internal" would also downgrade
// "oci.internal.evil.example". Shared so composing, building and refreshing agree.
func InsecureHost(repository string, insecure []string) bool {
	host := hostOf(repository)
	for _, h := range insecure {
		if h == host {
			return true
		}
	}
	return false
}
