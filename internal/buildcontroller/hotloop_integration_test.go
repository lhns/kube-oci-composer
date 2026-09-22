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

// countingReconciler wraps the real one and counts how often the queue delivers work.
type countingReconciler struct {
	inner *ImageBuildReconciler
	calls atomic.Int64
}

func (c *countingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c.calls.Add(1)
	return c.inner.Reconcile(ctx, req)
}

// TestAFailingBuildDoesNotSpinTheQueue: deleting a failed owned Job fires the Owns() watch, which
// starts a new Job at once, so the backoff never applies. Only a real watch can show this; the
// assertion is a reconcile rate.
func TestAFailingBuildDoesNotSpinTheQueue(t *testing.T) {
	ctx, k8s := integrationCtx(t)

	srv := buildableNamespace(t, ctx, k8s, "hotloop")

	obj := sampleBuild()
	obj.Namespace = "hotloop"
	obj.Spec.Context.SourceRef.Name = "src"
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

	// envtest has no job controller or kubelet. Fail every Job continuously, as a broken Dockerfile
	// would; failing only the first lets the test pass with the bug present.
	go failEveryJob(ctx, k8s, "hotloop")

	waitForJob(t, ctx, k8s, "hotloop")

	// The first backoff is 30s, so a correct controller reconciles only a handful of times here.
	// Let the initial create-and-fail settle first.
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

	// The failed Job is kept for the whole backoff so its pod's logs survive.
	var jobs batchv1.JobList
	if err := k8s.List(ctx, &jobs, client.InNamespace("hotloop")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Errorf("found %d Jobs, want the failed one kept for its backoff", len(jobs.Items))
	}
}

// failEveryJob keeps every Job in the namespace failed, standing in for the kubelet and the job
// controller.
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
// requires startTime, and FailureTarget=True before Failed=True.
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
