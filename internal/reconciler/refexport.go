package reconciler

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// DefaultWatchLabels is what makes kustomize-controller notice a substitution source changing.
//
// A LABEL: it is selected with --watch-configs-label-selector, and a label selector cannot match an
// annotation. Set by the controller because its absence is invisible; a default rather than a
// constant because which tool watches is the cluster's business. ADR 0055.
var DefaultWatchLabels = map[string]string{"reconcile.fluxcd.io/watch": "Enabled"}

// ExportRef writes the published reference into the ConfigMap the object names.
//
// Only after a confirmed publish, and all keys or none: a consumer substitutes whatever it finds,
// a missing key substitutes the empty string with no complaint, and the ConfigMap is replaced
// wholesale so one key is never updated while another is stale.
func ExportRef(
	ctx context.Context, c client.Client, obj client.Object, spec *ociv1alpha1.RefExport,
	allowed []string, watch map[string]string, digest, ref string,
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
		decorate(cm, obj, spec, watch)
		cm.Data = data
		return c.Create(ctx, cm)
	case err != nil:
		return fmt.Errorf("reading %s/%s: %w", spec.Namespace, spec.Name, err)
	}

	// Adopting somebody else's ConfigMap would replace its contents wholesale, and a substitution
	// source is exactly the kind of object a human writes by hand. Refuse rather than take it over.
	if cm.Labels[ManagedByLabel] != managedBy {
		return Terminal(
			"ConfigMap %s/%s already exists and was not created by this controller; "+
				"push.writeRefTo will not overwrite it -- choose another name or remove it",
			spec.Namespace, spec.Name)
	}

	decorate(cm, obj, spec, watch)
	cm.Data = data
	return c.Update(ctx, cm)
}

// decorate marks the ConfigMap as this controller's, and makes a consumer notice it change.
//
// Labelled rather than owner-referenced: a cross-namespace owner reference is invalid, and the
// useful target for a substitution source is the CONSUMER's namespace, not the object's. So this
// is not garbage-collected with the object, which is stated in ADR 0055 rather than discovered.
func decorate(
	cm *corev1.ConfigMap, obj client.Object, spec *ociv1alpha1.RefExport, watch map[string]string,
) {
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	for k, v := range spec.Labels {
		cm.Labels[k] = v
	}
	// After the user's, so neither the watch marker nor the ownership labels can be turned off by
	// a spec that sets the same keys -- losing the first silently disables the feature, and losing
	// the others makes this object indistinguishable from a hand-written one.
	for k, v := range watch {
		cm.Labels[k] = v
	}
	cm.Labels[ManagedByLabel] = managedBy
	cm.Labels["oci.lhns.de/owner-namespace"] = obj.GetNamespace()
	cm.Labels["oci.lhns.de/owner-name"] = obj.GetName()

	if len(spec.Annotations) > 0 && cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	for k, v := range spec.Annotations {
		cm.Annotations[k] = v
	}
}

// ManagedByLabel marks what this controller generated, and is what stops it adopting anything else.
const ManagedByLabel = "app.kubernetes.io/managed-by"

const managedBy = "kube-oci-composer"

func namespaceAllowed(ns string, allowed []string) bool {
	for _, a := range allowed {
		if a == ns {
			return true
		}
	}
	return false
}

// DeleteExportedRef removes a ConfigMap this controller wrote for an object being deleted.
//
// Needed because a cross-namespace owner reference is invalid, so Kubernetes will not reclaim it --
// everything else a build creates is (ADR 0050). Matches on the managed-by AND owner labels, so
// another object's export, or one a human took over, is left alone.
func DeleteExportedRef(
	ctx context.Context, c client.Client, obj client.Object, spec *ociv1alpha1.RefExport,
) error {
	if spec == nil {
		return nil
	}
	var cm corev1.ConfigMap
	err := c.Get(ctx, types.NamespacedName{Namespace: spec.Namespace, Name: spec.Name}, &cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s/%s: %w", spec.Namespace, spec.Name, err)
	}
	if cm.Labels[ManagedByLabel] != managedBy ||
		cm.Labels["oci.lhns.de/owner-namespace"] != obj.GetNamespace() ||
		cm.Labels["oci.lhns.de/owner-name"] != obj.GetName() {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, &cm))
}

// DefaultWatchLabelFlag is DefaultWatchLabels in the form the flag takes.
const DefaultWatchLabelFlag = "reconcile.fluxcd.io/watch=Enabled"

// ParseLabels turns a comma-separated key=value flag into labels.
//
// Malformed pairs are dropped rather than refused: an unparseable entry here would otherwise stop
// the controller starting over a cosmetic setting, and the label's absence shows up the first time
// a consumer does not notice a change.
func ParseLabels(v string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" {
			continue
		}
		out[k] = val
	}
	return out
}
