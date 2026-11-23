package buildcontroller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// finishJobWithout marks the object's Job succeeded `ago`, reporting a digest the registry does not
// hold: what is left when a collector takes the unnamed output.
func finishJobWithout(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild, ago time.Duration) (batchv1.Job, string) {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := img.Digest()
	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job, got %d", len(jobs))
	}
	job := jobs[0]
	job.UID = "the-job-whose-output-was-lost"
	if err := r.Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	job.Status.CompletionTime = ptr.To(metav1.NewTime(time.Now().Add(-ago)))
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: job.Name + "-00000", Namespace: obj.Namespace,
		Labels: map[string]string{"job-name": job.Name},
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(&job, batchv1.SchemeGroupVersion.WithKind("Job")),
		},
	}}
	if err := r.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "build",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: "{\"containerimage.digest\":\"" + h.String() + "\"}",
		}},
	}}
	if err := r.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return job, h.String()
}

func TestWithinTheGraceAGoneOutputIsStillPending(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	finishJobWithout(t, r, obj, time.Second)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	got := reload(t, r, obj)
	c := conditionOf(got, ociv1alpha1.ReadyCondition)
	if c == nil || c.Reason != ociv1alpha1.ReasonDependencyNotReady || got.Status.Failures != 0 {
		t.Fatalf("want Pending and no failure counted inside the grace")
	}
	if n := len(jobsIn(t, r, obj.Namespace)); n != 1 {
		t.Fatalf("jobs=%d", n)
	}
}

func TestALostOutputIsCountedOnceThenRebuilt(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	finishJobWithout(t, r, obj, 2*time.Minute)

	for i := range 3 {
		_, err := reconcileOnce(t, r, obj)
		if err != nil {
			t.Fatal(err)
		}
		got := reload(t, r, obj)
		c := conditionOf(got, ociv1alpha1.ReadyCondition)
		if c.Reason != ociv1alpha1.ReasonBuildFailed || got.Status.Failures != 1 {
			t.Fatalf("pass %d: want BuildFailed counted once", i)
		}
		if !strings.Contains(c.Message, outputLostPrefix) {
			t.Fatalf("message does not say what happened: %s", c.Message)
		}
		if n := len(jobsIn(t, r, obj.Namespace)); n != 1 {
			t.Fatalf("pass %d: the Job went inside the backoff (%d)", i, n)
		}
	}

	// Past the backoff: the Job is cleared, and the next pass builds afresh.
	got := reload(t, r, obj)
	got.Status.LastAttempt.FinishedAt = ptr.To(metav1.NewTime(time.Now().Add(-time.Hour)))
	if err := r.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, r, got); err != nil {
		t.Fatal(err)
	}
	if n := len(jobsIn(t, r, obj.Namespace)); n != 0 {
		t.Fatalf("the Job whose output was lost survived its backoff (%d)", n)
	}
	if _, err := reconcileOnce(t, r, got); err != nil {
		t.Fatal(err)
	}
	if n := len(jobsIn(t, r, obj.Namespace)); n != 1 {
		t.Fatalf("no fresh Job after the loss (%d)", n)
	}
	// The stale pod of the first Job is still there (same job-name label); a fresh success must
	// not read its digest.
	fresh := jobsIn(t, r, obj.Namespace)[0]
	fresh.UID = "the-fresh-job"
	if err := r.Update(context.Background(), &fresh); err != nil {
		t.Fatal(err)
	}
	digest := succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, got); err != nil {
		t.Fatal(err)
	}
	final := reload(t, r, obj)
	if final.Status.Artifact == nil || final.Status.Artifact.Digest != digest || final.Status.Failures != 0 {
		t.Fatalf("after the rebuild: artifact=%+v failures=%d want %s", final.Status.Artifact, final.Status.Failures, digest)
	}
}

// A manifest that shows up late is named, even after the loss was counted.
func TestALateManifestIsNamedNotRebuilt(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	job, _ := finishJobWithout(t, r, obj, 2*time.Minute)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	if f := reload(t, r, obj).Status.Failures; f != 1 {
		t.Fatalf("failures=%d", f)
	}
	// Now the content appears: replace the pod's digest with one that exists.
	_, digest := pushByDigest(t, obj.Spec.Push.Repository)
	var pod corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: obj.Namespace, Name: job.Name + "-00000"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].State.Terminated.Message = "{\"containerimage.digest\":\"" + digest + "\"}"
	if err := r.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	got := reload(t, r, obj)
	if got.Status.Artifact == nil || got.Status.Failures != 0 || len(jobsIn(t, r, obj.Namespace)) != 1 {
		t.Fatalf("artifact=%+v failures=%d", got.Status.Artifact, got.Status.Failures)
	}
	ref, _ := name.NewTag(obj.Spec.Push.Repository+":v1", name.Insecure)
	if _, err := remote.Head(ref); err != nil {
		t.Fatalf("not named: %v", err)
	}
}

// A PUBLISHED image lost while its Job still exists (ADR 0051 inside the Job's TTL).
func TestAPublishedImageLostInsideTheJobTTLIsRebuiltAfterTheBackoff(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	digest := succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	if a := reload(t, r, obj).Status.Artifact; a == nil || a.Digest != digest {
		t.Fatalf("not published: %+v", a)
	}
	// The registry loses everything.
	repo := obj.Spec.Push.Repository
	for _, ref := range []string{repo + ":v1", repo + ":digest-" + digest[len("sha256:"):], repo + "@" + digest} {
		p, err := name.ParseReference(ref, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Delete(p); err != nil {
			t.Fatalf("deleting %s: %v", ref, err)
		}
	}
	job := jobsIn(t, r, obj.Namespace)[0]
	job.Status.CompletionTime = ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	got := reload(t, r, obj)
	c := conditionOf(got, ociv1alpha1.ReadyCondition)
	if c.Reason != ociv1alpha1.ReasonBuildFailed || got.Status.Failures != 1 ||
		!strings.Contains(c.Message, outputLostPrefix) {
		t.Fatalf("want the loss counted: reason=%s failures=%d", c.Reason, got.Status.Failures)
	}
	if n := len(jobsIn(t, r, obj.Namespace)); n != 1 {
		t.Fatalf("the Job went inside the backoff (%d)", n)
	}
	got.Status.LastAttempt.FinishedAt = ptr.To(metav1.NewTime(time.Now().Add(-time.Hour)))
	if err := r.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	if n := len(jobsIn(t, r, obj.Namespace)); n != 0 {
		t.Fatalf("the Job outlived its backoff (%d)", n)
	}
}

// Nothing but the registry's own "gone" may count a loss or delete a Job.
func TestAnAnswerThatIsNotGoneNeverCountsOrDeletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int // 0: the server is closed
	}{
		{"a 404 without the registry's error code", http.StatusNotFound},
		{"a 503", http.StatusServiceUnavailable},
		{"a closed server", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			if tc.code == 0 {
				srv = registryAnswering(t, http.StatusOK)
				srv.Close()
			} else {
				srv = registryAnswering(t, tc.code)
			}
			host := strings.TrimPrefix(srv.URL, "http://")
			obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) { b.Spec.Push.Repository = host + "/me/app" })
			r := harness(t, pinnedFrom, obj)
			r.JobConfig.InsecureRegistries = append(r.JobConfig.InsecureRegistries, host)
			if _, err := reconcileOnce(t, r, obj); err != nil {
				t.Fatal(err)
			}
			finishJobWithout(t, r, obj, time.Hour)
			for range 3 {
				if _, err := reconcileOnce(t, r, obj); err != nil {
					t.Fatal(err)
				}
			}
			got := reload(t, r, obj)
			if got.Status.Failures != 0 || len(jobsIn(t, r, obj.Namespace)) != 1 {
				t.Fatalf("failures=%d jobs=%d", got.Status.Failures, len(jobsIn(t, r, obj.Namespace)))
			}
			if c := conditionOf(got, ociv1alpha1.ReadyCondition); c != nil && strings.Contains(c.Message, outputLostPrefix) {
				t.Fatalf("called a loss: %s", c.Message)
			}
		})
	}
}

func TestAJobWithoutACompletionTimeStaysPending(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	job, _ := finishJobWithout(t, r, obj, time.Hour)
	job.Status.CompletionTime = nil
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	got := reload(t, r, obj)
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil || c.Reason != ociv1alpha1.ReasonDependencyNotReady ||
		got.Status.Failures != 0 || len(jobsIn(t, r, obj.Namespace)) != 1 {
		t.Fatalf("want Pending: %+v failures=%d", c, got.Status.Failures)
	}
}

// A refused build is decided before the read-back, so a digest the registry lacks is no loss.
func TestARefusedBuildIsNotRebuiltWhenItsDigestIsAbsent(t *testing.T) {
	for _, policy := range []ociv1alpha1.TagConflictPolicy{ociv1alpha1.ConflictFail, ociv1alpha1.ConflictKeep} {
		t.Run(string(policy), func(t *testing.T) {
			obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
				b.Spec.Push.OnConflict = policy
				b.Spec.Push.Tags = []string{"v1"}
			})
			r := harness(t, pinnedFrom, obj)
			if _, err := reconcileOnce(t, r, obj); err != nil {
				t.Fatal(err)
			}
			// Somebody else takes the tag while the Job runs.
			_, other := pushByDigest(t, obj.Spec.Push.Repository)
			tagAs(t, obj.Spec.Push.Repository, "v1", other)
			finishJobWithout(t, r, obj, time.Hour)
			for range 3 {
				if _, err := reconcileOnce(t, r, obj); err != nil {
					t.Fatal(err)
				}
			}
			got := reload(t, r, obj)
			if got.Status.Failures != 0 || len(jobsIn(t, r, obj.Namespace)) != 1 {
				t.Fatalf("failures=%d jobs=%d", got.Status.Failures, len(jobsIn(t, r, obj.Namespace)))
			}
			if c := conditionOf(got, ociv1alpha1.ReadyCondition); c != nil && strings.Contains(c.Message, outputLostPrefix) {
				t.Fatalf("called a loss: %s", c.Message)
			}
		})
	}
}

// A predecessor's pod under the same Job name is not this Job's: Pending, not "no digest".
func TestAStalePodAloneIsPending(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	job, _ := finishJobWithout(t, r, obj, time.Hour)
	job.UID = "the-recreated-job"
	if err := r.Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatal(err)
	}
	c := conditionOf(reload(t, r, obj), ociv1alpha1.ReadyCondition)
	if c == nil || c.Reason != ociv1alpha1.ReasonDependencyNotReady || strings.Contains(c.Message, "no image digest") {
		t.Fatalf("want Pending on the missing pod: %+v", c)
	}
}
