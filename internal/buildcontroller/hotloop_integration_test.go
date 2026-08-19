//go:build integration

package buildcontroller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// countingReconciler wraps the real one and counts how often the queue delivers work, which is the
// quantity the hot loop was pathological in.
type countingReconciler struct {
	inner *ImageBuildReconciler
	calls atomic.Int64
}

func (c *countingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c.calls.Add(1)
	return c.inner.Reconcile(ctx, req)
}

// TestAFailingBuildDoesNotSpinTheQueue is the test the unit suite structurally cannot write.
//
// The defect: the failure path deleted the failed Job so the next attempt would not adopt it. But
// deleting an OWNED Job wakes this controller through its own Owns() watch, and that reconcile
// finds no Job and starts another — so RequeueAfter's backoff never applied and a failing build
// retried every few seconds indefinitely, destroying each pod's logs on the way. Under a fake
// client there are no watches, so the loop cannot happen and every unit test passed.
//
// Running a real manager makes the watch real. The assertion is a rate: a build that has failed
// once must not be reconciled tens of times in the seconds that follow.
func TestAFailingBuildDoesNotSpinTheQueue(t *testing.T) {
	ctx, k8s := integrationCtx(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "hotloop"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	srv := contextServer(t, contextTarball(t, "src-abc123/", pinnedFrom))
	src := fluxSource("hotloop", "src", srv.URL, "sha256:ctx", "main@sha1:abcd")
	if err := k8s.Create(ctx, src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	// The stand-in CRD declares no status subresource, so status is written by a plain update —
	// deliberately, because it lets a test publish an artifact without running source-controller.
	if err := k8s.Update(ctx, src); err != nil {
		t.Fatalf("writing source status: %v", err)
	}

	obj := sampleBuild()
	obj.Namespace = "hotloop"
	obj.Spec.Context.Name = "src"
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("creating ImageBuild: %v", err)
	}

	mgr, err := manager.New(cfg, manager.Options{
		Scheme:  testScheme(t),
		Metrics: server.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	counted := &countingReconciler{inner: &ImageBuildReconciler{
		Client: mgr.GetClient(), JobConfig: sampleConfig(), HTTPClient: srv.Client(),
	}}
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&ociv1alpha1.ImageBuild{}).
		Owns(&batchv1.Job{}).
		Complete(reconcile.Func(counted.Reconcile)); err != nil {
		t.Fatalf("wiring controller: %v", err)
	}

	go func() { _ = mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	// Envtest runs no job controller and no kubelet, so a Job never finishes on its own. This
	// stands in for both, and it has to run CONTINUOUSLY rather than once: the loop only appears
	// when every attempt fails, which is what a broken Dockerfile does in production. Failing just
	// the first Job lets the recreated one sit pending forever, and the test then passes with the
	// bug present — which it did, before this existed.
	go failEveryJob(ctx, k8s, "hotloop")

	waitForJob(t, ctx, k8s, "hotloop")

	// Let the loop run. The first backoff is 30s, so a correct controller reconciles a handful of
	// times in this window — once for the failure, plus watch events for its own status writes.
	// The hot loop managed a reconcile every few seconds and climbed without bound.
	// Let it settle first: the initial create-and-fail is legitimately a few reconciles.
	time.Sleep(3 * time.Second)
	before := counted.calls.Load()
	time.Sleep(15 * time.Second)
	during := counted.calls.Load() - before

	const tolerated = 12
	if during > tolerated {
		t.Errorf("a build that failed once was reconciled %d times in 15s (tolerating %d): the "+
			"failure path is waking the controller through its own Job watch, so the backoff "+
			"never applies", during, tolerated)
	}

	// And the Job must still be there: it is kept for the whole backoff so its pod's logs survive,
	// which is the other half of the same fix.
	var jobs batchv1.JobList
	if err := k8s.List(ctx, &jobs, client.InNamespace("hotloop")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Errorf("found %d Jobs, want the failed one kept for its backoff", len(jobs.Items))
	}
}

// failEveryJob keeps every Job in the namespace failed, standing in for the kubelet and the job
// controller. It is what makes a retry loop observable: with the failure path deleting the Job on
// sight, each recreation fails again immediately and the controller never reaches its backoff.
func failEveryJob(ctx context.Context, k8s client.Client, namespace string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}

		var jobs batchv1.JobList
		if err := k8s.List(ctx, &jobs, client.InNamespace(namespace)); err != nil {
			continue
		}
		for i := range jobs.Items {
			job := &jobs.Items[i]
			if job.Status.Failed > 0 || !job.DeletionTimestamp.IsZero() {
				continue
			}
			// Best effort: a Job being deleted underneath this is the normal case, not an error.
			_ = markJobFailed(ctx, k8s, job)
		}
	}
}

// markJobFailed drives a Job to Failed the way the job controller does. The real API server
// enforces the transition and the fake client does not: startTime is required on a finished Job,
// and Failed=True is rejected without FailureTarget=True first.
func markJobFailed(ctx context.Context, k8s client.Client, job *batchv1.Job) error {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobConditionType("FailureTarget"), Status: corev1.ConditionTrue,
		Reason: "BackoffLimitExceeded", Message: "the RUN exited 1", LastTransitionTime: now,
	}}
	if err := k8s.Status().Update(ctx, job); err != nil {
		return err
	}

	job.Status.Failed = 1
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "BackoffLimitExceeded", Message: "the RUN exited 1", LastTransitionTime: now,
	})
	return k8s.Status().Update(ctx, job)
}

func waitForJob(t *testing.T, ctx context.Context, k8s client.Client, namespace string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var jobs batchv1.JobList
		if err := k8s.List(ctx, &jobs, client.InNamespace(namespace)); err == nil && len(jobs.Items) == 1 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("no Job was created; the controller never got as far as starting a build")
}

// fluxSource is a GitRepository with a published artifact, as source-controller would leave one.
func fluxSource(namespace, name, url, digest, revision string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersion{Group: "source.toolkit.fluxcd.io", Version: "v1"}.
		WithKind("GitRepository"))
	u.SetNamespace(namespace)
	u.SetName(name)
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"url": url, "digest": digest, "revision": revision,
	}, "status", "artifact")
	return u
}
