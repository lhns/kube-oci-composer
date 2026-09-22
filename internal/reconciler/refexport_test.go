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
	// The exported name carries the kind, which comes from the scheme: typed reads clear TypeMeta.
	if err := ociv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func owner() *ociv1alpha1.ImageBuild {
	return &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
	}
}

func exportSpec() *ociv1alpha1.RefExport {
	return &ociv1alpha1.RefExport{
		Namespace: "flux-system",
		Keys:      ociv1alpha1.RefExportKeys{Ref: "APP_REF", Digest: "APP_DIGEST"},
	}
}

// exportedName is what owner() writes: derived from the object, never chosen.
const exportedName = "imagebuild-team-a-app"

// wroteTo is the status record a previous export would have left.
func wroteTo(ns string) *ociv1alpha1.RefExportStatus {
	return &ociv1alpha1.RefExportStatus{Name: exportedName, Namespace: ns}
}

// ourLabels are what decorate() stamps, for a ConfigMap a test pre-creates as ours.
func ourLabels() map[string]string {
	return map[string]string{
		ManagedByLabel:      managedBy,
		ownerNamespaceLabel: "team-a",
		ownerNameLabel:      "app",
	}
}

// allowingFlux is the operator configuration most of these tests run under.
func allowingFlux() ExportOptions {
	return ExportOptions{Namespaces: []string{"flux-system"}, WatchLabels: fluxWatch}
}

const (
	testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testRef    = "registry.example/team-a/app@" + testDigest
)

// TestAnExportIsRefusedOutsideTheAllowList: writing into flux-system parameterises everything, so
// the default must be that no foreign namespace is permitted.
func TestAnExportIsRefusedOutsideTheAllowList(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	_, err := ExportRef(context.Background(), c, owner(), exportSpec(), ExportOptions{WatchLabels: fluxWatch}, testDigest, testRef)
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

// TestAnExportWritesBothFormsAndTheWatchLabel: the full ref as well as the digest (a consumer
// composing "repo@${DIGEST}" breaks silently on an absent key), and the watch label, without which
// a new digest waits for the consumer's next interval.
func TestAnExportWritesBothFormsAndTheWatchLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	if _, err := ExportRef(context.Background(), c, owner(), exportSpec(),
		allowingFlux(), testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &cm); err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	if cm.Data["APP_REF"] != testRef {
		t.Errorf("ref = %q, want %q", cm.Data["APP_REF"], testRef)
	}
	if cm.Data["APP_DIGEST"] != testDigest {
		t.Errorf("digest = %q, want %q", cm.Data["APP_DIGEST"], testDigest)
	}
	// A label, not an annotation: kustomize-controller selects with --watch-configs-label-selector.
	if cm.Labels["reconcile.fluxcd.io/watch"] != "Enabled" {
		t.Errorf("watch marker is not a label (labels=%v annotations=%v); a label selector cannot "+
			"match an annotation, so nothing would ever notice this change",
			cm.Labels, cm.Annotations)
	}
	if cm.Labels["app.kubernetes.io/managed-by"] == "" {
		t.Error("the export is indistinguishable from a hand-written substitution source")
	}
}

// TestAnIncompleteReferenceIsNeverWritten: Flux silently substitutes an empty string for a missing
// key, so a half-written export is worse than none.
func TestAnIncompleteReferenceIsNeverWritten(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	if _, err := ExportRef(context.Background(), c, owner(), exportSpec(),
		allowingFlux(), "", ""); err == nil {
		t.Fatal("an empty reference was exported")
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &cm); err == nil {
		t.Errorf("a ConfigMap was written anyway: %v", cm.Data)
	}
}

// TestAnExportReplacesRatherThanMerges — a consumer must never see one key updated and another
// stale, which is what patching key by key would allow.
func TestAnExportReplacesRatherThanMerges(t *testing.T) {
	// Ours, from a previous export (a foreign one is refused: TestAForeignConfigMapIsNeverAdopted).
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: exportedName, Namespace: "flux-system",
			Labels: ourLabels(),
		},
		Data: map[string]string{"APP_REF": "stale", "LEFTOVER": "x"},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(existing).Build()

	if _, err := ExportRef(context.Background(), c, owner(), exportSpec(),
		allowingFlux(), testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &cm); err != nil {
		t.Fatal(err)
	}
	if _, stale := cm.Data["LEFTOVER"]; stale {
		t.Error("a key from a previous export survived, so the ConfigMap describes two publishes")
	}
	if cm.Data["APP_REF"] != testRef {
		t.Errorf("ref = %q, want the new one", cm.Data["APP_REF"])
	}
}

// TestAForeignConfigMapIsNeverAdopted: an export replaces Data wholesale, so adopting a
// hand-written ConfigMap would destroy its contents.
func TestAForeignConfigMapIsNeverAdopted(t *testing.T) {
	theirs := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: exportedName, Namespace: "flux-system"},
		Data:       map[string]string{"SOMETHING_ELSE": "hand written"},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(theirs).Build()

	_, err := ExportRef(context.Background(), c, owner(), exportSpec(),
		allowingFlux(), testDigest, testRef)
	if err == nil {
		t.Fatal("a ConfigMap this controller did not create was taken over")
	}
	if !IsTerminal(err) {
		t.Errorf("must be terminal -- picking another name is what fixes it; got %v", err)
	}

	var after corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Data["SOMETHING_ELSE"] != "hand written" {
		t.Errorf("somebody else's content was replaced: %v", after.Data)
	}
}

// TestExtraMetadataIsAddedButCannotDisableTheFeature: spec labels must not override the watch
// marker (nothing would notice a new digest) or the ownership label (the adoption check keys on it).
func TestExtraMetadataIsAddedButCannotDisableTheFeature(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	spec := exportSpec()
	spec.Labels = map[string]string{
		"team":                      "team-a",
		"reconcile.fluxcd.io/watch": "Disabled",
		ManagedByLabel:              "someone-else",
	}
	spec.Annotations = map[string]string{"note": "generated"}

	// Every key permitted, so this tests the ordering, not the allow-list gate.
	opts := allowingFlux()
	opts.AllowedLabels = []string{"team", "reconcile.fluxcd.io/watch", ManagedByLabel}
	opts.AllowedAnnotations = []string{"note"}

	if _, err := ExportRef(context.Background(), c, owner(), spec,
		opts, testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Labels["team"] != "team-a" {
		t.Errorf("the extra label was dropped: %v", cm.Labels)
	}
	if cm.Annotations["note"] != "generated" {
		t.Errorf("the extra annotation was dropped: %v", cm.Annotations)
	}
	if cm.Labels["reconcile.fluxcd.io/watch"] != "Enabled" {
		t.Error("a spec label turned the watch marker off, which silently disables the feature")
	}
	if cm.Labels[ManagedByLabel] != "kube-oci-composer" {
		t.Error("a spec label disowned the ConfigMap, so this controller would refuse its own export")
	}
}

// TestDeletingTheObjectRemovesItsExport: a cross-namespace owner reference is invalid, so unlike
// everything else a build creates (ADR 0050) the export must be deleted explicitly.
func TestDeletingTheObjectRemovesItsExport(t *testing.T) {
	mine := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: exportedName, Namespace: "flux-system",
			Labels: ourLabels(),
		},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(mine).Build()

	if err := DeleteExportedRef(context.Background(), c, owner(), wroteTo("flux-system")); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	var gone corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &gone); err == nil {
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
			"oci.lhns.de/owner-namespace": "team-a",
			"oci.lhns.de/owner-name":      "something-else",
		}},
		{"not ours at all", map[string]string{"owner": "a human"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theirs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: exportedName, Namespace: "flux-system", Labels: tc.labels,
			}}
			c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(theirs).Build()

			if err := DeleteExportedRef(context.Background(), c, owner(), wroteTo("flux-system")); err != nil {
				t.Fatalf("deleting: %v", err)
			}
			var still corev1.ConfigMap
			if err := c.Get(context.Background(),
				types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &still); err != nil {
				t.Error("deleted a ConfigMap this object did not export")
			}
		})
	}
}

// TestTheWatchMarkerIsConfigurable — ADR 0009 borrows Flux's conventions without depending on
// them, so an operator running something else can choose the marker, or none.
func TestTheWatchMarkerIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		watch map[string]string
		want  map[string]string
		gone  []string
	}{
		{"flux, when asked for", fluxWatch,
			map[string]string{"reconcile.fluxcd.io/watch": "Enabled"}, nil},
		{"something else entirely", map[string]string{"argocd.argoproj.io/watch": "true"},
			map[string]string{"argocd.argoproj.io/watch": "true"},
			[]string{"reconcile.fluxcd.io/watch"}},
		{"none at all", nil, nil, []string{"reconcile.fluxcd.io/watch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()
			if _, err := ExportRef(context.Background(), c, owner(), exportSpec(),
				ExportOptions{Namespaces: []string{"flux-system"}, WatchLabels: tc.watch},
				testDigest, testRef); err != nil {
				t.Fatalf("exporting: %v", err)
			}
			var cm corev1.ConfigMap
			if err := c.Get(context.Background(),
				types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &cm); err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.want {
				if cm.Labels[k] != v {
					t.Errorf("label %s = %q, want %q", k, cm.Labels[k], v)
				}
			}
			for _, k := range tc.gone {
				if _, ok := cm.Labels[k]; ok {
					t.Errorf("label %s was set although it was not asked for", k)
				}
			}
			// Ownership is never optional: the adoption refusal and the finalizer both key on it.
			if cm.Labels[ManagedByLabel] != "kube-oci-composer" {
				t.Error("the export disowned itself")
			}
		})
	}
}

// TestParseLabelsDropsWhatItCannotRead — an unparseable entry must not stop the controller starting.
func TestParseLabelsDropsWhatItCannotRead(t *testing.T) {
	got := ParseLabels("a=1, b=2 ,,garbage,=3,c=")
	want := map[string]string{"a": "1", "b": "2", "c": ""}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// fluxWatch is what an operator sets for Flux; the controller adds no watch label by default.
var fluxWatch = map[string]string{"reconcile.fluxcd.io/watch": "Enabled"}
