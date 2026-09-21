package reconciler

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// RemoteAuth builds the options for talking to a registry: the transport to trust, and the
// credential to present.
//
// One implementation, because there were three -- the composer's, the builder's and the
// refresher's -- and transport.go states the rule this file now keeps: "all four places that talk
// to a registry have to agree about what is trusted, and a fourth copy of the decision is how they
// stop agreeing." The copies were still identical when they were found, which is the good case;
// the point is that nothing was holding them that way.
//
// The credential rule is the load-bearing part and it lives in DefaultRegistry.CredentialFor: the
// operator's credential reaches the operator's own registry and nowhere else. A credential sent to
// a host a tenant chose is exfiltrated whether the request carrying it reads or writes, so the
// refresher is bound by it exactly as the publish paths are.
type RemoteAuth struct {
	// Reader fetches the Secret. A client.Reader rather than a full client: nothing here writes.
	Reader client.Reader

	// Transport, when set, trusts an additional CA on top of the system roots.
	Transport http.RoundTripper

	// Default decides whose credential applies to which host.
	Default DefaultRegistry

	// Soft reports a credential that is absent or unusable -- conditions that ordinarily resolve
	// on their own, as a Secret arrives from SOPS or a Kustomization applied moments later.
	//
	// A reconciler passes Pending, so the object waits and says what it is waiting for. A
	// background refresher passes fmt.Errorf, because it has no object to make pending and a
	// failure there is counted rather than surfaced. Nil means Pending.
	Soft func(format string, args ...any) error
}

// Options returns the remote options for one repository.
//
// The repository is passed in rather than derived, because each caller resolves it differently --
// a build's push target, a composition's write repo, a reference read back out of status -- and
// that derivation is the part that legitimately differs.
func (a RemoteAuth) Options(
	ctx context.Context, namespace, repository string, push *ociv1alpha1.Push,
) ([]remote.Option, error) {
	opts := []remote.Option{remote.WithContext(ctx)}
	if a.Transport != nil {
		opts = append(opts, remote.WithTransport(a.Transport))
	}

	soft := a.Soft
	if soft == nil {
		soft = Pending
	}

	var ownRef string
	if push != nil && push.SecretRef != nil {
		ownRef = push.SecretRef.Name
	}
	name, ns := a.Default.CredentialFor(namespace, ownRef, repository)
	if name == "" {
		return append(opts, remote.WithAuth(authn.Anonymous)), nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := a.Reader.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, soft("push secret %s not found yet", key)
		}
		return nil, fmt.Errorf("reading push secret %s: %w", key, err)
	}

	kc, err := KeychainFromSecret(&secret)
	if err != nil {
		return nil, soft("push secret %s is unusable: %v", key, err)
	}
	return append(opts, remote.WithAuthFromKeychain(kc)), nil
}
