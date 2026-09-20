package reconciler

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// WatchAnnotation is what makes kustomize-controller notice a substitution source changing.
//
// Set by this controller rather than left to the user: without it a new digest is picked up only
// at the consumer's next interval, and its absence is invisible -- the ConfigMap looks correct and
// the rollout simply does not happen.
const WatchAnnotation = "reconcile.fluxcd.io/watch"

// ExportRef writes the published reference into the ConfigMap the object names.
//
// Callers pass the digest and the PULL reference, and only after a confirmed publish. Writing
// speculatively is the one thing this must never do: a consumer substitutes whatever is there, and
// a missing key substitutes the empty string silently -- Flux has no server-side strict mode.
//
// All keys or none: the ConfigMap is replaced wholesale rather than patched key by key, so a
// consumer never observes one field updated and another stale.
func ExportRef(
	ctx context.Context, c client.Client, obj client.Object, spec *ociv1alpha1.RefExport,
	allowed []string, digest, ref string,
) error {
	if spec == nil {
		return nil
	}
	if digest == "" || ref == "" {
		return fmt.Errorf("refusing to export an incomplete reference (digest %q, ref %q)", digest, ref)
	}
	if !namespaceAllowed(spec.Namespace, allowed) {
		return Terminal(
			"push.writeRefTo names namespace %q, which this controller is not permitted to write to: "+
				"add it to --ref-export-namespaces", spec.Namespace)
	}

	data := map[string]string{}
	if spec.Keys.Ref != "" {
		data[spec.Keys.Ref] = ref
	}
	if spec.Keys.Digest != "" {
		data[spec.Keys.Digest] = digest
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace},
	}
	err := c.Get(ctx, types.NamespacedName{Namespace: spec.Namespace, Name: spec.Name}, cm)
	switch {
	case apierrors.IsNotFound(err):
		decorate(cm, obj)
		cm.Data = data
		return c.Create(ctx, cm)
	case err != nil:
		return fmt.Errorf("reading %s/%s: %w", spec.Namespace, spec.Name, err)
	}

	decorate(cm, obj)
	cm.Data = data
	return c.Update(ctx, cm)
}

// decorate marks the ConfigMap as this controller's, and makes a consumer notice it change.
//
// Labelled rather than owner-referenced: a cross-namespace owner reference is invalid, and the
// useful target for a substitution source is the CONSUMER's namespace, not the object's. So this
// is not garbage-collected with the object, which is stated in ADR 0055 rather than discovered.
func decorate(cm *corev1.ConfigMap, obj client.Object) {
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels["app.kubernetes.io/managed-by"] = "kube-oci-composer"
	cm.Labels["oci.lhns.de/owner-namespace"] = obj.GetNamespace()
	cm.Labels["oci.lhns.de/owner-name"] = obj.GetName()
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[WatchAnnotation] = "Enabled"
}

func namespaceAllowed(ns string, allowed []string) bool {
	for _, a := range allowed {
		if a == ns {
			return true
		}
	}
	return false
}
