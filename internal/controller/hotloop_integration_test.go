//go:build integration

package controller

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// countingReconciler wraps the real one and counts how often the queue delivers work.
type countingReconciler struct {
	inner *ImageCompositionReconciler
	calls atomic.Int64
}

func (c *countingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c.calls.Add(1)
	return c.inner.Reconcile(ctx, req)
}

// TestAFailingCompositionDoesNotWakeItselfThroughStatus: a pass that fails after the Progressing
// write flips conditions, and an unfiltered For() turns each flip into another reconcile. Only a
// real watch shows this; the assertion is a reconcile rate.
func TestAFailingCompositionDoesNotWakeItselfThroughStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The server serves bytes that do not match the declared digest: a terminal error raised after
	// the progress write.
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not the declared bytes"))
	}))
	defer content.Close()

	reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer reg.Close()

	obj := composition("hotloop", urlLayer("core", content.URL+"/a.tar.gz", validDigest, "/core"))
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("creating ImageComposition: %v", err)
	}

	mgr, err := manager.New(cfg, manager.Options{
		Scheme:  integrationScheme(),
		Metrics: server.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	r := &ImageCompositionReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: &record.FakeRecorder{},
		Default:  recon.DefaultRegistry{Host: strings.TrimPrefix(reg.URL, "http://")},
		Fetcher:  oci.NewFetcher(),
	}
	counted := &countingReconciler{inner: r}
	if err := r.setup(mgr, reconcile.Func(counted.Reconcile)); err != nil {
		t.Fatalf("wiring controller: %v", err)
	}

	go func() { _ = mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	time.Sleep(10 * time.Second)
	if got := counted.calls.Load(); got > 6 {
		t.Errorf("a composition that failed terminally was reconciled %d times in 10s: its own "+
			"status writes are waking the controller", got)
	}

	var got ociv1alpha1.ImageComposition
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), &got); err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.StalledCondition) {
		t.Errorf("want Stalled=True, got %+v", got.Status.Conditions)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) ||
		meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReconcilingCondition) != nil {
		t.Errorf("want Ready=False and no Reconciling, got %+v", got.Status.Conditions)
	}
}
