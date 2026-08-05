package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/serve"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// A composition may reference things it does not own: a Flux source, a Secret, a ConfigMap. None
// of those raise an event on this object when they are created, so treating their absence as
// terminal wedges the composition permanently — the fix arrives and nothing wakes up to notice.
//
// This is not hypothetical. A consumer applied four compositions and their GitRepositories in one
// commit; the one that lost the race went Stalled and sat there reporting "source not found"
// while the GitRepository it named was Ready in the same namespace. Only deleting it cleared it.

// pendingReconciler is servingReconciler plus the Flux source kinds, so a test can reconcile a
// sourceRef composition end to end and then create the source underneath it.
func pendingReconciler(t *testing.T, objs ...client.Object) *ImageCompositionReconciler {
	t.Helper()

	blobs, err := store.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("creating blob store: %v", err)
	}
	srv, err := serve.New("oci.test", ":0", blobs, false)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	host := strings.TrimPrefix(httpSrv.URL, "http://")
	srv.Addr = host[strings.LastIndex(host, ":"):]

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
		Server:   srv,
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
	// A fixed short retry, not exponential backoff: this is a normal step in converging a
	// commit, and backing off would make a one-second race take minutes to clear.
	if res.RequeueAfter != pendingRetryInterval {
		t.Fatalf("RequeueAfter %v, want %v", res.RequeueAfter, pendingRetryInterval)
	}

	got := reload(t, r, obj)
	// The whole point. Stalled here would mean waiting for a generation change that creating
	// the GitRepository does not produce.
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

// TestCompositionRecoversWhenTheSourceAppears — the half that actually proves "never stuck".
// Conditions could be perfect and the object could still never build again; this reconciles a
// composition whose source is absent, creates the source, and requires the very next reconcile
// to publish. No annotation, no delete, no human.
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

	// The GitRepository lands, exactly as it would a moment after the composition in a
	// same-commit apply.
	url, digest := tarball(t, map[string]string{"plugin/a.jar": "aaa"})
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
