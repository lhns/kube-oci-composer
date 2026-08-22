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
// Uses a direct client rather than a manager's cache because it runs before the manager starts: a
// key that cannot be read or cannot sign should fail the process at boot, not the first artifact.
//
// The namespace is always the CONTROLLER's own, never an object's — the operator signs, and a
// tenant does not choose which key their artifact is signed with. Same rule as the push credential
// (see DefaultRegistry.CredentialFor in internal/reconciler).
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
