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
// Shared by the composer, the builder and the refresher so they agree. The credential rule lives
// in DefaultRegistry.CredentialFor, and binds the read-only refresher exactly as it binds the
// publish paths: a credential sent to a tenant-chosen host is exfiltrated either way.
type RemoteAuth struct {
	// Reader fetches the Secret. A client.Reader rather than a full client: nothing here writes.
	Reader client.Reader

	// Transport, when set, trusts an additional CA on top of the system roots.
	Transport http.RoundTripper

	// Default decides whose credential applies to which host.
	Default DefaultRegistry

	// Soft reports a credential that is absent or unusable, which usually resolves on its own as
	// the Secret arrives. A reconciler passes Pending; a background refresher, with no object to make
	// pending, passes fmt.Errorf. Nil means Pending.
	Soft func(format string, args ...any) error
}

// Options returns the remote options for one repository.
//
// The repository is passed in because each caller derives it differently.
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
