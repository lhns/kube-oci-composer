package controller

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// pullOptions builds remote options for pulling a base image.
//
// Separate from the push path deliberately: pulling a base image is a different credential with a
// different scope, and reusing spec.push.secretRef would silently send a push-scoped token to
// whatever registry the base happens to live in.
func (r *ImageCompositionReconciler) pullOptions(ctx context.Context, namespace string, ref *ociv1alpha1.LocalObjectReference) ([]remote.Option, error) {
	if ref == nil {
		return []remote.Option{remote.WithAuth(authn.Anonymous)}, nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Waits rather than stalls: the Secret may be on its way from SOPS, Reflector or
			// a Kustomization applied moments later, and its creation raises no event here.
			return nil, recon.Pending("pull secret %s not found yet", key)
		}
		return nil, fmt.Errorf("reading pull secret %s: %w", key, err)
	}

	kc, err := recon.KeychainFromSecret(&secret)
	if err != nil {
		// The Secret exists but is malformed. Fixing it means editing the SECRET, which does
		// not bump this generation — so this waits rather than stalls.
		return nil, recon.Pending("pull secret %s is unusable: %v", key, err)
	}
	return []remote.Option{remote.WithAuthFromKeychain(kc)}, nil
}
