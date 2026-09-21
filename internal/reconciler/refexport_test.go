package reconciler

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

func exportScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func owner() *ociv1alpha1.ImageBuild {
	return &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "pymods", Namespace: "synapse"},
	}
}

func exportSpec() *ociv1alpha1.RefExport {
	return &ociv1alpha1.RefExport{
		Name: "pymods-ref", Namespace: "flux-system",
		Keys: ociv1alpha1.RefExportKeys{Ref: "PYMODS_REF", Digest: "PYMODS_DIGEST"},
	}
}

const (
	testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testRef    = "registry.example/synapse/pymods@" + testDigest
)

// TestAnExportIsRefusedOutsideTheAllowList is the privilege boundary.
//
// The useful target is the consumer's namespace -- flux-system, which parameterises everything --
// so this is a real escalation and the default has to be that nothing is permitted.
func TestAnExportIsRefusedOutsideTheAllowList(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	err := ExportRef(context.Background(), c, owner(), exportSpec(), nil, testDigest, testRef)
	if err == nil {
		t.Fatal("an empty allow-list permitted a write to flux-system")
	}
	if !IsTerminal(err) {
		t.Errorf("must be terminal -- editing the spec or the flag is what fixes it; got %v", err)
	}
	if !strings.Contains(err.Error(), "ref-export-namespaces") {
		t.Errorf("the message must name the flag that governs it: %v", err)
	}
}

// TestAnExportWritesBothFormsAndTheWatchLabel.
//
// The full ref as well as the digest, because a consumer substituting a bare digest produces a
// trailing "@" on an empty string if the key is ever absent. And the watch annotation, because
// without it a new digest is picked up only at the consumer's next interval and its absence is
// invisible.
func TestAnExportWritesBothFormsAndTheWatchLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	if err := ExportRef(context.Background(), c, owner(), exportSpec(),
		[]string{"flux-system"}, testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &cm); err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	if cm.Data["PYMODS_REF"] != testRef {
		t.Errorf("ref = %q, want %q", cm.Data["PYMODS_REF"], testRef)
	}
	if cm.Data["PYMODS_DIGEST"] != testDigest {
		t.Errorf("digest = %q, want %q", cm.Data["PYMODS_DIGEST"], testDigest)
	}
	// A LABEL. kustomize-controller selects these with --watch-configs-label-selector, and a
	// label selector cannot match an annotation -- as an annotation this is inert, and inert in
	// the way the feature is most dangerous: the ConfigMap looks right and nothing rolls out.
	if cm.Labels[WatchLabel] != "Enabled" {
		t.Errorf("watch marker is not a label (labels=%v annotations=%v); a label selector cannot "+
			"match an annotation, so nothing would ever notice this change",
			cm.Labels, cm.Annotations)
	}
	if cm.Labels["app.kubernetes.io/managed-by"] == "" {
		t.Error("the export is indistinguishable from a hand-written substitution source")
	}
}

// TestAnIncompleteReferenceIsNeverWritten.
//
// A missing key substitutes the EMPTY STRING and Flux says nothing about it, so a half-written
// export is worse than none: the consumer deploys an image reference that is silently wrong.
func TestAnIncompleteReferenceIsNeverWritten(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	if err := ExportRef(context.Background(), c, owner(), exportSpec(),
		[]string{"flux-system"}, "", ""); err == nil {
		t.Fatal("an empty reference was exported")
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &cm); err == nil {
		t.Errorf("a ConfigMap was written anyway: %v", cm.Data)
	}
}

// TestAnExportReplacesRatherThanMerges — a consumer must never see one key updated and another
// stale, which is what patching key by key would allow.
func TestAnExportReplacesRatherThanMerges(t *testing.T) {
	// Ours, from a previous export -- a foreign one is refused instead, which
	// TestAForeignConfigMapIsNeverAdopted covers.
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pymods-ref", Namespace: "flux-system",
			Labels: map[string]string{ManagedByLabel: "kube-oci-composer"},
		},
		Data: map[string]string{"PYMODS_REF": "stale", "LEFTOVER": "x"},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(existing).Build()

	if err := ExportRef(context.Background(), c, owner(), exportSpec(),
		[]string{"flux-system"}, testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &cm); err != nil {
		t.Fatal(err)
	}
	if _, stale := cm.Data["LEFTOVER"]; stale {
		t.Error("a key from a previous export survived, so the ConfigMap describes two publishes")
	}
	if cm.Data["PYMODS_REF"] != testRef {
		t.Errorf("ref = %q, want the new one", cm.Data["PYMODS_REF"])
	}
}

// TestAForeignConfigMapIsNeverAdopted.
//
// A substitution source is exactly the kind of object a human writes by hand, and this replaces
// Data wholesale -- so adopting one silently destroys whatever else was in it.
func TestAForeignConfigMapIsNeverAdopted(t *testing.T) {
	theirs := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pymods-ref", Namespace: "flux-system"},
		Data:       map[string]string{"SOMETHING_ELSE": "hand written"},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(theirs).Build()

	err := ExportRef(context.Background(), c, owner(), exportSpec(),
		[]string{"flux-system"}, testDigest, testRef)
	if err == nil {
		t.Fatal("a ConfigMap this controller did not create was taken over")
	}
	if !IsTerminal(err) {
		t.Errorf("must be terminal -- picking another name is what fixes it; got %v", err)
	}

	var after corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Data["SOMETHING_ELSE"] != "hand written" {
		t.Errorf("somebody else's content was replaced: %v", after.Data)
	}
}

// TestExtraMetadataIsAddedButCannotDisableTheFeature.
//
// The passthrough is for a consumer's own conventions. It must not be able to remove the watch
// marker or the ownership label: losing the first silently stops anything noticing a new digest,
// and losing the second makes this indistinguishable from a hand-written ConfigMap -- which is
// what the adoption refusal above keys on.
func TestExtraMetadataIsAddedButCannotDisableTheFeature(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	spec := exportSpec()
	spec.Labels = map[string]string{
		"team":         "synapse",
		WatchLabel:     "Disabled",
		ManagedByLabel: "someone-else",
	}
	spec.Annotations = map[string]string{"note": "generated"}

	if err := ExportRef(context.Background(), c, owner(), spec,
		[]string{"flux-system"}, testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Labels["team"] != "synapse" {
		t.Errorf("the extra label was dropped: %v", cm.Labels)
	}
	if cm.Annotations["note"] != "generated" {
		t.Errorf("the extra annotation was dropped: %v", cm.Annotations)
	}
	if cm.Labels[WatchLabel] != "Enabled" {
		t.Error("a spec label turned the watch marker off, which silently disables the feature")
	}
	if cm.Labels[ManagedByLabel] != "kube-oci-composer" {
		t.Error("a spec label disowned the ConfigMap, so this controller would refuse its own export")
	}
}

// TestDeletingTheObjectRemovesItsExport.
//
// Everything else a build creates is reclaimed on its own: the Secrets belong to its Job and the
// Job belongs to the object (ADR 0050). The export cannot be, because a cross-namespace owner
// reference is invalid -- so it is the one thing that needs deleting deliberately.
func TestDeletingTheObjectRemovesItsExport(t *testing.T) {
	mine := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pymods-ref", Namespace: "flux-system",
			Labels: map[string]string{
				ManagedByLabel:                "kube-oci-composer",
				"oci.lhns.de/owner-namespace": "synapse",
				"oci.lhns.de/owner-name":      "pymods",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(mine).Build()

	if err := DeleteExportedRef(context.Background(), c, owner(), exportSpec()); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	var gone corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &gone); err == nil {
		t.Error("the export survived its object")
	}
}

// TestDeletionLeavesSomebodyElsesExportAlone — two objects may legitimately name ConfigMaps in one
// namespace, and a human may have taken one over.
func TestDeletionLeavesSomebodyElsesExportAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"another object's export", map[string]string{
			ManagedByLabel:                "kube-oci-composer",
			"oci.lhns.de/owner-namespace": "synapse",
			"oci.lhns.de/owner-name":      "something-else",
		}},
		{"not ours at all", map[string]string{"owner": "a human"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theirs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "pymods-ref", Namespace: "flux-system", Labels: tc.labels,
			}}
			c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(theirs).Build()

			if err := DeleteExportedRef(context.Background(), c, owner(), exportSpec()); err != nil {
				t.Fatalf("deleting: %v", err)
			}
			var still corev1.ConfigMap
			if err := c.Get(context.Background(),
				types.NamespacedName{Namespace: "flux-system", Name: "pymods-ref"}, &still); err != nil {
				t.Error("deleted a ConfigMap this object did not export")
			}
		})
	}
}
