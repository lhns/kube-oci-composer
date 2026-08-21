package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lhns/kube-oci-composer/internal/attest"
)

// loadSigningKey reads the cosign key pair from the CONTROLLER's own namespace.
//
// Read with a direct client rather than through the manager's cache: this runs before the manager
// starts, and it must fail startup rather than the first artifact.
//
// The namespace is the controller's, never an object's, on the same rule as the push credential
// (recon.DefaultRegistry.CredentialFor): the operator signs, and a tenant does not get to choose
// which key their artifact is signed with.
func loadSigningKey(namespace, name string) (*attest.Key, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("building a client: %w", err)
	}

	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, fmt.Errorf("reading %s/%s: %w", namespace, name, err)
	}
	return attest.LoadKey(&secret)
}
