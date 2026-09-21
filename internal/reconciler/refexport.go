package reconciler

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// ManagedByLabel marks what this controller generated, and is what stops it adopting anything else.
const ManagedByLabel = "app.kubernetes.io/managed-by"

const (
	managedBy           = "kube-oci-composer"
	ownerNamespaceLabel = "oci.lhns.de/owner-namespace"
	ownerNameLabel      = "oci.lhns.de/owner-name"
)

// ExportOptions is what the operator, rather than the object, decides about an export.
type ExportOptions struct {
	// Namespaces a target OTHER than the object's own must appear in. Empty permits none.
	Namespaces []string
	// WatchLabels are added to every generated ConfigMap, so a consumer notices it change.
	WatchLabels map[string]string
	// AllowedLabels and AllowedAnnotations gate what the object may set. Entries are exact keys,
	// or a prefix with a trailing "*". Empty permits none.
	AllowedLabels      []string
	AllowedAnnotations []string
}

// ExportName is the ConfigMap an object writes, derived from the object and never chosen.
//
// Two objects therefore cannot ask for the same ConfigMap, which is what makes taking over another
// object's export impossible rather than merely refused; and because an object's name cannot
// change, neither can its export's (ADR 0056). The kind is in it because both kinds share a
// namespace; the namespace is in it because one target collects exports from the whole cluster.
// Takes a Scheme rather than reading the object's TypeMeta: controller-runtime clears that on a
// typed read, so the GVK on a fetched object is empty and the name would silently lose its kind.
func ExportName(obj client.Object, scheme *runtime.Scheme) (string, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return "", fmt.Errorf("determining the kind of %T: %w", obj, err)
	}
	return fmt.Sprintf("%s-%s-%s", strings.ToLower(gvk.Kind), obj.GetNamespace(), obj.GetName()), nil
}

// ExportRef writes the published reference into the ConfigMap this object owns.
//
// Only after a confirmed publish, and all keys or none: a consumer substitutes whatever it finds,
// a missing key substitutes the empty string with no complaint, and the ConfigMap is replaced
// wholesale so one key is never updated while another is stale.
//
// Returns what was written, for status.refExport.
func ExportRef(
	ctx context.Context, c client.Client, obj client.Object, spec *ociv1alpha1.RefExport,
	opts ExportOptions, digest, ref string,
) (*ociv1alpha1.RefExportStatus, error) {
	if spec == nil {
		return nil, nil
	}
	if digest == "" || ref == "" {
		return nil, fmt.Errorf("refusing to export an incomplete reference (digest %q, ref %q)", digest, ref)
	}
	if spec.Namespace != obj.GetNamespace() && !contains(opts.Namespaces, spec.Namespace) {
		return nil, Terminal(
			"push.writeRefTo names namespace %q, which is neither this object's own nor permitted "+
				"by the controller: add it to --ref-export-namespaces", spec.Namespace)
	}

	name, err := ExportName(obj, c.Scheme())
	if err != nil {
		return nil, err
	}
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		return nil, Terminal(
			"the exported ConfigMap would be named %q, which is not a valid name: %s",
			name, strings.Join(msgs, "; "))
	}
	if err := checkKeys(spec.Labels, opts.AllowedLabels, "labels"); err != nil {
		return nil, err
	}
	if err := checkKeys(spec.Annotations, opts.AllowedAnnotations, "annotations"); err != nil {
		return nil, err
	}

	data := map[string]string{}
	if spec.Keys.Ref != "" {
		data[spec.Keys.Ref] = ref
	}
	if spec.Keys.Digest != "" {
		data[spec.Keys.Digest] = digest
	}

	written := &ociv1alpha1.RefExportStatus{Name: name, Namespace: spec.Namespace}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spec.Namespace}}
	err = c.Get(ctx, types.NamespacedName{Namespace: spec.Namespace, Name: name}, cm)
	switch {
	case apierrors.IsNotFound(err):
		decorate(cm, obj, spec, opts.WatchLabels)
		cm.Data = data
		// A cross-namespace owner reference is invalid, so only a same-namespace export can be
		// reclaimed by Kubernetes. The rest is what the finalizer is for.
		if spec.Namespace == obj.GetNamespace() {
			if err := ctrl.SetControllerReference(obj, cm, c.Scheme()); err != nil {
				return nil, fmt.Errorf("owning %s/%s: %w", spec.Namespace, name, err)
			}
		}
		return written, c.Create(ctx, cm)
	case err != nil:
		return nil, fmt.Errorf("reading %s/%s: %w", spec.Namespace, name, err)
	}

	// Writing over a ConfigMap this controller did not create for THIS object would replace its
	// contents wholesale. A substitution source is exactly the kind of object a human writes by
	// hand, and another object's export is one a consumer is already reading.
	if !ownedBy(cm, obj) {
		return nil, Terminal(
			"ConfigMap %s/%s exists and is not this object's export (managed-by %q, owner %s/%s); "+
				"push.writeRefTo will not overwrite it",
			spec.Namespace, name, cm.Labels[ManagedByLabel],
			cm.Labels[ownerNamespaceLabel], cm.Labels[ownerNameLabel])
	}

	decorate(cm, obj, spec, opts.WatchLabels)
	cm.Data = data
	return written, c.Update(ctx, cm)
}

// DeleteExportedRef removes a ConfigMap this controller wrote for this object.
//
// Needed because a cross-namespace owner reference is invalid, so Kubernetes will not reclaim one
// written into another namespace -- everything else a build creates it does (ADR 0050). Takes what
// was actually written rather than what the spec now asks for, so moving or removing writeRefTo
// cleans up rather than strands.
func DeleteExportedRef(
	ctx context.Context, c client.Client, obj client.Object, written *ociv1alpha1.RefExportStatus,
) error {
	if written == nil {
		return nil
	}
	var cm corev1.ConfigMap
	err := c.Get(ctx, types.NamespacedName{Namespace: written.Namespace, Name: written.Name}, &cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s/%s: %w", written.Namespace, written.Name, err)
	}
	// This controller holds ConfigMap delete on every namespace (ADR 0056), so this predicate is
	// the boundary, not RBAC. It is the same one ExportRef refuses to write past.
	if !ownedBy(&cm, obj) {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, &cm))
}

// ownedBy reports whether this ConfigMap is the export this controller wrote for this object.
//
// One predicate for both writing and deleting: when those disagreed, an object could overwrite
// another object's export while refusing to delete it.
func ownedBy(cm *corev1.ConfigMap, obj client.Object) bool {
	return cm.Labels[ManagedByLabel] == managedBy &&
		cm.Labels[ownerNamespaceLabel] == obj.GetNamespace() &&
		cm.Labels[ownerNameLabel] == obj.GetName()
}

// decorate marks the ConfigMap as this object's, and makes a consumer notice it change.
func decorate(
	cm *corev1.ConfigMap, obj client.Object, spec *ociv1alpha1.RefExport, watch map[string]string,
) {
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	for k, v := range spec.Labels {
		cm.Labels[k] = v
	}
	// After the object's, so neither the watch marker nor the ownership labels can be turned off by
	// a spec that sets the same keys -- losing the first silently disables the feature, and losing
	// the others makes this ConfigMap indistinguishable from a hand-written one.
	for k, v := range watch {
		cm.Labels[k] = v
	}
	cm.Labels[ManagedByLabel] = managedBy
	cm.Labels[ownerNamespaceLabel] = obj.GetNamespace()
	cm.Labels[ownerNameLabel] = obj.GetName()

	if len(spec.Annotations) > 0 && cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	for k, v := range spec.Annotations {
		cm.Annotations[k] = v
	}
}

// checkKeys refuses metadata the operator has not permitted.
//
// Refused rather than dropped: a dropped key leaves a ConfigMap that looks correct while whatever
// was meant to select on it never does.
func checkKeys(set map[string]string, allowed []string, what string) error {
	for k := range set {
		if !keyAllowed(k, allowed) {
			return Terminal(
				"push.writeRefTo.%s sets %q, which the controller does not permit; "+
					"add it to --ref-export-allowed-%s", what, k, what)
		}
	}
	return nil
}

func keyAllowed(key string, allowed []string) bool {
	for _, a := range allowed {
		if prefix, ok := strings.CutSuffix(a, "*"); ok {
			if strings.HasPrefix(key, prefix) {
				return true
			}
			continue
		}
		if a == key {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

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
