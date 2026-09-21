package reconciler

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// What an allow-listed namespace does NOT permit.
//
// The allow-list says which namespace may be written to and nothing about who may write what
// inside it. These are the boundaries that hold there instead -- and since ADR 0056 put the
// ClusterRole's ConfigMap verbs on every namespace, several of them have nothing behind them.

// TestOneObjectCannotTakeOverAnothersExport.
//
// Before the name was derived, a second object could name the same ConfigMap and, because the
// managed-by label matched, simply update it -- repointing a consuming Kustomization at its own
// image. The derived name makes that unaskable; this asserts it, on content rather than on an
// error, because the failure that matters is the first object's value changing.
func TestOneObjectCannotTakeOverAnothersExport(t *testing.T) {
	theirs := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: exportedName, Namespace: "flux-system", Labels: ourLabels(),
		},
		Data: map[string]string{"APP_REF": "the first object's image"},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(theirs).Build()

	intruder := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "intruder", Namespace: "team-a"},
	}
	if _, err := ExportRef(context.Background(), c, intruder, exportSpec(), allowingFlux(),
		testDigest, "registry.example/team-a/intruder@"+testDigest); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	// It landed under its own name, so it could not have collided at all.
	var mine corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: "imagebuild-team-a-intruder"},
		&mine); err != nil {
		t.Fatalf("the export did not land under the intruder's own name: %v", err)
	}

	var victim corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &victim); err != nil {
		t.Fatal(err)
	}
	if victim.Data["APP_REF"] != "the first object's image" {
		t.Errorf("another object's export was repointed: %v", victim.Data)
	}
}

// TestTheOwnerCheckRefusesEvenAManagedConfigMap tests the predicate directly.
//
// The derived name is what normally prevents this, so without a test aimed at the guard itself it
// could rot unnoticed -- which is how it came to be applied on delete but not on write.
func TestTheOwnerCheckRefusesEvenAManagedConfigMap(t *testing.T) {
	someoneElses := ourLabels()
	someoneElses[ownerNameLabel] = "a-different-build"
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: exportedName, Namespace: "flux-system", Labels: someoneElses,
		},
		Data: map[string]string{"APP_REF": "theirs"},
	}
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).WithObjects(existing).Build()

	_, err := ExportRef(context.Background(), c, owner(), exportSpec(), allowingFlux(),
		testDigest, testRef)
	if err == nil {
		t.Fatal("wrote over a ConfigMap owned by another object")
	}
	if !IsTerminal(err) {
		t.Errorf("must be terminal; got %v", err)
	}

	var after corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Data["APP_REF"] != "theirs" {
		t.Errorf("their content was replaced: %v", after.Data)
	}
}

// TestWhichNamespacesAreWritable is the boundary the API server no longer holds.
//
// The ClusterRole carries ConfigMap write on every namespace, because RBAC is granted before an
// object exists (ADR 0056). This is what stops it reaching one nobody permitted, so the third row
// has nothing behind it.
func TestWhichNamespacesAreWritable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		allowed []string
		ok      bool
	}{
		{"its own, always", "team-a", nil, true},
		{"one the operator named", "flux-system", []string{"flux-system"}, true},
		{"one nobody named", "kube-system", []string{"flux-system"}, false},
		{"its own, even alongside an allow-list", "team-a", []string{"flux-system"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()
			spec := exportSpec()
			spec.Namespace = tc.target

			written, err := ExportRef(context.Background(), c, owner(), spec,
				ExportOptions{Namespaces: tc.allowed, WatchLabels: fluxWatch}, testDigest, testRef)

			if tc.ok {
				if err != nil {
					t.Fatalf("refused a permitted namespace: %v", err)
				}
				if written.Namespace != tc.target || written.Name != exportedName {
					t.Errorf("recorded %v, want %s/%s", written, tc.target, exportedName)
				}
				return
			}
			if err == nil {
				t.Fatal("wrote into a namespace nobody permitted")
			}
			if !IsTerminal(err) {
				t.Errorf("must be terminal; got %v", err)
			}
			if !strings.Contains(err.Error(), "ref-export-namespaces") {
				t.Errorf("the message must name the flag that governs it: %v", err)
			}
			var cm corev1.ConfigMap
			if err := c.Get(context.Background(),
				types.NamespacedName{Namespace: tc.target, Name: exportedName}, &cm); err == nil {
				t.Error("a ConfigMap was written anyway")
			}
		})
	}
}

// TestAnOwnNamespaceExportIsOwnedRatherThanFinalized.
//
// A cross-namespace owner reference is invalid, which is the only reason the finalizer exists. An
// export into the object's own namespace is reclaimed by Kubernetes instead, so that object's
// deletion stops depending on this controller running at all.
func TestAnOwnNamespaceExportIsOwnedRatherThanFinalized(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		wantOwner bool
	}{
		{"its own namespace", "team-a", true},
		{"somebody else's", "flux-system", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()
			spec := exportSpec()
			spec.Namespace = tc.namespace

			if _, err := ExportRef(context.Background(), c, owner(), spec, allowingFlux(),
				testDigest, testRef); err != nil {
				t.Fatalf("exporting: %v", err)
			}
			var cm corev1.ConfigMap
			if err := c.Get(context.Background(),
				types.NamespacedName{Namespace: tc.namespace, Name: exportedName}, &cm); err != nil {
				t.Fatal(err)
			}
			if got := len(cm.OwnerReferences) > 0; got != tc.wantOwner {
				t.Errorf("owner references present = %v, want %v -- a cross-namespace one is "+
					"invalid, and without one in the same namespace nothing reclaims it",
					got, tc.wantOwner)
			}
		})
	}
}

// TestMetadataKeysAreRefusedRatherThanDropped.
//
// These land on an object in a namespace this object may not otherwise touch. Refused rather than
// dropped, because a dropped key leaves a ConfigMap that looks exactly right while whatever was
// meant to select on it never does -- the same failure the watch label has.
func TestMetadataKeysAreRefusedRatherThanDropped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  map[string]string
		allowed []string
		ok      bool
	}{
		{"exactly permitted", map[string]string{"team": "team-a"}, []string{"team"}, true},
		{"a permitted prefix", map[string]string{"example.com/tier": "prod"}, []string{"example.com/*"}, true},
		{"nothing permitted", map[string]string{"team": "team-a"}, nil, false},
		{"outside the prefix", map[string]string{"other.com/x": "1"}, []string{"example.com/*"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()
			spec := exportSpec()
			spec.Labels = tc.labels

			opts := allowingFlux()
			opts.AllowedLabels = tc.allowed
			_, err := ExportRef(context.Background(), c, owner(), spec, opts, testDigest, testRef)

			if tc.ok {
				if err != nil {
					t.Fatalf("refused a permitted key: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("an unpermitted key was accepted")
			}
			if !IsTerminal(err) {
				t.Errorf("must be terminal; got %v", err)
			}
			for k := range tc.labels {
				if !strings.Contains(err.Error(), k) {
					t.Errorf("the message must name the offending key %q: %v", k, err)
				}
			}
			var cm corev1.ConfigMap
			if err := c.Get(context.Background(),
				types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &cm); err == nil {
				t.Error("the ConfigMap was written despite the refusal")
			}
		})
	}
}

// TestTheExportedNameCarriesTheKind -- both kinds export and they share a namespace, so a name
// without the kind would collide between an ImageBuild and an ImageComposition of the same name.
func TestTheExportedNameCarriesTheKind(t *testing.T) {
	scheme := exportScheme(t)
	for _, tc := range []struct {
		obj  client.Object
		want string
	}{
		{&ociv1alpha1.ImageBuild{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"}},
			"imagebuild-team-a-app"},
		{&ociv1alpha1.ImageComposition{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"}},
			"imagecomposition-team-a-app"},
	} {
		got, err := ExportName(tc.obj, scheme)
		if err != nil {
			t.Fatalf("naming: %v", err)
		}
		if got != tc.want {
			t.Errorf("name = %q, want %q", got, tc.want)
		}
	}
}

// TestAnUnnameableExportIsRefusedWithTheNameItBuilt.
//
// An object name may be 253 characters on its own, so the derived one can exceed the limit.
// Reporting the composed name matters: "too long" about a name nobody wrote is not actionable.
func TestAnUnnameableExportIsRefusedWithTheNameItBuilt(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()
	long := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 250), Namespace: "team-a"},
	}

	_, err := ExportRef(context.Background(), c, long, exportSpec(), allowingFlux(),
		testDigest, testRef)
	if err == nil {
		t.Fatal("a name over the limit was accepted")
	}
	if !IsTerminal(err) {
		t.Errorf("must be terminal -- only renaming the object fixes it; got %v", err)
	}
	if !strings.Contains(err.Error(), "imagebuild-team-a-aaa") {
		t.Errorf("the message must show the name that was built, not the one asked for: %v", err)
	}
}

// TestAnExportDeletedByHandComesBack.
//
// The ConfigMap is not the record -- status is. Somebody removing it by hand, or a namespace being
// recreated, must not leave the object believing an export exists that does not, because the next
// publish is the only thing that would notice.
func TestAnExportDeletedByHandComesBack(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	if _, err := ExportRef(context.Background(), c, owner(), exportSpec(), allowingFlux(),
		testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "flux-system", Name: exportedName}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &cm); err != nil {
		t.Fatalf("deleting by hand: %v", err)
	}

	if _, err := ExportRef(context.Background(), c, owner(), exportSpec(), allowingFlux(),
		testDigest, testRef); err != nil {
		t.Fatalf("re-exporting after a hand deletion: %v", err)
	}
	var again corev1.ConfigMap
	if err := c.Get(context.Background(), key, &again); err != nil {
		t.Fatalf("the export was not recreated: %v", err)
	}
	if again.Data["APP_REF"] != testRef {
		t.Errorf("recreated with the wrong content: %v", again.Data)
	}
}

// TestRevokingANamespaceLeavesWhatWasAlreadyWritten.
//
// An operator removing a namespace from the allow-list stops NEW exports. It must not also make the
// controller tear down what a consumer is already substituting from: that would turn a
// configuration change into an outage, at a moment when nobody is looking at this object.
//
// The refusal is terminal, so it says so on the object rather than failing quietly.
func TestRevokingANamespaceLeavesWhatWasAlreadyWritten(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(exportScheme(t)).Build()

	if _, err := ExportRef(context.Background(), c, owner(), exportSpec(), allowingFlux(),
		testDigest, testRef); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	// The operator revokes it.
	_, err := ExportRef(context.Background(), c, owner(), exportSpec(),
		ExportOptions{WatchLabels: fluxWatch}, testDigest, testRef)
	if err == nil {
		t.Fatal("exported into a namespace that is no longer permitted")
	}
	if !IsTerminal(err) {
		t.Errorf("must be terminal -- only the spec or the flag fixes it; got %v", err)
	}

	var still corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "flux-system", Name: exportedName}, &still); err != nil {
		t.Fatalf("the existing export was removed when the namespace was revoked: %v", err)
	}
	if still.Data["APP_REF"] != testRef {
		t.Errorf("the existing export was rewritten: %v", still.Data)
	}
}
