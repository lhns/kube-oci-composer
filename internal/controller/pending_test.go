package controller

import (
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/google/go-containerregistry/pkg/registry"
	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// A missing dependency the composition does not own (Flux source, Secret, ConfigMap) raises no
// event on it when created, so treating its absence as terminal would wedge the composition.

// pendingReconciler is registryReconciler plus the Flux source kinds, so a test can create the
// source after the composition has already reconciled.
func pendingReconciler(t *testing.T, objs ...client.Object) *ImageCompositionReconciler {
	t.Helper()

	httpSrv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(httpSrv.Close)
	host := strings.TrimPrefix(httpSrv.URL, "http://")

	scheme := fluxScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(o)
		if _, ok := o.(*ociv1alpha1.ImageComposition); ok {
			builder = builder.WithStatusSubresource(o)
		}
	}

	return &ImageCompositionReconciler{
		Client:   builder.Build(),
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(64),
		Default:  recon.DefaultRegistry{Host: host},
		Fetcher:  oci.NewFetcher(),
	}
}

// TestMissingSourceRequeuesWithoutStalling — the condition and requeue half of the contract.
func TestMissingSourceRequeuesWithoutStalling(t *testing.T) {
	obj := composition("waiting", ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "not-yet"},
		To:        "/content",
	})
	r := pendingReconciler(t, obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("waiting on a dependency must not be returned to the queue as an error: %v", err)
	}
	// A fixed short retry, not exponential backoff, so a one-second race clears in seconds.
	if res.RequeueAfter != pendingRetryInterval {
		t.Fatalf("RequeueAfter %v, want %v", res.RequeueAfter, pendingRetryInterval)
	}

	got := reload(t, r, obj)
	// Stalled would wait for a generation change that creating the GitRepository never produces.
	if meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.StalledCondition) != nil {
		t.Fatal("a dependency that does not exist yet must never set Stalled")
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %+v", got.Status.Conditions)
	}
	if ready.Reason != ociv1alpha1.ReasonDependencyNotReady {
		t.Fatalf("Ready reason %q, want %q", ready.Reason, ociv1alpha1.ReasonDependencyNotReady)
	}
}

// TestCompositionRecoversWhenTheSourceAppears — the half that proves "never stuck": once the
// source is created, the very next reconcile must publish, with no human intervention.
func TestCompositionRecoversWhenTheSourceAppears(t *testing.T) {
	obj := composition("recovers", ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "arrives-later"},
		To:        "/content",
	})
	r := pendingReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if meta.IsStatusConditionTrue(reload(t, r, obj).Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatal("must not be Ready while its source is missing")
	}

	url, digest := contentServer(t, map[string]string{"plugin/a.jar": "aaa"})
	repo := gitRepository("arrives-later", "default", url, digest, "main@sha1:abcd")
	if err := r.Create(t.Context(), repo); err != nil {
		t.Fatalf("creating the source: %v", err)
	}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after the source appeared: %v", err)
	}
	got := reload(t, r, obj)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatalf("expected Ready=True once the source exists, got %+v", got.Status.Conditions)
	}
	if got.Status.Artifact == nil || got.Status.Artifact.Ref == "" {
		t.Fatal("expected a published artifact after recovery")
	}
}
