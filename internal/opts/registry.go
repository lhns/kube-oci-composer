// Package opts holds the startup options both controllers share.
//
// It exists because they genuinely share them: where to publish, whose credential to use, what to
// trust, and what supply-chain material to attach are the same questions for an ImageComposition
// and an ImageBuild, and the answers have to mean the same thing in both binaries. Defined twice,
// the help text drifts and the two controllers quietly disagree about a flag they both accept.
package opts

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/lhns/kube-oci-composer/internal/attest"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// Registry is where a controller publishes, and what it trusts and attaches when it does.
type Registry struct {
	DefaultRegistry    string
	PublicRegistryHost string
	DefaultPushSecret  string
	InsecureRegistries string
	CAFile             string

	SBOM             bool
	Provenance       bool
	SigningKeySecret string
}

// Bind registers the flags. Call before flag.Parse.
func (o *Registry) Bind(fs *flag.FlagSet) {
	fs.StringVar(&o.DefaultRegistry, "default-registry", "",
		"Registry to publish to when an object names no repository of its own, as "+
			"\"registry.example:5000\" or \"registry.example:5000/prefix\". Objects then publish "+
			"to <default-registry>/<namespace>/<name>.\n"+
			"Namespace-qualified deliberately: one registry is shared by the whole cluster, so a "+
			"bare object name would collide the moment two namespaces both have an \"app\".")

	fs.StringVar(&o.PublicRegistryHost, "public-registry-host", "",
		"What a WORKLOAD should pull from, when that differs from --default-registry. "+
			"Used ONLY to render status.artifact.ref. One string cannot satisfy two resolvers: this "+
			"controller reaches the registry through cluster DNS, and a kubelet reaches it with the "+
			"NODE's resolver, which has never heard of anything.svc.cluster.local. Empty means the two "+
			"are the same name, which is correct for an external registry or an ingress with real DNS.")

	fs.StringVar(&o.DefaultPushSecret, "default-push-secret", "",
		"dockerconfigjson Secret authenticating pushes to --default-registry. Read from THIS "+
			"controller's namespace (POD_NAMESPACE), not the object's: it is the operator's "+
			"credential, not a tenant's.\n"+
			"Used ONLY for objects that named no repository. An object that chooses its own "+
			"registry authenticates with its own secretRef or not at all -- otherwise anyone able "+
			"to create an object could point it at a host they control and be handed this password.")

	fs.StringVar(&o.InsecureRegistries, "insecure-registry", "",
		"Comma-separated registry hosts that may be reached over plain HTTP. Matched on host, so "+
			"naming one internal registry does not downgrade any other request.")

	fs.StringVar(&o.CAFile, "registry-ca-file", "",
		"PEM bundle of ADDITIONAL root CAs to trust, on top of the system roots. Needed when the "+
			"registry serves a certificate signed by a CA the image does not already trust -- the "+
			"chart's self-signed mode, or a corporate CA. Applies to every registry this controller "+
			"talks to, including base-image pulls, because a composition layered on a build pulls its "+
			"base from the same registry it pushes to.")

	fs.BoolVar(&o.SBOM, "sbom", false,
		"Attach an SBOM to every artifact, as an OCI referrer. For compositions it is derived from "+
			"the digest-pinned inputs and is exact rather than scanned; a build's is produced by "+
			"BuildKit and is a scan of the result (ADR 0008).")

	fs.BoolVar(&o.Provenance, "provenance", false,
		"Attach SLSA provenance to every artifact, as an OCI referrer.")

	fs.StringVar(&o.SigningKeySecret, "signing-key-secret", "",
		"Name of a Secret in THIS controller's namespace holding a cosign key pair, created with "+
			"`cosign generate-key-pair k8s://<namespace>/<name>`. Signing is inert until something "+
			"verifies at admission -- see docs/examples/verify. Never nameable by an object: the "+
			"operator signs, tenants do not choose the key.")
}

// Default builds the publish target. namespace is the CONTROLLER's own, from POD_NAMESPACE.
func (o *Registry) Default(namespace string) recon.DefaultRegistry {
	return recon.DefaultRegistry{
		Host:       o.DefaultRegistry,
		PublicHost: o.PublicRegistryHost,
		SecretName: o.DefaultPushSecret,
		Namespace:  namespace,
	}
}

// Insecure is the plain-HTTP host list, split.
func (o *Registry) Insecure() []string { return splitList(o.InsecureRegistries) }

// Transport returns the RoundTripper to talk to registries with, and the CA bytes themselves for
// callers that must pass them on -- the builder projects them into each build pod.
//
// Both are nil when no CA is configured, which is the ordinary case.
func (o *Registry) Transport() (http.RoundTripper, []byte, error) {
	if o.CAFile == "" {
		return nil, nil, nil
	}
	rt, err := recon.Transport(o.CAFile)
	if err != nil {
		return nil, nil, err
	}
	ca, err := os.ReadFile(o.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the registry CA: %w", err)
	}
	return rt, ca, nil
}

// Attestor builds the supply-chain attacher, loading the signing key if one is named.
//
// The key is read HERE, at startup, so a key that cannot sign fails the process rather than the
// first artifact -- the same reasoning the chart applies to an unpinned builder image.
func (o *Registry) Attestor(ctx context.Context, namespace string) (*attest.Attestor, error) {
	a := &attest.Attestor{SBOM: o.SBOM, Provenance: o.Provenance}
	if o.SigningKeySecret == "" {
		return a, nil
	}
	if namespace == "" {
		return nil, fmt.Errorf("POD_NAMESPACE is unset, so the signing key cannot be read")
	}
	key, err := attest.LoadKeyFromCluster(ctx, namespace, o.SigningKeySecret)
	if err != nil {
		return nil, err
	}
	a.Key = key
	return a, nil
}

// splitList turns a comma-separated flag into a slice, dropping empties. Both binaries had their
// own copy.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
