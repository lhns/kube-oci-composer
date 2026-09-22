package attest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LoadKeyFromCluster reads the cosign key pair from a Secret and returns a ready signer.
//
// It uses a direct client because it runs before the manager starts, so a bad key fails the
// process at boot. The namespace is always the controller's own: tenants do not choose the signing
// key (same rule as DefaultRegistry.CredentialFor in internal/reconciler).
func LoadKeyFromCluster(ctx context.Context, namespace, name string) (*Key, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("building a client: %w", err)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, fmt.Errorf("reading %s/%s: %w", namespace, name, err)
	}
	return LoadKey(&secret)
}
