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
	// Host is where the CONTROLLERS reach the registry -- "registry.example:5000" or
	// "registry.example:5000/prefix". Empty disables the whole mechanism, and an object that names
	// no repository then has nowhere to go.
	//
	// With the bundled registry this is the in-cluster Service name, which is reachable through
	// cluster DNS and is not reachable by a kubelet. See PublicHost.
	Host string

	// PublicHost is what a WORKLOAD is told to pull from, and it exists because one string cannot
	// satisfy two resolvers.
	//
	// The controllers resolve the registry through cluster DNS to push and to refresh. The kubelet
	// resolves it with the NODE's resolver to pull, and the node's resolver has never heard of
	// anything.svc.cluster.local. status.artifact.ref holds one string, so before this split the
	// operator had to pick which half to break: leave it internal and no Pod can pull, or set it to
	// a node-resolvable name and the controllers fail with "no such host" before publishing
	// anything.
	//
	// Empty means "same as Host", which is correct whenever one name genuinely works from both
	// places -- an external registry, or an ingress with real DNS.
	PublicHost string

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
	return repositoryAt(d.Host, namespace, name)
}

// PublicRepositoryFor is the same repository, addressed as a workload should address it.
//
// Used ONLY to render status.artifact.ref and status.artifact.tags. Everything that actually talks
// to a registry -- pushing, the tag-conflict check, the retention refresh -- uses RepositoryFor,
// because those run from inside the cluster where the public name may not resolve at all.
func (d DefaultRegistry) PublicRepositoryFor(namespace, name string) string {
	if d.PublicHost == "" {
		return d.RepositoryFor(namespace, name)
	}
	return repositoryAt(d.PublicHost, namespace, name)
}

// PublicRepository maps a repository the controller wrote to onto the name a workload should pull.
//
// Only the HOST is rewritten, and only for the operator's own registry. An object that named its
// own repository somewhere else is reported back exactly as written -- the operator's public name
// says nothing about a registry the operator does not run.
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

// InsecureHost reports whether a repository's host is in the operator's plain-HTTP list.
//
// Matched on HOST, not on prefix: naming one internal registry must not downgrade every other
// request, and a prefix match on "oci.internal" would also match "oci.internal.evil.example".
//
// Lives here rather than in each controller because all three call sites -- composing, building and
// refreshing -- have to agree. They were three copies of this function, and three copies of a
// security-relevant comparison is two too many.
func InsecureHost(repository string, insecure []string) bool {
	host, _, _ := strings.Cut(repository, "/")
	for _, h := range insecure {
		if h == host {
			return true
		}
	}
	return false
}
