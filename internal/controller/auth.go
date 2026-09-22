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
// Separate from the push path: reusing spec.push.secretRef would send a push-scoped token to
// whatever registry the base lives in.
func (r *ImageCompositionReconciler) pullOptions(ctx context.Context, namespace string, ref *ociv1alpha1.LocalObjectReference) ([]remote.Option, error) {
	// The CA applies here too: a composition layered on an ImageBuild's output pulls its base from
	// the bundled registry.
	var opts []remote.Option
	if r.Transport != nil {
		opts = append(opts, remote.WithTransport(r.Transport))
	}
	if ref == nil {
		return append(opts, remote.WithAuth(authn.Anonymous)), nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Pending: creating the Secret raises no event on this object.
			return nil, recon.Pending("pull secret %s not found yet", key)
		}
		return nil, fmt.Errorf("reading pull secret %s: %w", key, err)
	}

	kc, err := recon.KeychainFromSecret(&secret)
	if err != nil {
		// Pending: fixing the Secret does not bump this generation.
		return nil, recon.Pending("pull secret %s is unusable: %v", key, err)
	}
	return append(opts, remote.WithAuthFromKeychain(kc)), nil
}
