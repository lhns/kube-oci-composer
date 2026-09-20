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
	if cm.Annotations[WatchAnnotation] != "Enabled" {
		t.Error("no watch annotation, so a consumer picks the change up only at its next interval " +
			"and nothing says why")
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
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pymods-ref", Namespace: "flux-system"},
		Data:       map[string]string{"PYMODS_REF": "stale", "LEFTOVER": "x"},
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
