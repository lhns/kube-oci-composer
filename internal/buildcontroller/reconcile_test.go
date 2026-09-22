package buildcontroller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// The reconcile loop, against a fake client. CRD schema rules are covered by the envtest suite in
// internal/controller, and pure rendering by job_test.go.

const pinnedFrom = "FROM busybox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := ociv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// contextTarball is a build context holding one Dockerfile. prefix wraps it in a directory, as a
// release tarball does (see stripComponents); source-controller publishes files at the root, so
// sourceRef fixtures pass "" (ADR 0045).
func contextTarball(t *testing.T, prefix, dockerfile string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: prefix + "Dockerfile", Mode: 0o644,
		Size: int64(len(dockerfile)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("writing header: %v", err)
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		t.Fatalf("writing body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

func contextServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gitRepository is a Flux source with a published artifact.
func gitRepository(namespace, name, url, digest string) *unstructured.Unstructured {
	return gitRepositoryAt(namespace, name, url, digest, "main@sha1:abcd")
}

// gitRepositoryAt is the same with the revision stated, for the checks that read it.
func gitRepositoryAt(namespace, name, url, digest, revision string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersion{Group: "source.toolkit.fluxcd.io", Version: "v1"}.
		WithKind("GitRepository"))
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetGeneration(1)
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"url":      url,
		"digest":   digest,
		"revision": revision,
	}, "status", "artifact")
	_ = unstructured.SetNestedField(u.Object, int64(1), "status", "observedGeneration")
	return u
}

// harness wires a reconciler over a fake client, with a server standing in for the context.
func harness(t *testing.T, dockerfile string, objs ...client.Object) *ImageBuildReconciler {
	t.Helper()
	srv := contextServer(t, contextTarball(t, "", dockerfile))

	// A real registry for the controller to tag in; objects naming the placeholder repository are
	// pointed at it.
	host := startRegistry(t)
	for _, o := range objs {
		b, ok := o.(*ociv1alpha1.ImageBuild)
		if !ok || b.Spec.Push == nil || b.Spec.Push.Repository != "ghcr.io/me/app" {
			continue
		}
		b.Spec.Push.Repository = host + "/me/app"
	}

	all := append([]client.Object{gitRepository("team-a", "src", srv.URL, "sha256:ctx")}, objs...)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(all...).
		WithStatusSubresource(&ociv1alpha1.ImageBuild{}).
		Build()

	cfg := sampleConfig()
	cfg.InsecureRegistries = []string{host}
	return &ImageBuildReconciler{Client: c, JobConfig: cfg, HTTPClient: srv.Client()}
}

func buildOf(t *testing.T, mutate func(*ociv1alpha1.ImageBuild)) *ociv1alpha1.ImageBuild {
	t.Helper()
	obj := sampleBuild()
	obj.Generation = 1
	if mutate != nil {
		mutate(obj)
	}
	return obj
}

func reconcileOnce(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name},
	})
}

func reload(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild) *ociv1alpha1.ImageBuild {
	t.Helper()
	var out ociv1alpha1.ImageBuild
	key := types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name}
	if err := r.Get(context.Background(), key, &out); err != nil {
		t.Fatalf("reloading: %v", err)
	}
	return &out
}

func conditionOf(obj *ociv1alpha1.ImageBuild, condType string) *metav1.Condition {
	for i := range obj.Status.Conditions {
		if obj.Status.Conditions[i].Type == condType {
			return &obj.Status.Conditions[i]
		}
	}
	return nil
}

func jobsIn(t *testing.T, r *ImageBuildReconciler, ns string) []batchv1.Job {
	t.Helper()
	var list batchv1.JobList
	if err := r.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	return list.Items
}

// TestReconcileCreatesAJob — the first reconcile of a new object.
func TestReconcileCreatesAJob(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job, got %d", len(jobs))
	}
	if got := reload(t, r, obj).Status.BuildRef; got == nil || got.Name != jobs[0].Name {
		t.Errorf("status.buildRef does not name the Job it created: %+v", got)
	}
}

// TestReconcileIsIdempotentWhileBuilding: a second pass observes the running Job rather than
// starting another.
func TestReconcileIsIdempotentWhileBuilding(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	for i := range 3 {
		if _, err := reconcileOnce(t, r, obj); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 1 {
		t.Fatalf("three reconciles produced %d Jobs, want 1", len(jobs))
	}
}

// TestReconcileShortCircuitsOnUnchangedInputs — the whole point of hashing inputs.
func TestReconcileShortCircuitsOnUnchangedInputs(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after success: %v", err)
	}
	if reload(t, r, obj).Status.Artifact == nil {
		t.Fatal("no artifact recorded after a successful build")
	}

	// Remove the Job, then reconcile again: nothing changed, so nothing should be rebuilt.
	for _, j := range jobsIn(t, r, obj.Namespace) {
		if err := r.Delete(context.Background(), &j); err != nil {
			t.Fatalf("deleting job: %v", err)
		}
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile on unchanged inputs: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Errorf("unchanged inputs started %d Jobs; the short-circuit did not fire", len(jobs))
	}
}

// TestSuccessRecordsTheArtifact: the digest comes back through the termination message, and the
// history record carries the input hash.
func TestSuccessRecordsTheArtifact(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	digest := succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after success: %v", err)
	}

	got := reload(t, r, obj)
	if got.Status.Artifact == nil || got.Status.Artifact.Digest != digest {
		t.Fatalf("artifact = %+v, want digest %s", got.Status.Artifact, digest)
	}
	if want := obj.Spec.Push.Repository + "@" + digest; got.Status.Artifact.Ref != want {
		t.Errorf("ref = %q, want %q", got.Status.Artifact.Ref, want)
	}
	if len(got.Status.History) != 1 || got.Status.History[0].InputHash == "" {
		t.Errorf("history = %+v, want one record carrying an input hash", got.Status.History)
	}
	if got.Status.BuildRef != nil {
		t.Error("buildRef survived a successful build")
	}
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True", c)
	}
}

// TestFailureDoesNotStall: a failing RUN is fixed in another object, so stalling would never wake
// (ADR 0025).
func TestFailureDoesNotStall(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	failJob(t, r, obj, "the RUN exited 1")

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("a build failure surfaced as a reconcile error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("a failed build did not schedule a retry")
	}

	got := reload(t, r, obj)
	if c := conditionOf(got, ociv1alpha1.StalledCondition); c != nil {
		t.Errorf("a build failure set Stalled: %+v", c)
	}
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != ociv1alpha1.ReasonBuildFailed {
		t.Errorf("Ready = %+v, want False/BuildFailed", c)
	}
	if got.Status.Failures != 1 {
		t.Errorf("failures = %d, want 1", got.Status.Failures)
	}
}

// TestFailedBuildDoesNotRetryInAHotLoop pins the state half: the failed Job survives its backoff
// and the failure is counted once. The rate half needs real watches; see
// TestAFailingBuildDoesNotSpinTheQueue.
func TestFailedBuildDoesNotRetryInAHotLoop(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	failJob(t, r, obj, "the RUN exited 1")

	// Several reconciles inside the backoff window, as the Job watch would produce.
	for i := range 3 {
		if _, err := reconcileOnce(t, r, obj); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if n := len(jobsIn(t, r, obj.Namespace)); n != 1 {
			t.Fatalf("after %d re-observations there are %d Jobs; the failed one must be kept "+
				"during the backoff so its pod's logs survive", i+1, n)
		}
	}
	if f := reload(t, r, obj).Status.Failures; f != 1 {
		t.Errorf("failures = %d after re-observing one failure, want 1", f)
	}

	// Once the backoff has elapsed, the Job is cleared so the next pass builds afresh.
	got := reload(t, r, obj)
	got.Status.LastAttempt.FinishedAt = ptr.To(metav1.NewTime(time.Now().Add(-time.Hour)))
	if err := r.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("backdating the attempt: %v", err)
	}
	if _, err := reconcileOnce(t, r, got); err != nil {
		t.Fatalf("reconcile after the backoff: %v", err)
	}
	if n := len(jobsIn(t, r, obj.Namespace)); n != 0 {
		t.Errorf("the failed Job survived its backoff (%d Jobs); the retry would adopt it forever", n)
	}
}

// TestSuspendSaysSo — a suspended object must not look stalled or silently idle.
func TestSuspendSaysSo(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) { o.Spec.Suspend = true })
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(jobsIn(t, r, obj.Namespace)) != 0 {
		t.Error("a suspended object started a build")
	}
	c := conditionOf(reload(t, r, obj), ociv1alpha1.ReadyCondition)
	if c == nil || c.Reason != ociv1alpha1.ReasonSuspended {
		t.Errorf("Ready = %+v, want reason Suspended", c)
	}
}

// TestMissingSourceIsPendingNotStalled: creating the source is the fix.
func TestMissingSourceIsPendingNotStalled(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) { o.Spec.Context.SourceRef.Name = "absent" })
	r := harness(t, pinnedFrom, obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("a missing source surfaced as an error: %v", err)
	}
	if res.RequeueAfter != recon.PendingRetryInterval {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, recon.PendingRetryInterval)
	}
	got := reload(t, r, obj)
	if c := conditionOf(got, ociv1alpha1.StalledCondition); c != nil {
		t.Errorf("a missing source set Stalled: %+v", c)
	}
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil ||
		c.Reason != ociv1alpha1.ReasonDependencyNotReady {
		t.Errorf("Ready = %+v, want DependencyNotReady", c)
	}
}

// TestUnpinnedFromIsRefusedBeforeAJobExists (ADR 0025).
func TestUnpinnedFromIsRefusedBeforeAJobExists(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, "FROM golang:1.26\n", obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile returned an error rather than recording one: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Fatalf("an unpinned FROM still started %d Jobs", len(jobs))
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.ReadyCondition); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False", c)
	}
}

// TestMissingPushIsTerminal: only a spec edit fixes it, so it legitimately stalls.
func TestMissingPushIsTerminal(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) { o.Spec.Push = nil })
	r := harness(t, pinnedFrom, obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("a stalled object scheduled a retry (%v); the spec change is the wake-up", res.RequeueAfter)
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition); c == nil {
		t.Error("a missing spec.push did not stall")
	}
}

// succeedJob marks the object's Job succeeded and plants a pod reporting the digest the way the
// build container's termination message does.
func succeedJob(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild) string {
	t.Helper()
	// Stand in for the Job's push by digest, so the controller has something real to tag.
	_, digest := pushByDigest(t, obj.Spec.Push.Repository)
	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job to succeed, got %d", len(jobs))
	}
	job := jobs[0]
	job.Status.Succeeded = 1
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("updating job status: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-abcde",
			Namespace: obj.Namespace,
			Labels:    map[string]string{"job-name": job.Name},
		},
	}
	if err := r.Create(context.Background(), pod); err != nil {
		t.Fatalf("creating pod: %v", err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "build",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: "{\"containerimage.digest\":\"" + digest + "\"}",
		}},
	}}
	if err := r.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("updating pod status: %v", err)
	}
	return digest
}

// failJob marks the object's Job failed.
func failJob(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild, msg string) {
	t.Helper()
	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job to fail, got %d", len(jobs))
	}
	job := jobs[0]
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: msg,
	}}
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("updating job status: %v", err)
	}
}

// TestReconcileRequestIsEchoed: `flux reconcile` waits for status.lastHandledReconcileAt to match,
// including after a failure.
func TestReconcileRequestIsEchoed(t *testing.T) {
	const requested = "2026-01-01T00:00:00Z"
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Annotations = map[string]string{ociv1alpha1.ReconcileRequestAnnotation: requested}
	})
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := reload(t, r, obj).Status.LastHandledReconcileAt; got != requested {
		t.Errorf("lastHandledReconcileAt = %q, want %q", got, requested)
	}

	// And again after the build has failed.
	failJob(t, r, obj, "the RUN exited 1")
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after failure: %v", err)
	}
	if got := reload(t, r, obj).Status.LastHandledReconcileAt; got != requested {
		t.Errorf("after a failed build lastHandledReconcileAt = %q, want %q", got, requested)
	}
}

// TestContextMustBeInTheSameNamespace: the builder reads Flux sources cluster-wide, so another
// namespace's source would leak its content.
func TestContextMustBeInTheSameNamespace(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Context.SourceRef.Namespace = "other-team"
	})
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Fatalf("a build started from another namespace's context: %v", jobs)
	}
	c := conditionOf(reload(t, r, obj), ociv1alpha1.ReadyCondition)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", c)
	}
	if !strings.Contains(c.Message, "same namespace") {
		t.Errorf("the refusal does not say why: %q", c.Message)
	}
}

// TestContextRevisionIsHonoured: a revision mismatch waits rather than building the wrong commit.
func TestContextRevisionIsHonoured(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Context.SourceRef.Revision = "v0.6.8"
	})
	srv := contextServer(t, contextTarball(t, "", pinnedFrom))
	src := gitRepositoryAt("team-a", "src", srv.URL, "sha256:ctx", "v0.6.5@sha1:aaaaaaa")

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(src, obj).WithStatusSubresource(&ociv1alpha1.ImageBuild{}).Build()
	r := &ImageBuildReconciler{Client: c, JobConfig: sampleConfig(), HTTPClient: srv.Client()}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Fatalf("built from the wrong revision: %v", jobs)
	}
	// Pending, not stalled: the source catching up raises no generation change here.
	cond := conditionOf(reload(t, r, obj), ociv1alpha1.ReadyCondition)
	if cond == nil || cond.Reason != ociv1alpha1.ReasonDependencyNotReady {
		t.Fatalf("Ready = %+v, want reason %s", cond, ociv1alpha1.ReasonDependencyNotReady)
	}
}

// TestContextRevisionMatchingBuilds: a matching revision builds.
func TestContextRevisionMatchingBuilds(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Context.SourceRef.Revision = "v0.6.8"
	})
	srv := contextServer(t, contextTarball(t, "", pinnedFrom))
	src := gitRepositoryAt("team-a", "src", srv.URL, "sha256:ctx", "v0.6.8@sha1:b739efb5")

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(src, obj).WithStatusSubresource(&ociv1alpha1.ImageBuild{}).Build()
	r := &ImageBuildReconciler{Client: c, JobConfig: sampleConfig(), HTTPClient: srv.Client()}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 1 {
		t.Fatalf("a matching revision did not build: %d jobs", len(jobs))
	}
}

// TestAnInlineDockerfileNeedsNoContext: a Dockerfile that reads no files needs no Flux source.
func TestAnInlineDockerfileNeedsNoContext(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Context = nil
		o.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{
			Inline: "FROM scratch@sha256:" + strings.Repeat("a", 64) + "\n",
		}
	})
	// No Flux source for this object: reaching for one would fail.
	r := harness(t, "", obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 1 {
		t.Fatalf("want one Job from a context-less build, got %d", len(jobs))
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition); c != nil {
		t.Errorf("a context-less inline build stalled: %+v", c)
	}
}

// TestAnUnpinnedInlineFromStalls: the Dockerfile is the spec, so the fix raises a generation change.
// Contrast TestAnUnpinnedContextFromDoesNotStall.
func TestAnUnpinnedInlineFromStalls(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Context = nil
		o.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: "FROM golang:1.26\n"}
	})
	r := harness(t, "", obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("reconcile returned an error rather than recording one: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Fatalf("an unpinned FROM still started %d Jobs", len(jobs))
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition); c == nil {
		t.Error("an unpinned FROM in an inline Dockerfile did not stall, but only a spec edit fixes it")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("a stalled object scheduled a retry (%v); the spec change is the wake-up", res.RequeueAfter)
	}
}

// TestAnUnpinnedContextFromDoesNotStall: the fix is a push to the source, which raises no
// generation change here.
func TestAnUnpinnedContextFromDoesNotStall(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, "FROM golang:1.26\n", obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition); c != nil {
		t.Errorf("a Dockerfile in the context stalled: %+v — but pushing a fix there raises no "+
			"generation change to wake it", c)
	}
	if res.RequeueAfter == 0 {
		t.Error("no retry scheduled, so a fix pushed to the source would never be noticed")
	}
}
