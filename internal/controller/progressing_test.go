package controller

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// While a composition is assembled, the previous pass's Ready=True must not stand: anything waiting
// on the object would proceed with the image being replaced (ADR 0061). An edited ConfigMap is the
// sharp case, because the generation does not move and kstatus has only the conditions to go by.

// statusWrite is what one status patch left on the object.
type statusWrite struct {
	ready              *metav1.Condition
	reconciling        *metav1.Condition
	observedGeneration int64
	lastHandled        string
}

// recordingReconciler is registryReconciler plus a log of every status write, in order.
func recordingReconciler(t *testing.T, objs ...client.Object) (*ImageCompositionReconciler, *[]statusWrite) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)

	var writes []statusWrite
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&ociv1alpha1.ImageComposition{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if err := c.SubResource(sub).Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				o := obj.(*ociv1alpha1.ImageComposition)
				writes = append(writes, statusWrite{
					ready:              meta.FindStatusCondition(o.Status.Conditions, ociv1alpha1.ReadyCondition),
					reconciling:        meta.FindStatusCondition(o.Status.Conditions, ociv1alpha1.ReconcilingCondition),
					observedGeneration: o.Status.ObservedGeneration,
					lastHandled:        o.Status.LastHandledReconcileAt,
				})
				return nil
			},
		}).Build()

	return &ImageCompositionReconciler{
		Client:   c,
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(64),
		Default:  recon.DefaultRegistry{Host: strings.TrimPrefix(srv.URL, "http://")},
		Fetcher:  oci.NewFetcher(),
	}, &writes
}

func TestAssemblyIsNotReadyForThePreviousImage(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "default"},
		Data:       map[string]string{"app.conf": "one"},
	}
	obj := composition("progress", configMapLayer("settings", "settings", false, "/config"))
	obj.Generation = 1
	obj.Annotations = map[string]string{ociv1alpha1.ReconcileRequestAnnotation: "first"}
	r, writes := recordingReconciler(t, cm, obj)
	key := types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name}
	pass := func(what string) {
		t.Helper()
		*writes = nil
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// The first write of an assembling pass says so, without claiming the pass is over; the last
	// is Ready for what was published.
	wantAssembling := func(what, stillPublished string, generation int64, handled string) {
		t.Helper()
		if len(*writes) < 2 {
			t.Fatalf("%s: %d status writes, want progress then outcome", what, len(*writes))
		}
		w := (*writes)[0]
		if w.ready == nil || w.ready.Status != metav1.ConditionUnknown || w.ready.Reason != ociv1alpha1.ReasonProgressing {
			t.Fatalf("%s: Ready during assembly = %+v, want Unknown/Progressing", what, w.ready)
		}
		if w.reconciling == nil || w.reconciling.Status != metav1.ConditionTrue {
			t.Errorf("%s: Reconciling during assembly = %+v, want True", what, w.reconciling)
		}
		if stillPublished != "" && !strings.Contains(w.ready.Message, stillPublished) {
			t.Errorf("%s: %q does not say %s is still published", what, w.ready.Message, stillPublished)
		}
		if w.observedGeneration != generation || w.lastHandled != handled {
			t.Errorf("%s: mid-pass write stamped the pass (observedGeneration %d, lastHandled %q); "+
				"want %d, %q", what, w.observedGeneration, w.lastHandled, generation, handled)
		}
		last := (*writes)[len(*writes)-1]
		if last.ready == nil || last.ready.Status != metav1.ConditionTrue || last.reconciling != nil {
			t.Errorf("%s: final Ready = %+v, Reconciling = %+v; want True and none", what, last.ready, last.reconciling)
		}
	}

	pass("first publish")
	wantAssembling("first publish", "", 0, "")

	pass("converged")
	for _, w := range *writes {
		if w.ready != nil && w.ready.Status != metav1.ConditionTrue {
			t.Errorf("a converged pass wrote Ready = %+v; the cheap path must not flicker", w.ready)
		}
	}

	var published ociv1alpha1.ImageComposition
	if err := r.Get(context.Background(), key, &published); err != nil {
		t.Fatal(err)
	}

	// An input changes, the spec does not: only the conditions can tell a waiter to hold on.
	cm.Data["app.conf"] = "two"
	if err := r.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	published.Annotations[ociv1alpha1.ReconcileRequestAnnotation] = "second"
	if err := r.Update(context.Background(), &published); err != nil {
		t.Fatal(err)
	}
	pass("edited ConfigMap")
	wantAssembling("edited ConfigMap", published.Status.Artifact.Ref, 1, "first")
}
